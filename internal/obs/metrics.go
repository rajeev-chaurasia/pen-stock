package obs

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Values for the direction label on TokensTotal.
const (
	DirectionPrompt     = "prompt"
	DirectionCompletion = "completion"
)

var (
	// requestDurationBuckets spans fast cache hits through long streamed
	// completions, 5ms to 120s.
	requestDurationBuckets = prometheus.ExponentialBucketsRange(0.005, 120, 14)
	// ttftBuckets spans time to first token, 25ms to 30s.
	ttftBuckets = prometheus.ExponentialBucketsRange(0.025, 30, 12)
)

// Metrics holds all penstock instruments on a private registry so tests
// and multiple instances never collide on package globals.
type Metrics struct {
	registry *prometheus.Registry

	RequestsTotal          *prometheus.CounterVec
	RequestDurationSeconds *prometheus.HistogramVec
	TTFTSeconds            *prometheus.HistogramVec
	TokensTotal            *prometheus.CounterVec
	CacheEventsTotal       *prometheus.CounterVec
	// CostUSDTotal is the money side of the same traffic the counters
	// above measure. Tokens tell an operator how busy the gateway is;
	// only this tells them what it is costing.
	CostUSDTotal *prometheus.CounterVec
	// DenialsTotal counts refusals by the limit that refused, which is
	// how an operator tells a tenant hitting its ceiling apart from one
	// being rate limited.
	DenialsTotal *prometheus.CounterVec
}

// NewMetrics builds and registers every instrument on a fresh registry.
func NewMetrics() *Metrics {
	m := &Metrics{
		registry: prometheus.NewRegistry(),
		RequestsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "penstock_requests_total",
			Help: "Requests handled, by path, provider, and HTTP status code.",
		}, []string{"path", "provider", "code"}),
		RequestDurationSeconds: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "penstock_request_duration_seconds",
			Help:    "End to end request duration in seconds.",
			Buckets: requestDurationBuckets,
		}, []string{"path", "provider", "stream"}),
		TTFTSeconds: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "penstock_ttft_seconds",
			Help:    "Time to first token in seconds for streamed responses.",
			Buckets: ttftBuckets,
		}, []string{"provider"}),
		TokensTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "penstock_tokens_total",
			Help: "Tokens processed, by provider and direction (prompt or completion).",
		}, []string{"provider", "direction"}),
		CostUSDTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "penstock_cost_usd_total",
			Help: "Settled cost in USD, by tenant, provider, and model.",
		}, []string{"tenant", "provider", "model"}),
		DenialsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "penstock_denials_total",
			Help: "Requests refused by a tenant limit, by tenant and reason.",
		}, []string{"tenant", "reason"}),
		CacheEventsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "penstock_cache_events_total",
			Help: "Cache events by type. Registered ahead of the caching phase.",
		}, []string{"event"}),
	}
	// Go runtime series belong here for one reason: a soak looking for a
	// leak needs heap and goroutine counts, and without them the only
	// evidence available is resident set size sampled from outside,
	// which cannot tell a leak from a fragmented allocator. The admin
	// listener is loopback by default, so this is operator data on an
	// operator surface.
	m.registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	m.registry.MustRegister(
		m.RequestsTotal,
		m.RequestDurationSeconds,
		m.TTFTSeconds,
		m.TokensTotal,
		m.CostUSDTotal,
		m.DenialsTotal,
		m.CacheEventsTotal,
	)
	return m
}

// Handler exposes the private registry in Prometheus text format.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}
