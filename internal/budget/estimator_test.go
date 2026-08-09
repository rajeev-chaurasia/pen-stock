package budget

import (
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/rajeev-chaurasia/pen-stock/internal/pricing"
)

// Expected token counts below are worked out by hand from the estimator's
// constants: four characters per token, rounded up, plus four tokens of
// framing per message. They are written out rather than recomputed from
// the constants, so a change to either one has to be looked at.
const (
	testDefaultCompletion = 256
	testMaxCompletion     = 4096

	// usdEpsilon absorbs binary floating point slop on a decimal figure.
	usdEpsilon = 1e-12
)

func testOpts() EstimatorOptions {
	return EstimatorOptions{
		DefaultCompletionTokens: testDefaultCompletion,
		MaxCompletionTokens:     testMaxCompletion,
	}
}

// userBody wraps a raw JSON content value in a one message request.
func userBody(contentJSON string) []byte {
	return []byte(`{"model":"m","messages":[{"role":"user","content":` + contentJSON + `}]}`)
}

func TestEstimatePromptTokens(t *testing.T) {
	const imagePart = `{"type":"image_url","image_url":{"url":"https://example.com/a-long-url-that-must-not-be-billed.png"}}`

	tests := []struct {
		name string
		body []byte
		want int
	}{
		{
			name: "string content",
			body: userBody(`"hello"`), // 5 chars -> 2, plus 4 framing
			want: 6,
		},
		{
			name: "array content with one text part counts the same as the string",
			body: userBody(`[{"type":"text","text":"hello"}]`),
			want: 6,
		},
		{
			name: "several text parts are summed",
			body: userBody(`[{"type":"text","text":"hell"},{"type":"text","text":"o"}]`),
			want: 6,
		},
		{
			name: "image parts are ignored, not failed on",
			body: userBody(`[{"type":"text","text":"hello"},` + imagePart + `]`),
			want: 6,
		},
		{
			name: "image only turn still costs its framing",
			body: userBody(`[` + imagePart + `]`),
			want: 4,
		},
		{
			name: "empty string content costs framing alone",
			body: userBody(`""`),
			want: 4,
		},
		{
			name: "null content costs framing alone",
			body: userBody(`null`),
			want: 4,
		},
		{
			name: "missing content field costs framing alone",
			body: []byte(`{"model":"m","messages":[{"role":"user"}]}`),
			want: 4,
		},
		{
			name: "every role is charged",
			body: []byte(`{"messages":[{"role":"system","content":"hello"},{"role":"user","content":"hello"}]}`),
			want: 12,
		},
		{
			name: "unknown top level fields do not disturb the count",
			body: []byte(`{"messages":[{"role":"user","content":"hello"}],"temperature":0.4,"stream":true,"tools":[]}`),
			want: 6,
		},
	}

	est := NewEstimator(nil, nil, testOpts())
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := est.Estimate("m", tt.body).PromptTokens; got != tt.want {
				t.Errorf("PromptTokens = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestEstimatePromptScalesLinearly(t *testing.T) {
	est := NewEstimator(nil, nil, testOpts())

	conversation := func(turns int) []byte {
		msgs := make([]string, turns)
		for i := range msgs {
			msgs[i] = `{"role":"user","content":"hello"}`
		}
		return []byte(`{"messages":[` + strings.Join(msgs, ",") + `]}`)
	}

	// One "hello" turn is 6 tokens, so N turns are 6N.
	for _, turns := range []int{1, 10, 100, 1000} {
		t.Run(fmt.Sprintf("%d turns", turns), func(t *testing.T) {
			want := 6 * turns
			if got := est.Estimate("m", conversation(turns)).PromptTokens; got != want {
				t.Errorf("PromptTokens = %d, want %d", got, want)
			}
		})
	}
}

func TestEstimateRoundsUp(t *testing.T) {
	// A partial token is a whole token: rounding down would under reserve
	// on every message that is not an exact multiple of the divisor.
	tests := []struct {
		chars int
		want  int // framing (4) plus ceil(chars/4)
	}{
		{chars: 1, want: 5},
		{chars: 3, want: 5},
		{chars: 4, want: 5},
		{chars: 5, want: 6}, // 1.25 tokens of text must not round to 1
		{chars: 7, want: 6},
		{chars: 8, want: 6},
		{chars: 9, want: 7},
		{chars: 401, want: 105},
	}

	est := NewEstimator(nil, nil, testOpts())
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%d chars", tt.chars), func(t *testing.T) {
			body := userBody(`"` + strings.Repeat("a", tt.chars) + `"`)
			got := est.Estimate("m", body).PromptTokens
			if got != tt.want {
				t.Errorf("PromptTokens = %d, want %d", got, tt.want)
			}
			if got < messageOverheadTokens+tt.chars/charsPerToken {
				t.Errorf("PromptTokens = %d rounded down", got)
			}
		})
	}
}

