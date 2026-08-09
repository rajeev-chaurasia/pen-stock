package main

import (
	"math"
	"testing"
)

// This file tests the arithmetic the published 43.9 percent rests on,
// not the command line around it. Four functions decide that number:
// cosine, which ranks every probe; Thresholds, which chooses the sweep
// points; ratio, which turns counts into the rates that get quoted; and
// the labelling rule that decides which hits are wrong.

// cosine is deliberately a second implementation of the one in
// internal/cache, so the study can disagree with the code it measures
// and say so. That only works if both are independently right, which is
// what this table checks: the production copy is held to the same values
// by internal/cache/semantic_test.go.
func TestCosineMatchesHandComputedValues(t *testing.T) {
	const tolerance = 1e-12
	cases := []struct {
		name string
		a, b []float32
		want float64
	}{
		{"identical vectors are 1", []float32{1, 2, 3}, []float32{1, 2, 3}, 1},
		{"orthogonal vectors are 0", []float32{1, 0}, []float32{0, 1}, 0},
		{"opposite vectors are -1", []float32{1, 2}, []float32{-1, -2}, -1},
		{"scale does not matter", []float32{1, 2, 3}, []float32{10, 20, 30}, 1},
		// 3*1 + 4*0 = 3, magnitudes 5 and 1, so 0.6 exactly.
		{"a 3-4-5 triangle against the x axis", []float32{3, 4}, []float32{1, 0}, 0.6},
		{"45 degrees is one over root two", []float32{1, 1}, []float32{1, 0}, 1 / math.Sqrt2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := cosine(tc.a, tc.b); math.Abs(got-tc.want) > tolerance {
				t.Errorf("cosine = %v, want %v", got, tc.want)
			}
		})
	}
}

// A zero magnitude vector has no direction, so there is no angle to
// report. Returning zero rather than NaN keeps a single bad embedding
// from poisoning every comparison it appears in.
func TestCosineReturnsZeroRatherThanNaN(t *testing.T) {
	cases := []struct {
		name string
		a, b []float32
	}{
		{"zero magnitude", []float32{0, 0}, []float32{1, 1}},
		{"both zero", []float32{0, 0}, []float32{0, 0}},
		{"mismatched lengths", []float32{1, 2, 3}, []float32{1, 2}},
		{"empty", []float32{}, []float32{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := cosine(tc.a, tc.b)
			if math.IsNaN(got) {
				t.Fatalf("cosine returned NaN for %v and %v", tc.a, tc.b)
			}
			if got != 0 {
				t.Errorf("cosine = %v, want 0", got)
			}
		})
	}
}

func TestThresholds(t *testing.T) {
	t.Run("inclusive of both ends", func(t *testing.T) {
		got, err := Thresholds(0.80, 0.85, 0.01)
		if err != nil {
			t.Fatalf("Thresholds: %v", err)
		}
		want := []float64{0.80, 0.81, 0.82, 0.83, 0.84, 0.85}
		if len(got) != len(want) {
			t.Fatalf("got %v, want %v", got, want)
		}
		for i := range want {
			// Exact equality on purpose: these are printed as thresholds
			// and appear in the committed result JSON, so 0.8300000000001
			// would leak into a published figure.
			if got[i] != want[i] {
				t.Errorf("threshold %d = %v, want %v", i, got[i], want[i])
			}
		}
	})

	t.Run("a single point when min equals max", func(t *testing.T) {
		got, err := Thresholds(0.95, 0.95, 0.01)
		if err != nil {
			t.Fatalf("Thresholds: %v", err)
		}
		if len(got) != 1 || got[0] != 0.95 {
			t.Errorf("got %v, want exactly [0.95]", got)
		}
	})

	t.Run("refusals", func(t *testing.T) {
		if _, err := Thresholds(0.8, 0.9, 0); err == nil {
			t.Error("a zero step was accepted, which would not terminate")
		}
		if _, err := Thresholds(0.8, 0.9, -0.01); err == nil {
			t.Error("a negative step was accepted")
		}
		if _, err := Thresholds(0.9, 0.8, 0.01); err == nil {
			t.Error("min above max was accepted")
		}
	})
}

// ratio is what turns hit counts into the percentages that get quoted.
// An empty label must report zero rather than divide by zero, or a
// sweep point with no probes of some label would print NaN into the
// committed results.
func TestRatio(t *testing.T) {
	cases := []struct {
		part, whole int
		want        float64
	}{
		{0, 0, 0},
		{1, 0, 0},
		{25, 100, 0.25},
		{1, 3, 1.0 / 3.0},
		// The shipped figure: 25 of 57 opposite probes hit at 0.95.
		{25, 57, 25.0 / 57.0},
	}
	for _, tc := range cases {
		got := ratio(tc.part, tc.whole)
		if math.IsNaN(got) {
			t.Errorf("ratio(%d, %d) is NaN", tc.part, tc.whole)
		}
		if math.Abs(got-tc.want) > 1e-12 {
			t.Errorf("ratio(%d, %d) = %v, want %v", tc.part, tc.whole, got, tc.want)
		}
	}
}
