package gemini

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
)

// capturedRequest is what the test upstream saw. It travels back over a
// channel rather than through shared memory so the race detector has a
// real happens-before edge to follow.
type capturedRequest struct {
	path   string
	query  string
	rawURL string
	apiKey string
	body   string
}

// upstream serves respond to any request and publishes the request it
// received.
func upstream(t *testing.T, respond string) (*httptest.Server, <-chan capturedRequest) {
	t.Helper()
	seen := make(chan capturedRequest, 1)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		select {
		case seen <- capturedRequest{
			path:   r.URL.Path,
			query:  r.URL.RawQuery,
			rawURL: r.URL.String(),
			apiKey: r.Header.Get(authHeader),
			body:   string(body),
		}:
		default:
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, respond)
	}))
	t.Cleanup(ts.Close)
	return ts, seen
}

func awaitRequest(t *testing.T, seen <-chan capturedRequest) capturedRequest {
	t.Helper()
	select {
	case got := <-seen:
		return got
	case <-time.After(5 * time.Second):
		t.Fatal("upstream never saw a request")
		return capturedRequest{}
	}
}

// drainStream reads a stream to its terminal error, assembling the
// assistant text out of the OpenAI shaped deltas.
func drainStream(t *testing.T, r providers.StreamReader) (content string, usage *providers.Usage, err error) {
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
		var envelope chatCompletion
		if jsonErr := json.Unmarshal(chunk.Data, &envelope); jsonErr != nil {
			t.Fatalf("chunk is not valid JSON: %v (%s)", jsonErr, chunk.Data)
		}
		for _, c := range envelope.Choices {
			if c.Delta != nil {
				sb.WriteString(c.Delta.Content)
			}
		}
	}
}

func streamOnce(t *testing.T, body string) (providers.StreamReader, func()) {
	t.Helper()
	ts, _ := upstream(t, body)
	p := New("test", ts.URL, "k", nil)
	reader, err := p.ChatStream(context.Background(), &providers.ChatRequest{
		Model:  "gemini-2.0-flash",
		Stream: true,
		Raw:    json.RawMessage(`{"model":"gemini-2.0-flash","stream":true,"messages":[{"role":"user","content":"hi"}]}`),
	})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	return reader, func() { _ = reader.Close() }
}