func TestEstimateCompletionTokens(t *testing.T) {
	tests := []struct {
		name string
		opts EstimatorOptions
		body []byte
		want int
	}{
		{
			name: "max_tokens is honored",
			opts: testOpts(),
			body: []byte(`{"messages":[],"max_tokens":100}`),
			want: 100,
		},
		{
			name: "max_completion_tokens is honored",
			opts: testOpts(),
			body: []byte(`{"messages":[],"max_completion_tokens":250}`),
			want: 250,
		},
		{
			name: "both present: the smaller one binds",
			opts: testOpts(),
			body: []byte(`{"messages":[],"max_tokens":500,"max_completion_tokens":100}`),
			want: 100,
		},
		{
			name: "both present in the other order: still the smaller one",
			opts: testOpts(),
			body: []byte(`{"messages":[],"max_tokens":100,"max_completion_tokens":500}`),
			want: 100,
		},
		{
			name: "no cap uses the configured default",
			opts: testOpts(),
			body: []byte(`{"messages":[{"role":"user","content":"hello"}]}`),
			want: testDefaultCompletion,
		},
		{
			name: "a null cap is no cap",
			opts: testOpts(),
			body: []byte(`{"messages":[],"max_tokens":null}`),
			want: testDefaultCompletion,
		},
		{
			name: "a zero cap is not a cap",
			opts: testOpts(),
			body: []byte(`{"messages":[],"max_tokens":0}`),
			want: testDefaultCompletion,
		},
		{
			name: "a negative cap is not a cap",
			opts: testOpts(),
			body: []byte(`{"messages":[],"max_tokens":-5}`),
			want: testDefaultCompletion,
		},
		{
			name: "an absurd max_tokens is clamped to the ceiling",
			opts: testOpts(),
			body: []byte(`{"messages":[],"max_tokens":1000000}`),
			want: testMaxCompletion,
		},
		{
			name: "an absurd max_completion_tokens is clamped to the ceiling",
			opts: testOpts(),
			body: []byte(`{"messages":[],"max_completion_tokens":1000000}`),
			want: testMaxCompletion,
		},
		{
			name: "the ceiling also clamps the default",
			opts: EstimatorOptions{DefaultCompletionTokens: 8000, MaxCompletionTokens: 512},
			body: []byte(`{"messages":[]}`),
			want: 512,
		},
		{
			name: "no ceiling configured leaves the client cap alone",
			opts: EstimatorOptions{DefaultCompletionTokens: testDefaultCompletion},
			body: []byte(`{"messages":[],"max_tokens":1000000}`),
			want: 1000000,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			est := NewEstimator(nil, nil, tt.opts)
			if got := est.Estimate("m", tt.body).CompletionTokens; got != tt.want {
				t.Errorf("CompletionTokens = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestEstimateUnreadableBody(t *testing.T) {
	// A body that will not parse is charged its own byte length as text,
	// floored, so that nothing unreadable ever estimates at zero.
	tests := []struct {
		name string
		body []byte
		want int
	}{
		{name: "nil body", body: nil, want: unreadableBodyFloorTokens},
		{name: "empty body", body: []byte{}, want: unreadableBodyFloorTokens},
		{name: "truncated json", body: []byte(`{"messages":[{"role":"user",`), want: unreadableBodyFloorTokens},
		{name: "not json at all", body: []byte("not json at all"), want: unreadableBodyFloorTokens},
		{name: "json array instead of an object", body: []byte(`[1,2,3]`), want: unreadableBodyFloorTokens},
		{name: "messages is not an array", body: []byte(`{"messages":"hello"}`), want: unreadableBodyFloorTokens},
		{name: "cap is not a number", body: []byte(`{"messages":[],"max_tokens":"lots"}`), want: unreadableBodyFloorTokens},
		{
			// 8000 bytes of garbage: 4 framing plus ceil(8000/4).
			name: "a large unreadable body is charged above the floor",
			body: []byte(strings.Repeat("x", 8000)),
			want: 2004,
		},
	}

	est := NewEstimator(nil, nil, testOpts())
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := est.Estimate("m", tt.body)
			if got.PromptTokens != tt.want {
				t.Errorf("PromptTokens = %d, want %d", got.PromptTokens, tt.want)
			}
			if got.PromptTokens <= 0 {
				t.Error("an unreadable body estimated at zero prompt tokens would slip past a budget")
			}
			if got.CompletionTokens != testDefaultCompletion {
				t.Errorf("CompletionTokens = %d, want the default %d", got.CompletionTokens, testDefaultCompletion)
			}
		})
	}
}

func TestEstimateWellFormedButEmpty(t *testing.T) {
	// These parse, so they are not charged the unreadable floor. The
	// completion allowance still keeps them from costing nothing.
	tests := []struct {
		name string
		body []byte
	}{
		{name: "empty object", body: []byte(`{}`)},
		{name: "json null", body: []byte(`null`)},
		{name: "no messages", body: []byte(`{"messages":[]}`)},
	}

	est := NewEstimator(nil, nil, testOpts())
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := est.Estimate("m", tt.body)
			if got.PromptTokens != 0 {
				t.Errorf("PromptTokens = %d, want 0", got.PromptTokens)
			}
			if got.CompletionTokens != testDefaultCompletion {
				t.Errorf("CompletionTokens = %d, want %d", got.CompletionTokens, testDefaultCompletion)
			}
		})
	}
}

