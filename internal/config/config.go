// Package config loads and validates the Penstock gateway configuration.
package config

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"regexp"
	"slices"

	"gopkg.in/yaml.v3"
)

// ErrMissingEnv reports an api_key ${VAR} reference whose variable is
// unset or empty. Empty counts as missing because an empty key boots a
// gateway whose every upstream call fails auth.
var ErrMissingEnv = errors.New("environment variable not set")

// Defaults applied by Load when the corresponding field is absent.
const (
	// DefaultListen is loopback on purpose. Reaching a wider audience is
	// a deliberate act that requires configuring auth.client_keys first.
	DefaultListen              = "127.0.0.1:8080"
	DefaultReadTimeoutMS       = 30000
	DefaultUpstreamTimeoutMS   = 120000
	DefaultStreamIdleTimeoutMS = 60000
	DefaultServiceName         = "penstock"
	DefaultLogLevel            = LogLevelInfo
	DefaultAdminListen         = "127.0.0.1:9090"
	DefaultMaxInflight         = 256
)

// MaxTimeoutMS caps every *_timeout_ms field at one hour. Larger values
// are almost certainly a unit mistake and would leave connections pinned
// long past any sane deadline.
const MaxTimeoutMS int = 3600000

// MinClientKeyLength is the shortest client key worth calling a secret.
const MinClientKeyLength = 16

// isLoopbackListen reports whether addr binds only the loopback
// interface. An empty or wildcard host reaches every interface.
func isLoopbackListen(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Accepted telemetry log levels.
const (
	LogLevelDebug = "debug"
	LogLevelInfo  = "info"
	LogLevelWarn  = "warn"
	LogLevelError = "error"
)

var envRefPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// tenantNamePattern is the alphabet a tenant name may use. The name ends
// up as a metrics label and a log field, where whitespace and separators
// either need quoting or silently split the value.
var tenantNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// Load reads the YAML file at path, expands ${VAR} references in provider
// api_key fields, fills in defaults, and validates the result. Unknown YAML
// fields are rejected.
func Load(path string) (*Config, error) {
	// The path comes from the operator's own command line, so there is
	// no untrusted input to traverse with.
	// #nosec G304
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open config: %w", err)
	}
	defer func() { _ = f.Close() }()

	var cfg Config
	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}

	if err := cfg.expandSecrets(); err != nil {
		return nil, err
	}
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config %s:\n%w", path, err)
	}
	return &cfg, nil
}

// expandSecrets resolves ${VAR} references in every field that holds a
// secret. Client and tenant keys get the same treatment as provider keys
// because they are what stands between a caller and the provider keys.
func (c *Config) expandSecrets() error {
	return errors.Join(c.expandAPIKeys(), c.expandClientKeys(), c.expandTenantKeys())
}

// expandEnvRefs replaces every ${VAR} in s with its value. A variable
// that is unset or set to empty is reported through describe, which
// names the offending field; an empty value is a failure rather than a
// blank because a ${VAR:-} compose default would otherwise boot a
// gateway whose every call fails auth.
func expandEnvRefs(s string, describe func(varName string) error) (string, []error) {
	var errs []error
	expanded := envRefPattern.ReplaceAllStringFunc(s, func(ref string) string {
		name := ref[2 : len(ref)-1]
		val, ok := os.LookupEnv(name)
		if !ok || val == "" {
			errs = append(errs, describe(name))
			return ref
		}
		return val
	})
	return expanded, errs
}

func (c *Config) expandClientKeys() error {
	var errs []error
	for i, key := range c.Auth.ClientKeys {
		expanded, refErrs := expandEnvRefs(key, func(varName string) error {
			return fmt.Errorf("auth: client_keys[%d] references %s, which is unset or empty: %w", i, varName, ErrMissingEnv)
		})
		c.Auth.ClientKeys[i] = expanded
		errs = append(errs, refErrs...)
	}
	return errors.Join(errs...)
}

func (c *Config) expandTenantKeys() error {
	var errs []error
	for i := range c.Auth.Tenants {
		t := &c.Auth.Tenants[i]
		for j, key := range t.Keys {
			expanded, refErrs := expandEnvRefs(key, func(varName string) error {
				return fmt.Errorf("auth: tenant %q: keys[%d] references %s, which is unset or empty: %w", t.Name, j, varName, ErrMissingEnv)
			})
			t.Keys[j] = expanded
			errs = append(errs, refErrs...)
		}
	}
	return errors.Join(errs...)
}

