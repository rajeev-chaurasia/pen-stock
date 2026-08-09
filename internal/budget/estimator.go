package budget

import (
	"encoding/json"
	"strings"

	"github.com/rajeev-chaurasia/pen-stock/internal/pricing"
	"github.com/rajeev-chaurasia/pen-stock/internal/providers"
)

const (
	// charsPerToken converts characters to tokens. English text runs near
	// four characters per token on every tokenizer the gateway fronts,
	// which is close enough for a reservation that real usage replaces at
	// settle time. Characters are counted as bytes, so text outside ASCII
	// counts high, and that leans the estimate the safe way.
	charsPerToken = 4

	// messageOverheadTokens is what one turn costs before any of its text:
	// the role label and the framing the wire format wraps around every
	// message. Counting only text would undercount a long conversation of
	// short turns by more than the text itself.
	messageOverheadTokens = 4

	// unreadableBodyFloorTokens is the least a body the estimator could
	// not parse is charged. It is roughly a page of prompt: large enough
	// that a stream of malformed bodies cannot be free, small enough not
	// to exhaust a tenant that still has real budget.
	unreadableBodyFloorTokens = 1024

	// fallbackCompletionTokens stands in when the options carry no default
	// of their own. A zero allowance would estimate the answer at nothing,
	// and an answer estimated at nothing is the undershoot the overshoot
	// bound is built to avoid.
	fallbackCompletionTokens = 1024

	// contentPartTypeText selects the text entries out of a multimodal
	// content array. Every other part type carries no characters to bill.
	contentPartTypeText = "text"
)

// EstimatorOptions bounds the completion allowance an estimate may claim.
type EstimatorOptions struct {
	// DefaultCompletionTokens is the allowance assumed when the request
	// names no cap of its own.
	DefaultCompletionTokens int
	// MaxCompletionTokens is the operator's ceiling on that allowance,
	// applied to whatever the client asked for. Zero means no ceiling.
	MaxCompletionTokens int
}

// estimator predicts consumption from an OpenAI style chat body. It holds
// nothing mutable, so one instance serves every request.
type estimator struct {
	table *pricing.Table
	// resolve maps a routed model to the vendor kind and upstream model
	// that will be billed for it.
	resolve func(model string) (kind, upstream string)
	opts    EstimatorOptions
}

// NewEstimator builds an Estimator over an OpenAI style request body.
//
// kindOf maps a routed model name to the provider kind the price table is
// keyed by. Either it or table may be nil, in which case estimates carry
// tokens and zero USD, so token limits keep working with no price list
// configured.
func NewEstimator(table *pricing.Table, resolve func(model string) (kind, upstream string), opts EstimatorOptions) Estimator {
	if opts.DefaultCompletionTokens <= 0 {
		opts.DefaultCompletionTokens = fallbackCompletionTokens
	}
	return &estimator{table: table, resolve: resolve, opts: opts}
}

// Estimate predicts what one request will consume. It never fails: a body
// it cannot read is charged conservatively rather than not at all, because
// an estimate of zero admits the request however little budget is left.
func (e *estimator) Estimate(model string, raw []byte) Estimate {
	var est Estimate

	var req chatRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		est.PromptTokens = unreadablePromptTokens(raw)
		est.CompletionTokens = e.completionTokens(0)
	} else {
		est.PromptTokens = promptTokens(req.Messages)
		est.CompletionTokens = e.completionTokens(req.requestedCompletionCap())
	}

	est.USD = e.usd(model, est)
	return est
}

// chatRequest is the slice of a chat.completions body that bears on cost.
// Everything else a client may send is ignored, so a field the gateway
// does not model cannot turn a priceable request into an unreadable one.
type chatRequest struct {
	Messages []chatMessage `json:"messages"`
	// MaxTokens is the spelling older clients still send;
	// MaxCompletionTokens is the current one.
	MaxTokens           *int `json:"max_tokens"`
	MaxCompletionTokens *int `json:"max_completion_tokens"`
}

