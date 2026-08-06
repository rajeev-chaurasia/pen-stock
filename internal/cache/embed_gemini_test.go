package cache

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const testAPIKey = "AIzaSyTESTKEY_do_not_log_me"

// capturedRequest is what the fake upstream saw, so a test can assert
// the wire shape rather than the code that produced it.
type capturedRequest struct {
	method string
	path   string
	query  string
	header http.Header
	body   embedRequest
	raw    string
	calls  int
}

func newEmbedServer(t *testing.T, status int, response string) (*httptest.Server, *capturedRequest) {
	t.Helper()
	got := &capturedRequest{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.calls++
		got.method = r.Method
		got.path = r.URL.Path
		got.query = r.URL.RawQuery
		got.header = r.Header.Clone()
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading request body: %v", err)
		}
		got.raw = string(raw)
		if err := json.Unmarshal(raw, &got.body); err != nil {
			t.Errorf("request body is not a batchEmbedContents body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if _, err := io.WriteString(w, response); err != nil {
			t.Errorf("writing response: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, got
}

func TestEmbedRequestShapeAndAuthHeader(t *testing.T) {
	srv, got := newEmbedServer(t, http.StatusOK, `{"embeddings":[{"values":[0.1,0.2]}]}`)
	e := NewGeminiEmbedder(srv.URL, testAPIKey, "", srv.Client())

	if _, err := e.Embed(context.Background(), []string{"how do I rotate a key"}); err != nil {
		t.Fatalf("Embed: %v", err)
	}

	if got.method != http.MethodPost {
		t.Errorf("method = %q, want POST", got.method)
	}
	if want := "/models/" + DefaultEmbedModel + ":batchEmbedContents"; got.path != want {
		t.Errorf("path = %q, want %q", got.path, want)
	}
	if got.header.Get(embedAuthHeader) != testAPIKey {
		t.Errorf("%s = %q, want the API key", embedAuthHeader, got.header.Get(embedAuthHeader))
	}
	if ct := got.header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	// The key must never reach the query string, where it would be
	// copied into every access log and trace along the way.
	if got.query != "" {
		t.Errorf("query string = %q, want none", got.query)
	}
	if strings.Contains(got.query, testAPIKey) {
		t.Error("the API key reached the query string")
	}

	if len(got.body.Requests) != 1 {
		t.Fatalf("sent %d requests, want 1", len(got.body.Requests))
	}
	item := got.body.Requests[0]
	if want := "models/" + DefaultEmbedModel; item.Model != want {
		t.Errorf("nested model = %q, want %q", item.Model, want)
	}
	if len(item.Content.Parts) != 1 || item.Content.Parts[0].Text != "how do I rotate a key" {
		t.Errorf("content parts = %+v, want the single input text", item.Content.Parts)
	}
}

func TestEmbedBatchesInInputOrder(t *testing.T) {
	srv, got := newEmbedServer(t, http.StatusOK,
		`{"embeddings":[{"values":[1,0,0]},{"values":[0,1,0]},{"values":[0,0,1]}]}`)
	e := NewGeminiEmbedder(srv.URL, testAPIKey, DefaultEmbedModel, srv.Client())

	texts := []string{"first", "second", "third"}
	vectors, err := e.Embed(context.Background(), texts)
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}

	if len(got.body.Requests) != len(texts) {
		t.Fatalf("sent %d requests, want %d", len(got.body.Requests), len(texts))
	}
	for i, text := range texts {
		if sent := got.body.Requests[i].Content.Parts[0].Text; sent != text {
			t.Errorf("request %d carried %q, want %q", i, sent, text)
		}
	}

	want := [][]float32{{1, 0, 0}, {0, 1, 0}, {0, 0, 1}}
	if len(vectors) != len(want) {
		t.Fatalf("got %d vectors, want %d", len(vectors), len(want))
	}
	for i := range want {
		if cosineSimilarity(vectors[i], want[i]) != 1 {
			t.Errorf("vector %d = %v, want %v", i, vectors[i], want[i])
		}
	}
}

func TestEmbedEmptyInputSkipsUpstream(t *testing.T) {
	srv, got := newEmbedServer(t, http.StatusOK, `{"embeddings":[]}`)
	e := NewGeminiEmbedder(srv.URL, testAPIKey, "", srv.Client())

	for _, texts := range [][]string{nil, {}} {
		vectors, err := e.Embed(context.Background(), texts)
		if err != nil {
			t.Fatalf("Embed(%v): %v", texts, err)
		}
		if len(vectors) != 0 {
			t.Fatalf("Embed(%v) returned %d vectors, want none", texts, len(vectors))
		}
	}
	if got.calls != 0 {
		t.Fatalf("upstream was called %d times for empty input, want 0", got.calls)
	}
}

func TestEmbedErrorNeverCarriesTheKey(t *testing.T) {
	// The upstream is made hostile on purpose: it echoes the key back in
	// its error body. Google does not do this, but the error text is not
	// something this package controls, and it ends up in logs.
	body := `{"error":{"code":401,"message":"API key not valid: ` + testAPIKey + `","status":"UNAUTHENTICATED"}}`
	srv, _ := newEmbedServer(t, http.StatusUnauthorized, body)
	e := NewGeminiEmbedder(srv.URL, testAPIKey, "", srv.Client())

	vectors, err := e.Embed(context.Background(), []string{"anything"})
	if err == nil {
		t.Fatal("a 401 produced no error")
	}
	if vectors != nil {
		t.Fatalf("a failed embed returned %d vectors, want none", len(vectors))
	}
	if !errors.Is(err, ErrEmbedFailed) {
		t.Errorf("error %v is not an ErrEmbedFailed, so a caller cannot treat it as a miss", err)
	}
	if strings.Contains(err.Error(), testAPIKey) {
		t.Fatal("the API key appears in the error message")
	}
	if !strings.Contains(err.Error(), redactedKey) {
		t.Errorf("error %q does not show where the key was removed", err)
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error %q does not report the upstream status", err)
	}
}

func TestEmbedNon2xxStatuses(t *testing.T) {
	for _, status := range []int{
		http.StatusBadRequest,
		http.StatusForbidden,
		http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusServiceUnavailable,
	} {
		srv, _ := newEmbedServer(t, status, "")
		e := NewGeminiEmbedder(srv.URL, testAPIKey, "", srv.Client())

		_, err := e.Embed(context.Background(), []string{"anything"})
		if err == nil {
			t.Errorf("status %d produced no error", status)
			continue
		}
		if !errors.Is(err, ErrEmbedFailed) {
			t.Errorf("status %d produced %v, which is not an ErrEmbedFailed", status, err)
		}
		if strings.Contains(err.Error(), testAPIKey) {
			t.Errorf("status %d leaked the API key", status)
		}
	}
}

func TestEmbedRejectsMalformedResponses(t *testing.T) {
	tests := []struct {
		name     string
		inputs   []string
		response string
	}{
		{"not json", []string{"a"}, `{"embeddings":`},
		{"html error page", []string{"a"}, `<html><body>502 Bad Gateway</body></html>`},
		{"empty body", []string{"a"}, ``},
		{"no embeddings field", []string{"a"}, `{}`},
		{"fewer vectors than inputs", []string{"a", "b"}, `{"embeddings":[{"values":[1,2]}]}`},
		{"more vectors than inputs", []string{"a"}, `{"embeddings":[{"values":[1,2]},{"values":[3,4]}]}`},
		{"empty vector", []string{"a"}, `{"embeddings":[{"values":[]}]}`},
		{"missing values", []string{"a"}, `{"embeddings":[{}]}`},
		{"second vector empty", []string{"a", "b"}, `{"embeddings":[{"values":[1,2]},{"values":[]}]}`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := newEmbedServer(t, http.StatusOK, tc.response)
			e := NewGeminiEmbedder(srv.URL, testAPIKey, "", srv.Client())

			vectors, err := e.Embed(context.Background(), tc.inputs)
			if err == nil {
				t.Fatalf("a malformed response produced %d vectors and no error", len(vectors))
			}
			// A partial answer is the dangerous outcome: it pairs a
			// vector with whichever input happens to sit at that index.
			if vectors != nil {
				t.Fatalf("a malformed response returned %d vectors, want none", len(vectors))
			}
			if !errors.Is(err, ErrEmbedFailed) {
				t.Errorf("error %v is not an ErrEmbedFailed", err)
			}
		})
	}
}

func TestEmbedUnreachableUpstreamIsAnOrdinaryError(t *testing.T) {
	srv, _ := newEmbedServer(t, http.StatusOK, `{"embeddings":[{"values":[1]}]}`)
	client := srv.Client()
	url := srv.URL
	srv.Close()

	e := NewGeminiEmbedder(url, testAPIKey, "", client)
	_, err := e.Embed(context.Background(), []string{"anything"})
	if err == nil {
		t.Fatal("a dead upstream produced no error")
	}
	if !errors.Is(err, ErrEmbedFailed) {
		t.Errorf("error %v is not an ErrEmbedFailed", err)
	}
	if strings.Contains(err.Error(), testAPIKey) {
		t.Error("a transport error leaked the API key")
	}
}

func TestEmbedCancelledContext(t *testing.T) {
	srv, got := newEmbedServer(t, http.StatusOK, `{"embeddings":[{"values":[1]}]}`)
	e := NewGeminiEmbedder(srv.URL, testAPIKey, "", srv.Client())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := e.Embed(ctx, []string{"anything"})
	if err == nil {
		t.Fatal("a cancelled context produced no error")
	}
	if !errors.Is(err, ErrEmbedFailed) {
		t.Errorf("error %v is not an ErrEmbedFailed", err)
	}
	// The cause survives wrapping, so a caller can still tell its own
	// cancellation apart from an upstream fault.
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error %v does not report the cancellation", err)
	}
	if got.calls != 0 {
		t.Errorf("a cancelled request still reached the upstream %d times", got.calls)
	}
}

