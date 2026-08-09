package config

import (
	"errors"
	"strings"
	"testing"
)

// key returns a key long enough to clear MinClientKeyLength, prefixed so
// cases can tell two of them apart.
func key(prefix string) string {
	return prefix + strings.Repeat("k", MinClientKeyLength)
}

// oneTenant is the smallest tenant that validates.
func oneTenant() []TenantConfig {
	return []TenantConfig{{Name: "demo", Keys: []string{key("demo")}}}
}

func TestValidateTenants(t *testing.T) {
	tests := []struct {
		name         string
		mutate       func(*Config)
		wantContains []string
	}{
		{
			name:   "tenant with keys and limits passes",
			mutate: func(c *Config) { c.Auth.Tenants = oneTenant() },
		},
		{
			name: "zero limits mean unlimited and pass",
			mutate: func(c *Config) {
				c.Auth.Tenants = oneTenant()
				c.Auth.Tenants[0].Limits = TenantLimits{}
			},
		},
		{
			name: "underscore and hyphen names pass",
			mutate: func(c *Config) {
				c.Auth.Tenants = []TenantConfig{
					{Name: "team_one-2", Keys: []string{key("a")}},
					{Name: "TEAM2", Keys: []string{key("b")}},
				}
			},
		},
		{
			name: "missing name",
			mutate: func(c *Config) {
				c.Auth.Tenants = []TenantConfig{{Keys: []string{key("a")}}}
			},
			wantContains: []string{"auth: tenants[0]: name is required"},
		},
		{
			name: "name with whitespace",
			mutate: func(c *Config) {
				c.Auth.Tenants = []TenantConfig{{Name: "team one", Keys: []string{key("a")}}}
			},
			wantContains: []string{`auth: tenant "team one": name must use only letters, digits, underscore and hyphen`},
		},
		{
			name: "name with a separator character",
			mutate: func(c *Config) {
				c.Auth.Tenants = []TenantConfig{{Name: "acme/prod", Keys: []string{key("a")}}}
			},
			wantContains: []string{`auth: tenant "acme/prod": name must use only letters`, "metrics label"},
		},
		{
			name: "name with a dot",
			mutate: func(c *Config) {
				c.Auth.Tenants = []TenantConfig{{Name: "acme.prod", Keys: []string{key("a")}}}
			},
			wantContains: []string{`auth: tenant "acme.prod": name must use only letters`},
		},
		{
			name: "duplicate names",
			mutate: func(c *Config) {
				c.Auth.Tenants = []TenantConfig{
					{Name: "demo", Keys: []string{key("a")}},
					{Name: "demo", Keys: []string{key("b")}},
				}
			},
			wantContains: []string{`auth: tenant "demo": duplicate name, tenant names must be unique`},
		},
		{
			name: "tenant without keys",
			mutate: func(c *Config) {
				c.Auth.Tenants = []TenantConfig{{Name: "demo"}}
			},
			wantContains: []string{`auth: tenant "demo": keys is empty`},
		},
		{
			name: "short tenant key",
			mutate: func(c *Config) {
				c.Auth.Tenants = []TenantConfig{{Name: "demo", Keys: []string{key("a"), "short"}}}
			},
			wantContains: []string{`auth: tenant "demo": keys[1] is shorter than 16 characters`},
		},
		{
			name: "key shared by two tenants",
			mutate: func(c *Config) {
				shared := key("shared")
				c.Auth.Tenants = []TenantConfig{
					{Name: "first", Keys: []string{shared}},
					{Name: "second", Keys: []string{key("b"), shared}},
				}
			},
			wantContains: []string{`auth: tenant "second": keys[1] already belongs to tenant "first"`},
		},
		{
			name: "key repeated inside one tenant",
			mutate: func(c *Config) {
				dup := key("dup")
				c.Auth.Tenants = []TenantConfig{{Name: "demo", Keys: []string{dup, dup}}}
			},
			wantContains: []string{`auth: tenant "demo": keys[1] already belongs to tenant "demo"`},
		},
		{
			name: "key also listed in client_keys",
			mutate: func(c *Config) {
				shared := key("shared")
				c.Auth.ClientKeys = []string{shared}
				c.Auth.Tenants = []TenantConfig{{Name: "demo", Keys: []string{shared}}}
			},
			wantContains: []string{`auth: tenant "demo": keys[0] also appears in auth.client_keys`},
		},
		{
			// An anonymous key authenticates but names no tenant, so once
			// accounting is on there is no account to reserve against. The
			// gateway used to accept this and answer every anonymous call
			// with a 500, blaming itself for a config it had agreed to.
			name: "anonymous client keys alongside tenants",
			mutate: func(c *Config) {
				c.Auth.ClientKeys = []string{key("anon")}
				c.Auth.Tenants = oneTenant()
			},
			wantContains: []string{
				"auth: client_keys and tenants are both set",
				"move each client key under a tenant",
			},
		},
		{
			name: "client keys alone stay valid",
			mutate: func(c *Config) {
				c.Auth.ClientKeys = []string{key("anon")}
				c.Auth.Tenants = nil
			},
		},
		{
			name: "negative requests_per_minute",
			mutate: func(c *Config) {
				c.Auth.Tenants = oneTenant()
				c.Auth.Tenants[0].Limits.RequestsPerMinute = -1
			},
			wantContains: []string{`auth: tenant "demo": requests_per_minute is -1, must not be negative`},
		},
		{
			name: "negative tokens_per_minute",
			mutate: func(c *Config) {
				c.Auth.Tenants = oneTenant()
				c.Auth.Tenants[0].Limits.TokensPerMinute = -1000
			},
			wantContains: []string{`auth: tenant "demo": tokens_per_minute is -1000, must not be negative`},
		},
		{
			name: "negative daily_usd",
			mutate: func(c *Config) {
				c.Auth.Tenants = oneTenant()
				c.Auth.Tenants[0].Limits.DailyUSD = -0.5
			},
			wantContains: []string{`auth: tenant "demo": daily_usd is -0.5, must not be negative`},
		},
		{
			name: "negative monthly_usd",
			mutate: func(c *Config) {
				c.Auth.Tenants = oneTenant()
				c.Auth.Tenants[0].Limits.MonthlyUSD = -10
			},
			wantContains: []string{`auth: tenant "demo": monthly_usd is -10, must not be negative`},
		},
		{
			name: "every tenant problem is reported at once",
			mutate: func(c *Config) {
				shared := key("shared")
				c.Auth.ClientKeys = []string{shared}
				c.Auth.Tenants = []TenantConfig{
					{Name: "bad name", Keys: []string{shared, "short"}},
					{Name: "ok", Limits: TenantLimits{DailyUSD: -1}},
				}
			},
			wantContains: []string{
				`auth: tenant "bad name": name must use only letters`,
				`auth: tenant "bad name": keys[0] also appears in auth.client_keys`,
				`auth: tenant "bad name": keys[1] is shorter than 16 characters`,
				`auth: tenant "ok": keys is empty`,
				`auth: tenant "ok": daily_usd is -1, must not be negative`,
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

// A validation message ends up in logs and in operator pastes, so it may
// name the tenant and the index but never the secret itself.
func TestTenantErrorsNeverEchoKeys(t *testing.T) {
	secret := key("supersecret")
	tooWeak := "hunter2"
	cfg := validConfig()
	cfg.Auth.ClientKeys = []string{secret}
	cfg.Auth.Tenants = []TenantConfig{
		{Name: "bad name", Keys: []string{secret}},
		{Name: "bad name", Keys: []string{tooWeak}},
	}

	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate = nil, want error")
	}
	for _, leak := range []string{secret, tooWeak} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("error %q leaks a key value", err)
		}
	}
}

func TestTenantKeysExpandFromEnv(t *testing.T) {
	const varName = "PENSTOCK_TEST_TENANT_KEY"
	expanded := key("env")

	tests := []struct {
		name         string
		configured   string
		value        string
		set          bool
		want         string
		wantContains []string
	}{
		{
			name:       "literal key is left alone",
			configured: key("literal"),
			want:       key("literal"),
		},
		{
			name:       "reference expands",
			configured: "${" + varName + "}",
			value:      expanded,
			set:        true,
			want:       expanded,
		},
		{
			name:       "embedded reference expands",
			configured: "pre-${" + varName + "}-post",
			value:      "middle",
			set:        true,
			want:       "pre-middle-post",
		},
		{
			name:         "unset variable is an error",
			configured:   "${" + varName + "}",
			wantContains: []string{`auth: tenant "demo": keys[0] references ` + varName, "unset or empty"},
		},
		{
			name:         "empty variable is an error",
			configured:   "${" + varName + "}",
			value:        "",
			set:          true,
			wantContains: []string{`auth: tenant "demo": keys[0] references ` + varName},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.set {
				t.Setenv(varName, tt.value)
			}
			cfg := validConfig()
			cfg.Auth.Tenants = []TenantConfig{{Name: "demo", Keys: []string{tt.configured}}}

			err := cfg.expandSecrets()
			if len(tt.wantContains) == 0 {
				if err != nil {
					t.Fatalf("expandSecrets: %v", err)
				}
				if got := cfg.Auth.Tenants[0].Keys[0]; got != tt.want {
					t.Errorf("tenant key = %q, want %q", got, tt.want)
				}
				return
			}
			if !errors.Is(err, ErrMissingEnv) {
				t.Fatalf("expandSecrets = %v, want ErrMissingEnv", err)
			}
			for _, want := range tt.wantContains {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not contain %q", err, want)
				}
			}
		})
	}
}

