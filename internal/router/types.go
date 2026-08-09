package router

import (
	"time"

	"github.com/rajeev-chaurasia/pen-stock/internal/providers"
)

// Strategy names the order in which a chain is tried.
type Strategy string

const (
	// StrategyPriority tries the chain in the order it was configured.
	StrategyPriority Strategy = "priority"
	// StrategyLeastLatency prefers whichever healthy provider has been
	// answering fastest.
	StrategyLeastLatency Strategy = "least_latency"
	// StrategyRoundRobin spreads load evenly, which on free tiers means
	// spreading the request budget across independent quotas.
	StrategyRoundRobin Strategy = "round_robin"
)

// Health is the view the selector has of one provider. Implementations
// must be safe for concurrent use: every in flight request reads it.
type Health interface {
	// Available reports whether the provider may be tried at now.
	Available(provider string, now time.Time) bool
	// RecordSuccess reports a completed call and how long its first
	// token took, which feeds latency aware selection.
	RecordSuccess(provider string, ttft time.Duration, now time.Time)
	// RecordFailure reports a failure worth holding against the
	// provider. retryAfter carries an upstream Retry-After when the
	// provider sent one, and is zero otherwise.
	RecordFailure(provider string, class providers.ErrorClass, retryAfter time.Duration, now time.Time)
	// Latency reports the current smoothed time to first token, and
	// whether enough samples exist to trust it.
	Latency(provider string) (time.Duration, bool)
}

// Selector orders a chain for one attempt sequence. It returns provider
// names, not providers, so selection stays independent of construction.
type Selector interface {
	// Order returns the candidates to try, best first. Unavailable
	// providers are excluded, so an empty result means the whole chain
	// is currently sick.
	Order(chain []string, h Health, now time.Time) []string
}

// Clock exists so tests can drive breaker windows and backoff without
// sleeping. Production passes a real clock.
type Clock interface {
	Now() time.Time
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

// Options configures one routed model.
type Options struct {
	Strategy Strategy
	// MaxAttempts bounds total upstream calls for one client request,
	// across every provider in the chain. Without a ceiling a long
	// chain turns one client request into a storm.
	MaxAttempts int
	// RetryBaseDelay is the first backoff step; later steps grow
	// exponentially with full jitter.
	RetryBaseDelay time.Duration
	// MaxRetryDelay caps a single backoff so a slow chain cannot outlive
	// the caller's patience.
	MaxRetryDelay time.Duration
	// BreakerThreshold is how many health-relevant failures in a row
	// open the breaker.
	BreakerThreshold int
	// BreakerCooldown is how long an open breaker stays open before a
	// single probe is allowed through.
	BreakerCooldown time.Duration
}

// Defaults for Options fields left zero.
const (
	DefaultMaxAttempts      = 3
	DefaultRetryBaseDelay   = 100 * time.Millisecond
	DefaultMaxRetryDelay    = 2 * time.Second
	DefaultBreakerThreshold = 5
	DefaultBreakerCooldown  = 30 * time.Second
)

func (o Options) withDefaults() Options {
	if o.Strategy == "" {
		o.Strategy = StrategyPriority
	}
	if o.MaxAttempts <= 0 {
		o.MaxAttempts = DefaultMaxAttempts
	}
	if o.RetryBaseDelay <= 0 {
		o.RetryBaseDelay = DefaultRetryBaseDelay
	}
	if o.MaxRetryDelay <= 0 {
		o.MaxRetryDelay = DefaultMaxRetryDelay
	}
	if o.BreakerThreshold <= 0 {
		o.BreakerThreshold = DefaultBreakerThreshold
	}
	if o.BreakerCooldown <= 0 {
		o.BreakerCooldown = DefaultBreakerCooldown
	}
	return o
}

// Attempt records one upstream try, for tracing and for the tests that
// assert a chain behaved the way the policy says it should.
type Attempt struct {
	Provider string
	Class    providers.ErrorClass
	Err      error
	Duration time.Duration
}
