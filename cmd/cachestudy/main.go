// Command cachestudy measures how often Penstock's semantic cache tier
// would answer the wrong question.
//
// Published cache hit rates are not results on their own. A semantic
// cache can be tuned to any hit rate you like by lowering its similarity
// threshold, and the cost of doing so is invisible in the hit rate: it
// shows up as confidently wrong answers that nobody traces back to the
// cache. The number that matters is the false hit rate beside it, and
// this command produces it.
//
// The ground truth is built rather than inferred. Replaying a public
// dataset means guessing afterwards whether a hit was correct, usually
// by asking a model, which is the same judgement the cache is being
// tested on. Instead the corpus is hand written in labelled groups: a
// repeat is the same question resent, a paraphrase is the same question
// reworded so the same answer serves, and an opposite is the same
// sentence with one word flipped so that the same answer is wrong. An
// unrelated question is the floor. Nothing has to be judged at scoring
// time because the label was decided when the question was written.
//
// It calls a real embedding API and refuses to run without one, because
// a similarity number is a property of a specific model and a study that
// invented one would be worth nothing.
//
//	GEMINI_API_KEY=... go run ./cmd/cachestudy
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"time"

	"github.com/rajeev-chaurasia/pen-stock/internal/cache"
)

const (
	// embedKeyEnv is the only credential this command reads. It is the
	// same variable the gateway uses, so the study runs against the
	// embedder the gateway would actually be configured with.
	embedKeyEnv = "GEMINI_API_KEY"

	// defaultCorpusPath is the hand written ground truth.
	defaultCorpusPath = "bench/corpus/questions.json"

	// resultsDir is where a run's raw output goes. A figure that is not
	// in a file here has no business being in the writeup.
	resultsDir = "bench/results"

	// defaultEmbedBatch is how many texts go in one batchEmbedContents
	// call, kept well under the API's per request ceiling.
	defaultEmbedBatch int = 25

	// defaultEmbedPerMinute is the item budget the run holds itself to.
	// Gemini's free tier counts each embedded text against a per minute
	// quota of 100, whatever batch it arrived in, so the default leaves
	// headroom rather than discovering the ceiling by hitting it.
	defaultEmbedPerMinute int = 90

	// studyTenant and studyModel identify the requests the study builds.
	// One tenant, because the cache never shares entries across tenants
	// and a second one would only ever add misses.
	studyTenant string = "cachestudy"
	studyModel  string = "cachestudy-model"

	// runStamp formats the timestamp in a result filename.
	runStamp = "20060102T150405Z"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "cachestudy:", err)
		os.Exit(1)
	}
}

// options is the whole command line.
type options struct {
	corpusPath     string
	outPath        string
	minThreshold   float64
	maxThreshold   float64
	step           float64
	embedBaseURL   string
	embedModel     string
	embedBatch     int
	embedPerMinute int
	completions    completionOptions
	priced         bool
}

func parseFlags() options {
	var o options
	flag.StringVar(&o.corpusPath, "corpus", defaultCorpusPath, "labelled question corpus")
	flag.StringVar(&o.outPath, "out", "", "result file, defaults to "+resultsDir+"/cache-study-<timestamp>.json")
	flag.Float64Var(&o.minThreshold, "min", 0.80, "lowest similarity threshold in the sweep")
	flag.Float64Var(&o.maxThreshold, "max", 0.99, "highest similarity threshold in the sweep")
	flag.Float64Var(&o.step, "step", 0.01, "sweep step")
	flag.StringVar(&o.embedBaseURL, "embed-base-url", "", "embedder base URL, empty for the package default")
	flag.StringVar(&o.embedModel, "embed-model", "", "embedding model, empty for the package default")
	flag.IntVar(&o.embedBatch, "embed-batch", defaultEmbedBatch, "texts per embedding request")
	flag.IntVar(&o.embedPerMinute, "embed-per-minute", defaultEmbedPerMinute, "embedded texts per minute, 0 for no pacing")
	flag.BoolVar(&o.priced, "completions", false, "generate a real answer per question so hit rates can be priced in tokens")
	flag.StringVar(&o.completions.BaseURL, "completions-url", "http://127.0.0.1:8100/v1", "OpenAI compatible endpoint for -completions")
	flag.StringVar(&o.completions.Model, "completions-model", "local", "model name sent to -completions-url")
	flag.IntVar(&o.completions.MaxTokens, "completions-max-tokens", 160, "answer length cap for -completions")
	flag.IntVar(&o.completions.Concurrency, "completions-concurrency", 4, "parallel generations for -completions")
	flag.Parse()
	return o
}