// testTable is a hand built price list: two priced models, one free one.
func testTable() *pricing.Table {
	return &pricing.Table{
		Version: 3,
		Updated: "2026-01-31",
		Prices: map[string]pricing.Price{
			"openai/gpt-4o-mini":          {InputPerMTok: 0.15, OutputPerMTok: 0.60},
			"anthropic/claude-sonnet-4-5": {InputPerMTok: 3, OutputPerMTok: 15},
			"openai_compat/llmsim-small":  {Free: true},
		},
	}
}

func TestEstimateUSD(t *testing.T) {
	// "hello" is 6 prompt tokens and max_tokens caps the answer at 100.
	body := []byte(`{"messages":[{"role":"user","content":"hello"}],"max_tokens":100}`)

	tests := []struct {
		name    string
		resolve func(string) (string, string)
		model   string
		wantUSD float64
	}{
		{
			name:    "priced model",
			resolve: func(m string) (string, string) { return "openai", m },
			model:   "gpt-4o-mini",
			wantUSD: 6.09e-05, // (6*0.15 + 100*0.60) / 1e6
		},
		{
			name:    "a second kind prices from its own rates",
			resolve: func(m string) (string, string) { return "anthropic", m },
			model:   "claude-sonnet-4-5",
			wantUSD: 0.001518, // (6*3 + 100*15) / 1e6
		},
		{
			name:    "free model costs nothing",
			resolve: func(m string) (string, string) { return "openai_compat", m },
			model:   "llmsim-small",
			wantUSD: 0,
		},
		{
			name:    "unpriced model is never guessed at",
			resolve: func(m string) (string, string) { return "openai", m },
			model:   "gpt-9-turbo",
			wantUSD: 0,
		},
		{
			name:    "unknown kind is unpriced too",
			resolve: func(m string) (string, string) { return "", m },
			model:   "gpt-4o-mini",
			wantUSD: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			est := NewEstimator(testTable(), tt.resolve, testOpts())
			got := est.Estimate(tt.model, body)

			if math.Abs(got.USD-tt.wantUSD) > usdEpsilon {
				t.Errorf("USD = %v, want %v", got.USD, tt.wantUSD)
			}
			// An unpriced model must still carry tokens, or token limits
			// would stop applying to exactly the models nobody priced.
			if got.PromptTokens != 6 {
				t.Errorf("PromptTokens = %d, want 6", got.PromptTokens)
			}
			if got.CompletionTokens != 100 {
				t.Errorf("CompletionTokens = %d, want 100", got.CompletionTokens)
			}
		})
	}
}

func TestEstimateWithoutPricing(t *testing.T) {
	resolve := func(m string) (string, string) { return "openai", m }

	tests := []struct {
		name    string
		table   *pricing.Table
		resolve func(string) (string, string)
	}{
		{name: "nil table and nil resolver", table: nil, resolve: nil},
		{name: "nil table with a resolver", table: nil, resolve: resolve},
		{name: "table with a nil resolver", table: testTable(), resolve: nil},
	}

	body := []byte(`{"messages":[{"role":"user","content":"hello"}],"max_tokens":100}`)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			est := NewEstimator(tt.table, tt.resolve, testOpts())
			got := est.Estimate("gpt-4o-mini", body)

			if got.USD != 0 {
				t.Errorf("USD = %v, want 0", got.USD)
			}
			if got.PromptTokens != 6 || got.CompletionTokens != 100 {
				t.Errorf("tokens = %d/%d, want 6/100", got.PromptTokens, got.CompletionTokens)
			}
		})
	}
}

func TestNewEstimatorZeroOptions(t *testing.T) {
	// Unconfigured options must not estimate the answer at nothing.
	est := NewEstimator(nil, nil, EstimatorOptions{})

	got := est.Estimate("m", []byte(`{"messages":[{"role":"user","content":"hello"}]}`))
	if got.CompletionTokens != fallbackCompletionTokens {
		t.Errorf("CompletionTokens = %d, want the fallback %d", got.CompletionTokens, fallbackCompletionTokens)
	}
	if got.PromptTokens != 6 {
		t.Errorf("PromptTokens = %d, want 6", got.PromptTokens)
	}
}
