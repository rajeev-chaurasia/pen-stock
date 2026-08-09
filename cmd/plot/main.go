// Command plot renders the committed benchmark results as SVG figures
// for the README and docs.
//
// It reads the result files exactly as the benchmarks wrote them and
// draws what is in them. It does not smooth, it does not resample, and
// no axis it draws starts anywhere but zero unless the chart says so on
// its face. A figure that flatters the gateway more than the numbers do
// is worse than no figure, because it is the part of the evidence a
// reader checks last.
//
// Two rules the output obeys, both of which are about being read rather
// than about being right:
//
//   - No background is painted. GitHub's light and dark themes are both
//     in scope and the colours are chosen to survive either, with a
//     prefers-color-scheme block layered on for browsers that report
//     one. See theme.go for the contrast arithmetic.
//   - Every figure is checked before it is written. Text that collides
//     with other text, with a bar, or with a plotted curve, and anything
//     that falls outside the viewBox, fails the run instead of shipping.
//
// SVG and the standard library only. These figures are regenerated from
// a repository that must build with no toolchain beyond Go, and a text
// format that git can diff is worth more here than a raster one.
//
//	go run ./cmd/plot
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

// The runs these figures are drawn from.
//
// Naming them rather than globbing the results directory is deliberate:
// bench/results holds every run that was made, including the ones
// docs/comparison.md discards and explains, and a figure that picked up
// whichever file sorted last would eventually publish one of those.
const (
	cacheStudyFile = "cache-study-20260806T233311Z.json"

	// The Linux run, and only the Linux run. LiteLLM's own launcher
	// refuses to ask for uvloop on Windows, so the Windows measurement is
	// of a different code path inside LiteLLM rather than of LiteLLM with
	// a part missing. docs/comparison.md keeps both and quotes this one.
	compareFile = "compare-linux-uvicorn-w1-20260807T011936Z.summary.json"
)

func main() {
	resultsDir := flag.String("results", filepath.Join("bench", "results"),
		"directory holding the committed benchmark results")
	outDir := flag.String("out", filepath.Join("docs", "img"),
		"directory to write the SVG figures into")
	dump := flag.Bool("dump", false,
		"print the bounding box of every drawn element, for inspecting layout")
	flag.Parse()

	if err := run(*resultsDir, *outDir, *dump); err != nil {
		fmt.Fprintln(os.Stderr, "plot:", err)
		os.Exit(1)
	}
}

func run(resultsDir, outDir string, dump bool) error {
	study, err := loadCacheStudy(filepath.Join(resultsDir, cacheStudyFile))
	if err != nil {
		return err
	}
	comparison, err := loadComparison(filepath.Join(resultsDir, compareFile))
	if err != nil {
		return err
	}

	figures := []struct {
		file string
		draw func() (*Canvas, string, string)
	}{
		{"cache-threshold-sweep.svg", func() (*Canvas, string, string) {
			return drawThresholdSweep(study)
		}},
		{"gateway-overhead.svg", func() (*Canvas, string, string) {
			return drawOverhead(comparison)
		}},
	}

	for _, f := range figures {
		canvas, title, desc := f.draw()
		if err := canvas.Validate(); err != nil {
			return fmt.Errorf("%s: %w", f.file, err)
		}
		path := filepath.Join(outDir, f.file)
		if err := os.WriteFile(path, canvas.Bytes(title, desc), 0o600); err != nil {
			return err
		}
		fmt.Println("wrote", path)
		fmt.Println("  checked:", canvas.Report())
		if dump {
			fmt.Print(canvas.Dump())
		}
	}
	return nil
}
