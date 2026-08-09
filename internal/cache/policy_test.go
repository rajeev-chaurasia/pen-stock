package cache

import "testing"

// Eligibility is asserted by Reason, never only by Cacheable. The reason
// becomes a metric label an operator reads to tell a cache that is cold
// from one that is refusing everything, so it is part of the contract
// rather than a debugging aid.
func TestEligible(t *testing.T) {
	const ceiling = DefaultMaxTemperature

	cases := []struct {
		name string
		body string
		want IneligibleReason
	}{
		{
			name: "a zero temperature request is cacheable",
			body: `{"temperature":0,"messages":[{"role":"user","content":"hi"}]}`,
			want: ReasonEligible,
		},
		{
			// The one that reads like a bug and is not. An absent
			// temperature means the provider's default, which is above zero
			// almost everywhere, so the answer is not reproducible and the
			// gateway cannot promise the same reply twice.
			name: "no temperature at all is refused",
			body: `{"messages":[{"role":"user","content":"hi"}]}`,
			want: ReasonTemperature,
		},
		{
			// The comparison is strictly greater than, so the ceiling
			// itself is allowed and anything above it is not.
			name: "a hair above the ceiling is refused",
			body: `{"temperature":0.0001,"messages":[]}`,
			want: ReasonTemperature,
		},
		{
			name: "tools",
			body: `{"temperature":0,"tools":[{"type":"function"}]}`,
			want: ReasonToolUse,
		},
		{
			name: "tool_choice",
			body: `{"temperature":0,"tool_choice":"auto"}`,
			want: ReasonToolUse,
		},
		{
			name: "functions, the older spelling",
			body: `{"temperature":0,"functions":[{"name":"f"}]}`,
			want: ReasonToolUse,
		},
		{
			// Honoured for the same reason as tool_choice: an old client
			// must not silently get a replayed side effect.
			name: "function_call, the older spelling",
			body: `{"temperature":0,"function_call":{"name":"f"}}`,
			want: ReasonToolUse,
		},
		{
			// Zero is a legitimate pinned seed, not an absent one. Only a
			// nil check is correct here; a value check would cache exactly
			// the requests that asked hardest to be reproducible on their
			// own terms.
			name: "seed zero is a real seed",
			body: `{"temperature":0,"seed":0}`,
			want: ReasonSeeded,
		},
		{
			name: "a non zero seed",
			body: `{"temperature":0,"seed":42}`,
			want: ReasonSeeded,
		},
		{
			name: "logprobs changes the body shape",
			body: `{"temperature":0,"logprobs":true}`,
			want: ReasonStreamOptions,
		},
		{
			name: "logprobs false is not a refusal",
			body: `{"temperature":0,"logprobs":false}`,
			want: ReasonEligible,
		},
		{
			name: "n above one asks for variety by definition",
			body: `{"temperature":0,"n":2}`,
			want: ReasonStreamOptions,
		},
		{
			name: "n of one is fine",
			body: `{"temperature":0,"n":1}`,
			want: ReasonEligible,
		},
		{
			name: "an unreadable body cannot be judged, so it is refused",
			body: `{"temperature":`,
			want: ReasonUnparsable,
		},
		{
			// Not what you might assume: these parse, so they are not
			// unparsable. They then fall through to the temperature check.
			name: "an empty object parses and then lacks a temperature",
			body: `{}`,
			want: ReasonTemperature,
		},
		{
			name: "null parses and then lacks a temperature",
			body: `null`,
			want: ReasonTemperature,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Eligible([]byte(tc.body), ceiling)
			if got.Reason != tc.want {
				t.Errorf("reason = %q, want %q", got.Reason, tc.want)
			}
			if want := tc.want == ReasonEligible; got.Cacheable != want {
				t.Errorf("cacheable = %v, want %v", got.Cacheable, want)
			}
		})
	}
}

// A field that is present but carries nothing is not tool use. Refusing
// on it would exclude every client that serialises empty collections
// rather than omitting them, which is most of them.
func TestEligibleIgnoresEmptyToolFields(t *testing.T) {
	bodies := []string{
		`{"temperature":0,"tools":null}`,
		`{"temperature":0,"tools":[]}`,
		`{"temperature":0,"tool_choice":null}`,
		`{"temperature":0,"functions":[]}`,
		`{"temperature":0,"function_call":null}`,
		`{"temperature":0,"tools":[],"tool_choice":null,"functions":[],"function_call":null}`,
	}
	for _, body := range bodies {
		t.Run(body, func(t *testing.T) {
			if got := Eligible([]byte(body), DefaultMaxTemperature); !got.Cacheable {
				t.Errorf("refused with %q, want it cacheable: an empty field is not tool use", got.Reason)
			}
		})
	}
}

// The checks run in a fixed order and the label an operator sees depends
// on it. A request that is both tool using and hot reports tool use,
// because that is the refusal that matters: raising the temperature
// ceiling would not make it cacheable.
func TestEligibleReportsTheFirstReasonInOrder(t *testing.T) {
	body := `{"temperature":1.5,"seed":7,"tools":[{"type":"function"}]}`
	if got := Eligible([]byte(body), DefaultMaxTemperature); got.Reason != ReasonToolUse {
		t.Errorf("reason = %q, want %q: tool use is checked before seed and temperature", got.Reason, ReasonToolUse)
	}

	// And seed before temperature, for the same reason.
	body = `{"temperature":1.5,"seed":7}`
	if got := Eligible([]byte(body), DefaultMaxTemperature); got.Reason != ReasonSeeded {
		t.Errorf("reason = %q, want %q: seed is checked before temperature", got.Reason, ReasonSeeded)
	}
}

// The ceiling is configurable, so an operator who accepts some variety
// gets what they asked for and nothing above it.
func TestEligibleHonoursAConfiguredCeiling(t *testing.T) {
	const ceiling = 0.3
	cases := []struct {
		temperature string
		cacheable   bool
	}{
		{"0", true},
		{"0.3", true},
		{"0.30001", false},
		{"1", false},
	}
	for _, tc := range cases {
		t.Run(tc.temperature, func(t *testing.T) {
			body := `{"temperature":` + tc.temperature + `}`
			if got := Eligible([]byte(body), ceiling); got.Cacheable != tc.cacheable {
				t.Errorf("cacheable = %v with temperature %s against a %v ceiling, want %v",
					got.Cacheable, tc.temperature, ceiling, tc.cacheable)
			}
		})
	}
}
