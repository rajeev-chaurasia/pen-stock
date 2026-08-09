package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// CacheStudy is the part of cmd/cachestudy's result file these figures
// read. Everything else in that file is left unmodelled on purpose: a
// plot that quietly depends on a field is harder to audit than one whose
// inputs are listed.
type CacheStudy struct {
	Corpus struct {
		Groups         int            `json:"groups"`
		ProbesByLabel  map[string]int `json:"probes_by_label"`
		DistinctPrompt int            `json:"distinct_prompts_embedded"`
	} `json:"corpus"`
	Embedder struct {
		Model      string `json:"model"`
		Dimensions int    `json:"dimensions"`
	} `json:"embedder"`
	Sweep   []SweepPoint `json:"sweep"`
	Verdict struct {
		BestMarginThreshold float64 `json:"best_margin_threshold"`
		BestMarginPP        float64 `json:"best_margin_pp"`
		Separable           bool    `json:"separable"`
	} `json:"verdict"`
}

// SweepPoint is one similarity threshold and what the semantic tier did
// at it.
type SweepPoint struct {
	Threshold float64                `json:"threshold"`
	Labels    map[string]LabelCounts `json:"labels"`
}

// LabelCounts is the outcome for one corpus label at one threshold.
type LabelCounts struct {
	N              int     `json:"n"`
	Hits           int     `json:"hits"`
	CorrectHits    int     `json:"correct_hits"`
	FalseHits      int     `json:"false_hits"`
	CorrectHitRate float64 `json:"correct_hit_rate"`
	FalseHitRate   float64 `json:"false_hit_rate"`
}

// Corpus labels, as written by cmd/cachestudy.
const (
	labelParaphrase = "paraphrase"
	labelOpposite   = "opposite"
)

func loadCacheStudy(path string) (*CacheStudy, error) {
	var s CacheStudy
	if err := readJSON(path, &s); err != nil {
		return nil, err
	}
	if len(s.Sweep) == 0 {
		return nil, fmt.Errorf("%s: no sweep points", path)
	}
	for _, p := range s.Sweep {
		for _, l := range []string{labelParaphrase, labelOpposite} {
			if _, ok := p.Labels[l]; !ok {
				return nil, fmt.Errorf("%s: threshold %.2f is missing label %q", path, p.Threshold, l)
			}
		}
	}
	// The figure annotates the shipped floor, so a study that never swept
	// it cannot be drawn. Failing here beats drawing a chart with the one
	// operating point anyone can run silently missing from it.
	if findThreshold(&s, shippedFloor) == nil {
		return nil, fmt.Errorf("%s: sweep does not cover the shipped floor %.2f", path, shippedFloor)
	}
	return &s, nil
}

// k6Summary is the shape k6 writes with --summary-export.
type k6Summary struct {
	Metrics map[string]struct {
		Values map[string]float64 `json:"values"`
	} `json:"metrics"`
}

// The four arms of bench/compare. Arms 1 and 4 send identical traffic to
// the upstream with no gateway in the path, so the gap between them is
// this run's own measurement noise.
const (
	armDirect        = "direct_latency"
	armDirectRecheck = "direct_recheck_latency"
	armPenstock      = "penstock_latency"
	armLiteLLM       = "litellm_latency"
)

// Statistic names one summary statistic and the key k6 files it under.
type Statistic struct {
	Name string
	Key  string
}

// The statistics the comparison figure draws, in the order it draws
// them. Mean leads because it is the only one docs/comparison.md
// permits quoting as a per request overhead: means subtract exactly,
// quantiles do not.
var statistics = []Statistic{
	{"mean", "avg"},
	{"p50", "p(50)"},
	{"p95", "p(95)"},
	{"p99", "p(99)"},
}

// Overhead is what one gateway added to one statistic, beside the amount
// this run could not distinguish from noise.
type Overhead struct {
	Statistic  string
	Penstock   float64
	LiteLLM    float64
	NoiseFloor float64
}

// BelowFloor reports whether Penstock's delta is smaller than the run
// could resolve. When it is, the number is not a measurement of anything
// and the figure has to say so.
func (o Overhead) BelowFloor() bool {
	return abs(o.Penstock) < o.NoiseFloor
}

// Comparison is the gateway overhead figure's whole input.
type Comparison struct {
	Run       string
	Overheads []Overhead
	Samples   int
}

func loadComparison(path string) (*Comparison, error) {
	var s k6Summary
	if err := readJSON(path, &s); err != nil {
		return nil, err
	}

	value := func(arm, key string) (float64, error) {
		m, ok := s.Metrics[arm]
		if !ok {
			return 0, fmt.Errorf("%s: no metric %q", path, arm)
		}
		v, ok := m.Values[key]
		if !ok {
			return 0, fmt.Errorf("%s: metric %q has no %q", path, arm, key)
		}
		return v, nil
	}

	c := &Comparison{Run: trimSummarySuffix(filepath.Base(path))}
	for _, st := range statistics {
		direct, err := value(armDirect, st.Key)
		if err != nil {
			return nil, err
		}
		recheck, err := value(armDirectRecheck, st.Key)
		if err != nil {
			return nil, err
		}
		penstock, err := value(armPenstock, st.Key)
		if err != nil {
			return nil, err
		}
		litellm, err := value(armLiteLLM, st.Key)
		if err != nil {
			return nil, err
		}
		c.Overheads = append(c.Overheads, Overhead{
			Statistic:  st.Name,
			Penstock:   penstock - direct,
			LiteLLM:    litellm - direct,
			NoiseFloor: abs(recheck - direct),
		})
	}

	if n, err := value(armDirect, "count"); err == nil {
		c.Samples = int(n)
	}
	return c, nil
}

func readJSON(path string, into any) error {
	// #nosec G304 -- this is a developer tool and the path comes from a
	// flag with a committed default, not from a request.
	raw, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, into); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

func trimSummarySuffix(name string) string {
	const suffix = ".summary.json"
	if len(name) > len(suffix) && name[len(name)-len(suffix):] == suffix {
		return name[:len(name)-len(suffix)]
	}
	return name
}

func abs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}
