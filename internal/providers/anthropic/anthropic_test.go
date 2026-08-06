package anthropic_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rajeev-chaurasia/pen-stock/internal/config"
	"github.com/rajeev-chaurasia/pen-stock/internal/providers"
	"github.com/rajeev-chaurasia/pen-stock/internal/providers/anthropic"
)

const (
	testModel = "claude-sonnet-4-5-20250929"

	// okResponse is the shortest well-formed Messages reply, used when a
	// test cares about the request rather than the answer.
	okResponse = `{"id":"msg_01","type":"message","role":"assistant","model":"` + testModel + `",` +
		`"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn",` +
		`"usage":{"input_tokens":4,"output_tokens":1}}`
)

// sentRequest is the Anthropic body this adapter produced.
type sentRequest struct {
	Model         string   `json:"model"`
	System        string   `json:"system"`
	MaxTokens     int      `json:"max_tokens"`
	Temperature   *float64 `json:"temperature"`
	TopP          *float64 `json:"top_p"`
	StopSequences []string `json:"stop_sequences"`
	Stream        bool     `json:"stream"`
	Messages      []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
}

// gotCompletion is the OpenAI body this adapter handed back.
type gotCompletion struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Model   string `json:"model"`
	Created int64  `json:"created"`
	Choices []struct {
		Index   int `json:"index"`
		Message struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

type gotChunk struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Model   string `json:"model"`
	Choices []struct {
		Delta struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
}

type recorder struct {
	path   string
	header http.Header
	body   []byte
}

// jsonUpstream serves one canned Messages reply and records what it was
// asked for.
func jsonUpstream(t *testing.T, respBody string) (*httptest.Server, *recorder) {
	t.Helper()
	rec := &recorder{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rec.body = body
		rec.header = r.Header.Clone()
		rec.path = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, respBody)
	}))
	t.Cleanup(ts.Close)
	return ts, rec
}

func sseUpstream(t *testing.T, payload string) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, payload)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	t.Cleanup(ts.Close)
	return ts
}

func chatReq(raw string, stream bool) *providers.ChatRequest {
	return &providers.ChatRequest{Model: testModel, Stream: stream, Raw: json.RawMessage(raw)}
}

// translated posts raw through the adapter and returns the Anthropic
// body that came out the other side.
func translated(t *testing.T, raw string) sentRequest {
	t.Helper()
	ts, rec := jsonUpstream(t, okResponse)
	p := anthropic.New("claude", ts.URL, "k", nil)

	if _, err := p.Chat(context.Background(), chatReq(raw, false)); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	var out sentRequest
	if err := json.Unmarshal(rec.body, &out); err != nil {
		t.Fatalf("upstream body is not valid JSON: %v (%s)", err, rec.body)
	}
	return out
}

func completed(t *testing.T, upstreamBody string) gotCompletion {
	t.Helper()
	ts, _ := jsonUpstream(t, upstreamBody)
	p := anthropic.New("claude", ts.URL, "k", nil)

	resp, err := p.Chat(context.Background(), chatReq(`{"messages":[{"role":"user","content":"hi"}]}`, false))
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	var out gotCompletion
	if err := json.Unmarshal(resp.Body, &out); err != nil {
		t.Fatalf("translated body is not valid JSON: %v (%s)", err, resp.Body)
	}
	return out
}

// collect drains a stream, separating data chunks from keepalives.
func collect(t *testing.T, r providers.StreamReader) (chunks []providers.StreamChunk, keepalives int, err error) {
	t.Helper()
	for {
		c, recvErr := r.Recv()
		if recvErr != nil {
			return chunks, keepalives, recvErr
		}
		if c.Keepalive {
			keepalives++
			continue
		}
		chunks = append(chunks, c)
	}
}

func streamOf(t *testing.T, payload string) (chunks []providers.StreamChunk, keepalives int, err error) {
	t.Helper()
	ts := sseUpstream(t, payload)
	p := anthropic.New("claude", ts.URL, "k", nil)

	reader, streamErr := p.ChatStream(context.Background(), chatReq(`{"messages":[{"role":"user","content":"hi"}]}`, true))
	if streamErr != nil {
		t.Fatalf("ChatStream: %v", streamErr)
	}
	t.Cleanup(func() { _ = reader.Close() })
	return collect(t, reader)
}

