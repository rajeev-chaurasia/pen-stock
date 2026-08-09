package main

import (
	"bytes"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Anchor is an SVG text-anchor value.
type Anchor string

const (
	AnchorStart  Anchor = "start"
	AnchorMiddle Anchor = "middle"
	AnchorEnd    Anchor = "end"
)

// Weight is an SVG font-weight value.
type Weight string

const (
	WeightNormal Weight = "400"
	WeightBold   Weight = "600"
)

// Box is an axis aligned rectangle in user units.
type Box struct{ X0, Y0, X1, Y1 float64 }

func (b Box) overlaps(o Box) bool {
	return b.X0 < o.X1 && o.X0 < b.X1 && b.Y0 < o.Y1 && o.Y0 < b.Y1
}

// solidity says whether a recorded element is allowed to have other
// elements drawn over it. Bands and rules are backdrop: labels are meant
// to sit on top of them. Text and bars are not.
type solidity int

const (
	backdrop solidity = iota
	opaque
)

// element is one recorded piece of geometry, kept so the canvas can
// prove afterwards that nothing collides and nothing escaped the frame.
type element struct {
	kind  string
	label string
	box   Box
	solid solidity
}

// Canvas accumulates SVG markup and, alongside it, a bounding box for
// every element it draws.
//
// The second job is the reason this type exists rather than a pile of
// fmt.Fprintf calls. A figure whose labels collide, or whose axis title
// falls off the edge, is a bug that no compiler catches and that is easy
// to miss when the only check is a glance at a thumbnail. Validate turns
// that into a build failure.
type Canvas struct {
	w, h   float64
	body   bytes.Buffer
	elems  []element
	curves []curve
}

// curve is a drawn path kept for collision checking. A polyline's
// bounding box says almost nothing about where its ink is, so labels are
// tested against the segments themselves.
type curve struct {
	label string
	pts   []float64
}

// NewCanvas starts a figure w by h user units.
func NewCanvas(w, h float64) *Canvas {
	return &Canvas{w: w, h: h}
}

// Width and Height report the canvas extent.
func (c *Canvas) Width() float64  { return c.w }
func (c *Canvas) Height() float64 { return c.h }

func (c *Canvas) record(kind, label string, box Box, solid solidity) {
	c.elems = append(c.elems, element{kind: kind, label: label, box: box, solid: solid})
}

// Rect draws a filled rectangle carrying the given CSS class.
func (c *Canvas) Rect(b Box, class string, solid solidity) {
	fmt.Fprintf(&c.body, "  <rect x=%q y=%q width=%q height=%q class=%q/>\n",
		num(b.X0), num(b.Y0), num(b.X1-b.X0), num(b.Y1-b.Y0), class)
	c.record("rect", class, b, solid)
}

// Line draws a straight segment.
func (c *Canvas) Line(x0, y0, x1, y1 float64, class string) {
	fmt.Fprintf(&c.body, "  <line x1=%q y1=%q x2=%q y2=%q class=%q/>\n",
		num(x0), num(y0), num(x1), num(y1), class)
	c.record("line", class, normalize(x0, y0, x1, y1), backdrop)
}

// Polyline draws an open path through pts, given as x,y pairs.
func (c *Canvas) Polyline(pts []float64, class string) {
	var sb strings.Builder
	for i := 0; i+1 < len(pts); i += 2 {
		if i > 0 {
			sb.WriteByte(' ')
		}
		sb.WriteString(num(pts[i]) + "," + num(pts[i+1]))
	}
	fmt.Fprintf(&c.body, "  <polyline points=%q class=%q/>\n", sb.String(), class)
	c.record("polyline", class, bounds(pts), backdrop)
	c.curves = append(c.curves, curve{label: class, pts: append([]float64(nil), pts...)})
}

// Circle draws a marker of radius r.
func (c *Canvas) Circle(x, y, r float64, class string) {
	fmt.Fprintf(&c.body, "  <circle cx=%q cy=%q r=%q class=%q/>\n", num(x), num(y), num(r), class)
	c.record("circle", class, Box{x - r, y - r, x + r, y + r}, backdrop)
}

// Square draws a marker of side 2r centred on x,y, so that it reads as
// the same visual weight as a Circle of radius r.
func (c *Canvas) Square(x, y, r float64, class string) {
	fmt.Fprintf(&c.body, "  <rect x=%q y=%q width=%q height=%q class=%q/>\n",
		num(x-r), num(y-r), num(2*r), num(2*r), class)
	c.record("rect", class, Box{x - r, y - r, x + r, y + r}, backdrop)
}

// Label is a piece of text plus everything needed to place it and to
// estimate the space it will occupy.
type Label struct {
	Text   string
	X, Y   float64
	Size   float64
	Anchor Anchor
	Weight Weight
	Class  string
}

// Text draws a label and records the box it is expected to cover.
func (c *Canvas) Text(l Label) {
	weight := l.Weight
	if weight == "" {
		weight = WeightNormal
	}
	anchor := l.Anchor
	if anchor == "" {
		anchor = AnchorStart
	}
	fmt.Fprintf(&c.body,
		"  <text x=%q y=%q font-size=%q font-weight=%q text-anchor=%q class=%q>%s</text>\n",
		num(l.X), num(l.Y), num(l.Size), string(weight), string(anchor), l.Class, escape(l.Text))
	c.record("text", l.Text, textBox(l, weight, anchor), opaque)
}

// textBox estimates the ink extent of a label. It is deliberately a
// slight overestimate: a checker that flags a near miss costs one
// nudge, and a checker that misses a real collision costs a bad figure.
func textBox(l Label, weight Weight, anchor Anchor) Box {
	w := TextWidth(l.Text, l.Size, weight)
	var x0 float64
	switch anchor {
	case AnchorMiddle:
		x0 = l.X - w/2
	case AnchorEnd:
		x0 = l.X - w
	default:
		x0 = l.X
	}
	// Ascent and descent for a typical sans face, as fractions of the
	// font size.
	const (
		ascent  = 0.80
		descent = 0.24
	)
	return Box{x0, l.Y - ascent*l.Size, x0 + w, l.Y + descent*l.Size}
}

// TextWidth approximates the advance width of s at the given size.
func TextWidth(s string, size float64, weight Weight) float64 {
	var em float64
	for _, r := range s {
		em += runeWidth(r)
	}
	if weight == WeightBold {
		em *= 1.06
	}
	return em * size
}

// runeWidth is the advance of one rune as a fraction of the font size,
// approximated for a humanist sans face. Only the coarse classes matter
// for collision checking.
func runeWidth(r rune) float64 {
	switch {
	case r == ' ':
		return 0.28
	case strings.ContainsRune("iljtIf.,:;'`|!()[]{}/\\-", r):
		return 0.32
	case strings.ContainsRune("mwMW@%", r):
		return 0.88
	case r >= '0' && r <= '9':
		return 0.57
	case r >= 'A' && r <= 'Z':
		return 0.68
	default:
		return 0.56
	}
}

// Validate reports every way the figure is broken: an element outside
// the frame, or two elements that must not overlap doing so.
func (c *Canvas) Validate() error {
	var problems []string
	frame := Box{0, 0, c.w, c.h}

	for _, e := range c.elems {
		if e.box.X0 < frame.X0-0.5 || e.box.Y0 < frame.Y0-0.5 ||
			e.box.X1 > frame.X1+0.5 || e.box.Y1 > frame.Y1+0.5 {
			problems = append(problems, fmt.Sprintf(
				"%s %q escapes the viewBox: %s", e.kind, e.label, e.box))
		}
	}

	for i := range c.elems {
		if c.elems[i].solid != opaque {
			continue
		}
		for j := i + 1; j < len(c.elems); j++ {
			if c.elems[j].solid != opaque {
				continue
			}
			if c.elems[i].box.overlaps(c.elems[j].box) {
				problems = append(problems, fmt.Sprintf(
					"%s %q overlaps %s %q: %s vs %s",
					c.elems[i].kind, c.elems[i].label,
					c.elems[j].kind, c.elems[j].label,
					c.elems[i].box, c.elems[j].box))
			}
		}
	}

	// Labels against curve ink. The margin covers the stroke width and
	// the point markers sitting on the path, so a label that clears this
	// clears the drawn line too.
	const curveClearance = 5.0
	for _, e := range c.elems {
		if e.kind != "text" {
			continue
		}
		pad := Box{e.box.X0 - curveClearance, e.box.Y0 - curveClearance,
			e.box.X1 + curveClearance, e.box.Y1 + curveClearance}
		for _, cv := range c.curves {
			if seg := firstCrossing(pad, cv.pts); seg >= 0 {
				problems = append(problems, fmt.Sprintf(
					"label %q sits on curve %q at segment %d", e.label, cv.label, seg))
			}
		}
	}

	if len(problems) > 0 {
		return errors.New("layout: " + strings.Join(problems, "; "))
	}
	return nil
}

// firstCrossing returns the index of the first segment of pts that meets
// b, or -1 when none does.
func firstCrossing(b Box, pts []float64) int {
	for i := 0; i+3 < len(pts); i += 2 {
		if segMeetsBox(b, pts[i], pts[i+1], pts[i+2], pts[i+3]) {
			return i / 2
		}
	}
	return -1
}

// segMeetsBox reports whether the segment intersects the rectangle,
// by Liang-Barsky clipping.
func segMeetsBox(b Box, x0, y0, x1, y1 float64) bool {
	dx, dy := x1-x0, y1-y0
	t0, t1 := 0.0, 1.0
	for _, e := range [4][2]float64{
		{-dx, x0 - b.X0}, {dx, b.X1 - x0},
		{-dy, y0 - b.Y0}, {dy, b.Y1 - y0},
	} {
		p, q := e[0], e[1]
		switch {
		case p == 0:
			if q < 0 {
				return false // parallel and outside
			}
		case p < 0:
			if r := q / p; r > t1 {
				return false
			} else if r > t0 {
				t0 = r
			}
		default:
			if r := q / p; r < t0 {
				return false
			} else if r < t1 {
				t1 = r
			}
		}
	}
	return t0 <= t1
}

// Report counts the checks Validate performed, so a build log says what
// was actually proved rather than only that nothing complained.
func (c *Canvas) Report() string {
	var texts, solids int
	for _, e := range c.elems {
		if e.kind == "text" {
			texts++
		}
		if e.solid == opaque {
			solids++
		}
	}
	var segments int
	for _, cv := range c.curves {
		segments += len(cv.pts)/2 - 1
	}
	return fmt.Sprintf(
		"%.0fx%.0f, %d elements, %d labels, %d frame checks, %d solid pairs, %d label/segment pairs",
		c.w, c.h, len(c.elems), texts, len(c.elems), solids*(solids-1)/2, texts*segments)
}

// Dump lists every recorded box, so the geometry can be inspected
// without rendering the figure.
func (c *Canvas) Dump() string {
	var sb strings.Builder
	for _, e := range c.elems {
		fmt.Fprintf(&sb, "  %-9s %-24s %s\n", e.kind, e.box, truncate(e.label, 60))
	}
	return sb.String()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-3] + "..."
}