func TestTranslateRequest(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "user message becomes a content entry",
			in:   `{"model":"gemini-2.0-flash","messages":[{"role":"user","content":"hello"}]}`,
			want: `{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`,
		},
		{
			name: "system message becomes systemInstruction rather than a content entry",
			in: `{"model":"gemini-2.0-flash","messages":[` +
				`{"role":"system","content":"Be terse."},{"role":"user","content":"hello"}]}`,
			want: `{"contents":[{"role":"user","parts":[{"text":"hello"}]}],` +
				`"systemInstruction":{"parts":[{"text":"Be terse."}]}}`,
		},
		{
			name: "developer role is a system message too",
			in:   `{"model":"m","messages":[{"role":"developer","content":"Be terse."},{"role":"user","content":"hi"}]}`,
			want: `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"systemInstruction":{"parts":[{"text":"Be terse."}]}}`,
		},
		{
			name: "several system messages concatenate instead of the last one winning",
			in: `{"model":"m","messages":[{"role":"system","content":"One."},` +
				`{"role":"system","content":"Two."},{"role":"user","content":"hi"}]}`,
			want: `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],` +
				`"systemInstruction":{"parts":[{"text":"One."},{"text":"Two."}]}}`,
		},
		{
			name: "assistant maps to the model role",
			in: `{"model":"m","messages":[{"role":"user","content":"hi"},` +
				`{"role":"assistant","content":"hey"},{"role":"user","content":"more"}]}`,
			want: `{"contents":[{"role":"user","parts":[{"text":"hi"}]},` +
				`{"role":"model","parts":[{"text":"hey"}]},{"role":"user","parts":[{"text":"more"}]}]}`,
		},
		{
			name: "unknown role degrades to user so its text is not lost",
			in:   `{"model":"m","messages":[{"role":"function","content":"result"}]}`,
			want: `{"contents":[{"role":"user","parts":[{"text":"result"}]}]}`,
		},
		{
			name: "sampling and limit fields become generationConfig",
			in: `{"model":"m","messages":[{"role":"user","content":"hi"}],` +
				`"temperature":0.5,"top_p":0.9,"max_tokens":128,"stop":["END","STOP"]}`,
			want: `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],` +
				`"generationConfig":{"temperature":0.5,"topP":0.9,"maxOutputTokens":128,"stopSequences":["END","STOP"]}}`,
		},
		{
			name: "a bare string stop becomes a one element stopSequences",
			in:   `{"model":"m","messages":[{"role":"user","content":"hi"}],"stop":"END"}`,
			want: `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"stopSequences":["END"]}}`,
		},
		{
			name: "temperature zero survives, since omitting it would change the sampling",
			in:   `{"model":"m","messages":[{"role":"user","content":"hi"}],"temperature":0}`,
			want: `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"temperature":0}}`,
		},
		{
			name: "fields Gemini would reject are dropped",
			in: `{"model":"m","messages":[{"role":"user","content":"hi"}],` +
				`"n":2,"presence_penalty":0.3,"user":"u-1","logit_bias":{"1":2},"response_format":{"type":"json_object"}}`,
			want: `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`,
		},
		{
			name: "multimodal content keeps its text parts",
			in: `{"model":"m","messages":[{"role":"user","content":[` +
				`{"type":"text","text":"look: "},{"type":"image_url","image_url":{"url":"https://x/y.png"}},` +
				`{"type":"text","text":"what is it?"}]}]}`,
			want: `{"contents":[{"role":"user","parts":[{"text":"look: what is it?"}]}]}`,
		},
		{
			name: "no messages still sends a contents array, which Gemini requires",
			in:   `{"model":"m"}`,
			want: `{"contents":[]}`,
		},
		{
			// Dropping this would hand the caller Gemini's default cap
			// instead of the ceiling they asked for, and they pay for it.
			name: "max_completion_tokens is honored as the newer spelling",
			in:   `{"model":"m","messages":[{"role":"user","content":"hi"}],"max_completion_tokens":64}`,
			want: `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"maxOutputTokens":64}}`,
		},
		{
			name: "max_tokens wins when a client sends both spellings",
			in: `{"model":"m","messages":[{"role":"user","content":"hi"}],` +
				`"max_tokens":32,"max_completion_tokens":64}`,
			want: `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"maxOutputTokens":32}}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := translateRequest(json.RawMessage(tt.in))
			if err != nil {
				t.Fatalf("translateRequest: %v", err)
			}
			got, err := json.Marshal(out)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if string(got) != tt.want {
				t.Errorf("translated body =\n  %s\nwant\n  %s", got, tt.want)
			}
		})
	}
}

func TestTranslateRequestRejectsMalformedBody(t *testing.T) {
	if _, err := translateRequest(json.RawMessage(`{"messages":`)); err == nil {
		t.Fatal("translateRequest accepted a truncated body")
	}
}

func TestMapFinishReason(t *testing.T) {
	tests := []struct {
		gemini string
		want   string
	}{
		{gemini: "STOP", want: "stop"},
		{gemini: "MAX_TOKENS", want: "length"},
		{gemini: "SAFETY", want: "content_filter"},
		{gemini: "RECITATION", want: "content_filter"},
		{gemini: "BLOCKLIST", want: "content_filter"},
		{gemini: "PROHIBITED_CONTENT", want: "content_filter"},
		{gemini: "SPII", want: "content_filter"},
		{gemini: "IMAGE_SAFETY", want: "content_filter"},
		// Mid-stream candidates carry no reason, and OpenAI wants null
		// there rather than an invented one.
		{gemini: "", want: ""},
		// The OpenAI enum has no bucket for anything else.
		{gemini: "OTHER", want: "stop"},
		{gemini: "LANGUAGE", want: "stop"},
	}

	for _, tt := range tests {
		t.Run(tt.gemini+"_to_"+tt.want, func(t *testing.T) {
			if got := mapFinishReason(tt.gemini); got != tt.want {
				t.Errorf("mapFinishReason(%q) = %q, want %q", tt.gemini, got, tt.want)
			}
		})
	}
}

func TestTranslateCompletion(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantText   string
		wantFinish string
		wantModel  string
		wantUsage  providers.Usage
	}{
		{
			name: "parts concatenate and STOP maps to stop",
			body: `{"candidates":[{"content":{"parts":[{"text":"a"},{"text":"b"}],"role":"model"},"finishReason":"STOP","index":0}],` +
				`"usageMetadata":{"promptTokenCount":4,"candidatesTokenCount":2,"totalTokenCount":6},"modelVersion":"gemini-2.0-flash-001"}`,
			wantText:   "ab",
			wantFinish: "stop",
			wantModel:  "gemini-2.0-flash-001",
			wantUsage:  providers.Usage{PromptTokens: 4, CompletionTokens: 2, TotalTokens: 6},
		},
		{
			name: "MAX_TOKENS maps to length",
			body: `{"candidates":[{"content":{"parts":[{"text":"trunc"}],"role":"model"},"finishReason":"MAX_TOKENS","index":0}],` +
				`"usageMetadata":{"promptTokenCount":4,"candidatesTokenCount":8,"totalTokenCount":12}}`,
			wantText:   "trunc",
			wantFinish: "length",
			wantModel:  "req-model",
			wantUsage:  providers.Usage{PromptTokens: 4, CompletionTokens: 8, TotalTokens: 12},
		},
		{
			name: "SAFETY maps to content_filter",
			body: `{"candidates":[{"content":{"parts":[],"role":"model"},"finishReason":"SAFETY","index":0}],` +
				`"usageMetadata":{"promptTokenCount":4,"candidatesTokenCount":0,"totalTokenCount":4}}`,
			wantText:   "",
			wantFinish: "content_filter",
			wantModel:  "req-model",
			wantUsage:  providers.Usage{PromptTokens: 4, TotalTokens: 4},
		},
		{
			name: "a blocked prompt has no candidates at all",
			body: `{"promptFeedback":{"blockReason":"SAFETY","safetyRatings":[{"category":"HARM_CATEGORY_HARASSMENT","probability":"HIGH"}]},` +
				`"usageMetadata":{"promptTokenCount":11,"totalTokenCount":11}}`,
			wantText:   "",
			wantFinish: "content_filter",
			wantModel:  "req-model",
			wantUsage:  providers.Usage{PromptTokens: 11, TotalTokens: 11},
		},
		{
			name:       "a response without usageMetadata reports zero rather than failing",
			body:       `{"candidates":[{"content":{"parts":[{"text":"hi"}],"role":"model"},"finishReason":"STOP","index":0}]}`,
			wantText:   "hi",
			wantFinish: "stop",
			wantModel:  "req-model",
			wantUsage:  providers.Usage{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw, usage, err := translateCompletion([]byte(tt.body), "chatcmpl-fixed", "req-model")
			if err != nil {
				t.Fatalf("translateCompletion: %v", err)
			}
			if usage != tt.wantUsage {
				t.Errorf("usage = %+v, want %+v", usage, tt.wantUsage)
			}

			var got chatCompletion
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatalf("translated body is not JSON: %v", err)
			}
			if got.Object != objectCompletion {
				t.Errorf("object = %q, want %q", got.Object, objectCompletion)
			}
			if got.ID != "chatcmpl-fixed" {
				t.Errorf("id = %q, want the synthesized one", got.ID)
			}
			if got.Model != tt.wantModel {
				t.Errorf("model = %q, want %q", got.Model, tt.wantModel)
			}
			if len(got.Choices) != 1 {
				t.Fatalf("choices = %d, want 1", len(got.Choices))
			}
			if got.Choices[0].Message == nil {
				t.Fatal("choice carries no message")
			}
			if got.Choices[0].Message.Role != openAIRoleAssistant {
				t.Errorf("role = %q, want assistant", got.Choices[0].Message.Role)
			}
			if got.Choices[0].Message.Content != tt.wantText {
				t.Errorf("content = %q, want %q", got.Choices[0].Message.Content, tt.wantText)
			}
			if got.Choices[0].FinishReason == nil || *got.Choices[0].FinishReason != tt.wantFinish {
				t.Errorf("finish_reason = %v, want %q", got.Choices[0].FinishReason, tt.wantFinish)
			}
			if got.Usage == nil || *got.Usage != *usageToJSON(tt.wantUsage) {
				t.Errorf("usage block = %+v, want %+v", got.Usage, usageToJSON(tt.wantUsage))
			}
		})
	}
}

func TestNewCompletionIDIsUnique(t *testing.T) {
	first, second := newCompletionID(), newCompletionID()
	if !strings.HasPrefix(first, completionIDPrefix) {
		t.Errorf("id %q lacks the %q prefix", first, completionIDPrefix)
	}
	if first == second {
		t.Error("two completions were given the same id")
	}
}

// TestStreamEndsWithoutFinishReason is the behavior that separates this
// adapter from an OpenAI-wire one. Gemini sends no [DONE], so a body that
// simply stops is byte for byte what a severed upstream looks like; only
// a finishReason proves the turn finished.
func TestStreamEndsWithoutFinishReason(t *testing.T) {
	tests := []struct {
		name string
		body string
		want error
	}{
		{
			name: "no finishReason anywhere is truncation",
			body: `data: {"candidates":[{"content":{"parts":[{"text":"par"}],"role":"model"},"index":0}]}` + "\n\n",
			want: providers.ErrStreamTruncated,
		},
		{
			name: "an empty finishReason does not count",
			body: `data: {"candidates":[{"content":{"parts":[{"text":"par"}],"role":"model"},"finishReason":"","index":0}]}` + "\n\n",
			want: providers.ErrStreamTruncated,
		},
		{
			name: "usage on the last event is not a completeness signal either",
			body: `data: {"candidates":[{"content":{"parts":[{"text":"par"}],"role":"model"},"index":0}],` +
				`"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":1,"totalTokenCount":4}}` + "\n\n",
			want: providers.ErrStreamTruncated,
		},
		{
			name: "an empty body is truncation, not an empty answer",
			body: "",
			want: providers.ErrStreamTruncated,
		},
		{
			name: "a finishReason on the last event completes the stream",
			body: `data: {"candidates":[{"content":{"parts":[{"text":"whole"}],"role":"model"},"finishReason":"STOP","index":0}]}` + "\n\n",
			want: io.EOF,
		},
		{
			name: "a finishReason mid-body still completes it, since usage may trail",
			body: `data: {"candidates":[{"content":{"parts":[{"text":"whole"}],"role":"model"},"finishReason":"STOP","index":0}]}` + "\n\n" +
				`data: {"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":1,"totalTokenCount":4}}` + "\n\n",
			want: io.EOF,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reader, cleanup := streamOnce(t, tt.body)
			defer cleanup()

			_, _, err := drainStream(t, reader)
			if !errors.Is(err, tt.want) {
				t.Fatalf("stream ended with %v, want %v", err, tt.want)
			}
		})
	}
}

func TestStreamUsageComesFromTheLastEventCarryingIt(t *testing.T) {
	reader, cleanup := streamOnce(t,
		`data: {"candidates":[{"content":{"parts":[{"text":"a"}],"role":"model"},"index":0}],`+
			`"usageMetadata":{"promptTokenCount":7,"candidatesTokenCount":1,"totalTokenCount":8}}`+"\n\n"+
			`data: {"candidates":[{"content":{"parts":[{"text":"b"}],"role":"model"},"finishReason":"STOP","index":0}],`+
			`"usageMetadata":{"promptTokenCount":7,"candidatesTokenCount":2,"totalTokenCount":9}}`+"\n\n")
	defer cleanup()

	content, usage, err := drainStream(t, reader)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("stream ended with %v, want io.EOF", err)
	}
	if content != "ab" {
		t.Errorf("content = %q, want %q", content, "ab")
	}
	if usage == nil {
		t.Fatal("no usage reported")
	}
	if want := (providers.Usage{PromptTokens: 7, CompletionTokens: 2, TotalTokens: 9}); *usage != want {
		t.Errorf("usage = %+v, want %+v", *usage, want)
	}
}

func TestStreamChunkShape(t *testing.T) {
	reader, cleanup := streamOnce(t,
		": ping\n\n"+
			`data: {"candidates":[{"content":{"parts":[{"text":"Hi"}],"role":"model"},"index":0}],"modelVersion":"gemini-2.0-flash"}`+"\n\n"+
			`data: {"candidates":[{"content":{"parts":[{"text":"!"}],"role":"model"},"finishReason":"MAX_TOKENS","index":0}]}`+"\n\n")
	defer cleanup()

	first, err := reader.Recv()
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	if !first.Keepalive {
		t.Error("an SSE comment must surface as a keepalive")
	}

	var chunks []chatCompletion
	for {
		c, recvErr := reader.Recv()
		if recvErr != nil {
			if !errors.Is(recvErr, io.EOF) {
				t.Fatalf("Recv: %v", recvErr)
			}
			break
		}
		var decoded chatCompletion
		if err := json.Unmarshal(c.Data, &decoded); err != nil {
			t.Fatalf("chunk is not JSON: %v", err)
		}
		chunks = append(chunks, decoded)
	}

	if len(chunks) != 2 {
		t.Fatalf("got %d chunks, want 2", len(chunks))
	}
	for i, c := range chunks {
		if c.Object != objectChunk {
			t.Errorf("chunk %d object = %q, want %q", i, c.Object, objectChunk)
		}
		if c.ID != chunks[0].ID {
			t.Errorf("chunk %d id = %q, want the stream to keep one id", i, c.ID)
		}
	}
	// OpenAI announces the speaker once, on the first chunk.
	if chunks[0].Choices[0].Delta.Role != openAIRoleAssistant {
		t.Errorf("first delta role = %q, want assistant", chunks[0].Choices[0].Delta.Role)
	}
	if chunks[1].Choices[0].Delta.Role != "" {
		t.Errorf("second delta repeats the role %q", chunks[1].Choices[0].Delta.Role)
	}
	if chunks[0].Choices[0].FinishReason != nil {
		t.Errorf("mid-stream finish_reason = %v, want null", *chunks[0].Choices[0].FinishReason)
	}
	if chunks[1].Choices[0].FinishReason == nil || *chunks[1].Choices[0].FinishReason != openAIFinishLength {
		t.Errorf("final finish_reason = %v, want %q", chunks[1].Choices[0].FinishReason, openAIFinishLength)
	}
	if chunks[0].Model != "gemini-2.0-flash" {
		t.Errorf("model = %q, want the version Gemini reported", chunks[0].Model)
	}
}

// TestStreamSurfacesMidStreamError covers Gemini reporting a failure as
// an ordinary SSE event after it already answered 200.
func TestStreamSurfacesMidStreamError(t *testing.T) {
	reader, cleanup := streamOnce(t,
		`data: {"candidates":[{"content":{"parts":[{"text":"a"}],"role":"model"},"index":0}]}`+"\n\n"+
			`data: {"error":{"code":429,"message":"Resource has been exhausted.","status":"RESOURCE_EXHAUSTED"}}`+"\n\n")
	defer cleanup()

	_, _, err := drainStream(t, reader)
	var pe *providers.ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("stream ended with %v, want a *providers.ProviderError", err)
	}
	if pe.Class != providers.ErrClassRateLimited {
		t.Errorf("class = %q, want %q", pe.Class, providers.ErrClassRateLimited)
	}
	if pe.StatusCode != http.StatusTooManyRequests {
		t.Errorf("StatusCode = %d, want 429", pe.StatusCode)
	}
}

func TestStreamRejectsUnparsableEvent(t *testing.T) {
	reader, cleanup := streamOnce(t, "data: not json at all\n\n")
	defer cleanup()

	_, _, err := drainStream(t, reader)
	var pe *providers.ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("stream ended with %v, want a *providers.ProviderError", err)
	}
	if pe.Class != providers.ErrClassUpstream {
		t.Errorf("class = %q, want %q", pe.Class, providers.ErrClassUpstream)
	}
}

func TestStreamBoundsOneEvent(t *testing.T) {
	// A single data line that never terminates must not be buffered
	// without limit.
	oversized := "data: {\"x\":\"" + strings.Repeat("a", maxEventBytes+1024) + "\"}\n\n"
	reader, cleanup := streamOnce(t, oversized)
	defer cleanup()

	_, _, err := drainStream(t, reader)
	var pe *providers.ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("stream ended with %v, want a *providers.ProviderError", err)
	}
	if !errors.Is(err, errEventTooLarge) {
		t.Errorf("error %v does not wrap errEventTooLarge", err)
	}
}

func TestRequestTargetsTheGeminiEndpoint(t *testing.T) {
	tests := []struct {
		name      string
		stream    bool
		model     string
		wantPath  string
		wantQuery string
	}{
		{
			name:     "non stream uses generateContent",
			model:    "gemini-2.0-flash",
			wantPath: "/models/gemini-2.0-flash:generateContent",
		},
		{
			name:      "stream uses streamGenerateContent with alt=sse",
			stream:    true,
			model:     "gemini-2.0-flash",
			wantPath:  "/models/gemini-2.0-flash:streamGenerateContent",
			wantQuery: "alt=sse",
		},
		{
			name:     "a fully qualified model name is not doubled up",
			model:    "models/gemini-2.5-pro",
			wantPath: "/models/gemini-2.5-pro:generateContent",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts, seen := upstream(t,
				`{"candidates":[{"content":{"parts":[{"text":"ok"}],"role":"model"},"finishReason":"STOP","index":0}]}`+"\n\n")

			p := New("test", ts.URL, "secret-key", nil)
			req := &providers.ChatRequest{
				Model:  tt.model,
				Stream: tt.stream,
				Raw:    json.RawMessage(`{"model":"x","messages":[{"role":"user","content":"hi"}]}`),
			}
			var err error
			if tt.stream {
				var reader providers.StreamReader
				reader, err = p.ChatStream(context.Background(), req)
				if reader != nil {
					_ = reader.Close()
				}
			} else {
				_, err = p.Chat(context.Background(), req)
			}
			if err != nil {
				t.Fatalf("call: %v", err)
			}

			got := awaitRequest(t, seen)
			if got.path != tt.wantPath {
				t.Errorf("path = %q, want %q", got.path, tt.wantPath)
			}
			if got.query != tt.wantQuery {
				t.Errorf("query = %q, want %q", got.query, tt.wantQuery)
			}
			if got.apiKey != "secret-key" {
				t.Errorf("%s = %q, want the raw key", authHeader, got.apiKey)
			}
			// The key in a query string would land in access logs and
			// span attributes.
			if strings.Contains(got.rawURL, "secret-key") {
				t.Errorf("the api key reached the URL: %s", got.rawURL)
			}
		})
	}
}

func TestRequestBodyIsTranslated(t *testing.T) {
	ts, seen := upstream(t,
		`data: {"candidates":[{"content":{"parts":[{"text":"ok"}],"role":"model"},"finishReason":"STOP","index":0}]}`+"\n\n")

	p := New("test", ts.URL, "k", nil)
	reader, err := p.ChatStream(context.Background(), &providers.ChatRequest{
		Model:  "gemini-2.0-flash",
		Stream: true,
		Raw: json.RawMessage(`{"model":"gemini-2.0-flash","stream":true,"temperature":0.2,` +
			`"messages":[{"role":"system","content":"Be terse."},{"role":"user","content":"hi"}]}`),
	})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if _, _, err := drainStream(t, reader); !errors.Is(err, io.EOF) {
		t.Fatalf("stream ended with %v", err)
	}
	_ = reader.Close()

	body := awaitRequest(t, seen).body
	want := `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],` +
		`"systemInstruction":{"parts":[{"text":"Be terse."}]},` +
		`"generationConfig":{"temperature":0.2}}`
	if body != want {
		t.Errorf("upstream body =\n  %s\nwant\n  %s", body, want)
	}
	// stream and model live in the URL; passing them on would make
	// Gemini reject the call for unknown fields.
	if strings.Contains(body, `"stream"`) || strings.Contains(body, `"model"`) {
		t.Errorf("unmapped OpenAI fields leaked into the Gemini body: %s", body)
	}
}

func TestChatRejectsMalformedClientBody(t *testing.T) {
	p := New("test", "http://127.0.0.1:1", "k", nil)
	_, err := p.Chat(context.Background(), &providers.ChatRequest{Model: "m", Raw: json.RawMessage(`{"messages":`)})
	var pe *providers.ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("Chat = %v, want a *providers.ProviderError", err)
	}
	if pe.Class != providers.ErrClassInvalidRequest {
		t.Errorf("class = %q, want %q", pe.Class, providers.ErrClassInvalidRequest)
	}
}

func TestChatRejectsAnUnreadableSuccessBody(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = io.WriteString(w, "<html>a proxy interstitial</html>")
	}))
	defer ts.Close()

	p := New("test", ts.URL, "k", nil)
	_, err := p.Chat(context.Background(), &providers.ChatRequest{
		Model: "m",
		Raw:   json.RawMessage(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`),
	})
	var pe *providers.ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("Chat = %v, want a *providers.ProviderError", err)
	}
	if pe.Class != providers.ErrClassUpstream {
		t.Errorf("class = %q, want %q", pe.Class, providers.ErrClassUpstream)
	}
}

