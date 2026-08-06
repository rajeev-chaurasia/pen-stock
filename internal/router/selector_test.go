package router_test

import (
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rajeev-chaurasia/pen-stock/internal/providers"
	"github.com/rajeev-chaurasia/pen-stock/internal/router"
)

// stubHealth is a frozen view of provider health. The real tracker is
// owned elsewhere and a selector only ever reads, so the recorders here
// are inert on purpose: a test that changes health mid order would be
// testing the tracker instead of the ordering.
type stubHealth struct {
	down map[string]bool
	// sampled holds smoothed TTFT for the providers measured often
	// enough to trust. A provider absent from the map is one nobody has
	// a usable sample for yet.
	sampled map[string]time.Duration
}

func (s stubHealth) Available(provider string, _ time.Time) bool { return !s.down[provider] }

func (stubHealth) RecordSuccess(string, time.Duration, time.Time) {}

func (stubHealth) RecordFailure(string, providers.ErrorClass, time.Duration, time.Time) {}

func (s stubHealth) Latency(provider string) (time.Duration, bool) {
	d, ok := s.sampled[provider]
	return d, ok
}

func healthWithDown(down ...string) stubHealth {
	h := stubHealth{down: make(map[string]bool, len(down))}
	for _, name := range down {
		h.down[name] = true
	}
	return h
}

var (
	allStrategies = []router.Strategy{
		router.StrategyPriority,
		router.StrategyLeastLatency,
		router.StrategyRoundRobin,
	}

	testNow = time.Date(2026, time.August, 5, 12, 0, 0, 0, time.UTC)
)

func mustSelector(t *testing.T, s router.Strategy) router.Selector {
	t.Helper()
	sel, err := router.NewSelector(s)
	if err != nil {
		t.Fatalf("NewSelector(%q): %v", s, err)
	}
	if sel == nil {
		t.Fatalf("NewSelector(%q) returned a nil selector and no error", s)
	}
	return sel
}

// rotated is the expected round robin shape: same order, different entry
// point.
func rotated(items []string, start int) []string {
	out := make([]string, 0, len(items))
	for i := range items {
		out = append(out, items[(start+i)%len(items)])
	}
	return out
}

func assertOrder(t *testing.T, got, want []string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

// assertPermutation checks the invariant every strategy shares: the
// result is the available subset rearranged, with nothing dropped and
// nothing served twice. ctx names the case, since the property test runs
// this far more often than it prints.
func assertPermutation(t *testing.T, ctx string, got, wantSet []string) {
	t.Helper()
	if len(got) != len(wantSet) {
		t.Fatalf("%s: order %v has %d entries, want %d (%v)", ctx, got, len(got), len(wantSet), wantSet)
	}
	remaining := make(map[string]int, len(wantSet))
	for _, name := range wantSet {
		remaining[name]++
	}
	for _, name := range got {
		if remaining[name] == 0 {
			t.Fatalf("%s: order %v contains %q, which is not available (or is duplicated); available set is %v", ctx, got, name, wantSet)
		}
		remaining[name]--
	}
}

func TestNewSelectorAcceptsEveryKnownStrategy(t *testing.T) {
	for _, s := range allStrategies {
		t.Run(string(s), func(t *testing.T) {
			mustSelector(t, s)
		})
	}
}

func TestNewSelectorUnknownStrategyErrorNamesTheValidValues(t *testing.T) {
	for _, bad := range []router.Strategy{"", "fastest_vibes", "Priority"} {
		t.Run(fmt.Sprintf("%q", string(bad)), func(t *testing.T) {
			sel, err := router.NewSelector(bad)
			if err == nil {
				t.Fatalf("NewSelector(%q) accepted an unknown strategy, got %#v", string(bad), sel)
			}
			if sel != nil {
				t.Errorf("selector = %#v, want nil alongside the error", sel)
			}
			for _, want := range []string{"priority", "least_latency", "round_robin"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not name the valid value %q", err, want)
				}
			}
		})
	}
}

func TestPriorityOrder(t *testing.T) {
	tests := []struct {
		name  string
		chain []string
		down  []string
		want  []string
	}{
		{
			name:  "configured order is the ranking",
			chain: []string{"groq", "gemini", "anthropic"},
			want:  []string{"groq", "gemini", "anthropic"},
		},
		{
			name:  "unavailable head is skipped and the rest keep order",
			chain: []string{"groq", "gemini", "anthropic"},
			down:  []string{"groq"},
			want:  []string{"gemini", "anthropic"},
		},
		{
			name:  "unavailable middle provider is excluded",
			chain: []string{"groq", "gemini", "anthropic"},
			down:  []string{"gemini"},
			want:  []string{"groq", "anthropic"},
		},
		{
			name:  "single provider chain",
			chain: []string{"groq"},
			want:  []string{"groq"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sel := mustSelector(t, router.StrategyPriority)
			assertOrder(t, sel.Order(tc.chain, healthWithDown(tc.down...), testNow), tc.want)
		})
	}
}

