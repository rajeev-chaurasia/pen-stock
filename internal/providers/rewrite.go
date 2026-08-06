package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
)

// modelField is the request field naming the model on the OpenAI wire.
const modelField = "model"

// WithModel returns p wrapped so every request asks for upstreamModel
// instead of whatever the client named.
//
// This is what lets one routed name sit in front of providers that do
// not share a vocabulary: a chain over free tiers might mean
// llama-3.3-70b-versatile at one, mistral-small-latest at the next, and
// a vendor prefixed id at a third. Without the rewrite the second
// provider in any such chain would be asked for a model it has never
// heard of and would answer 404 forever.
//
// An empty upstreamModel returns p unchanged, so routes whose providers
// already agree on a name pay nothing.
func WithModel(p Provider, upstreamModel string) Provider {
	if upstreamModel == "" {
		return p
	}
	return &modelRewriter{inner: p, model: upstreamModel}
}

type modelRewriter struct {
	inner Provider
	model string
}

func (m *modelRewriter) Name() string { return m.inner.Name() }

func (m *modelRewriter) Chat(ctx context.Context, req *ChatRequest) (*ChatResponse, error) {
	rewritten, err := m.rewrite(req)
	if err != nil {
		return nil, err
	}
	return m.inner.Chat(ctx, rewritten)
}

func (m *modelRewriter) ChatStream(ctx context.Context, req *ChatRequest) (StreamReader, error) {
	rewritten, err := m.rewrite(req)
	if err != nil {
		return nil, err
	}
	return m.inner.ChatStream(ctx, rewritten)
}

// rewrite copies the request with the upstream's model name in both the
// envelope and the raw body. The caller's request is never mutated,
// because a fallback chain hands the same request to several providers
// and each needs its own name.
func (m *modelRewriter) rewrite(req *ChatRequest) (*ChatRequest, error) {
	out := *req
	out.Model = m.model

	raw, err := setModel(req.Raw, m.model)
	if err != nil {
		return nil, &ProviderError{
			Provider: m.inner.Name(),
			Class:    ErrClassInvalidRequest,
			Message:  "request body could not be retargeted at this provider",
			Err:      err,
		}
	}
	out.Raw = raw
	return &out, nil
}

// setModel replaces the model field while leaving every other field as
// the client sent it, including ones the gateway does not model.
func setModel(raw json.RawMessage, model string) (json.RawMessage, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return raw, nil
	}
	var body map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	// Numbers keep their original notation so a large id or an exponent
	// does not come back rewritten through float64.
	dec.UseNumber()
	if err := dec.Decode(&body); err != nil {
		return nil, fmt.Errorf("decode request body: %w", err)
	}
	body[modelField] = model

	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("encode request body: %w", err)
	}
	return encoded, nil
}