func (c *Config) expandAPIKeys() error {
	var errs []error
	for i := range c.Providers {
		p := &c.Providers[i]
		expanded, refErrs := expandEnvRefs(p.APIKey, func(varName string) error {
			return fmt.Errorf("provider %q: api_key references %s, which is unset or empty: %w", p.Name, varName, ErrMissingEnv)
		})
		p.APIKey = expanded
		errs = append(errs, refErrs...)
	}
	return errors.Join(errs...)
}

func (c *Config) applyDefaults() {
	if c.Server.Listen == "" {
		c.Server.Listen = DefaultListen
	}
	if c.Server.ReadTimeoutMS == 0 {
		c.Server.ReadTimeoutMS = DefaultReadTimeoutMS
	}
	if c.Server.UpstreamTimeoutMS == 0 {
		c.Server.UpstreamTimeoutMS = DefaultUpstreamTimeoutMS
	}
	if c.Server.StreamIdleTimeoutMS == 0 {
		c.Server.StreamIdleTimeoutMS = DefaultStreamIdleTimeoutMS
	}
	if c.Server.AdminListen == "" {
		c.Server.AdminListen = DefaultAdminListen
	}
	if c.Server.MaxInflight == 0 {
		c.Server.MaxInflight = DefaultMaxInflight
	}
	if c.Telemetry.ServiceName == "" {
		c.Telemetry.ServiceName = DefaultServiceName
	}
	if c.Telemetry.LogLevel == "" {
		c.Telemetry.LogLevel = DefaultLogLevel
	}
}

// hasKeys reports whether any caller has to authenticate at all. A
// tenant key is a credential like any other, so a deployment that
// declares only tenants is still closed to strangers.
func (a AuthConfig) hasKeys() bool {
	if len(a.ClientKeys) > 0 {
		return true
	}
	for _, t := range a.Tenants {
		if len(t.Keys) > 0 {
			return true
		}
	}
	return false
}

// validateAuth reports every problem with the configured keys through
// add. No message ever contains a key: naming the tenant and the index
// is enough to fix the file, and error text reaches logs.
func (c *Config) validateAuth(add func(format string, args ...any)) {
	// An unauthenticated gateway is an open door to a paid API key, so
	// it may only listen where nobody else can reach it.
	if !c.Auth.hasKeys() && !isLoopbackListen(c.Server.Listen) {
		add("auth: no client_keys and no tenant keys are configured, so server.listen must stay on loopback (127.0.0.1 or localhost), got %q; without keys any caller reaching this address spends the configured provider keys", c.Server.Listen)
	}

	// clientKeys and tenantOfKey record who already claims each key, so
	// the report lands on the tenant that repeated it.
	clientKeys := make(map[string]bool, len(c.Auth.ClientKeys))
	for i, key := range c.Auth.ClientKeys {
		if len(key) < MinClientKeyLength {
			add("auth: client_keys[%d] is shorter than %d characters and is too weak to guard a paid key", i, MinClientKeyLength)
		}
		clientKeys[key] = true
	}
	tenantOfKey := make(map[string]string)

	names := make(map[string]bool, len(c.Auth.Tenants))
	for i, t := range c.Auth.Tenants {
		label := fmt.Sprintf("tenant %q", t.Name)
		switch {
		case t.Name == "":
			label = fmt.Sprintf("tenants[%d]", i)
			add("auth: %s: name is required; it is how spend is attributed", label)
		case names[t.Name]:
			add("auth: %s: duplicate name, tenant names must be unique", label)
		case !tenantNamePattern.MatchString(t.Name):
			add("auth: %s: name must use only letters, digits, underscore and hyphen, with no whitespace, because it becomes a metrics label", label)
		}
		names[t.Name] = true

		if len(t.Keys) == 0 {
			add("auth: %s: keys is empty; a tenant with no keys can never be reached", label)
		}
		for j, key := range t.Keys {
			if len(key) < MinClientKeyLength {
				add("auth: %s: keys[%d] is shorter than %d characters and is too weak to guard a paid key", label, j, MinClientKeyLength)
			}
			switch owner, taken := tenantOfKey[key]; {
			case clientKeys[key]:
				add("auth: %s: keys[%d] also appears in auth.client_keys, which leaves the caller's identity ambiguous", label, j)
			case taken:
				add("auth: %s: keys[%d] already belongs to tenant %q, and a key may identify only one tenant", label, j, owner)
			default:
				tenantOfKey[key] = t.Name
			}
		}

		for _, lim := range []struct {
			name  string
			value float64
		}{
			{"requests_per_minute", float64(t.Limits.RequestsPerMinute)},
			{"tokens_per_minute", float64(t.Limits.TokensPerMinute)},
			{"daily_usd", t.Limits.DailyUSD},
			{"monthly_usd", t.Limits.MonthlyUSD},
		} {
			if lim.value < 0 {
				add("auth: %s: %s is %v, must not be negative; omit it or use 0 for unlimited", label, lim.name, lim.value)
			}
		}
	}
}

