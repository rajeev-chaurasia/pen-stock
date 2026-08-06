package config

import (
	"strings"
	"testing"
)

func TestOpenGatewayMustStayOnLoopback(t *testing.T) {
	// An unauthenticated gateway forwards anyone's request using the
	// operator's paid provider key, so binding it wider than loopback is
	// refused at load time rather than discovered on a bill.
	cases := []struct {
		name      string
		listen    string
		keys      []string
		wantError bool
	}{
		{"loopback without keys", "127.0.0.1:8080", nil, false},
		{"localhost without keys", "localhost:8080", nil, false},
		{"ipv6 loopback without keys", "[::1]:8080", nil, false},
		{"wildcard without keys", ":8080", nil, true},
		{"all interfaces without keys", "0.0.0.0:8080", nil, true},
		{"lan address without keys", "192.168.1.10:8080", nil, true},
		{"wildcard with keys", ":8080", []string{strings.Repeat("k", 32)}, false},
		{"lan address with keys", "0.0.0.0:8080", []string{strings.Repeat("k", 32)}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validConfig()
			cfg.Server.Listen = tc.listen
			cfg.Auth.ClientKeys = tc.keys

			err := cfg.Validate()
			if tc.wantError && err == nil {
				t.Fatalf("Validate = nil for listen %q without keys, want a refusal", tc.listen)
			}
			if !tc.wantError && err != nil {
				t.Fatalf("Validate = %v, want nil", err)
			}
			if tc.wantError && !strings.Contains(err.Error(), "client_keys") {
				t.Errorf("error %q should explain the missing client_keys", err)
			}
		})
	}
}

func TestShortClientKeysRejected(t *testing.T) {
	cfg := validConfig()
	cfg.Server.Listen = ":8080"
	cfg.Auth.ClientKeys = []string{"short"}

	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate = nil for a five character key, want a refusal")
	}
	if !strings.Contains(err.Error(), "too weak") {
		t.Errorf("error %q should name the weakness", err)
	}
}

func TestDefaultListenIsLoopback(t *testing.T) {
	// The safe default matters more than the convenient one: an operator
	// who never sets listen must not end up publicly exposed.
	if !isLoopbackListen(DefaultListen) {
		t.Errorf("DefaultListen = %q, want a loopback address", DefaultListen)
	}
	if !isLoopbackListen(DefaultAdminListen) {
		t.Errorf("DefaultAdminListen = %q, want a loopback address", DefaultAdminListen)
	}
}
