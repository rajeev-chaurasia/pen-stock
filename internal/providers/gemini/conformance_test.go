package gemini_test

import (
	"net/http"
	"testing"

	"github.com/rajeev-chaurasia/pen-stock/internal/providers"
	"github.com/rajeev-chaurasia/pen-stock/internal/providers/conformance"
	"github.com/rajeev-chaurasia/pen-stock/internal/providers/gemini"
)

// TestConformance runs the shared provider contract against the Gemini
// adapter. The fixtures are shaped the way the real generateContent and
// streamGenerateContent endpoints reply, down to the fields this adapter
// ignores, so the suite proves the translation rather than a convenient
// simplification of it.
func TestConformance(t *testing.T) {
	conformance.Run(t, conformance.Suite{
		Name:       "gemini",
		New:        func(baseURL, apiKey string) providers.Provider { return gemini.New("test", baseURL, apiKey, nil) },
		AuthHeader: "x-goog-api-key",
		AuthValue:  func(key string) string { return key },

		NonStream: conformance.NonStreamCase{
			// Gemini splits one answer across parts freely, so the
			// fixture does too.
			UpstreamBody: []byte(`{"candidates":[{"content":{"parts":[{"text":"Hello "},{"text":"there"}],"role":"model"},` +
				`"finishReason":"STOP","index":0,` +
				`"safetyRatings":[{"category":"HARM_CATEGORY_HATE_SPEECH","probability":"NEGLIGIBLE"}]}],` +
				`"usageMetadata":{"promptTokenCount":9,"candidatesTokenCount":3,"totalTokenCount":12},` +
				`"modelVersion":"gemini-2.0-flash"}`),
			WantContent: "Hello there",
			WantUsage:   providers.Usage{PromptTokens: 9, CompletionTokens: 3, TotalTokens: 12},
		},

		Stream: conformance.StreamCase{
			// The last event carries the finishReason, which is the only
			// completeness signal Gemini offers, plus the final usage.
			UpstreamBody: []byte(": keep-alive\n\n" +
				`data: {"candidates":[{"content":{"parts":[{"text":"Hel"}],"role":"model"},"index":0}],` +
				`"usageMetadata":{"promptTokenCount":9,"candidatesTokenCount":1,"totalTokenCount":10},` +
				`"modelVersion":"gemini-2.0-flash"}` + "\n\n" +
				`data: {"candidates":[{"content":{"parts":[{"text":"lo"}],"role":"model"},"finishReason":"STOP","index":0}],` +
				`"usageMetadata":{"promptTokenCount":9,"candidatesTokenCount":2,"totalTokenCount":11},` +
				`"modelVersion":"gemini-2.0-flash"}` + "\n\n"),
			WantContent: "Hello",
			WantUsage:   providers.Usage{PromptTokens: 9, CompletionTokens: 2, TotalTokens: 11},
		},

		Truncated: conformance.StreamCase{
			// No finishReason anywhere: the body just stops, the way a
			// crashed upstream behind a proxy leaves it. Gemini has no
			// [DONE], so this is indistinguishable from a finished turn
			// on bytes alone.
			UpstreamBody: []byte(
				`data: {"candidates":[{"content":{"parts":[{"text":"par"}],"role":"model"},"index":0}],` +
					`"modelVersion":"gemini-2.0-flash"}` + "\n\n"),
			WantContent: "par",
		},

		Errors: []conformance.ErrorCase{
			{
				Name:      "unauthenticated",
				Status:    http.StatusUnauthorized,
				Body:      []byte(`{"error":{"code":401,"message":"Request had invalid authentication credentials.","status":"UNAUTHENTICATED"}}`),
				WantClass: providers.ErrClassAuth,
			},
			{
				Name:      "permission denied",
				Status:    http.StatusForbidden,
				Body:      []byte(`{"error":{"code":403,"message":"Generative Language API has not been used in this project before.","status":"PERMISSION_DENIED"}}`),
				WantClass: providers.ErrClassAuth,
			},
			{
				Name:      "resource exhausted",
				Status:    http.StatusTooManyRequests,
				Body:      []byte(`{"error":{"code":429,"message":"Resource has been exhausted (e.g. check quota).","status":"RESOURCE_EXHAUSTED"}}`),
				WantClass: providers.ErrClassRateLimited,
			},
			{
				Name: "invalid argument keeps error details out of the message",
				// details carries upstream internals; only error.message
				// may reach a caller.
				Status: http.StatusBadRequest,
				Body: []byte(`{"error":{"code":400,"message":"API key not valid. Please pass a valid API key.","status":"INVALID_ARGUMENT",` +
					`"details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"API_KEY_INVALID","domain":"googleapis.com",` +
					`"metadata":{"service":"generativelanguage.googleapis.com","internal_trace":"INTERNAL-TRACE-DO-NOT-LEAK"}}]}}`),
				WantClass:    providers.ErrClassInvalidRequest,
				SecretInBody: "INTERNAL-TRACE-DO-NOT-LEAK",
			},
			{
				Name:      "model not found",
				Status:    http.StatusNotFound,
				Body:      []byte(`{"error":{"code":404,"message":"models/gemini-9-ultra is not found for API version v1beta.","status":"NOT_FOUND"}}`),
				WantClass: providers.ErrClassModelNotFound,
			},
			{
				Name:      "bare 404 from a mistyped base_url",
				Status:    http.StatusNotFound,
				Body:      []byte("404 page not found"),
				WantClass: providers.ErrClassUpstream,
			},
			{
				Name:      "internal",
				Status:    http.StatusInternalServerError,
				Body:      []byte(`{"error":{"code":500,"message":"An internal error has occurred.","status":"INTERNAL"}}`),
				WantClass: providers.ErrClassUpstream,
			},
			{
				Name:      "overloaded",
				Status:    http.StatusServiceUnavailable,
				Body:      []byte(`{"error":{"code":503,"message":"The model is overloaded. Please try again later.","status":"UNAVAILABLE"}}`),
				WantClass: providers.ErrClassUpstream,
			},
		},
	})
}
