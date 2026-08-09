package main

import "fmt"

// Layout of the threshold sweep figure, in user units.
const (
	sweepHeight = 430.0

	sweepPlotLeft   = 62.0
	sweepPlotRight  = 674.0
	sweepPlotTop    = 84.0
	sweepPlotBottom = 352.0

	// The 100% rule sits below the top of the frame. Both curves run
	// along it from 0.80 to 0.84, and a series pinned to the frame edge
	// reads as clipped.
	sweepValueTop = 96.0

	sweepThresholdMin = 0.80
	sweepThresholdMax = 0.99

	// shippedFloor is the lowest similarity internal/cache will accept
	// when the semantic tier is switched on. It is the only threshold on
	// this chart anyone can actually run, so it is the only one marked.
	shippedFloor = 0.95

	markerRadius = 3.4
)

// drawThresholdSweep renders the semantic cache's hit rate on
// paraphrases against its false hit rate on opposites, at every
// threshold in the study.
//
// One point: the wrong answer curve is not below the right answer curve,
// so there is no line to draw between them.
func drawThresholdSweep(s *CacheStudy) (*Canvas, string, string) {
	c := NewCanvas(figureWidth, sweepHeight)

	x := NewScale(sweepThresholdMin, sweepThresholdMax, sweepPlotLeft, sweepPlotRight)
	y := NewScale(0, 100, sweepPlotBottom, sweepValueTop)

	c.Text(Label{
		Text: "No similarity threshold separates a paraphrase from its opposite",
		X:    14, Y: 26, Size: sizeTitle, Weight: WeightBold, Class: classInk,
	})
	c.Text(Label{
		Text: fmt.Sprintf("%d labelled probes over %d question groups, embedded with %s (%d dimensions)",
			s.Corpus.DistinctPrompt, s.Corpus.Groups, s.Embedder.Model, s.Embedder.Dimensions),
		X: 14, Y: 46, Size: sizeSubtitle, Class: classInk,
	})
	c.Text(Label{
		Text: "cache hit rate (% of that group's probes)",
		X:    14, Y: 72, Size: sizeAxis, Class: classInk,
	})

	// Horizontal rules only. Two curves have to be compared against each
	// other at a chosen threshold, which means reading a value off each,
	// and that is what a value rule is for. Vertical rules would add ink
	// without adding a reading.
	for v := 0.0; v <= 100.0; v += 20 {
		gy := y.At(v)
		c.Line(sweepPlotLeft, gy, sweepPlotRight, gy, classRule)
		c.Text(Label{
			Text: fmt.Sprintf("%.0f", v), X: sweepPlotLeft - 8, Y: gy + 4,
			Size: sizeTick, Anchor: AnchorEnd, Class: classInk,
		})
	}
	c.Line(sweepPlotLeft, sweepPlotBottom, sweepPlotRight, sweepPlotBottom, classAxis)

	for _, t := range []float64{0.80, 0.85, 0.90, 0.95, 0.99} {
		c.Text(Label{
			Text: fmt.Sprintf("%.2f", t), X: x.At(t), Y: sweepPlotBottom + 20,
			Size: sizeTick, Anchor: AnchorMiddle, Class: classInk,
		})
	}
	c.Text(Label{
		Text: "cosine similarity threshold",
		X:    (sweepPlotLeft + sweepPlotRight) / 2, Y: sweepPlotBottom + 40,
		Size: sizeAxis, Anchor: AnchorMiddle, Class: classInk,
	})

	// The shipped floor, marked before the curves so the curves sit on top.
	fx := x.At(shippedFloor)
	c.Line(fx, sweepPlotTop, fx, sweepPlotBottom, classMark)
	c.Text(Label{
		Text: "0.95, the floor Penstock enforces",
		X:    fx, Y: sweepPlotTop - 6, Size: sizeTick, Anchor: AnchorMiddle, Class: classInk,
	})

	wrong := seriesPoints(s, x, y, labelOpposite, func(l LabelCounts) float64 { return l.FalseHitRate })
	right := seriesPoints(s, x, y, labelParaphrase, func(l LabelCounts) float64 { return l.CorrectHitRate })

	// Draw order carries information here. The two series are exactly
	// coincident at 100% from 0.80 to 0.84, so the dashed curve goes on
	// top of the solid one: the solid shows through the gaps and the
	// reader sees two series rather than one. For the same reason the
	// circles go on top of the squares, leaving the square's corners
	// visible wherever a point is shared.
	c.Polyline(right, classRightLine)
	c.Polyline(wrong, classWrongLine)
	for i := 0; i+1 < len(wrong); i += 2 {
		c.Square(wrong[i], wrong[i+1], markerRadius, classWrong)
	}
	for i := 0; i+1 < len(right); i += 2 {
		c.Circle(right[i], right[i+1], markerRadius, classRight)
	}

	// Direct labels rather than a legend. Each one is also given a leader
	// to its own curve, because the two series cross twice near 0.89 and
	// a label floating between them would be ambiguous.
	c.Text(Label{
		Text: "opposite meaning (wrong answers)",
		X:    440, Y: 130, Size: sizeAnnot, Class: classWrong,
	})
	c.Line(462, 137, 466, 184, classLead)

	c.Text(Label{
		Text: "paraphrase (right answers)",
		X:    390, Y: 294, Size: sizeAnnot, Anchor: AnchorEnd, Class: classRight,
	})
	c.Line(394, 290, 495, 244, classLead)

	// The readings at the only threshold that ships. loadCacheStudy has
	// already refused a study that does not cover it.
	atFloor := findThreshold(s, shippedFloor)
	c.Text(Label{
		Text: fmt.Sprintf("%.0f%% of opposites", atFloor.Labels[labelOpposite].FalseHitRate*100),
		X:    fx + 8, Y: 228, Size: sizeAnnot, Class: classWrong,
	})
	c.Text(Label{
		Text: fmt.Sprintf("%.0f%% of paraphrases", atFloor.Labels[labelParaphrase].CorrectHitRate*100),
		X:    fx - 8, Y: 305, Size: sizeAnnot, Anchor: AnchorEnd, Class: classRight,
	})

	c.Text(Label{
		Text: "Source: bench/results/" + cacheStudyFile + ". Method and caveats: docs/cache-quality.md",
		X:    14, Y: 414, Size: sizeFootnote, Class: classInk,
	})

	title := "Semantic cache hit rate against false hit rate, by similarity threshold"
	desc := fmt.Sprintf(
		"Two curves over cosine similarity thresholds %.2f to %.2f. The false hit rate on "+
			"opposite-meaning questions runs level with or above the correct hit rate on "+
			"paraphrases across most of the range, so no threshold separates them. At the "+
			"shipped floor of %.2f the tier serves %.0f%% of opposites from cache and %.0f%% "+
			"of paraphrases.",
		sweepThresholdMin, sweepThresholdMax, shippedFloor,
		atFloor.Labels[labelOpposite].FalseHitRate*100,
		atFloor.Labels[labelParaphrase].CorrectHitRate*100)
	return c, title, desc
}

// seriesPoints turns one label's sweep column into pixel coordinates.
func seriesPoints(s *CacheStudy, x, y Scale, label string, pick func(LabelCounts) float64) []float64 {
	pts := make([]float64, 0, 2*len(s.Sweep))
	for _, p := range s.Sweep {
		pts = append(pts, x.At(p.Threshold), y.At(pick(p.Labels[label])*100))
	}
	return pts
}

func findThreshold(s *CacheStudy, t float64) *SweepPoint {
	for i := range s.Sweep {
		if abs(s.Sweep[i].Threshold-t) < 1e-9 {
			return &s.Sweep[i]
		}
	}
	return nil
}
