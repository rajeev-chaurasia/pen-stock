package gemini

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/rajeev-chaurasia/pen-stock/internal/providers"
)

// OpenAI roles the gateway receives, and the two roles Gemini accepts on
// a content entry. Gemini calls the assistant "model".
const (
	openAIRoleSystem    = "system"
	openAIRoleDeveloper = "developer"
	openAIRoleAssistant = "assistant"

	geminiRoleUser  = "user"
	geminiRoleModel = "model"
)

// Gemini finish reasons. Only the ones that map to a distinct OpenAI
// value are named; anything else falls back to "stop".
const (
	finishStop         = "STOP"
	finishMaxTokens    = "MAX_TOKENS"
	finishSafety       = "SAFETY"
	finishRecitation   = "RECITATION"
	finishBlocklist    = "BLOCKLIST"
	finishProhibited   = "PROHIBITED_CONTENT"
	finishSPII         = "SPII"
	finishImageSafety  = "IMAGE_SAFETY"
	finishUnexpectedFC = "UNEXPECTED_TOOL_CALL"
)

// OpenAI finish_reason values the gateway serves to clients.
const (
	openAIFinishStop          = "stop"
	openAIFinishLength        = "length"
	openAIFinishContentFilter = "content_filter"
)

const (
	objectCompletion = "chat.completion"
	objectChunk      = "chat.completion.chunk"

	// completionIDPrefix fronts the synthesized id. Gemini returns no id
	// of its own, and clients key their bookkeeping on one.
	completionIDPrefix = "chatcmpl-"

	// contentPartTypeText selects the text entries out of an OpenAI
	// multimodal content array.
	contentPartTypeText = "text"

	// modelNamePrefix is the fully qualified form of a Gemini model name.
	// Callers who paste it verbatim would otherwise produce /models/models/x.
	modelNamePrefix = "models/"
)

// openAIRequest is the subset of chat.completions this adapter maps.
//
// Everything else a client may send is dropped on purpose, because
// Gemini rejects unknown fields outright rather than ignoring them.
// Dropped today: n, presence_penalty, frequency_penalty, logit_bias,
// logprobs, top_logprobs, seed, user, response_format, tools,
// tool_choice, functions, function_call, parallel_tool_calls,
// stream_options, service_tier, store, metadata, and reasoning_effort.
type openAIRequest struct {
	Model       string          `json:"model"`
	Messages    []openAIMessage `json:"messages"`
	Temperature *float64        `json:"temperature"`
	TopP        *float64        `json:"top_p"`
	MaxTokens   *int            `json:"max_tokens"`
	// MaxCompletionTokens is OpenAI's newer spelling of max_tokens.
	// Honoring both matters for cost: dropping it would quietly give the
	// caller Gemini's default output cap instead of the ceiling they
	// asked for, and they pay for the difference.
	MaxCompletionTokens *int `json:"max_completion_tokens"`
	// Stop is a string or an array of strings on the OpenAI wire.
	Stop json.RawMessage `json:"stop"`
}

// outputTokenLimit prefers the explicit max_tokens and falls back to the
// newer spelling.
func (r openAIRequest) outputTokenLimit() *int {
	if r.MaxTokens != nil {
		return r.MaxTokens
	}
	return r.MaxCompletionTokens
}

// openAIMessage keeps Content raw because it is either a string or an
// array of typed parts.
type openAIMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type geminiRequest struct {
	Contents          []geminiContent   `json:"contents"`
	SystemInstruction *geminiContent    `json:"systemInstruction,omitempty"`
	GenerationConfig  *generationConfig `json:"generationConfig,omitempty"`
}

type geminiContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []geminiPart `json:"parts"`
}

type geminiPart struct {
	Text string `json:"text"`
}

type generationConfig struct {
	Temperature     *float64 `json:"temperature,omitempty"`
	TopP            *float64 `json:"topP,omitempty"`
	MaxOutputTokens *int     `json:"maxOutputTokens,omitempty"`
	StopSequences   []string `json:"stopSequences,omitempty"`
}