// Validate checks the whole config and reports every problem found, joined
// into a single multi-line error.
func (c *Config) Validate() error {
	var errs []error
	add := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf(format, args...))
	}

	// Zero timeouts never reach here through Load, which replaces them
	// with defaults first; negative values would silently disable the
	// deadline, so they are rejected rather than defaulted.
	for _, tmo := range []struct {
		name  string
		value int
	}{
		{"read_timeout_ms", c.Server.ReadTimeoutMS},
		{"upstream_timeout_ms", c.Server.UpstreamTimeoutMS},
		{"stream_idle_timeout_ms", c.Server.StreamIdleTimeoutMS},
	} {
		switch {
		case tmo.value < 0:
			add("server: %s is %d; a negative timeout disables the deadline entirely, use a positive value or omit the field for the default", tmo.name, tmo.value)
		case tmo.value > MaxTimeoutMS:
			add("server: %s is %d, must be at most %d (one hour); values are milliseconds", tmo.name, tmo.value, MaxTimeoutMS)
		}
	}

	if c.Server.MaxInflight < 0 {
		add("server: max_inflight is %d, must not be negative", c.Server.MaxInflight)
	}

	c.validateAuth(add)

	if len(c.Providers) == 0 {
		add("providers: at least one provider is required")
	}
	providerNames := make(map[string]bool, len(c.Providers))
	providerModels := make(map[string]map[string]bool, len(c.Providers))
	for i, p := range c.Providers {
		label := fmt.Sprintf("provider %q", p.Name)
		if p.Name == "" {
			label = fmt.Sprintf("providers[%d]", i)
			add("%s: name is required", label)
		} else if providerNames[p.Name] {
			add("%s: duplicate name, provider names must be unique", label)
		}
		providerNames[p.Name] = true

		if !slices.Contains(AllKinds, p.Kind) {
			add("%s: unknown kind %q, must be one of %v", label, p.Kind, AllKinds)
		}

		if p.BaseURL == "" {
			add("%s: base_url is required", label)
		} else if u, err := url.Parse(p.BaseURL); err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			add("%s: base_url %q is not a valid http(s) URL", label, p.BaseURL)
		}

		if p.APIKey == "" {
			add("%s: api_key is required and must not be empty", label)
		}

		if len(p.Models) > 0 {
			set := make(map[string]bool, len(p.Models))
			for _, m := range p.Models {
				set[m] = true
			}
			providerModels[p.Name] = set
		}
	}

	if len(c.Routes) == 0 {
		add("routes: at least one route is required")
	}
	routeModels := make(map[string]bool, len(c.Routes))
	for i, r := range c.Routes {
		label := fmt.Sprintf("route %q", r.Model)
		if r.Model == "" {
			label = fmt.Sprintf("routes[%d]", i)
			add("%s: model is required", label)
		} else if routeModels[r.Model] {
			add("%s: duplicate model, route models must be unique", label)
		}
		routeModels[r.Model] = true

		if r.Provider != "" && len(r.Providers) > 0 {
			add("%s: set either provider or providers, not both; naming both leaves the order ambiguous", label)
		}
		chain := r.Chain()
		if len(chain) == 0 {
			add("%s: provider or providers is required", label)
		}

		seen := make(map[string]bool, len(chain))
		for _, name := range chain {
			switch {
			case seen[name]:
				add("%s: provider %q appears twice in the chain", label, name)
			case !providerNames[name]:
				add("%s: provider %q is not declared under providers", label, name)
			default:
				upstream := r.UpstreamModel(name)
				if models := providerModels[name]; models != nil && !models[upstream] {
					add("%s: provider %q does not list model %q under models", label, name, upstream)
				}
			}
			seen[name] = true
		}

		if r.Strategy != "" && !slices.Contains(AllStrategies, r.Strategy) {
			add("%s: strategy %q must be one of %v", label, r.Strategy, AllStrategies)
		}
		// A rename for a provider that does not serve this route is
		// almost always a typo in one of the two names.
		for name := range r.ProviderModels {
			if !seen[name] {
				add("%s: provider_models names %q, which is not in this route's chain", label, name)
			}
		}
	}

	switch c.Telemetry.LogLevel {
	case LogLevelDebug, LogLevelInfo, LogLevelWarn, LogLevelError:
	default:
		add("telemetry: log_level %q must be one of %s, %s, %s, %s",
			c.Telemetry.LogLevel, LogLevelDebug, LogLevelInfo, LogLevelWarn, LogLevelError)
	}

	return errors.Join(errs...)
}
