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

	if err := cfg.expandAPIKeys(); err != nil {
		return nil, err
	}
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config %s:\n%w", path, err)
	}
	return &cfg, nil
}

func (c *Config) expandAPIKeys() error {
	var errs []error
	for i := range c.Providers {
		p := &c.Providers[i]
		p.APIKey = envRefPattern.ReplaceAllStringFunc(p.APIKey, func(ref string) string {
			name := ref[2 : len(ref)-1]
			val, ok := os.LookupEnv(name)
			if !ok || val == "" {
				errs = append(errs, fmt.Errorf("provider %q: api_key references %s, which is unset or empty: %w", p.Name, name, ErrMissingEnv))
				return ref
			}
			return val
		})
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

	// An unauthenticated gateway is an open door to a paid API key, so
	// it may only listen where nobody else can reach it.
	if len(c.Auth.ClientKeys) == 0 && !isLoopbackListen(c.Server.Listen) {
		add("auth: no client_keys are configured, so server.listen must stay on loopback (127.0.0.1 or localhost), got %q; without keys any caller reaching this address spends the configured provider keys", c.Server.Listen)
	}
	for i, key := range c.Auth.ClientKeys {
		if len(key) < MinClientKeyLength {
			add("auth: client_keys[%d] is shorter than %d characters and is too weak to guard a paid key", i, MinClientKeyLength)
		}
	}

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

		switch {
		case r.Provider == "":
			add("%s: provider is required", label)
		case !providerNames[r.Provider]:
			add("%s: provider %q is not declared under providers", label, r.Provider)
		default:
			if models := providerModels[r.Provider]; models != nil && !models[r.Model] {
				add("%s: provider %q does not list model %q under models", label, r.Provider, r.Model)
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