func decodeChunk(t *testing.T, data []byte) gotChunk {
	t.Helper()
	var out gotChunk
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("chunk is not valid JSON: %v (%s)", err, data)
	}
	if out.Object != "chat.completion.chunk" {
		t.Errorf("chunk object = %q, want chat.completion.chunk", out.Object)
	}
	if len(out.Choices) != 1 {
		t.Fatalf("chunk has %d choices, want 1", len(out.Choices))
	}
	return out
}

// TestSystemMessageIsLiftedOutOfMessages covers the difference between
// the two formats that is easiest to get wrong: Anthropic rejects a
// system turn left inside messages[].
func TestSystemMessageIsLiftedOutOfMessages(t *testing.T) {
	tests := []struct {
		name         string
		raw          string
		wantSystem   string
		wantRoles    []string
		wantContents []string
	}{
		{
			name: "system lifted to the top level field",
			raw: `{"messages":[{"role":"system","content":"You are terse."},` +
				`{"role":"user","content":"hi"}]}`,
			wantSystem:   "You are terse.",
			wantRoles:    []string{"user"},
			wantContents: []string{`"hi"`},
		},
		{
			name: "several system turns are joined",
			raw: `{"messages":[{"role":"system","content":"Be terse."},` +
				`{"role":"user","content":"hi"},` +
				`{"role":"system","content":"Answer in English."}]}`,
			wantSystem:   "Be terse.\n\nAnswer in English.",
			wantRoles:    []string{"user"},
			wantContents: []string{`"hi"`},
		},
		{
			name: "developer is the renamed system role and lifts too",
			raw: `{"messages":[{"role":"developer","content":"Be terse."},` +
				`{"role":"user","content":"hi"}]}`,
			wantSystem: "Be terse.",
			wantRoles:  []string{"user"},
		},
		{
			name: "system sent as typed parts",
			raw: `{"messages":[{"role":"system","content":[{"type":"text","text":"Be terse."}]},` +
				`{"role":"user","content":"hi"}]}`,
			wantSystem: "Be terse.",
			wantRoles:  []string{"user"},
		},
		{
			name: "user and assistant turns pass through in order",
			raw: `{"messages":[{"role":"user","content":"hi"},` +
				`{"role":"assistant","content":"hello"},` +
				`{"role":"user","content":"again"}]}`,
			wantSystem: "",
			wantRoles:  []string{"user", "assistant", "user"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := translated(t, tc.raw)
			if got.System != tc.wantSystem {
				t.Errorf("system = %q, want %q", got.System, tc.wantSystem)
			}
			if len(got.Messages) != len(tc.wantRoles) {
				t.Fatalf("messages = %d, want %d (%s)", len(got.Messages), len(tc.wantRoles), got.Messages)
			}
			for i, want := range tc.wantRoles {
				if got.Messages[i].Role != want {
					t.Errorf("messages[%d].role = %q, want %q", i, got.Messages[i].Role, want)
				}
			}
			for i, want := range tc.wantContents {
				if string(got.Messages[i].Content) != want {
					t.Errorf("messages[%d].content = %s, want %s", i, got.Messages[i].Content, want)
				}
			}
		})
	}
}

