package router

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"time"
)

// strategies lists every value NewSelector accepts. The unknown strategy
// error names them, so a typo in a config file explains its own fix.
var strategies = []Strategy{
	StrategyPriority,
	StrategyLeastLatency,
	StrategyRoundRobin,
}

// NewSelector returns the selector for a strategy.
//
// An unknown strategy is an error rather than a quiet fall back to
// priority: sending live traffic through a rule the operator did not ask
// for is worse than refusing to start. Options.withDefaults already fills
// an unset strategy, so an empty value reaching here is also a mistake.
func NewSelector(s Strategy) (Selector, error) {
	switch s {
	case StrategyPriority:
		return prioritySelector{}, nil
	case StrategyLeastLatency:
		return leastLatencySelector{}, nil
	case StrategyRoundRobin:
		return &roundRobinSelector{}, nil
	default:
		return nil, fmt.Errorf("unknown strategy %q (valid: %s)", string(s), strategyNames())
	}
}

func strategyNames() string {
	names := make([]string, len(strategies))
	for i, s := range strategies {
		names[i] = string(s)
	}
	return strings.Join(names, ", ")
}

// availableFrom keeps the providers that may be tried at now, in
// configured order.
//
// Every strategy starts here, because exclusion outranks ordering: an
// unavailable provider is not ranked last, it is not ranked at all, so a
// flattering latency number can never talk an open breaker into a call.
// The result is always non nil, so an empty chain and a wholly sick chain
// look the same to the caller and neither needs a nil check.
func availableFrom(chain []string, h Health, now time.Time) []string {
	out := make([]string, 0, len(chain))
	for _, name := range chain {
		// A route wired up without health tracking excludes nobody
		// rather than taking down the request with it.
		if h == nil || h.Available(name, now) {
			out = append(out, name)
		}
	}
	return out
}

// prioritySelector tries the chain as configured. The operator's order is
// the ranking, and nothing else gets a vote.
type prioritySelector struct{}

func (prioritySelector) Order(chain []string, h Health, now time.Time) []string {
	return availableFrom(chain, h, now)
}

// leastLatencySelector prefers whichever provider has been answering
// fastest, measured as smoothed time to first token.
type leastLatencySelector struct{}

// latencyRank is one provider's sort key.
type latencyRank struct {
	name    string
	ttft    time.Duration
	sampled bool
}

// Order puts every provider without enough latency samples ahead of every
// sampled one, then sorts the sampled ones by ascending smoothed TTFT.
//
// Leading with the unmeasured providers looks backwards, since their
// speed is exactly what we do not know. The alternative starves them:
// samples only come from traffic, and a provider parked at the back of
// the chain receives traffic only when everything ahead of it fails, so
// it stays unsampled, stays at the back, and its line in the config is
// decorative forever. Ranking it first costs one attempt on an unknown
// provider and buys the measurement that puts it in its true place from
// the next call on, where a slow one promptly sinks.
func (leastLatencySelector) Order(chain []string, h Health, now time.Time) []string {
	avail := availableFrom(chain, h, now)

	// Snapshot each latency once. Health is read by every in flight
	// request and written by every completion, so a value re-read inside
	// the comparator could change mid sort and leave the comparison
	// inconsistent with itself.
	ranks := make([]latencyRank, len(avail))
	for i, name := range avail {
		r := latencyRank{name: name}
		if h != nil {
			r.ttft, r.sampled = h.Latency(name)
		}
		ranks[i] = r
	}

	// A stable sort is what makes configured order the tie break, both
	// among the unsampled group and between equal latencies.
	slices.SortStableFunc(ranks, func(a, b latencyRank) int {
		if a.sampled != b.sampled {
			if a.sampled {
				return 1
			}
			return -1
		}
		if !a.sampled {
			return 0
		}
		return cmp.Compare(a.ttft, b.ttft)
	})

	out := make([]string, len(ranks))
	for i, r := range ranks {
		out[i] = r.name
	}
	return out
}

// roundRobinSelector rotates which provider leads the chain. Free tier
// quotas are per provider, so always leading with the same one drains a
// single bucket while its peers sit idle.
type roundRobinSelector struct {
	// calls only ever moves forward and is read only as a rotation
	// offset, so wraparound is harmless.
	calls atomic.Uint64
}

// Order rotates the starting point by one provider per call and keeps the
// configured relative order after it, so the chain stays predictable
// while the load spreads.
func (s *roundRobinSelector) Order(chain []string, h Health, now time.Time) []string {
	avail := availableFrom(chain, h, now)
	total := uint64(len(avail))
	if total == 0 {
		// Nothing to lead. Do not spend a turn on it, so the next real
		// call still starts where it would have.
		return avail
	}

	start := s.calls.Add(1) - 1
	out := make([]string, 0, len(avail))
	for i := uint64(0); i < total; i++ {
		out = append(out, avail[(start+i)%total])
	}
	return out
}
