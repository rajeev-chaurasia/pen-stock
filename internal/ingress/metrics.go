package ingress

// MetricsSink receives request-level measurements from the ingress. It is
// satisfied structurally by obs.Metrics so ingress stays free of metric
// library imports.
type MetricsSink interface {
	ObserveRequest(path, provider, code string, seconds float64, stream bool)
	ObserveTTFT(provider string, seconds float64)
	AddTokens(provider string, prompt, completion int)
	// AddCost records settled spend, which is the only signal that says
	// what traffic costs rather than how much of it there is.
	AddCost(tenant, provider, model string, usd float64)
	// AddDenial records a request refused by a tenant limit.
	AddDenial(tenant, reason string)
	// AddCacheEvent records a cache hit, miss, or policy refusal.
	AddCacheEvent(event string)
}

// streamAnswerer is implemented by a stream reader that knows which
// upstream produced it. A routed model can be served by any provider in
// its fallback chain, and cost belongs to whoever actually answered.
type streamAnswerer interface {
	AnsweringProvider() string
}

// providerOfStream reports the upstream behind a reader, or empty when
// the reader does not track one.
func providerOfStream(r any) string {
	if a, ok := r.(streamAnswerer); ok {
		return a.AnsweringProvider()
	}
	return ""
}

// answeringProvider prefers the upstream that answered and falls back to
// the route label when nothing more specific is known.
func answeringProvider(routeLabel, answered string) string {
	if answered != "" {
		return answered
	}
	return routeLabel
}

type noopSink struct{}

func (noopSink) ObserveRequest(string, string, string, float64, bool) {}
func (noopSink) ObserveTTFT(string, float64)                          {}
func (noopSink) AddTokens(string, int, int)                           {}
func (noopSink) AddCost(string, string, string, float64)              {}
func (noopSink) AddDenial(string, string)                             {}
func (noopSink) AddCacheEvent(string)                                 {}

// knownPaths bounds the path label so unmatched URLs cannot explode
// metric cardinality.
var knownPaths = map[string]struct{}{
	"/v1/chat/completions": {},
	"/v1/models":           {},
	"/healthz":             {},
}

const pathOther = "other"

// NormalizePath collapses unrecognized paths to a single label. Callers
// tagging telemetry with a client supplied path need this: a scanner
// walking random URLs would otherwise mint unbounded label or span name
// cardinality in the backend.
func NormalizePath(path string) string { return normalizePath(path) }

func normalizePath(path string) string {
	if _, ok := knownPaths[path]; ok {
		return path
	}
	return pathOther
}
