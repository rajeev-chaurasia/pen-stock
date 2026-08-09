package main

import "testing"

// The layout checker is the only thing standing between a bad figure and
// the README, so these tests are about the checker itself rather than
// about the figures. A validator that never fails would let every one of
// them through while reporting success.

func TestValidateAcceptsACleanLayout(t *testing.T) {
	c := NewCanvas(400, 200)
	c.Text(Label{Text: "left", X: 10, Y: 30, Size: 12, Class: classInk})
	c.Text(Label{Text: "right", X: 200, Y: 30, Size: 12, Class: classInk})
	c.Polyline([]float64{10, 150, 390, 190}, classRightLine)

	if err := c.Validate(); err != nil {
		t.Fatalf("clean layout rejected: %v", err)
	}
}

func TestValidateCatchesOverlappingLabels(t *testing.T) {
	c := NewCanvas(400, 200)
	c.Text(Label{Text: "a long label", X: 10, Y: 30, Size: 12, Class: classInk})
	c.Text(Label{Text: "sitting on top", X: 20, Y: 32, Size: 12, Class: classInk})

	if err := c.Validate(); err == nil {
		t.Fatal("two labels drawn on the same spot were accepted")
	}
}

func TestValidateCatchesLabelOverBar(t *testing.T) {
	c := NewCanvas(400, 200)
	c.Rect(Box{50, 50, 300, 80}, classWrong, opaque)
	c.Text(Label{Text: "over the bar", X: 60, Y: 70, Size: 12, Class: classInk})

	if err := c.Validate(); err == nil {
		t.Fatal("a label drawn over a bar was accepted")
	}
}

func TestValidateAllowsLabelOverBackdrop(t *testing.T) {
	c := NewCanvas(400, 200)
	c.Rect(Box{50, 50, 300, 80}, classBand, backdrop)
	c.Text(Label{Text: "inside the band", X: 60, Y: 70, Size: 12, Class: classInk})

	if err := c.Validate(); err != nil {
		t.Fatalf("a label over a noise band was rejected: %v", err)
	}
}

func TestValidateCatchesLabelOnCurve(t *testing.T) {
	c := NewCanvas(400, 200)
	c.Polyline([]float64{10, 100, 390, 100}, classRightLine)
	c.Text(Label{Text: "on the line", X: 100, Y: 102, Size: 12, Class: classInk})

	if err := c.Validate(); err == nil {
		t.Fatal("a label sitting on a plotted curve was accepted")
	}
}

func TestValidateCatchesEscapeFromViewBox(t *testing.T) {
	c := NewCanvas(400, 200)
	c.Text(Label{Text: "hanging off the right edge", X: 380, Y: 30, Size: 12, Class: classInk})

	if err := c.Validate(); err == nil {
		t.Fatal("a label past the right edge was accepted")
	}
}

func TestSegMeetsBoxIgnoresASegmentThatMissesEntirely(t *testing.T) {
	b := Box{100, 100, 200, 120}
	if segMeetsBox(b, 0, 0, 90, 10) {
		t.Fatal("a segment well clear of the box was reported as meeting it")
	}
	if !segMeetsBox(b, 0, 110, 300, 110) {
		t.Fatal("a segment straight through the box was reported as missing it")
	}
}
