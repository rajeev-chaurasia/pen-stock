package ingress_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rajeev-chaurasia/pen-stock/internal/ingress"
	"github.com/rajeev-chaurasia/pen-stock/internal/providers"
)

// truncatingReader ends without the [DONE] sentinel, the way a severed
// upstream does.
type truncatingReader struct {
	frames [][]byte
	i      int
}

func (r *truncatingReader) Recv() (providers.StreamChunk, error) {
	if r.i >= len(r.frames) {
		return providers.StreamChunk{}, providers.ErrStreamTruncated
	}
	c := providers.StreamChunk{Data: r.frames[r.i]}
	r.i++
	return c, nil
}

func (r *truncatingReader) Close() error { return nil }

// keepaliveReader emits comment style keepalives before its data.
type keepaliveReader struct {
	keepalives int
	sent       int
	done       bool
}

func (r *keepaliveReader) Recv() (providers.StreamChunk, error) {
	if r.sent < r.keepalives {
		r.sent++
		time.Sleep(30 * time.Millisecond)
		return providers.StreamChunk{Keepalive: true}, nil
	}
	if !r.done {
		r.done = true
		return providers.StreamChunk{Data: []byte(`{"late":true}`)}, nil
	}
	return providers.StreamChunk{}, io.EOF
}

func (r *keepaliveReader) Close() error { return nil }

func streamProvider(name string, make func() providers.StreamReader) *fakeProvider {
	return &fakeProvider{
		name: name,
		streamFn: func(_ context.Context, _ *providers.ChatRequest) (providers.StreamReader, error) {
			return make(), nil
		},
	}
}

func TestTruncatedStreamNeverFabricatesDone(t *testing.T) {
	prov := streamProvider("fp", func() providers.StreamReader {
		return &truncatingReader{frames: [][]byte{[]byte(`{"partial":1}`)}}
	})
	ts := newTestServer(t, defaultCfg(), map[string]providers.Provider{"m1": prov})

	resp := postChat(t, ts, `{"model":"m1","stream":true}`)
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if strings.Contains(string(body), "[DONE]") {
		t.Errorf("body = %q, must not claim completion for a truncated stream", body)
	}
	if !strings.Contains(string(body), "stream_truncated") {
		t.Errorf("body = %q, want a stream_truncated error frame", body)
	}
	if !strings.Contains(string(body), `{"partial":1}`) {
		t.Errorf("body = %q, want the partial content preserved", body)
	}
}

func TestCleanStreamStillSendsDone(t *testing.T) {
	prov := streamProvider("fp", func() providers.StreamReader {
		return &scriptReader{frames: [][]byte{[]byte(`{"a":1}`)}}
	})
	ts := newTestServer(t, defaultCfg(), map[string]providers.Provider{"m1": prov})

	resp := postChat(t, ts, `{"model":"m1","stream":true}`)
	body, _ := io.ReadAll(resp.Body)
	if !strings.HasSuffix(string(body), "data: [DONE]\n\n") {
		t.Errorf("body = %q, a complete stream must end with [DONE]", body)
	}
	if strings.Contains(string(body), "stream_truncated") {
		t.Errorf("body = %q, a complete stream must not be flagged truncated", body)
	}
}

func TestKeepaliveResetsIdleBudget(t *testing.T) {
	// Keepalives arrive every 30ms and real data only after 5 of them,
	// well past the 80ms idle budget a naive implementation would trip.
	prov := streamProvider("fp", func() providers.StreamReader {
		return &keepaliveReader{keepalives: 5}
	})
	cfg := defaultCfg()
	cfg.StreamIdleTimeoutMS = 80
	ts := newTestServer(t, cfg, map[string]providers.Provider{"m1": prov})

	resp := postChat(t, ts, `{"model":"m1","stream":true}`)
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !strings.Contains(string(body), `{"late":true}`) {
		t.Errorf("body = %q, heartbeating upstream was killed before its data arrived", body)
	}
	if !strings.HasSuffix(string(body), "data: [DONE]\n\n") {
		t.Errorf("body = %q, want a clean completion", body)
	}
	if !strings.Contains(string(body), ": keep-alive") {
		t.Errorf("body = %q, want keepalives forwarded to hold the client connection open", body)
	}
}

