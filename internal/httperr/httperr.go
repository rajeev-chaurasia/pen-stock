// Package httperr holds the gateway's client-facing JSON error envelope.
//
// The shape defined here is the OpenAI-compatible error envelope that
// client SDKs already know how to parse, so it is a wire contract rather
// than an internal convenience. Every listener the gateway opens answers
// failures with it: the proxy surface and the operator surface alike.
// That is why there is one definition and not one per package. Two
// copies stay identical only for as long as nobody edits either of them,
// and a caller that hits the other port would be the first to find out.
package httperr

import (
	"encoding/json"
	"net/http"
)

// ContentTypeJSON is the media type every response in this envelope
// carries. It is part of the contract, not a formatting choice.
const ContentTypeJSON = "application/json"

// The error type vocabulary shared by the gateway's surfaces. Clients
// branch on these exact strings, so they are contract values.
const (
	TypeInvalidRequest = "invalid_request_error"
	TypeRateLimit      = "rate_limit_error"
	TypeAPI            = "api_error"
)

// Body is the inner object of the envelope. Field order is part of the
// serialized form, so it is pinned by test rather than left to taste.
type Body struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code"`
}

// Envelope nests Body under the "error" key clients look for.
type Envelope struct {
	Error Body `json:"error"`
}

// WriteJSON sends v as the entire response body at the given status.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", ContentTypeJSON)
	w.WriteHeader(status)
	// Callers encode structs of strings, numbers, and bools, so a failure
	// at this point is a dead connection rather than something the
	// handler could act on.
	_ = json.NewEncoder(w).Encode(v)
}

// WriteError answers with the error envelope at the given status.
func WriteError(w http.ResponseWriter, status int, message, errType, code string) {
	WriteJSON(w, status, Envelope{Error: Body{Message: message, Type: errType, Code: code}})
}
