package providers

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// ErrorClass buckets provider failures into gateway-level semantics.
// Routing, retry, and client status mapping key off the class, never
// off provider-specific status codes.
type ErrorClass string

const (
	ErrClassAuth ErrorClass = "auth"
	// ErrClassPaymentRequired means the account cannot pay for the call:
	// out of credit, or a tier that was never activated. Distinct from
	// rate limiting because waiting does not fix it, and distinct from
	// auth because the credentials are fine. A router should fail over
	// rather than retry the same provider.
	ErrClassPaymentRequired ErrorClass = "payment_required"
	ErrClassRateLimited     ErrorClass = "rate_limited"
	ErrClassInvalidRequest  ErrorClass = "invalid_request"
	ErrClassModelNotFound   ErrorClass = "model_not_found"
	ErrClassUpstream        ErrorClass = "upstream_unavailable"
	ErrClassTimeout         ErrorClass = "timeout"
	ErrClassCanceled        ErrorClass = "canceled"
	ErrClassInternal        ErrorClass = "internal"
)

// ProviderError wraps any failure crossing the provider boundary.
type ProviderError struct {
	Provider   string
	Class      ErrorClass
	StatusCode int
	Message    string
	Err        error
}

func (e *ProviderError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %s: %s: %v", e.Provider, e.Class, e.Message, e.Err)
	}
	return fmt.Sprintf("%s: %s: %s", e.Provider, e.Class, e.Message)
}

func (e *ProviderError) Unwrap() error { return e.Err }

// ClassFromStatusAndBody maps an upstream response to an ErrorClass,
// using the body to disambiguate 404. A bare 404 from a mistyped
// base_url is gateway misconfiguration, not a missing model, and
// reporting it as model_not_found sends callers hunting the wrong bug.
func ClassFromStatusAndBody(code int, body []byte) ErrorClass {
	if code == http.StatusNotFound && !looksLikeErrorEnvelope(body) {
		return ErrClassUpstream
	}
	return ClassFromStatus(code)
}

// looksLikeErrorEnvelope reports whether body is an OpenAI-style error
// document rather than an HTML page or a router's plain text.
func looksLikeErrorEnvelope(body []byte) bool {
	var envelope struct {
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	return json.Unmarshal(body, &envelope) == nil && envelope.Error != nil
}

// ClassFromStatus maps an upstream HTTP status to an ErrorClass.
func ClassFromStatus(code int) ErrorClass {
	switch {
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		return ErrClassAuth
	case code == http.StatusPaymentRequired:
		return ErrClassPaymentRequired
	case code == http.StatusTooManyRequests:
		return ErrClassRateLimited
	case code == http.StatusNotFound:
		return ErrClassModelNotFound
	case code == http.StatusBadRequest || code == http.StatusUnprocessableEntity:
		return ErrClassInvalidRequest
	case code >= 500:
		return ErrClassUpstream
	default:
		return ErrClassInternal
	}
}