func run() error {
	opts := parseFlags()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	corpus, err := LoadCorpus(opts.corpusPath)
	if err != nil {
		return err
	}
	thresholds, err := Thresholds(opts.minThreshold, opts.maxThreshold, opts.step)
	if err != nil {
		return err
	}

	key := os.Getenv(embedKeyEnv)
	if key == "" {
		// Refusing is the point. A run without an embedder could only
		// produce made up similarities, and a made up similarity in a
		// study about false hit rates would be the exact dishonesty the
		// study exists to call out.
		return fmt.Errorf(
			"%s is not set. This study measures a real embedding model and will not fabricate similarities. "+
				"Export a key and rerun: %s=... go run ./cmd/cachestudy", embedKeyEnv, embedKeyEnv)
	}
	embedder := cache.NewGeminiEmbedder(opts.embedBaseURL, key, opts.embedModel, nil)

	texts, err := promptTexts(corpus, studyModel)
	if err != nil {
		return err
	}

	// Pricing runs first because it is the free half. Embedding spends a
	// metered quota, and spending it only to fail on a local model that
	// was never up wastes the part of the run that costs something.
	completions, summary, err := priceQuestions(ctx, opts, corpus)
	if err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "embedding %d distinct prompts once, for %d thresholds, at up to %d per minute\n",
		len(texts), len(thresholds), opts.embedPerMinute)
	vectors, err := embedAll(ctx, embedder, texts, opts.embedBatch, opts.embedPerMinute, progress("embedded"))
	if err != nil {
		return err
	}

	study, err := NewStudy(corpus, studyTenant, studyModel, vectors, completions)
	if err != nil {
		return err
	}

	exact, err := study.RunExactTier(ctx)
	if err != nil {
		return err
	}
	sweep, err := study.Sweep(ctx, thresholds)
	if err != nil {
		return err
	}
	similarity := study.Similarity()

	result := &Result{
		Schema:      resultSchema,
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Corpus: CorpusSummary{
			Path:            filepath.ToSlash(opts.corpusPath),
			Groups:          len(corpus.Groups),
			ProbesByLabel:   stringKeyed(corpus.LabelCounts()),
			GroupsByDomain:  corpus.DomainCounts(),
			DistinctPrompts: len(texts),
		},
		Embedder: EmbedderSummary{
			BaseURL:    baseURLOr(opts.embedBaseURL),
			Model:      modelOr(opts.embedModel),
			Dimensions: observedDimensions(vectors, embedder),
			BatchSize:  opts.embedBatch,
		},
		Request: RequestSummary{
			Tenant:      studyTenant,
			RoutedModel: studyModel,
			Temperature: "0",
			// The gateway embeds the role prefixed conversation text, not
			// the bare question, so the study does too.
			PromptTextForm: "user:<question>",
		},
		Completions: summary,
		ExactTier:   stringKeyedRows(exact),
		Similarity:  similarity,
		Sweep:       sweep,
		Verdict:     buildVerdict(sweep, similarity.Separable),
		Checks:      study.Verify(ctx),
	}

	outPath := opts.outPath
	if outPath == "" {
		outPath = filepath.Join(resultsDir, "cache-study-"+time.Now().UTC().Format(runStamp)+".json")
	}
	if err := WriteResult(outPath, result); err != nil {
		return err
	}
	if err := PrintReport(os.Stdout, result, filepath.ToSlash(outPath)); err != nil {
		return err
	}
	if result.Checks.CosineDisagreements > 0 {
		return errors.New("the study disagreed with the production store's similarity; the result is not usable")
	}
	return nil
}

// priceQuestions optionally generates a real answer for every question,
// so a hit rate can be turned into tokens avoided. It is optional
// because the false hit curve is the result and the token table is
// commentary on it.
func priceQuestions(ctx context.Context, opts options, corpus *Corpus) (map[string]completion, *CompletionsSummary, error) {
	if !opts.priced {
		return nil, nil, nil
	}
	questions := distinctQuestions(corpus)
	fmt.Fprintf(os.Stderr, "generating %d answers from %s to price the hits\n", len(questions), opts.completions.BaseURL)

	got, err := fetchCompletions(ctx, opts.completions, questions, progress("generated"))
	if err != nil {
		return nil, nil, err
	}

	total := 0
	for _, c := range got {
		total += c.Tokens
	}
	return got, &CompletionsSummary{
		BaseURL:     opts.completions.BaseURL,
		Model:       opts.completions.Model,
		MaxTokens:   opts.completions.MaxTokens,
		Questions:   len(got),
		TotalTokens: total,
		MeanTokens:  ratio(total, len(got)),
	}, nil
}

// distinctQuestions lists every question in the corpus once. A repeat is
// its anchor's text, so it needs no answer of its own.
func distinctQuestions(c *Corpus) []string {
	seen := make(map[string]struct{})
	var out []string
	add := func(q string) {
		if _, dup := seen[q]; dup {
			return
		}
		seen[q] = struct{}{}
		out = append(out, q)
	}
	for i := range c.Groups {
		add(c.Groups[i].Anchor)
		for _, p := range c.Groups[i].Probes {
			add(p.Text)
		}
	}
	return out
}

// progress reports a long running stage on stderr, so stdout carries
// only the report.
func progress(verb string) func(done, total int) {
	return func(done, total int) {
		fmt.Fprintf(os.Stderr, "\r  %s %d/%d", verb, done, total)
		if done == total {
			fmt.Fprintln(os.Stderr)
		}
	}
}

// observedDimensions reports the width the run actually saw, falling
// back to the width the embedder claims when nothing was embedded.
func observedDimensions(vectors map[string][]float32, embedder cache.Embedder) int {
	for _, v := range vectors {
		return len(v)
	}
	return embedder.Dimensions()
}

func baseURLOr(configured string) string {
	if configured == "" {
		return cache.DefaultEmbedBaseURL
	}
	return configured
}

func modelOr(configured string) string {
	if configured == "" {
		return cache.DefaultEmbedModel
	}
	return configured
}

func stringKeyed(in map[Label]int) map[string]int {
	out := make(map[string]int, len(in))
	for k, v := range in {
		out[string(k)] = v
	}
	return out
}

func stringKeyedRows(in map[Label]TierRow) map[string]TierRow {
	out := make(map[string]TierRow, len(in))
	for k, v := range in {
		out[string(k)] = v
	}
	return out
}