func TestLeastLatencyOrder(t *testing.T) {
	const (
		quick  = 40 * time.Millisecond
		medium = 300 * time.Millisecond
		slow   = 2 * time.Second
	)

	tests := []struct {
		name    string
		chain   []string
		sampled map[string]time.Duration
		down    []string
		want    []string
	}{
		{
			name:  "sampled providers sort by ascending smoothed ttft",
			chain: []string{"sluggish", "swift", "middling"},
			sampled: map[string]time.Duration{
				"sluggish": slow,
				"swift":    quick,
				"middling": medium,
			},
			want: []string{"swift", "middling", "sluggish"},
		},
		{
			name:  "unsampled provider leads sampled ones so it can earn the sample it would otherwise starve for",
			chain: []string{"sluggish", "newcomer", "swift"},
			sampled: map[string]time.Duration{
				"sluggish": slow,
				"swift":    quick,
			},
			want: []string{"newcomer", "swift", "sluggish"},
		},
		{
			name:  "several unsampled providers lead in configured order",
			chain: []string{"swift", "newB", "sluggish", "newA"},
			sampled: map[string]time.Duration{
				"swift":    quick,
				"sluggish": slow,
			},
			want: []string{"newB", "newA", "swift", "sluggish"},
		},
		{
			name:  "equal latencies break by configured order",
			chain: []string{"bravo", "alpha", "charlie"},
			sampled: map[string]time.Duration{
				"bravo":   medium,
				"alpha":   medium,
				"charlie": medium,
			},
			want: []string{"bravo", "alpha", "charlie"},
		},
		{
			name:  "the fastest provider is excluded when unavailable, not merely demoted",
			chain: []string{"sluggish", "swift", "middling"},
			sampled: map[string]time.Duration{
				"sluggish": slow,
				"swift":    quick,
				"middling": medium,
			},
			down: []string{"swift"},
			want: []string{"middling", "sluggish"},
		},
		{
			name:  "an unavailable unsampled provider does not lead",
			chain: []string{"newcomer", "swift"},
			sampled: map[string]time.Duration{
				"swift": quick,
			},
			down: []string{"newcomer"},
			want: []string{"swift"},
		},
		{
			name:  "no provider sampled yet keeps configured order",
			chain: []string{"groq", "gemini", "anthropic"},
			want:  []string{"groq", "gemini", "anthropic"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := healthWithDown(tc.down...)
			h.sampled = tc.sampled

			sel := mustSelector(t, router.StrategyLeastLatency)
			assertOrder(t, sel.Order(tc.chain, h, testNow), tc.want)
		})
	}
}

func TestLeastLatencyIsDeterministicAcrossCalls(t *testing.T) {
	chain := []string{"bravo", "alpha", "charlie", "newcomer", "delta"}
	h := stubHealth{sampled: map[string]time.Duration{
		"bravo":   200 * time.Millisecond,
		"alpha":   200 * time.Millisecond,
		"charlie": 50 * time.Millisecond,
		"delta":   200 * time.Millisecond,
	}}

	sel := mustSelector(t, router.StrategyLeastLatency)
	first := sel.Order(chain, h, testNow)
	for i := 1; i < 32; i++ {
		assertOrder(t, sel.Order(chain, h, testNow), first)
	}
}

func TestRoundRobinEachProviderLeadsExactlyOncePerCycle(t *testing.T) {
	chain := []string{"groq", "gemini", "anthropic", "openai"}
	h := stubHealth{}
	sel := mustSelector(t, router.StrategyRoundRobin)

	leads := make(map[string]int, len(chain))
	previous := ""
	for call := range chain {
		got := sel.Order(chain, h, testNow)
		if len(got) != len(chain) {
			t.Fatalf("call %d: order = %v, want all %d providers", call, got, len(chain))
		}
		if got[0] == previous {
			t.Errorf("call %d: %q led twice in a row, want a rotation", call, previous)
		}
		previous = got[0]
		leads[got[0]]++

		// The offset moves, the relative order does not.
		assertOrder(t, got, rotated(chain, slices.Index(chain, got[0])))
	}

	for _, name := range chain {
		if leads[name] != 1 {
			t.Errorf("%q led %d times over a full cycle of %d calls, want exactly 1", name, leads[name], len(chain))
		}
	}
}

func TestRoundRobinRotatesOverTheAvailableSubsetOnly(t *testing.T) {
	chain := []string{"groq", "gemini", "anthropic", "openai"}
	available := []string{"groq", "anthropic"}
	h := healthWithDown("gemini", "openai")
	sel := mustSelector(t, router.StrategyRoundRobin)

	leads := make(map[string]int, len(available))
	for call := range available {
		got := sel.Order(chain, h, testNow)
		assertPermutation(t, fmt.Sprintf("call %d", call), got, available)
		assertOrder(t, got, rotated(available, slices.Index(available, got[0])))
		leads[got[0]]++
	}

	for _, name := range available {
		if leads[name] != 1 {
			t.Errorf("%q led %d times over a full cycle of the available subset, want exactly 1", name, leads[name])
		}
	}
}

