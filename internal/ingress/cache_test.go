package ingress_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/rajeev-chaurasia/pen-stock/internal/cache"
	"github.com/rajeev-chaurasia/pen-stock/internal/ingress"
	"github.com/rajeev-chaurasia/pen-stock/internal/providers"
)

// countingChat answers every call and records how many reached it,
// which is the only assertion that proves a hit skipped the provider.
type countingChat struct {
	name   string
	mu     sync.Mutex
	calls  int
	body   []byte
	frames [][]byte
	usage  providers.Usage
}

func (c *countingChat) Name() string { return c.name }

func (c *countingChat) Chat(context.Context, *providers.ChatRequest) (*providers.ChatResponse, error) {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	return &providers.ChatResponse{Body: c.body, Provider: c.name, Usage: c.usage}, nil
}

func (c *countingChat) ChatStream(context.Context, *providers.ChatRequest) (providers.StreamReader, error) {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	return &usageReader{frames: c.frames, usage: c.usage}, nil
}

func (c *countingChat) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

func newCachedServer(t *testing.T, prov providers.Provider, sink ingress.MetricsSink) *httptest.Server {
	t.Helper()
	var events []cache.Event
	var mu sync.Mutex
	// One callback on every tier, the way the binary wires it. The exact
	// tier reports the hits, misses and stores it alone observes, and the
	// lookup reports only what no tier can see. Wiring it at the lookup
	// alone records nothing.
	record := func(e cache.Event) {
		mu.Lock()
		events = append(events, e)
		mu.Unlock()
		if sink != nil {
			sink.AddCacheEvent(string(e))
		}
	}
	lookup := cache.NewLookup(cache.LookupOptions{
		Exact:   cache.NewExact(cache.ExactOptions{OnEvent: record}),
		OnEvent: record,
	})
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	opts := []ingress.Option{ingress.WithCache(lookup)}
	if sink != nil {
		opts = append(opts, ingress.WithMetrics(sink))
	}
	srv := ingress.NewServer(defaultCfg(), map[string]providers.Provider{"m": prov}, log, opts...)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

const zeroTempBody = `{"model":"m","temperature":0,"messages":[{"role":"user","content":"what is a penstock"}]}`

func TestCacheHitSkipsTheProvider(t *testing.T) {
	prov := &countingChat{name: "groq", body: []byte(`{"answer":"a pipe"}`),
		usage: providers.Usage{PromptTokens: 8, CompletionTokens: 4}}
	sink := &recordingSink{}
	ts := newCachedServer(t, prov, sink)

	first := postChat(t, ts, zeroTempBody)
	firstBody, _ := io.ReadAll(first.Body)
	if first.StatusCode != http.StatusOK {
		t.Fatalf("first status = %d", first.StatusCode)
	}
	if got := prov.count(); got != 1 {
		t.Fatalf("upstream calls after the first request = %d, want 1", got)
	}

	second := postChat(t, ts, zeroTempBody)
	secondBody, _ := io.ReadAll(second.Body)

	// The whole point: the second request cost no provider call.
	if got := prov.count(); got != 1 {
		t.Errorf("upstream calls after a repeat = %d, want the cache to have answered", got)
	}
	if string(firstBody) != string(secondBody) {
		t.Errorf("cached body %s differs from the original %s", secondBody, firstBody)
	}
	if got := second.Header.Get("X-Penstock-Cache"); got != "hit-exact" {
		t.Errorf("cache header = %q, want a client to be able to tell", got)
	}
	if got := first.Header.Get("X-Penstock-Cache"); got != "" {
		t.Errorf("first response carried a cache header %q", got)
	}

	events := sink.cacheEventSnapshot()
	if !containsString(events, string(cache.EventMiss)) || !containsString(events, string(cache.EventExactHit)) {
		t.Errorf("cache events = %v, want a miss then a hit", events)
	}
}

func TestStreamHitReplaysFrames(t *testing.T) {
	prov := &countingChat{
		name:   "groq",
		frames: [][]byte{[]byte(`{"d":"Hel"}`), []byte(`{"d":"lo"}`)},
		usage:  providers.Usage{PromptTokens: 3, CompletionTokens: 2},
	}
	ts := newCachedServer(t, prov, nil)
	streamBody := `{"model":"m","temperature":0,"stream":true,"messages":[{"role":"user","content":"greet"}]}`

	first := postChat(t, ts, streamBody)
	firstRaw, _ := io.ReadAll(first.Body)
	if prov.count() != 1 {
		t.Fatalf("upstream calls = %d, want 1", prov.count())
	}

	second := postChat(t, ts, streamBody)
	secondRaw, _ := io.ReadAll(second.Body)

	if got := prov.count(); got != 1 {
		t.Errorf("upstream calls after a repeat = %d, want the cache to have replayed", got)
	}
	// A replayed stream has to look like a stream, terminator included,
	// or a client parsing SSE will hang waiting for one.
	if !strings.HasSuffix(string(secondRaw), "data: [DONE]\n\n") {
		t.Errorf("replayed stream = %q, want it terminated like a live one", secondRaw)
	}
	if !strings.Contains(string(secondRaw), `{"d":"Hel"}`) || !strings.Contains(string(secondRaw), `{"d":"lo"}`) {
		t.Errorf("replayed stream lost frames: %q", secondRaw)
	}
	if got := second.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		t.Errorf("replayed content type = %q", got)
	}
	// The frames themselves should match what the live stream sent.
	if countFrames(string(firstRaw)) != countFrames(string(secondRaw)) {
		t.Errorf("replay has %d frames, live had %d", countFrames(string(secondRaw)), countFrames(string(firstRaw)))
	}
}

// Policy refusals must reach the provider every time, or a caller who
// asked for variety would silently get one answer forever.
func TestVaryingRequestsAreNeverCached(t *testing.T) {
	prov := &countingChat{name: "groq", body: []byte(`{"a":1}`)}
	sink := &recordingSink{}
	ts := newCachedServer(t, prov, sink)
	varying := `{"model":"m","temperature":0.9,"messages":[{"role":"user","content":"surprise me"}]}`

	for i := 0; i < 3; i++ {
		resp := postChat(t, ts, varying)
		_, _ = io.ReadAll(resp.Body)
	}
	if got := prov.count(); got != 3 {
		t.Errorf("upstream calls = %d, want 3: a varying request must never be served from cache", got)
	}
	if events := sink.cacheEventSnapshot(); !containsString(events, string(cache.EventIneligible)) {
		t.Errorf("cache events = %v, want the refusal recorded", events)
	}
}

func TestGatewayWithoutCacheStillServes(t *testing.T) {
	prov := &countingChat{name: "groq", body: []byte(`{"a":1}`)}
	ts := newTestServer(t, defaultCfg(), map[string]providers.Provider{"m": prov})

	for i := 0; i < 2; i++ {
		resp := postChat(t, ts, zeroTempBody)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d without a cache configured", resp.StatusCode)
		}
		_, _ = io.ReadAll(resp.Body)
	}
	if got := prov.count(); got != 2 {
		t.Errorf("upstream calls = %d, want every request forwarded when caching is off", got)
	}
}

func countFrames(body string) int {
	return strings.Count(body, "data: ") - strings.Count(body, "data: [DONE]")
}

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
