// Package config loads and validates the Penstock gateway configuration.
package config

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"

	"gopkg.in/yaml.v3"
)

// ErrMissingEnv reports an api_key ${VAR} reference whose variable is unset.
var ErrMissingEnv = errors.New("environment variable not set")

// Defaults applied by Load when the corresponding field is absent.
const (
	DefaultListen              = ":8080"
	DefaultReadTimeoutMS       = 30000
	DefaultUpstreamTimeoutMS   = 120000
	DefaultStreamIdleTimeoutMS = 60000
	DefaultServiceName         = "penstock"
	DefaultLogLevel            = LogLevelInfo
)

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
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open config: %w", err)
	}
	defer f.Close()

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
			if !ok {
				errs = append(errs, fmt.Errorf("provider %q: api_key references %s: %w", p.Name, name, ErrMissingEnv))
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

	if len(c.Providers) == 0 {
		add("providers: at least one provider is required")
	}
	providerNames := make(map[string]bool, len(c.Providers))
	for i, p := range c.Providers {
		label := fmt.Sprintf("provider %q", p.Name)
		if p.Name == "" {
			label = fmt.Sprintf("providers[%d]", i)
			add("%s: name is required", label)
		} else if providerNames[p.Name] {
			add("%s: duplicate name, provider names must be unique", label)
		}
		providerNames[p.Name] = true

		switch p.Kind {
		case KindGroq, KindOpenAICompat:
		default:
			add("%s: unknown kind %q, must be %q or %q", label, p.Kind, KindGroq, KindOpenAICompat)
		}

		if p.BaseURL == "" {
			add("%s: base_url is required", label)
		} else if u, err := url.Parse(p.BaseURL); err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			add("%s: base_url %q is not a valid http(s) URL", label, p.BaseURL)
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

		if r.Provider == "" {
			add("%s: provider is required", label)
		} else if !providerNames[r.Provider] {
			add("%s: provider %q is not declared under providers", label, r.Provider)
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
