package openaiwire

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/rajeev-chaurasia/pen-stock/internal/config"
)

// unchanged marks a case whose body must reach the upstream byte for
// byte, which is stricter than JSON equality and is the point of those
// cases.
const unchanged = ""

func TestWithStreamUsage(t *testing.T) {
	const streamBody = `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`

	cases := []struct {
		name string
		kind config.ProviderKind
		in   string
		// want is the body the upstream must receive, compared as JSON.
		// Empty means the request must be forwarded verbatim.
		want string
	}{
		{
			name: "openai stream gets the opt in the client omitted",
			kind: config.KindOpenAI,
			in:   streamBody,
			want: `{"model":"m","stream":true,"stream_options":{"include_usage":true},` +
				`"messages":[{"role":"user","content":"hi"}]}`,
		},
		{
			name: "groq stream gets the opt in",
			kind: config.KindGroq,
			in:   streamBody,
			want: `{"model":"m","stream":true,"stream_options":{"include_usage":true},` +
				`"messages":[{"role":"user","content":"hi"}]}`,
		},
		{
			name: "cerebras stream gets the opt in",
			kind: config.KindCerebras,
			in:   streamBody,
			want: `{"model":"m","stream":true,"stream_options":{"include_usage":true},` +
				`"messages":[{"role":"user","content":"hi"}]}`,
		},
		{
			name: "openrouter stream gets the opt in",
			kind: config.KindOpenRouter,
			in:   streamBody,
			want: `{"model":"m","stream":true,"stream_options":{"include_usage":true},` +
				`"messages":[{"role":"user","content":"hi"}]}`,
		},
		{
			// Mistral has no stream_options, so sending one would trade a
			// working stream for token counts.
			name: "mistral is left alone",
			kind: config.KindMistral,
			in:   streamBody,
			want: unchanged,
		},
		{
			name: "openai_compat is left alone",
			kind: config.KindOpenAICompat,
			in:   streamBody,
			want: unchanged,
		},
		{
			name: "explicit true is preserved",
			kind: config.KindOpenAI,
			in:   `{"model":"m","stream":true,"stream_options":{"include_usage":true}}`,
			want: unchanged,
		},
		{
			// Opting out is the caller's decision, not the gateway's.
			name: "explicit false is preserved",
			kind: config.KindOpenAI,
			in:   `{"model":"m","stream":true,"stream_options":{"include_usage":false}}`,
			want: unchanged,
		},
		{
			name: "sibling stream option survives alongside the opt in",
			kind: config.KindOpenAI,
			in:   `{"model":"m","stream":true,"stream_options":{"chunk_size":8}}`,
			want: `{"model":"m","stream":true,"stream_options":{"chunk_size":8,"include_usage":true}}`,
		},
		{
			name: "non stream request is untouched",
			kind: config.KindOpenAI,
			in:   `{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
			want: unchanged,
		},
		{
			name: "explicit stream false is untouched",
			kind: config.KindOpenAI,
			in:   `{"model":"m","stream":false}`,
			want: unchanged,
		},
		{
			name: "stream_options of the wrong shape is left to the upstream",
			kind: config.KindOpenAI,
			in:   `{"model":"m","stream":true,"stream_options":null}`,
			want: unchanged,
		},
		{
			name: "body that is not an object is forwarded as is",
			kind: config.KindOpenAI,
			in:   `["not","a","request"]`,
			want: unchanged,
		},
		{
			name: "unparseable body is forwarded as is",
			kind: config.KindOpenAI,
			in:   `{"model":`,
			want: unchanged,
		},
		{
			// Everything the gateway does not model has to survive,
			// including vendor extensions and exact numeric notation.
			name: "unknown and awkward fields survive the round trip",
			kind: config.KindOpenAI,
			in: `{"model":"m","stream":true,"seed":42,"temperature":0.70,` +
				`"top_logprobs":null,"logit_bias":{"1234":-100},"stop":["\n\n","END"],` +
				`"huge_id":12345678901234567890,"vendor_ext":{"nested":{"on":true}},` +
				`"messages":[{"role":"user","content":"hi"}]}`,
			want: `{"model":"m","stream":true,"stream_options":{"include_usage":true},` +
				`"seed":42,"temperature":0.70,` +
				`"top_logprobs":null,"logit_bias":{"1234":-100},"stop":["\n\n","END"],` +
				`"huge_id":12345678901234567890,"vendor_ext":{"nested":{"on":true}},` +
				`"messages":[{"role":"user","content":"hi"}]}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prof, ok := profiles[tc.kind]
			if !ok {
				t.Fatalf("kind %q has no profile", tc.kind)
			}

			got := withStreamUsage(json.RawMessage(tc.in), prof)
			if tc.want == unchanged {
				if string(got) != tc.in {
					t.Fatalf("body = %s, want the request forwarded verbatim %s", got, tc.in)
				}
				return
			}
			assertJSONEqual(t, got, []byte(tc.want))
		})
	}
}

// TestWithStreamUsageKeepsNumericNotation guards the re-marshal step: a
// float64 round trip would quietly rewrite an integer too large for one.
func TestWithStreamUsageKeepsNumericNotation(t *testing.T) {
	const in = `{"stream":true,"huge_id":12345678901234567890,"exp":1e3}`

	got := string(withStreamUsage(json.RawMessage(in), profiles[config.KindOpenAI]))
	for _, literal := range []string{"12345678901234567890", "1e3"} {
		if !strings.Contains(got, literal) {
			t.Errorf("body = %s, want the literal %s preserved", got, literal)
		}
	}
}

// assertJSONEqual compares two bodies by value, since re-marshaling a map
// reorders keys. Numbers stay in their original notation so a rewritten
// literal still shows up as a difference.
func assertJSONEqual(t *testing.T, got, want []byte) {
	t.Helper()
	gotValue := decodeAny(t, got)
	wantValue := decodeAny(t, want)
	if !reflect.DeepEqual(gotValue, wantValue) {
		t.Errorf("body = %s, want %s", got, want)
	}
}

func decodeAny(t *testing.T, raw []byte) any {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return value
}
