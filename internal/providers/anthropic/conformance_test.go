package anthropic_test

import (
	"net/http"
	"testing"

	"github.com/rajeev-chaurasia/pen-stock/internal/providers"
	"github.com/rajeev-chaurasia/pen-stock/internal/providers/anthropic"
	"github.com/rajeev-chaurasia/pen-stock/internal/providers/conformance"
)

// statusOverloaded is Anthropic's "overloaded_error" status. It is not a
// registered HTTP code, so no net/http constant names it.
const statusOverloaded = 529

// anthropicStream is a realistic event-typed Messages stream: every
// event carries both an event line and a data line, message_start
// reports input tokens (plus a provisional output count this adapter
// ignores), a ping keeps the connection warm, message_delta carries the
// real output count and the stop reason, and message_stop closes it.
const anthropicStream = "event: message_start\n" +
	`data: {"type":"message_start","message":{"id":"msg_01Fx9k","type":"message","role":"assistant",` +
	`"model":"claude-sonnet-4-5-20250929","content":[],"stop_reason":null,"stop_sequence":null,` +
	`"usage":{"input_tokens":9,"output_tokens":1}}}` + "\n\n" +

	"event: content_block_start\n" +
	`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +

	"event: ping\n" +
	`data: {"type": "ping"}` + "\n\n" +

	"event: content_block_delta\n" +
	`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hel"}}` + "\n\n" +

	"event: content_block_delta\n" +
	`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"lo"}}` + "\r\n\r\n" +

	"event: content_block_stop\n" +
	`data: {"type":"content_block_stop","index":0}` + "\n\n" +

	"event: message_delta\n" +
	`data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},` +
	`"usage":{"output_tokens":2}}` + "\n\n" +

	"event: message_stop\n" +
	`data: {"type":"message_stop"}` + "\n\n"

// truncatedStream stops after a text delta. Anthropic sends no [DONE]
// sentinel, so the absence of message_stop is the only evidence that
// this answer is partial.
const truncatedStream = "event: message_start\n" +
	`data: {"type":"message_start","message":{"id":"msg_01Fx9k","type":"message","role":"assistant",` +
	`"model":"claude-sonnet-4-5-20250929","content":[],"stop_reason":null,"stop_sequence":null,` +
	`"usage":{"input_tokens":9,"output_tokens":1}}}` + "\n\n" +

	"event: content_block_delta\n" +
	`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"par"}}` + "\n\n"

// TestConformance runs the shared provider contract against the
// Anthropic adapter, which reaches it by translating a wire format that
// resembles the OpenAI one nowhere.
func TestConformance(t *testing.T) {
	conformance.Run(t, conformance.Suite{
		Name:       "anthropic",
		New:        func(baseURL, apiKey string) providers.Provider { return anthropic.New("test", baseURL, apiKey, nil) },
		AuthHeader: "x-api-key",
		// Anthropic authenticates with the raw key, not a bearer token.
		AuthValue: func(key string) string { return key },

		NonStream: conformance.NonStreamCase{
			UpstreamBody: []byte(`{"id":"msg_01XFDUDYJgAACzvnptvVoYEL","type":"message","role":"assistant",` +
				`"model":"claude-sonnet-4-5-20250929",` +
				`"content":[{"type":"text","text":"Hello there"}],` +
				`"stop_reason":"end_turn","stop_sequence":null,` +
				`"usage":{"input_tokens":9,"output_tokens":3}}`),
			WantContent: "Hello there",
			WantUsage:   providers.Usage{PromptTokens: 9, CompletionTokens: 3, TotalTokens: 12},
		},

		Stream: conformance.StreamCase{
			UpstreamBody: []byte(anthropicStream),
			WantContent:  "Hello",
			WantUsage:    providers.Usage{PromptTokens: 9, CompletionTokens: 2, TotalTokens: 11},
		},

		Truncated: conformance.StreamCase{
			UpstreamBody: []byte(truncatedStream),
			WantContent:  "par",
		},

		Errors: []conformance.ErrorCase{
			{
				Name:      "unauthorized",
				Status:    http.StatusUnauthorized,
				Body:      []byte(`{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`),
				WantClass: providers.ErrClassAuth,
			},
			{
				Name:   "rate limited",
				Status: http.StatusTooManyRequests,
				Body: []byte(`{"type":"error","error":{"type":"rate_limit_error",` +
					`"message":"Number of request tokens has exceeded your per-minute rate limit"}}`),
				WantClass: providers.ErrClassRateLimited,
			},
			{
				Name:   "bad request",
				Status: http.StatusBadRequest,
				Body: []byte(`{"type":"error","error":{"type":"invalid_request_error",` +
					`"message":"max_tokens: must be greater than 0"}}`),
				WantClass: providers.ErrClassInvalidRequest,
			},
			{
				Name:   "model not found",
				Status: http.StatusNotFound,
				Body: []byte(`{"type":"error","error":{"type":"not_found_error",` +
					`"message":"model: claude-does-not-exist"}}`),
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
				Body:      []byte(`{"type":"error","error":{"type":"api_error","message":"Internal server error"}}`),
				WantClass: providers.ErrClassUpstream,
			},
			{
				// Anthropic signals a saturated fleet with 529, which is
				// not a registered status code and must still route as
				// upstream trouble rather than as an internal fault.
				Name:      "overloaded",
				Status:    statusOverloaded,
				Body:      []byte(`{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`),
				WantClass: providers.ErrClassUpstream,
			},
			{
				// Only error.message is lifted out of a failure body, so
				// material echoed back beside it stays upstream.
				Name:   "echoed request material is not relayed",
				Status: http.StatusBadRequest,
				Body: []byte(`{"type":"error","error":{"type":"invalid_request_error",` +
					`"message":"messages: at least one message is required"},` +
					`"request_echo":{"authorization":"sk-ant-planted-secret-value"}}`),
				WantClass:    providers.ErrClassInvalidRequest,
				SecretInBody: "sk-ant-planted-secret-value",
			},
		},
	})
}
