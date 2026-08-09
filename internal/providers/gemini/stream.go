package gemini

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
	"time"

	"github.com/rajeev-chaurasia/pen-stock/internal/providers"
)

const (
	sseDataPrefix  = "data:"
	sseCommentByte = ':'

	// maxEventBytes bounds how much of a single SSE event is held in
	// memory. A well-formed Gemini event is a few KB, so 1 MiB leaves
	// ample headroom while keeping a hostile upstream from forcing
	// unbounded allocation through a never-terminated line.
	maxEventBytes int = 1 << 20
)

// errEventTooLarge marks an SSE event that exceeded maxEventBytes.
var errEventTooLarge = errors.New("sse event exceeds size limit")

// streamReader turns a streamGenerateContent SSE body into OpenAI shaped
// StreamChunks. It holds at most maxEventBytes of one event in memory at
// a time; the response is never buffered whole.
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
	br       *bufio.Reader

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

// event is one parsed SSE event. A keepalive carries no data and exists
// only to prove the upstream is still working.
type event struct {
	data      string
	keepalive bool
}

func newStreamReader(ctx context.Context, provider, model string, body io.ReadCloser) *streamReader {
	return &streamReader{
		ctx:      ctx,
		provider: provider,
		body:     body,
		br:       bufio.NewReader(body),
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

	chunk, err := r.translateEvent(ev.data)
	if err != nil {
		_ = r.Close()
		r.finish(err)
		return providers.StreamChunk{}, r.termErr
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
				// event:, id:, retry: and unknown fields carry nothing we need.
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
		if r.sawFinish {
			return io.EOF
		}
		// The body ended before any candidate reported a finishReason.
		// Gemini has no [DONE], so this is the only way to tell a
		// finished turn from a severed one, and calling it io.EOF would
		// let the caller present a partial answer as a finished one.
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
