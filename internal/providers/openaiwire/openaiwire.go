// Package openaiwire adapts any backend speaking the OpenAI chat wire
// protocol (OpenAI, Groq, Cerebras, Mistral, OpenRouter, llmsim, vLLM
// and friends) to the providers contract. They share one implementation;
// the little that differs per vendor lives in a profile.
package openaiwire

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/rajeev-chaurasia/pen-stock/internal/config"
	"github.com/rajeev-chaurasia/pen-stock/internal/providers"
)

const (
	chatCompletionsPath = "/chat/completions"

	// maxErrorBody caps how much of an upstream failure body is read to
	// build the error message.
	maxErrorBody = 8 << 10

	// maxResponseBytes caps a non-streaming upstream body. Chat
	// completions run to a few MB at most, so 32 MiB is generous while
	// keeping a hostile upstream from forcing unbounded allocation.
	maxResponseBytes int64 = 32 << 20
)

type provider struct {
	name    string
	baseURL string
	apiKey  string
	profile profile
	client  *http.Client
}

// New returns a Provider for an OpenAI-wire endpoint rooted at baseURL,
// with no vendor specific behavior: the bare wire format, which is what
// a self hosted backend gets. Vendor quirks arrive through the profile
// registry instead, keyed by config kind.
// A nil client gets a dedicated pooled default so providers never share
// transport state through a global.
func New(name, baseURL, apiKey string, client *http.Client) providers.Provider {
	return newWithProfile(name, baseURL, apiKey, profiles[config.KindOpenAICompat], client)
}

func newWithProfile(name, baseURL, apiKey string, prof profile, client *http.Client) providers.Provider {
	if client == nil {
		client = providers.NewHTTPClient()
	}
	return &provider{
		name:    name,
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		profile: prof,
		client:  client,
	}
}

func (p *provider) Name() string { return p.name }

func (p *provider) Chat(ctx context.Context, req *providers.ChatRequest) (*providers.ChatResponse, error) {
	resp, err := p.post(ctx, req.Raw)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if !providers.Is2xx(resp.StatusCode) {
		return nil, p.statusError(resp)
	}
	// Read one byte past the cap: a LimitReader hitting its limit is
	// indistinguishable from EOF, so the extra byte reveals truncation.
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, p.transportError(ctx, "read upstream response", err)
	}
	if int64(len(body)) > maxResponseBytes {
		return nil, &providers.ProviderError{
			Provider: p.name,
			Class:    providers.ErrClassUpstream,
			Message:  fmt.Sprintf("upstream response exceeds %d byte limit", maxResponseBytes),
		}
	}

	// Usage parse is best effort: the body is forwarded verbatim, so a
	// shape we do not recognize must not fail the request.
	var envelope struct {
		Usage usageJSON `json:"usage"`
	}
	_ = json.Unmarshal(body, &envelope)

	return &providers.ChatResponse{
		Model:    req.Model,
		Provider: p.name,
		Body:     body,
		Usage:    envelope.Usage.toUsage(),
	}, nil
}

func (p *provider) ChatStream(ctx context.Context, req *providers.ChatRequest) (providers.StreamReader, error) {
	resp, err := p.post(ctx, withStreamUsage(req.Raw, p.profile))
	if err != nil {
		return nil, err
	}
	if !providers.Is2xx(resp.StatusCode) {
		defer func() { _ = resp.Body.Close() }()
		return nil, p.statusError(resp)
	}
	return newStreamReader(ctx, p.name, resp.Body), nil
}

func (p *provider) post(ctx context.Context, raw json.RawMessage) (*http.Response, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+chatCompletionsPath, bytes.NewReader(raw))
	if err != nil {
		return nil, &providers.ProviderError{
			Provider: p.name,
			Class:    providers.ErrClassInternal,
			Message:  "build upstream request",
			Err:      fmt.Errorf("build upstream request: %w", err),
		}
	}
	// Profile headers go first so nothing a vendor asks for can displace
	// the credentials or the content type.
	for name, value := range p.profile.headers {
		httpReq.Header.Set(name, value)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, p.transportError(ctx, "call upstream", err)
	}
	return resp, nil
}

func (p *provider) statusError(resp *http.Response) *providers.ProviderError {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	return &providers.ProviderError{
		Provider:   p.name,
		Class:      providers.ClassFromStatusAndBody(resp.StatusCode, body),
		StatusCode: resp.StatusCode,
		Message:    upstreamErrorMessage(body, resp.StatusCode),
	}
}

func (p *provider) transportError(ctx context.Context, op string, err error) *providers.ProviderError {
	class := providers.ErrClassUpstream
	switch {
	case errors.Is(err, context.DeadlineExceeded) || ctx.Err() == context.DeadlineExceeded:
		class = providers.ErrClassTimeout
	case errors.Is(err, context.Canceled) || ctx.Err() == context.Canceled:
		class = providers.ErrClassCanceled
	}
	return &providers.ProviderError{
		Provider: p.name,
		Class:    class,
		Message:  op + " failed",
		Err:      fmt.Errorf("%s: %w", op, err),
	}
}

// upstreamErrorMessage prefers the OpenAI error envelope, then raw body
// text, then the bare status code.
func upstreamErrorMessage(body []byte, status int) string {
	var envelope struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err == nil && envelope.Error.Message != "" {
		return envelope.Error.Message
	}
	if s := strings.TrimSpace(string(body)); s != "" {
		return s
	}
	return fmt.Sprintf("upstream returned status %d", status)
}

type usageJSON struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

func (u usageJSON) toUsage() providers.Usage {
	return providers.Usage{
		PromptTokens:     u.PromptTokens,
		CompletionTokens: u.CompletionTokens,
		TotalTokens:      u.TotalTokens,
	}
}
