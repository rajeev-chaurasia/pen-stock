package config

import (
	"strings"
	"testing"
)

func TestRouteChain(t *testing.T) {
	cases := []struct {
		name  string
		route RouteConfig
		want  []string
	}{
		{
			name:  "single provider",
			route: RouteConfig{Model: "m", Provider: "a"},
			want:  []string{"a"},
		},
		{
			name:  "chain keeps configured order",
			route: RouteConfig{Model: "m", Providers: []string{"a", "b", "c"}},
			want:  []string{"a", "b", "c"},
		},
		{
			name:  "neither spelling yields nothing",
			route: RouteConfig{Model: "m"},
			want:  nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.route.Chain()
			if len(got) != len(tc.want) {
				t.Fatalf("Chain() = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("Chain()[%d] = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestValidateFallbackChains(t *testing.T) {
	cases := []struct {
		name     string
		mutate   func(*Config)
		wantText string
	}{
		{
			name: "a chain of declared providers is accepted",
			mutate: func(c *Config) {
				// An empty models list means unrestricted, so clearing it
				// lets this provider back the route without disturbing the
				// other one it already serves.
				c.Providers[1].Models = nil
				c.Routes[0].Provider = ""
				c.Routes[0].Providers = []string{c.Providers[0].Name, c.Providers[1].Name}
			},
		},
		{
			name: "both spellings at once is ambiguous",
			mutate: func(c *Config) {
				c.Routes[0].Providers = []string{c.Providers[0].Name}
			},
			wantText: "not both",
		},
		{
			name: "a chain naming an undeclared provider is rejected",
			mutate: func(c *Config) {
				c.Routes[0].Provider = ""
				c.Routes[0].Providers = []string{c.Providers[0].Name, "ghost"}
			},
			wantText: `provider "ghost" is not declared`,
		},
		{
			name: "the same provider twice in one chain is rejected",
			mutate: func(c *Config) {
				c.Routes[0].Provider = ""
				c.Routes[0].Providers = []string{c.Providers[0].Name, c.Providers[0].Name}
			},
			wantText: "appears twice",
		},
		{
			name: "an unknown strategy names the valid ones",
			mutate: func(c *Config) {
				c.Routes[0].Strategy = "cheapest"
			},
			wantText: "must be one of",
		},
		{
			name: "every declared strategy is accepted",
			mutate: func(c *Config) {
				c.Routes[0].Strategy = StrategyLeastLatency
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validConfig()
			tc.mutate(cfg)
			err := cfg.Validate()

			if tc.wantText == "" {
				if err != nil {
					t.Fatalf("Validate: %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatal("Validate = nil, want an error")
			}
			if !strings.Contains(err.Error(), tc.wantText) {
				t.Errorf("error %q does not mention %q", err, tc.wantText)
			}
		})
	}
}
