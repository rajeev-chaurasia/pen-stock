package openaiwire_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rajeev-chaurasia/pen-stock/internal/config"
	"github.com/rajeev-chaurasia/pen-stock/internal/providers"
	"github.com/rajeev-chaurasia/pen-stock/internal/providers/openaiwire"
)

func chatReq() *providers.ChatRequest {
	return &providers.ChatRequest{
		Model: "llama-3.3-70b",
		Raw:   json.RawMessage(`{"model":"llama-3.3-70b","messages":[{"role":"user","content":"hey"}],"seed":42}`),
	}
}

func assertClass(t *testing.T, err error, want providers.ErrorClass) *providers.ProviderError {
	t.Helper()
	var pe *providers.ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("want *providers.ProviderError, got %T: %v", err, err)
	}
	if pe.Class != want {
		t.Fatalf("class = %q, want %q", pe.Class, want)
	}
	return pe
}

type capturedRequest struct {
	path        string
	auth        string
	contentType string
	body        []byte
}

func TestChatHappyPath(t *testing.T) {
	const respBody = `{"id":"cmpl-1","object":"chat.completion",` +
		`"choices":[{"message":{"role":"assistant","content":"hi"}}],` +
		`"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`

	got := make(chan capturedRequest, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got <- capturedRequest{
			path:        r.URL.Path,
			auth:        r.Header.Get("Authorization"),
			contentType: r.Header.Get("Content-Type"),
			body:        body,
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, respBody)
	}))
	defer upstream.Close()

	p := openaiwire.New("groq", upstream.URL+"/", "sk-test", nil)
	if p.Name() != "groq" {
		t.Fatalf("Name() = %q, want groq", p.Name())
	}

	req := chatReq()
	resp, err := p.Chat(context.Background(), req)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}

	c := <-got
	if c.path != "/chat/completions" {
		t.Errorf("upstream path = %q, want /chat/completions", c.path)
	}
	if c.auth != "Bearer sk-test" {
		t.Errorf("Authorization = %q, want Bearer sk-test", c.auth)
	}
	if c.contentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", c.contentType)
	}
	if string(c.body) != string(req.Raw) {
		t.Errorf("forwarded body = %q, want raw request untouched %q", c.body, req.Raw)
	}

	if string(resp.Body) != respBody {
		t.Errorf("Body = %q, want upstream body verbatim", resp.Body)
	}
	if resp.Provider != "groq" || resp.Model != "llama-3.3-70b" {
		t.Errorf("Provider/Model = %q/%q", resp.Provider, resp.Model)
	}
	want := providers.Usage{PromptTokens: 7, CompletionTokens: 3, TotalTokens: 10}
	if resp.Usage != want {
		t.Errorf("Usage = %+v, want %+v", resp.Usage, want)
	}
}

func TestChatUpstreamStatusMapping(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		body      string
		wantClass providers.ErrorClass
		wantMsg   string
	}{
		{"bad request", 400, `{"error":{"message":"bad payload"}}`, providers.ErrClassInvalidRequest, "bad payload"},
		{"unauthorized", 401, `{"error":{"message":"invalid key"}}`, providers.ErrClassAuth, "invalid key"},
		{"forbidden", 403, `{"error":{"message":"no access"}}`, providers.ErrClassAuth, "no access"},
		{"not found", 404, `{"error":{"message":"no such model"}}`, providers.ErrClassModelNotFound, "no such model"},
		{"unprocessable", 422, `{"error":{"message":"bad schema"}}`, providers.ErrClassInvalidRequest, "bad schema"},
		{"rate limited", 429, `{"error":{"message":"slow down"}}`, providers.ErrClassRateLimited, "slow down"},
		{"server error", 500, `{"error":{"message":"boom"}}`, providers.ErrClassUpstream, "boom"},
		{"unavailable plain body", 503, "service melting", providers.ErrClassUpstream, "service melting"},
		{"teapot empty body", 418, "", providers.ErrClassInternal, "upstream returned status 418"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer upstream.Close()

			p := openaiwire.New("fp", upstream.URL, "k", nil)
			_, err := p.Chat(context.Background(), chatReq())
			pe := assertClass(t, err, tc.wantClass)
			if pe.StatusCode != tc.status {
				t.Errorf("StatusCode = %d, want %d", pe.StatusCode, tc.status)
			}
			if pe.Message != tc.wantMsg {
				t.Errorf("Message = %q, want %q", pe.Message, tc.wantMsg)
			}
		})
	}
}

