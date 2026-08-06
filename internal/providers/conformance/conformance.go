// Package conformance is the executable specification every provider
// adapter must satisfy. An adapter supplies the bytes its upstream would
// send; the suite asserts the behavior the gateway depends on, which is
// identical across providers no matter how different their wire formats
// are.
//
// Adapters translate into the OpenAI shape, because that is what the
// gateway serves to clients. The suite enforces that translation rather
// than trusting each adapter to remember it.
package conformance

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rajeev-chaurasia/pen-stock/internal/providers"
)

// Suite describes one adapter under test.
type Suite struct {
	// Name identifies the adapter in test output.
	Name string

	// New builds the adapter against a test upstream.
	New func(baseURL, apiKey string) providers.Provider

	// AuthHeader is the header carrying credentials, and AuthValue turns
	// a key into the value the adapter is expected to send.
	AuthHeader string
	AuthValue  func(apiKey string) string

	// NonStream is a successful completion as the upstream would send it.
	NonStream NonStreamCase

	// Stream is a successful streamed completion, terminated the way the
	// provider terminates one.
	Stream StreamCase

	// Truncated is a stream that stops without its completion marker,
	// which is what a severed upstream looks like.
	Truncated StreamCase

	// Errors map upstream failures to the class the gateway routes on.
	Errors []ErrorCase
}

// NonStreamCase is one upstream response body and what it must become.
type NonStreamCase struct {
	UpstreamBody []byte
	WantContent  string
	WantUsage    providers.Usage
}

// StreamCase is a full upstream stream body and what it must become.
type StreamCase struct {
	UpstreamBody []byte
	// WantContent is the assistant text assembled from every delta.
	WantContent string
	WantUsage   providers.Usage
}

// ErrorCase maps an upstream failure to its expected classification.
type ErrorCase struct {
	Name      string
	Status    int
	Body      []byte
	WantClass providers.ErrorClass
	// SecretInBody is planted text that must never reach the caller
	// through ProviderError.Message.
	SecretInBody string
}

const testAPIKey = "conformance-test-key-abcdef123456"

// Run executes the whole contract against one adapter.
func Run(t *testing.T, s Suite) {
	t.Helper()
	t.Run(s.Name+"/auth_header", func(t *testing.T) { testAuthHeader(t, s) })
	t.Run(s.Name+"/non_stream", func(t *testing.T) { testNonStream(t, s) })
	t.Run(s.Name+"/stream", func(t *testing.T) { testStream(t, s) })
	t.Run(s.Name+"/stream_truncated", func(t *testing.T) { testTruncated(t, s) })
	t.Run(s.Name+"/stream_split_reads", func(t *testing.T) { testSplitReads(t, s) })
	t.Run(s.Name+"/errors", func(t *testing.T) { testErrors(t, s) })
	t.Run(s.Name+"/cancel_releases_body", func(t *testing.T) { testCancel(t, s) })
	t.Run(s.Name+"/close_is_idempotent", func(t *testing.T) { testCloseIdempotent(t, s) })
}

// upstream serves body once, optionally in awkward fragments so split
// reads across chunk boundaries get exercised.
func upstream(t *testing.T, status int, body []byte, fragment int, seen *http.Header) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if seen != nil {
			*seen = r.Header.Clone()
		}
		_, _ = io.Copy(io.Discard, r.Body)
		if status != http.StatusOK {
			w.WriteHeader(status)
			_, _ = w.Write(body)
			return
		}
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("test upstream cannot flush")
			return
		}
		step := fragment
		if step <= 0 {
			step = len(body)
		}
		for start := 0; start < len(body); start += step {
			end := min(start+step, len(body))
			if _, err := w.Write(body[start:end]); err != nil {
				return
			}
			flusher.Flush()
		}
	}))
	t.Cleanup(ts.Close)
	return ts
}

func chatRequest(stream bool) *providers.ChatRequest {
	raw := `{"model":"test-model","messages":[{"role":"user","content":"hello"}]}`
	if stream {
		raw = `{"model":"test-model","stream":true,"messages":[{"role":"user","content":"hello"}]}`
	}
	return &providers.ChatRequest{Model: "test-model", Stream: stream, Raw: json.RawMessage(raw)}
}

