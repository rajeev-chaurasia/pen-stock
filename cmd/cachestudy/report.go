package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"text/tabwriter"
)

// resultSchema names the shape of the emitted result file.
const resultSchema = "penstock/cache-quality/result/v1"

// Result is the whole run, written to bench/results so every figure in
// the writeup can be traced back to output that was actually produced.
type Result struct {
	Schema      string              `json:"schema"`
	GeneratedAt string              `json:"generated_at"`
	Corpus      CorpusSummary       `json:"corpus"`
	Embedder    EmbedderSummary     `json:"embedder"`
	Request     RequestSummary      `json:"request"`
	Completions *CompletionsSummary `json:"completions"`
	ExactTier   map[string]TierRow  `json:"exact_tier"`
	Similarity  SimilaritySummary   `json:"similarity"`
	Sweep       []SweepRow          `json:"sweep"`
	Verdict     Verdict             `json:"verdict"`
	Checks      Checks              `json:"checks"`
}

// CorpusSummary records what was measured, so a result cannot be read
// without its sample size.
type CorpusSummary struct {
	Path            string         `json:"path"`
	Groups          int            `json:"groups"`
	ProbesByLabel   map[string]int `json:"probes_by_label"`
	GroupsByDomain  map[string]int `json:"groups_by_domain"`
	DistinctPrompts int            `json:"distinct_prompts_embedded"`
}

// EmbedderSummary records which embedder produced the vectors. A
// similarity number is a property of a model, not of language.
type EmbedderSummary struct {
	BaseURL    string `json:"base_url"`
	Model      string `json:"model"`
	Dimensions int    `json:"dimensions"`
	BatchSize  int    `json:"batch_size"`
}

// RequestSummary records the request the study built around each
// question, and the text the gateway derived from it to embed.
type RequestSummary struct {
	Tenant         string `json:"tenant"`
	RoutedModel    string `json:"routed_model"`
	Temperature    string `json:"temperature"`
	PromptTextForm string `json:"prompt_text_form"`
}

// CompletionsSummary records the model that priced the hits.
type CompletionsSummary struct {
	BaseURL     string  `json:"base_url"`
	Model       string  `json:"model"`
	MaxTokens   int     `json:"max_tokens"`
	Questions   int     `json:"questions"`
	TotalTokens int     `json:"total_completion_tokens"`
	MeanTokens  float64 `json:"mean_completion_tokens"`
}

// TierRow is one label's outcome against a single tier.
type TierRow struct {
	N              int     `json:"n"`
	Hits           int     `json:"hits"`
	OwnGroupHits   int     `json:"own_group_hits"`
	CrossGroupHits int     `json:"cross_group_hits"`
	HitRate        float64 `json:"hit_rate"`
}

// LabelRow is one label's outcome at one threshold.
//
// Correct and false are not two names for hit and miss. A repeat or a
// paraphrase answered from its own group is correct; the same probe
// answered from another group is false; and every hit on an opposite or
// an unrelated question is false no matter which entry served it.
type LabelRow struct {
	N              int     `json:"n"`
	Hits           int     `json:"hits"`
	OwnGroupHits   int     `json:"own_group_hits"`
	CrossGroupHits int     `json:"cross_group_hits"`
	CorrectHits    int     `json:"correct_hits"`
	FalseHits      int     `json:"false_hits"`
	HitRate        float64 `json:"hit_rate"`
	CorrectHitRate float64 `json:"correct_hit_rate"`
	FalseHitRate   float64 `json:"false_hit_rate"`
}

// TokenRow prices one threshold. Repeats are excluded: the exact tier
// answers those before the semantic tier is consulted, so counting them
// would credit this tier with savings it did not produce.
type TokenRow struct {
	ServedCorrect int     `json:"served_correct"`
	ServedWrong   int     `json:"served_wrong"`
	WrongShare    float64 `json:"wrong_share"`
}

// SweepRow is one threshold.
type SweepRow struct {
	Threshold float64             `json:"threshold"`
	Labels    map[string]LabelRow `json:"labels"`
	MarginPP  float64             `json:"paraphrase_minus_opposite_pp"`
	Tokens    TokenRow            `json:"tokens_excluding_repeats"`
}

// Stats is the shape of a sample of cosine scores.
type Stats struct {
	N      int     `json:"n"`
	Min    float64 `json:"min"`
	P05    float64 `json:"p05"`
	P25    float64 `json:"p25"`
	Median float64 `json:"median"`
	P75    float64 `json:"p75"`
	P95    float64 `json:"p95"`
	Max    float64 `json:"max"`
	Mean   float64 `json:"mean"`
}

