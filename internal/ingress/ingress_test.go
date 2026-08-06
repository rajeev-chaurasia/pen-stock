package ingress_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rajeev-chaurasia/pen-stock/internal/config"
	"github.com/rajeev-chaurasia/pen-stock/internal/ingress"
	"github.com/rajeev-chaurasia/pen-stock/internal/providers"
)

// fakeProvider scripts Chat and ChatStream per test.
type fakeProvider struct {
	name     string
	chatFn   func(ctx context.Context, req *providers.ChatRequest) (*providers.ChatResponse, error)
	streamFn func(ctx context.Context, req *providers.ChatRequest) (providers.StreamReader, error)
}

func (f *fakeProvider) Name() string { return f.name }

func (f *fakeProvider) Chat(ctx context.Context, req *providers.ChatRequest) (*providers.ChatResponse, error) {
	if f.chatFn == nil {
		return nil, errors.New("fake: chat not scripted")
	}
	return f.chatFn(ctx, req)
}

func (f *fakeProvider) ChatStream(ctx context.Context, req *providers.ChatRequest) (providers.StreamReader, error) {
	if f.streamFn == nil {
		return nil, errors.New("fake: stream not scripted")
	}
	return f.streamFn(ctx, req)
}

// scriptReader replays fixed frames then reports EOF.
type scriptReader struct {
	frames [][]byte
	i      int
}

func (r *scriptReader) Recv() (providers.StreamChunk, error) {
	if r.i >= len(r.frames) {
		return providers.StreamChunk{}, io.EOF
	}
	c := providers.StreamChunk{Data: r.frames[r.i]}
	r.i++
	return c, nil
}

func (r *scriptReader) Close() error { return nil }

// blockingReader emits one frame then blocks until closed or ctx ends.
type blockingReader struct {
	ctx       context.Context
	first     []byte
	sent      bool
	closed    chan struct{}
	closeOnce sync.Once
}

func newBlockingReader(ctx context.Context, first []byte) *blockingReader {
	return &blockingReader{ctx: ctx, first: first, closed: make(chan struct{})}
}

func (r *blockingReader) Recv() (providers.StreamChunk, error) {
	if !r.sent {
		r.sent = true
		return providers.StreamChunk{Data: r.first}, nil
	}
	select {
	case <-r.ctx.Done():
		return providers.StreamChunk{}, r.ctx.Err()
	case <-r.closed:
		return providers.StreamChunk{}, io.EOF
	}
}

func (r *blockingReader) Close() error {
	r.closeOnce.Do(func() { close(r.closed) })
	return nil
}

func defaultCfg() config.ServerConfig {
	return config.ServerConfig{UpstreamTimeoutMS: 5000, StreamIdleTimeoutMS: 5000}
}

func newTestServer(t *testing.T, cfg config.ServerConfig, routes map[string]providers.Provider) *httptest.Server {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ts := httptest.NewServer(ingress.NewServer(cfg, routes, log).Handler())
	t.Cleanup(ts.Close)
	return ts
}

