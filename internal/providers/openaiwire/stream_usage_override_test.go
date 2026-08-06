package openaiwire

import (
	"testing"

	"github.com/rajeev-chaurasia/pen-stock/internal/config"
)

// A self hosted backend that does support stream_options must be able to
// say so. Without usage, a streamed request reports no tokens, which
// means a budget cannot bill it and the ledger records a completion that
// appears to have cost nothing. Found by pointing the gateway at
// llama.cpp's server, which supports the field while the openai_compat
// default assumes nothing does.
func TestStreamUsageOverride(t *testing.T) {
	yes, no := true, false
	cases := []struct {
		name     string
		kind     config.ProviderKind
		override *bool
		want     bool
	}{
		{"openai_compat defaults to off", config.KindOpenAICompat, nil, false},
		{"openai_compat can opt in", config.KindOpenAICompat, &yes, true},
		{"groq defaults to on", config.KindGroq, nil, true},
		{"a vendor default can be overridden off", config.KindGroq, &no, false},
		{"mistral stays off by default", config.KindMistral, nil, false},
		{"mistral can be forced on by an operator who knows better", config.KindMistral, &yes, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			build := builderFor(tc.kind)
			p, err := build(config.ProviderConfig{
				Name:        "p",
				Kind:        tc.kind,
				BaseURL:     "https://example.invalid/v1",
				APIKey:      "k",
				StreamUsage: tc.override,
			})
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			got := p.(*provider).profile.streamUsage
			if got != tc.want {
				t.Errorf("streamUsage = %v, want %v", got, tc.want)
			}
		})
	}
}

// The override must not leak between providers built from the same
// kind, or one tenant's opt in would silently change everyone else's.
func TestStreamUsageOverrideDoesNotMutateTheSharedProfile(t *testing.T) {
	yes := true
	build := builderFor(config.KindOpenAICompat)

	if _, err := build(config.ProviderConfig{
		Name: "opted-in", Kind: config.KindOpenAICompat,
		BaseURL: "https://example.invalid/v1", APIKey: "k", StreamUsage: &yes,
	}); err != nil {
		t.Fatalf("build: %v", err)
	}

	plain, err := build(config.ProviderConfig{
		Name: "plain", Kind: config.KindOpenAICompat,
		BaseURL: "https://example.invalid/v1", APIKey: "k",
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if plain.(*provider).profile.streamUsage {
		t.Error("one provider's override changed the default for the next")
	}
}
