package cache

import (
	"testing"

	"github.com/rajeev-chaurasia/pen-stock/internal/config"
)

// The similarity floor exists in two packages: config rejects anything
// below it at load time, and this package applies it when a threshold
// is left unset. Nothing links the two constants at compile time, so a
// change to one would silently leave the other admitting neighbours the
// loader thinks it is refusing.
func TestSimilarityFloorMatchesTheLoader(t *testing.T) {
	if DefaultSimilarityThreshold != config.MinSemanticThreshold {
		t.Errorf("cache default %v and config floor %v disagree; one of them lets through a similarity the other refuses",
			DefaultSimilarityThreshold, config.MinSemanticThreshold)
	}
}

// A threshold of zero means "unset" and must resolve to the default
// rather than to "match anything", which is the reading that would turn
// an unconfigured semantic tier into one that answers every question
// with whatever it saw first.
func TestZeroThresholdTakesTheFloorNotEverything(t *testing.T) {
	cases := []struct {
		name      string
		threshold float64
	}{
		{"unset", 0},
		{"negative", -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := NewSemantic(SemanticOptions{Threshold: tc.threshold})

			// A pair at 0.707 is well above zero and well below the
			// floor: it must not be treated as the same question.
			ctx := t.Context()
			s.Add(ctx, "acme", []float32{1, 0}, &Entry{Body: []byte(`{"a":1}`)})
			if _, sim, ok := s.Nearest(ctx, "acme", []float32{1, 1}); ok {
				t.Errorf("a neighbour at similarity %v was accepted with threshold %v, want the floor applied", sim, tc.threshold)
			}
		})
	}
}
