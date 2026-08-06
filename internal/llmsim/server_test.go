package llmsim

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// The prompt content is 27 chars, so prompt_tokens must be 27/4 = 6.
const (
	chatBody         = `{"model":"gpt-test","messages":[{"role":"user","content":"hello world, tell me things"}]}`
	streamBody       = `{"model":"gpt-test","messages":[{"role":"user","content":"hello world, tell me things"}],"stream":true}`
	wantPromptTokens = 6
)

func newSim(t *testing.T, opts Options) *httptest.Server {
	t.Helper()
	if opts.TimeScale == 0 {
		opts.TimeScale = 0.001
	}
	ts := httptest.NewServer(New(opts))
	t.Cleanup(ts.Close)
	return ts
}

func postChat(t *testing.T, baseURL, body string) *http.Response {
	t.Helper()
	resp, err := http.Post(baseURL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	return resp
}

func decodeChat(t *testing.T, resp *http.Response) chatResponse {
	t.Helper()
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var out chatResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}

func contentOf(t *testing.T, cr chatResponse) string {
	t.Helper()
	if len(cr.Choices) != 1 || cr.Choices[0].Message == nil {
		t.Fatalf("malformed response: %+v", cr)
	}
	return cr.Choices[0].Message.Content
}

// readEvents collects SSE data payloads until the body ends. Read errors are
// returned, not fatal, because cut streams end abruptly by design.
func readEvents(resp *http.Response) ([]string, error) {
	defer resp.Body.Close()
	var events []string
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		payload, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			return events, fmt.Errorf("unexpected SSE line %q", line)
		}
		events = append(events, payload)
	}
	return events, sc.Err()
}

func TestSameSeedSameOutput(t *testing.T) {
	cases := []struct {
		name string
		seed int64
	}{
		{"seed 1", 1},
		{"seed 42", 42},
		{"seed large", 987654321},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := newSim(t, Options{Seed: tc.seed})
			b := newSim(t, Options{Seed: tc.seed})
			for i := 0; i < 2; i++ {
				ra := decodeChat(t, postChat(t, a.URL, chatBody))
				rb := decodeChat(t, postChat(t, b.URL, chatBody))
				if wantID := fmt.Sprintf("sim-%d", i); ra.ID != wantID {
					t.Errorf("id = %q, want %q", ra.ID, wantID)
				}
				if ca, cb := contentOf(t, ra), contentOf(t, rb); ca != cb {
					t.Errorf("request %d: content differs between identically seeded servers", i)
				}
				if ra.Usage == nil || rb.Usage == nil {
					t.Fatalf("request %d: missing usage", i)
				}
				if *ra.Usage != *rb.Usage {
					t.Errorf("request %d: usage differs: %+v vs %+v", i, *ra.Usage, *rb.Usage)
				}
			}
		})
	}
}

func TestDifferentRequestIndexesDiffer(t *testing.T) {
	ts := newSim(t, Options{Seed: 1})
	first := contentOf(t, decodeChat(t, postChat(t, ts.URL, chatBody)))
	second := contentOf(t, decodeChat(t, postChat(t, ts.URL, chatBody)))
	if first == second {
		t.Error("consecutive request indexes produced identical content")
	}
}

func TestConcurrentRequestsDeterministic(t *testing.T) {
	// Arrival order may shuffle which request gets which index, but the set
	// of (id, content) pairs must be identical across identically seeded
	// servers because each index owns its own RNG.
	const parallel = 8
	collect := func(ts *httptest.Server) map[string]string {
		var mu sync.Mutex
		var wg sync.WaitGroup
		got := make(map[string]string, parallel)
		for i := 0; i < parallel; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", strings.NewReader(chatBody))
				if err != nil {
					t.Errorf("post: %v", err)
					return
				}
				defer resp.Body.Close()
				var cr chatResponse
				if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
					t.Errorf("decode: %v", err)
					return
				}
				mu.Lock()
				got[cr.ID] = cr.Choices[0].Message.Content
				mu.Unlock()
			}()
		}
		wg.Wait()
		return got
	}

	a := collect(newSim(t, Options{Seed: 42}))
	b := collect(newSim(t, Options{Seed: 42}))
	if len(a) != parallel {
		t.Fatalf("got %d distinct ids, want %d", len(a), parallel)
	}
	for id, content := range a {
		if b[id] != content {
			t.Errorf("id %s: content differs across identically seeded servers", id)
		}
	}
}