func testAuthHeader(t *testing.T, s Suite) {
	var seen http.Header
	ts := upstream(t, http.StatusOK, s.NonStream.UpstreamBody, 0, &seen)
	p := s.New(ts.URL, testAPIKey)

	if _, err := p.Chat(context.Background(), chatRequest(false)); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	got := seen.Get(s.AuthHeader)
	if want := s.AuthValue(testAPIKey); got != want {
		t.Errorf("%s = %q, want %q", s.AuthHeader, got, want)
	}
}

func testNonStream(t *testing.T, s Suite) {
	ts := upstream(t, http.StatusOK, s.NonStream.UpstreamBody, 0, nil)
	p := s.New(ts.URL, testAPIKey)

	resp, err := p.Chat(context.Background(), chatRequest(false))
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Usage != s.NonStream.WantUsage {
		t.Errorf("usage = %+v, want %+v", resp.Usage, s.NonStream.WantUsage)
	}
	// The gateway serves the OpenAI shape, so every adapter must produce
	// it regardless of what its upstream speaks.
	assertOpenAICompletion(t, resp.Body, s.NonStream.WantContent)
}

func testStream(t *testing.T, s Suite) {
	ts := upstream(t, http.StatusOK, s.Stream.UpstreamBody, 0, nil)
	p := s.New(ts.URL, testAPIKey)

	reader, err := p.ChatStream(context.Background(), chatRequest(true))
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	defer func() { _ = reader.Close() }()

	content, usage, err := drain(t, reader)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("stream ended with %v, want io.EOF for a complete stream", err)
	}
	if content != s.Stream.WantContent {
		t.Errorf("assembled content = %q, want %q", content, s.Stream.WantContent)
	}
	if usage != nil && *usage != s.Stream.WantUsage {
		t.Errorf("usage = %+v, want %+v", *usage, s.Stream.WantUsage)
	}
	if usage == nil && s.Stream.WantUsage != (providers.Usage{}) {
		t.Errorf("no usage reported, want %+v", s.Stream.WantUsage)
	}
}

func testTruncated(t *testing.T, s Suite) {
	ts := upstream(t, http.StatusOK, s.Truncated.UpstreamBody, 0, nil)
	p := s.New(ts.URL, testAPIKey)

	reader, err := p.ChatStream(context.Background(), chatRequest(true))
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	defer func() { _ = reader.Close() }()

	// A stream that stops early must say so. Reporting io.EOF here would
	// let the gateway present a partial answer as a finished one.
	_, _, err = drain(t, reader)
	if !errors.Is(err, providers.ErrStreamTruncated) {
		t.Fatalf("truncated stream ended with %v, want ErrStreamTruncated", err)
	}
}

func testSplitReads(t *testing.T, s Suite) {
	// Network writes do not respect event boundaries, so the parser must
	// survive the body arriving in arbitrary pieces.
	for _, fragment := range []int{1, 3, 17} {
		t.Run(fragmentName(fragment), func(t *testing.T) {
			ts := upstream(t, http.StatusOK, s.Stream.UpstreamBody, fragment, nil)
			p := s.New(ts.URL, testAPIKey)

			reader, err := p.ChatStream(context.Background(), chatRequest(true))
			if err != nil {
				t.Fatalf("ChatStream: %v", err)
			}
			defer func() { _ = reader.Close() }()

			content, _, err := drain(t, reader)
			if !errors.Is(err, io.EOF) {
				t.Fatalf("stream ended with %v, want io.EOF", err)
			}
			if content != s.Stream.WantContent {
				t.Errorf("content = %q, want %q", content, s.Stream.WantContent)
			}
		})
	}
}

