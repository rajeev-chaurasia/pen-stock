package config

import "slices"

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
	Server     ServerConfig     `yaml:"server"`
	Auth       AuthConfig       `yaml:"auth"`
	Accounting AccountingConfig `yaml:"accounting"`
	Cache      CacheConfig      `yaml:"cache"`
	Providers  []ProviderConfig `yaml:"providers"`
	Routes     []RouteConfig    `yaml:"routes"`
	Telemetry  TelemetryConfig  `yaml:"telemetry"`
}

// CacheConfig controls answering a request from a previous one.
// Disabled by default: a cache is a correctness decision as much as a
// performance one, so it is opted into rather than inherited.
type CacheConfig struct {
	Enabled bool `yaml:"enabled"`
	// MaxEntries bounds the exact tier per gateway. Zero takes a
	// documented default rather than meaning unlimited, since an
	// unbounded cache is a memory leak with a friendly name.
	MaxEntries int `yaml:"max_entries"`
	// TTLSeconds is how long an answer stays usable.
	TTLSeconds int `yaml:"ttl_seconds"`
	// MaxTemperature is the highest temperature considered reproducible
	// enough to cache. Above it a caller asked for variety, and serving
	// a stored answer would quietly override them.
	MaxTemperature float64 `yaml:"max_temperature"`
	// Semantic answers a question close to one already asked. It needs
	// an embedder, and it is the tier that can be wrong, so it is a
	// separate switch from the exact tier.
	Semantic SemanticCacheConfig `yaml:"semantic"`
}

// SemanticCacheConfig configures similarity matching.
type SemanticCacheConfig struct {
	Enabled bool `yaml:"enabled"`
	// Threshold is the cosine similarity below which a neighbour is not
	// considered the same question. Lower means more hits and more
	// chances to answer something that was never asked.
	Threshold float64 `yaml:"threshold"`
	// EmbedModel and EmbedAPIKey reach the embedding service. The key
	// supports ${ENV_VAR} like every other secret in this file.
	EmbedModel  string `yaml:"embed_model"`
	EmbedAPIKey string `yaml:"embed_api_key"`
	EmbedURL    string `yaml:"embed_url"`
	// MaxPerTenant bounds stored vectors per tenant.
	MaxPerTenant int `yaml:"max_per_tenant"`
}

// AccountingConfig controls the record of what was spent, as opposed to
// the limits on spending it.
type AccountingConfig struct {
	// LedgerPath is where settled requests are appended as JSON lines.
	// Empty disables the ledger, in which case spend is still enforced
	// but only the running totals survive, and a restart forgets which
	// requests produced them.
	LedgerPath string `yaml:"ledger_path"`
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

// AuthConfig lists the keys callers must present as a bearer token,
// either anonymously or under a named tenant. With no keys of either
// kind the gateway serves anyone who can reach it, which is why Validate
// refuses that combination on a non-loopback listener.
type AuthConfig struct {
	// ClientKeys are anonymous: they open the gateway but carry no
	// identity and no limits of their own.
	ClientKeys []string `yaml:"client_keys"`
	// Tenants name their keys and give them limits. A tenant key
	// authenticates exactly like a client key, so a deployment may use
	// tenants alone.
	Tenants []TenantConfig `yaml:"tenants"`
}

// TenantConfig is one billable identity and the keys that speak for it.
// A key belongs to a single tenant, because a request that could be
// billed to either of two tenants can be billed to neither.
type TenantConfig struct {
	// Name is the only part of a tenant safe to put in a metric or a
	// log line, which is why it is restricted to a label-safe alphabet.
	Name string   `yaml:"name"`
	Keys []string `yaml:"keys"`
	// Limits are inline so a tenant reads as one flat YAML block
	// instead of burying the numbers under a nested key.
	Limits TenantLimits `yaml:",inline"`
}

// TenantLimits is what one tenant may consume. It mirrors the budget
// package's own Limits rather than reusing it: config sits below every
// other package and cannot import one that imports it back. The wiring
// layer converts between the two.
type TenantLimits struct {
	// RequestsPerMinute caps request rate. Zero means unlimited, as it
	// does for every limit below, so a partially configured tenant
	// stays usable instead of being locked out by omission.
	RequestsPerMinute int `yaml:"requests_per_minute"`
	// TokensPerMinute caps token throughput, prompt plus completion.
	// Zero means unlimited.
	TokensPerMinute int `yaml:"tokens_per_minute"`
	// DailyUSD caps spend over a rolling day. Zero means unlimited.
	DailyUSD float64 `yaml:"daily_usd"`
	// MonthlyUSD caps spend over a rolling month. Zero means unlimited.
	MonthlyUSD float64 `yaml:"monthly_usd"`
	// FailClosed decides what happens when the accounting store cannot
	// answer. True denies the request, which is what a hard cap on real
	// money needs; false allows it and leaves an alert behind.
	FailClosed bool `yaml:"fail_closed"`
}

// TenantForKey reports which tenant owns key, if any. Keys under
// client_keys have no tenant and miss here.
//
// This walks the configured tenants in order and compares in variable
// time, so it belongs at startup: the caller indexes the tenants once
// into whatever constant time matcher it uses per request, and that
// comparison is deliberately not implemented here.
func (c *Config) TenantForKey(key string) (name string, limits TenantLimits, ok bool) {
	// A caller presenting nothing must not match a misconfigured empty
	// key, even though Validate rejects one.
	if key == "" {
		return "", TenantLimits{}, false
	}
	for _, t := range c.Auth.Tenants {
		if slices.Contains(t.Keys, key) {
			return t.Name, t.Limits, true
		}
	}
	return "", TenantLimits{}, false
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
	// StreamUsage overrides whether streaming requests ask the backend
	// for token usage. Unset follows the kind's own default, which is
	// off for openai_compat because that kind fronts arbitrary self
	// hosted software and some of it rejects unknown fields.
	//
	// Turn it on for a backend that supports it. llama.cpp's server and
	// vLLM both do, and without it a streamed request to them reports no
	// tokens, which means budgets cannot bill it and the ledger records
	// a completion that appears to have cost nothing.
	StreamUsage *bool `yaml:"stream_usage"`
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
