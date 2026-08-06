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

func (m *Metrics) AddTokens(provider string, prompt, completion int) {
	if prompt > 0 {
		m.TokensTotal.WithLabelValues(provider, DirectionPrompt).Add(float64(prompt))
	}
	if completion > 0 {
		m.TokensTotal.WithLabelValues(provider, DirectionCompletion).Add(float64(completion))
	}
}