func TestRoundRobinIsSafeForConcurrentUse(t *testing.T) {
	// The counter is shared by every in flight request; CI runs this
	// under -race, which is the point of the test.
	const (
		workers   = 8
		callsEach = 64
	)

	chain := []string{"groq", "gemini", "anthropic"}
	h := stubHealth{}
	sel := mustSelector(t, router.StrategyRoundRobin)

	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range callsEach {
				got := sel.Order(chain, h, testNow)
				if len(got) != len(chain) {
					t.Errorf("order = %v, want %d providers", got, len(chain))
					return
				}
				if !slices.Equal(got, rotated(chain, slices.Index(chain, got[0]))) {
					t.Errorf("order = %v, want a rotation of %v", got, chain)
					return
				}
			}
		}()
	}
	wg.Wait()
}

func TestOrderExcludesUnavailableProvidersForEveryStrategy(t *testing.T) {
	chain := []string{"groq", "gemini", "anthropic", "openai"}
	h := healthWithDown("gemini", "openai")
	h.sampled = map[string]time.Duration{
		// The excluded ones look best on paper, which is the trap.
		"gemini":    10 * time.Millisecond,
		"openai":    20 * time.Millisecond,
		"groq":      900 * time.Millisecond,
		"anthropic": 950 * time.Millisecond,
	}

	for _, s := range allStrategies {
		t.Run(string(s), func(t *testing.T) {
			got := mustSelector(t, s).Order(chain, h, testNow)
			assertPermutation(t, string(s), got, []string{"groq", "anthropic"})
		})
	}
}

func TestOrderIsEmptyWhenEveryProviderIsUnavailable(t *testing.T) {
	chain := []string{"groq", "gemini", "anthropic"}
	h := healthWithDown(chain...)
	h.sampled = map[string]time.Duration{"groq": 5 * time.Millisecond}

	for _, s := range allStrategies {
		t.Run(string(s), func(t *testing.T) {
			got := mustSelector(t, s).Order(chain, h, testNow)
			if got == nil {
				t.Fatal("order = nil, want an empty non nil slice")
			}
			if len(got) != 0 {
				t.Fatalf("order = %v, want empty because the whole chain is sick", got)
			}
		})
	}
}

func TestOrderOfAnEmptyChainIsEmptyForEveryStrategy(t *testing.T) {
	for _, s := range allStrategies {
		t.Run(string(s), func(t *testing.T) {
			sel := mustSelector(t, s)
			for _, chain := range [][]string{nil, {}} {
				got := sel.Order(chain, stubHealth{}, testNow)
				if got == nil {
					t.Fatalf("order of %v = nil, want an empty non nil slice", chain)
				}
				if len(got) != 0 {
					t.Fatalf("order of %v = %v, want empty", chain, got)
				}
			}
		})
	}
}

func TestOrderDoesNotMutateTheCallerChain(t *testing.T) {
	original := []string{"sluggish", "swift", "newcomer", "middling"}
	h := stubHealth{sampled: map[string]time.Duration{
		"sluggish": 2 * time.Second,
		"swift":    40 * time.Millisecond,
		"middling": 300 * time.Millisecond,
	}}

	for _, s := range allStrategies {
		t.Run(string(s), func(t *testing.T) {
			chain := slices.Clone(original)
			mustSelector(t, s).Order(chain, h, testNow)
			assertOrder(t, chain, original)
		})
	}
}

// lcg is a deterministic generator for the property test. A fixed
// sequence means a failure reproduces on every machine and every run,
// with no seed to record and no clock to blame.
type lcg struct{ state uint64 }

func (g *lcg) next(n int) int {
	g.state = g.state*6364136223846793005 + 1442695040888963407
	return int((g.state >> 33) % uint64(n))
}

func TestOrderIsAPermutationOfTheAvailableSubsetForEveryStrategy(t *testing.T) {
	const (
		chains        = 128
		maxChainLen   = 8
		callsPerChain = 3
	)

	gen := &lcg{state: 0x9E3779B97F4A7C15}

	for c := range chains {
		size := gen.next(maxChainLen) + 1
		chain := make([]string, 0, size)
		h := stubHealth{
			down:    make(map[string]bool, size),
			sampled: make(map[string]time.Duration, size),
		}
		available := make([]string, 0, size)

		for p := range size {
			name := fmt.Sprintf("p%d_%d", c, p)
			chain = append(chain, name)
			if gen.next(3) == 0 {
				h.down[name] = true
			} else {
				available = append(available, name)
			}
			if gen.next(2) == 0 {
				h.sampled[name] = time.Duration(gen.next(500)) * time.Millisecond
			}
		}

		for _, s := range allStrategies {
			sel := mustSelector(t, s)
			// Repeat calls so the rotating strategy is checked at
			// several offsets, not only its first one.
			for call := range callsPerChain {
				ctx := fmt.Sprintf("strategy %q, chain %v, call %d", s, chain, call)
				assertPermutation(t, ctx, sel.Order(chain, h, testNow), available)
			}
		}
	}
}
