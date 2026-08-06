package openaiwire_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rajeev-chaurasia/pen-stock/internal/providers"
	"github.com/rajeev-chaurasia/pen-stock/internal/providers/openaiwire"
)

// groqStream is a realistic Groq-style SSE body: keep-alive comment,
// several delta chunks, one CRLF-terminated event, an explicit
// "usage": null chunk, a multi-line data event carrying final usage,
// and the [DONE] sentinel.
const groqStream = ": keep-alive\n" +
	"data: {\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"\"}}]}\n\n" +
	"data: {\"choices\":[{\"delta\":{\"content\":\"Hel\"}}]}\n\n" +
	"data: {\"choices\":[{\"delta\":{\"content\":\"lo\"}}]}\r\n\r\n" +
	"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":null}\n\n" +
	"event: chunk\n" +
	"data: {\"choices\":[],\n" +
	"data:  \"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\n\n" +
	"data: [DONE]\n\n"

var groqStreamWant = []struct {
	data  string
	usage *providers.Usage
}{
	{data: `{"choices":[{"delta":{"role":"assistant","content":""}}]}`},
	{data: `{"choices":[{"delta":{"content":"Hel"}}]}`},
	{data: `{"choices":[{"delta":{"content":"lo"}}]}`},
	{data: `{"choices":[{"delta":{},"finish_reason":"stop"}],"usage":null}`},
	{
		data:  "{\"choices\":[],\n \"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}",
		usage: &providers.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
	},
}

// sseUpstream serves payload as an SSE response, optionally fragmented
// into awkward write sizes to exercise split reads.
func sseUpstream(t *testing.T, payload string, fragment int) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("upstream writer is not a flusher")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		step := fragment
		if step <= 0 {
			step = len(payload)
		}
		for start := 0; start < len(payload); start += step {
			end := min(start+step, len(payload))
			if _, err := io.WriteString(w, payload[start:end]); err != nil {
				return
			}
			flusher.Flush()
		}
	}))
	t.Cleanup(ts.Close)
	return ts
}

func collectChunks(t *testing.T, r providers.StreamReader) []providers.StreamChunk {
	t.Helper()
	var chunks []providers.StreamChunk
	for {
		c, err := r.Recv()
		if errors.Is(err, io.EOF) {
			return chunks
		}
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		chunks = append(chunks, c)
	}
}

func assertGroqChunks(t *testing.T, chunks []providers.StreamChunk) {
	t.Helper()
	if len(chunks) != len(groqStreamWant) {
		t.Fatalf("got %d chunks, want %d", len(chunks), len(groqStreamWant))
	}
	for i, want := range groqStreamWant {
		if string(chunks[i].Data) != want.data {
			t.Errorf("chunk %d data = %q, want %q", i, chunks[i].Data, want.data)
		}
		switch {
		case want.usage == nil && chunks[i].Usage != nil:
			t.Errorf("chunk %d has unexpected usage %+v", i, *chunks[i].Usage)
		case want.usage != nil && chunks[i].Usage == nil:
			t.Errorf("chunk %d missing usage", i)
		case want.usage != nil && *chunks[i].Usage != *want.usage:
			t.Errorf("chunk %d usage = %+v, want %+v", i, *chunks[i].Usage, *want.usage)
		}
	}
}

func TestChatStreamParsesGroqStyleSSE(t *testing.T) {
	upstream := sseUpstream(t, groqStream, 0)
	p := openaiwire.New("groq", upstream.URL, "k", nil)

	reader, err := p.ChatStream(context.Background(), chatReq())
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	defer reader.Close()

	assertGroqChunks(t, collectChunks(t, reader))

	// EOF must be sticky after [DONE].
	if _, err := reader.Recv(); !errors.Is(err, io.EOF) {
		t.Fatalf("Recv after DONE = %v, want io.EOF", err)
	}
}

func TestChatStreamSplitReads(t *testing.T) {
	for _, fragment := range []int{1, 3, 7} {
		t.Run(string(rune('0'+fragment)), func(t *testing.T) {
			upstream := sseUpstream(t, groqStream, fragment)
			p := openaiwire.New("groq", upstream.URL, "k", nil)

			reader, err := p.ChatStream(context.Background(), chatReq())
			if err != nil {
				t.Fatalf("ChatStream: %v", err)
			}
			defer reader.Close()

			assertGroqChunks(t, collectChunks(t, reader))
		})
	}
}

func TestChatStreamCancelMidStream(t *testing.T) {
	handlerDone := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(handlerDone)
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("upstream writer is not a flusher")
			return
		}
		// Drain the body so the server notices the client disconnect and
		// cancels r.Context().
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n")
		flusher.Flush()
		// Held open until the client tears the connection down.
		<-r.Context().Done()
	}))
	defer upstream.Close()

	p := openaiwire.New("groq", upstream.URL, "k", nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	reader, err := p.ChatStream(ctx, chatReq())
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	defer reader.Close()

	if _, err := reader.Recv(); err != nil {
		t.Fatalf("first Recv: %v", err)
	}

	recvErr := make(chan error, 1)
	go func() {
		_, err := reader.Recv()
		recvErr <- err
	}()
	cancel()

	select {
	case err := <-recvErr:
		assertClass(t, err, providers.ErrClassCanceled)
	case <-time.After(3 * time.Second):
		t.Fatal("Recv did not return promptly after cancel")
	}

	// The closed-channel sentinel proves the response body was released:
	// the upstream handler only exits once the connection is torn down.
	select {
	case <-handlerDone:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream handler still running; body not closed")
	}
}

func TestStreamReaderCloseSemantics(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("upstream writer is not a flusher")
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[]}\n\n")
		flusher.Flush()
		<-r.Context().Done()
	}))
	defer upstream.Close()

	p := openaiwire.New("groq", upstream.URL, "k", nil)
	reader, err := p.ChatStream(context.Background(), chatReq())
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}

	if _, err := reader.Recv(); err != nil {
		t.Fatalf("first Recv: %v", err)
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}

	recvErr := make(chan error, 1)
	go func() {
		_, err := reader.Recv()
		recvErr <- err
	}()
	select {
	case err := <-recvErr:
		if !errors.Is(err, io.EOF) {
			t.Fatalf("Recv after Close = %v, want io.EOF", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Recv after Close blocked")
	}
}