func TestTenantForKey(t *testing.T) {
	first := key("first")
	second := key("second")
	spare := key("spare")
	clientKey := key("anonymous")

	cfg := validConfig()
	cfg.Auth.ClientKeys = []string{clientKey}
	cfg.Auth.Tenants = []TenantConfig{
		{Name: "one", Keys: []string{first}, Limits: TenantLimits{RequestsPerMinute: 60, DailyUSD: 1.5, FailClosed: true}},
		{Name: "two", Keys: []string{spare, second}},
	}

	tests := []struct {
		name       string
		presented  string
		wantName   string
		wantLimits TenantLimits
		wantOK     bool
	}{
		{
			name:       "first tenant",
			presented:  first,
			wantName:   "one",
			wantLimits: TenantLimits{RequestsPerMinute: 60, DailyUSD: 1.5, FailClosed: true},
			wantOK:     true,
		},
		{
			name:      "second key of a later tenant",
			presented: second,
			wantName:  "two",
			wantOK:    true,
		},
		{name: "unknown key", presented: key("nobody")},
		{name: "anonymous client key has no tenant", presented: clientKey},
		{name: "empty key", presented: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			name, limits, ok := cfg.TenantForKey(tt.presented)
			if ok != tt.wantOK {
				t.Fatalf("TenantForKey ok = %v, want %v", ok, tt.wantOK)
			}
			if name != tt.wantName {
				t.Errorf("TenantForKey name = %q, want %q", name, tt.wantName)
			}
			if limits != tt.wantLimits {
				t.Errorf("TenantForKey limits = %+v, want %+v", limits, tt.wantLimits)
			}
		})
	}
}