// geminiResponse covers both generateContent and one streamGenerateContent
// event, which carry the same candidate shape.
type geminiResponse struct {
	Candidates     []geminiCandidate `json:"candidates"`
	UsageMetadata  *usageMetadata    `json:"usageMetadata"`
	ModelVersion   string            `json:"modelVersion"`
	PromptFeedback *promptFeedback   `json:"promptFeedback"`
	Error          *geminiError      `json:"error"`
}

type geminiCandidate struct {
	Content      geminiContent `json:"content"`
	FinishReason string        `json:"finishReason"`
	Index        int           `json:"index"`
}

// promptFeedback reports a prompt Gemini refused. It arrives with HTTP
// 200 and no candidates, so it is a success on the wire and a refusal in
// substance.
type promptFeedback struct {
	BlockReason string `json:"blockReason"`
}

type usageMetadata struct {
	PromptTokenCount     int `json:"promptTokenCount"`
	CandidatesTokenCount int `json:"candidatesTokenCount"`
	TotalTokenCount      int `json:"totalTokenCount"`
}

func (u *usageMetadata) toUsage() providers.Usage {
	if u == nil {
		return providers.Usage{}
	}
	return providers.Usage{
		PromptTokens:     u.PromptTokenCount,
		CompletionTokens: u.CandidatesTokenCount,
		TotalTokens:      u.TotalTokenCount,
	}
}

// geminiError is the google.rpc.Status envelope Gemini returns on
// failure, both as an HTTP error body and as an in-stream SSE event.
type geminiError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Status  string `json:"status"`
}

// chatCompletion serves both OpenAI shapes: a choice carries Message for
// chat.completion and Delta for chat.completion.chunk.
type chatCompletion struct {
	ID      string     `json:"id"`
	Object  string     `json:"object"`
	Created int64      `json:"created"`
	Model   string     `json:"model"`
	Choices []choice   `json:"choices"`
	Usage   *usageJSON `json:"usage,omitempty"`
}

type choice struct {
	Index   int      `json:"index"`
	Message *message `json:"message,omitempty"`
	Delta   *message `json:"delta,omitempty"`
	// FinishReason is null until the turn ends, which is what OpenAI
	// clients expect on intermediate chunks.
	FinishReason *string `json:"finish_reason"`
}

type message struct {
	Role    string `json:"role,omitempty"`
	Content string `json:"content"`
}

type usageJSON struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

func usageToJSON(u providers.Usage) *usageJSON {
	return &usageJSON{
		PromptTokens:     u.PromptTokens,
		CompletionTokens: u.CompletionTokens,
		TotalTokens:      u.TotalTokens,
	}
}

// translateRequest turns an OpenAI chat.completions body into a Gemini
// generateContent body. The model and stream flag live in the URL rather
// than the payload, so neither is carried over.
func translateRequest(raw json.RawMessage) (*geminiRequest, error) {
	var in openAIRequest
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, fmt.Errorf("decode chat completions request: %w", err)
	}

	out := &geminiRequest{Contents: make([]geminiContent, 0, len(in.Messages))}
	var systemParts []geminiPart
	for _, m := range in.Messages {
		text := messageText(m.Content)
		if m.Role == openAIRoleSystem || m.Role == openAIRoleDeveloper {
			// Gemini takes the system prompt out of band. Several of them
			// concatenate rather than the last one winning, so no
			// instruction is silently dropped.
			if text != "" {
				systemParts = append(systemParts, geminiPart{Text: text})
			}
			continue
		}
		out.Contents = append(out.Contents, geminiContent{
			Role:  geminiRole(m.Role),
			Parts: []geminiPart{{Text: text}},
		})
	}
	if len(systemParts) > 0 {
		out.SystemInstruction = &geminiContent{Parts: systemParts}
	}
	out.GenerationConfig = generationConfigOf(&in)
	return out, nil
}

