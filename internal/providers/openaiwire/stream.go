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

	// done is touched only by the Recv caller, marking a terminal state.
	done bool
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
		return providers.StreamChunk{}, io.EOF
	}
	if r.closed.Load() {
		r.done = true
		return providers.StreamChunk{}, io.EOF
	}

	data, err := r.nextEvent()
	if err != nil {
		r.done = true
		userClosed := r.closed.Load()
		_ = r.Close()
		return providers.StreamChunk{}, r.recvError(err, userClosed)
	}
	if data == sseDoneSentinel {
		r.done = true
		_ = r.Close()
		return providers.StreamChunk{}, io.EOF
	}

	chunk := providers.StreamChunk{Data: []byte(data)}
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
// Comment lines and non-data fields are skipped; CRLF endings and data
// split across multiple lines are handled per the SSE spec. An event
// larger than maxEventBytes aborts with errEventTooLarge.
func (r *streamReader) nextEvent() (string, error) {
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
					return strings.Join(data, "\n"), nil
				}
				// blank line outside an event; the next event starts
				// with a fresh budget
				remaining = maxEventBytes
			case trimmed[0] == sseCommentByte:
				// comment or keep-alive line
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
			if errors.Is(err, io.EOF) && haveData {
				// stream ended mid-event; deliver what arrived
				return strings.Join(data, "\n"), nil
			}
			return "", err
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
		// upstream ended without [DONE]; treat as a normal end of stream
		return io.EOF
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
		return io.EOF
	default:
		return &providers.ProviderError{
			Provider: r.provider,
			Class:    providers.ErrClassUpstream,
			Message:  "stream read failed",
			Err:      err,
		}
	}
}
