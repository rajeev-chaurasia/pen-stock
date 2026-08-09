package providers

import (
	"errors"
	"io"
	"strings"
	"testing"
	"testing/iotest"
)

// collect drains a scanner, returning the events and the error that
// ended it. The error always arrives: a scanner never reports that a
// stream finished, only what it read.
func collect(t *testing.T, body string, r io.Reader) ([]SSEEvent, error) {
	t.Helper()
	s := NewSSEScanner(r)
	var out []SSEEvent
	for {
		ev, err := s.Next()
		if err != nil {
			return out, err
		}
		out = append(out, ev)
		if len(out) > 100 {
			t.Fatalf("scanner did not terminate on %q", body)
		}
	}
}

// Every case runs three times, through readers that hand over bytes at
// different granularity. A scanner whose buffering is wrong cannot
// survive all three, and a case that asserts nothing cannot tell them
// apart either, which is what makes this more than decoration.
func eachReader(t *testing.T, body string, check func(t *testing.T, events []SSEEvent, err error)) {
	t.Helper()
	shapes := []struct {
		name string
		wrap func(io.Reader) io.Reader
	}{
		{"whole", func(r io.Reader) io.Reader { return r }},
		{"one byte at a time", iotest.OneByteReader},
		{"half at a time", iotest.HalfReader},
	}
	for _, shape := range shapes {
		t.Run(shape.name, func(t *testing.T) {
			events, err := collect(t, body, shape.wrap(strings.NewReader(body)))
			check(t, events, err)
		})
	}
}

func TestSSEScannerJoinsMultipleDataLines(t *testing.T) {
	eachReader(t, "data: a\ndata: b\n\n", func(t *testing.T, events []SSEEvent, err error) {
		if !errors.Is(err, io.EOF) {
			t.Fatalf("err = %v, want io.EOF", err)
		}
		if len(events) != 1 {
			t.Fatalf("events = %+v, want 1", events)
		}
		if events[0].Data != "a\nb" {
			t.Errorf("data = %q, want %q: the lines of one event are joined with a newline", events[0].Data, "a\nb")
		}
	})
}

func TestSSEScannerHandlesCRLF(t *testing.T) {
	eachReader(t, "data: a\r\ndata: b\r\n\r\n", func(t *testing.T, events []SSEEvent, err error) {
		if !errors.Is(err, io.EOF) {
			t.Fatalf("err = %v, want io.EOF", err)
		}
		if len(events) != 1 || events[0].Data != "a\nb" {
			t.Errorf("events = %+v, want one event carrying %q with the carriage returns gone", events, "a\nb")
		}
	})
}

// Exactly one space after the colon is optional padding; a second one is
// part of the value. Getting this wrong silently corrupts every payload
// that happens to start with a space.
func TestSSEScannerTrimsExactlyOneLeadingSpace(t *testing.T) {
	cases := []struct {
		body string
		want string
	}{
		{"data:a\n\n", "a"},
		{"data: a\n\n", "a"},
		{"data:  a\n\n", " a"},
	}
	for _, tc := range cases {
		t.Run(strings.ReplaceAll(tc.body, "\n", "."), func(t *testing.T) {
			eachReader(t, tc.body, func(t *testing.T, events []SSEEvent, _ error) {
				if len(events) != 1 {
					t.Fatalf("events = %+v, want 1", events)
				}
				if events[0].Data != tc.want {
					t.Errorf("data = %q, want %q", events[0].Data, tc.want)
				}
			})
		})
	}
}

func TestSSEScannerSurfacesCommentsAsKeepalives(t *testing.T) {
	eachReader(t, ": ping\n\ndata: a\n\n", func(t *testing.T, events []SSEEvent, _ error) {
		if len(events) != 2 {
			t.Fatalf("events = %+v, want a keepalive then an event", events)
		}
		if !events[0].Keepalive || events[0].HasData {
			t.Errorf("first = %+v, want a keepalive carrying nothing", events[0])
		}
		if events[1].Data != "a" {
			t.Errorf("second = %+v, want the real event", events[1])
		}
	})
}

// A comment inside a partly built event is swallowed. Surfacing it would
// interrupt an event mid assembly with a keepalive that means nothing.
func TestSSEScannerSwallowsCommentsInsideAnEvent(t *testing.T) {
	eachReader(t, "event: message\n: ping\ndata: a\n\n", func(t *testing.T, events []SSEEvent, _ error) {
		if len(events) != 1 {
			t.Fatalf("events = %+v, want only the assembled event", events)
		}
		if events[0].Keepalive {
			t.Error("a comment inside an event surfaced as a keepalive")
		}
		if events[0].Name != "message" || events[0].Data != "a" {
			t.Errorf("event = %+v, want name message and data a", events[0])
		}
	})
}

// The distinction the type's own doc comment justifies: a named event
// with no data field is not the same as one whose data field is empty.
// Anthropic needs the first (message_stop carries nothing); the other
// two adapters skip it rather than emit an empty chunk.
func TestSSEScannerDistinguishesAbsentDataFromEmptyData(t *testing.T) {
	t.Run("name only", func(t *testing.T) {
		eachReader(t, "event: message_stop\n\n", func(t *testing.T, events []SSEEvent, _ error) {
			if len(events) != 1 {
				t.Fatalf("events = %+v, want 1", events)
			}
			if events[0].Name != "message_stop" {
				t.Errorf("name = %q, want message_stop", events[0].Name)
			}
			if events[0].HasData {
				t.Error("HasData is true for an event that carried no data field")
			}
		})
	})
	t.Run("empty data", func(t *testing.T) {
		eachReader(t, "data:\n\n", func(t *testing.T, events []SSEEvent, _ error) {
			if len(events) != 1 {
				t.Fatalf("events = %+v, want 1", events)
			}
			if !events[0].HasData {
				t.Error("HasData is false for an event whose data field was present but empty")
			}
			if events[0].Data != "" {
				t.Errorf("data = %q, want empty", events[0].Data)
			}
		})
	})
}

