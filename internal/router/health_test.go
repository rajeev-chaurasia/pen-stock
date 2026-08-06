package router

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/rajeev-chaurasia/pen-stock/internal/providers"
)

// healthFakeClock drives every window in these tests. Time moves only when
// a test says so, so nothing here sleeps and no assertion is a race with
// the wall clock.
type healthFakeClock struct {
	mu  sync.Mutex
	now time.Time
}

var _ Clock = (*healthFakeClock)(nil)

func newHealthFakeClock() *healthFakeClock {
	return &healthFakeClock{
		now: time.Date(2026, time.January, 2, 15, 4, 5, 0, time.UTC),
	}
}

func (c *healthFakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Advance moves the clock forward and reports the new time, which is what
// tests pass as the now argument.
func (c *healthFakeClock) Advance(d time.Duration) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	return c.now
}

// healthFailN reports the same failure class n times at one instant.
func healthFailN(h Health, provider string, class providers.ErrorClass, n int, now time.Time) {
	for range n {
		h.RecordFailure(provider, class, 0, now)
	}
}

const healthProvider = "groq"

func TestHealthBreakerOpensAtThreshold(t *testing.T) {
	t.Parallel()

	// Every class that says something about the provider itself should be
	// able to trip the breaker on its own.
	cases := []struct {
		name  string
		class providers.ErrorClass
	}{
		{"upstream", providers.ErrClassUpstream},
		{"timeout", providers.ErrClassTimeout},
		{"auth", providers.ErrClassAuth},
		{"internal", providers.ErrClassInternal},
	}

	const threshold = 3

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			clk := newHealthFakeClock()
			h := NewHealth(Options{BreakerThreshold: threshold, BreakerCooldown: time.Minute}, clk)

			healthFailN(h, healthProvider, tc.class, threshold-1, clk.Now())
			if !h.Available(healthProvider, clk.Now()) {
				t.Fatalf("provider unavailable after %d of %d failures", threshold-1, threshold)
			}

			h.RecordFailure(healthProvider, tc.class, 0, clk.Now())
			if h.Available(healthProvider, clk.Now()) {
				t.Fatal("breaker did not open on the threshold failure")
			}
		})
	}
}

func TestHealthSuccessResetsFailureStreak(t *testing.T) {
	t.Parallel()

	const threshold = 3
	clk := newHealthFakeClock()
	h := NewHealth(Options{BreakerThreshold: threshold, BreakerCooldown: time.Minute}, clk)

	healthFailN(h, healthProvider, providers.ErrClassUpstream, threshold-1, clk.Now())
	h.RecordSuccess(healthProvider, 40*time.Millisecond, clk.Now())

	// The streak restarts from zero, so the same count of failures again
	// must not be enough.
	healthFailN(h, healthProvider, providers.ErrClassUpstream, threshold-1, clk.Now())
	if !h.Available(healthProvider, clk.Now()) {
		t.Fatal("success did not reset the consecutive failure count")
	}

	h.RecordFailure(healthProvider, providers.ErrClassUpstream, 0, clk.Now())
	if h.Available(healthProvider, clk.Now()) {
		t.Fatal("breaker did not open once the streak reached the threshold again")
	}
}

func TestHealthHalfOpenAdmitsExactlyOneProbe(t *testing.T) {
	t.Parallel()

	const (
		threshold = 2
		cooldown  = 30 * time.Second
		callers   = 64
	)

	clk := newHealthFakeClock()
	h := NewHealth(Options{BreakerThreshold: threshold, BreakerCooldown: cooldown}, clk)
	healthFailN(h, healthProvider, providers.ErrClassUpstream, threshold, clk.Now())

	if h.Available(healthProvider, clk.Now()) {
		t.Fatal("breaker should be open before the cooldown elapses")
	}
	now := clk.Advance(cooldown)

	// The herd: every caller asks at the same instant, right as the
	// cooldown expires. Exactly one may get through.
	admitted := make([]bool, callers)
	var release sync.WaitGroup
	var done sync.WaitGroup
	release.Add(1)
	for i := range admitted {
		done.Add(1)
		go func() {
			defer done.Done()
			release.Wait()
			admitted[i] = h.Available(healthProvider, now)
		}()
	}
	release.Done()
	done.Wait()

	got := 0
	for _, ok := range admitted {
		if ok {
			got++
		}
	}
	if got != 1 {
		t.Fatalf("half open admitted %d of %d callers, want exactly 1", got, callers)
	}

	// The slot stays claimed until the probe reports back.
	if h.Available(healthProvider, now) {
		t.Fatal("a second probe was admitted while the first was still in flight")
	}
}

