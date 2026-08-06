package config

// ProviderKind selects the adapter implementation for a configured backend.
type ProviderKind string

const (
	// KindGroq targets the Groq OpenAI-compatible API.
	KindGroq ProviderKind = "groq"
	// KindOpenAICompat targets any OpenAI-wire endpoint, including llmsim.
	KindOpenAICompat ProviderKind = "openai_compat"
	// KindOpenAI targets the OpenAI API.
	KindOpenAI ProviderKind = "openai"
	// KindCerebras targets the Cerebras inference API.
	KindCerebras ProviderKind = "cerebras"
	// KindMistral targets Mistral La Plateforme.
	KindMistral ProviderKind = "mistral"
	// KindOpenRouter targets the OpenRouter aggregator.
	KindOpenRouter ProviderKind = "openrouter"
	// KindGemini targets Google's Gemini API, which does not speak the
	// OpenAI wire format and is translated by its own adapter.
	KindGemini ProviderKind = "gemini"
	// KindAnthropic targets the Anthropic Messages API, likewise
	// translated by its own adapter.
	KindAnthropic ProviderKind = "anthropic"
)

// AllKinds lists every kind the gateway can build. Validation uses it so
// a new adapter shows up in error messages without extra wiring.
var AllKinds = []ProviderKind{
	KindGroq, KindOpenAICompat, KindOpenAI, KindCerebras,
	KindMistral, KindOpenRouter, KindGemini, KindAnthropic,
}

type Config struct {
	Server    ServerConfig     `yaml:"server"`
	Auth      AuthConfig       `yaml:"auth"`
	Providers []ProviderConfig `yaml:"providers"`
	Routes    []RouteConfig    `yaml:"routes"`
	Telemetry TelemetryConfig  `yaml:"telemetry"`
}

type ServerConfig struct {
	Listen              string `yaml:"listen"`
	ReadTimeoutMS       int    `yaml:"read_timeout_ms"`
	UpstreamTimeoutMS   int    `yaml:"upstream_timeout_ms"`
	StreamIdleTimeoutMS int    `yaml:"stream_idle_timeout_ms"`
	// AdminListen serves metrics and other operator surfaces. It stays
	// off the public listener because token spend and latency profiles
	// are not callers' business.
	AdminListen string `yaml:"admin_listen"`
	// MaxInflight bounds concurrent requests. Each one can hold a
	// request body plus an upstream response, so no bound means no
	// memory ceiling.
	MaxInflight int `yaml:"max_inflight"`
}

// AuthConfig lists the keys callers must present as a bearer token.
// With no keys the gateway serves anyone who can reach it, which is
// why Validate refuses that combination on a non-loopback listener.
type AuthConfig struct {
	ClientKeys []string `yaml:"client_keys"`
}

type ProviderConfig struct {
	Name    string       `yaml:"name"`
	Kind    ProviderKind `yaml:"kind"`
	BaseURL string       `yaml:"base_url"`
	// APIKey supports ${ENV_VAR} expansion at load time so keys never
	// live in the config file itself.
	APIKey string `yaml:"api_key"`
	// Models restricts which route models this provider may serve.
	// Empty means unrestricted: any route may target the provider.
	Models []string `yaml:"models"`
}

type RouteConfig struct {
	Model string `yaml:"model"`
	// Provider serves this model alone. Mutually exclusive with
	// Providers: naming both leaves the intended order ambiguous.
	Provider string `yaml:"provider"`
	// Providers is a fallback chain, tried in the order written unless
	// Strategy says otherwise.
	Providers []string `yaml:"providers"`
	// Strategy orders the chain. Empty means priority order.
	Strategy string `yaml:"strategy"`
	// ProviderModels renames the model per provider, keyed by provider
	// name. A chain over independent free tiers rarely shares a
	// vocabulary, so without this the second provider in such a chain
	// would be asked for a model it has never heard of. A provider left
	// out of the map is asked for the route's own model name.
	ProviderModels map[string]string `yaml:"provider_models"`
}

// UpstreamModel reports the model name to ask provider for on this
// route, falling back to the route's own name.
func (r RouteConfig) UpstreamModel(provider string) string {
	if m, ok := r.ProviderModels[provider]; ok && m != "" {
		return m
	}
	return r.Model
}

// Chain returns the providers serving this route, in configured order,
// whichever spelling the operator used.
func (r RouteConfig) Chain() []string {
	if len(r.Providers) > 0 {
		return r.Providers
	}
	if r.Provider != "" {
		return []string{r.Provider}
	}
	return nil
}

// Route strategy names. These mirror the router package, which cannot
// be imported here, so a test in the router package pins them together.
const (
	StrategyPriority     = "priority"
	StrategyLeastLatency = "least_latency"
	StrategyRoundRobin   = "round_robin"
)

// AllStrategies lists every accepted route strategy.
var AllStrategies = []string{StrategyPriority, StrategyLeastLatency, StrategyRoundRobin}

type TelemetryConfig struct {
	ServiceName  string `yaml:"service_name"`
	OTLPEndpoint string `yaml:"otlp_endpoint"`
	LogLevel     string `yaml:"log_level"`
}
