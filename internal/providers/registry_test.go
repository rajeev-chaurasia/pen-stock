package providers_test

import (
	"testing"

	"github.com/rajeev-chaurasia/pen-stock/internal/config"
	"github.com/rajeev-chaurasia/pen-stock/internal/providers"

	// Every adapter, exactly as a binary serving all of them must import
	// them. An adapter missing here is a kind the gateway cannot build.
	_ "github.com/rajeev-chaurasia/pen-stock/internal/providers/anthropic"
	_ "github.com/rajeev-chaurasia/pen-stock/internal/providers/gemini"
	_ "github.com/rajeev-chaurasia/pen-stock/internal/providers/openaiwire"
)

// TestEveryKindBuilds is the guard for a failure that no unit test can
// see: registration happens in init, so a kind whose adapter is never
// imported passes config validation and then fails at startup.
func TestEveryKindBuilds(t *testing.T) {
	for _, kind := range config.AllKinds {
		t.Run(string(kind), func(t *testing.T) {
			cfgs := []config.ProviderConfig{{
				Name:    "p",
				Kind:    kind,
				BaseURL: "https://example.invalid/v1",
				APIKey:  "test-key",
			}}
			built, err := providers.BuildAll(cfgs)
			if err != nil {
				t.Fatalf("BuildAll(%q): %v", kind, err)
			}
			p, ok := built["p"]
			if !ok {
				t.Fatalf("BuildAll(%q) returned no provider named p", kind)
			}
			if p.Name() != "p" {
				t.Errorf("Name() = %q, want p", p.Name())
			}
		})
	}
}

func TestUnknownKindIsRejected(t *testing.T) {
	_, err := providers.BuildAll([]config.ProviderConfig{{
		Name:    "p",
		Kind:    config.ProviderKind("does-not-exist"),
		BaseURL: "https://example.invalid/v1",
		APIKey:  "k",
	}})
	if err == nil {
		t.Fatal("BuildAll = nil error for an unknown kind")
	}
}
