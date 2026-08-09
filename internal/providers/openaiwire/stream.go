package openaiwire

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"

	"github.com/rajeev-chaurasia/pen-stock/internal/providers"
)

const sseDoneSentinel = "[DONE]"

// streamReader turns an SSE response body into StreamChunks. Framing is
// the shared providers.SSEScanner, which holds at most one event in
// memory; the response is never buffered whole.
type streamReader struct {
	ctx      context.Context
	provider string
	body     io.ReadCloser
	sse      *providers.SSEScanner

	closeOnce sync.Once
	closed    atomic.Bool
	closeErr  error

	// These are touched only by the Recv caller. sawDone records whether
	// the upstream sent its [DONE] sentinel, which is the only evidence
	// that a completion is whole. termErr is repeated to every Recv
	// after the first terminal one.
	done    bool
	sawDone bool
	termErr error
}

// finish latches the terminal error so repeated Recv calls agree on how
// the stream ended.
func (r *streamReader) finish(err error) {
	r.done = true
	r.termErr = err
}

func newStreamReader(ctx context.Context, provider string, body io.ReadCloser) *streamReader {
	return &streamReader{
		ctx:      ctx,
		provider: provider,
		body:     body,
		sse:      providers.NewSSEScanner(body),
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
			// This wire carries everything in the data field, so an event
			// named but never filled in is nothing to forward.
			continue
		}
		if ev.Data == sseDoneSentinel {
			r.sawDone = true
			_ = r.Close()
			r.finish(io.EOF)
			return providers.StreamChunk{}, io.EOF
		}

		chunk := providers.StreamChunk{Data: []byte(ev.Data)}
		var envelope struct {
			Usage *usageJSON `json:"usage"`
		}
		if jsonErr := json.Unmarshal(chunk.Data, &envelope); jsonErr == nil && envelope.Usage != nil {
			usage := envelope.Usage.toUsage()
			chunk.Usage = &usage
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

func (r *streamReader) recvError(err error, userClosed bool) error {
	switch {
	case errors.Is(err, io.EOF):
		if r.sawDone {
			return io.EOF
		}
		// The body ended without [DONE], which on the OpenAI wire is the
		// only completeness signal. Reporting io.EOF here would let the
		// caller present a partial answer as a finished one.
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
