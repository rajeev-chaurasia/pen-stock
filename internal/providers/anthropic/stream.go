package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rajeev-chaurasia/pen-stock/internal/providers"
)

const (
	// Anthropic's stream is event typed: the name on the event line,
	// not the shape of the payload, says what arrived.
	eventMessageStart      = "message_start"
	eventMessageDelta      = "message_delta"
	eventMessageStop       = "message_stop"
	eventContentBlockDelta = "content_block_delta"
	eventPing              = "ping"

	deltaTypeText = "text_delta"
)

// streamPayload is every event body this adapter reads, in one struct:
// the delta member is shared by content_block_delta (text) and
// message_delta (stop reason), which never collide on a single event.
type streamPayload struct {
	Type    string `json:"type"`
	Message *struct {
		ID    string     `json:"id"`
		Model string     `json:"model"`
		Usage *usageJSON `json:"usage"`
	} `json:"message"`
	Delta *struct {
		Type       string `json:"type"`
		Text       string `json:"text"`
		StopReason string `json:"stop_reason"`
	} `json:"delta"`
	Usage *usageJSON `json:"usage"`
}

type completionChunk struct {
	ID      string        `json:"id"`
	Object  string        `json:"object"`
	Created int64         `json:"created"`
	Model   string        `json:"model"`
	Choices []chunkChoice `json:"choices"`
	Usage   *openAIUsage  `json:"usage,omitempty"`
}

type chunkChoice struct {
	Index int        `json:"index"`
	Delta chunkDelta `json:"delta"`
	// FinishReason is null until the turn ends, the way OpenAI streams
	// it, so a pointer rather than an empty string.
	FinishReason *string `json:"finish_reason"`
}

type chunkDelta struct {
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
}

// streamReader turns an event-typed Anthropic SSE body into OpenAI
// chunks. Framing is the shared providers.SSEScanner, which holds at
// most one event in memory; the response is never buffered whole.
type streamReader struct {
	ctx      context.Context
	provider string
	body     io.ReadCloser
	sse      *providers.SSEScanner
	created  int64

	closeOnce sync.Once
	closed    atomic.Bool
	closeErr  error

	// Everything below is touched only by the Recv caller.
	done    bool
	termErr error

	// sawStop records the message_stop event. Anthropic sends no [DONE]
	// sentinel, so this is the only evidence a completion is whole.
	sawStop bool

	id    string
	model string

	// Token counts arrive in two halves: input with message_start and
	// output with message_delta. Usage is reported once both are known.
	inputTokens  int
	outputTokens int
	haveInput    bool
	haveOutput   bool
}

func newStreamReader(ctx context.Context, provider, model string, body io.ReadCloser) *streamReader {
	return &streamReader{
		ctx:      ctx,
		provider: provider,
		model:    model,
		body:     body,
		sse:      providers.NewSSEScanner(body),
		created:  time.Now().Unix(),
	}
}

func (r *streamReader) Recv() (providers.StreamChunk, error) {
	if r.done {
		return providers.StreamChunk{}, r.termErr
	}
	if r.closed.Load() {
		// Closed underneath us, so whatever arrived is all there is.
		r.finish(providers.ErrStreamTruncated)
		return providers.StreamChunk{}, r.termErr
	}

	// Several Anthropic events carry no OpenAI chunk, so one Recv may
	// have to consume more than one of them.
	for {
		ev, err := r.sse.Next()
		if err != nil {
			userClosed := r.closed.Load()
			_ = r.Close()
			r.finish(r.recvError(err, userClosed))
			return providers.StreamChunk{}, r.termErr
		}
		if ev.Keepalive {
			return providers.StreamChunk{Keepalive: true}, nil
		}

		chunk, err := r.translate(ev)
		if err != nil {
			_ = r.Close()
			r.finish(err)
			return providers.StreamChunk{}, r.termErr
		}
		if chunk != nil {
			return *chunk, nil
		}
		if r.sawStop {
			_ = r.Close()
			r.finish(io.EOF)
			return providers.StreamChunk{}, io.EOF
		}
	}
}

// Close releases the response body. Safe to call twice and safe to call
// concurrently with a blocked Recv, which it unblocks.
func (r *streamReader) Close() error {
	r.closeOnce.Do(func() {
		r.closed.Store(true)
		r.closeErr = r.body.Close()
	})
	return r.closeErr
}

// finish latches the terminal error so repeated Recv calls agree on how
// the stream ended.
func (r *streamReader) finish(err error) {
	r.done = true
	r.termErr = err
}

