package obs

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMetricsHandlerServesRegisteredMetrics(t *testing.T) {
	m := NewMetrics()
	m.RequestsTotal.WithLabelValues("/v1/chat/completions", "groq", "200").Inc()
	m.RequestDurationSeconds.WithLabelValues("/v1/chat/completions", "groq", "false").Observe(0.42)
	m.TTFTSeconds.WithLabelValues("groq").Observe(0.1)
	m.TokensTotal.WithLabelValues("groq", DirectionPrompt).Add(128)
	m.CacheEventsTotal.WithLabelValues("miss").Inc()

	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("metrics endpoint returned %d, want %d", rec.Code, http.StatusOK)
	}
	body := rec.Body.String()

	tests := []struct {
		name   string
		metric string
	}{
		{name: "requests counter", metric: "penstock_requests_total"},
		{name: "duration histogram", metric: "penstock_request_duration_seconds"},
		{name: "ttft histogram", metric: "penstock_ttft_seconds"},
		{name: "tokens counter", metric: "penstock_tokens_total"},
		{name: "cache events counter", metric: "penstock_cache_events_total"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !strings.Contains(body, tt.metric) {
				t.Errorf("metrics output missing %q", tt.metric)
			}
		})
	}
}

func TestMetricsPrivateRegistry(t *testing.T) {
	// Two instances must register without a duplicate-registration panic,
	// which proves nothing leaks onto the default registry.
	a := NewMetrics()
	b := NewMetrics()
	if a.registry == b.registry {
		t.Fatal("expected each Metrics to own a distinct registry")
	}
}