// Pair is one measured question pair, kept whole so a reader can check
// a claim against the sentences that produced it rather than against a
// summary of them.
type Pair struct {
	Cosine float64 `json:"cosine"`
	Group  string  `json:"group,omitempty"`
	A      string  `json:"a"`
	B      string  `json:"b"`
	Flip   string  `json:"flip,omitempty"`
}

// SimilaritySummary is the raw distance evidence behind the sweep.
type SimilaritySummary struct {
	OwnAnchorByLabel map[string]Stats `json:"own_anchor_by_label"`
	CrossAnchorPairs Stats            `json:"cross_anchor_pairs"`
	// HighestOpposites are the pairs a threshold has to sit above to be
	// safe, and LowestParaphrases are the ones it has to sit below to be
	// useful. Printed together because the two lists overlapping is the
	// entire finding.
	HighestOpposites   []Pair `json:"highest_scoring_opposites"`
	LowestParaphrases  []Pair `json:"lowest_scoring_paraphrases"`
	ClosestAnchorPairs []Pair `json:"closest_unrelated_anchor_pairs"`
	// ParaphraseVsOppositeAUC is the probability a random paraphrase
	// outscores a random opposite. 0.5 is no signal at all.
	ParaphraseVsOppositeAUC        float64 `json:"paraphrase_vs_opposite_auc"`
	OppositesAboveParaphraseMedian float64 `json:"opposites_above_paraphrase_median"`
	ParaphrasesBelowOppositeMedian float64 `json:"paraphrases_below_opposite_median"`
	// Separable is true only when every paraphrase outscores every
	// opposite, which is what a threshold would need to be safe.
	Separable bool `json:"separable"`
}

// Criterion asks whether any threshold meets a stated tolerance for
// wrong answers, and what hit rate the best one buys.
type Criterion struct {
	Name                    string   `json:"name"`
	MaxOppositeFalseHitRate float64  `json:"max_opposite_false_hit_rate"`
	Satisfied               bool     `json:"satisfied"`
	Threshold               *float64 `json:"threshold"`
	ParaphraseHitRate       float64  `json:"paraphrase_hit_rate"`
	OppositeFalseHitRate    float64  `json:"opposite_false_hit_rate"`
}

// Verdict answers the question the study was built to answer.
type Verdict struct {
	Question            string      `json:"question"`
	Answer              string      `json:"answer"`
	BestMarginThreshold float64     `json:"best_margin_threshold"`
	BestMarginPP        float64     `json:"best_margin_pp"`
	Separable           bool        `json:"separable"`
	Criteria            []Criterion `json:"criteria"`
}

// Checks records what the run verified about itself.
type Checks struct {
	CosineDisagreements int `json:"cosine_disagreements"`
}

// verdictQuestion is the study's stated question, carried in the result
// file so a reader is not left inferring what was being asked.
const verdictQuestion = "Is there any similarity threshold where the paraphrase hit rate is meaningfully above the opposite false hit rate?"

// criteria are the tolerances a reader might have for wrong answers.
// They are stated up front rather than chosen after seeing the numbers.
var criteria = []struct {
	name string
	max  float64
}{
	{"no wrong answers", 0.0},
	{"under 1 in 20 wrong", 0.05},
	{"under 1 in 10 wrong", 0.10},
}

// buildVerdict reduces the sweep to an answer. A criterion is satisfied
// only when some threshold both holds the opposite false hit rate at or
// below its ceiling and catches a paraphrase at all: a threshold that is
// safe because it never fires has not solved anything.
func buildVerdict(sweep []SweepRow, separable bool) Verdict {
	v := Verdict{Question: verdictQuestion, Separable: separable}

	for i, row := range sweep {
		if i == 0 || row.MarginPP > v.BestMarginPP {
			v.BestMarginPP = row.MarginPP
			v.BestMarginThreshold = row.Threshold
		}
	}

	for _, c := range criteria {
		best := Criterion{Name: c.name, MaxOppositeFalseHitRate: c.max}
		for i := range sweep {
			row := sweep[i]
			opposite := row.Labels[string(LabelOpposite)].FalseHitRate
			paraphrase := row.Labels[string(LabelParaphrase)].CorrectHitRate
			if opposite > c.max || paraphrase <= 0 {
				continue
			}
			if !best.Satisfied || paraphrase > best.ParaphraseHitRate {
				threshold := row.Threshold
				best.Satisfied = true
				best.Threshold = &threshold
				best.ParaphraseHitRate = paraphrase
				best.OppositeFalseHitRate = opposite
			}
		}
		v.Criteria = append(v.Criteria, best)
	}

	v.Answer = answerFrom(v)
	return v
}