func TestChatContextDeadline(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Drain the body so the server watches the connection and cancels
		// r.Context() when the client gives up.
		_, _ = io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
	}))
	defer upstream.Close()

	p := openaiwire.New("slow", upstream.URL, "k", nil)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := p.Chat(ctx, chatReq())
	assertClass(t, err, providers.ErrClassTimeout)
}

func TestChatContextCanceled(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
	}))
	defer upstream.Close()

	p := openaiwire.New("slow", upstream.URL, "k", nil)
	ctx, cancel := context.WithCancel(context.Background())
	timer := time.AfterFunc(50*time.Millisecond, cancel)
	defer timer.Stop()
	defer cancel()

	_, err := p.Chat(ctx, chatReq())
	assertClass(t, err, providers.ErrClassCanceled)
}

func TestChatStreamUpstreamError(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"message":"bad key"}}`)
	}))
	defer upstream.Close()

	p := openaiwire.New("fp", upstream.URL, "k", nil)
	reader, err := p.ChatStream(context.Background(), chatReq())
	if reader != nil {
		t.Fatal("want nil reader on upstream error")
	}
	pe := assertClass(t, err, providers.ErrClassAuth)
	if pe.Message != "bad key" {
		t.Errorf("Message = %q, want bad key", pe.Message)
	}
}

func TestBuildAll(t *testing.T) {
	t.Run("known kinds", func(t *testing.T) {
		m, err := providers.BuildAll([]config.ProviderConfig{
			{Name: "groq-main", Kind: config.KindGroq, BaseURL: "https://api.groq.com/openai/v1", APIKey: "k1"},
			{Name: "sim", Kind: config.KindOpenAICompat, BaseURL: "http://127.0.0.1:9999/v1", APIKey: "k2"},
		})
		if err != nil {
			t.Fatalf("BuildAll: %v", err)
		}
		if len(m) != 2 {
			t.Fatalf("len = %d, want 2", len(m))
		}
		for _, name := range []string{"groq-main", "sim"} {
			p, ok := m[name]
			if !ok {
				t.Fatalf("missing provider %q", name)
			}
			if p.Name() != name {
				t.Errorf("Name() = %q, want %q", p.Name(), name)
			}
		}
	})

	t.Run("unknown kind names the entry", func(t *testing.T) {
		_, err := providers.BuildAll([]config.ProviderConfig{
			{Name: "mystery", Kind: "anthropic", BaseURL: "https://example.com"},
		})
		if err == nil {
			t.Fatal("want error for unknown kind")
		}
		if !strings.Contains(err.Error(), "mystery") || !strings.Contains(err.Error(), "anthropic") {
			t.Errorf("error %q should name the entry and its kind", err)
		}
	})

	t.Run("missing base_url", func(t *testing.T) {
		_, err := providers.BuildAll([]config.ProviderConfig{
			{Name: "sim-bad", Kind: config.KindOpenAICompat},
		})
		if err == nil {
			t.Fatal("want error for missing base_url")
		}
		if !strings.Contains(err.Error(), "sim-bad") || !strings.Contains(err.Error(), "base_url") {
			t.Errorf("error %q should name the entry and the missing field", err)
		}
	})

	t.Run("duplicate name", func(t *testing.T) {
		_, err := providers.BuildAll([]config.ProviderConfig{
			{Name: "dup", Kind: config.KindGroq, BaseURL: "https://a.example"},
			{Name: "dup", Kind: config.KindGroq, BaseURL: "https://b.example"},
		})
		if err == nil {
			t.Fatal("want error for duplicate name")
		}
		if !strings.Contains(err.Error(), "dup") {
			t.Errorf("error %q should name the duplicate entry", err)
		}
	})
}
