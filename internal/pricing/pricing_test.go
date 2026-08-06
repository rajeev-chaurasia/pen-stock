package pricing

import (
	"errors"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/rajeev-chaurasia/pen-stock/internal/providers"
)

// usdEpsilon absorbs the last bit of binary floating point slop. Decimal
// cents are not exactly representable in a float64, so an equality check
// on a USD figure has to allow a hair of error.
const usdEpsilon = 1e-12

func fixture(name string) string {
	return filepath.Join("testdata", name)
}

func loadFixture(t *testing.T, name string) *Table {
	t.Helper()
	tbl, err := Load(fixture(name))
	if err != nil {
		t.Fatalf("Load(%s): %v", name, err)
	}
	return tbl
}

func TestLoadValid(t *testing.T) {
	tbl := loadFixture(t, "valid.yaml")

	if got, want := tbl.Version, 7; got != want {
		t.Errorf("Version = %d, want %d", got, want)
	}
	if got, want := tbl.Updated, "2026-03-14"; got != want {
		t.Errorf("Updated = %q, want %q", got, want)
	}
	if got, want := len(tbl.Prices), 5; got != want {
		t.Fatalf("len(Prices) = %d, want %d", got, want)
	}
	if got, want := tbl.Prices["openai/gpt-4o-mini"].InputPerMTok, 0.15; got != want {
		t.Errorf("gpt-4o-mini input_per_mtok = %v, want %v", got, want)
	}
	if !tbl.Prices["openai_compat/llmsim-small"].Free {
		t.Error("llmsim-small Free = false, want true")
	}
}

func TestCost(t *testing.T) {
	tbl := loadFixture(t, "valid.yaml")

	tests := []struct {
		name    string
		kind    string
		model   string
		usage   providers.Usage
		wantUSD float64
		free    bool
	}{
		{
			name:    "one million tokens each way",
			kind:    "openai",
			model:   "gpt-4o-mini",
			usage:   providers.Usage{PromptTokens: 1_000_000, CompletionTokens: 1_000_000},
			wantUSD: 0.75, // 0.15 in + 0.60 out
		},
		{
			name:    "sub cent request",
			kind:    "openai",
			model:   "gpt-4o-mini",
			usage:   providers.Usage{PromptTokens: 1000, CompletionTokens: 500},
			wantUSD: 0.00045, // (1000*0.15 + 500*0.60) / 1e6
		},
		{
			name:    "asymmetric rates",
			kind:    "groq",
			model:   "llama-3.3-70b-versatile",
			usage:   providers.Usage{PromptTokens: 12345, CompletionTokens: 6789},
			wantUSD: 0.01264686, // (12345*0.59 + 6789*0.79) / 1e6
		},
		{
			name:    "large prompt",
			kind:    "anthropic",
			model:   "claude-sonnet-4-5",
			usage:   providers.Usage{PromptTokens: 250_000, CompletionTokens: 10_000},
			wantUSD: 0.9, // (250000*3 + 10000*15) / 1e6
		},
		{
			name:    "model id containing a slash",
			kind:    "groq",
			model:   "openai/gpt-oss-120b",
			usage:   providers.Usage{PromptTokens: 2_000_000, CompletionTokens: 1_000_000},
			wantUSD: 1.05, // 2*0.15 + 0.75
		},
		{
			name:    "free tier still reports a price version",
			kind:    "openai_compat",
			model:   "llmsim-small",
			usage:   providers.Usage{PromptTokens: 900_000, CompletionTokens: 400_000},
			wantUSD: 0,
			free:    true,
		},
		{
			name:    "no usage costs nothing",
			kind:    "openai",
			model:   "gpt-4o-mini",
			usage:   providers.Usage{},
			wantUSD: 0,
		},
		{
			name:    "negative counts clamp to zero rather than crediting back",
			kind:    "openai",
			model:   "gpt-4o-mini",
			usage:   providers.Usage{PromptTokens: -100_000, CompletionTokens: 1000},
			wantUSD: 0.0006, // (0 + 1000*0.60) / 1e6
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := tbl.Cost(tt.kind, tt.model, tt.usage)
			if !ok {
				t.Fatalf("Cost(%q, %q) not found", tt.kind, tt.model)
			}
			if math.Abs(got.USD-tt.wantUSD) > usdEpsilon {
				t.Errorf("USD = %v, want %v", got.USD, tt.wantUSD)
			}
			if got, want := got.PriceVersion, tbl.Version; got != want {
				t.Errorf("PriceVersion = %d, want %d", got, want)
			}
			if got.Free != tt.free {
				t.Errorf("Free = %v, want %v", got.Free, tt.free)
			}
		})
	}
}

