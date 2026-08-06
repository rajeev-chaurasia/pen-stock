package ingress

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/rajeev-chaurasia/pen-stock/internal/providers"
)

const (
	// StatusClientClosedRequest is the nginx 499 convention for a client
	// that disconnected before the response completed.
	StatusClientClosedRequest = 499

	errTypeInvalidRequest = "invalid_request_error"
	errTypeRateLimit      = "rate_limit_error"
	errTypeAPI            = "api_error"

	codeModelNotFound = "model_not_found"

	// msgUpstreamAuth deliberately hides upstream auth details: a bad
	// provider key is gateway misconfiguration, not the caller's problem.
	msgUpstreamAuth = "upstream auth failed"
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
	_ = json.NewEncoder(w).Encode(v)
}

func writeErrorJSON(w http.ResponseWriter, status int, message, errType, code string) {
	writeJSON(w, status, errorEnvelope{Error: errorBody{Message: message, Type: errType, Code: code}})
}

// writeUpstreamError maps a provider failure to the client-facing wire.
// Only ProviderError.Message ever reaches the client; wrapped transport
// errors and upstream headers stay server-side.
func (s *Server) writeUpstreamError(w http.ResponseWriter, err error) {
	var pe *providers.ProviderError
	if !errors.As(err, &pe) {
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			writeErrorJSON(w, http.StatusGatewayTimeout, "upstream timed out", errTypeAPI, "timeout")
		case errors.Is(err, context.Canceled):
			writeErrorJSON(w, StatusClientClosedRequest, "request canceled", errTypeAPI, "canceled")
		default:
			writeErrorJSON(w, http.StatusInternalServerError, "internal error", errTypeAPI, "internal")
		}
		return
	}

	status, errType, code := classToWire(pe.Class)
	msg := pe.Message
	if pe.Class == providers.ErrClassAuth {
		msg = msgUpstreamAuth
	}
	if msg == "" {
		msg = string(pe.Class)
	}
	writeErrorJSON(w, status, msg, errType, code)
}

func classToWire(c providers.ErrorClass) (status int, errType, code string) {
	switch c {
	case providers.ErrClassAuth:
		return http.StatusBadGateway, errTypeAPI, "upstream_auth_failed"
	case providers.ErrClassRateLimited:
		return http.StatusTooManyRequests, errTypeRateLimit, "rate_limited"
	case providers.ErrClassInvalidRequest:
		return http.StatusBadRequest, errTypeInvalidRequest, "invalid_request"
	case providers.ErrClassModelNotFound:
		return http.StatusNotFound, errTypeInvalidRequest, codeModelNotFound
	case providers.ErrClassUpstream:
		return http.StatusBadGateway, errTypeAPI, "upstream_unavailable"
	case providers.ErrClassTimeout:
		return http.StatusGatewayTimeout, errTypeAPI, "timeout"
	case providers.ErrClassCanceled:
		return StatusClientClosedRequest, errTypeAPI, "canceled"
	default:
		return http.StatusInternalServerError, errTypeAPI, "internal"
	}
}