// TestMaxTokensIsAlwaysSent covers the field chat.completions treats as
// optional and the Messages API requires.
func TestMaxTokensIsAlwaysSent(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want int
	}{
		{
			name: "default injected when the client omits it",
			raw:  `{"messages":[{"role":"user","content":"hi"}]}`,
			want: 4096,
		},
		{
			name: "explicit max_tokens wins",
			raw:  `{"messages":[{"role":"user","content":"hi"}],"max_tokens":128}`,
			want: 128,
		},
		{
			name: "max_completion_tokens is honored too",
			raw:  `{"messages":[{"role":"user","content":"hi"}],"max_completion_tokens":256}`,
			want: 256,
		},
		{
			name: "explicit null falls back to the default",
			raw:  `{"messages":[{"role":"user","content":"hi"}],"max_tokens":null}`,
			want: 4096,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := translated(t, tc.raw).MaxTokens; got != tc.want {
				t.Errorf("max_tokens = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestSamplingAndStopTranslation(t *testing.T) {
	tests := []struct {
		name  string
		raw   string
		check func(t *testing.T, got sentRequest)
	}{
		{
			name: "temperature and top_p carry across",
			raw:  `{"messages":[{"role":"user","content":"hi"}],"temperature":0.2,"top_p":0.9}`,
			check: func(t *testing.T, got sentRequest) {
				if got.Temperature == nil || *got.Temperature != 0.2 {
					t.Errorf("temperature = %v, want 0.2", got.Temperature)
				}
				if got.TopP == nil || *got.TopP != 0.9 {
					t.Errorf("top_p = %v, want 0.9", got.TopP)
				}
			},
		},
		{
			name: "omitted sampling knobs stay omitted",
			raw:  `{"messages":[{"role":"user","content":"hi"}]}`,
			check: func(t *testing.T, got sentRequest) {
				if got.Temperature != nil || got.TopP != nil {
					t.Errorf("temperature = %v, top_p = %v, want both absent", got.Temperature, got.TopP)
				}
			},
		},
		{
			name: "a bare stop string becomes one stop sequence",
			raw:  `{"messages":[{"role":"user","content":"hi"}],"stop":"END"}`,
			check: func(t *testing.T, got sentRequest) {
				if len(got.StopSequences) != 1 || got.StopSequences[0] != "END" {
					t.Errorf("stop_sequences = %v, want [END]", got.StopSequences)
				}
			},
		},
		{
			name: "a stop array becomes stop_sequences",
			raw:  `{"messages":[{"role":"user","content":"hi"}],"stop":["A","B"]}`,
			check: func(t *testing.T, got sentRequest) {
				if len(got.StopSequences) != 2 || got.StopSequences[0] != "A" || got.StopSequences[1] != "B" {
					t.Errorf("stop_sequences = %v, want [A B]", got.StopSequences)
				}
			},
		},
		{
			name: "the routed model overrides the one in the body",
			raw:  `{"model":"whatever-the-client-typed","messages":[{"role":"user","content":"hi"}]}`,
			check: func(t *testing.T, got sentRequest) {
				if got.Model != testModel {
					t.Errorf("model = %q, want %q", got.Model, testModel)
				}
			},
		},
		{
			name: "a buffered call does not ask for a stream",
			raw:  `{"messages":[{"role":"user","content":"hi"}]}`,
			check: func(t *testing.T, got sentRequest) {
				if got.Stream {
					t.Error("stream = true on a buffered call")
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tc.check(t, translated(t, tc.raw))
		})
	}
}

func TestChatStreamAsksForAStream(t *testing.T) {
	ts, rec := jsonUpstream(t, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	p := anthropic.New("claude", ts.URL, "k", nil)

	reader, err := p.ChatStream(context.Background(), chatReq(`{"messages":[{"role":"user","content":"hi"}]}`, true))
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	defer reader.Close()

	var got sentRequest
	if err := json.Unmarshal(rec.body, &got); err != nil {
		t.Fatalf("upstream body is not valid JSON: %v", err)
	}
	if !got.Stream {
		t.Error("stream = false, want true on a streaming call")
	}
}

func TestRequestHeadersAndPath(t *testing.T) {
	ts, rec := jsonUpstream(t, okResponse)
	p := anthropic.New("claude", ts.URL, "secret-key", nil)

	if _, err := p.Chat(context.Background(), chatReq(`{"messages":[{"role":"user","content":"hi"}]}`, false)); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if got := rec.header.Get("x-api-key"); got != "secret-key" {
		t.Errorf("x-api-key = %q, want the raw key", got)
	}
	if got := rec.header.Get("Authorization"); got != "" {
		t.Errorf("Authorization = %q, want no bearer token on this API", got)
	}
	if got := rec.header.Get("anthropic-version"); got != "2023-06-01" {
		t.Errorf("anthropic-version = %q, want 2023-06-01", got)
	}
	if rec.path != "/messages" {
		t.Errorf("path = %q, want /messages", rec.path)
	}
}

// stubTransport answers every request without a network, recording the
// URL the adapter chose.
type stubTransport struct {
	url string
}

func (s *stubTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	s.url = req.URL.String()
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(okResponse)),
		Request:    req,
	}, nil
}

func TestBaseURLDefaultAndTrimming(t *testing.T) {
	tests := []struct {
		name    string
		baseURL string
		want    string
	}{
		{
			name:    "empty base_url falls back to the public endpoint",
			baseURL: "",
			want:    "https://api.anthropic.com/v1/messages",
		},
		{
			name:    "a trailing slash does not double up",
			baseURL: "https://gateway.internal/anthropic/v1/",
			want:    "https://gateway.internal/anthropic/v1/messages",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stub := &stubTransport{}
			p := anthropic.New("claude", tc.baseURL, "k", &http.Client{Transport: stub})
			if _, err := p.Chat(context.Background(), chatReq(`{"messages":[{"role":"user","content":"hi"}]}`, false)); err != nil {
				t.Fatalf("Chat: %v", err)
			}
			if stub.url != tc.want {
				t.Errorf("url = %q, want %q", stub.url, tc.want)
			}
		})
	}
}

func TestKindIsRegistered(t *testing.T) {
	built, err := providers.BuildAll([]config.ProviderConfig{{Name: "claude", Kind: config.KindAnthropic}})
	if err != nil {
		t.Fatalf("BuildAll: %v", err)
	}
	p, ok := built["claude"]
	if !ok {
		t.Fatal("no provider built for kind anthropic")
	}
	if p.Name() != "claude" {
		t.Errorf("Name = %q, want claude", p.Name())
	}
}

func TestStopReasonMapping(t *testing.T) {
	tests := []struct {
		name       string
		stopReason string
		want       string
	}{
		{name: "end_turn is a normal stop", stopReason: `"end_turn"`, want: "stop"},
		{name: "max_tokens is a length cutoff", stopReason: `"max_tokens"`, want: "length"},
		{name: "stop_sequence is a stop too", stopReason: `"stop_sequence"`, want: "stop"},
		{name: "tool_use maps to tool_calls", stopReason: `"tool_use"`, want: "tool_calls"},
		{name: "an unknown reason still ended the turn", stopReason: `"refusal"`, want: "stop"},
		{name: "no reason reports none", stopReason: `null`, want: ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"id":"msg_01","type":"message","role":"assistant","model":"` + testModel + `",` +
				`"content":[{"type":"text","text":"hi"}],"stop_reason":` + tc.stopReason + `,` +
				`"usage":{"input_tokens":1,"output_tokens":1}}`
			got := completed(t, body)
			if len(got.Choices) != 1 {
				t.Fatalf("choices = %d, want 1", len(got.Choices))
			}
			if got.Choices[0].FinishReason != tc.want {
				t.Errorf("finish_reason = %q, want %q", got.Choices[0].FinishReason, tc.want)
			}
		})
	}
}

func TestNonStreamResponseTranslation(t *testing.T) {
	// Several text blocks plus a non-text block, which is what a message
	// with a tool call looks like. Only text belongs in the OpenAI
	// content string.
	body := `{"id":"msg_01XFDUDYJgAACzvnptvVoYEL","type":"message","role":"assistant","model":"` + testModel + `",` +
		`"content":[{"type":"text","text":"Hello "},` +
		`{"type":"tool_use","id":"toolu_01","name":"lookup","input":{}},` +
		`{"type":"text","text":"there"}],` +
		`"stop_reason":"end_turn","stop_sequence":null,` +
		`"usage":{"input_tokens":11,"output_tokens":4}}`

	ts, _ := jsonUpstream(t, body)
	p := anthropic.New("claude", ts.URL, "k", nil)

	resp, err := p.Chat(context.Background(), chatReq(`{"messages":[{"role":"user","content":"hi"}]}`, false))
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Provider != "claude" {
		t.Errorf("provider = %q, want claude", resp.Provider)
	}
	want := providers.Usage{PromptTokens: 11, CompletionTokens: 4, TotalTokens: 15}
	if resp.Usage != want {
		t.Errorf("usage = %+v, want %+v", resp.Usage, want)
	}

	var got gotCompletion
	if err := json.Unmarshal(resp.Body, &got); err != nil {
		t.Fatalf("translated body is not valid JSON: %v", err)
	}
	if got.ID != "msg_01XFDUDYJgAACzvnptvVoYEL" {
		t.Errorf("id = %q, want the upstream id passed through", got.ID)
	}
	if got.Object != "chat.completion" {
		t.Errorf("object = %q, want chat.completion", got.Object)
	}
	if got.Model != testModel {
		t.Errorf("model = %q, want %q", got.Model, testModel)
	}
	if got.Created == 0 {
		t.Error("created = 0, want a timestamp the OpenAI shape requires")
	}
	if got.Choices[0].Message.Role != "assistant" {
		t.Errorf("role = %q, want assistant", got.Choices[0].Message.Role)
	}
	if got.Choices[0].Message.Content != "Hello there" {
		t.Errorf("content = %q, want the text blocks concatenated", got.Choices[0].Message.Content)
	}
	if got.Usage.TotalTokens != 15 || got.Usage.PromptTokens != 11 || got.Usage.CompletionTokens != 4 {
		t.Errorf("usage in body = %+v, want 11/4/15", got.Usage)
	}
}

func TestStreamParsesEventTypedSSE(t *testing.T) {
	chunks, keepalives, err := streamOf(t, anthropicStream)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("stream ended with %v, want io.EOF after message_stop", err)
	}
	if keepalives != 1 {
		t.Errorf("keepalives = %d, want the ping surfaced once", keepalives)
	}
	if len(chunks) != 4 {
		t.Fatalf("chunks = %d, want role, two deltas, and a final chunk", len(chunks))
	}

	first := decodeChunk(t, chunks[0].Data)
	if first.Choices[0].Delta.Role != "assistant" {
		t.Errorf("first delta role = %q, want assistant", first.Choices[0].Delta.Role)
	}
	if first.ID != "msg_01Fx9k" {
		t.Errorf("id = %q, want the id from message_start", first.ID)
	}
	if first.Model != testModel {
		t.Errorf("model = %q, want the model from message_start", first.Model)
	}

	for i, want := range []string{"Hel", "lo"} {
		got := decodeChunk(t, chunks[i+1].Data)
		if got.Choices[0].Delta.Content != want {
			t.Errorf("chunk %d content = %q, want %q", i+1, got.Choices[0].Delta.Content, want)
		}
		if got.Choices[0].FinishReason != nil {
			t.Errorf("chunk %d finish_reason = %q, want null mid-stream", i+1, *got.Choices[0].FinishReason)
		}
	}

	last := decodeChunk(t, chunks[3].Data)
	if last.Choices[0].Delta.Content != "" {
		t.Errorf("final delta content = %q, want empty", last.Choices[0].Delta.Content)
	}
	if last.Choices[0].FinishReason == nil || *last.Choices[0].FinishReason != "stop" {
		t.Errorf("final finish_reason = %v, want stop", last.Choices[0].FinishReason)
	}
}

// TestStreamUsageAssembledAcrossEvents pins the two-halves rule: input
// tokens arrive with message_start, output tokens with message_delta,
// and nothing is reported until both are in hand.
func TestStreamUsageAssembledAcrossEvents(t *testing.T) {
	chunks, _, err := streamOf(t, anthropicStream)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("stream ended with %v, want io.EOF", err)
	}
	for i, c := range chunks[:len(chunks)-1] {
		if c.Usage != nil {
			t.Errorf("chunk %d reported usage %+v before message_delta", i, *c.Usage)
		}
	}
	last := chunks[len(chunks)-1]
	if last.Usage == nil {
		t.Fatal("no usage on the message_delta chunk")
	}
	// message_start advertised output_tokens 1 and message_delta
	// corrected it to 2; the later count is the real one.
	want := providers.Usage{PromptTokens: 9, CompletionTokens: 2, TotalTokens: 11}
	if *last.Usage != want {
		t.Errorf("usage = %+v, want %+v", *last.Usage, want)
	}
}

func TestStreamWithoutUsageReportsNone(t *testing.T) {
	// A message_delta carrying no usage leaves the pair incomplete, so
	// the adapter reports nothing rather than half a bill.
	stream := "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_01","model":"` + testModel + `",` +
		`"usage":{"input_tokens":9,"output_tokens":1}}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"

	chunks, _, err := streamOf(t, stream)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("stream ended with %v, want io.EOF", err)
	}
	for i, c := range chunks {
		if c.Usage != nil {
			t.Errorf("chunk %d reported usage %+v with no output token count", i, *c.Usage)
		}
	}
}

// TestStreamWithoutMessageStopIsTruncated is the behavior this adapter
// exists to get right. Anthropic sends no [DONE], so a body that ends
// before message_stop is a partial answer and must never look like a
// finished one.
func TestStreamWithoutMessageStopIsTruncated(t *testing.T) {
	tests := []struct {
		name    string
		payload string
	}{
		{
			name:    "body stops after a text delta",
			payload: truncatedStream,
		},
		{
			name: "body stops after message_delta, before message_stop",
			payload: truncatedStream + "event: message_delta\n" +
				`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}` + "\n\n",
		},
		{
			name: "the final event arrives torn",
			payload: truncatedStream + "event: content_block_delta\n" +
				`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_del`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := streamOf(t, tc.payload)
			if !errors.Is(err, providers.ErrStreamTruncated) {
				t.Fatalf("stream ended with %v, want ErrStreamTruncated", err)
			}
		})
	}
}

func TestTruncationIsSticky(t *testing.T) {
	ts := sseUpstream(t, truncatedStream)
	p := anthropic.New("claude", ts.URL, "k", nil)

	reader, err := p.ChatStream(context.Background(), chatReq(`{"messages":[{"role":"user","content":"hi"}]}`, true))
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	defer reader.Close()

	if _, _, err := collect(t, reader); !errors.Is(err, providers.ErrStreamTruncated) {
		t.Fatalf("first drain ended with %v, want ErrStreamTruncated", err)
	}
	// A caller polling again must not read a severed stream as a clean
	// end on the second try.
	if _, err := reader.Recv(); !errors.Is(err, providers.ErrStreamTruncated) {
		t.Fatalf("Recv after truncation = %v, want ErrStreamTruncated", err)
	}
}

func TestPingAndCommentsAreKeepalives(t *testing.T) {
	stream := ": upstream is thinking\n\n" +
		"event: ping\n" +
		`data: {"type": "ping"}` + "\n\n" +
		"event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_01","model":"` + testModel + `",` +
		`"usage":{"input_tokens":3,"output_tokens":1}}}` + "\n\n" +
		"event: ping\n" +
		`data: {"type": "ping"}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"

	chunks, keepalives, err := streamOf(t, stream)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("stream ended with %v, want io.EOF", err)
	}
	if keepalives != 3 {
		t.Errorf("keepalives = %d, want two pings and one comment", keepalives)
	}
	if len(chunks) != 2 {
		t.Fatalf("chunks = %d, want the role chunk and one delta", len(chunks))
	}
	for i, c := range chunks {
		if len(c.Data) == 0 {
			t.Errorf("chunk %d carries no data", i)
		}
	}
}

func TestStreamRecvAfterCloseDoesNotBlock(t *testing.T) {
	ts := sseUpstream(t, anthropicStream)
	p := anthropic.New("claude", ts.URL, "k", nil)

	reader, err := p.ChatStream(context.Background(), chatReq(`{"messages":[{"role":"user","content":"hi"}]}`, true))
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, recvErr := reader.Recv()
		done <- recvErr
	}()
	select {
	case err := <-done:
		// Closing mid-stream means the completion never finished.
		if !errors.Is(err, providers.ErrStreamTruncated) {
			t.Errorf("Recv after Close = %v, want ErrStreamTruncated", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Recv after Close blocked")
	}
}

func TestMalformedClientBodyIsInvalidRequest(t *testing.T) {
	p := anthropic.New("claude", "http://127.0.0.1:1", "k", nil)

	_, err := p.Chat(context.Background(), &providers.ChatRequest{Model: testModel, Raw: json.RawMessage(`{"messages":`)})
	var pe *providers.ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("error %v is not a *providers.ProviderError", err)
	}
	if pe.Class != providers.ErrClassInvalidRequest {
		t.Errorf("class = %q, want invalid_request", pe.Class)
	}
}