// An unpriced model must be visible as unpriced. Returning a zero cost
// with ok would make it indistinguishable from a free tier model.
func TestCostUnknownModel(t *testing.T) {
	tbl := loadFixture(t, "valid.yaml")

	tests := []struct{ kind, model string }{
		{"openai", "gpt-4o"},
		{"", "gpt-4o-mini"},
		{"openai", ""},
		{"groq", "openai"},                      // prefix of a real key
		{"openai/gpt-4o-mini", ""},              // slash smuggled into the kind
		{"cerebras", "llama-3.3-70b-versatile"}, // right model, wrong kind
	}

	for _, tt := range tests {
		got, ok := tbl.Cost(tt.kind, tt.model, providers.Usage{PromptTokens: 100})
		if ok {
			t.Errorf("Cost(%q, %q) = %+v, true; want found=false", tt.kind, tt.model, got)
		}
		if got != (Cost{}) {
			t.Errorf("Cost(%q, %q) = %+v, want the zero Cost", tt.kind, tt.model, got)
		}
	}
}

func TestModelsSorted(t *testing.T) {
	tbl := loadFixture(t, "valid.yaml")

	got := tbl.Models()
	want := []string{
		"anthropic/claude-sonnet-4-5",
		"groq/llama-3.3-70b-versatile",
		"groq/openai/gpt-oss-120b",
		"openai/gpt-4o-mini",
		"openai_compat/llmsim-small",
	}
	if !slices.Equal(got, want) {
		t.Errorf("Models() = %v, want %v", got, want)
	}
}

func TestLoadErrors(t *testing.T) {
	tests := []struct {
		name         string
		path         string
		wantContains []string
	}{
		{
			name:         "missing file",
			path:         fixture("does_not_exist.yaml"),
			wantContains: []string{"open price table"},
		},
		{
			name:         "unknown yaml field",
			path:         fixture("unknown_field.yaml"),
			wantContains: []string{"parse price table", "input_per_1k", "not found"},
		},
		{
			name:         "empty file",
			path:         fixture("empty.yaml"),
			wantContains: []string{"invalid price table", "version is 0", "updated is required", "at least one model price"},
		},
		{
			name: "every validation failure is collected",
			path: fixture("invalid.yaml"),
			wantContains: []string{
				"version is 0",
				`updated "March 2026" is not an ISO date`,
				`models["no-slash-here"]: key must be`,
				`models["openai/negative"]: input_per_mtok is -1`,
				`models["openai/unpriced"]: input_per_mtok and output_per_mtok are both missing`,
				`models["openai/free-but-priced"]: free is true but a rate is set`,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(tt.path)
			if err == nil {
				t.Fatal("Load = nil error, want error")
			}
			for _, want := range tt.wantContains {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not contain %q", err, want)
				}
			}
		})
	}
}