func answerFrom(v Verdict) string {
	strict := v.Criteria[0]
	loose := v.Criteria[len(v.Criteria)-1]
	switch {
	case strict.Satisfied:
		return fmt.Sprintf(
			"Yes. At a threshold of %.2f the semantic tier catches %.0f%% of paraphrases and answers no opposite question.",
			*strict.Threshold, 100*strict.ParaphraseHitRate)
	case loose.Satisfied:
		return fmt.Sprintf(
			"Partly. No threshold is clean, but at %.2f the tier catches %.0f%% of paraphrases while answering %.0f%% of opposites wrongly.",
			*loose.Threshold, 100*loose.ParaphraseHitRate, 100*loose.OppositeFalseHitRate)
	default:
		return fmt.Sprintf(
			"No. Every threshold that catches a paraphrase at all also answers more than %.0f%% of opposite-meaning questions from the cache. The best paraphrase-minus-opposite margin over the whole sweep is %+.1f percentage points, at a threshold of %.2f.",
			100*loose.MaxOppositeFalseHitRate, v.BestMarginPP, v.BestMarginThreshold)
	}
}

// WriteResult writes the raw result. A study whose numbers cannot be
// recomputed from a committed file is an assertion, not a measurement.
func WriteResult(path string, r *Result) error {
	encoded, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("encode result: %w", err)
	}
	if err := os.WriteFile(path, append(encoded, '\n'), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// reportWriter keeps the first write error and skips the rest, the same
// way bufio.Writer does. A report is a few dozen lines and checking each
// one at its call site would bury the report's shape in error handling.
type reportWriter struct {
	out io.Writer
	err error
}

func (p *reportWriter) printf(format string, args ...any) {
	if p.err != nil {
		return
	}
	_, p.err = fmt.Fprintf(p.out, format, args...)
}

// table writes tab separated lines through a tabwriter so the columns
// line up. The header is just the first row.
func (p *reportWriter) table(header string, rows []string) {
	if p.err != nil {
		return
	}
	tw := tabwriter.NewWriter(p.out, 0, 0, 2, ' ', 0)
	for _, line := range append([]string{header}, rows...) {
		if _, err := fmt.Fprintln(tw, line); err != nil {
			p.err = err
			return
		}
	}
	p.err = tw.Flush()
}

// PrintReport writes the readable form of a result, and names the file
// holding the raw form so the two are never quoted apart.
func PrintReport(w io.Writer, r *Result, resultPath string) error {
	p := &reportWriter{out: w}

	p.printf("\ncorpus            %s\n", r.Corpus.Path)
	p.printf("groups            %d across %d domains\n", r.Corpus.Groups, len(r.Corpus.GroupsByDomain))
	for i, label := range labelOrder {
		if i == 0 {
			p.printf("probes            ")
		} else {
			p.printf(", ")
		}
		p.printf("%s %d", label, r.Corpus.ProbesByLabel[string(label)])
	}
	p.printf("\nembedder          %s, %d dimensions, %d distinct prompts embedded once\n",
		r.Embedder.Model, r.Embedder.Dimensions, r.Corpus.DistinctPrompts)
	if r.Completions != nil {
		p.printf("completions       %s, %d answers, %d completion tokens total\n",
			r.Completions.Model, r.Completions.Questions, r.Completions.TotalTokens)
	}

	printExactTier(p, r)
	printSimilarity(p, r)
	printSweep(p, r)
	printVerdict(p, r)
	p.printf("\nraw result written to %s\n", resultPath)

	if p.err != nil {
		return fmt.Errorf("write report: %w", p.err)
	}
	return nil
}

func printExactTier(p *reportWriter, r *Result) {
	p.printf("\nEXACT TIER (no embedder involved)\n")
	rows := make([]string, 0, len(labelOrder))
	for _, label := range labelOrder {
		row := r.ExactTier[string(label)]
		rows = append(rows, fmt.Sprintf("%s\t%d\t%d\t%.1f%%", label, row.N, row.Hits, 100*row.HitRate))
	}
	p.table("label\tn\thits\thit rate", rows)
	p.printf("The repeat row is 100%% by construction: a repeat is the same bytes, so it is the same key.\n")
}

func printSimilarity(p *reportWriter, r *Result) {
	p.printf("\nCOSINE TO OWN ANCHOR, by label\n")
	rows := make([]string, 0, len(labelOrder)+1)
	statsRow := func(name string, s Stats) string {
		return fmt.Sprintf("%s\t%d\t%.3f\t%.3f\t%.3f\t%.3f\t%.3f\t%.3f",
			name, s.N, s.Min, s.P05, s.Median, s.P95, s.Max, s.Mean)
	}
	for _, label := range labelOrder {
		rows = append(rows, statsRow(string(label), r.Similarity.OwnAnchorByLabel[string(label)]))
	}
	rows = append(rows, statsRow("anchor pairs", r.Similarity.CrossAnchorPairs))
	p.table("label\tn\tmin\tp05\tmedian\tp95\tmax\tmean", rows)

	p.printf("paraphrase beats opposite %.1f%% of the time (50%% is no signal)\n",
		100*r.Similarity.ParaphraseVsOppositeAUC)
	p.printf("%.1f%% of opposites score above the median paraphrase\n",
		100*r.Similarity.OppositesAboveParaphraseMedian)
	p.printf("separable by any threshold: %v\n", r.Similarity.Separable)

	// A threshold has to sit above every one of these to be safe and
	// below every one of the next list to be useful. Printing them
	// together is the finding in its rawest form.
	p.printf("\nHighest scoring OPPOSITES, the pairs a safe threshold must exclude\n")
	p.table("cosine\tflip\tanchor\topposite", pairRows(r.Similarity.HighestOpposites, true))
	p.printf("\nLowest scoring paraphrases, the pairs a useful threshold must admit\n")
	p.table("cosine\tanchor\tparaphrase", pairRows(r.Similarity.LowestParaphrases, false))
}

func pairRows(pairs []Pair, withFlip bool) []string {
	rows := make([]string, 0, len(pairs))
	for _, pair := range pairs {
		if withFlip {
			rows = append(rows, fmt.Sprintf("%.3f\t%s\t%s\t%s", pair.Cosine, pair.Flip, pair.A, pair.B))
			continue
		}
		rows = append(rows, fmt.Sprintf("%.3f\t%s\t%s", pair.Cosine, pair.A, pair.B))
	}
	return rows
}

func printSweep(p *reportWriter, r *Result) {
	priced := r.Completions != nil
	p.printf("\nTHRESHOLD SWEEP, semantic tier against a cache warmed with every anchor\n")

	header := "threshold\trepeat hit\tparaphrase hit\tOPPOSITE FALSE\tunrelated false\tmargin"
	if priced {
		header += "\twrong tokens"
	}
	rows := make([]string, 0, len(r.Sweep))
	for _, row := range r.Sweep {
		line := fmt.Sprintf("%.2f\t%.1f%%\t%.1f%%\t%.1f%%\t%.1f%%\t%+.1fpp",
			row.Threshold,
			100*row.Labels[string(LabelRepeat)].CorrectHitRate,
			100*row.Labels[string(LabelParaphrase)].CorrectHitRate,
			100*row.Labels[string(LabelOpposite)].FalseHitRate,
			100*row.Labels[string(LabelUnrelated)].FalseHitRate,
			row.MarginPP)
		if priced {
			line += fmt.Sprintf("\t%.1f%%", 100*row.Tokens.WrongShare)
		}
		rows = append(rows, line)
	}
	p.table(header, rows)
	if priced {
		p.printf("wrong tokens is the share of tokens served from cache that answered a different question, repeats excluded.\n")
	}
}

func printVerdict(p *reportWriter, r *Result) {
	p.printf("\nVERDICT\n%s\n%s\n\n", r.Verdict.Question, r.Verdict.Answer)
	rows := make([]string, 0, len(r.Verdict.Criteria))
	for _, c := range r.Verdict.Criteria {
		threshold := "-"
		if c.Threshold != nil {
			threshold = fmt.Sprintf("%.2f", *c.Threshold)
		}
		rows = append(rows, fmt.Sprintf("%s\t%v\t%s\t%.1f%%\t%.1f%%",
			c.Name, c.Satisfied, threshold, 100*c.ParaphraseHitRate, 100*c.OppositeFalseHitRate))
	}
	p.table("tolerance\tmet\tthreshold\tparaphrase hit\topposite false", rows)

	if r.Checks.CosineDisagreements > 0 {
		p.printf("\nWARNING: %d probes where this command's cosine disagreed with the production store. Do not quote this run.\n",
			r.Checks.CosineDisagreements)
	}
}
