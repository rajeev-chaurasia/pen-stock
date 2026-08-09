package cache

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

	"github.com/rajeev-chaurasia/pen-stock/internal/providers"
)

const (
	// DefaultEmbedBaseURL is the public Gemini endpoint, the same root
	// the chat adapter uses, so an operator already holding a Gemini key
	// needs no second credential to turn this tier on.
	DefaultEmbedBaseURL = "https://generativelanguage.googleapis.com/v1beta"

	// DefaultEmbedModel is the embedding model used when none is given.
	// Verified live against the API: the older text-embedding-004 now
	// answers 404 for newly issued keys, the same way older generation
	// chat models do.
	DefaultEmbedModel = "gemini-embedding-001"

	// DefaultEmbedDimensions is the width of a gemini-embedding-001
	// vector, confirmed by counting a live response. At four bytes each
	// that is 12KB per cached vector, which is why the semantic store is
	// bounded per tenant rather than left to grow.
	DefaultEmbedDimensions = 3072

	// legacyEmbedDimensions is the width of the older 768 wide models,
	// kept so a deployment pinned to one still reports honestly.
	legacyEmbedDimensions = 768

	// embedAuthHeader is the header Gemini reads the API key from. The
	// API also accepts ?key=, which is refused here: query strings are
	// copied into access logs, proxy records, and span attributes, and a
	// key that reaches any of those is a leaked key.
	embedAuthHeader = "x-goog-api-key"

	embedModelPrefix = "models/"
	embedMethod      = ":batchEmbedContents"

	// maxEmbedErrorBody caps how much of a failure body is read to build
	// an error message.
	maxEmbedErrorBody = 8 << 10

	// maxEmbedResponseBytes caps a success body. A batch of 768 float
	// vectors runs to a few MB, so this is generous while keeping a
	// misbehaving upstream from forcing unbounded allocation.
	maxEmbedResponseBytes int64 = 32 << 20

	// redactedKey stands in for the API key in any string that might be
	// logged.
	redactedKey = "[redacted]"
)

// ErrEmbedFailed is the root of every failure this embedder reports.
//
// It is an ordinary error on purpose. An embedder that cannot be reached
// means the semantic tier cannot answer this request, which is a cache
// miss and nothing more: the exact tier and the provider call behind it
// are both still available. A gateway that failed requests because its
// optional cache was down would be worse than a gateway with no cache.
var ErrEmbedFailed = errors.New("embed failed")

// embedModelDimensions records the widths this package is sure of. A
// model that is not listed reports an unknown width rather than a
// guessed one, since a wrong width is worse than an absent one: the
// semantic store learns its width from the first vector it accepts, and
// only needs this to be right when it is stated at all.
var embedModelDimensions = map[string]int{
	"gemini-embedding-001": DefaultEmbedDimensions,
	"text-embedding-004":   legacyEmbedDimensions,
	"embedding-001":        legacyEmbedDimensions,
}

type geminiEmbedder struct {
	baseURL string
	apiKey  string
	// model is the bare name for the URL path, qualified is the
	// models/-prefixed form the request body wants. Gemini asks for both
	// spellings of the same fact in the same call.
	model     string
	qualified string
	client    *http.Client
}

// NewGeminiEmbedder returns an Embedder backed by Gemini's
// batchEmbedContents endpoint. Empty baseURL or model take the package
// defaults, and a nil client gets its own pooled transport rather than
// sharing global state.
func NewGeminiEmbedder(baseURL, apiKey, model string, client *http.Client) Embedder {
	if baseURL == "" {
		baseURL = DefaultEmbedBaseURL
	}
	if model == "" {
		model = DefaultEmbedModel
	}
	if client == nil {
		client = providers.NewHTTPClient()
	}
	bare := strings.TrimPrefix(model, embedModelPrefix)
	return &geminiEmbedder{
		baseURL:   strings.TrimRight(baseURL, "/"),
		apiKey:    apiKey,
		model:     bare,
		qualified: embedModelPrefix + bare,
		client:    client,
	}
}

// Dimensions reports the model's vector width, or zero when this package
// does not know it.
func (g *geminiEmbedder) Dimensions() int { return embedModelDimensions[g.model] }

// embedRequest is the batchEmbedContents body. Each nested request
// repeats the model even though the URL already names it, which is the
// API's shape rather than a mistake.
type embedRequest struct {
	Requests []embedItem `json:"requests"`
}

