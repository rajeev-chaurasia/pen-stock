package router

import (
	"sync"
	"time"

	"github.com/rajeev-chaurasia/pen-stock/internal/providers"
)

// breakerState is where one provider sits in the circuit breaker cycle.
type breakerState int

const (
	// breakerClosed is the healthy state: traffic flows and consecutive
	// failures are counted.
	breakerClosed breakerState = iota
	// breakerOpen sheds every request until the cooldown elapses.
	breakerOpen
	// breakerHalfOpen lets exactly one probe through to learn whether the
	// provider came back.
	breakerHalfOpen
)

const (
	// defaultRateLimitCooldown parks a provider that answered 429 without a
	// Retry-After. Free tiers usually meter per minute, so a shorter window
	// mostly spends the next request re-learning the same 429, while a much
	// longer one gives away quota that has already refilled.
	defaultRateLimitCooldown time.Duration = 30 * time.Second

	// paymentRequiredCooldown parks a provider whose account cannot pay.
	// Nothing refills here: only an operator adding credit or activating
	// the tier fixes it, so the wait is minutes rather than seconds. It
	// stays bounded so the provider rejoins the chain by itself once
	// billing is sorted, with no gateway restart needed.
	paymentRequiredCooldown time.Duration = 15 * time.Minute

	// probeStallTimeout reclaims a half open slot whose probe never
	// reported back. A caller that dies between Available and its Record
	// call would otherwise wedge the provider out of the chain forever.
	// Generous on purpose: a slow completion that eventually reports should
	// not be double probed, and admitting a second probe is a far smaller
	// mistake than never admitting another one.
	probeStallTimeout time.Duration = 2 * time.Minute

	// latencyAlpha weights the newest sample in the moving average. At 0.2
	// a sample still carries about a third of its weight five calls later:
	// one slow response cannot reorder the chain, but a provider that
	// genuinely degrades falls behind within a handful of requests.
	latencyAlpha float64 = 0.2

	// minLatencySamples is how many timings a provider needs before its
	// average is worth ranking on. One lucky call is not evidence.
	minLatencySamples int = 5
)

// providerHealth is the mutable state of a single provider. Every field is
// read and written with tracker.mu held.
type providerHealth struct {
	breaker             breakerState
	consecutiveFailures int
	openedAt            time.Time

	probeInFlight bool
	probeStarted  time.Time

	// cooldownUntil is a pause the provider asked for itself (429, 402).
	// It runs independently of the breaker: either gate alone is enough to
	// make the provider unavailable.
	cooldownUntil time.Time

	ewma    time.Duration
	samples int
}

// tracker implements Health for a set of providers behind one mutex. Each
// critical section is a few field reads and writes, which costs far less
// than the upstream call the caller is about to make.
type tracker struct {
	opts  Options
	clock Clock

	mu    sync.Mutex
	state map[string]*providerHealth
}

var _ Health = (*tracker)(nil)

// NewHealth returns a health tracker for one routed model. A nil clock
// means the real one.
func NewHealth(opts Options, clock Clock) Health {
	if clock == nil {
		clock = realClock{}
	}
	return &tracker{
		opts:  opts.withDefaults(),
		clock: clock,
		state: make(map[string]*providerHealth),
	}
}

// Available reports whether provider may be tried at now.
//
// It is not a pure query. When the breaker is ready for a half open probe
// this call claims that probe for the caller, and every other caller is
// turned away until the probe reports through RecordSuccess or
// RecordFailure. That is what keeps a recovering provider from being hit
// by the whole waiting herd at once. A claimed probe that never reports is
// released after probeStallTimeout.
func (t *tracker) Available(provider string, now time.Time) bool {
	now = t.at(now)

	t.mu.Lock()
	defer t.mu.Unlock()

	s := t.state[provider]
	if s == nil {
		// No history means nothing is known against it.
		return true
	}

	// Checked ahead of the breaker so an upstream pause is never spent as
	// a probe. The two gates compose: neither can mask the other.
	if now.Before(s.cooldownUntil) {
		return false
	}

	switch s.breaker {
	case breakerOpen:
		if now.Sub(s.openedAt) < t.opts.BreakerCooldown {
			return false
		}
		s.breaker = breakerHalfOpen
		return s.admitProbe(now)
	case breakerHalfOpen:
		return s.admitProbe(now)
	default: // breakerClosed
		return true
	}
}