func TestTenantForKeyWithoutTenants(t *testing.T) {
	cfg := validConfig()
	if _, _, ok := cfg.TenantForKey(key("any")); ok {
		t.Error("TenantForKey ok = true with no tenants configured")
	}
}

// The loopback fail safe counts a tenant key as authentication, and
// counts nothing else.
func TestLoopbackFailSafeWithTenants(t *testing.T) {
	tests := []struct {
		name        string
		listen      string
		clientKeys  []string
		tenants     []TenantConfig
		wantRefusal bool
	}{
		{
			name:    "tenant keys allow a public listener",
			listen:  "0.0.0.0:8080",
			tenants: oneTenant(),
		},
		{
			name:    "tenant keys allow a wildcard listener",
			listen:  ":8080",
			tenants: oneTenant(),
		},
		{
			name:       "client keys still allow a public listener",
			listen:     "0.0.0.0:8080",
			clientKeys: []string{key("client")},
		},
		{
			name:    "tenant keys on loopback are fine",
			listen:  "127.0.0.1:8080",
			tenants: oneTenant(),
		},
		{
			name:   "no keys of either kind on loopback is fine",
			listen: "127.0.0.1:8080",
		},
		{
			name:        "no keys of either kind refuses a public listener",
			listen:      "0.0.0.0:8080",
			wantRefusal: true,
		},
		{
			name:        "a tenant declaring no keys does not count as auth",
			listen:      "0.0.0.0:8080",
			tenants:     []TenantConfig{{Name: "empty"}},
			wantRefusal: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig()
			cfg.Server.Listen = tt.listen
			cfg.Auth.ClientKeys = tt.clientKeys
			cfg.Auth.Tenants = tt.tenants

			err := cfg.Validate()
			if !tt.wantRefusal {
				if err != nil {
					t.Fatalf("Validate: %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatal("Validate = nil, want a refusal")
			}
			for _, want := range []string{"client_keys", "must stay on loopback"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not explain the refusal (%q)", err, want)
				}
			}
		})
	}
}

