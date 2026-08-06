package openaiwire_test

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/rajeev-chaurasia/pen-stock/internal/providers"
	"github.com/rajeev-chaurasia/pen-stock/internal/providers/conformance"
	"github.com/rajeev-chaurasia/pen-stock/internal/providers/openaiwire"
)

// TestConformance runs the shared provider contract against the
// OpenAI-wire adapter, which is the reference implementation every other
// adapter is measured against.
func TestConformance(t *testing.T) {
	conformance.Run(t, conformance.Suite{
		Name:       "openaiwire",
		New:        func(baseURL, apiKey string) providers.Provider { return openaiwire.New("test", baseURL, apiKey, nil) },
		AuthHeader: "Authorization",
		AuthValue:  func(key string) string { return fmt.Sprintf("Bearer %s", key) },

		NonStream: conformance.NonStreamCase{
			UpstreamBody: []byte(`{"id":"c1","object":"chat.completion","model":"test-model",` +
				`"choices":[{"index":0,"message":{"role":"assistant","content":"Hello there"},"finish_reason":"stop"}],` +
				`"usage":{"prompt_tokens":9,"completion_tokens":3,"total_tokens":12}}`),
			WantContent: "Hello there",
			WantUsage:   providers.Usage{PromptTokens: 9, CompletionTokens: 3, TotalTokens: 12},
		},

		Stream: conformance.StreamCase{
			UpstreamBody: []byte(": keep-alive\n\n" +
				`data: {"object":"chat.completion.chunk","choices":[{"delta":{"role":"assistant","content":"Hel"}}]}` + "\n\n" +
				`data: {"object":"chat.completion.chunk","choices":[{"delta":{"content":"lo"}}]}` + "\n\n" +
				`data: {"object":"chat.completion.chunk","choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":9,"completion_tokens":2,"total_tokens":11}}` + "\n\n" +
				"data: [DONE]\n\n"),
			WantContent: "Hello",
			WantUsage:   providers.Usage{PromptTokens: 9, CompletionTokens: 2, TotalTokens: 11},
		},

		Truncated: conformance.StreamCase{
			// No [DONE]: the body simply stops, the way a crashed
			// upstream behind a proxy leaves it.
			UpstreamBody: []byte(
				`data: {"object":"chat.completion.chunk","choices":[{"delta":{"content":"par"}}]}` + "\n\n"),
			WantContent: "par",
		},

		Errors: []conformance.ErrorCase{
			{
				Name:      "unauthorized",
				Status:    http.StatusUnauthorized,
				Body:      []byte(`{"error":{"message":"Invalid API Key","type":"invalid_request_error"}}`),
				WantClass: providers.ErrClassAuth,
			},
			{
				Name:      "rate limited",
				Status:    http.StatusTooManyRequests,
				Body:      []byte(`{"error":{"message":"Rate limit reached","type":"rate_limit_error"}}`),
				WantClass: providers.ErrClassRateLimited,
			},
			{
				Name:      "bad request",
				Status:    http.StatusBadRequest,
				Body:      []byte(`{"error":{"message":"temperature must be <= 2","type":"invalid_request_error"}}`),
				WantClass: providers.ErrClassInvalidRequest,
			},
			{
				Name:      "model not found",
				Status:    http.StatusNotFound,
				Body:      []byte(`{"error":{"message":"model does not exist","type":"invalid_request_error"}}`),
				WantClass: providers.ErrClassModelNotFound,
			},
			{
				Name:      "bare 404 from a mistyped base_url",
				Status:    http.StatusNotFound,
				Body:      []byte("404 page not found"),
				WantClass: providers.ErrClassUpstream,
			},
			{
				Name:      "server error",
				Status:    http.StatusInternalServerError,
				Body:      []byte(`{"error":{"message":"internal"}}`),
				WantClass: providers.ErrClassUpstream,
			},
			{
				Name:      "overloaded",
				Status:    http.StatusServiceUnavailable,
				Body:      []byte(`{"error":{"message":"overloaded"}}`),
				WantClass: providers.ErrClassUpstream,
			},
		},
	})
}
