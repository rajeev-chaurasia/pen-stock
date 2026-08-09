package main

// Colour choice, and why it is what it is.
//
// These figures are read on GitHub, which renders an SVG referenced from
// markdown inside an <img>. Two constraints follow and they pull against
// each other.
//
// First, the page background is not knowable. GitHub's theme is a user
// setting, and it does not have to agree with the browser's
// prefers-color-scheme, so a media query inside the SVG cannot be relied
// on to fire when the page is dark. The figures therefore paint no
// background at all and every base colour has to survive on both white
// and GitHub's dark #0d1117.
//
// Second, no single colour reaches WCAG AA 4.5:1 against both of those.
// That is arithmetic rather than an opinion: clearing 4.5:1 on #0d1117
// needs a relative luminance of at least 0.198, clearing it on white
// needs at most 0.183, and there is no colour in between. The best any
// fixed colour can do on both is about 4.2:1.
//
// So the base palette is the compromise that survives either background,
// and the dark media query is layered on top as an improvement for the
// readers whose browser does report a preference. Measured contrast of
// the base colours, white first, then #0d1117:
//
//	ink      #757575   4.6:1   4.1:1
//	right    #0072B2   5.2:1   3.7:1
//	wrong    #D55E00   3.9:1   4.9:1
//
// The two series colours are the Okabe-Ito blue and vermillion, which
// stay distinguishable under deuteranopia, protanopia and tritanopia.
// Nothing in either figure is encoded by colour alone regardless: every
// series carries a direct text label, and the sweep's two curves also
// differ in dash pattern and in marker shape.
const styleBlock = `  <style>
    text { font-family: ui-sans-serif, -apple-system, "Segoe UI", Helvetica, Arial, sans-serif; }
    .ink { fill: #757575; }
    .rule { stroke: #757575; stroke-opacity: 0.30; stroke-width: 1; fill: none; }
    .axis { stroke: #757575; stroke-opacity: 0.75; stroke-width: 1; fill: none; }
    .band { fill: #757575; fill-opacity: 0.16; }
    .lead { stroke: #757575; stroke-opacity: 0.55; stroke-width: 1; fill: none; }
    .mark { stroke: #757575; stroke-opacity: 0.60; stroke-width: 1.25;
            stroke-dasharray: 4 3; fill: none; }
    .right { fill: #0072B2; }
    .right-line { stroke: #0072B2; stroke-width: 2.2; fill: none;
                  stroke-linejoin: round; stroke-linecap: round; }
    .wrong { fill: #D55E00; }
    .wrong-line { stroke: #D55E00; stroke-width: 2.2; fill: none; stroke-dasharray: 7 4;
                  stroke-linejoin: round; stroke-linecap: round; }
    @media (prefers-color-scheme: dark) {
      .ink { fill: #C9D1D9; }
      .rule { stroke: #C9D1D9; stroke-opacity: 0.24; }
      .axis { stroke: #C9D1D9; stroke-opacity: 0.62; }
      .band { fill: #C9D1D9; fill-opacity: 0.14; }
      .lead { stroke: #C9D1D9; stroke-opacity: 0.50; }
      .mark { stroke: #C9D1D9; stroke-opacity: 0.55; }
      .right, .right-line { fill: #56B4E9; }
      .right-line { stroke: #56B4E9; fill: none; }
      .wrong, .wrong-line { fill: #E69F00; }
      .wrong-line { stroke: #E69F00; fill: none; }
    }
  </style>
`

// Type sizes, in user units, which equal CSS pixels at the 700 unit
// width these figures are drawn for.
const (
	sizeTitle    = 16.0
	sizeSubtitle = 12.0
	sizeAxis     = 12.5
	sizeTick     = 12.0
	sizeAnnot    = 12.5
	sizeFootnote = 11.0
)

// CSS class names, kept as constants so a typo is a compile error rather
// than an invisible unstyled element.
const (
	classInk       = "ink"
	classRule      = "rule"
	classAxis      = "axis"
	classBand      = "band"
	classLead      = "lead"
	classMark      = "mark"
	classRight     = "right"
	classRightLine = "right-line"
	classWrong     = "wrong"
	classWrongLine = "wrong-line"
)

// figureWidth is the README content width these figures must stay legible at.
const figureWidth = 700.0