func validTable() *Table {
	return &Table{
		Version: 3,
		Updated: "2026-01-31",
		Prices: map[string]Price{
			"openai/gpt-4o-mini": {InputPerMTok: 0.15, OutputPerMTok: 0.60},
		},
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name         string
		mutate       func(*Table)
		wantContains []string
	}{
		{
			name:   "valid",
			mutate: func(*Table) {},
		},
		{
			name:   "free entry needs no rates",
			mutate: func(tbl *Table) { tbl.Prices["openrouter/x:free"] = Price{Free: true} },
		},
		{
			name:         "version below minimum",
			mutate:       func(tbl *Table) { tbl.Version = 0 },
			wantContains: []string{"version is 0, must be at least 1"},
		},
		{
			name:         "negative version",
			mutate:       func(tbl *Table) { tbl.Version = -2 },
			wantContains: []string{"version is -2"},
		},
		{
			name:         "missing updated",
			mutate:       func(tbl *Table) { tbl.Updated = "" },
			wantContains: []string{"updated is required"},
		},
		{
			name:         "malformed updated",
			mutate:       func(tbl *Table) { tbl.Updated = "31/01/2026" },
			wantContains: []string{`updated "31/01/2026" is not an ISO date`},
		},
		{
			name:         "empty models map",
			mutate:       func(tbl *Table) { tbl.Prices = map[string]Price{} },
			wantContains: []string{"at least one model price is required"},
		},
		{
			name:         "nil models map",
			mutate:       func(tbl *Table) { tbl.Prices = nil },
			wantContains: []string{"at least one model price is required"},
		},
		{
			name:         "key without a separator",
			mutate:       func(tbl *Table) { tbl.Prices["gpt-4o"] = Price{InputPerMTok: 1} },
			wantContains: []string{`models["gpt-4o"]: key must be`},
		},
		{
			name:         "key with an empty kind",
			mutate:       func(tbl *Table) { tbl.Prices["/gpt-4o"] = Price{InputPerMTok: 1} },
			wantContains: []string{`models["/gpt-4o"]: key must be`},
		},
		{
			name:         "key with an empty model",
			mutate:       func(tbl *Table) { tbl.Prices["openai/"] = Price{InputPerMTok: 1} },
			wantContains: []string{`models["openai/"]: key must be`},
		},
		{
			name:         "key with surrounding space",
			mutate:       func(tbl *Table) { tbl.Prices["openai/ gpt-4o"] = Price{InputPerMTok: 1} },
			wantContains: []string{`models["openai/ gpt-4o"]: key must be`},
		},
		{
			name:         "negative input rate",
			mutate:       func(tbl *Table) { tbl.Prices["openai/a"] = Price{InputPerMTok: -0.5, OutputPerMTok: 1} },
			wantContains: []string{`models["openai/a"]: input_per_mtok is -0.5, must not be negative`},
		},
		{
			name:         "negative output rate",
			mutate:       func(tbl *Table) { tbl.Prices["openai/a"] = Price{InputPerMTok: 1, OutputPerMTok: -2} },
			wantContains: []string{`models["openai/a"]: output_per_mtok is -2, must not be negative`},
		},
		{
			name:         "both rates missing",
			mutate:       func(tbl *Table) { tbl.Prices["openai/a"] = Price{} },
			wantContains: []string{`models["openai/a"]: input_per_mtok and output_per_mtok are both missing`},
		},
		{
			name:         "free with a rate set",
			mutate:       func(tbl *Table) { tbl.Prices["openai/a"] = Price{OutputPerMTok: 3, Free: true} },
			wantContains: []string{`models["openai/a"]: free is true but a rate is set`},
		},
		{
			name: "problems are reported together",
			mutate: func(tbl *Table) {
				tbl.Version = 0
				tbl.Updated = "yesterday"
				tbl.Prices["nope"] = Price{InputPerMTok: -1}
			},
			wantContains: []string{
				"version is 0",
				"is not an ISO date",
				`models["nope"]: key must be`,
				`models["nope"]: input_per_mtok is -1`,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tbl := validTable()
			tt.mutate(tbl)
			err := tbl.Validate()

			if len(tt.wantContains) == 0 {
				if err != nil {
					t.Fatalf("Validate: %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatal("Validate = nil, want error")
			}
			for _, want := range tt.wantContains {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not contain %q", err, want)
				}
			}
		})
	}
}

// TestDefaultTable guards the shipped price table against drift: the file
// compiled into the binary has to parse and validate like any other.
func TestDefaultTable(t *testing.T) {
	tbl, err := DefaultTable()
	if err != nil {
		t.Fatalf("DefaultTable: %v", err)
	}
	if tbl.Version < MinVersion {
		t.Errorf("Version = %d, want at least %d", tbl.Version, MinVersion)
	}
	if len(tbl.Prices) == 0 {
		t.Fatal("DefaultTable has no prices")
	}

	// The gateway ships adapters for these kinds, so each needs at least
	// one priced model or its traffic lands in the ledger unpriced.
	kinds := map[string]bool{}
	for _, k := range tbl.Models() {
		kind, _, _ := strings.Cut(k, keySeparator)
		kinds[kind] = true
	}
	for _, want := range []string{"openai", "anthropic", "gemini", "groq", "cerebras", "mistral", "openrouter"} {
		if !kinds[want] {
			t.Errorf("default table prices no %s model", want)
		}
	}

	cost, ok := tbl.Cost("groq", "llama-3.3-70b-versatile", providers.Usage{PromptTokens: 1_000_000})
	if !ok {
		t.Fatal("default table does not price groq/llama-3.3-70b-versatile")
	}
	if cost.USD <= 0 {
		t.Errorf("USD = %v, want a positive cost for a paid model", cost.USD)
	}
	if cost.PriceVersion != tbl.Version {
		t.Errorf("PriceVersion = %d, want %d", cost.PriceVersion, tbl.Version)
	}
}

// Load has to reject a directory rather than treat it as an empty table.
func TestLoadDirectory(t *testing.T) {
	_, err := Load(t.TempDir())
	if err == nil {
		t.Fatal("Load(dir) = nil error, want error")
	}
	if !strings.Contains(err.Error(), "price table") {
		t.Errorf("error %q does not mention the price table", err)
	}
}

func TestLoadMissingFileUnwraps(t *testing.T) {
	_, err := Load(fixture("does_not_exist.yaml"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Load error = %v, want one wrapping os.ErrNotExist", err)
	}
}