func TestHealthProbeOutcome(t *testing.T) {
	t.Parallel()

	const (
		threshold = 2
		cooldown  = 30 * time.Second
	)

	cases := []struct {
		name string
		// report tells the tracker how the probe went.
		report        func(h Health, now time.Time)
		wantAvailable bool
	}{
		{
			name: "success closes the breaker",
			report: func(h Health, now time.Time) {
				h.RecordSuccess(healthProvider, 50*time.Millisecond, now)
			},
			wantAvailable: true,
		},
		{
			name: "failure re-opens the breaker",
			report: func(h Health, now time.Time) {
				h.RecordFailure(healthProvider, providers.ErrClassUpstream, 0, now)
			},
			wantAvailable: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			clk := newHealthFakeClock()
			h := NewHealth(Options{BreakerThreshold: threshold, BreakerCooldown: cooldown}, clk)
			healthFailN(h, healthProvider, providers.ErrClassUpstream, threshold, clk.Now())

			probeAt := clk.Advance(cooldown)
			if !h.Available(healthProvider, probeAt) {
				t.Fatal("no probe admitted after the cooldown elapsed")
			}
			tc.report(h, probeAt)

			if got := h.Available(healthProvider, probeAt); got != tc.wantAvailable {
				t.Fatalf("Available after probe = %v, want %v", got, tc.wantAvailable)
			}
			if tc.wantAvailable {
				return
			}

			// A failed probe restarts the whole cooldown rather than
			// letting the next caller straight back in.
			if h.Available(healthProvider, clk.Advance(cooldown-time.Nanosecond)) {
				t.Fatal("breaker re-opened for less than a full cooldown")
			}
			if !h.Available(healthProvider, clk.Advance(time.Nanosecond)) {
				t.Fatal("no probe admitted after the restarted cooldown elapsed")
			}
		})
	}
}

func TestHealthUpstreamCooldownWindow(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		class      providers.ErrorClass
		retryAfter time.Duration
		want       time.Duration
	}{
		{
			name:       "429 honors Retry-After",
			class:      providers.ErrClassRateLimited,
			retryAfter: 7 * time.Second,
			want:       7 * time.Second,
		},
		{
			name:  "429 without Retry-After falls back to the default",
			class: providers.ErrClassRateLimited,
			want:  defaultRateLimitCooldown,
		},
		{
			name:  "payment required parks for minutes",
			class: providers.ErrClassPaymentRequired,
			want:  paymentRequiredCooldown,
		},
		{
			name:       "payment required honors a longer Retry-After",
			class:      providers.ErrClassPaymentRequired,
			retryAfter: paymentRequiredCooldown + time.Hour,
			want:       paymentRequiredCooldown + time.Hour,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			clk := newHealthFakeClock()
			// A threshold well above one failure keeps the breaker out of
			// this: only the cooldown should be deciding.
			h := NewHealth(Options{BreakerThreshold: 10, BreakerCooldown: time.Hour}, clk)

			h.RecordFailure(healthProvider, tc.class, tc.retryAfter, clk.Now())
			if h.Available(healthProvider, clk.Now()) {
				t.Fatal("provider available immediately after an upstream cooldown")
			}
			if h.Available(healthProvider, clk.Advance(tc.want-time.Nanosecond)) {
				t.Fatalf("provider available before the %s cooldown elapsed", tc.want)
			}
			if !h.Available(healthProvider, clk.Advance(time.Nanosecond)) {
				t.Fatalf("provider still blocked after the %s cooldown elapsed", tc.want)
			}
		})
	}
}

