// Package cache answers a request from a previous one when doing so is
// safe, and refuses to when it is not.
//
// The refusals matter more than the hits. A cache that returns the wrong
// answer is worse than no cache at all: it is a correctness bug that
// looks like a latency win, and it is invisible until someone notices
// their model has stopped listening to them.
//
// # What is never cached
//
// A request is only eligible when its answer is reproducible and belongs
// to nobody but its own tenant. That rules out:
//
//   - Sampling that was asked to vary. A temperature above the
//     configured ceiling means the caller wants a different answer each
//     time, and handing back a stored one silently overrides that.
//   - Tool and function calls. The answer is an instruction to act, and
//     acting twice on one stored decision is not a cache hit, it is a
//     replayed side effect.
//   - Anything carrying a seed, since a caller pinning a seed is asking
//     the provider to decide, not the gateway.
//
// # Tenancy
//
// Entries are keyed per tenant and never shared across them. Two tenants
// asking the same question get their own entries, which costs a little
// hit rate and buys the guarantee that one tenant's prompt or answer can
// never surface in another's response. That trade is not configurable,
// because the failure it prevents is a data leak rather than a
// performance regression.
package cache

import (
	"encoding/json"
	"strings"
)

// DefaultMaxTemperature is the highest temperature considered
// reproducible enough to cache. Zero asks for the most likely answer
// every time, which is the only setting where a stored reply is the
// same answer the provider would have given.
const DefaultMaxTemperature = 0.0

// Eligibility reports whether a request may be cached, and why not when
// it may not. The reason is surfaced as a metric label so an operator
// can see whether a low hit rate is the cache missing or the policy
// correctly refusing.
type Eligibility struct {
	Cacheable bool
	Reason    IneligibleReason
}

// IneligibleReason names the policy that refused.
type IneligibleReason string

const (
	ReasonEligible      IneligibleReason = ""
	ReasonTemperature   IneligibleReason = "temperature_too_high"
	ReasonToolUse       IneligibleReason = "tool_use"
	ReasonSeeded        IneligibleReason = "seeded"
	ReasonStreamOptions IneligibleReason = "unsupported_options"
	ReasonUnparsable    IneligibleReason = "unparsable_request"
)

// requestShape is the part of a chat request that decides eligibility.
// Everything here either changes the answer or changes whether the
// answer may be reused.
type requestShape struct {
	Temperature *float64        `json:"temperature"`
	TopP        *float64        `json:"top_p"`
	N           *int            `json:"n"`
	Seed        *int64          `json:"seed"`
	Tools       json.RawMessage `json:"tools"`
	ToolChoice  json.RawMessage `json:"tool_choice"`
	Functions   json.RawMessage `json:"functions"`
	// FunctionCall is the older spelling of tool_choice and has to be
	// honored too, or an old client silently gets replayed side effects.
	FunctionCall json.RawMessage `json:"function_call"`
	// Logprobs changes the response body even when the text matches, so
	// a cached reply would be missing fields the caller asked for.
	Logprobs *bool `json:"logprobs"`
}

// Eligible reports whether raw may be served from or stored in the
// cache, under the given temperature ceiling.
//
// An unparsable body is refused rather than cached: the gateway
// forwards bodies it does not fully model, and a body it cannot read is
// one whose cache safety it cannot judge.
func Eligible(raw []byte, maxTemperature float64) Eligibility {
	var shape requestShape
	if err := json.Unmarshal(raw, &shape); err != nil {
		return Eligibility{Reason: ReasonUnparsable}
	}

	if hasContent(shape.Tools) || hasContent(shape.ToolChoice) ||
		hasContent(shape.Functions) || hasContent(shape.FunctionCall) {
		return Eligibility{Reason: ReasonToolUse}
	}
	if shape.Seed != nil {
		return Eligibility{Reason: ReasonSeeded}
	}
	if shape.Logprobs != nil && *shape.Logprobs {
		return Eligibility{Reason: ReasonStreamOptions}
	}
	// A request asking for several completions is asking for variety by
	// definition.
	if shape.N != nil && *shape.N > 1 {
		return Eligibility{Reason: ReasonStreamOptions}
	}
	// An absent temperature means the provider's default, which is not
	// zero for most of them, so it is not reproducible either.
	if shape.Temperature == nil || *shape.Temperature > maxTemperature {
		return Eligibility{Reason: ReasonTemperature}
	}
	return Eligibility{Cacheable: true, Reason: ReasonEligible}
}

// hasContent reports whether a raw JSON field carries anything beyond
// null, an empty array, or an empty object. A client that sends
// "tools": [] has not asked for tools.
func hasContent(raw json.RawMessage) bool {
	trimmed := strings.TrimSpace(string(raw))
	switch trimmed {
	case "", "null", "[]", "{}":
		return false
	default:
		return true
	}
}
