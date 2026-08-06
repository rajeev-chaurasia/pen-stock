package gemini

import (
	"testing"

	"github.com/rajeev-chaurasia/pen-stock/internal/providers"
)

// Thinking models spend tokens reasoning before they answer. Gemini
// reports those separately from the visible answer but charges them at
// the output rate, so a gateway that bills only the visible part
// understates the cost badly. A live gemini-3-flash-preview call
// returned 6 visible tokens against 76 total.
func TestUsageFoldsThinkingTokensIntoCompletion(t *testing.T) {
	cases := []struct {
		name string
		in   *usageMetadata
		want providers.Usage
	}{
		{
			name: "nil metadata reports nothing rather than guessing",
			in:   nil,
			want: providers.Usage{},
		},
		{
			name: "a model without thinking is unchanged",
			in: &usageMetadata{
				PromptTokenCount:     10,
				CandidatesTokenCount: 20,
				TotalTokenCount:      30,
			},
			want: providers.Usage{PromptTokens: 10, CompletionTokens: 20, TotalTokens: 30},
		},
		{
			name: "reasoning tokens are billed as output, so they count as completion",
			in: &usageMetadata{
				PromptTokenCount:     8,
				CandidatesTokenCount: 6,
				ThoughtsTokenCount:   62,
				TotalTokenCount:      76,
			},
			want: providers.Usage{PromptTokens: 8, CompletionTokens: 68, TotalTokens: 76},
		},
		{
			name: "a missing total is derived rather than left at zero",
			in: &usageMetadata{
				PromptTokenCount:     5,
				CandidatesTokenCount: 3,
				ThoughtsTokenCount:   4,
			},
			want: providers.Usage{PromptTokens: 5, CompletionTokens: 7, TotalTokens: 12},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.in.toUsage(); got != tc.want {
				t.Errorf("toUsage() = %+v, want %+v", got, tc.want)
			}
		})
	}
}
