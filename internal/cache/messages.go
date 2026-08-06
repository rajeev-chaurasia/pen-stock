package cache

import "encoding/json"

// message is one turn reduced to what a semantic comparison cares
// about: who spoke and what they said.
type message struct {
	role string
	text string
}

// contentPartText selects the billable, comparable entries out of a
// multimodal content array.
const contentPartText = "text"

// decodeMessages pulls the conversation out of a chat request. It
// returns ok false for a body it cannot read, which the caller treats
// as "not comparable" rather than as an empty conversation: an empty
// string would make every unreadable request look identical to every
// other one, and they would all match each other.
func decodeMessages(raw []byte) ([]message, bool) {
	var body struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, false
	}
	if len(body.Messages) == 0 {
		return nil, false
	}

	out := make([]message, 0, len(body.Messages))
	for _, m := range body.Messages {
		out = append(out, message{role: m.Role, text: contentText(m.Content)})
	}
	return out, true
}

// contentText accepts both content shapes on the OpenAI wire: a plain
// string, or an array of typed parts of which only the text ones carry
// anything a comparison can use.
func contentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}

	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return ""
	}
	var joined string
	for _, p := range parts {
		if p.Type == contentPartText {
			joined += p.Text
		}
	}
	return joined
}
