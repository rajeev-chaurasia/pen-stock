package admin

import (
	"encoding/json"
	"net/http"
)

const (
	contentTypeJSON = "application/json"

	// errTypeInvalidRequest reuses the ingress envelope's vocabulary so
	// an operator's tooling can parse both surfaces the same way.
	errTypeInvalidRequest = "invalid_request_error"

	codeTenantNotFound   = "tenant_not_found"
	codeNotFound         = "not_found"
	codeMethodNotAllowed = "method_not_allowed"
)

type errorBody struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code"`
}

type errorEnvelope struct {
	Error errorBody `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", contentTypeJSON)
	w.WriteHeader(status)
	// Every value encoded here is a struct of numbers, bools, and
	// configured names, so a failure at this point is a dead connection
	// rather than something the handler can act on.
	_ = json.NewEncoder(w).Encode(v)
}

// writeError answers with the gateway's JSON error envelope. Every path
// on this handler goes through it, so no caller ever meets net/http's
// plain text default.
func writeError(w http.ResponseWriter, status int, message, code string) {
	writeJSON(w, status, errorEnvelope{Error: errorBody{
		Message: message,
		Type:    errTypeInvalidRequest,
		Code:    code,
	}})
}
