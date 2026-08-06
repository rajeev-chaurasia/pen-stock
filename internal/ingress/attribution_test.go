package ingress_test

import (
	"context"
	"io"
	"net/http"
	"testing"

	"github.com/rajeev-chaurasia/pen-stock/internal/providers"
)

// answeringReader stands in for a routed stream reader that knows which
// upstream in the chain actually answered.
type answeringReader struct {
	upstream string
	frames   [][]byte
	i        int
	usage    providers.Usage
}

func (r *answeringReader) Recv() (providers.StreamChunk, error) {
	if r.i >= len(r.frames) {
		return providers.StreamChunk{}, io.EOF
	}
	c := providers.StreamChunk{Data: r.frames[r.i]}
	if r.i == len(r.frames)-1 {
		u := r.usage
		c.Usage = &u
	}
	r.i++
	return c, nil
}

func (r *answeringReader) Close() error              { return nil }
func (r *answeringReader) AnsweringProvider() string { return r.upstream }

// A routed model is served by a chain, so the label on cost and latency
// has to be the provider that answered. Attributing spend to the route
// name would merge every provider's usage into one bucket and make the
// per provider cost numbers meaningless.
func TestCostIsAttributedToTheAnsweringProvider(t *testing.T) {
	t.Run("non stream", func(t *testing.T) {
		sink := &recordingSink{}
		routed := &fakeProvider{
			name: "routed-model",
			chatFn: func(_ context.Context, _ *providers.ChatRequest) (*providers.ChatResponse, error) {
				return &providers.ChatResponse{
					Body:     []byte(`{"ok":true}`),
					Provider: "groq",
					Usage:    providers.Usage{PromptTokens: 11, CompletionTokens: 22},
				}, nil
			},
		}
		ts := newSinkServer(t, map[string]providers.Provider{"m": routed}, sink)

		resp := postChat(t, ts, `{"model":"m"}`)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		requests, _, tokens := sink.snapshot()
		if len(tokens) != 1 || tokens[0] != "groq|11|22" {
			t.Errorf("tokens = %v, want them billed to groq", tokens)
		}
		if len(requests) != 1 || requests[0] != "/v1/chat/completions|groq|200|false" {
			t.Errorf("requests = %v, want the request labelled groq", requests)
		}
	})

	t.Run("stream", func(t *testing.T) {
		sink := &recordingSink{}
		routed := &fakeProvider{
			name: "routed-model",
			streamFn: func(_ context.Context, _ *providers.ChatRequest) (providers.StreamReader, error) {
				return &answeringReader{
					upstream: "mistral",
					frames:   [][]byte{[]byte(`{"a":1}`), []byte(`{"b":2}`)},
					usage:    providers.Usage{PromptTokens: 5, CompletionTokens: 9},
				}, nil
			},
		}
		ts := newSinkServer(t, map[string]providers.Provider{"m": routed}, sink)

		resp := postChat(t, ts, `{"model":"m","stream":true}`)
		if _, err := io.ReadAll(resp.Body); err != nil {
			t.Fatalf("drain: %v", err)
		}
		requests, ttft, tokens := sink.snapshot()
		if len(tokens) != 1 || tokens[0] != "mistral|5|9" {
			t.Errorf("tokens = %v, want them billed to mistral", tokens)
		}
		if len(ttft) != 1 || ttft[0] != "mistral" {
			t.Errorf("ttft = %v, want it recorded against mistral", ttft)
		}
		if len(requests) != 1 || requests[0] != "/v1/chat/completions|mistral|200|true" {
			t.Errorf("requests = %v, want the request labelled mistral", requests)
		}
	})

	t.Run("falls back to the route label when nothing more specific is known", func(t *testing.T) {
		sink := &recordingSink{}
		plain := &fakeProvider{
			name: "solo",
			chatFn: func(_ context.Context, _ *providers.ChatRequest) (*providers.ChatResponse, error) {
				return &providers.ChatResponse{Body: []byte(`{}`)}, nil
			},
		}
		ts := newSinkServer(t, map[string]providers.Provider{"m": plain}, sink)

		postChat(t, ts, `{"model":"m"}`)
		requests, _, _ := sink.snapshot()
		if len(requests) != 1 || requests[0] != "/v1/chat/completions|solo|200|false" {
			t.Errorf("requests = %v, want the provider's own name", requests)
		}
	})
}
