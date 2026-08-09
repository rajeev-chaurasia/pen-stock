package config

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func fixture(name string) string {
	return filepath.Join("testdata", name)
}

func TestLoadValid(t *testing.T) {
	t.Setenv("PENSTOCK_TEST_GROQ_KEY", "gsk-test-123")

	cfg, err := Load(fixture("valid.yaml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if got, want := cfg.Server.Listen, "127.0.0.1:9090"; got != want {
		t.Errorf("Listen = %q, want %q", got, want)
	}
	if got, want := cfg.Server.ReadTimeoutMS, 1000; got != want {
		t.Errorf("ReadTimeoutMS = %d, want %d", got, want)
	}
	if got, want := cfg.Server.UpstreamTimeoutMS, 2000; got != want {
		t.Errorf("UpstreamTimeoutMS = %d, want %d", got, want)
	}
	if got, want := cfg.Server.StreamIdleTimeoutMS, 3000; got != want {
		t.Errorf("StreamIdleTimeoutMS = %d, want %d", got, want)
	}
	if got, want := len(cfg.Providers), 2; got != want {
		t.Fatalf("len(Providers) = %d, want %d", got, want)
	}
	if got, want := cfg.Providers[0].APIKey, "gsk-test-123"; got != want {
		t.Errorf("groq APIKey = %q, want expanded %q", got, want)
	}
	if got, want := cfg.Providers[1].APIKey, "sim"; got != want {
		t.Errorf("llmsim APIKey = %q, want literal %q", got, want)
	}
	if got, want := cfg.Providers[0].Kind, KindGroq; got != want {
		t.Errorf("groq Kind = %q, want %q", got, want)
	}
	if got, want := len(cfg.Routes), 2; got != want {
		t.Fatalf("len(Routes) = %d, want %d", got, want)
	}
	if got, want := cfg.Routes[0].Provider, "groq"; got != want {
		t.Errorf("Routes[0].Provider = %q, want %q", got, want)
	}
	if got, want := cfg.Telemetry.ServiceName, "penstock-test"; got != want {
		t.Errorf("ServiceName = %q, want %q", got, want)
	}
	if got, want := cfg.Telemetry.LogLevel, "debug"; got != want {
		t.Errorf("LogLevel = %q, want %q", got, want)
	}
}

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load(fixture("minimal.yaml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if got := cfg.Server.Listen; got != DefaultListen {
		t.Errorf("Listen = %q, want default %q", got, DefaultListen)
	}
	if got := cfg.Server.ReadTimeoutMS; got != DefaultReadTimeoutMS {
		t.Errorf("ReadTimeoutMS = %d, want default %d", got, DefaultReadTimeoutMS)
	}
	if got := cfg.Server.UpstreamTimeoutMS; got != DefaultUpstreamTimeoutMS {
		t.Errorf("UpstreamTimeoutMS = %d, want default %d", got, DefaultUpstreamTimeoutMS)
	}
	if got := cfg.Server.StreamIdleTimeoutMS; got != DefaultStreamIdleTimeoutMS {
		t.Errorf("StreamIdleTimeoutMS = %d, want default %d", got, DefaultStreamIdleTimeoutMS)
	}
	if got := cfg.Telemetry.ServiceName; got != DefaultServiceName {
		t.Errorf("ServiceName = %q, want default %q", got, DefaultServiceName)
	}
	if got := cfg.Telemetry.LogLevel; got != DefaultLogLevel {
		t.Errorf("LogLevel = %q, want default %q", got, DefaultLogLevel)
	}
}

func TestLoadErrors(t *testing.T) {
	tests := []struct {
		name         string
		path         string
		wantContains []string
		wantIs       error
	}{
		{
			name:         "missing file",
			path:         fixture("does_not_exist.yaml"),
			wantContains: []string{"open config"},
		},
		{
			name:         "missing env var",
			path:         fixture("missing_env.yaml"),
			wantContains: []string{"PENSTOCK_TEST_UNSET_VAR", `provider "groq"`},
			wantIs:       ErrMissingEnv,
		},
		{
			name:         "unknown yaml field",
			path:         fixture("unknown_field.yaml"),
			wantContains: []string{"max_connections", "not found"},
		},
		{
			name: "validation failures are collected",
			path: fixture("invalid.yaml"),
			wantContains: []string{
				`provider "dup": duplicate name`,
				`route "some-model": provider "nowhere" is not declared`,
				`log_level "loud"`,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := Load(tt.path)
			if err == nil {
				t.Fatalf("Load = %+v, want error", cfg)
			}
			if tt.wantIs != nil && !errors.Is(err, tt.wantIs) {
				t.Errorf("errors.Is(err, %v) = false, err = %v", tt.wantIs, err)
			}
			for _, want := range tt.wantContains {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not contain %q", err, want)
				}
			}
		})
	}
}