// RecordSuccess clears the failure streak, closes the breaker, and folds
// the timing into the latency average.
//
// The timestamp is deliberately unused: success ends every window this
// tracker keeps rather than starting one. An unexpired cooldown does
// survive, because it came from the provider's own Retry-After and a call
// that was already in flight when the 429 landed says nothing about
// whether the bucket has refilled since.
func (t *tracker) RecordSuccess(provider string, ttft time.Duration, _ time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()

	s := t.get(provider)
	s.breaker = breakerClosed
	s.consecutiveFailures = 0
	s.probeInFlight = false

	// A non-streaming call has no time to first token to report, and
	// folding a zero in would flatter the provider.
	if ttft > 0 {
		s.observeLatency(ttft)
	}
}

// RecordFailure folds one failure into the provider's health. Classes that
// say something about the request rather than the provider are dropped by
// countsAgainstHealth, so one bad caller cannot take a healthy provider out
// of rotation for everyone.
func (t *tracker) RecordFailure(provider string, class providers.ErrorClass, retryAfter time.Duration, now time.Time) {
	now = t.at(now)

	t.mu.Lock()
	defer t.mu.Unlock()

	s := t.get(provider)

	if pause := cooldownFor(class, retryAfter); pause > 0 {
		// Never shorten a pause already running: two 429s answered at once
		// must not talk the provider back into rotation early.
		if until := now.Add(pause); until.After(s.cooldownUntil) {
			s.cooldownUntil = until
		}
	}

	if !countsAgainstHealth(class) {
		return
	}
	s.consecutiveFailures++

	switch s.breaker {
	case breakerHalfOpen:
		// The probe came back sick, so the whole cooldown starts over.
		s.breaker = breakerOpen
		s.openedAt = now
		s.probeInFlight = false
	case breakerOpen:
		// Already shedding. A straggler report from a call that started
		// before the breaker opened must not extend the outage.
	default: // breakerClosed
		if s.consecutiveFailures >= t.opts.BreakerThreshold {
			s.breaker = breakerOpen
			s.openedAt = now
		}
	}
}

// Latency reports the smoothed time to first token, and whether enough
// samples exist for a selector to rank on it.
func (t *tracker) Latency(provider string) (time.Duration, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	s := t.state[provider]
	if s == nil || s.samples < minLatencySamples {
		return 0, false
	}
	return s.ewma, true
}

// at falls back to the clock when a caller has no timestamp of its own, so
// a zero time is never read as year one, which would expire every cooldown
// at once.
func (t *tracker) at(now time.Time) time.Time {
	if now.IsZero() {
		return t.clock.Now()
	}
	return now
}

// get returns the provider's state, creating it on first report. Reads
// never allocate: a provider nobody has reported on is healthy by
// definition, so Available and Latency answer from a nil entry.
func (t *tracker) get(provider string) *providerHealth {
	s := t.state[provider]
	if s == nil {
		s = &providerHealth{}
		t.state[provider] = s
	}
	return s
}

// admitProbe hands the single half open slot to one caller and turns the
// rest away.
func (s *providerHealth) admitProbe(now time.Time) bool {
	if s.probeInFlight && now.Sub(s.probeStarted) < probeStallTimeout {
		return false
	}
	s.probeInFlight = true
	s.probeStarted = now
	return true
}

// observeLatency folds one timing into the moving average. The first
// sample seeds it, since starting from zero would take several calls to
// climb to a truth we already know.
func (s *providerHealth) observeLatency(ttft time.Duration) {
	s.samples++
	if s.samples == 1 {
		s.ewma = ttft
		return
	}
	s.ewma = time.Duration(latencyAlpha*float64(ttft) + (1-latencyAlpha)*float64(s.ewma))
}

// cooldownFor is how long a failure class parks a provider, independently
// of the breaker. Only classes where the provider effectively told us to
// come back later get one.
func cooldownFor(class providers.ErrorClass, retryAfter time.Duration) time.Duration {
	switch class {
	case providers.ErrClassRateLimited:
		if retryAfter > 0 {
			return retryAfter
		}
		return defaultRateLimitCooldown
	case providers.ErrClassPaymentRequired:
		// An upstream naming a longer wait than our own guess knows
		// something we do not, so honor it.
		if retryAfter > paymentRequiredCooldown {
			return retryAfter
		}
		return paymentRequiredCooldown
	default:
		return 0
	}
}
