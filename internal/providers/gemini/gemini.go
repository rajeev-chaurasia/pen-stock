// Package gemini adapts Google's Gemini generateContent API to the
// providers contract. Gemini does not speak the OpenAI chat wire
// protocol, so unlike the pass-through adapters this one translates in
// both directions: OpenAI shaped requests in, OpenAI shaped responses
// out, with the Gemini dialect confined to this package.
package gemini

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/rajeev-chaurasia/pen-stock/internal/config"
	"github.com/rajeev-chaurasia/pen-stock/internal/providers"
)

const (
	// DefaultBaseURL is the public Gemini endpoint, used when config
	// leaves base_url empty.
	DefaultBaseURL = "https://generativelanguage.googleapis.com/v1beta"

	// authHeader is the header name Gemini reads the API key from. Gemini
	// also accepts a ?key= query parameter, which is refused here: query
	// strings land in access logs, proxy traces, and span attributes.
	authHeader = "x-goog-api-key"

	modelsPath            = "/models/"
	generateContent       = ":generateContent"
	streamGenerateContent = ":streamGenerateContent"
	// sseQuery asks for server-sent events; without it
	// streamGenerateContent returns a JSON array instead.
	sseQuery = "alt=sse"

	// maxErrorBody caps how much of an upstream failure body is read to
	// build the error message.
	maxErrorBody = 8 << 10

	// maxResponseBytes caps a non-streaming upstream body. Chat
	// completions run to a few MB at most, so 32 MiB is generous while
	// keeping a hostile upstream from forcing unbounded allocation.
	maxResponseBytes int64 = 32 << 20

	defaultMaxIdleConnsPerHost = 32
)

// Gemini reports failures with a google.rpc.Status code. HTTP status
// alone is usually enough, but a proxy that rewrites the status leaves
// this string as the only honest signal.
const (
	statusResourceExhausted = "RESOURCE_EXHAUSTED"
	statusUnauthenticated   = "UNAUTHENTICATED"
	statusPermissionDenied  = "PERMISSION_DENIED"
)

func init() {
	providers.RegisterKind(config.KindGemini, fromConfig)
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

// New returns a Provider for a Gemini endpoint rooted at baseURL, which
// defaults to the public API when empty. A nil client gets a dedicated
// pooled default so providers never share transport state through a
// global.
func New(name, baseURL, apiKey string, client *http.Client) providers.Provider {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	if client == nil {
		client = defaultClient()
	}
	return &provider{
		name:    name,
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		client:  client,
	}
}

// defaultClient keeps the stdlib transport defaults (proxy, TLS, HTTP/2)
// and widens the per-host idle pool for gateway-style fan-in. No client
// Timeout on purpose: deadlines arrive via ctx and a client timeout
// would kill long streams.
func defaultClient() *http.Client {
	transport := &http.Transport{}
	if t, ok := http.DefaultTransport.(*http.Transport); ok {
		transport = t.Clone()
	}
	transport.MaxIdleConnsPerHost = defaultMaxIdleConnsPerHost
	return &http.Client{Transport: transport}
}

func (p *provider) Name() string { return p.name }

func (p *provider) Chat(ctx context.Context, req *providers.ChatRequest) (*providers.ChatResponse, error) {
	resp, err := p.post(ctx, req, generateContent, "")
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if !is2xx(resp.StatusCode) {
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

	translated, usage, err := translateCompletion(body, newCompletionID(), req.Model)
	if err != nil {
		// The body is not forwarded verbatim here, so a shape we cannot
		// read is a hard failure rather than something to shrug off.
		return nil, &providers.ProviderError{
			Provider:   p.name,
			Class:      providers.ErrClassUpstream,
			StatusCode: resp.StatusCode,
			Message:    "upstream response is not a Gemini completion",
			Err:        err,
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
	resp, err := p.post(ctx, req, streamGenerateContent, sseQuery)
	if err != nil {
		return nil, err
	}
	if !is2xx(resp.StatusCode) {
		defer func() { _ = resp.Body.Close() }()
		return nil, p.statusError(resp)
	}
	return newStreamReader(ctx, p.name, req.Model, resp.Body), nil
}

func (p *provider) post(ctx context.Context, req *providers.ChatRequest, method, query string) (*http.Response, error) {
	payload, err := translateRequest(req.Raw)
	if err != nil {
		return nil, &providers.ProviderError{
			Provider: p.name,
			Class:    providers.ErrClassInvalidRequest,
			Message:  "request is not a valid chat completions body",
			Err:      err,
		}
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, p.internalError("encode upstream request", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint(req.Model, method, query), bytes.NewReader(encoded))
	if err != nil {
		return nil, p.internalError("build upstream request", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set(authHeader, p.apiKey)

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, p.transportError(ctx, "call upstream", err)
	}
	return resp, nil
}

// endpoint builds {baseURL}/models/{model}:{method}. The model is escaped
// as a path segment; the colon stays literal because it is part of the
// method syntax rather than the resource name.
func (p *provider) endpoint(model, method, query string) string {
	u := p.baseURL + modelsPath + url.PathEscape(strings.TrimPrefix(model, modelNamePrefix)) + method
	if query != "" {
		u += "?" + query
	}
	return u
}

func (p *provider) statusError(resp *http.Response) *providers.ProviderError {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	envelope := parseErrorEnvelope(body)
	return &providers.ProviderError{
		Provider:   p.name,
		Class:      classify(resp.StatusCode, body, envelope),
		StatusCode: resp.StatusCode,
		Message:    upstreamErrorMessage(body, envelope, resp.StatusCode),
	}
}

func (p *provider) internalError(op string, err error) *providers.ProviderError {
	return &providers.ProviderError{
		Provider: p.name,
		Class:    providers.ErrClassInternal,
		Message:  op,
		Err:      fmt.Errorf("%s: %w", op, err),
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

// transportClass separates a caller-driven end (deadline, cancel) from a
// genuine upstream fault, since the gateway retries only the latter.
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

func parseErrorEnvelope(body []byte) *geminiError {
	var envelope struct {
		Error *geminiError `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil
	}
	return envelope.Error
}

// classify buckets an upstream failure. The google.rpc.Status string wins
// when Gemini sends one, then ClassFromStatusAndBody decides, which also
// keeps a bare 404 from a mistyped base_url out of the model_not_found
// bucket.
func classify(status int, body []byte, envelope *geminiError) providers.ErrorClass {
	if envelope != nil {
		switch envelope.Status {
		case statusResourceExhausted:
			return providers.ErrClassRateLimited
		case statusUnauthenticated, statusPermissionDenied:
			return providers.ErrClassAuth
		}
	}
	return providers.ClassFromStatusAndBody(status, body)
}

// upstreamErrorMessage prefers the Gemini error message, then raw body
// text, then the bare status code. Only error.message is taken: the
// error.details array carries upstream internals that have no business
// reaching a caller.
func upstreamErrorMessage(body []byte, envelope *geminiError, status int) string {
	if envelope != nil && envelope.Message != "" {
		return envelope.Message
	}
	if s := strings.TrimSpace(string(body)); s != "" {
		return s
	}
	return fmt.Sprintf("upstream returned status %d", status)
}

func is2xx(code int) bool { return code >= 200 && code < 300 }