func TestNonStreamResponse(t *testing.T) {
	ts := newSim(t, Options{Seed: 7})
	cr := decodeChat(t, postChat(t, ts.URL, chatBody))

	if cr.Object != objectCompletion {
		t.Errorf("object = %q, want %q", cr.Object, objectCompletion)
	}
	if cr.Model != "gpt-test" {
		t.Errorf("model = %q, want the echoed request model", cr.Model)
	}
	content := contentOf(t, cr)
	if cr.Usage == nil {
		t.Fatal("missing usage")
	}
	if cr.Usage.PromptTokens != wantPromptTokens {
		t.Errorf("prompt_tokens = %d, want %d", cr.Usage.PromptTokens, wantPromptTokens)
	}
	if got := len(strings.Fields(content)); got != cr.Usage.CompletionTokens {
		t.Errorf("content has %d tokens, usage says %d", got, cr.Usage.CompletionTokens)
	}
	if cr.Usage.TotalTokens != cr.Usage.PromptTokens+cr.Usage.CompletionTokens {
		t.Errorf("total_tokens = %d, want prompt + completion", cr.Usage.TotalTokens)
	}
	if fr := cr.Choices[0].FinishReason; fr == nil || *fr != finishStop {
		t.Errorf("finish_reason = %v, want %q", fr, finishStop)
	}
}

func TestStreamSSE(t *testing.T) {
	ts := newSim(t, Options{Seed: 7})
	resp := postChat(t, ts.URL, streamBody)
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	events, err := readEvents(resp)
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	if len(events) < 3 {
		t.Fatalf("got %d events, want at least a token, a usage chunk and [DONE]", len(events))
	}
	if last := events[len(events)-1]; last != doneEvent {
		t.Fatalf("last event = %q, want %q", last, doneEvent)
	}

	var chunks []chatResponse
	for i, ev := range events[:len(events)-1] {
		var c chatResponse
		if err := json.Unmarshal([]byte(ev), &c); err != nil {
			t.Fatalf("event %d is not valid chunk JSON: %v", i, err)
		}
		if c.Object != objectChunk {
			t.Fatalf("event %d object = %q, want %q", i, c.Object, objectChunk)
		}
		if c.ID != "sim-0" {
			t.Fatalf("event %d id = %q, want sim-0", i, c.ID)
		}
		chunks = append(chunks, c)
	}

	final := chunks[len(chunks)-1]
	if final.Usage == nil {
		t.Fatal("final chunk carries no usage")
	}
	if fr := final.Choices[0].FinishReason; fr == nil || *fr != finishStop {
		t.Errorf("final finish_reason = %v, want %q", fr, finishStop)
	}

	var text strings.Builder
	for _, c := range chunks[:len(chunks)-1] {
		if c.Choices[0].Delta == nil || c.Choices[0].Delta.Content == "" {
			t.Fatal("token chunk missing delta content")
		}
		text.WriteString(c.Choices[0].Delta.Content)
	}
	if role := chunks[0].Choices[0].Delta.Role; role != roleAssistant {
		t.Errorf("first chunk delta role = %q, want %q", role, roleAssistant)
	}
	if got := len(chunks) - 1; got != final.Usage.CompletionTokens {
		t.Errorf("streamed %d token chunks, usage says %d", got, final.Usage.CompletionTokens)
	}

	// The same seed and index must yield identical text without streaming.
	plain := newSim(t, Options{Seed: 7})
	if want := contentOf(t, decodeChat(t, postChat(t, plain.URL, chatBody))); text.String() != want {
		t.Errorf("streamed text differs from non-stream text for the same seed")
	}
}

func TestFail429Always(t *testing.T) {
	ts := newSim(t, Options{Seed: 1, Fail429: 1})
	for i := 0; i < 3; i++ {
		resp := postChat(t, ts.URL, chatBody)
		if resp.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("request %d: status = %d, want 429", i, resp.StatusCode)
		}
		if ra := resp.Header.Get("Retry-After"); ra != retryAfterSeconds {
			t.Errorf("request %d: Retry-After = %q, want %q", i, ra, retryAfterSeconds)
		}
		var body errorBody
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatalf("request %d: decode error body: %v", i, err)
		}
		resp.Body.Close()
		if body.Error.Message == "" || body.Error.Type == "" {
			t.Errorf("request %d: incomplete error body: %+v", i, body)
		}
	}
}