func testErrors(t *testing.T, s Suite) {
	for _, ec := range s.Errors {
		t.Run(ec.Name, func(t *testing.T) {
			ts := upstream(t, ec.Status, ec.Body, 0, nil)
			p := s.New(ts.URL, testAPIKey)

			_, err := p.Chat(context.Background(), chatRequest(false))
			if err == nil {
				t.Fatal("Chat = nil error, want a failure")
			}
			var pe *providers.ProviderError
			if !errors.As(err, &pe) {
				t.Fatalf("error %v is not a *providers.ProviderError", err)
			}
			if pe.Class != ec.WantClass {
				t.Errorf("class = %q, want %q", pe.Class, ec.WantClass)
			}
			if pe.StatusCode != ec.Status {
				t.Errorf("StatusCode = %d, want %d", pe.StatusCode, ec.Status)
			}
			// The key must never ride along in an error, since the
			// message is the one field that can reach a caller.
			if strings.Contains(pe.Error(), testAPIKey) {
				t.Errorf("error text leaks the api key: %s", pe.Error())
			}
			if ec.SecretInBody != "" && strings.Contains(pe.Message, ec.SecretInBody) {
				t.Errorf("error message leaks planted secret %q: %s", ec.SecretInBody, pe.Message)
			}
		})
	}
}

func testCancel(t *testing.T, s Suite) {
	handlerGone := make(chan struct{})
	var once sync.Once
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done()
		once.Do(func() { close(handlerGone) })
	}))
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	p := s.New(ts.URL, testAPIKey)
	reader, err := p.ChatStream(ctx, chatRequest(true))
	if err != nil {
		cancel()
		t.Fatalf("ChatStream: %v", err)
	}

	recvDone := make(chan struct{})
	go func() {
		defer close(recvDone)
		for {
			if _, err := reader.Recv(); err != nil {
				return
			}
		}
	}()

	cancel()
	select {
	case <-recvDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Recv did not return after context cancel")
	}
	select {
	case <-handlerGone:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream connection was not released after cancel")
	}
	_ = reader.Close()
}

func testCloseIdempotent(t *testing.T, s Suite) {
	ts := upstream(t, http.StatusOK, s.Stream.UpstreamBody, 0, nil)
	p := s.New(ts.URL, testAPIKey)

	reader, err := p.ChatStream(context.Background(), chatRequest(true))
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if err := reader.Close(); err != nil {
		t.Errorf("first Close: %v", err)
	}
	if err := reader.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}

	// A Recv after Close must return promptly rather than block on a
	// connection nobody is going to feed.
	done := make(chan error, 1)
	go func() {
		_, err := reader.Recv()
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Error("Recv after Close returned no error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Recv after Close blocked")
	}
}

// drain reads a stream to its end, assembling assistant text from the
// OpenAI shaped deltas every adapter must emit.
func drain(t *testing.T, r providers.StreamReader) (content string, usage *providers.Usage, err error) {
	t.Helper()
	var sb strings.Builder
	for {
		chunk, recvErr := r.Recv()
		if recvErr != nil {
			return sb.String(), usage, recvErr
		}
		if chunk.Keepalive {
			continue
		}
		if chunk.Usage != nil {
			u := *chunk.Usage
			usage = &u
		}
		var envelope struct {
			Object  string `json:"object"`
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if jsonErr := json.Unmarshal(chunk.Data, &envelope); jsonErr != nil {
			t.Errorf("chunk is not valid JSON: %v (%s)", jsonErr, chunk.Data)
			continue
		}
		if envelope.Object != "" && envelope.Object != "chat.completion.chunk" {
			t.Errorf("chunk object = %q, want chat.completion.chunk", envelope.Object)
		}
		for _, c := range envelope.Choices {
			sb.WriteString(c.Delta.Content)
		}
	}
}

func assertOpenAICompletion(t *testing.T, body []byte, wantContent string) {
	t.Helper()
	var envelope struct {
		Object  string `json:"object"`
		Choices []struct {
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("response body is not valid JSON: %v", err)
	}
	if envelope.Object != "chat.completion" {
		t.Errorf("object = %q, want chat.completion", envelope.Object)
	}
	if len(envelope.Choices) == 0 {
		t.Fatal("response has no choices")
	}
	if got := envelope.Choices[0].Message.Content; got != wantContent {
		t.Errorf("content = %q, want %q", got, wantContent)
	}
	if role := envelope.Choices[0].Message.Role; role != "assistant" {
		t.Errorf("role = %q, want assistant", role)
	}
}

func fragmentName(n int) string {
	switch n {
	case 1:
		return "one_byte_writes"
	case 3:
		return "three_byte_writes"
	default:
		return "seventeen_byte_writes"
	}
}
