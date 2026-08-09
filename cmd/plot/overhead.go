package main

import (
	"fmt"
	"math"
)

// Layout of the gateway overhead figure, in user units.
const (
	overheadHeight = 400.0

	overheadPlotLeft  = 118.0
	overheadPlotRight = 566.0
	overheadRowsTop   = 82.0
	overheadRowHeight = 50.0

	overheadLabelRight = 104.0
	overheadBarHeight  = 15.0
	overheadTickStep   = 4.0
)

// drawOverhead renders what each gateway added to end to end latency,
// against the amount this run could not tell from nothing.
//
// One point: LiteLLM's overhead is far outside the measurement noise on
// every statistic, and Penstock's sits on the edge of it.
//
// The noise floor is drawn rather than described because without it the
// Penstock bars invite a reading the data does not support. A delta of
// -0.03 ms is not a gateway making requests faster, it is a quantity
// smaller than the harness can resolve, and only the band says so.
func drawOverhead(cmp *Comparison) (*Canvas, string, string) {
	c := NewCanvas(figureWidth, overheadHeight)

	domainMax := overheadTickStep
	for _, o := range cmp.Overheads {
		for domainMax < math.Max(o.LiteLLM, o.Penstock)*1.02 {
			domainMax += overheadTickStep
		}
	}
	// Half a tick of negative room, so the symmetric noise band and any
	// negative delta are both visible. Zero is drawn as a solid rule and
	// carries a tick, so the bars are still read from zero.
	domainMin := -overheadTickStep / 2
	x := NewScale(domainMin, domainMax, overheadPlotLeft, overheadPlotRight)
	zero := x.At(0)
	rowsBottom := overheadRowsTop + overheadRowHeight*float64(len(cmp.Overheads))

	c.Text(Label{
		Text: "LiteLLM adds about 12 ms per request, Penstock about 1 ms",
		X:    14, Y: 26, Size: sizeTitle, Weight: WeightBold, Class: classInk,
	})
	c.Text(Label{
		Text: fmt.Sprintf("Linux, LiteLLM 1.95.0 on uvicorn with uvloop confirmed loaded. "+
			"%d samples per arm at 20 requests/s.", cmp.Samples),
		X: 14, Y: 46, Size: sizeSubtitle, Class: classInk,
	})

	for v := 0.0; v <= domainMax; v += overheadTickStep {
		gx := x.At(v)
		class := classRule
		if v == 0 {
			class = classAxis
		}
		c.Line(gx, overheadRowsTop, gx, rowsBottom, class)
		c.Text(Label{
			Text: fmt.Sprintf("%.0f", v), X: gx, Y: rowsBottom + 20,
			Size: sizeTick, Anchor: AnchorMiddle, Class: classInk,
		})
	}
	c.Text(Label{
		Text: "added latency vs no gateway (ms)",
		X:    (overheadPlotLeft + overheadPlotRight) / 2, Y: rowsBottom + 40,
		Size: sizeAxis, Anchor: AnchorMiddle, Class: classInk,
	})

	for i, o := range cmp.Overheads {
		yc := overheadRowsTop + overheadRowHeight*(float64(i)+0.5)

		// The band is symmetric about zero because the floor is a
		// magnitude of uncertainty, not a direction. Anything inside it
		// is indistinguishable from no overhead at all.
		c.Rect(Box{x.At(-o.NoiseFloor), yc - 20, x.At(o.NoiseFloor), yc + 20}, classBand, backdrop)

		c.Text(Label{
			Text: o.Statistic, X: overheadLabelRight, Y: yc - 4,
			Size: sizeTick, Anchor: AnchorEnd, Weight: WeightBold, Class: classInk,
		})
		c.Text(Label{
			Text: fmt.Sprintf("floor %.2f ms", o.NoiseFloor),
			X:    overheadLabelRight, Y: yc + 13,
			Size: sizeFootnote, Anchor: AnchorEnd, Class: classInk,
		})

		note := ""
		if o.BelowFloor() {
			note = " (below the noise floor)"
		}
		bar(c, x, zero, yc-overheadBarHeight-3, o.Penstock, classRight,
			"Penstock "+signed(o.Penstock)+note)
		bar(c, x, zero, yc+3, o.LiteLLM, classWrong,
			"LiteLLM "+signed(o.LiteLLM))
	}

	c.Text(Label{
		Text: "Grey band is this run's own noise floor: the gap between two identical " +
			"no-gateway arms of the same run.",
		X: 14, Y: 344, Size: sizeFootnote, Class: classInk,
	})
	c.Text(Label{
		Text: "Percentile rows are differences of quantiles. Only the mean row is " +
			"exactly the mean per request overhead.",
		X: 14, Y: 360, Size: sizeFootnote, Class: classInk,
	})
	c.Text(Label{
		Text: "Source: bench/results/" + compareFile + ". Caveats: docs/comparison.md",
		X:    14, Y: 376, Size: sizeFootnote, Class: classInk,
	})

	title := "Gateway overhead against the measurement noise floor"
	desc := fmt.Sprintf(
		"Added latency per request for Penstock and LiteLLM at four statistics, each beside "+
			"the noise floor of the same run. Mean overhead was %s for Penstock and %s for "+
			"LiteLLM against a floor of %.2f ms. Every LiteLLM figure is far outside the "+
			"floor; Penstock's tail deltas are inside it and are not resolvable by this harness.",
		signed(cmp.Overheads[0].Penstock), signed(cmp.Overheads[0].LiteLLM),
		cmp.Overheads[0].NoiseFloor)
	return c, title, desc
}

// bar draws one horizontal bar from zero and labels it at its far end.
// The label names its own series, which is why these figures carry no
// legend and why a reader who cannot separate the two colours loses
// nothing.
func bar(c *Canvas, x Scale, zero, top, value float64, class, label string) {
	end := x.At(value)
	c.Rect(normalize(zero, top, end, top+overheadBarHeight), class, opaque)
	c.Text(Label{
		Text: label, X: math.Max(zero, end) + 8, Y: top + overheadBarHeight - 3.5,
		Size: sizeAnnot, Class: class,
	})
}

func signed(ms float64) string {
	return fmt.Sprintf("%+.2f ms", ms)
}
