package openaiwire

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/rajeev-chaurasia/pen-stock/internal/providers"
)

const (
	sseDataPrefix   = "data:"
	sseDoneSentinel = "[DONE]"
	sseCommentByte  = ':'

	// maxEventBytes bounds how much of a single SSE event is held in
	// memory. A well-formed chat chunk is a few KB, so 1 MiB leaves
	// ample headroom while keeping a hostile upstream from forcing
	// unbounded allocation through a never-terminated line.
	maxEventBytes int = 1 << 20
)

// errEventTooLarge marks an SSE event that exceeded maxEventBytes.
var errEventTooLarge = errors.New("sse event exceeds size limit")

// streamReader turns an SSE response body into StreamChunks. It holds at
// most maxEventBytes of one event in memory at a time; the response is
// never buffered whole.
type streamReader struct {
	ctx      context.Context
	provider string
	body     io.ReadCloser
	br       *bufio.Reader

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

// event is one parsed SSE event. A keepalive carries no data and exists
// only to prove the upstream is still working.
type event struct {
	data      string
	keepalive bool
}

func newStreamReader(ctx context.Context, provider string, body io.ReadCloser) *streamReader {
	return &streamReader{
		ctx:      ctx,
		provider: provider,
		body:     body,
		br:       bufio.NewReader(body),
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

	ev, err := r.nextEvent()
	if err != nil {
		userClosed := r.closed.Load()
		_ = r.Close()
		r.finish(r.recvError(err, userClosed))
		return providers.StreamChunk{}, r.termErr
	}
	if ev.keepalive {
		// Report liveness so the caller can hold its idle budget open
		// during a long time to first token.
		return providers.StreamChunk{Keepalive: true}, nil
	}
	if ev.data == sseDoneSentinel {
		r.sawDone = true
		_ = r.Close()
		r.finish(io.EOF)
		return providers.StreamChunk{}, io.EOF
	}

	chunk := providers.StreamChunk{Data: []byte(ev.data)}
	var envelope struct {
		Usage *usageJSON `json:"usage"`
	}
	if jsonErr := json.Unmarshal(chunk.Data, &envelope); jsonErr == nil && envelope.Usage != nil {
		usage := envelope.Usage.toUsage()
		chunk.Usage = &usage
	}
	return chunk, nil
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

// nextEvent reads one SSE event and returns its joined data payload.
// Non-data fields are skipped; CRLF endings and data split across
// multiple lines are handled per the SSE spec. Comment lines surface as
// keepalives rather than being swallowed, because some backends send
// nothing else during a long time to first token. An event larger than
// maxEventBytes aborts with errEventTooLarge.
func (r *streamReader) nextEvent() (event, error) {
	var data []string
	haveData := false
	remaining := maxEventBytes
	for {
		line, err := r.readLine(&remaining)
		if len(line) > 0 {
			trimmed := strings.TrimRight(line, "\r\n")
			switch {
			case trimmed == "":
				if haveData {
					return event{data: strings.Join(data, "\n")}, nil
				}
				// blank line outside an event; the next event starts
				// with a fresh budget
				remaining = maxEventBytes
			case trimmed[0] == sseCommentByte:
				if !haveData {
					return event{keepalive: true}, nil
				}
			case strings.HasPrefix(trimmed, sseDataPrefix):
				value := strings.TrimPrefix(trimmed, sseDataPrefix)
				value = strings.TrimPrefix(value, " ")
				data = append(data, value)
				haveData = true
			default:
				// event:, id:, retry: and unknown fields carry nothing we need
			}
		}
		if err != nil {
			// Pending event data at EOF is discarded, per the SSE spec.
			// Delivering a half-arrived event would hand the caller torn
			// JSON that looks like a complete chunk.
			return event{}, err
		}
	}
}

// readLine returns one line including its terminator, charging its size
// against the per-event budget. ReadSlice keeps the buffered reader from
// accumulating a never-terminated line the way ReadString would; pieces
// are reassembled here under the budget.
func (r *streamReader) readLine(remaining *int) (string, error) {
	var line []byte
	for {
		chunk, err := r.br.ReadSlice('\n')
		*remaining -= len(chunk)
		if *remaining < 0 {
			return "", errEventTooLarge
		}
		line = append(line, chunk...)
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return string(line), err
	}
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
	case errors.Is(err, errEventTooLarge):
		return &providers.ProviderError{
			Provider: r.provider,
			Class:    providers.ErrClassUpstream,
			Message:  fmt.Sprintf("upstream SSE event exceeds %d byte limit", maxEventBytes),
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
		// the read failed because Close tore down the body on purpose
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