type embedItem struct {
	Model string `json:"model"`
	// No taskType is sent. The stored vector and the query vector have to
	// live in the same space to be comparable, and asking for
	// RETRIEVAL_DOCUMENT on one side and RETRIEVAL_QUERY on the other
	// would quietly compare projections of different spaces.
	Content embedContent `json:"content"`
}

type embedContent struct {
	Parts []embedPart `json:"parts"`
}

type embedPart struct {
	Text string `json:"text"`
}

type embedResponse struct {
	Embeddings []struct {
		Values []float32 `json:"values"`
	} `json:"embeddings"`
}

// Embed returns one vector per input, in input order.
func (g *geminiEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	// Nothing to embed is not a reason to talk to anyone.
	if len(texts) == 0 {
		return nil, nil
	}

	payload := embedRequest{Requests: make([]embedItem, len(texts))}
	for i, text := range texts {
		payload.Requests[i] = embedItem{
			Model:   g.qualified,
			Content: embedContent{Parts: []embedPart{{Text: text}}},
		}
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("%w: encode request: %w", ErrEmbedFailed, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.endpoint(), bytes.NewReader(encoded))
	if err != nil {
		return nil, fmt.Errorf("%w: build request: %w", ErrEmbedFailed, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(embedAuthHeader, g.apiKey)

	resp, err := g.client.Do(req)
	if err != nil {
		// A transport error quotes the URL, which carries no key because
		// the key travels in a header. The cause is kept in the chain so
		// a caller can still tell a cancelled context from a dead
		// upstream.
		return nil, fmt.Errorf("%w: call upstream: %w", ErrEmbedFailed, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, g.statusError(resp)
	}
	return g.decode(resp.Body, len(texts))
}

// decode reads the response and returns vectors only if the whole batch
// is present and usable. A short or unreadable batch is an error rather
// than a partial result: a caller handed fewer vectors than inputs has
// no way to tell which input each one belongs to, and pairing them by
// position would attach an answer to the wrong question.
func (g *geminiEmbedder) decode(body io.Reader, want int) ([][]float32, error) {
	// One byte past the cap, since a LimitReader at its limit is
	// indistinguishable from a clean EOF.
	raw, err := io.ReadAll(io.LimitReader(body, maxEmbedResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%w: read response: %w", ErrEmbedFailed, err)
	}
	if int64(len(raw)) > maxEmbedResponseBytes {
		return nil, fmt.Errorf("%w: response exceeds %d byte limit", ErrEmbedFailed, maxEmbedResponseBytes)
	}

	var decoded embedResponse
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, fmt.Errorf("%w: response is not a batchEmbedContents body: %w", ErrEmbedFailed, err)
	}
	if len(decoded.Embeddings) != want {
		return nil, fmt.Errorf("%w: upstream returned %d embeddings for %d inputs", ErrEmbedFailed, len(decoded.Embeddings), want)
	}

	vectors := make([][]float32, want)
	for i, embedding := range decoded.Embeddings {
		if len(embedding.Values) == 0 {
			return nil, fmt.Errorf("%w: upstream returned an empty vector at index %d", ErrEmbedFailed, i)
		}
		vectors[i] = embedding.Values
	}
	return vectors, nil
}

// endpoint builds {baseURL}/models/{model}:batchEmbedContents. The model
// is escaped as a path segment; the colon stays literal because it is
// method syntax rather than part of the resource name.
func (g *geminiEmbedder) endpoint() string {
	return g.baseURL + "/" + embedModelPrefix + url.PathEscape(g.model) + embedMethod
}

func (g *geminiEmbedder) statusError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxEmbedErrorBody))
	detail := g.redact(strings.TrimSpace(string(body)))
	if detail == "" {
		return fmt.Errorf("%w: upstream returned status %d", ErrEmbedFailed, resp.StatusCode)
	}
	return fmt.Errorf("%w: upstream returned status %d: %s", ErrEmbedFailed, resp.StatusCode, detail)
}

// redact strips the API key out of anything on its way into an error.
// Google's own errors do not echo the key back, but the text of an
// upstream failure is not something this package controls, and an error
// string ends up in logs, spans, and pasted bug reports.
func (g *geminiEmbedder) redact(s string) string {
	if g.apiKey == "" {
		return s
	}
	return strings.ReplaceAll(s, g.apiKey, redactedKey)
}