func TestClassify(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   providers.ErrorClass
	}{
		{
			name:   "the rpc status wins when a proxy rewrote the http status",
			status: http.StatusOK,
			body:   `{"error":{"code":429,"message":"quota","status":"RESOURCE_EXHAUSTED"}}`,
			want:   providers.ErrClassRateLimited,
		},
		{
			name:   "UNAUTHENTICATED is auth",
			status: http.StatusBadRequest,
			body:   `{"error":{"code":400,"message":"bad key","status":"UNAUTHENTICATED"}}`,
			want:   providers.ErrClassAuth,
		},
		{
			name:   "PERMISSION_DENIED is auth",
			status: http.StatusForbidden,
			body:   `{"error":{"code":403,"message":"denied","status":"PERMISSION_DENIED"}}`,
			want:   providers.ErrClassAuth,
		},
		{
			name:   "an envelope 404 is a missing model",
			status: http.StatusNotFound,
			body:   `{"error":{"code":404,"message":"not found","status":"NOT_FOUND"}}`,
			want:   providers.ErrClassModelNotFound,
		},
		{
			name:   "a bare 404 is a mistyped base_url, not a missing model",
			status: http.StatusNotFound,
			body:   "404 page not found",
			want:   providers.ErrClassUpstream,
		},
		{
			name:   "an unknown rpc status falls back to the http status",
			status: http.StatusInternalServerError,
			body:   `{"error":{"code":500,"message":"boom","status":"INTERNAL"}}`,
			want:   providers.ErrClassUpstream,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := []byte(tt.body)
			if got := classify(tt.status, body, parseErrorEnvelope(body)); got != tt.want {
				t.Errorf("classify = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestUpstreamErrorMessage(t *testing.T) {
	tests := []struct {
		name   string
		body   string
		status int
		want   string
	}{
		{
			name:   "the envelope message is preferred",
			body:   `{"error":{"code":400,"message":"bad request","status":"INVALID_ARGUMENT"}}`,
			status: http.StatusBadRequest,
			want:   "bad request",
		},
		{
			name:   "non envelope bodies fall back to their text",
			body:   "upstream exploded",
			status: http.StatusBadGateway,
			want:   "upstream exploded",
		},
		{
			name:   "an empty body falls back to the status",
			body:   "",
			status: http.StatusBadGateway,
			want:   "upstream returned status 502",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := []byte(tt.body)
			if got := upstreamErrorMessage(body, parseErrorEnvelope(body), tt.status); got != tt.want {
				t.Errorf("upstreamErrorMessage = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestErrorBodyIsBounded(t *testing.T) {
	// A hostile upstream must not be able to push an unbounded error body
	// into the gateway's memory or its logs.
	flood := strings.Repeat("z", 4*maxErrorBody)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, flood)
	}))
	defer ts.Close()

	p := New("test", ts.URL, "k", nil)
	_, err := p.Chat(context.Background(), &providers.ChatRequest{
		Model: "m",
		Raw:   json.RawMessage(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`),
	})
	var pe *providers.ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("Chat = %v, want a *providers.ProviderError", err)
	}
	if len(pe.Message) > maxErrorBody {
		t.Errorf("error message is %d bytes, want at most %d", len(pe.Message), maxErrorBody)
	}
}

func TestNewDefaultsTheBaseURL(t *testing.T) {
	p, ok := New("test", "", "k", nil).(*provider)
	if !ok {
		t.Fatal("New did not return the gemini provider")
	}
	if p.baseURL != DefaultBaseURL {
		t.Errorf("baseURL = %q, want %q", p.baseURL, DefaultBaseURL)
	}
	if p.Name() != "test" {
		t.Errorf("Name = %q, want test", p.Name())
	}

	trimmed, ok := New("test", "https://example.com/v1beta/", "k", nil).(*provider)
	if !ok {
		t.Fatal("New did not return the gemini provider")
	}
	if trimmed.baseURL != "https://example.com/v1beta" {
		t.Errorf("baseURL = %q, want the trailing slash gone", trimmed.baseURL)
	}
}

func TestFromConfigRegistersTheGeminiKind(t *testing.T) {
	built, err := providers.BuildAll([]config.ProviderConfig{
		{Name: "g", Kind: config.KindGemini, APIKey: "k"},
	})
	if err != nil {
		t.Fatalf("BuildAll: %v", err)
	}
	p, ok := built["g"].(*provider)
	if !ok {
		t.Fatalf("kind %q built %T, want the gemini adapter", config.KindGemini, built["g"])
	}
	if p.baseURL != DefaultBaseURL {
		t.Errorf("baseURL = %q, want the default when config leaves it empty", p.baseURL)
	}
}
