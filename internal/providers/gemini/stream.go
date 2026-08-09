package gemini

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

// streamReader turns a streamGenerateContent SSE body into OpenAI shaped
// StreamChunks. Framing is the shared providers.SSEScanner, which holds
// at most one event in memory; the response is never buffered whole.
//
// COMPLETENESS. The OpenAI wire ends a stream with a "data: [DONE]"
// sentinel, so an adapter there can treat a missing sentinel as proof of
// truncation. Gemini sends no sentinel: the stream simply ends when the
// body ends, which is byte for byte what a severed connection, a killed
// upstream, or a proxy timeout also looks like. "The body ended" is
// therefore worthless as a completeness signal on its own.
//
// The one thing Gemini does promise is that a finished turn carries a
// non-empty finishReason on its candidate. That is the signal used here:
// sawFinish latches when any candidate reports one, and the body ending
// resolves to io.EOF only if it latched. A body that ends before any
// finishReason yields ErrStreamTruncated, so the gateway can never relay
// a half-generated answer as a whole one. Reading continues past the
// finishReason to the end of the body, because Gemini may still send a
// trailing usageMetadata event after the turn is done.
type streamReader struct {
	ctx      context.Context
	provider string
	body     io.ReadCloser
	sse      *providers.SSEScanner

	// id, created and model make the emitted chunks look like one
	// coherent OpenAI stream. Gemini identifies neither the completion
	// nor the chunks.
	id      string
	created int64
	model   string

	closeOnce sync.Once
	closed    atomic.Bool
	closeErr  error

	// These are touched only by the Recv caller. sawFinish records the
	// completeness signal described above; termErr is repeated to every
	// Recv after the first terminal one.
	done      bool
	sawFinish bool
	sentRole  bool
	termErr   error
}

func newStreamReader(ctx context.Context, provider, model string, body io.ReadCloser) *streamReader {
	return &streamReader{
		ctx:      ctx,
		provider: provider,
		body:     body,
		sse:      providers.NewSSEScanner(body),
		id:       newCompletionID(),
		created:  time.Now().Unix(),
		model:    model,
	}
}

// finish latches the terminal error so repeated Recv calls agree on how
// the stream ended.
func (r *streamReader) finish(err error) {
	r.done = true
	r.termErr = err
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

	for {
		ev, err := r.sse.Next()
		if err != nil {
			userClosed := r.closed.Load()
			_ = r.Close()
			r.finish(r.recvError(err, userClosed))
			return providers.StreamChunk{}, r.termErr
		}
		if ev.Keepalive {
			// Report liveness so the caller can hold its idle budget open
			// during a long time to first token.
			return providers.StreamChunk{Keepalive: true}, nil
		}
		if !ev.HasData {
			// Gemini carries everything in the data field, so an event
			// named but never filled in is nothing to forward.
			continue
		}

		chunk, err := r.translateEvent(ev.Data)
		if err != nil {
			_ = r.Close()
			r.finish(err)
			return providers.StreamChunk{}, r.termErr
		}
		return chunk, nil
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

// translateEvent turns one Gemini event into an OpenAI chat.completion.chunk.
func (r *streamReader) translateEvent(data string) (providers.StreamChunk, error) {
	var in geminiResponse
	if err := json.Unmarshal([]byte(data), &in); err != nil {
		return providers.StreamChunk{}, &providers.ProviderError{
			Provider: r.provider,
			Class:    providers.ErrClassUpstream,
			Message:  "upstream sent a stream event that is not JSON",
			Err:      fmt.Errorf("decode stream event: %w", err),
		}
	}
	if in.Error != nil {
		// Gemini reports a mid-stream failure as an ordinary SSE event
		// after a 200, so the only place to notice it is here.
		return providers.StreamChunk{}, &providers.ProviderError{
			Provider:   r.provider,
			Class:      classify(in.Error.Code, nil, in.Error),
			StatusCode: in.Error.Code,
			Message:    in.Error.Message,
		}
	}

	delta := &message{}
	if !r.sentRole {
		// OpenAI announces the speaker once, on the first chunk.
		delta.Role = openAIRoleAssistant
		r.sentRole = true
	}
	var finish *string
	if len(in.Candidates) > 0 {
		delta.Content = candidateText(&in.Candidates[0])
		if mapped := mapFinishReason(in.Candidates[0].FinishReason); mapped != "" {
			r.sawFinish = true
			finish = &mapped
		}
	}

	out := chatCompletion{
		ID:      r.id,
		Object:  objectChunk,
		Created: r.created,
		Model:   responseModel(in.ModelVersion, r.model),
		Choices: []choice{{Index: 0, Delta: delta, FinishReason: finish}},
	}

	chunk := providers.StreamChunk{}
	if in.UsageMetadata != nil {
		usage := in.UsageMetadata.toUsage()
		chunk.Usage = &usage
		out.Usage = usageToJSON(usage)
	}

	encoded, err := json.Marshal(out)
	if err != nil {
		return providers.StreamChunk{}, &providers.ProviderError{
			Provider: r.provider,
			Class:    providers.ErrClassInternal,
			Message:  "encode stream chunk",
			Err:      fmt.Errorf("encode stream chunk: %w", err),
		}
	}
	chunk.Data = encoded
	return chunk, nil
}

func (r *streamReader) recvError(err error, userClosed bool) error {
	switch {
	case errors.Is(err, io.EOF):
		if r.sawFinish {
			return io.EOF
		}
		// The body ended before any candidate reported a finishReason.
		// Gemini has no [DONE], so this is the only way to tell a
		// finished turn from a severed one, and calling it io.EOF would
		// let the caller present a partial answer as a finished one.
		return providers.ErrStreamTruncated
	case errors.Is(err, providers.ErrSSEEventTooLarge):
		return &providers.ProviderError{
			Provider: r.provider,
			Class:    providers.ErrClassUpstream,
			Message:  fmt.Sprintf("upstream SSE event exceeds %d byte limit", providers.MaxSSEEventBytes),
			Err:      err,
		}
	case errors.Is(err, context.DeadlineExceeded) || r.ctx.Err() == context.DeadlineExceeded:
		return &providers.ProviderError{
			Provider: r.provider,
			Class:    providers.ErrClassTimeout,
			Message:  "stream read timed out",
			Err:      err,
		}
	case errors.Is(err, context.Canceled) || r.ctx.Err() == context.Canceled:
		return &providers.ProviderError{
			Provider: r.provider,
			Class:    providers.ErrClassCanceled,
			Message:  "stream canceled",
			Err:      err,
		}
	case userClosed:
		// The read failed because Close tore down the body on purpose.
		return providers.ErrStreamTruncated
	default:
		return &providers.ProviderError{
			Provider: r.provider,
			Class:    providers.ErrClassUpstream,
			Message:  "stream read failed",
			Err:      err,
		}
	}
}
