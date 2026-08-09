package providers

import (
	"bufio"
	"errors"
	"io"
	"strings"
)

const (
	sseDataPrefix  = "data:"
	sseEventPrefix = "event:"
	sseCommentByte = ':'
	sseLineJoiner  = "\n"

	// MaxSSEEventBytes bounds how much of a single SSE event is held in
	// memory. A well-formed chat event is a few KB, so 1 MiB leaves ample
	// headroom while keeping a hostile upstream from forcing unbounded
	// allocation through a never-terminated line. The response as a whole
	// is never buffered.
	MaxSSEEventBytes int = 1 << 20
)

// ErrSSEEventTooLarge reports an event that exceeded MaxSSEEventBytes.
// Each adapter classifies it itself, because the error it reports names
// its own provider.
var ErrSSEEventTooLarge = errors.New("sse event exceeds size limit")

// SSEEvent is one parsed server-sent event.
//
// HasData separates an event whose data field was empty from one that
// carried no data field at all: adapters that ignore event names skip
// the latter rather than translating it into an empty chunk. A keepalive
// carries nothing and exists only to prove the upstream is still working.
type SSEEvent struct {
	Name      string
	Data      string
	HasData   bool
	Keepalive bool
}

// SSEScanner reads events off a streaming response body.
//
// FRAMING IS SHARED, COMPLETENESS IS NOT. Every provider this gateway
// speaks to frames its stream the same way, because they all speak SSE:
// data fields joined across lines, CRLF endings, comment keepalives,
// unknown fields skipped, one event per blank line, and a byte budget so
// a hostile upstream cannot force unbounded allocation. That much is
// mechanical and belongs in one place.
//
// What must stay with each adapter is how it decides a completion is
// whole. The OpenAI wire sends a [DONE] sentinel, Gemini latches a
// non-empty finishReason, Anthropic sends message_stop. A scanner cannot
// tell any of them apart, and a body that simply ends looks byte for
// byte like a severed connection, so this type deliberately reports only
// what it read and never that the stream is finished. Deciding
// completeness here would collapse three different rules into one and
// let a truncated answer be reported as a finished one, which is exactly
// what ErrStreamTruncated exists to prevent.
type SSEScanner struct {
	br *bufio.Reader
}

// NewSSEScanner wraps a streaming body. The scanner buffers at most one
// event, never the whole response.
func NewSSEScanner(body io.Reader) *SSEScanner {
	return &SSEScanner{br: bufio.NewReader(body)}
}

// Next reads one SSE event. Comment lines surface as keepalives rather
// than being swallowed, because some backends send nothing else during a
// long time to first token. An event larger than MaxSSEEventBytes aborts
// with ErrSSEEventTooLarge. Pending event data at EOF is discarded, per
// the SSE spec: delivering a half-arrived event would hand the caller
// torn JSON that looks like a complete chunk.
func (s *SSEScanner) Next() (SSEEvent, error) {
	var ev SSEEvent
	var data []string
	remaining := MaxSSEEventBytes
	for {
		line, err := s.readLine(&remaining)
		if len(line) > 0 {
			trimmed := strings.TrimRight(line, "\r\n")
			switch {
			case trimmed == "":
				if ev.HasData || ev.Name != "" {
					ev.Data = strings.Join(data, sseLineJoiner)
					return ev, nil
				}
				// Blank line outside an event; the next one starts with
				// a fresh budget.
				remaining = MaxSSEEventBytes
			case trimmed[0] == sseCommentByte:
				if !ev.HasData && ev.Name == "" {
					return SSEEvent{Keepalive: true}, nil
				}
			case strings.HasPrefix(trimmed, sseDataPrefix):
				data = append(data, fieldValue(trimmed, sseDataPrefix))
				ev.HasData = true
			case strings.HasPrefix(trimmed, sseEventPrefix):
				ev.Name = fieldValue(trimmed, sseEventPrefix)
			default:
				// id:, retry: and unknown fields carry nothing we need.
			}
		}
		if err != nil {
			return SSEEvent{}, err
		}
	}
}

func fieldValue(line, prefix string) string {
	return strings.TrimPrefix(strings.TrimPrefix(line, prefix), " ")
}

// readLine returns one line including its terminator, charging its size
// against the per-event budget. ReadSlice keeps the buffered reader from
// accumulating a never-terminated line the way ReadString would; pieces
// are reassembled here under the budget.
func (s *SSEScanner) readLine(remaining *int) (string, error) {
	var line []byte
	for {
		chunk, err := s.br.ReadSlice('\n')
		*remaining -= len(chunk)
		if *remaining < 0 {
			return "", ErrSSEEventTooLarge
		}
		line = append(line, chunk...)
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return string(line), err
	}
}
