package ingress

// MetricsSink receives request-level measurements from the ingress. It is
// satisfied structurally by obs.Metrics so ingress stays free of metric
// library imports.
type MetricsSink interface {
	ObserveRequest(path, provider, code string, seconds float64, stream bool)
	ObserveTTFT(provider string, seconds float64)
	AddTokens(provider string, prompt, completion int)
}

type noopSink struct{}

func (noopSink) ObserveRequest(string, string, string, float64, bool) {}
func (noopSink) ObserveTTFT(string, float64)                          {}
func (noopSink) AddTokens(string, int, int)                           {}

// knownPaths bounds the path label so unmatched URLs cannot explode
// metric cardinality.
var knownPaths = map[string]struct{}{
	"/v1/chat/completions": {},
	"/v1/models":           {},
	"/healthz":             {},
}

const pathOther = "other"

func normalizePath(path string) string {
	if _, ok := knownPaths[path]; ok {
		return path
	}
	return pathOther
}
