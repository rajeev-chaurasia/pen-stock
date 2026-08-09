package ingress_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rajeev-chaurasia/pen-stock/internal/cache"
	"github.com/rajeev-chaurasia/pen-stock/internal/ingress"
	"github.com/rajeev-chaurasia/pen-stock/internal/providers"
)

// constantEmbedder maps every question to the same vector, so any two
// prompts are perfectly similar. That makes a semantic hit certain
// without reaching an embedding model, which is what lets this test
// assert on the label rather than on the matching.
type constantEmbedder struct{}

func (constantEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = []float32{1, 0, 0, 0}
	}
	return out, nil
}

func (constantEmbedder) Dimensions() int { return 4 }

func newSemanticServer(t *testing.T, prov providers.Provider) *httptest.Server {
	t.Helper()
	lookup := cache.NewLookup(cache.LookupOptions{
		Exact:    cache.NewExact(cache.ExactOptions{}),
		Semantic: cache.NewSemantic(cache.SemanticOptions{Threshold: 0.95}),
		Embedder: constantEmbedder{},
	})
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := ingress.NewServer(defaultCfg(), map[string]providers.Provider{"m": prov}, log,
		ingress.WithCache(lookup))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

// A streamed answer served from the semantic tier used to report
// hit-exact, because the replay path named the label instead of asking
// what had been found. The header is the only way an operator can tell
// a same question hit from a similar question one, so a wrong label
// hides exactly the thing the similarity threshold is tuned against.
func TestStreamedSemanticHitIsLabelledSemantic(t *testing.T) {
	prov := &countingChat{
		name:   "groq",
		frames: [][]byte{[]byte(`{"choices":[{"delta":{"content":"a pipe"}}]}`)},
		usage:  providers.Usage{PromptTokens: 8, CompletionTokens: 4},
	}
	ts := newSemanticServer(t, prov)

	const stored = `{"model":"m","stream":true,"temperature":0,"messages":[{"role":"user","content":"what is a penstock"}]}`
	// A different question, so only the semantic tier can answer it.
	const similar = `{"model":"m","stream":true,"temperature":0,"messages":[{"role":"user","content":"describe a penstock please"}]}`

	first := postChat(t, ts, stored)
	drain(t, first)
	if got := prov.count(); got != 1 {
		t.Fatalf("upstream calls after the first request = %d, want 1", got)
	}

	second := postChat(t, ts, similar)
	drain(t, second)
	if got := prov.count(); got != 1 {
		t.Fatalf("upstream calls after a similar question = %d, want the semantic tier to have answered", got)
	}
	if got := second.Header.Get("X-Penstock-Cache"); got != "hit-semantic" {
		t.Errorf("cache header = %q, want hit-semantic: the answer came from a different question", got)
	}
}

// The same label, checked on the whole response path, so the two
// transports cannot drift apart again.
func TestWholeSemanticHitIsLabelledSemantic(t *testing.T) {
	prov := &countingChat{name: "groq", body: []byte(`{"answer":"a pipe"}`),
		usage: providers.Usage{PromptTokens: 8, CompletionTokens: 4}}
	ts := newSemanticServer(t, prov)

	first := postChat(t, ts, `{"model":"m","temperature":0,"messages":[{"role":"user","content":"what is a penstock"}]}`)
	drain(t, first)
	second := postChat(t, ts, `{"model":"m","temperature":0,"messages":[{"role":"user","content":"describe a penstock please"}]}`)
	drain(t, second)

	if got := prov.count(); got != 1 {
		t.Fatalf("upstream calls = %d, want the semantic tier to have answered the second", got)
	}
	if got := second.Header.Get("X-Penstock-Cache"); got != "hit-semantic" {
		t.Errorf("cache header = %q, want hit-semantic", got)
	}
}

// The 413 body used to quote a limit ten times the one being enforced,
// so a client trimming its payload to the advertised figure would be
// refused again. The number now comes from the constant.
func TestOversizedBodyReportsTheLimitItActuallyEnforces(t *testing.T) {
	prov := &countingChat{name: "groq", body: []byte(`{}`)}
	ts := newCachedServer(t, prov, nil)

	huge := `{"model":"m","messages":[{"role":"user","content":"` +
		strings.Repeat("x", 2<<20) + `"}]}`
	resp := postChat(t, ts, huge)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", resp.StatusCode)
	}
	if strings.Contains(string(body), "10MB") {
		t.Errorf("the message still advertises 10MB while 1 MiB is enforced: %s", body)
	}
	if !strings.Contains(string(body), "1 MiB") {
		t.Errorf("message = %s, want it to name the limit actually enforced", body)
	}
}

func drain(t *testing.T, resp *http.Response) {
	t.Helper()
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
}