func TestRepeatedUsageCountedOnce(t *testing.T) {
	// Backends with continuous usage stats repeat cumulative totals on
	// every chunk; summing them would multiply the count per stream.
	sink := &recordingSink{}
	prov := streamProvider("fp", func() providers.StreamReader {
		return &repeatUsageReader{n: 3, usage: providers.Usage{PromptTokens: 10, CompletionTokens: 5}}
	})
	ts := newSinkServer(t, map[string]providers.Provider{"m1": prov}, sink)

	resp := postChat(t, ts, `{"model":"m1","stream":true}`)
	_, _ = io.ReadAll(resp.Body)

	_, _, tokens := sink.snapshot()
	if len(tokens) != 1 || tokens[0] != "fp|10|5" {
		t.Errorf("tokens = %v, want a single fp|10|5 record", tokens)
	}
}

type repeatUsageReader struct {
	n     int
	i     int
	usage providers.Usage
}

func (r *repeatUsageReader) Recv() (providers.StreamChunk, error) {
	if r.i >= r.n {
		return providers.StreamChunk{}, io.EOF
	}
	r.i++
	u := r.usage
	return providers.StreamChunk{Data: []byte(`{"c":1}`), Usage: &u}, nil
}

func (r *repeatUsageReader) Close() error { return nil }

func TestStreamHeaderTimeout(t *testing.T) {
	// An upstream that accepts the connection and never sends headers
	// must not hold the request open: the idle timer cannot help here
	// because it only starts once headers arrive.
	prov := &fakeProvider{
		name: "fp",
		streamFn: func(ctx context.Context, _ *providers.ChatRequest) (providers.StreamReader, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	cfg := defaultCfg()
	cfg.UpstreamTimeoutMS = 150
	cfg.StreamIdleTimeoutMS = 5000
	ts := newTestServer(t, cfg, map[string]providers.Provider{"m1": prov})

	start := time.Now()
	resp := postChat(t, ts, `{"model":"m1","stream":true}`)
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("request took %v; no header timeout applied to streams", elapsed)
	}
	if resp.StatusCode != http.StatusGatewayTimeout {
		t.Errorf("status = %d, want 504", resp.StatusCode)
	}
}

func TestClientKeysRequired(t *testing.T) {
	prov := &fakeProvider{
		name: "fp",
		chatFn: func(_ context.Context, _ *providers.ChatRequest) (*providers.ChatResponse, error) {
			return &providers.ChatResponse{Body: []byte(`{}`)}, nil
		},
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := ingress.NewServer(defaultCfg(), map[string]providers.Provider{"m1": prov}, log,
		ingress.WithClientKeys([]string{"secret-one", "secret-two"}))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	cases := []struct {
		name   string
		header string
		want   int
	}{
		{"no header", "", http.StatusUnauthorized},
		{"wrong key", "Bearer nope", http.StatusUnauthorized},
		{"not bearer", "Basic secret-one", http.StatusUnauthorized},
		{"bare prefix", "Bearer ", http.StatusUnauthorized},
		{"valid key", "Bearer secret-one", http.StatusOK},
		{"second key", "Bearer secret-two", http.StatusOK},
		{"lowercase scheme", "bearer secret-one", http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions",
				strings.NewReader(`{"model":"m1"}`))
			if err != nil {
				t.Fatalf("new request: %v", err)
			}
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("do: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Errorf("status = %d, want %d", resp.StatusCode, tc.want)
			}
		})
	}

	// Health checks stay reachable so a load balancer needs no key.
	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("healthz: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("healthz status = %d, want 200 without a key", resp.StatusCode)
	}
}

