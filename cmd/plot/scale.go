package main

// Scale maps a data interval onto a pixel interval.
//
// It has no "nice axis" logic and no automatic domain fitting. Both are
// conveniences that quietly decide where an axis starts, and every axis
// in these figures either starts at zero or has its start chosen by hand
// for a stated reason.
type Scale struct {
	d0, d1 float64
	p0, p1 float64
}

// NewScale maps [d0,d1] onto [p0,p1].
func NewScale(d0, d1, p0, p1 float64) Scale {
	return Scale{d0: d0, d1: d1, p0: p0, p1: p1}
}

// At converts a data value to a pixel position.
func (s Scale) At(v float64) float64 {
	if s.d1 == s.d0 {
		return s.p0
	}
	return s.p0 + (v-s.d0)/(s.d1-s.d0)*(s.p1-s.p0)
}

// Span converts a data width to a pixel width, sign preserved.
func (s Scale) Span(v float64) float64 {
	return s.At(s.d0+v) - s.p0
}
