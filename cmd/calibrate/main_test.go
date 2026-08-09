package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rajeev-chaurasia/pen-stock/internal/llmsim"
)

// The recording half of this command talks to a paid provider, so
// nothing here goes near a network. The stream parser is exercised
// against a local httptest server and the aggregation is pure.

func TestStats(t *testing.T) {
	cases := []struct {
		name     string
		values   []float64
		wantMean float64
		wantP95  float64
	}{
		// The default sample count. Nearest rank puts p95 on the slowest
		// sample; a floor-based index would land on the second slowest and
		// quietly publish a faster tail than the provider actually has.
		{"eight samples", []float64{1, 2, 3, 4, 5, 6, 7, 8}, 4.5, 8},
		{"twenty samples", seq(20), 10.5, 19},
		// One sample must not index out of range or collapse to zero.
		{"single sample", []float64{42}, 42, 42},
		// Samples arrive in recording order, not sorted order.
		{"unsorted input", []float64{9, 1, 5, 3}, 4.5, 9},
		// Profiles get read and diffed by hand, so the numbers are rounded.
		{"rounds to two places", []float64{1, 1, 2}, 1.33, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := stats(tc.values)
			if got["mean"] != tc.wantMean {
				t.Errorf("mean = %v, want %v", got["mean"], tc.wantMean)
			}
			if got["p95"] != tc.wantP95 {
				t.Errorf("p95 = %v, want %v", got["p95"], tc.wantP95)
			}
		})
	}
}

// llmsim rejects a profile whose p95 is below its mean, so the two must
// never cross no matter how the samples are shaped.
func TestStatsP95NeverBelowMean(t *testing.T) {
	cases := [][]float64{
		{1, 1, 1, 1, 1},
		{0.1, 900, 900, 900, 900, 900},
		{5, 4, 3, 2, 1},
		seq(97),
	}
	for i, values := range cases {
		got := stats(values)
		if got["p95"] < got["mean"] {
			t.Errorf("case %d: p95 %v below mean %v", i, got["p95"], got["mean"])
		}
	}
}

func TestStatsEmpty(t *testing.T) {
	got := stats(nil)
	if got["mean"] != 0 || got["p95"] != 0 {
		t.Errorf("stats(nil) = %v, want zeros", got)
	}
}

// The whole point of the command is producing something llmsim can
// replay, and the JSON field names are the only thing holding the two
// halves together.
func TestProfileRoundTripsThroughLLMSim(t *testing.T) {
	ttfts := []float64{300, 350, 400, 900}
	itls := []float64{5, 6, 7, 8, 40}
	tokens := []float64{39, 39, 39, 39}

	profile := buildProfile("groq-llama", ttfts, itls, tokens, map[string]any{
		"model":       "llama-3.3-70b-versatile",
		"samples":     4,
		"recorded_at": "2026-08-06T20:09:30Z",
	})
	path := writeProfile(t, profile)

	loaded, err := llmsim.LoadProfile(path)
	if err != nil {
		t.Fatalf("LoadProfile: %v", err)
	}
	if loaded.Name != "groq-llama" {
		t.Errorf("name = %q, want groq-llama", loaded.Name)
	}
	want := []struct {
		field string
		got   llmsim.Dist
		src   []float64
	}{
		{"ttft_ms", loaded.TTFT, ttfts},
		{"itl_ms", loaded.ITL, itls},
		{"output_tokens", loaded.OutputTokens, tokens},
	}
	for _, w := range want {
		s := stats(w.src)
		if w.got.Mean != s["mean"] || w.got.P95 != s["p95"] {
			t.Errorf("%s = %+v, want mean %v p95 %v", w.field, w.got, s["mean"], s["p95"])
		}
	}

	// Provenance is what separates a recorded profile from an invented
	// one, so it has to survive serialisation.
	var raw map[string]any
	data, err := os.ReadFile(path) // #nosec G304
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	recorded, ok := raw["recorded"].(map[string]any)
	if !ok {
		t.Fatalf("recorded block missing from %s", data)
	}
	if recorded["model"] != "llama-3.3-70b-versatile" || recorded["recorded_at"] != "2026-08-06T20:09:30Z" {
		t.Errorf("provenance lost: %v", recorded)
	}
}

// The second line of defence. run refuses a sample count below one, so
// a profile of zeros should never be written in the first place, but a
// profile that claims a model answers instantly would poison every
// benchmark that replayed it. llmsim refusing to load one means the
// failure surfaces as an error rather than as numbers.
func TestProfileFromNoSamplesIsRejectedByLLMSim(t *testing.T) {
	path := writeProfile(t, buildProfile("empty", nil, nil, nil, map[string]any{}))
	if _, err := llmsim.LoadProfile(path); err == nil {
		t.Fatal("expected llmsim to reject an all-zero profile")
	}
}

