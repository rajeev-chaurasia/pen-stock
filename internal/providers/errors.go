package providers

import (
	"fmt"
	"net/http"
)

// ErrorClass buckets provider failures into gateway-level semantics.
// Routing, retry, and client status mapping key off the class, never
// off provider-specific status codes.
type ErrorClass string

const (
	ErrClassAuth           ErrorClass = "auth"
	ErrClassRateLimited    ErrorClass = "rate_limited"
	ErrClassInvalidRequest ErrorClass = "invalid_request"
	ErrClassModelNotFound  ErrorClass = "model_not_found"
	ErrClassUpstream       ErrorClass = "upstream_unavailable"
	ErrClassTimeout        ErrorClass = "timeout"
	ErrClassCanceled       ErrorClass = "canceled"
	ErrClassInternal       ErrorClass = "internal"
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

// ClassFromStatus maps an upstream HTTP status to an ErrorClass.
func ClassFromStatus(code int) ErrorClass {
	switch {
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		return ErrClassAuth
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
