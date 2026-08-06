package config

import (
	"errors"
	"strings"
	"testing"
)

func TestClientKeysExpandFromEnv(t *testing.T) {
	// Client keys guard the provider keys, so they must be resolvable
	// from the environment rather than written into the config file.
	t.Setenv("PENSTOCK_TEST_CLIENT_KEY", strings.Repeat("a", 32))

	cfg := validConfig()
	cfg.Server.Listen = ":8080"
	cfg.Auth.ClientKeys = []string{"${PENSTOCK_TEST_CLIENT_KEY}"}

	if err := cfg.expandSecrets(); err != nil {
		t.Fatalf("expandSecrets: %v", err)
	}
	if got := cfg.Auth.ClientKeys[0]; got != strings.Repeat("a", 32) {
		t.Errorf("client key = %q, want the expanded value", got)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate after expansion: %v", err)
	}
}

func TestClientKeyMissingEnvIsAnError(t *testing.T) {
	cases := []struct {
		name  string
		value string
		set   bool
	}{
		{name: "unset variable", set: false},
		{name: "empty variable", value: "", set: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.set {
				t.Setenv("PENSTOCK_TEST_EMPTY_CLIENT_KEY", tc.value)
			}
			cfg := validConfig()
			cfg.Auth.ClientKeys = []string{"${PENSTOCK_TEST_EMPTY_CLIENT_KEY}"}

			err := cfg.expandSecrets()
			if !errors.Is(err, ErrMissingEnv) {
				t.Fatalf("expandSecrets = %v, want ErrMissingEnv", err)
			}
			if !strings.Contains(err.Error(), "PENSTOCK_TEST_EMPTY_CLIENT_KEY") {
				t.Errorf("error %q should name the variable", err)
			}
		})
	}
}

func TestLiteralClientKeysStillWork(t *testing.T) {
	cfg := validConfig()
	literal := strings.Repeat("b", 32)
	cfg.Auth.ClientKeys = []string{literal}

	if err := cfg.expandSecrets(); err != nil {
		t.Fatalf("expandSecrets: %v", err)
	}
	if cfg.Auth.ClientKeys[0] != literal {
		t.Errorf("literal key was rewritten to %q", cfg.Auth.ClientKeys[0])
	}
}