// A config written before tenants existed must load, validate and behave
// exactly as it did.
func TestClientKeysOnlyIsUnchanged(t *testing.T) {
	literal := key("legacy")
	cfg := validConfig()
	cfg.Server.Listen = ":8080"
	cfg.Auth.ClientKeys = []string{literal}

	if err := cfg.expandSecrets(); err != nil {
		t.Fatalf("expandSecrets: %v", err)
	}
	if got := cfg.Auth.ClientKeys[0]; got != literal {
		t.Errorf("client key = %q, want it untouched", got)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v, want nil", err)
	}
	if len(cfg.Auth.Tenants) != 0 {
		t.Errorf("Tenants = %+v, want none invented", cfg.Auth.Tenants)
	}
	if _, _, ok := cfg.TenantForKey(literal); ok {
		t.Error("TenantForKey ok = true for an anonymous client key")
	}
}

// TestLoadTenantsYAML pins the YAML shape, including the inline limits
// and the public listener a tenants-only config is allowed to bind.
func TestLoadTenantsYAML(t *testing.T) {
	demoKey := key("demo")
	t.Setenv("PENSTOCK_TEST_DEMO_KEY", demoKey)

	cfg, err := Load(fixture("tenants.yaml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, want := len(cfg.Auth.Tenants), 2; got != want {
		t.Fatalf("len(Tenants) = %d, want %d", got, want)
	}

	demo := cfg.Auth.Tenants[0]
	if got, want := demo.Keys[0], demoKey; got != want {
		t.Errorf("demo key = %q, want the expanded value", got)
	}
	want := TenantLimits{
		RequestsPerMinute: 60,
		TokensPerMinute:   100000,
		DailyUSD:          1.00,
		MonthlyUSD:        10.00,
		FailClosed:        true,
	}
	if demo.Limits != want {
		t.Errorf("demo limits = %+v, want %+v", demo.Limits, want)
	}

	// A tenant that sets no limits is unlimited rather than locked out.
	if got := cfg.Auth.Tenants[1].Limits; got != (TenantLimits{}) {
		t.Errorf("second tenant limits = %+v, want the zero value", got)
	}
	if name, _, ok := cfg.TenantForKey(demoKey); !ok || name != "demo" {
		t.Errorf("TenantForKey = %q, %v, want \"demo\", true", name, ok)
	}
}
