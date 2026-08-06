package config

// ProviderKind selects the adapter implementation for a configured backend.
type ProviderKind string

const (
	// KindGroq targets the Groq OpenAI-compatible API.
	KindGroq ProviderKind = "groq"
	// KindOpenAICompat targets any OpenAI-wire endpoint, including llmsim.
	KindOpenAICompat ProviderKind = "openai_compat"
)

type Config struct {
	Server    ServerConfig     `yaml:"server"`
	Providers []ProviderConfig `yaml:"providers"`
	Routes    []RouteConfig    `yaml:"routes"`
	Telemetry TelemetryConfig  `yaml:"telemetry"`
}

type ServerConfig struct {
	Listen              string `yaml:"listen"`
	ReadTimeoutMS       int    `yaml:"read_timeout_ms"`
	UpstreamTimeoutMS   int    `yaml:"upstream_timeout_ms"`
	StreamIdleTimeoutMS int    `yaml:"stream_idle_timeout_ms"`
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
	Model    string `yaml:"model"`
	Provider string `yaml:"provider"`
}

type TelemetryConfig struct {
	ServiceName  string `yaml:"service_name"`
	OTLPEndpoint string `yaml:"otlp_endpoint"`
	LogLevel     string `yaml:"log_level"`
}
