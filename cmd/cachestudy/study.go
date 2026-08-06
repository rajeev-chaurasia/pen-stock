package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/rajeev-chaurasia/pen-stock/internal/cache"
)

const (
	// studyTTL is long enough that nothing expires mid run. The study
	// measures similarity, not lifetime, and an entry ageing out during
	// the sweep would show up as a threshold effect it is not.
	studyTTL time.Duration = 24 * time.Hour

	// probeThreshold admits any candidate with a positive cosine, so the
	// production store can be asked for its own idea of the best score
	// and compared against this command's. It is not part of the sweep.
	probeThreshold float64 = 1e-9

	// cosineTolerance is how far this command's cosine may differ from
	// the production store's before the run reports a disagreement.
	// Both sum in float64 over the same inputs, so the only expected
	// difference is summation order.
	cosineTolerance float64 = 1e-9
)

// storedAnswer is what the study puts in an Entry body. The cached
// answer's identity is the whole point: a hit is scored by which
// question's answer came back, so that has to travel with the entry
// rather than be recovered from a pointer.
type storedAnswer struct {
	Group      string `json:"group"`
	Question   string `json:"question"`
	Completion string `json:"completion,omitempty"`
	Tokens     int    `json:"tokens,omitempty"`
}

// chatRequest is the request shape the study sends. A struct rather than
// a map, so the encoding is byte stable and a repeat really is a repeat.
type chatRequest struct {
	Model       string        `json:"model"`
	Temperature float64       `json:"temperature"`
	Messages    []chatMessage `json:"messages"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// probeRecord is one labelled request, resolved down to the bytes and
// the vector the gateway would actually work with.
type probeRecord struct {
	group  *Group
	label  Label
	text   string
	flip   string
	body   []byte
	vector []float32
	// tokens is what the answer to this question cost to generate, used
	// to turn a hit rate into tokens avoided. Zero when completions were
	// not measured.
	tokens int
}

// anchorRecord is one warm cache entry.
type anchorRecord struct {
	group  *Group
	body   []byte
	vector []float32
	entry  *cache.Entry
}

// Study holds everything the sweep needs, embedded once.
type Study struct {
	corpus  *Corpus
	tenant  string
	model   string
	anchors []anchorRecord
	probes  []probeRecord
}

// promptTexts returns every distinct string the gateway would embed, in
// corpus order. The text is what internal/cache.PromptText produces from
// the request body, not the bare question, so the study embeds exactly
// what the gateway embeds.
func promptTexts(c *Corpus, model string) ([]string, error) {
	seen := make(map[string]struct{})
	var texts []string
	add := func(question string) error {
		body, err := buildBody(model, question)
		if err != nil {
			return err
		}
		text := cache.PromptText(body)
		if text == "" {
			return fmt.Errorf("prompt text is empty for %q", question)
		}
		if _, dup := seen[text]; dup {
			return nil
		}
		seen[text] = struct{}{}
		texts = append(texts, text)
		return nil
	}

	for i := range c.Groups {
		g := &c.Groups[i]
		if err := add(g.Anchor); err != nil {
			return nil, err
		}
		for _, p := range g.Probes {
			if err := add(p.Text); err != nil {
				return nil, err
			}
		}
	}
	return texts, nil
}

// NewStudy resolves the corpus against a vector per prompt text. It also
// checks every request body is cacheable at all: a study that measured
// similarity on bodies the gateway would refuse to cache would be
// measuring a path that never runs.
func NewStudy(c *Corpus, tenant, model string, vectors map[string][]float32, completions map[string]completion) (*Study, error) {
	s := &Study{corpus: c, tenant: tenant, model: model}

	for i := range c.Groups {
		g := &c.Groups[i]

		body, vector, err := resolve(model, g.Anchor, vectors)
		if err != nil {
			return nil, fmt.Errorf("group %q anchor: %w", g.ID, err)
		}
		answer := storedAnswer{Group: g.ID, Question: g.Anchor}
		if comp, ok := completions[g.Anchor]; ok {
			answer.Completion = comp.Text
			answer.Tokens = comp.Tokens
		}
		encoded, err := json.Marshal(answer)
		if err != nil {
			return nil, fmt.Errorf("group %q: encode stored answer: %w", g.ID, err)
		}
		s.anchors = append(s.anchors, anchorRecord{
			group:  g,
			body:   body,
			vector: vector,
			entry:  &cache.Entry{Body: encoded, Model: model, Provider: "cachestudy"},
		})

		for _, p := range g.Probes {
			probeBody, probeVector, err := resolve(model, p.Text, vectors)
			if err != nil {
				return nil, fmt.Errorf("group %q probe %q: %w", g.ID, p.Text, err)
			}
			s.probes = append(s.probes, probeRecord{
				group:  g,
				label:  p.Label,
				text:   p.Text,
				flip:   p.Flip,
				body:   probeBody,
				vector: probeVector,
				tokens: completions[p.Text].Tokens,
			})
		}
	}
	return s, nil
}

// resolve builds a question's request body and finds its vector.
func resolve(model, question string, vectors map[string][]float32) ([]byte, []float32, error) {
	body, err := buildBody(model, question)
	if err != nil {
		return nil, nil, err
	}
	if e := cache.Eligible(body, cache.DefaultMaxTemperature); !e.Cacheable {
		return nil, nil, fmt.Errorf("request body is not cacheable: %s", e.Reason)
	}
	vector, ok := vectors[cache.PromptText(body)]
	if !ok {
		return nil, nil, errors.New("no vector was embedded for this question")
	}
	return body, vector, nil
}

func buildBody(model, question string) ([]byte, error) {
	body, err := json.Marshal(chatRequest{
		Model:       model,
		Temperature: 0,
		Messages:    []chatMessage{{Role: "user", Content: question}},
	})
	if err != nil {
		return nil, fmt.Errorf("encode request body: %w", err)
	}
	return body, nil
}

// RunExactTier warms the exact cache with every anchor and replays every
// probe against it. The repeat row is expected to be 100 percent and the
// rest zero, and it is measured rather than asserted so that a change to
// canonicalization shows up here instead of silently changing the
// baseline the semantic tier is compared against.
func (s *Study) RunExactTier(ctx context.Context) (map[Label]TierRow, error) {
	store := cache.NewExact(cache.ExactOptions{
		MaxEntries: len(s.anchors) + 1,
		TTL:        studyTTL,
	})
	for _, a := range s.anchors {
		key, err := cache.BuildKey(s.tenant, s.model, a.body)
		if err != nil {
			return nil, fmt.Errorf("build key for %q: %w", a.group.ID, err)
		}
		store.Put(ctx, key, a.entry)
	}

	rows := make(map[Label]TierRow, len(labelOrder))
	for i := range s.probes {
		p := &s.probes[i]
		row := rows[p.label]
		row.N++
		key, err := cache.BuildKey(s.tenant, s.model, p.body)
		if err != nil {
			return nil, fmt.Errorf("build key for probe %q: %w", p.text, err)
		}
		if entry, ok := store.Get(ctx, key); ok {
			row.Hits++
			hit, err := decodeAnswer(entry)
			if err != nil {
				return nil, err
			}
			if hit.Group == p.group.ID {
				row.OwnGroupHits++
			} else {
				row.CrossGroupHits++
			}
		}
		rows[p.label] = row
	}
	for label, row := range rows {
		row.HitRate = ratio(row.Hits, row.N)
		rows[label] = row
	}
	return rows, nil
}

// Sweep runs the semantic tier once per threshold against a store warmed
// with every anchor, and scores each hit by whose answer came back.
//
// The store is the production one. Rebuilding it per threshold costs a
// few hundred vector copies and buys the guarantee that the hit or miss
// decision is the gateway's, not a reimplementation of it that could
// drift.
func (s *Study) Sweep(ctx context.Context, thresholds []float64) ([]SweepRow, error) {
	rows := make([]SweepRow, 0, len(thresholds))
	for _, threshold := range thresholds {
		row, err := s.sweepOne(ctx, threshold)
		if err != nil {
			return nil, err
		}
		rows = append(rows, row)
	}
	return rows, nil
}

func (s *Study) sweepOne(ctx context.Context, threshold float64) (SweepRow, error) {
	store := s.warmSemantic(ctx, threshold)

	labels := make(map[Label]*LabelRow, len(labelOrder))
	for _, label := range labelOrder {
		labels[label] = &LabelRow{}
	}
	tokens := TokenRow{}

	for i := range s.probes {
		p := &s.probes[i]
		row := labels[p.label]
		row.N++

		entry, _, ok := store.Nearest(ctx, s.tenant, p.vector)
		if !ok {
			continue
		}
		hit, err := decodeAnswer(entry)
		if err != nil {
			return SweepRow{}, err
		}
		row.Hits++
		own := hit.Group == p.group.ID
		if own {
			row.OwnGroupHits++
		} else {
			row.CrossGroupHits++
		}
		// A repeat or a paraphrase is answered correctly only by its own
		// group. Every hit on an opposite or an unrelated question is
		// wrong however close it scored, which is the whole point of
		// labelling the corpus instead of trusting the distance.
		if own && (p.label == LabelRepeat || p.label == LabelParaphrase) {
			row.CorrectHits++
		} else {
			row.FalseHits++
		}

		// Token accounting covers only the traffic the semantic tier
		// exists to catch. Repeats are answered by the exact tier before
		// this one is consulted, so counting them here would credit the
		// semantic tier with savings it did not produce.
		if p.label != LabelRepeat {
			if own && p.label == LabelParaphrase {
				tokens.ServedCorrect += p.tokens
			} else {
				tokens.ServedWrong += p.tokens
			}
		}
	}

	out := SweepRow{Threshold: threshold, Labels: make(map[string]LabelRow, len(labels))}
	for label, row := range labels {
		row.HitRate = ratio(row.Hits, row.N)
		row.CorrectHitRate = ratio(row.CorrectHits, row.N)
		row.FalseHitRate = ratio(row.FalseHits, row.N)
		out.Labels[string(label)] = *row
	}
	out.MarginPP = 100 * (out.Labels[string(LabelParaphrase)].CorrectHitRate -
		out.Labels[string(LabelOpposite)].FalseHitRate)
	tokens.WrongShare = ratio(tokens.ServedWrong, tokens.ServedCorrect+tokens.ServedWrong)
	out.Tokens = tokens
	return out, nil
}

func (s *Study) warmSemantic(ctx context.Context, threshold float64) cache.Semantic {
	store := cache.NewSemantic(cache.SemanticOptions{
		Threshold:    threshold,
		MaxPerTenant: len(s.anchors) + 1,
	})
	for _, a := range s.anchors {
		store.Add(ctx, s.tenant, a.vector, a.entry)
	}
	return store
}

// Similarity reports the raw distances behind the sweep: how far each
// label sits from its own anchor, and how far unrelated anchors sit from
// each other. The sweep says what a threshold does; this says why no
// threshold can do better.
func (s *Study) Similarity() SimilaritySummary {
	byLabel := make(map[Label][]float64, len(labelOrder))
	pairsByLabel := make(map[Label][]Pair, len(labelOrder))
	anchorByGroup := make(map[string][]float32, len(s.anchors))
	for _, a := range s.anchors {
		anchorByGroup[a.group.ID] = a.vector
	}
	for i := range s.probes {
		p := &s.probes[i]
		score := cosine(p.vector, anchorByGroup[p.group.ID])
		byLabel[p.label] = append(byLabel[p.label], score)
		pairsByLabel[p.label] = append(pairsByLabel[p.label], Pair{
			Cosine: score,
			Group:  p.group.ID,
			A:      p.group.Anchor,
			B:      p.text,
			Flip:   p.flip,
		})
	}

	// The cross pair floor uses anchors only. Every anchor is a distinct
	// question from a corpus spread over four domains, so each pair is
	// genuinely unrelated and there are enough of them to see a tail.
	var cross []float64
	var crossPairs []Pair
	for i := range s.anchors {
		for j := i + 1; j < len(s.anchors); j++ {
			score := cosine(s.anchors[i].vector, s.anchors[j].vector)
			cross = append(cross, score)
			crossPairs = append(crossPairs, Pair{
				Cosine: score,
				Group:  s.anchors[i].group.ID + " vs " + s.anchors[j].group.ID,
				A:      s.anchors[i].group.Anchor,
				B:      s.anchors[j].group.Anchor,
			})
		}
	}

	summary := SimilaritySummary{
		OwnAnchorByLabel:   make(map[string]Stats, len(byLabel)),
		CrossAnchorPairs:   describe(cross),
		HighestOpposites:   topPairs(pairsByLabel[LabelOpposite], true),
		LowestParaphrases:  topPairs(pairsByLabel[LabelParaphrase], false),
		ClosestAnchorPairs: topPairs(crossPairs, true),
	}
	for label, values := range byLabel {
		summary.OwnAnchorByLabel[string(label)] = describe(values)
	}

	paraphrases, opposites := byLabel[LabelParaphrase], byLabel[LabelOpposite]
	summary.ParaphraseVsOppositeAUC = auc(paraphrases, opposites)
	summary.OppositesAboveParaphraseMedian = ratio(countAbove(opposites, median(paraphrases)), len(opposites))
	summary.ParaphrasesBelowOppositeMedian = ratio(countBelow(paraphrases, median(opposites)), len(paraphrases))
	summary.Separable = len(paraphrases) > 0 && len(opposites) > 0 &&
		minOf(paraphrases) > maxOf(opposites)
	return summary
}

// Verify cross checks this command's cosine against the production
// store's, by asking a store that admits almost anything for its best
// score and comparing. A disagreement would mean the similarity table
// and the sweep are describing different arithmetic.
func (s *Study) Verify(ctx context.Context) Checks {
	store := s.warmSemantic(ctx, probeThreshold)
	checks := Checks{}
	for i := range s.probes {
		p := &s.probes[i]
		var best float64
		for _, a := range s.anchors {
			if score := cosine(p.vector, a.vector); score > best {
				best = score
			}
		}
		_, score, ok := store.Nearest(ctx, s.tenant, p.vector)
		if !ok {
			// Nothing cleared a threshold of essentially zero, which can
			// only happen if every anchor is orthogonal or opposed to
			// this probe. Agreement then means this command saw nothing
			// either.
			if best >= probeThreshold {
				checks.CosineDisagreements++
			}
			continue
		}
		if math.Abs(score-best) > cosineTolerance {
			checks.CosineDisagreements++
		}
	}
	return checks
}

// Probes exposes the resolved probes so the caller can request
// completions for exactly the questions the study will score.
func (s *Study) Probes() []probeRecord { return s.probes }

// extremePairs is how many pairs from each end are carried in a result.
// Enough to see whether the worst cases are a handful of outliers or the
// shape of the whole sample, and few enough to read.
const extremePairs int = 10

// topPairs returns the highest or lowest scoring pairs, sorted so the
// most interesting one is first.
func topPairs(pairs []Pair, highest bool) []Pair {
	sorted := append([]Pair(nil), pairs...)
	sort.Slice(sorted, func(i, j int) bool {
		if highest {
			return sorted[i].Cosine > sorted[j].Cosine
		}
		return sorted[i].Cosine < sorted[j].Cosine
	})
	return sorted[:min(extremePairs, len(sorted))]
}

func decodeAnswer(e *cache.Entry) (storedAnswer, error) {
	var hit storedAnswer
	if err := json.Unmarshal(e.Body, &hit); err != nil {
		return storedAnswer{}, fmt.Errorf("decode stored answer: %w", err)
	}
	return hit, nil
}

// cosine mirrors the formula internal/cache uses to decide a hit. It is
// duplicated rather than exported because the study must not change the
// package it is measuring, and Verify checks the two agree on every
// probe rather than assuming they do.
func cosine(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, magA, magB float64
	for i := range a {
		x, y := float64(a[i]), float64(b[i])
		dot += x * y
		magA += x * x
		magB += y * y
	}
	if magA == 0 || magB == 0 {
		return 0
	}
	return dot / math.Sqrt(magA*magB)
}

// Thresholds builds the sweep points, rounded so a float sum does not
// print 0.8300000000000001 as a threshold.
func Thresholds(minValue, maxValue, step float64) ([]float64, error) {
	if step <= 0 {
		return nil, errors.New("threshold step must be positive")
	}
	if minValue > maxValue {
		return nil, errors.New("threshold min is above max")
	}
	var out []float64
	for i := 0; ; i++ {
		v := round6(minValue + float64(i)*step)
		if v > maxValue+1e-9 {
			break
		}
		out = append(out, v)
	}
	return out, nil
}

func round6(v float64) float64 { return math.Round(v*1e6) / 1e6 }

func ratio(part, whole int) float64 {
	if whole == 0 {
		return 0
	}
	return float64(part) / float64(whole)
}

// describe reduces a sample to the shape of its distribution.
func describe(values []float64) Stats {
	if len(values) == 0 {
		return Stats{}
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	var sum float64
	for _, v := range sorted {
		sum += v
	}
	return Stats{
		N:      len(sorted),
		Min:    sorted[0],
		P05:    percentile(sorted, 0.05),
		P25:    percentile(sorted, 0.25),
		Median: percentile(sorted, 0.50),
		P75:    percentile(sorted, 0.75),
		P95:    percentile(sorted, 0.95),
		Max:    sorted[len(sorted)-1],
		Mean:   sum / float64(len(sorted)),
	}
}

// percentile is nearest rank on an already sorted sample. No
// interpolation: these are small samples and an interpolated value is a
// number nothing in the corpus actually scored.
func percentile(sorted []float64, q float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(math.Ceil(q*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

func median(values []float64) float64 {
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	return percentile(sorted, 0.50)
}

// auc is the probability that a randomly drawn paraphrase scores above a
// randomly drawn opposite, ties counted as half. It is the whole
// separation question in one number: 0.5 means the two groups carry no
// ordering signal at all, and below 0.5 means opposites rank higher.
func auc(positive, negative []float64) float64 {
	if len(positive) == 0 || len(negative) == 0 {
		return 0
	}
	var wins float64
	for _, p := range positive {
		for _, n := range negative {
			switch {
			case p > n:
				wins++
			case p == n:
				wins += 0.5
			}
		}
	}
	return wins / float64(len(positive)*len(negative))
}

func countAbove(values []float64, bound float64) int {
	n := 0
	for _, v := range values {
		if v > bound {
			n++
		}
	}
	return n
}

func countBelow(values []float64, bound float64) int {
	n := 0
	for _, v := range values {
		if v < bound {
			n++
		}
	}
	return n
}

func minOf(values []float64) float64 {
	out := values[0]
	for _, v := range values[1:] {
		if v < out {
			out = v
		}
	}
	return out
}

func maxOf(values []float64) float64 {
	out := values[0]
	for _, v := range values[1:] {
		if v > out {
			out = v
		}
	}
	return out
}