func TestFailCutTruncatesStream(t *testing.T) {
	// A control server with no injection reveals the full token count for
	// seed 1 index 0; the cut stream must emit exactly half and no [DONE].
	control := newSim(t, Options{Seed: 1})
	full := decodeChat(t, postChat(t, control.URL, chatBody))
	if full.Usage == nil {
		t.Fatal("control response missing usage")
	}

	ts := newSim(t, Options{Seed: 1, FailCut: 1})
	events, _ := readEvents(postChat(t, ts.URL, streamBody))

	if want := full.Usage.CompletionTokens / 2; len(events) != want {
		t.Errorf("cut stream emitted %d chunks, want %d", len(events), want)
	}
	for i, ev := range events {
		if ev == doneEvent {
			t.Fatal("cut stream must not emit [DONE]")
		}
		var c chatResponse
		if err := json.Unmarshal([]byte(ev), &c); err != nil {
			t.Fatalf("event %d is not valid chunk JSON: %v", i, err)
		}
		if c.Usage != nil {
			t.Fatal("cut stream must not emit a usage chunk")
		}
	}
}

func TestClientDisconnectStopsStream(t *testing.T) {
	// Real-time scale with a slow profile: the full stream would take over
	// ten seconds, so the handler finishing quickly proves it saw the cancel.
	slow := Profile{
		Name:         "slow",
		TTFT:         Dist{Mean: 5, P95: 6},
		ITL:          Dist{Mean: 300, P95: 301},
		OutputTokens: Dist{Mean: 40, P95: 41},
	}
	sim := New(Options{Seed: 1, Profile: slow, TimeScale: 1})
	done := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sim.ServeHTTP(w, r)
		close(done)
	}))
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ts.URL+"/v1/chat/completions", strings.NewReader(streamBody))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	if _, err := bufio.NewReader(resp.Body).ReadString('\n'); err != nil {
		t.Fatalf("read first event: %v", err)
	}
	cancel()
	resp.Body.Close()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("handler kept streaming after client disconnect")
	}
}

func TestHealthzAndModels(t *testing.T) {
	ts := newSim(t, Options{Seed: 1})

	t.Run("healthz", func(t *testing.T) {
		resp, err := http.Get(ts.URL + "/healthz")
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("status = %d, want 200", resp.StatusCode)
		}
	})

	t.Run("models", func(t *testing.T) {
		resp, err := http.Get(ts.URL + "/v1/models")
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		defer resp.Body.Close()
		var out struct {
			Object string `json:"object"`
			Data   []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if out.Object != "list" || len(out.Data) != 1 || out.Data[0].ID != DefaultProfile.Name {
			t.Errorf("unexpected models payload: %+v", out)
		}
	})
}

func TestBadRequestBody(t *testing.T) {
	ts := newSim(t, Options{Seed: 1})
	resp := postChat(t, ts.URL, "not json")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestOversizedRequestBodyRejected(t *testing.T) {
	ts := newSim(t, Options{Seed: 1})
	// Valid JSON shape whose content pushes the body past the 1 MiB cap.
	body := `{"model":"gpt-test","messages":[{"role":"user","content":"` +
		strings.Repeat("a", int(maxRequestBodyBytes)+1) + `"}]}`

	resp := postChat(t, ts.URL, body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", resp.StatusCode)
	}
	var eb errorBody
	if err := json.NewDecoder(resp.Body).Decode(&eb); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if !strings.Contains(eb.Error.Message, fmt.Sprint(maxRequestBodyBytes)) {
		t.Errorf("error message %q does not name the byte limit", eb.Error.Message)
	}
}

func TestHangSaturationSheds503(t *testing.T) {
	// TimeScale 1 keeps the single permitted hang pinned for the whole
	// test; the second request must be shed immediately, not queued.
	sim := New(Options{Seed: 1, FailHang: 1, MaxConcurrentHangs: 1, TimeScale: 1})
	ts := httptest.NewServer(sim)
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ts.URL+"/v1/chat/completions", strings.NewReader(chatBody))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	go func() {
		// Hangs until cancel releases it at test end.
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			resp.Body.Close()
		}
	}()

	// The hanging request owns the slot once the semaphore fills.
	deadline := time.Now().Add(3 * time.Second)
	for len(sim.hangSlots) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("first request never occupied the hang slot")
		}
		time.Sleep(time.Millisecond)
	}

	resp := postChat(t, ts.URL, chatBody)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 while hang slots are saturated", resp.StatusCode)
	}
	var eb errorBody
	if err := json.NewDecoder(resp.Body).Decode(&eb); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if eb.Error.Message == "" || eb.Error.Type == "" {
		t.Errorf("incomplete error body: %+v", eb)
	}
}