func TestSSEScannerSkipsUnknownFields(t *testing.T) {
	eachReader(t, "id: 7\nretry: 100\nfoo: bar\ndata: a\n\n", func(t *testing.T, events []SSEEvent, _ error) {
		if len(events) != 1 {
			t.Fatalf("events = %+v, want the unknown fields skipped without ending the event", events)
		}
		if events[0].Data != "a" {
			t.Errorf("data = %q, want a", events[0].Data)
		}
	})
}

func TestSSEScannerEmitsNothingForBlankLinesOutsideAnEvent(t *testing.T) {
	eachReader(t, "\n\n\ndata: a\n\n\n\n", func(t *testing.T, events []SSEEvent, _ error) {
		if len(events) != 1 {
			t.Fatalf("events = %+v, want exactly one: consecutive blanks are not empty events", events)
		}
	})
}

// A body that stops mid event is byte for byte what a severed connection
// looks like, so the pending data is dropped rather than handed on as a
// chunk. Every adapter's truncation detection rests on this.
func TestSSEScannerDiscardsAPartialEventAtEOF(t *testing.T) {
	eachReader(t, "data: complete\n\ndata: partial", func(t *testing.T, events []SSEEvent, err error) {
		if !errors.Is(err, io.EOF) {
			t.Fatalf("err = %v, want io.EOF", err)
		}
		if len(events) != 1 {
			t.Fatalf("events = %+v, want only the complete one", events)
		}
		if events[0].Data != "complete" {
			t.Errorf("data = %q, want the complete event", events[0].Data)
		}
	})
}

// The budget is what stops a hostile upstream forcing unbounded
// allocation, which makes it a security property rather than a
// convenience.
func TestSSEScannerRejectsAnOversizedEvent(t *testing.T) {
	body := "data: " + strings.Repeat("x", MaxSSEEventBytes+1) + "\n\n"
	_, err := collect(t, "oversized", strings.NewReader(body))
	if !errors.Is(err, ErrSSEEventTooLarge) {
		t.Errorf("err = %v, want ErrSSEEventTooLarge", err)
	}
}

// The same budget, reached without a single newline, which is the path
// that goes through bufio's buffer-full handling rather than a whole
// line. A scanner that only charged complete lines would buffer this
// without limit.
func TestSSEScannerRejectsAFloodWithNoLineEnding(t *testing.T) {
	_, err := collect(t, "flood", strings.NewReader(strings.Repeat("x", MaxSSEEventBytes+1)))
	if !errors.Is(err, ErrSSEEventTooLarge) {
		t.Errorf("err = %v, want ErrSSEEventTooLarge", err)
	}
}

// The budget is per Next call, and blank lines outside an event reset
// it. That reset is only load bearing when one call reads many blank
// lines before reaching an event, which is what a keepalive-heavy idle
// stream looks like: without it the accumulated blanks exhaust the
// budget and a perfectly ordinary event is refused as oversized.
//
// Enough newlines to overrun the cap on their own, then a small event.
func TestSSEScannerResetsTheBudgetOnBlankLines(t *testing.T) {
	body := strings.Repeat("\n", MaxSSEEventBytes+10) + "data: a\n\n"

	events, err := collect(t, "blank flood then an event", strings.NewReader(body))
	if errors.Is(err, ErrSSEEventTooLarge) {
		t.Fatal("blank lines consumed the byte budget; an idle stream would be refused as oversized")
	}
	if !errors.Is(err, io.EOF) {
		t.Fatalf("err = %v, want io.EOF", err)
	}
	if len(events) != 1 || events[0].Data != "a" {
		t.Errorf("events = %+v, want the one event after the blanks", events)
	}
}

// A long stream of large events, each within the cap, must all parse.
// This is about the per call budget rather than the reset above.
func TestSSEScannerAcceptsManyLargeEventsInSequence(t *testing.T) {
	const chunk = 64 << 10
	var b strings.Builder
	for i := 0; i < 40; i++ { // 2.5 MiB total, well past the per event cap
		b.WriteString("data: ")
		b.WriteString(strings.Repeat("y", chunk))
		b.WriteString("\n\n")
	}
	events, err := collect(t, "many large events", strings.NewReader(b.String()))
	if !errors.Is(err, io.EOF) {
		t.Fatalf("err = %v, want io.EOF", err)
	}
	if len(events) != 40 {
		t.Errorf("events = %d, want 40", len(events))
	}
}

// A transport failure is not an end of stream and must not be reported
// as one, or an adapter would mistake a severed connection for a body
// that simply ran out.
func TestSSEScannerReportsAReaderErrorRatherThanSwallowingIt(t *testing.T) {
	boom := errors.New("connection reset")
	r := io.MultiReader(strings.NewReader("data: a\n\n"), iotest.ErrReader(boom))

	s := NewSSEScanner(r)
	first, err := s.Next()
	if err != nil {
		t.Fatalf("first Next: %v", err)
	}
	if first.Data != "a" {
		t.Errorf("first = %+v, want the complete event", first)
	}
	if _, err = s.Next(); !errors.Is(err, boom) {
		t.Errorf("err = %v, want the reader's own error", err)
	}
}