func postChat(t *testing.T, ts *httptest.Server, body string) *http.Response {
	t.Helper()
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func decodeError(t *testing.T, r io.Reader) (message, errType, code string) {
	t.Helper()
	var envelope struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(r).Decode(&envelope); err != nil {
		t.Fatalf("decode error envelope: %v", err)
	}
	return envelope.Error.Message, envelope.Error.Type, envelope.Error.Code
}

func TestNonStreamPassthrough(t *testing.T) {
	upstreamBody := `{"id":"resp-1","choices":[{"message":{"content":"ok"}}],"nonstandard":{"x":[1,2,3]}}`
	gotRaw := make(chan []byte, 1)
	prov := &fakeProvider{
		name: "fp",
		chatFn: func(ctx context.Context, req *providers.ChatRequest) (*providers.ChatResponse, error) {
			gotRaw <- append([]byte(nil), req.Raw...)
			return &providers.ChatResponse{Model: req.Model, Provider: "fp", Body: json.RawMessage(upstreamBody)}, nil
		},
	}
	ts := newTestServer(t, defaultCfg(), map[string]providers.Provider{"m1": prov})

	sent := `{"model":"m1","messages":[{"role":"user","content":"hi"}],"custom_flag":true}`
	resp := postChat(t, ts, sent)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(body) != upstreamBody {
		t.Errorf("body = %q, want provider body byte-for-byte", body)
	}
	if raw := <-gotRaw; string(raw) != sent {
		t.Errorf("provider saw Raw = %q, want client body byte-for-byte", raw)
	}
}

func TestStreamPassthrough(t *testing.T) {
	prov := &fakeProvider{
		name: "fp",
		streamFn: func(ctx context.Context, req *providers.ChatRequest) (providers.StreamReader, error) {
			return &scriptReader{frames: [][]byte{[]byte(`{"a":1}`), []byte(`{"b":2}`)}}, nil
		},
	}
	ts := newTestServer(t, defaultCfg(), map[string]providers.Provider{"m1": prov})

	resp := postChat(t, ts, `{"model":"m1","stream":true}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("Cache-Control = %q, want no-cache", cc)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	want := "data: {\"a\":1}\n\ndata: {\"b\":2}\n\ndata: [DONE]\n\n"
	if string(body) != want {
		t.Errorf("body = %q, want frame-for-frame %q", body, want)
	}
}

func TestUnknownModelNotFound(t *testing.T) {
	ts := newTestServer(t, defaultCfg(), map[string]providers.Provider{"m1": &fakeProvider{name: "fp"}})

	resp := postChat(t, ts, `{"model":"ghost"}`)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	msg, errType, code := decodeError(t, resp.Body)
	if msg == "" {
		t.Error("error.message is empty")
	}
	if errType != "invalid_request_error" {
		t.Errorf("error.type = %q, want invalid_request_error", errType)
	}
	if code != "model_not_found" {
		t.Errorf("error.code = %q, want model_not_found", code)
	}
}

func TestBadRequestBodies(t *testing.T) {
	ts := newTestServer(t, defaultCfg(), map[string]providers.Provider{"m1": &fakeProvider{name: "fp"}})
	cases := []struct {
		name string
		body string
	}{
		{"invalid json", `{not json`},
		{"missing model", `{"messages":[]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := postChat(t, ts, tc.body)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", resp.StatusCode)
			}
			if _, errType, _ := decodeError(t, resp.Body); errType != "invalid_request_error" {
				t.Errorf("error.type = %q, want invalid_request_error", errType)
			}
		})
	}
}

func TestProviderErrorMapping(t *testing.T) {
	cases := []struct {
		class      providers.ErrorClass
		wantStatus int
	}{
		{providers.ErrClassAuth, http.StatusBadGateway},
		{providers.ErrClassRateLimited, http.StatusTooManyRequests},
		{providers.ErrClassInvalidRequest, http.StatusBadRequest},
		{providers.ErrClassModelNotFound, http.StatusNotFound},
		{providers.ErrClassUpstream, http.StatusBadGateway},
		{providers.ErrClassTimeout, http.StatusGatewayTimeout},
		{providers.ErrClassCanceled, ingress.StatusClientClosedRequest},
		{providers.ErrClassInternal, http.StatusInternalServerError},
	}
	for _, tc := range cases {
		t.Run(string(tc.class), func(t *testing.T) {
			prov := &fakeProvider{
				name: "fp",
				chatFn: func(ctx context.Context, req *providers.ChatRequest) (*providers.ChatResponse, error) {
					return nil, &providers.ProviderError{Provider: "fp", Class: tc.class, Message: "kaput"}
				},
			}
			ts := newTestServer(t, defaultCfg(), map[string]providers.Provider{"m1": prov})

			resp := postChat(t, ts, `{"model":"m1"}`)
			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.wantStatus)
			}
			msg, _, _ := decodeError(t, resp.Body)
			if msg == "" {
				t.Error("error.message is empty")
			}
			if tc.class == providers.ErrClassAuth && msg != "upstream auth failed" {
				t.Errorf("auth message = %q, want the fixed gateway wording", msg)
			}
		})
	}
}

func TestAuthErrorNeverLeaks(t *testing.T) {
	prov := &fakeProvider{
		name: "fp",
		chatFn: func(ctx context.Context, req *providers.ChatRequest) (*providers.ChatResponse, error) {
			return nil, &providers.ProviderError{
				Provider: "fp",
				Class:    providers.ErrClassAuth,
				Message:  "Bearer sk-secret-123 rejected by upstream",
			}
		},
	}
	ts := newTestServer(t, defaultCfg(), map[string]providers.Provider{"m1": prov})

	resp := postChat(t, ts, `{"model":"m1"}`)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if strings.Contains(string(body), "sk-secret-123") {
		t.Fatalf("upstream auth detail leaked to client: %s", body)
	}
	msg, _, _ := decodeError(t, bytes.NewReader(body))
	if msg != "upstream auth failed" {
		t.Errorf("message = %q, want upstream auth failed", msg)
	}
}

