package obs

import "strconv"

// These methods satisfy the ingress MetricsSink interface structurally.

func (m *Metrics) ObserveRequest(path, provider, code string, seconds float64, stream bool) {
	m.RequestsTotal.WithLabelValues(path, provider, code).Inc()
	m.RequestDurationSeconds.WithLabelValues(path, provider, strconv.FormatBool(stream)).Observe(seconds)
}

func (m *Metrics) ObserveTTFT(provider string, seconds float64) {
	m.TTFTSeconds.WithLabelValues(provider).Observe(seconds)
}

// AddCost records settled spend. An unpriced model still records a
// zero, so the series exists and its absence is visibly a zero rather
// than a gap that could be mistaken for no traffic.
func (m *Metrics) AddCost(tenant, provider, model string, usd float64) {
	if usd < 0 {
		return
	}
	m.CostUSDTotal.WithLabelValues(labelOrNone(tenant), provider, model).Add(usd)
}

// AddDenial records a refusal by the limit that caused it.
func (m *Metrics) AddDenial(tenant, reason string) {
	m.DenialsTotal.WithLabelValues(labelOrNone(tenant), reason).Inc()
}

// AddCacheEvent records a cache outcome. Hits, misses and refusals are
// separate events on purpose: a low hit rate caused by policy correctly
// refusing to cache is a different problem from one caused by the cache
// never seeing a repeat.
func (m *Metrics) AddCacheEvent(event string) {
	m.CacheEventsTotal.WithLabelValues(event).Inc()
}

// labelOrNone keeps an anonymous caller from producing an empty label,
// which reads as missing data rather than as a request with no tenant.
func labelOrNone(tenant string) string {
	if tenant == "" {
		return "anonymous"
	}
	return tenant
}

func (m *Metrics) AddTokens(provider string, prompt, completion int) {
	if prompt > 0 {
		m.TokensTotal.WithLabelValues(provider, DirectionPrompt).Add(float64(prompt))
	}
	if completion > 0 {
		m.TokensTotal.WithLabelValues(provider, DirectionCompletion).Add(float64(completion))
	}
}
