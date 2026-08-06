package ingress

import (
	"context"
	"errors"
	"net/http"
	"regexp"

	"github.com/rajeev-chaurasia/pen-stock/internal/httperr"
	"github.com/rajeev-chaurasia/pen-stock/internal/providers"
)

const (
	// StatusClientClosedRequest is the nginx 499 convention for a client
	// that disconnected before the response completed.
	StatusClientClosedRequest = 499

	// The client-facing error vocabulary is defined once in httperr, since
	// the admin surface answers with the same envelope. These names keep
	// this package's call sites reading as they always have.
	errTypeInvalidRequest = httperr.TypeInvalidRequest
	errTypeRateLimit      = httperr.TypeRateLimit
	errTypeAPI            = httperr.TypeAPI

	codeModelNotFound = "model_not_found"

	// msgUpstreamAuth deliberately hides upstream auth details: a bad
	// provider key is gateway misconfiguration, not the caller's problem.
	msgUpstreamAuth = "upstream auth failed"

	// maxRelayedMessage caps how much upstream text can reach a client on
	// the one class where relaying it is useful.
	maxRelayedMessage = 512
)

// The shared envelope under this package's historical names, so the
// streaming path can keep composing a frame directly.
type (
	errorBody     = httperr.Body
	errorEnvelope = httperr.Envelope
)

// secretPattern matches text that must never be relayed to a client:
// bearer tokens, common provider key prefixes, and bare private IPs that
// upstream proxies like to name in their error pages.
var secretPattern = regexp.MustCompile(
	`(?i)(bearer\s+\S+|\b(sk|gsk|xai|api)[-_][A-Za-z0-9._-]{8,}|\b(?:10|127|192\.168|172\.(?:1[6-9]|2\d|3[01]))(?:\.\d{1,3}){2,3}(?::\d+)?)`)

const redacted = "[redacted]"

// sanitizeUpstreamMessage strips secrets and internal addresses from
// upstream text and shortens it to something a client can act on.
func sanitizeUpstreamMessage(msg string) string {
	clean := secretPattern.ReplaceAllString(msg, redacted)
	if len(clean) > maxRelayedMessage {
		clean = clean[:maxRelayedMessage] + "..."
	}
	return clean
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	httperr.WriteJSON(w, status, v)
}

func writeErrorJSON(w http.ResponseWriter, status int, message, errType, code string) {
	httperr.WriteError(w, status, message, errType, code)
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

	// The full upstream text stays server side. Clients get gateway
	// authored wording, because upstream error pages routinely name
	// internal hosts and can echo the request's own credentials.
	s.log.Error("upstream failure",
		"provider", pe.Provider,
		"class", string(pe.Class),
		"upstream_status", pe.StatusCode,
		"message", pe.Message,
		"error", pe.Err,
	)

	msg := classMessage(pe.Class)
	if pe.Class == providers.ErrClassInvalidRequest && pe.Message != "" {
		// A rejected request is the one case where upstream detail helps
		// the caller fix their own payload.
		msg = sanitizeUpstreamMessage(pe.Message)
	}
	writeErrorJSON(w, status, msg, errType, code)
}

func classMessage(c providers.ErrorClass) string {
	switch c {
	case providers.ErrClassAuth:
		return msgUpstreamAuth
	case providers.ErrClassPaymentRequired:
		return "the upstream account requires payment or an activated quota"
	case providers.ErrClassRateLimited:
		return "upstream rate limit reached"
	case providers.ErrClassInvalidRequest:
		return "upstream rejected the request"
	case providers.ErrClassModelNotFound:
		return "the requested model is not available upstream"
	case providers.ErrClassUpstream:
		return "upstream is unavailable"
	case providers.ErrClassTimeout:
		return "upstream timed out"
	case providers.ErrClassCanceled:
		return "request canceled"
	default:
		return "internal error"
	}
}

func classToWire(c providers.ErrorClass) (status int, errType, code string) {
	switch c {
	case providers.ErrClassAuth:
		return http.StatusBadGateway, errTypeAPI, "upstream_auth_failed"
	case providers.ErrClassPaymentRequired:
		return http.StatusPaymentRequired, errTypeAPI, "upstream_payment_required"
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