func TestUpstreamTimeoutApplied(t *testing.T) {
	prov := &fakeProvider{
		name: "fp",
		chatFn: func(ctx context.Context, req *providers.ChatRequest) (*providers.ChatResponse, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	cfg := defaultCfg()
	cfg.UpstreamTimeoutMS = 40
	ts := newTestServer(t, cfg, map[string]providers.Provider{"m1": prov})

	start := time.Now()
	resp := postChat(t, ts, `{"model":"m1"}`)
	if resp.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504", resp.StatusCode)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("request took %v; upstream timeout not applied", elapsed)
	}
}

func TestClientDisconnectCancelsUpstream(t *testing.T) {
	upstreamCanceled := make(chan struct{})
	prov := &fakeProvider{
		name: "fp",
		streamFn: func(ctx context.Context, req *providers.ChatRequest) (providers.StreamReader, error) {
			go func() {
				<-ctx.Done()
				close(upstreamCanceled)
			}()
			return newBlockingReader(ctx, []byte(`{"n":1}`)), nil
		},
	}
	ts := newTestServer(t, defaultCfg(), map[string]providers.Provider{"m1": prov})

	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"m1","stream":true}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}

	frame := "data: {\"n\":1}\n\n"
	buf := make([]byte, len(frame))
	if _, err := io.ReadFull(resp.Body, buf); err != nil {
		t.Fatalf("read first frame: %v", err)
	}
	if string(buf) != frame {
		t.Fatalf("first frame = %q, want %q", buf, frame)
	}

	// Dropping the response body mid-stream is the client disconnect.
	resp.Body.Close()

	select {
	case <-upstreamCanceled:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream ctx not canceled after client disconnect")
	}
}

func TestStreamIdleTimeoutAborts(t *testing.T) {
	var reader *blockingReader
	prov := &fakeProvider{
		name: "fp",
		streamFn: func(ctx context.Context, req *providers.ChatRequest) (providers.StreamReader, error) {
			reader = newBlockingReader(ctx, []byte(`{"n":1}`))
			return reader, nil
		},
	}
	cfg := defaultCfg()
	cfg.StreamIdleTimeoutMS = 80
	ts := newTestServer(t, cfg, map[string]providers.Provider{"m1": prov})

	start := time.Now()
	resp := postChat(t, ts, `{"model":"m1","stream":true}`)
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("stream ran %v; idle timeout not applied", elapsed)
	}
	first := "data: {\"n\":1}\n\n"
	if !strings.HasPrefix(string(body), first) {
		t.Errorf("body = %q, want it to start with the first frame %q", body, first)
	}
	if strings.Contains(string(body), "[DONE]") {
		t.Error("aborted stream must not carry the [DONE] sentinel")
	}
	// The client is told why the stream stopped rather than being left
	// to infer it from a severed connection.
	if !strings.Contains(string(body), "stream_truncated") {
		t.Errorf("body = %q, want a stream_truncated error frame", body)
	}

	// The gateway must have released the reader on abort.
	select {
	case <-reader.closed:
	case <-time.After(2 * time.Second):
		t.Fatal("reader not closed after idle abort")
	}
}

func TestBodySizeCap(t *testing.T) {
	ts := newTestServer(t, defaultCfg(), map[string]providers.Provider{"m1": &fakeProvider{name: "fp"}})

	big := bytes.Repeat([]byte("a"), 10<<20+1)
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", bytes.NewReader(big))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", resp.StatusCode)
	}
}

func TestModelsEndpoint(t *testing.T) {
	routes := map[string]providers.Provider{
		"zeta-1":  &fakeProvider{name: "prov-z"},
		"alpha-1": &fakeProvider{name: "prov-a"},
	}
	ts := newTestServer(t, defaultCfg(), routes)

	resp, err := http.Get(ts.URL + "/v1/models")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var list struct {
		Object string `json:"object"`
		Data   []struct {
			ID      string `json:"id"`
			Object  string `json:"object"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if list.Object != "list" {
		t.Errorf("object = %q, want list", list.Object)
	}
	if len(list.Data) != 2 {
		t.Fatalf("len(data) = %d, want 2", len(list.Data))
	}
	if list.Data[0].ID != "alpha-1" || list.Data[1].ID != "zeta-1" {
		t.Errorf("ids = %q, %q; want sorted alpha-1, zeta-1", list.Data[0].ID, list.Data[1].ID)
	}
	if list.Data[0].OwnedBy != "prov-a" || list.Data[1].OwnedBy != "prov-z" {
		t.Errorf("owned_by = %q, %q; want prov-a, prov-z", list.Data[0].OwnedBy, list.Data[1].OwnedBy)
	}
	for _, d := range list.Data {
		if d.Object != "model" {
			t.Errorf("entry object = %q, want model", d.Object)
		}
	}
}

func TestHealthz(t *testing.T) {
	ts := newTestServer(t, defaultCfg(), nil)

	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["status"] != "ok" {
		t.Errorf("status field = %q, want ok", body["status"])
	}
}