// geminiRole maps an OpenAI role onto the two Gemini accepts. Anything
// unrecognized becomes user so its text still reaches the model instead
// of being dropped on the floor.
func geminiRole(role string) string {
	if role == openAIRoleAssistant || role == geminiRoleModel {
		return geminiRoleModel
	}
	return geminiRoleUser
}

func generationConfigOf(in *openAIRequest) *generationConfig {
	cfg := &generationConfig{
		Temperature:     in.Temperature,
		TopP:            in.TopP,
		MaxOutputTokens: in.outputTokenLimit(),
		StopSequences:   stopSequences(in.Stop),
	}
	if cfg.Temperature == nil && cfg.TopP == nil && cfg.MaxOutputTokens == nil && len(cfg.StopSequences) == 0 {
		return nil
	}
	return cfg
}

// stopSequences accepts both wire forms of the OpenAI stop field.
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

// messageText flattens OpenAI message content. The multimodal array form
// keeps only its text parts, since images would need Gemini inlineData
// this adapter does not model.
func messageText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return ""
	}
	var sb strings.Builder
	for i := range parts {
		if parts[i].Type == contentPartTypeText {
			sb.WriteString(parts[i].Text)
		}
	}
	return sb.String()
}

// translateCompletion turns a generateContent body into the OpenAI
// chat.completion shape the gateway serves.
func translateCompletion(body []byte, id string, fallbackModel string) (json.RawMessage, providers.Usage, error) {
	var in geminiResponse
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, providers.Usage{}, fmt.Errorf("decode generateContent response: %w", err)
	}

	usage := in.UsageMetadata.toUsage()
	finish := openAIFinishStop
	text := ""
	if len(in.Candidates) > 0 {
		text = candidateText(&in.Candidates[0])
		if mapped := mapFinishReason(in.Candidates[0].FinishReason); mapped != "" {
			finish = mapped
		}
	} else if in.PromptFeedback != nil && in.PromptFeedback.BlockReason != "" {
		// A blocked prompt is a 200 with no candidates. Reporting it as a
		// plain empty stop would hide the refusal from the caller.
		finish = openAIFinishContentFilter
	}

	out := chatCompletion{
		ID:      id,
		Object:  objectCompletion,
		Created: time.Now().Unix(),
		Model:   responseModel(in.ModelVersion, fallbackModel),
		Choices: []choice{{
			Index:        0,
			Message:      &message{Role: openAIRoleAssistant, Content: text},
			FinishReason: &finish,
		}},
		Usage: usageToJSON(usage),
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		return nil, providers.Usage{}, fmt.Errorf("encode chat completion: %w", err)
	}
	return encoded, usage, nil
}

// candidateText concatenates every text part, since Gemini splits a
// single answer across parts freely.
func candidateText(c *geminiCandidate) string {
	if len(c.Content.Parts) == 1 {
		return c.Content.Parts[0].Text
	}
	var sb strings.Builder
	for i := range c.Content.Parts {
		sb.WriteString(c.Content.Parts[i].Text)
	}
	return sb.String()
}

// mapFinishReason converts a Gemini finish reason to the OpenAI value.
// An empty reason stays empty: mid-stream chunks carry none, and OpenAI
// expects null there. Unrecognized reasons degrade to "stop" because the
// OpenAI enum has no bucket for them.
func mapFinishReason(reason string) string {
	switch reason {
	case "":
		return ""
	case finishMaxTokens:
		return openAIFinishLength
	case finishSafety, finishRecitation, finishBlocklist, finishProhibited, finishSPII, finishImageSafety:
		return openAIFinishContentFilter
	case finishStop, finishUnexpectedFC:
		return openAIFinishStop
	default:
		return openAIFinishStop
	}
}

func responseModel(reported, fallback string) string {
	if reported != "" {
		return reported
	}
	return fallback
}

// newCompletionID synthesizes an id for a completion Gemini never
// identifies. crypto/rand keeps ids unguessable so they are safe to put
// in logs and client-visible traces.
func newCompletionID() string {
	return completionIDPrefix + strings.ToLower(rand.Text())
}