func TestInflightLimitShedsLoad(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })

	prov := &fakeProvider{
		name: "fp",
		chatFn: func(_ context.Context, _ *providers.ChatRequest) (*providers.ChatResponse, error) {
			<-release
			return &providers.ChatResponse{Body: []byte(`{}`)}, nil
		},
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := ingress.NewServer(defaultCfg(), map[string]providers.Provider{"m1": prov}, log,
		ingress.WithInflightLimit(1))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	started := make(chan struct{})
	go func() {
		close(started)
		resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json",
			strings.NewReader(`{"model":"m1"}`))
		if err == nil {
			resp.Body.Close()
		}
	}()
	<-started
	time.Sleep(100 * time.Millisecond)

	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"m1"}`))
	if err != nil {
		t.Fatalf("second request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 when saturated", resp.StatusCode)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Error("want a Retry-After header on a shed request")
	}
	once.Do(func() { close(release) })
}

func TestUpstreamMessagesAreNotRelayedVerbatim(t *testing.T) {
	// A realistic gateway error page names internal infrastructure and
	// can echo the request's own credentials.
	leak := `502 Bad Gateway: backend=10.4.2.11:9000 auth="Bearer gsk_live_abc123def456"`
	cases := []struct {
		name   string
		class  providers.ErrorClass
		status int
	}{
		{"upstream", providers.ErrClassUpstream, http.StatusBadGateway},
		{"rate limited", providers.ErrClassRateLimited, http.StatusTooManyRequests},
		{"model not found", providers.ErrClassModelNotFound, http.StatusNotFound},
		{"auth", providers.ErrClassAuth, http.StatusBadGateway},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prov := &fakeProvider{
				name: "fp",
				chatFn: func(_ context.Context, _ *providers.ChatRequest) (*providers.ChatResponse, error) {
					return nil, &providers.ProviderError{Provider: "fp", Class: tc.class, Message: leak}
				},
			}
			ts := newTestServer(t, defaultCfg(), map[string]providers.Provider{"m1": prov})
			resp := postChat(t, ts, `{"model":"m1"}`)
			if resp.StatusCode != tc.status {
				t.Errorf("status = %d, want %d", resp.StatusCode, tc.status)
			}
			body, _ := io.ReadAll(resp.Body)
			for _, secret := range []string{"gsk_live_abc123def456", "10.4.2.11", "Bearer"} {
				if strings.Contains(string(body), secret) {
					t.Errorf("response leaked %q: %s", secret, body)
				}
			}
		})
	}
}

func TestInvalidRequestRelaysSanitizedDetail(t *testing.T) {
	// A rejected payload is the one case where upstream wording helps
	// the caller, but it still must not carry secrets.
	msg := `temperature must be <= 2 (request from Bearer sk_live_9999999999)`
	prov := &fakeProvider{
		name: "fp",
		chatFn: func(_ context.Context, _ *providers.ChatRequest) (*providers.ChatResponse, error) {
			return nil, &providers.ProviderError{
				Provider: "fp", Class: providers.ErrClassInvalidRequest, Message: msg,
			}
		},
	}
	ts := newTestServer(t, defaultCfg(), map[string]providers.Provider{"m1": prov})
	resp := postChat(t, ts, `{"model":"m1"}`)

	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "temperature must be") {
		t.Errorf("body = %s, want the actionable part of the upstream message", body)
	}
	if strings.Contains(string(body), "sk_live_9999999999") {
		t.Errorf("body = %s, leaked a key", body)
	}
}

func TestLongUpstreamMessageTruncated(t *testing.T) {
	prov := &fakeProvider{
		name: "fp",
		chatFn: func(_ context.Context, _ *providers.ChatRequest) (*providers.ChatResponse, error) {
			return nil, &providers.ProviderError{
				Provider: "fp", Class: providers.ErrClassInvalidRequest,
				Message: strings.Repeat("x", 8<<10),
			}
		},
	}
	ts := newTestServer(t, defaultCfg(), map[string]providers.Provider{"m1": prov})
	resp := postChat(t, ts, `{"model":"m1"}`)

	body, _ := io.ReadAll(resp.Body)
	if len(body) > 1024 {
		t.Errorf("response is %d bytes, want the relayed message capped", len(body))
	}
}

func TestPanicInHandlerDoesNotKillServer(t *testing.T) {
	prov := &fakeProvider{
		name: "fp",
		chatFn: func(_ context.Context, _ *providers.ChatRequest) (*providers.ChatResponse, error) {
			panic("provider exploded")
		},
	}
	ts := newTestServer(t, defaultCfg(), map[string]providers.Provider{"m1": prov})

	resp := postChat(t, ts, `{"model":"m1"}`)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 from the recovery middleware", resp.StatusCode)
	}

	// The server must still be serving afterwards.
	health, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("healthz after panic: %v", err)
	}
	defer health.Body.Close()
	if health.StatusCode != http.StatusOK {
		t.Errorf("healthz = %d after a panic, want 200", health.StatusCode)
	}
}

func TestNoSniffHeaderSet(t *testing.T) {
	prov := &fakeProvider{
		name: "fp",
		chatFn: func(_ context.Context, _ *providers.ChatRequest) (*providers.ChatResponse, error) {
			return &providers.ChatResponse{Body: []byte(`{"ok":true}`)}, nil
		},
	}
	ts := newTestServer(t, defaultCfg(), map[string]providers.Provider{"m1": prov})
	resp := postChat(t, ts, `{"model":"m1"}`)
	if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
}

func TestBodyCapIsOneMegabyte(t *testing.T) {
	ts := newTestServer(t, defaultCfg(), map[string]providers.Provider{"m1": &fakeProvider{name: "fp"}})
	big := `{"model":"m1","padding":"` + strings.Repeat("a", 2<<20) + `"}`
	resp := postChat(t, ts, big)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413 for a 2MB body", resp.StatusCode)
	}
	var envelope map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		t.Fatalf("decode: %v", err)
	}
}