func TestRecordCountsOnlyContentFrames(t *testing.T) {
	// Buffered so the handler never blocks, and so the test reads the
	// path through a channel rather than racing the handler goroutine.
	pathCh := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pathCh <- r.URL.Path
		flusher := sseHeader(w)
		// A role-only opener and an empty delta carry no text. Counting
		// them would inflate the token count and invent a gap that no
		// token ever waited for.
		writeFrame(w, flusher, `{"choices":[{"delta":{"role":"assistant"}}]}`)
		writeFrame(w, flusher, `{"choices":[{"delta":{"content":"one"}}]}`)
		writeFrame(w, flusher, `{"choices":[{"delta":{"content":""}}]}`)
		writeFrame(w, flusher, `{"choices":[{"delta":{"content":"two"}}]}`)
		writeFrame(w, flusher, `{"choices":[{"delta":{"content":"three"}}]}`)
		writeFrame(w, flusher, "[DONE]")
		// Anything after [DONE] is not part of the completion.
		writeFrame(w, flusher, `{"choices":[{"delta":{"content":"late"}}]}`)
	}))
	defer srv.Close()

	// The trailing slash is deliberate: base URLs get pasted from docs
	// with and without one, and a doubled slash 404s on real providers.
	obs, err := record(context.Background(), srv.Client(), srv.URL+"/v1/", "k", "m", "p")
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	if gotPath := <-pathCh; gotPath != "/v1/chat/completions" {
		t.Errorf("request path = %q", gotPath)
	}
	if obs.tokens != 3 {
		t.Errorf("tokens = %d, want 3", obs.tokens)
	}
	// One gap per token after the first. Off by one here shifts every
	// inter token number in the profile.
	if len(obs.gaps) != obs.tokens-1 {
		t.Errorf("gaps = %d, want %d", len(obs.gaps), obs.tokens-1)
	}
}

// ttft is measured from the request, each gap from the frame before it.
// Measuring a gap from the request instead would make inter token
// latency grow across a stream and wildly overstate the tail.
func TestRecordMeasuresTTFTAndGapsSeparately(t *testing.T) {
	const delay = 150 * time.Millisecond
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		flusher := sseHeader(w)
		time.Sleep(delay)
		writeFrame(w, flusher, `{"choices":[{"delta":{"content":"one"}}]}`)
		writeFrame(w, flusher, `{"choices":[{"delta":{"content":"two"}}]}`)
		time.Sleep(delay)
		writeFrame(w, flusher, `{"choices":[{"delta":{"content":"three"}}]}`)
		writeFrame(w, flusher, "[DONE]")
	}))
	defer srv.Close()

	obs, err := record(context.Background(), srv.Client(), srv.URL, "k", "m", "p")
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	// Half the injected delay keeps the bounds clear of scheduler noise.
	const floor = delay / 2
	if obs.ttft < floor {
		t.Errorf("ttft = %v, want at least %v", obs.ttft, floor)
	}
	if len(obs.gaps) != 2 {
		t.Fatalf("gaps = %v, want 2", obs.gaps)
	}
	if obs.gaps[0] >= floor {
		t.Errorf("gap after an immediate frame = %v, want under %v", obs.gaps[0], floor)
	}
	if obs.gaps[1] < floor {
		t.Errorf("gap after a delayed frame = %v, want at least %v", obs.gaps[1], floor)
	}
}

func TestRecordErrors(t *testing.T) {
	cases := []struct {
		name    string
		handler http.HandlerFunc
		wantIn  string
	}{
		{
			// A throttled sample must abort the run. Swallowing it would
			// drop the slow samples and publish an optimistic profile.
			name: "rate limited",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusTooManyRequests)
				fmt.Fprint(w, `{"error":"rate limit reached"}`)
			},
			wantIn: "429",
		},
		{
			// An empty stream would otherwise contribute a zero ttft and
			// pull the whole profile down.
			name: "no content frames",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				flusher := sseHeader(w)
				writeFrame(w, flusher, `{"choices":[{"delta":{"role":"assistant"}}]}`)
				writeFrame(w, flusher, "[DONE]")
			},
			wantIn: "no content frames",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.handler)
			defer srv.Close()
			_, err := record(context.Background(), srv.Client(), srv.URL, "k", "m", "p")
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("error = %v, want it to mention %q", err, tc.wantIn)
			}
		})
	}
}

func TestHasContent(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		want    bool
	}{
		{"content", `{"choices":[{"delta":{"content":"hi"}}]}`, true},
		{"role only opener", `{"choices":[{"delta":{"role":"assistant"}}]}`, false},
		{"empty string content", `{"choices":[{"delta":{"content":""}}]}`, false},
		{"finish frame", `{"choices":[{"delta":{},"finish_reason":"stop"}]}`, false},
		// A frame that will not parse is not a token; erroring on it would
		// abandon a recording over a keepalive or a vendor extension.
		{"malformed", `{"choices":`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasContent(tc.payload); got != tc.want {
				t.Errorf("hasContent = %v, want %v", got, tc.want)
			}
		})
	}
}

func seq(n int) []float64 {
	out := make([]float64, 0, n)
	for i := 1; i <= n; i++ {
		out = append(out, float64(i))
	}
	return out
}

func writeProfile(t *testing.T, profile map[string]any) string {
	t.Helper()
	encoded, err := json.MarshalIndent(profile, "", "  ")
	if err != nil {
		t.Fatalf("marshal profile: %v", err)
	}
	path := filepath.Join(t.TempDir(), "profile.json")
	if err := os.WriteFile(path, append(encoded, '\n'), 0o600); err != nil {
		t.Fatalf("write profile: %v", err)
	}
	return path
}

// sseHeader prepares a streaming response. Frames have to be flushed
// individually or the gap timings under test would all collapse to zero.
func sseHeader(w http.ResponseWriter) http.Flusher {
	w.Header().Set("Content-Type", "text/event-stream")
	return w.(http.Flusher)
}

// writeFrame ignores write errors on purpose: record stops reading at
// [DONE], so a later frame legitimately lands on a closed stream.
func writeFrame(w http.ResponseWriter, flusher http.Flusher, payload string) {
	fmt.Fprintf(w, "data: %s\n\n", payload)
	flusher.Flush()
}
