package openaiwire

import (
	"bytes"
	"encoding/json"
)

const (
	fieldStream        = "stream"
	fieldStreamOptions = "stream_options"
	fieldIncludeUsage  = "include_usage"
)

// withStreamUsage opts a streaming request into upstream token
// accounting. Per tenant budgets are the whole reason: on the OpenAI
// wire a stream reports no usage at all unless the request asked for it,
// and a budget that cannot see what a call cost is decoration. Callers
// almost never ask, so the gateway asks for them.
//
// A value the client set is never disturbed, including an explicit
// false: declining usage is the caller's call to make. The body round
// trips through a generic map so fields this gateway does not model
// reach the upstream anyway.
func withStreamUsage(raw json.RawMessage, prof profile) json.RawMessage {
	if !prof.streamUsage {
		return raw
	}
	body := decodeObject(raw)
	if body == nil {
		return raw
	}
	// stream_options is meaningful only on a streaming call, and some
	// upstreams reject it outright on the others.
	if stream, ok := body[fieldStream].(bool); !ok || !stream {
		return raw
	}

	opts, ok := body[fieldStreamOptions].(map[string]any)
	if !ok {
		if _, present := body[fieldStreamOptions]; present {
			// Present but not an object: the client owns that field, and
			// the upstream is the right judge of what it means.
			return raw
		}
		opts = make(map[string]any, 1)
	}
	if _, set := opts[fieldIncludeUsage]; set {
		return raw
	}
	opts[fieldIncludeUsage] = true
	body[fieldStreamOptions] = opts

	patched, err := json.Marshal(body)
	if err != nil {
		return raw
	}
	return patched
}

// decodeObject parses raw as a lone JSON object, keeping numbers in the
// notation the client wrote so re-marshaling cannot round a seed or a
// large id. Anything else, including trailing bytes, comes back nil and
// is forwarded exactly as it arrived.
func decodeObject(raw json.RawMessage) map[string]any {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()

	var body map[string]any
	if err := dec.Decode(&body); err != nil || body == nil {
		return nil
	}
	if dec.More() {
		return nil
	}
	return body
}
