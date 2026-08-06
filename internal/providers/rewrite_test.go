package providers_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/rajeev-chaurasia/pen-stock/internal/providers"
)

type capturingProvider struct {
	name string
	seen *providers.ChatRequest
}

func (c *capturingProvider) Name() string { return c.name }

func (c *capturingProvider) Chat(_ context.Context, req *providers.ChatRequest) (*providers.ChatResponse, error) {
	c.seen = req
	return &providers.ChatResponse{Provider: c.name}, nil
}

func (c *capturingProvider) ChatStream(_ context.Context, req *providers.ChatRequest) (providers.StreamReader, error) {
	c.seen = req
	return nil, nil
}

func TestWithModelRetargetsTheRequest(t *testing.T) {
	inner := &capturingProvider{name: "mistral"}
	wrapped := providers.WithModel(inner, "mistral-small-latest")

	original := &providers.ChatRequest{
		Model: "auto",
		Raw:   json.RawMessage(`{"model":"auto","messages":[{"role":"user","content":"hi"}],"temperature":0.5}`),
	}
	if _, err := wrapped.Chat(context.Background(), original); err != nil {
		t.Fatalf("Chat: %v", err)
	}

	if inner.seen.Model != "mistral-small-latest" {
		t.Errorf("envelope model = %q, want the upstream name", inner.seen.Model)
	}
	var body map[string]any
	if err := json.Unmarshal(inner.seen.Raw, &body); err != nil {
		t.Fatalf("decode forwarded body: %v", err)
	}
	if body["model"] != "mistral-small-latest" {
		t.Errorf("body model = %v, want the upstream name", body["model"])
	}
	// Everything the client sent has to survive the rewrite.
	if body["temperature"] != 0.5 {
		t.Errorf("temperature = %v, want it preserved", body["temperature"])
	}
	if _, ok := body["messages"]; !ok {
		t.Error("messages were dropped by the rewrite")
	}

	// A chain hands the same request to several providers in turn, so
	// rewriting for one must not corrupt it for the next.
	if original.Model != "auto" {
		t.Errorf("caller's request was mutated: model = %q", original.Model)
	}
	if !strings.Contains(string(original.Raw), `"model":"auto"`) {
		t.Errorf("caller's raw body was mutated: %s", original.Raw)
	}
}

func TestWithModelIsIdentityWhenNoOverrideIsGiven(t *testing.T) {
	inner := &capturingProvider{name: "groq"}
	if got := providers.WithModel(inner, ""); got != providers.Provider(inner) {
		t.Error("an empty override should return the provider unchanged")
	}
}

func TestWithModelRejectsAMalformedBody(t *testing.T) {
	inner := &capturingProvider{name: "groq"}
	wrapped := providers.WithModel(inner, "real-model")

	_, err := wrapped.Chat(context.Background(), &providers.ChatRequest{
		Model: "auto",
		Raw:   json.RawMessage(`{"model":`),
	})
	if err == nil {
		t.Fatal("Chat = nil error for an unparseable body")
	}
	if inner.seen != nil {
		t.Error("a malformed body was still forwarded upstream")
	}
}

func TestWithModelKeepsNumericNotation(t *testing.T) {
	// Round tripping through float64 would rewrite a large integer into
	// exponent form and change what the upstream receives.
	inner := &capturingProvider{name: "groq"}
	wrapped := providers.WithModel(inner, "m")

	if _, err := wrapped.Chat(context.Background(), &providers.ChatRequest{
		Raw: json.RawMessage(`{"model":"auto","seed":12345678901234567890}`),
	}); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if !strings.Contains(string(inner.seen.Raw), "12345678901234567890") {
		t.Errorf("forwarded body lost the original number notation: %s", inner.seen.Raw)
	}
}