// validConfig returns a config that passes Validate; cases mutate it.
func validConfig() *Config {
	return &Config{
		Server: ServerConfig{
			Listen:              DefaultListen,
			ReadTimeoutMS:       DefaultReadTimeoutMS,
			UpstreamTimeoutMS:   DefaultUpstreamTimeoutMS,
			StreamIdleTimeoutMS: DefaultStreamIdleTimeoutMS,
		},
		Providers: []ProviderConfig{
			{Name: "groq", Kind: KindGroq, BaseURL: "https://api.groq.com/openai/v1", APIKey: "k"},
			{Name: "llmsim", Kind: KindOpenAICompat, BaseURL: "http://127.0.0.1:8089/v1", APIKey: "sim"},
		},
		Routes: []RouteConfig{
			{Model: "llama-3.3-70b-versatile", Provider: "groq"},
			{Model: "llmsim-small", Provider: "llmsim"},
		},
		Telemetry: TelemetryConfig{ServiceName: DefaultServiceName, LogLevel: DefaultLogLevel},
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name         string
		mutate       func(*Config)
		wantContains []string
	}{
		{
			name:   "valid config passes",
			mutate: func(*Config) {},
		},
		{
			name:         "no providers",
			mutate:       func(c *Config) { c.Providers = nil },
			wantContains: []string{"at least one provider"},
		},
		{
			name:         "empty provider name",
			mutate:       func(c *Config) { c.Providers[1].Name = "" },
			wantContains: []string{"providers[1]: name is required"},
		},
		{
			name:         "duplicate provider names",
			mutate:       func(c *Config) { c.Providers[1].Name = "groq" },
			wantContains: []string{`provider "groq": duplicate name`},
		},
		{
			name:         "unknown provider kind",
			mutate:       func(c *Config) { c.Providers[0].Kind = "bedrock" },
			wantContains: []string{`provider "groq": unknown kind "bedrock"`},
		},
		{
			name:         "empty base_url",
			mutate:       func(c *Config) { c.Providers[0].BaseURL = "" },
			wantContains: []string{`provider "groq": base_url is required`},
		},
		{
			name:         "unparseable base_url",
			mutate:       func(c *Config) { c.Providers[0].BaseURL = "://bad" },
			wantContains: []string{`provider "groq": base_url "://bad" is not a valid http(s) URL`},
		},
		{
			name:         "non-http scheme",
			mutate:       func(c *Config) { c.Providers[0].BaseURL = "ftp://api.groq.com" },
			wantContains: []string{`provider "groq": base_url "ftp://api.groq.com" is not a valid http(s) URL`},
		},
		{
			name:         "base_url without host",
			mutate:       func(c *Config) { c.Providers[0].BaseURL = "http://" },
			wantContains: []string{`provider "groq": base_url "http://" is not a valid http(s) URL`},
		},
		{
			name:         "no routes",
			mutate:       func(c *Config) { c.Routes = nil },
			wantContains: []string{"at least one route"},
		},
		{
			name:         "empty route model",
			mutate:       func(c *Config) { c.Routes[0].Model = "" },
			wantContains: []string{"routes[0]: model is required"},
		},
		{
			name:         "duplicate route models",
			mutate:       func(c *Config) { c.Routes[1].Model = c.Routes[0].Model },
			wantContains: []string{`route "llama-3.3-70b-versatile": duplicate model`},
		},
		{
			name:         "empty route provider",
			mutate:       func(c *Config) { c.Routes[0].Provider = "" },
			wantContains: []string{`route "llama-3.3-70b-versatile": provider or providers is required`},
		},
		{
			name:         "route references undeclared provider",
			mutate:       func(c *Config) { c.Routes[0].Provider = "ghost" },
			wantContains: []string{`route "llama-3.3-70b-versatile": provider "ghost" is not declared`},
		},
		{
			name:         "negative read timeout",
			mutate:       func(c *Config) { c.Server.ReadTimeoutMS = -1 },
			wantContains: []string{"server: read_timeout_ms is -1"},
		},
		{
			name:         "negative upstream timeout",
			mutate:       func(c *Config) { c.Server.UpstreamTimeoutMS = -50 },
			wantContains: []string{"server: upstream_timeout_ms is -50"},
		},
		{
			name:         "negative stream idle timeout",
			mutate:       func(c *Config) { c.Server.StreamIdleTimeoutMS = -1 },
			wantContains: []string{"server: stream_idle_timeout_ms is -1"},
		},
		{
			name:         "read timeout over max",
			mutate:       func(c *Config) { c.Server.ReadTimeoutMS = MaxTimeoutMS + 1 },
			wantContains: []string{"server: read_timeout_ms is 3600001, must be at most 3600000"},
		},
		{
			name:         "upstream timeout over max",
			mutate:       func(c *Config) { c.Server.UpstreamTimeoutMS = MaxTimeoutMS + 1 },
			wantContains: []string{"server: upstream_timeout_ms is 3600001, must be at most 3600000"},
		},
		{
			name:         "stream idle timeout over max",
			mutate:       func(c *Config) { c.Server.StreamIdleTimeoutMS = MaxTimeoutMS + 1 },
			wantContains: []string{"server: stream_idle_timeout_ms is 3600001, must be at most 3600000"},
		},
		{
			name:         "empty api_key",
			mutate:       func(c *Config) { c.Providers[0].APIKey = "" },
			wantContains: []string{`provider "groq": api_key is required`},
		},
		{
			name: "route model outside the provider models list",
			mutate: func(c *Config) {
				c.Providers[0].Models = []string{"other-model"}
			},
			wantContains: []string{`route "llama-3.3-70b-versatile": provider "groq" does not list model "llama-3.3-70b-versatile"`},
		},
		{
			name: "route model inside the provider models list passes",
			mutate: func(c *Config) {
				c.Providers[0].Models = []string{"llama-3.3-70b-versatile", "other-model"}
			},
		},
		{
			name:         "invalid log level",
			mutate:       func(c *Config) { c.Telemetry.LogLevel = "verbose" },
			wantContains: []string{`log_level "verbose" must be one of debug, info, warn, error`},
		},
		{
			name: "all problems reported together",
			mutate: func(c *Config) {
				c.Providers[0].BaseURL = ""
				c.Routes[1].Provider = "ghost"
				c.Telemetry.LogLevel = "verbose"
			},
			wantContains: []string{
				`provider "groq": base_url is required`,
				`route "llmsim-small": provider "ghost" is not declared`,
				`log_level "verbose"`,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig()
			tt.mutate(cfg)
			err := cfg.Validate()

			if len(tt.wantContains) == 0 {
				if err != nil {
					t.Fatalf("Validate: %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatal("Validate = nil, want error")
			}
			for _, want := range tt.wantContains {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not contain %q", err, want)
				}
			}
		})
	}
}

// TestLoadExampleConfig keeps the shipped example in sync with the loader.
func TestLoadExampleConfig(t *testing.T) {
	t.Setenv("GROQ_API_KEY", "gsk-example")

	cfg, err := Load(filepath.Join("..", "..", "config.example.yaml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, want := cfg.Providers[0].APIKey, "gsk-example"; got != want {
		t.Errorf("groq APIKey = %q, want %q", got, want)
	}

	// The example documents the router defaults by writing them out, so
	// a field renamed in the struct and not in the file would silently
	// stop parsing and leave the example advertising a knob that does
	// nothing.
	if cfg.Router.MaxAttempts == 0 || cfg.Router.BreakerThreshold == 0 || cfg.Router.BreakerCooldownSeconds == 0 {
		t.Errorf("router block did not parse from the example: %+v", cfg.Router)
	}

	// The cache block documents its defaults by writing them out, so the
	// values have to arrive even though the tier itself ships off.
	if cfg.Cache.Enabled {
		t.Error("the example enables caching; it should ship off")
	}
	if cfg.Cache.MaxEntries == 0 || cfg.Cache.TTLSeconds == 0 {
		t.Errorf("cache block did not parse from the example: %+v", cfg.Cache)
	}
	// The semantic tier stays commented out on the measured evidence, so
	// an example that quietly enables it is a bug in the example.
	if cfg.Cache.Semantic.Enabled {
		t.Error("the example enables the semantic cache tier; it should stay commented out")
	}
}

func TestExpandAPIKeyEmbedded(t *testing.T) {
	t.Setenv("PENSTOCK_TEST_PART", "middle")

	cfg := validConfig()
	cfg.Providers[0].APIKey = "pre-${PENSTOCK_TEST_PART}-post"
	if err := cfg.expandAPIKeys(); err != nil {
		t.Fatalf("expandAPIKeys: %v", err)
	}
	if got, want := cfg.Providers[0].APIKey, "pre-middle-post"; got != want {
		t.Errorf("APIKey = %q, want %q", got, want)
	}
}

// A set-but-empty variable must fail the same way as an unset one, or a
// ${VAR:-} compose default boots the gateway with an empty key.
func TestExpandAPIKeyEmptyEnv(t *testing.T) {
	t.Setenv("PENSTOCK_TEST_EMPTY_VAR", "")

	cfg := validConfig()
	cfg.Providers[0].APIKey = "${PENSTOCK_TEST_EMPTY_VAR}"
	err := cfg.expandAPIKeys()
	if !errors.Is(err, ErrMissingEnv) {
		t.Fatalf("expandAPIKeys = %v, want ErrMissingEnv", err)
	}
	if !strings.Contains(err.Error(), "PENSTOCK_TEST_EMPTY_VAR") {
		t.Errorf("error %q does not name the variable", err)
	}
}
