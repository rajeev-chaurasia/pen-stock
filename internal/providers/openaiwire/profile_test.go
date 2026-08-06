package openaiwire

import (
	"errors"
	"testing"

	"github.com/rajeev-chaurasia/pen-stock/internal/config"
)

func TestBaseURLResolution(t *testing.T) {
	cases := []struct {
		name       string
		kind       config.ProviderKind
		configured string
		want       string
	}{
		{"openai default", config.KindOpenAI, "", openAIBaseURL},
		{"groq default", config.KindGroq, "", groqBaseURL},
		{"cerebras default", config.KindCerebras, "", cerebrasBaseURL},
		{"mistral default", config.KindMistral, "", mistralBaseURL},
		{"openrouter default", config.KindOpenRouter, "", openRouterBaseURL},
		{
			// Operators put proxies and regional endpoints in front of
			// these APIs, so a configured address is never second guessed.
			name:       "operator base_url wins over the default",
			kind:       config.KindOpenAI,
			configured: "https://openai.proxy.internal/v1",
			want:       "https://openai.proxy.internal/v1",
		},
		{
			name:       "trailing slash is trimmed",
			kind:       config.KindGroq,
			configured: "https://groq.proxy.internal/v1/",
			want:       "https://groq.proxy.internal/v1",
		},
		{
			name:       "openai_compat takes what it is given",
			kind:       config.KindOpenAICompat,
			configured: "http://127.0.0.1:8089/v1",
			want:       "http://127.0.0.1:8089/v1",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			built, err := builderFor(tc.kind)(config.ProviderConfig{
				Name:    "p",
				Kind:    tc.kind,
				BaseURL: tc.configured,
				APIKey:  "k",
			})
			if err != nil {
				t.Fatalf("build %s: %v", tc.kind, err)
			}
			if got := built.(*provider).baseURL; got != tc.want {
				t.Errorf("baseURL = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestOpenAICompatNeedsBaseURL keeps the self hosted kind honest: llmsim
// and vLLM live wherever the operator put them, so there is nothing to
// guess and silence is a misconfiguration.
func TestOpenAICompatNeedsBaseURL(t *testing.T) {
	_, err := builderFor(config.KindOpenAICompat)(config.ProviderConfig{Name: "sim", APIKey: "k"})
	if !errors.Is(err, errNoBaseURL) {
		t.Fatalf("error = %v, want it to wrap errNoBaseURL", err)
	}
}
