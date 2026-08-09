package obs

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// An empty tenant label reads on a dashboard as missing data rather than
// as a request that had no tenant. Two methods apply the rule through
// one helper, so a later edit can fix one and miss the other: both are
// asserted here for that reason, not for symmetry.
func TestAnonymousTenantIsLabelledNotLeftEmpty(t *testing.T) {
	m := NewMetrics()

	m.AddCost("", "groq", "llama", 0.5)
	m.AddCost("acme", "groq", "llama", 0.25)
	m.AddDenial("", "request_rate")
	m.AddDenial("acme", "request_rate")

	if got := testutil.ToFloat64(m.CostUSDTotal.WithLabelValues("anonymous", "groq", "llama")); got != 0.5 {
		t.Errorf("anonymous cost = %v, want 0.5", got)
	}
	if got := testutil.ToFloat64(m.CostUSDTotal.WithLabelValues("acme", "groq", "llama")); got != 0.25 {
		t.Errorf("named cost = %v, want 0.25", got)
	}
	if got := testutil.ToFloat64(m.DenialsTotal.WithLabelValues("anonymous", "request_rate")); got != 1 {
		t.Errorf("anonymous denials = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.DenialsTotal.WithLabelValues("acme", "request_rate")); got != 1 {
		t.Errorf("named denials = %v, want 1", got)
	}
}

// A negative cost is a bug upstream of here. Recording it would move a
// running total backwards, and creating the series at zero would be
// indistinguishable from a real zero cost request.
func TestNegativeCostCreatesNoSeries(t *testing.T) {
	m := NewMetrics()

	m.AddCost("acme", "groq", "llama", -1)
	if got := testutil.CollectAndCount(m.CostUSDTotal); got != 0 {
		t.Errorf("cost series = %d, want none created by a negative amount", got)
	}

	// And a genuine zero does create one, so an unpriced model is visibly
	// zero rather than a gap that reads as no traffic.
	m.AddCost("acme", "groq", "llama", 0)
	if got := testutil.CollectAndCount(m.CostUSDTotal); got != 1 {
		t.Errorf("cost series = %d, want a zero to be recorded", got)
	}
}

// Zero tokens in a direction means nothing flowed that way, which is not
// the same as flowing zero.
func TestTokensRecordOnlyTheDirectionsThatMoved(t *testing.T) {
	m := NewMetrics()

	m.AddTokens("groq", 0, 0)
	if got := testutil.CollectAndCount(m.TokensTotal); got != 0 {
		t.Errorf("token series = %d, want none when nothing moved", got)
	}

	m.AddTokens("groq", 5, 0)
	if got := testutil.CollectAndCount(m.TokensTotal); got != 1 {
		t.Errorf("token series = %d, want only the prompt direction", got)
	}
	if got := testutil.ToFloat64(m.TokensTotal.WithLabelValues("groq", DirectionPrompt)); got != 5 {
		t.Errorf("prompt tokens = %v, want 5", got)
	}
}

// The label sets differ on purpose: code belongs on the counter, where
// an operator counts failures, and stream on the histogram, where a
// streamed request's shape is different enough that mixing the two would
// make the percentiles meaningless. Neither carries the other, and a
// later tidy-up that "harmonises" them would quietly change what both
// mean.
func TestRequestLabelsAreDeliberatelyAsymmetric(t *testing.T) {
	m := NewMetrics()
	m.ObserveRequest("/v1/chat/completions", "groq", "200", 0.4, true)

	if got := testutil.ToFloat64(m.RequestsTotal.WithLabelValues("/v1/chat/completions", "groq", "200")); got != 1 {
		t.Errorf("counter = %v, want 1 recorded against the status code", got)
	}
	if got := testutil.CollectAndCount(m.RequestDurationSeconds); got != 1 {
		t.Errorf("duration series = %d, want 1", got)
	}
	// The histogram is keyed by stream rather than by code, so a second
	// request with a different status joins the same series.
	m.ObserveRequest("/v1/chat/completions", "groq", "500", 0.2, true)
	if got := testutil.CollectAndCount(m.RequestDurationSeconds); got != 1 {
		t.Errorf("duration series = %d after a second status code, want them sharing one series", got)
	}
	if got := testutil.CollectAndCount(m.RequestsTotal); got != 2 {
		t.Errorf("counter series = %d, want one per status code", got)
	}
}

func TestCacheEventsAreCountedByName(t *testing.T) {
	m := NewMetrics()
	m.AddCacheEvent("exact_hit")
	m.AddCacheEvent("exact_hit")
	m.AddCacheEvent("miss")

	if got := testutil.ToFloat64(m.CacheEventsTotal.WithLabelValues("exact_hit")); got != 2 {
		t.Errorf("exact_hit = %v, want 2", got)
	}
	if got := testutil.ToFloat64(m.CacheEventsTotal.WithLabelValues("miss")); got != 1 {
		t.Errorf("miss = %v, want 1", got)
	}
}
