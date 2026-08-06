// Package pricing turns provider token usage into USD from a versioned
// list price table, and records what it priced in an append-only ledger.
package pricing

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/rajeev-chaurasia/pen-stock/internal/providers"
)

const (
	// keySeparator splits a table key into provider kind and model. Only
	// the first one separates: model ids on aggregators contain slashes.
	keySeparator = "/"
	// MinVersion is the lowest usable price list version. An absent
	// version field decodes to 0, and a ledger row stamped 0 could not be
	// traced back to any price list.
	MinVersion = 1
	// tokensPerMTok turns a per-million-token rate into a per-token one.
	tokensPerMTok = 1_000_000.0
	// updatedLayout is the ISO date layout accepted for `updated`.
	updatedLayout = "2006-01-02"
)

// Price is one model's list price in USD per million tokens. Free marks a
// model that costs nothing but whose usage still has to be accounted for.
type Price struct {
	InputPerMTok  float64 `yaml:"input_per_mtok"`
	OutputPerMTok float64 `yaml:"output_per_mtok"`
	Free          bool    `yaml:"free"`
}

// Table is a versioned set of model prices keyed by "<kind>/<model>".
type Table struct {
	Version int              `yaml:"version"`
	Updated string           `yaml:"updated"`
	Prices  map[string]Price `yaml:"models"`
}

// Cost is what one completion cost, carrying the price list version that
// produced it so the figure stays explainable after a price change.
type Cost struct {
	USD          float64
	PriceVersion int
	Free         bool
}

// Cost prices usage for one model. The second result is false when the
// model has no entry: an unpriced model is reported as such rather than
// guessed at, so it shows up instead of quietly reading as free.
func (t *Table) Cost(kind, model string, u providers.Usage) (Cost, bool) {
	p, ok := t.Prices[key(kind, model)]
	if !ok {
		return Cost{}, false
	}
	return Cost{
		USD:          costUSD(p, u.PromptTokens, u.CompletionTokens),
		PriceVersion: t.Version,
		Free:         p.Free,
	}, true
}

// Models returns every key in the table, sorted, for operator listings.
func (t *Table) Models() []string {
	return slices.Sorted(maps.Keys(t.Prices))
}

// costUSD prices one completion from per-million-token rates. Token counts
// arrive from upstreams the gateway does not control, so a negative count
// is clamped to zero rather than credited back to the tenant.
func costUSD(p Price, promptTokens, completionTokens int) float64 {
	if p.Free {
		return 0
	}
	in := float64(clampTokens(promptTokens)) * p.InputPerMTok
	out := float64(clampTokens(completionTokens)) * p.OutputPerMTok
	// One division at the end keeps the rounding to a single step.
	return (in + out) / tokensPerMTok
}

func clampTokens(n int) int {
	if n < 0 {
		return 0
	}
	return n
}

func key(kind, model string) string {
	return kind + keySeparator + model
}

// validKey reports whether k is a well formed "<kind>/<model>" key.
func validKey(k string) bool {
	kind, model, found := strings.Cut(k, keySeparator)
	if !found || kind == "" || model == "" {
		return false
	}
	return strings.TrimSpace(kind) == kind && strings.TrimSpace(model) == model
}

// Load reads the price table at path. Unknown YAML fields are rejected so
// a misspelled rate fails loudly instead of pricing at zero.
func Load(path string) (*Table, error) {
	// The path comes from the operator's own configuration, so there is
	// no untrusted input to traverse with.
	// #nosec G304
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open price table: %w", err)
	}
	defer func() { _ = f.Close() }()

	t, err := decode(f)
	if err != nil {
		return nil, fmt.Errorf("parse price table %s: %w", path, err)
	}
	if err := t.Validate(); err != nil {
		return nil, fmt.Errorf("invalid price table %s:\n%w", path, err)
	}
	return t, nil
}

func decode(r io.Reader) (*Table, error) {
	var t Table
	dec := yaml.NewDecoder(r)
	dec.KnownFields(true)
	if err := dec.Decode(&t); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return &t, nil
}

// Validate checks the whole table and reports every problem found, joined
// into a single multi-line error.
func (t *Table) Validate() error {
	var errs []error
	add := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf(format, args...))
	}

	if t.Version < MinVersion {
		add("version is %d, must be at least %d; the version is stamped on every ledger row and 0 would leave that row untraceable", t.Version, MinVersion)
	}
	if t.Updated == "" {
		add("updated is required, as an ISO date such as 2026-01-31")
	} else if _, err := time.Parse(updatedLayout, t.Updated); err != nil {
		add("updated %q is not an ISO date (%s)", t.Updated, updatedLayout)
	}
	if len(t.Prices) == 0 {
		add("models: at least one model price is required")
	}

	// Sorted so a table with several problems reports them in a stable
	// order, which keeps the message diffable.
	for _, k := range t.Models() {
		p := t.Prices[k]
		label := fmt.Sprintf("models[%q]", k)

		if !validKey(k) {
			add(`%s: key must be "<kind>/<model>", for example "openai/gpt-4o-mini"`, label)
		}
		if p.InputPerMTok < 0 {
			add("%s: input_per_mtok is %v, must not be negative", label, p.InputPerMTok)
		}
		if p.OutputPerMTok < 0 {
			add("%s: output_per_mtok is %v, must not be negative", label, p.OutputPerMTok)
		}

		switch {
		case p.Free && (p.InputPerMTok != 0 || p.OutputPerMTok != 0):
			add("%s: free is true but a rate is set; a free model must cost nothing", label)
		case !p.Free && p.InputPerMTok == 0 && p.OutputPerMTok == 0:
			add("%s: input_per_mtok and output_per_mtok are both missing; set them, or mark the model free: true", label)
		}
	}

	return errors.Join(errs...)
}

// embeddedTable is the price table compiled into the binary, and this
// file is the only copy of it. Keeping a second copy at the repository
// root would mean two sources of truth for money, so operators who want
// their own numbers copy this one out and pass it to Load.
//
//go:embed pricing.yaml
var embeddedTable []byte

var (
	defaultOnce  sync.Once
	defaultTable *Table
	defaultErr   error
)

// DefaultTable returns the price table built into the binary, so the
// gateway prices requests with no external file present. The table is
// shared and must be treated as read only; operators who need different
// numbers pass their own file to Load.
func DefaultTable() (*Table, error) {
	defaultOnce.Do(func() {
		t, err := decode(bytes.NewReader(embeddedTable))
		if err != nil {
			defaultErr = fmt.Errorf("parse embedded price table: %w", err)
			return
		}
		if err := t.Validate(); err != nil {
			defaultErr = fmt.Errorf("invalid embedded price table:\n%w", err)
			return
		}
		defaultTable = t
	})
	return defaultTable, defaultErr
}