// chatMessage keeps Content raw because it is either a string or an array
// of typed parts.
type chatMessage struct {
	Content json.RawMessage `json:"content"`
}

// requestedCompletionCap is the client's own ceiling on the answer, or
// zero when it named none. Both spellings cap the same thing, so when a
// client sends both, the smaller one is the one that binds.
func (r chatRequest) requestedCompletionCap() int {
	limit := 0
	for _, c := range []*int{r.MaxTokens, r.MaxCompletionTokens} {
		// A cap of zero or less is not a cap. Upstreams reject it, and
		// honoring it here would reserve nothing for the answer.
		if c == nil || *c <= 0 {
			continue
		}
		if limit == 0 || *c < limit {
			limit = *c
		}
	}
	return limit
}

// completionTokens is the allowance reserved for the answer.
//
// The clamp to MaxCompletionTokens is what keeps the overshoot bound in
// the package doc small: the operator's ceiling, not the client's cap,
// decides the largest completion any single in flight request can run to,
// so one request cannot carry a tenant far past its budget.
func (e *estimator) completionTokens(requested int) int {
	n := requested
	if n <= 0 {
		n = e.opts.DefaultCompletionTokens
	}
	if e.opts.MaxCompletionTokens > 0 {
		n = min(n, e.opts.MaxCompletionTokens)
	}
	return n
}

// promptTokens charges every message for its text plus its framing. A body
// that parses but names no messages honestly estimates no prompt; the
// completion allowance is what keeps such a request from being free.
func promptTokens(msgs []chatMessage) int {
	total := 0
	for _, m := range msgs {
		total += messageOverheadTokens + tokensForChars(len(textOf(m.Content)))
	}
	return total
}

// unreadablePromptTokens prices a body the estimator could not parse. The
// bytes are real even when the JSON is not, so they are charged as text,
// floored so that a short piece of garbage is not nearly free.
func unreadablePromptTokens(raw []byte) int {
	return max(messageOverheadTokens+tokensForChars(len(raw)), unreadableBodyFloorTokens)
}

// tokensForChars converts a character count to tokens, rounding up. A
// budget that rounds down under reserves on every single message, and
// those shortfalls compound across a conversation.
func tokensForChars(chars int) int {
	if chars <= 0 {
		return 0
	}
	return (chars + charsPerToken - 1) / charsPerToken
}

// textOf pulls the text out of a content field, which is either a bare
// string or an array of typed parts. A non text part carries no characters
// to charge for and is skipped rather than treated as a parse failure,
// because refusing to estimate a request with an image in it would deny
// every multimodal call.
func textOf(content json.RawMessage) string {
	var s string
	if err := json.Unmarshal(content, &s); err == nil {
		return s
	}

	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(content, &parts); err != nil {
		return ""
	}

	var sb strings.Builder
	for _, p := range parts {
		if p.Type == contentPartTypeText {
			sb.WriteString(p.Text)
		}
	}
	return sb.String()
}

// usd prices an estimate through the price table. A model with no entry is
// worth zero USD and keeps its token estimate, so token limits still bind
// on it. A missing price is never guessed at: a guessed figure would be
// reserved, settled, and reported as if it were money.
func (e *estimator) usd(model string, est Estimate) float64 {
	if e.table == nil || e.resolve == nil {
		return 0
	}
	// The routed name is resolved to the vendor and model that will
	// actually be billed. Pricing the routed label instead finds nothing
	// whenever a route carries an alias, and an unpriced estimate
	// reserves nothing, which quietly disables the USD cap.
	kind, upstream := e.resolve(model)
	if upstream != "" {
		model = upstream
	}
	if kind == "" {
		return 0
	}
	cost, ok := e.table.Cost(kind, model, providers.Usage{
		PromptTokens:     est.PromptTokens,
		CompletionTokens: est.CompletionTokens,
	})
	if !ok {
		return 0
	}
	return cost.USD
}
