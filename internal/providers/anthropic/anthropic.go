// Package anthropic adapts the Anthropic Messages API to the providers
// contract. Anthropic does not speak the OpenAI chat wire protocol, so
// this adapter translates on the way out and back again on the way in,
// for both buffered and streamed completions. Everything the gateway
// hands to clients stays in the OpenAI shape.
package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/rajeev-chaurasia/pen-stock/internal/config"
	"github.com/rajeev-chaurasia/pen-stock/internal/providers"
)

const (
	// defaultBaseURL is the public Messages API root, used when a
	// provider entry leaves base_url empty.
	defaultBaseURL = "https://api.anthropic.com/v1"
	messagesPath   = "/messages"

	// headerAPIKey carries the credential. Anthropic does not accept a
	// bearer token on this API.
	headerAPIKey = "x-api-key"

	// headerVersion names the wire contract. Anthropic requires it on
	// every request and rejects the call outright when it is missing,
	// so it is a constant of this adapter rather than a knob.
	headerVersion    = "anthropic-version"
	anthropicVersion = "2023-06-01"

	headerContentType = "Content-Type"
	contentTypeJSON   = "application/json"

	// maxErrorBody caps how much of an upstream failure body is read to
	// build the error message.
	maxErrorBody int64 = 8 << 10

	// maxResponseBytes caps a non-streaming upstream body. Completions
	// run to a few MB at most, so 32 MiB is generous while keeping a
	// hostile upstream from forcing unbounded allocation.
	maxResponseBytes int64 = 32 << 20
)

func init() {
	providers.RegisterKind(config.KindAnthropic, fromConfig)
}

func fromConfig(cfg config.ProviderConfig) (providers.Provider, error) {
	return New(cfg.Name, cfg.BaseURL, cfg.APIKey, nil), nil
}

type provider struct {
	name    string
	baseURL string
	apiKey  string
	client  *http.Client
}

// New returns a Provider for the Anthropic Messages API rooted at
// baseURL, falling back to the public endpoint when baseURL is empty. A
// nil client gets a dedicated pooled default so providers never share
// transport state through a global.
func New(name, baseURL, apiKey string, client *http.Client) providers.Provider {
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	if client == nil {
		client = providers.NewHTTPClient()
	}
	return &provider{
		name:    name,
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		client:  client,
	}
}

func (p *provider) Name() string { return p.name }

func (p *provider) Chat(ctx context.Context, req *providers.ChatRequest) (*providers.ChatResponse, error) {
	resp, err := p.send(ctx, req, false)
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

	translated, usage, err := translateResponse(body, req.Model, time.Now().Unix())
	if err != nil {
		return nil, &providers.ProviderError{
			Provider: p.name,
			Class:    providers.ErrClassUpstream,
			Message:  "upstream response is not a messages completion",
			Err:      err,
		}
	}
	return &providers.ChatResponse{
		Model:    req.Model,
		Provider: p.name,
		Body:     translated,
		Usage:    usage,
	}, nil
}

func (p *provider) ChatStream(ctx context.Context, req *providers.ChatRequest) (providers.StreamReader, error) {
	resp, err := p.send(ctx, req, true)
	if err != nil {
		return nil, err
	}
	if !providers.Is2xx(resp.StatusCode) {
		defer func() { _ = resp.Body.Close() }()
		return nil, p.statusError(resp)
	}
	return newStreamReader(ctx, p.name, req.Model, resp.Body), nil
}

// send translates the client body and posts it. Translation failures are
// the caller's fault, not the upstream's, so they classify as invalid
// requests and never reach the network.
func (p *provider) send(ctx context.Context, req *providers.ChatRequest, stream bool) (*http.Response, error) {
	body, err := translateRequest(req.Raw, req.Model, stream)
	if err != nil {
		return nil, &providers.ProviderError{
			Provider: p.name,
			Class:    providers.ErrClassInvalidRequest,
			Message:  "request cannot be expressed on the messages api",
			Err:      err,
		}
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+messagesPath, bytes.NewReader(body))
	if err != nil {
		return nil, &providers.ProviderError{
			Provider: p.name,
			Class:    providers.ErrClassInternal,
			Message:  "build upstream request",
			Err:      fmt.Errorf("build upstream request: %w", err),
		}
	}
	httpReq.Header.Set(headerContentType, contentTypeJSON)
	httpReq.Header.Set(headerAPIKey, p.apiKey)
	httpReq.Header.Set(headerVersion, anthropicVersion)

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, p.transportError(ctx, "call upstream", err)
	}
	return resp, nil
}

// statusError classifies an upstream failure. Anthropic's 529
// "overloaded" is not a registered status code, but it lands in the 5xx
// bucket that ClassFromStatusAndBody already reads as upstream trouble,
// so it needs no special case here.
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
	return &providers.ProviderError{
		Provider: p.name,
		Class:    transportClass(ctx, err),
		Message:  op + " failed",
		Err:      fmt.Errorf("%s: %w", op, err),
	}
}

// transportClass separates a dead upstream from a deadline the caller
// set and from a cancel the caller asked for. The distinction drives
// retry, so it is shared by the request and stream paths.
func transportClass(ctx context.Context, err error) providers.ErrorClass {
	switch {
	case errors.Is(err, context.DeadlineExceeded) || ctx.Err() == context.DeadlineExceeded:
		return providers.ErrClassTimeout
	case errors.Is(err, context.Canceled) || ctx.Err() == context.Canceled:
		return providers.ErrClassCanceled
	default:
		return providers.ErrClassUpstream
	}
}

// upstreamErrorMessage prefers the message out of Anthropic's error
// envelope, then the raw body, then the bare status. Only the message
// field is lifted: the rest of a failure body can echo request material
// that has no business travelling back to a caller.
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
