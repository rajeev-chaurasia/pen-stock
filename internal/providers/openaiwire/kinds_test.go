package openaiwire_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rajeev-chaurasia/pen-stock/internal/config"
	"github.com/rajeev-chaurasia/pen-stock/internal/providers"
	"github.com/rajeev-chaurasia/pen-stock/internal/providers/conformance"
)

// wireKinds is every kind the openaiwire adapter registers.
var wireKinds = []config.ProviderKind{
	config.KindOpenAI,
	config.KindGroq,
	config.KindCerebras,
	config.KindMistral,
	config.KindOpenRouter,
	config.KindOpenAICompat,
}

// buildKind constructs a provider the way a config file would, so these
// tests cover the registration path an operator actually takes.
func buildKind(kind config.ProviderKind, baseURL, apiKey string) providers.Provider {
	built, err := providers.BuildAll([]config.ProviderConfig{{
		Name:    string(kind),
		Kind:    kind,
		BaseURL: baseURL,
		APIKey:  apiKey,
	}})
	if err != nil {
		// Only reachable if a kind lost its registration, which makes
		// every case below meaningless; say so immediately and loudly
		// rather than through a nil provider three frames later.
		panic(fmt.Sprintf("build %s provider: %v", kind, err))
	}
	return built[string(kind)]
}

// TestConformanceEveryKind runs the full contract against every
// registered kind. They share a wire format, so one fixture set is
// parameterized by kind; what genuinely differs between them (endpoint,
// headers, stream usage) rides on the profile and is asserted below.
func TestConformanceEveryKind(t *testing.T) {
	for _, kind := range wireKinds {
		conformance.Run(t, wireSuite(kind))
	}
}

func wireSuite(kind config.ProviderKind) conformance.Suite {
	return conformance.Suite{
		Name: string(kind),
		New: func(baseURL, apiKey string) providers.Provider {
			return buildKind(kind, baseURL, apiKey)
		},
		AuthHeader: "Authorization",
		AuthValue:  func(key string) string { return "Bearer " + key },

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
	}
}

const (
	wantReferer = "https://github.com/rajeev-chaurasia/pen-stock"
	wantTitle   = "Penstock"
)

// TestOpenRouterAttributionHeaders covers what OpenRouter asks
// integrators to send on every call.
func TestOpenRouterAttributionHeaders(t *testing.T) {
	seen, _ := callKind(t, config.KindOpenRouter, false, `{"model":"m"}`)

	if got := seen.Get("HTTP-Referer"); got != wantReferer {
		t.Errorf("HTTP-Referer = %q, want %q", got, wantReferer)
	}
	if got := seen.Get("X-Title"); got != wantTitle {
		t.Errorf("X-Title = %q, want %q", got, wantTitle)
	}
	// Attribution is not allowed to cost the request its credentials or
	// its content type.
	if got := seen.Get("Authorization"); got != "Bearer test-key" {
		t.Errorf("Authorization = %q, want it intact", got)
	}
	if got := seen.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
}

func TestAttributionHeadersAreOpenRouterOnly(t *testing.T) {
	for _, kind := range wireKinds {
		if kind == config.KindOpenRouter {
			continue
		}
		t.Run(string(kind), func(t *testing.T) {
			seen, _ := callKind(t, kind, false, `{"model":"m"}`)
			for _, header := range []string{"HTTP-Referer", "X-Title"} {
				if got := seen.Get(header); got != "" {
					t.Errorf("%s = %q, want it unset for kind %s", header, got, kind)
				}
			}
		})
	}
}

// TestStreamUsageOnTheWire is the end to end half of the injection
// tests: what the upstream receives, not what the helper returns.
func TestStreamUsageOnTheWire(t *testing.T) {
	const streamBody = `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`

	for _, kind := range wireKinds {
		t.Run(string(kind), func(t *testing.T) {
			_, body := callKind(t, kind, true, streamBody)

			var sent struct {
				StreamOptions *struct {
					IncludeUsage *bool `json:"include_usage"`
				} `json:"stream_options"`
			}
			if err := json.Unmarshal(body, &sent); err != nil {
				t.Fatalf("forwarded body is not JSON: %v (%s)", err, body)
			}

			switch kind {
			case config.KindMistral, config.KindOpenAICompat:
				if string(body) != streamBody {
					t.Errorf("forwarded body = %s, want it verbatim", body)
				}
			default:
				if sent.StreamOptions == nil || sent.StreamOptions.IncludeUsage == nil {
					t.Fatalf("forwarded body = %s, want stream_options.include_usage", body)
				}
				if !*sent.StreamOptions.IncludeUsage {
					t.Errorf("include_usage = false, want true")
				}
				if !strings.Contains(string(body), `"content":"hi"`) {
					t.Errorf("forwarded body = %s, want the original messages kept", body)
				}
			}
		})
	}
}

// callKind sends one request of the given shape through a kind and
// reports what the upstream saw.
func callKind(t *testing.T, kind config.ProviderKind, stream bool, raw string) (http.Header, []byte) {
	t.Helper()

	const completion = `{"id":"c1","object":"chat.completion",` +
		`"choices":[{"message":{"role":"assistant","content":"hi"}}]}`

	var (
		seenHeader http.Header
		seenBody   []byte
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenHeader = r.Header.Clone()
		seenBody, _ = io.ReadAll(r.Body)
		if stream {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, completion)
	}))
	defer upstream.Close()

	p := buildKind(kind, upstream.URL, "test-key")
	req := &providers.ChatRequest{Model: "m", Stream: stream, Raw: json.RawMessage(raw)}

	if stream {
		reader, err := p.ChatStream(context.Background(), req)
		if err != nil {
			t.Fatalf("ChatStream: %v", err)
		}
		for {
			if _, err := reader.Recv(); err != nil {
				break
			}
		}
		_ = reader.Close()
		return seenHeader, seenBody
	}

	if _, err := p.Chat(context.Background(), req); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	return seenHeader, seenBody
}
