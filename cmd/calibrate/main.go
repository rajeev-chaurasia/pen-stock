// Command calibrate records how a real provider actually behaves and
// writes an llmsim profile from it.
//
// This exists because of the standard criticism of gateway benchmarks:
// they run against a mock that answers instantly, so the number they
// report is mostly the mock's speed and tells you nothing about the
// gateway. A profile recorded here makes llmsim replay the latency
// shape of real traffic, which is the only way the resulting overhead
// figure means anything.
//
// It calls a real provider and spends real tokens, so it is a separate
// command run deliberately rather than part of any test.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

const (
	// defaultPrompt asks for enough output to measure inter token gaps
	// without spending much.
	defaultPrompt = "Count slowly from one to twenty, one number per line."

	// sampleGap paces requests so a free tier rate limit is not the
	// thing being measured.
	sampleGap = 2 * time.Second

	maxErrorBody = 4 << 10
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "calibrate:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		baseURL = flag.String("base-url", "https://api.groq.com/openai/v1", "provider base URL, OpenAI wire")
		model   = flag.String("model", "llama-3.3-70b-versatile", "model to record")
		keyEnv  = flag.String("key-env", "GROQ_API_KEY", "environment variable holding the API key")
		samples = flag.Int("samples", 8, "how many completions to record")
		out     = flag.String("out", "bench/profiles/provider.json", "where to write the profile")
		name    = flag.String("name", "", "profile name, defaults to the model")
		prompt  = flag.String("prompt", defaultPrompt, "prompt to send")
	)
	flag.Parse()

	key := os.Getenv(*keyEnv)
	if key == "" {
		return fmt.Errorf("%s is empty; export a real key to record against a real provider", *keyEnv)
	}
	if *name == "" {
		*name = *model
	}

	client := &http.Client{Timeout: 2 * time.Minute}
	var (
		ttfts  []float64
		itls   []float64
		tokens []float64
	)

	fmt.Fprintf(os.Stderr, "recording %d completions from %s\n", *samples, *model)
	for i := 0; i < *samples; i++ {
		if i > 0 {
			time.Sleep(sampleGap)
		}
		obs, err := record(context.Background(), client, *baseURL, key, *model, *prompt)
		if err != nil {
			return fmt.Errorf("sample %d: %w", i+1, err)
		}
		ttfts = append(ttfts, obs.ttft.Seconds()*1000)
		tokens = append(tokens, float64(obs.tokens))
		for _, gap := range obs.gaps {
			itls = append(itls, gap.Seconds()*1000)
		}
		fmt.Fprintf(os.Stderr, "  sample %d: ttft %.0fms, %d chunks, median gap %.1fms\n",
			i+1, obs.ttft.Seconds()*1000, obs.tokens, median(toMillis(obs.gaps)))
	}

	profile := map[string]any{
		"name":          *name,
		"ttft_ms":       stats(ttfts),
		"itl_ms":        stats(itls),
		"output_tokens": stats(tokens),
	}
	// The provenance travels with the numbers. A profile whose origin
	// is unknown is indistinguishable from one somebody invented.
	profile["recorded"] = map[string]any{
		"provider_base_url": *baseURL,
		"model":             *model,
		"samples":           *samples,
		"chunk_samples":     len(itls),
		"recorded_at":       time.Now().UTC().Format(time.RFC3339),
		"prompt":            *prompt,
	}

	encoded, err := json.MarshalIndent(profile, "", "  ")
	if err != nil {
		return fmt.Errorf("encode profile: %w", err)
	}
	// A profile carries timing, not secrets, but there is no reason for
	// it to be group writable either.
	if err := os.WriteFile(*out, append(encoded, '\n'), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", *out, err)
	}
	fmt.Fprintf(os.Stderr, "wrote %s\n", *out)
	fmt.Println(string(encoded))
	return nil
}

// observation is one completion's timing.
type observation struct {
	ttft   time.Duration
	gaps   []time.Duration
	tokens int
}

// record streams one completion and times it. Inter token gaps are
// measured between SSE frames carrying content, which is as close to a
// per token interval as the wire exposes.
func record(ctx context.Context, client *http.Client, baseURL, key, model, prompt string) (observation, error) {
	body, err := json.Marshal(map[string]any{
		"model":       model,
		"stream":      true,
		"max_tokens":  200,
		"temperature": 0,
		"messages":    []map[string]string{{"role": "user", "content": prompt}},
	})
	if err != nil {
		return observation{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(baseURL, "/")+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return observation{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)

	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return observation{}, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		return observation{}, fmt.Errorf("upstream %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}

	var obs observation
	last := start
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			break
		}
		if !hasContent(payload) {
			continue
		}
		now := time.Now()
		if obs.tokens == 0 {
			obs.ttft = now.Sub(start)
		} else {
			obs.gaps = append(obs.gaps, now.Sub(last))
		}
		last = now
		obs.tokens++
	}
	if err := scanner.Err(); err != nil {
		return observation{}, err
	}
	if obs.tokens == 0 {
		return observation{}, fmt.Errorf("stream carried no content frames")
	}
	return obs, nil
}

// hasContent reports whether a chunk carried visible text, so an empty
// role-only opener is not counted as a token.
func hasContent(payload string) bool {
	var chunk struct {
		Choices []struct {
			Delta struct {
				Content string `json:"content"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
		return false
	}
	for _, c := range chunk.Choices {
		if c.Delta.Content != "" {
			return true
		}
	}
	return false
}

// stats reduces samples to the mean and p95 llmsim's profile expects.
func stats(values []float64) map[string]float64 {
	if len(values) == 0 {
		return map[string]float64{"mean": 0, "p95": 0}
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)

	var sum float64
	for _, v := range sorted {
		sum += v
	}
	mean := sum / float64(len(sorted))

	idx := int(math.Ceil(0.95*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return map[string]float64{
		"mean": round(mean),
		"p95":  round(sorted[idx]),
	}
}

func round(v float64) float64 { return math.Round(v*100) / 100 }

func toMillis(d []time.Duration) []float64 {
	out := make([]float64, 0, len(d))
	for _, v := range d {
		out = append(out, v.Seconds()*1000)
	}
	return out
}

func median(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	return sorted[len(sorted)/2]
}
