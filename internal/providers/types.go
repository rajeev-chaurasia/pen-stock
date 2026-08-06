package providers

import (
	"context"
	"encoding/json"
	"errors"
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
// A Keepalive chunk carries no data: it reports that the upstream is
// alive but still working, which some backends signal with SSE comments
// during a long time to first token.
type StreamChunk struct {
	Data      []byte
	Usage     *Usage
	Keepalive bool
}

// ErrStreamTruncated reports an upstream stream that ended without its
// terminating [DONE] sentinel. On the OpenAI wire that sentinel is the
// only completeness signal, so this must never be reported as io.EOF:
// callers would relay a partial answer as a whole one.
var ErrStreamTruncated = errors.New("upstream stream ended without the [DONE] sentinel")

// StreamReader yields chunks until a terminal error. io.EOF means the
// upstream sent [DONE] and the completion is whole; ErrStreamTruncated
// means it is not. Close is safe to call twice and must release the
// underlying connection.
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