// Bytes renders the finished document.
func (c *Canvas) Bytes(title, desc string) []byte {
	var out bytes.Buffer
	fmt.Fprintf(&out,
		"<svg xmlns=\"http://www.w3.org/2000/svg\" width=%q height=%q viewBox=\"0 0 %s %s\" role=\"img\">\n",
		num(c.w), num(c.h), num(c.w), num(c.h))
	fmt.Fprintf(&out, "  <title>%s</title>\n  <desc>%s</desc>\n", escape(title), escape(desc))
	out.WriteString(styleBlock)
	out.Write(c.body.Bytes())
	out.WriteString("</svg>\n")
	return out.Bytes()
}

func (b Box) String() string {
	return fmt.Sprintf("[%s %s %s %s]", num(b.X0), num(b.Y0), num(b.X1), num(b.Y1))
}

func normalize(x0, y0, x1, y1 float64) Box {
	if x0 > x1 {
		x0, x1 = x1, x0
	}
	if y0 > y1 {
		y0, y1 = y1, y0
	}
	return Box{x0, y0, x1, y1}
}

func bounds(pts []float64) Box {
	if len(pts) < 2 {
		return Box{}
	}
	b := Box{pts[0], pts[1], pts[0], pts[1]}
	for i := 2; i+1 < len(pts); i += 2 {
		b.X0 = min(b.X0, pts[i])
		b.X1 = max(b.X1, pts[i])
		b.Y0 = min(b.Y0, pts[i+1])
		b.Y1 = max(b.Y1, pts[i+1])
	}
	return b
}

// num formats a coordinate with a fixed precision so that regenerating a
// figure from unchanged data produces a byte identical file.
func num(v float64) string {
	s := strconv.FormatFloat(v, 'f', 2, 64)
	s = strings.TrimRight(s, "0")
	s = strings.TrimSuffix(s, ".")
	if s == "" || s == "-0" {
		return "0"
	}
	return s
}

func escape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	return r.Replace(s)
}