func TestEmbedDefaults(t *testing.T) {
	e, ok := NewGeminiEmbedder("", testAPIKey, "", nil).(*geminiEmbedder)
	if !ok {
		t.Fatal("NewGeminiEmbedder did not return a geminiEmbedder")
	}
	if e.baseURL != DefaultEmbedBaseURL {
		t.Errorf("base URL = %q, want %q", e.baseURL, DefaultEmbedBaseURL)
	}
	if e.model != DefaultEmbedModel {
		t.Errorf("model = %q, want %q", e.model, DefaultEmbedModel)
	}
	if e.client == nil {
		t.Error("a nil client was not replaced with a default")
	}
	if want := DefaultEmbedBaseURL + "/models/" + DefaultEmbedModel + ":batchEmbedContents"; e.endpoint() != want {
		t.Errorf("endpoint = %q, want %q", e.endpoint(), want)
	}
}

func TestEmbedEndpointNormalisesModelAndBaseURL(t *testing.T) {
	tests := []struct {
		name     string
		baseURL  string
		model    string
		wantPath string
	}{
		{"bare model", "https://example.test/v1beta", "text-embedding-004", "/models/text-embedding-004:batchEmbedContents"},
		// An operator copying the model name out of the API docs brings
		// the prefix with it, and must not get models/models/....
		{"qualified model", "https://example.test/v1beta", "models/text-embedding-004", "/models/text-embedding-004:batchEmbedContents"},
		{"trailing slash on base", "https://example.test/v1beta/", "embedding-001", "/models/embedding-001:batchEmbedContents"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e, ok := NewGeminiEmbedder(tc.baseURL, testAPIKey, tc.model, nil).(*geminiEmbedder)
			if !ok {
				t.Fatal("NewGeminiEmbedder did not return a geminiEmbedder")
			}
			if want := "https://example.test/v1beta" + tc.wantPath; e.endpoint() != want {
				t.Errorf("endpoint = %q, want %q", e.endpoint(), want)
			}
			if want := "models/" + strings.TrimPrefix(tc.model, "models/"); e.qualified != want {
				t.Errorf("qualified model = %q, want %q", e.qualified, want)
			}
		})
	}
}

