package cache

import (
	"context"
	"fmt"
	"os"
	"testing"
)

// TestMeasureRealSimilarities is a measurement, not an assertion. It
// exists so the similarity threshold is chosen from what an embedder
// actually produces rather than from a guess, and it skips unless a key
// is present so CI never depends on a live service.
//
// Run it with: GEMINI_API_KEY=... go test ./internal/cache -run Measure -v
func TestMeasureRealSimilarities(t *testing.T) {
	key := os.Getenv("GEMINI_API_KEY")
	if key == "" {
		t.Skip("no GEMINI_API_KEY, skipping the live measurement")
	}
	e := NewGeminiEmbedder("", key, "", nil)

	cases := []struct {
		kind string
		a, b string
	}{
		{"same question", "What is the capital city of France?", "Which city is the capital of France?"},
		{"same question", "How do I sort a list in Python?", "What is the way to sort a python list?"},
		{"opposite", "How do I enable logging in this application?", "How do I disable logging in this application?"},
		{"opposite", "Should I use a mutex here?", "Should I avoid a mutex here?"},
		{"opposite", "How do I start the service?", "How do I stop the service?"},
		{"opposite", "Is it safe to delete this file?", "Is it unsafe to delete this file?"},
		{"unrelated", "What is the capital of France?", "What is the boiling point of water?"},
	}

	for _, tc := range cases {
		vecs, err := e.Embed(context.Background(), []string{tc.a, tc.b})
		if err != nil {
			t.Fatalf("embed: %v", err)
		}
		sim := cosineSimilarity(vecs[0], vecs[1])
		fmt.Printf("%-14s %.6f  %q vs %q\n", tc.kind, sim, tc.a, tc.b)
	}
}