func TestHealthPaymentRequiredOutlastsRateLimit(t *testing.T) {
	t.Parallel()

	// An unactivated account is not a bucket that refills, so its pause has
	// to be an order of magnitude longer than a plain 429.
	if paymentRequiredCooldown < 10*defaultRateLimitCooldown {
		t.Fatalf("paymentRequiredCooldown %s is not much longer than defaultRateLimitCooldown %s",
			paymentRequiredCooldown, defaultRateLimitCooldown)
	}

	clk := newHealthFakeClock()
	h := NewHealth(Options{BreakerThreshold: 10, BreakerCooldown: time.Hour}, clk)

	h.RecordFailure(healthProvider, providers.ErrClassPaymentRequired, 0, clk.Now())
	if h.Available(healthProvider, clk.Advance(defaultRateLimitCooldown)) {
		t.Fatal("payment required expired on the rate limit schedule")
	}
	// Billing gets fixed: the provider rejoins the chain on its own.
	if !h.Available(healthProvider, clk.Advance(paymentRequiredCooldown)) {
		t.Fatal("payment required never expired")
	}
}

func TestHealthRequestFaultsNeverOpenTheBreaker(t *testing.T) {
	t.Parallel()

	// These classes are the caller's problem, not the provider's. No
	// volume of them may take a healthy provider out of rotation.
	cases := []struct {
		name  string
		class providers.ErrorClass
	}{
		{"invalid_request", providers.ErrClassInvalidRequest},
		{"canceled", providers.ErrClassCanceled},
		{"model_not_found", providers.ErrClassModelNotFound},
	}

	const flood = 200

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			clk := newHealthFakeClock()
			h := NewHealth(Options{BreakerThreshold: 2, BreakerCooldown: time.Hour}, clk)

			healthFailN(h, healthProvider, tc.class, flood, clk.Now())
			if !h.Available(healthProvider, clk.Now()) {
				t.Fatalf("%d %s failures opened the breaker", flood, tc.class)
			}

			// Still counted for nothing after time passes, and a real
			// failure still starts its streak from zero.
			healthFailN(h, healthProvider, providers.ErrClassUpstream, 1, clk.Advance(time.Minute))
			if !h.Available(healthProvider, clk.Now()) {
				t.Fatal("request faults leaked into the failure streak")
			}
		})
	}
}

func TestHealthCooldownAndBreakerCompose(t *testing.T) {
	t.Parallel()

	const (
		threshold = 2
		cooldown  = 10 * time.Second
		retry     = 5 * time.Minute
	)

	t.Run("rate limit blocks while the breaker is closed", func(t *testing.T) {
		t.Parallel()

		clk := newHealthFakeClock()
		h := NewHealth(Options{BreakerThreshold: threshold, BreakerCooldown: cooldown}, clk)

		h.RecordFailure(healthProvider, providers.ErrClassRateLimited, retry, clk.Now())
		if h.Available(healthProvider, clk.Now()) {
			t.Fatal("a closed breaker masked an active rate limit cooldown")
		}
	})

	t.Run("rate limit outlives the breaker cooldown", func(t *testing.T) {
		t.Parallel()

		clk := newHealthFakeClock()
		h := NewHealth(Options{BreakerThreshold: threshold, BreakerCooldown: cooldown}, clk)

		healthFailN(h, healthProvider, providers.ErrClassUpstream, threshold, clk.Now())
		h.RecordFailure(healthProvider, providers.ErrClassRateLimited, retry, clk.Now())

		// The breaker is ready to probe, but the provider told us its
		// quota is gone. A probe now would be a guaranteed 429.
		if h.Available(healthProvider, clk.Advance(cooldown)) {
			t.Fatal("breaker probe was admitted during a rate limit cooldown")
		}
		// The blocked window must not have burned the probe slot.
		if !h.Available(healthProvider, clk.Advance(retry)) {
			t.Fatal("no probe admitted once both windows elapsed")
		}
	})

	t.Run("the longer of two rate limits wins", func(t *testing.T) {
		t.Parallel()

		clk := newHealthFakeClock()
		h := NewHealth(Options{BreakerThreshold: threshold, BreakerCooldown: cooldown}, clk)

		h.RecordFailure(healthProvider, providers.ErrClassRateLimited, retry, clk.Now())
		h.RecordFailure(healthProvider, providers.ErrClassRateLimited, time.Second, clk.Now())
		if h.Available(healthProvider, clk.Advance(time.Second)) {
			t.Fatal("a shorter Retry-After shortened a pause already running")
		}
	})
}