func TestEmbedDimensions(t *testing.T) {
	tests := []struct {
		model string
		want  int
	}{
		{"", DefaultEmbedDimensions},
		// Width confirmed by counting a live response rather than taken
		// from documentation.
		{DefaultEmbedModel, 3072},
		{"models/text-embedding-004", legacyEmbedDimensions},
		{"embedding-001", legacyEmbedDimensions},
		// An unknown model reports an unknown width rather than a guess,
		// so nothing downstream validates against a number this package
		// made up.
		{"some-future-model", 0},
	}

	for _, tc := range tests {
		t.Run(tc.model, func(t *testing.T) {
			if got := NewGeminiEmbedder("", testAPIKey, tc.model, nil).Dimensions(); got != tc.want {
				t.Errorf("Dimensions() = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestEmbedderFeedsSemanticStore is the seam between the two halves of
// this tier: whatever width the embedder produces is the width the store
// adopts, without either side being told the number.
func TestEmbedderFeedsSemanticStore(t *testing.T) {
	srv, _ := newEmbedServer(t, http.StatusOK, `{"embeddings":[{"values":[0.6,0.8,0.0]}]}`)
	e := NewGeminiEmbedder(srv.URL, testAPIKey, "", srv.Client())
	ctx := context.Background()

	vectors, err := e.Embed(ctx, []string{"how do I rotate a key"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}

	s := NewSemantic(SemanticOptions{})
	s.Add(ctx, "acme", vectors[0], entry("stored"))

	got, score, ok := s.Nearest(ctx, "acme", vectors[0])
	if !ok {
		t.Fatal("an embedded vector did not match itself")
	}
	if string(got.Body) != "stored" || score != 1 {
		t.Fatalf("got %q at score %v, want the stored entry at 1", got.Body, score)
	}
}
