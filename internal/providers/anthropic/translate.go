package anthropic

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/rajeev-chaurasia/pen-stock/internal/providers"
)

const (
	roleSystem    = "system"
	roleDeveloper = "developer"
	roleAssistant = "assistant"

	objectCompletion = "chat.completion"
	objectChunk      = "chat.completion.chunk"

	blockTypeText = "text"

	stopReasonEndTurn      = "end_turn"
	stopReasonMaxTokens    = "max_tokens"
	stopReasonStopSequence = "stop_sequence"
	stopReasonToolUse      = "tool_use"

	finishStop      = "stop"
	finishLength    = "length"
	finishToolCalls = "tool_calls"

	// systemJoiner separates several system turns folded into the single
	// system field Anthropic offers.
	systemJoiner = "\n\n"

	// defaultMaxTokens is sent when the client omits max_tokens. The
	// field is optional on chat.completions and mandatory on the
	// Messages API, so rejecting the call instead would break every
	// client that never had to think about it.
	defaultMaxTokens = 4096
)

// openAIRequest is the subset of chat.completions that survives the
// crossing. Anything Anthropic has no equivalent for is dropped rather
// than forwarded, because the Messages API rejects members it does not
// recognize.
type openAIRequest struct {
	Model    string          `json:"model"`
	Messages []openAIMessage `json:"messages"`
	// MaxCompletionTokens is the current OpenAI spelling; MaxTokens is
	// the one older clients still send.
	MaxTokens           *int            `json:"max_tokens"`
	MaxCompletionTokens *int            `json:"max_completion_tokens"`
	Temperature         *float64        `json:"temperature"`
	TopP                *float64        `json:"top_p"`
	Stop                json.RawMessage `json:"stop"`
}

type openAIMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type messagesRequest struct {
	Model         string             `json:"model"`
	Messages      []anthropicMessage `json:"messages"`
	MaxTokens     int                `json:"max_tokens"`
	System        string             `json:"system,omitempty"`
	Temperature   *float64           `json:"temperature,omitempty"`
	TopP          *float64           `json:"top_p,omitempty"`
	StopSequences []string           `json:"stop_sequences,omitempty"`
	Stream        bool               `json:"stream,omitempty"`
}

type anthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// translateRequest converts an OpenAI chat.completions body into an
// Anthropic Messages body. model overrides the model named in the body
// so gateway routing decisions win.
func translateRequest(raw json.RawMessage, model string, stream bool) ([]byte, error) {
	var in openAIRequest
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, fmt.Errorf("decode chat request: %w", err)
	}

	out := messagesRequest{
		Model:         in.Model,
		Messages:      make([]anthropicMessage, 0, len(in.Messages)),
		MaxTokens:     maxTokens(in),
		Temperature:   in.Temperature,
		TopP:          in.TopP,
		StopSequences: stopSequences(in.Stop),
		Stream:        stream,
	}
	if model != "" {
		out.Model = model
	}

	var system []string
	for _, m := range in.Messages {
		// The Messages API has no system role inside messages[] and
		// rejects one left there. Lifting it to the top-level system
		// field is the single largest difference between the two
		// formats, and the one that is easiest to miss.
		if m.Role == roleSystem || m.Role == roleDeveloper {
			if text := textOf(m.Content); text != "" {
				system = append(system, text)
			}
			continue
		}
		// A user or assistant turn means the same thing on both wires,
		// so it crosses as a conversion rather than a rewrite.
		out.Messages = append(out.Messages, anthropicMessage(m))
	}
	out.System = strings.Join(system, systemJoiner)

	body, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("encode messages request: %w", err)
	}
	return body, nil
}

func maxTokens(in openAIRequest) int {
	switch {
	case in.MaxTokens != nil:
		return *in.MaxTokens
	case in.MaxCompletionTokens != nil:
		return *in.MaxCompletionTokens
	default:
		return defaultMaxTokens
	}
}

// textOf pulls plain text out of an OpenAI content field, which is
// either a bare string or an array of typed parts.
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
		if p.Type == blockTypeText {
			sb.WriteString(p.Text)
		}
	}
	return sb.String()
}

// stopSequences accepts both OpenAI shapes for stop: a bare string or an
// array of them.
func stopSequences(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var one string
	if err := json.Unmarshal(raw, &one); err == nil {
		if one == "" {
			return nil
		}
		return []string{one}
	}
	var many []string
	if err := json.Unmarshal(raw, &many); err != nil {
		return nil
	}
	return many
}

type messagesResponse struct {
	ID         string         `json:"id"`
	Model      string         `json:"model"`
	Content    []contentBlock `json:"content"`
	StopReason string         `json:"stop_reason"`
	Usage      usageJSON      `json:"usage"`
}

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type usageJSON struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

func (u usageJSON) toUsage() providers.Usage {
	return providers.Usage{
		PromptTokens:     u.InputTokens,
		CompletionTokens: u.OutputTokens,
		TotalTokens:      u.InputTokens + u.OutputTokens,
	}
}

type completion struct {
	ID      string             `json:"id"`
	Object  string             `json:"object"`
	Created int64              `json:"created"`
	Model   string             `json:"model"`
	Choices []completionChoice `json:"choices"`
	Usage   openAIUsage        `json:"usage"`
}

type completionChoice struct {
	Index        int           `json:"index"`
	Message      completionMsg `json:"message"`
	FinishReason string        `json:"finish_reason"`
}

type completionMsg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type openAIUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

func toOpenAIUsage(u providers.Usage) openAIUsage {
	return openAIUsage{
		PromptTokens:     u.PromptTokens,
		CompletionTokens: u.CompletionTokens,
		TotalTokens:      u.TotalTokens,
	}
}

// translateResponse converts an Anthropic message into a
// chat.completion. created is passed in because Anthropic reports no
// timestamp of its own and OpenAI clients expect the field.
func translateResponse(body []byte, model string, created int64) ([]byte, providers.Usage, error) {
	var in messagesResponse
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, providers.Usage{}, fmt.Errorf("decode messages response: %w", err)
	}

	// A message is a list of blocks; only text blocks have a place in a
	// chat.completion's single content string.
	var text strings.Builder
	for _, block := range in.Content {
		if block.Type == blockTypeText {
			text.WriteString(block.Text)
		}
	}

	usage := in.Usage.toUsage()
	if in.Model != "" {
		model = in.Model
	}
	out := completion{
		ID:      in.ID,
		Object:  objectCompletion,
		Created: created,
		Model:   model,
		Choices: []completionChoice{{
			Message:      completionMsg{Role: roleAssistant, Content: text.String()},
			FinishReason: finishReason(in.StopReason),
		}},
		Usage: toOpenAIUsage(usage),
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		return nil, providers.Usage{}, fmt.Errorf("encode chat completion: %w", err)
	}
	return encoded, usage, nil
}

// finishReason maps an Anthropic stop_reason onto the OpenAI vocabulary.
func finishReason(stopReason string) string {
	switch stopReason {
	case "":
		return ""
	case stopReasonEndTurn, stopReasonStopSequence:
		return finishStop
	case stopReasonMaxTokens:
		return finishLength
	case stopReasonToolUse:
		return finishToolCalls
	default:
		// An unrecognized reason still means the turn ended. Passing it
		// through verbatim would hand clients a value no OpenAI SDK
		// knows how to read.
		return finishStop
	}
}
