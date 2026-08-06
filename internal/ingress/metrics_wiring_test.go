package ingress_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/rajeev-chaurasia/pen-stock/internal/ingress"
	"github.com/rajeev-chaurasia/pen-stock/internal/providers"
)

type recordingSink struct {
	mu       sync.Mutex
	requests []string
	ttft     []string
	tokens   []string
}

func (s *recordingSink) ObserveRequest(path, provider, code string, seconds float64, stream bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, fmt.Sprintf("%s|%s|%s|%t", path, provider, code, stream))
}

func (s *recordingSink) ObserveTTFT(provider string, seconds float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ttft = append(s.ttft, provider)
}

func (s *recordingSink) AddTokens(provider string, prompt, completion int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokens = append(s.tokens, fmt.Sprintf("%s|%d|%d", provider, prompt, completion))
}

func (s *recordingSink) snapshot() (requests, ttft, tokens []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.requests...), append([]string(nil), s.ttft...), append([]string(nil), s.tokens...)
}

// usageReader replays frames and reports usage on the final one.
type usageReader struct {
	frames [][]byte
	usage  providers.Usage
	i      int
}

func (r *usageReader) Recv() (providers.StreamChunk, error) {
	if r.i >= len(r.frames) {
		return providers.StreamChunk{}, io.EOF
	}
	c := providers.StreamChunk{Data: r.frames[r.i]}
	if r.i == len(r.frames)-1 {
		c.Usage = &r.usage
	}
	r.i++
	return c, nil
}

func (r *usageReader) Close() error { return nil }

func newSinkServer(t *testing.T, routes map[string]providers.Provider, sink ingress.MetricsSink) *httptest.Server {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := ingress.NewServer(defaultCfg(), routes, log, ingress.WithMetrics(sink))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func TestMetricsNonStream(t *testing.T) {
	sink := &recordingSink{}
	prov := &fakeProvider{
		name: "sim",
		chatFn: func(_ context.Context, _ *providers.ChatRequest) (*providers.ChatResponse, error) {
			return &providers.ChatResponse{
				Body:  []byte(`{"ok":true}`),
				Usage: providers.Usage{PromptTokens: 10, CompletionTokens: 20, TotalTokens: 30},
			}, nil
		},
	}
	ts := newSinkServer(t, map[string]providers.Provider{"m": prov}, sink)

	resp := postChat(t, ts, `{"model":"m"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	requests, ttft, tokens := sink.snapshot()
	if len(requests) != 1 || requests[0] != "/v1/chat/completions|sim|200|false" {
		t.Fatalf("requests = %v", requests)
	}
	if len(ttft) != 0 {
		t.Fatalf("ttft observed on non-stream: %v", ttft)
	}
	if len(tokens) != 1 || tokens[0] != "sim|10|20" {
		t.Fatalf("tokens = %v", tokens)
	}
}

func TestMetricsStream(t *testing.T) {
	sink := &recordingSink{}
	prov := &fakeProvider{
		name: "sim",
		streamFn: func(_ context.Context, _ *providers.ChatRequest) (providers.StreamReader, error) {
			return &usageReader{
				frames: [][]byte{[]byte(`{"a":1}`), []byte(`{"b":2}`)},
				usage:  providers.Usage{PromptTokens: 5, CompletionTokens: 7},
			}, nil
		},
	}
	ts := newSinkServer(t, map[string]providers.Provider{"m": prov}, sink)

	resp := postChat(t, ts, `{"model":"m","stream":true}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if _, err := io.ReadAll(resp.Body); err != nil {
		t.Fatalf("drain stream: %v", err)
	}
	requests, ttft, tokens := sink.snapshot()
	if len(requests) != 1 || requests[0] != "/v1/chat/completions|sim|200|true" {
		t.Fatalf("requests = %v", requests)
	}
	if len(ttft) != 1 || ttft[0] != "sim" {
		t.Fatalf("ttft = %v, want one observation for sim", ttft)
	}
	if len(tokens) != 1 || tokens[0] != "sim|5|7" {
		t.Fatalf("tokens = %v", tokens)
	}
}

func TestMetricsUnknownPathNormalized(t *testing.T) {
	sink := &recordingSink{}
	ts := newSinkServer(t, map[string]providers.Provider{}, sink)

	resp, err := http.Get(ts.URL + "/definitely/not/a/route")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()
	requests, _, _ := sink.snapshot()
	if len(requests) != 1 || requests[0] != "other||404|false" {
		t.Fatalf("requests = %v, want normalized path 'other'", requests)
	}
}
