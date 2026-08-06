package providers

import (
	"context"
	"encoding/json"
)

// Usage is the token accounting reported by a provider for one completion.
type Usage struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
}

// ChatRequest carries a parsed envelope plus the raw client body.
// Raw is forwarded to wire-compatible providers untouched so the gateway
// never drops fields it does not model.
type ChatRequest struct {
	Model  string
	Stream bool
	Raw    json.RawMessage
}

// ChatResponse is a normalized non-streaming completion.
type ChatResponse struct {
	Model    string
	Provider string
	Body     json.RawMessage
	Usage    Usage
}

// StreamChunk is one SSE event payload from a streaming completion.
// Usage is non-nil only on the chunk where the provider reports totals.
type StreamChunk struct {
	Data  []byte
	Usage *Usage
}

// StreamReader yields chunks until io.EOF. Close is safe to call twice
// and must release the underlying connection.
type StreamReader interface {
	Recv() (StreamChunk, error)
	Close() error
}

// Provider is the contract every backend adapter implements.
type Provider interface {
	Name() string
	Chat(ctx context.Context, req *ChatRequest) (*ChatResponse, error)
	ChatStream(ctx context.Context, req *ChatRequest) (StreamReader, error)
}