// translate turns one Anthropic event into at most one OpenAI chunk. A
// nil chunk means the event carried only structure
// (content_block_start, content_block_stop) or state this reader
// accumulates rather than forwards.
func (r *streamReader) translate(ev providers.SSEEvent) (*providers.StreamChunk, error) {
	var payload streamPayload
	if ev.Data != "" {
		if err := json.Unmarshal([]byte(ev.Data), &payload); err != nil {
			return nil, r.protocolError("decode stream event", err)
		}
	}
	name := ev.Name
	if name == "" {
		// Some proxies strip the event line. The payload names itself
		// too, so the stream is still readable without it.
		name = payload.Type
	}

	switch name {
	case eventPing:
		return &providers.StreamChunk{Keepalive: true}, nil

	case eventMessageStart:
		if m := payload.Message; m != nil {
			r.id = m.ID
			if m.Model != "" {
				r.model = m.Model
			}
			if m.Usage != nil {
				r.inputTokens = m.Usage.InputTokens
				r.haveInput = true
			}
		}
		// OpenAI clients expect the assistant role on the first chunk.
		return r.chunk(chunkDelta{Role: roleAssistant}, nil, nil)

	case eventContentBlockDelta:
		if payload.Delta == nil || payload.Delta.Type != deltaTypeText {
			// Tool input arrives as partial JSON, which has no place in
			// a text delta.
			return nil, nil
		}
		return r.chunk(chunkDelta{Content: payload.Delta.Text}, nil, nil)

	case eventMessageDelta:
		if payload.Usage != nil {
			r.outputTokens = payload.Usage.OutputTokens
			r.haveOutput = true
		}
		var finish *string
		if payload.Delta != nil {
			if reason := finishReason(payload.Delta.StopReason); reason != "" {
				finish = &reason
			}
		}
		return r.chunk(chunkDelta{}, finish, r.usage())

	case eventMessageStop:
		r.sawStop = true
		return nil, nil

	default:
		return nil, nil
	}
}

// usage reports totals only once both halves have arrived, so the
// gateway records usage once, at the end of the stream, the way it does
// for providers that report it in a single event.
func (r *streamReader) usage() *providers.Usage {
	if !r.haveInput || !r.haveOutput {
		return nil
	}
	return &providers.Usage{
		PromptTokens:     r.inputTokens,
		CompletionTokens: r.outputTokens,
		TotalTokens:      r.inputTokens + r.outputTokens,
	}
}

func (r *streamReader) chunk(delta chunkDelta, finish *string, usage *providers.Usage) (*providers.StreamChunk, error) {
	out := completionChunk{
		ID:      r.id,
		Object:  objectChunk,
		Created: r.created,
		Model:   r.model,
		Choices: []chunkChoice{{Delta: delta, FinishReason: finish}},
	}
	if usage != nil {
		reported := toOpenAIUsage(*usage)
		out.Usage = &reported
	}
	data, err := json.Marshal(out)
	if err != nil {
		return nil, r.protocolError("encode completion chunk", err)
	}
	return &providers.StreamChunk{Data: data, Usage: usage}, nil
}

// recvError decides how the stream ended.
//
// TRUNCATION RULE: Anthropic sends no [DONE] sentinel. The message_stop
// event is the only completeness signal on this wire, so a body that
// simply ends is truncated unless message_stop already arrived. Every
// early end, a severed connection, a crashed backend behind a proxy, or
// a local Close, reports ErrStreamTruncated. Returning io.EOF in those
// cases would let the gateway serve half an answer as a whole one, and
// bill it as one too.
func (r *streamReader) recvError(err error, userClosed bool) error {
	if errors.Is(err, io.EOF) {
		if r.sawStop {
			return io.EOF
		}
		return providers.ErrStreamTruncated
	}
	if errors.Is(err, providers.ErrSSEEventTooLarge) {
		return &providers.ProviderError{
			Provider: r.provider,
			Class:    providers.ErrClassUpstream,
			Message:  fmt.Sprintf("upstream SSE event exceeds %d byte limit", providers.MaxSSEEventBytes),
			Err:      err,
		}
	}
	// A canceled or expired context explains a failed read better than a
	// local Close does, so it is reported ahead of it.
	class := transportClass(r.ctx, err)
	if class == providers.ErrClassUpstream && userClosed {
		return providers.ErrStreamTruncated
	}
	return &providers.ProviderError{
		Provider: r.provider,
		Class:    class,
		Message:  streamErrorMessage(class),
		Err:      err,
	}
}

func streamErrorMessage(class providers.ErrorClass) string {
	switch class {
	case providers.ErrClassTimeout:
		return "stream read timed out"
	case providers.ErrClassCanceled:
		return "stream canceled"
	default:
		return "stream read failed"
	}
}

func (r *streamReader) protocolError(op string, err error) error {
	return &providers.ProviderError{
		Provider: r.provider,
		Class:    providers.ErrClassUpstream,
		Message:  op + " failed",
		Err:      fmt.Errorf("%s: %w", op, err),
	}
}