func TestHealthLatencyNeedsMinimumSamples(t *testing.T) {
	t.Parallel()

	if _, ok := NewHealth(Options{}, newHealthFakeClock()).Latency("unknown"); ok {
		t.Fatal("Latency reported ok for a provider with no history")
	}

	for samples := 1; samples <= minLatencySamples; samples++ {
		t.Run(fmt.Sprintf("%d_samples", samples), func(t *testing.T) {
			clk := newHealthFakeClock()
			h := NewHealth(Options{}, clk)
			for range samples {
				h.RecordSuccess(healthProvider, 100*time.Millisecond, clk.Now())
			}

			got, ok := h.Latency(healthProvider)
			want := samples >= minLatencySamples
			if ok != want {
				t.Fatalf("Latency ok = %v after %d samples, want %v", ok, samples, want)
			}
			if !ok && got != 0 {
				t.Fatalf("Latency returned %s alongside ok=false, want 0", got)
			}
			if ok && got != 100*time.Millisecond {
				t.Fatalf("Latency = %s over identical samples, want 100ms", got)
			}
		})
	}
}

func TestHealthLatencyFollowsRecentSamples(t *testing.T) {
	t.Parallel()

	const (
		fast = 100 * time.Millisecond
		slow = time.Second
	)

	clk := newHealthFakeClock()
	h := NewHealth(Options{}, clk)
	for range minLatencySamples {
		h.RecordSuccess(healthProvider, fast, clk.Now())
	}

	previous, ok := h.Latency(healthProvider)
	if !ok {
		t.Fatal("Latency not ready after the minimum samples")
	}

	// A sustained slowdown has to show up, but no single sample may drag
	// the average all the way to the new value.
	for range 3 {
		h.RecordSuccess(healthProvider, slow, clk.Advance(time.Second))
		got, _ := h.Latency(healthProvider)
		if got <= previous {
			t.Fatalf("Latency %s did not rise above %s after a slow sample", got, previous)
		}
		if got >= slow {
			t.Fatalf("Latency %s reached the raw sample %s, smoothing is not applied", got, slow)
		}
		previous = got
	}

	// A zero timing carries no information and must not be folded in.
	h.RecordSuccess(healthProvider, 0, clk.Now())
	if got, _ := h.Latency(healthProvider); got != previous {
		t.Fatalf("Latency moved to %s on a zero sample, want %s", got, previous)
	}
}

func TestHealthZeroTimeUsesTheClock(t *testing.T) {
	t.Parallel()

	clk := newHealthFakeClock()
	h := NewHealth(Options{}, clk)

	h.RecordFailure(healthProvider, providers.ErrClassRateLimited, 0, time.Time{})
	if h.Available(healthProvider, time.Time{}) {
		t.Fatal("a zero timestamp skipped the cooldown instead of reading the clock")
	}

	clk.Advance(defaultRateLimitCooldown)
	if !h.Available(healthProvider, time.Time{}) {
		t.Fatal("cooldown did not expire against the clock")
	}

	// A nil clock is the production path and must not panic.
	if !NewHealth(Options{}, nil).Available(healthProvider, time.Time{}) {
		t.Fatal("a fresh tracker with the real clock reported unavailable")
	}
}

// TestHealthConcurrentUse hammers every entry point from many goroutines at
// once. It asserts only that the tracker keeps answering and stays
// internally consistent; the interleaving bugs this guards against are
// caught by the -race build, which CI runs.
func TestHealthConcurrentUse(t *testing.T) {
	t.Parallel()

	const (
		workers = 16
		rounds  = 250
	)

	clk := newHealthFakeClock()
	h := NewHealth(Options{BreakerThreshold: 3, BreakerCooldown: time.Second}, clk)
	chain := []string{"groq", "cerebras", "gemini"}

	var wg sync.WaitGroup
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for r := range rounds {
				provider := chain[(w+r)%len(chain)]
				now := clk.Advance(time.Millisecond)
				switch r % 4 {
				case 0:
					h.Available(provider, now)
				case 1:
					h.RecordSuccess(provider, time.Duration(r)*time.Millisecond, now)
				case 2:
					h.RecordFailure(provider, providers.ErrClassUpstream, 0, now)
				default:
					h.RecordFailure(provider, providers.ErrClassRateLimited, time.Second, now)
				}
				h.Latency(provider)
			}
		}()
	}
	wg.Wait()

	for _, provider := range chain {
		if d, ok := h.Latency(provider); ok && d <= 0 {
			t.Fatalf("%s reported a usable latency of %s", provider, d)
		}
	}
}
