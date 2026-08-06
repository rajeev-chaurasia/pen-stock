// Package router turns one model name into an attempt across a chain of
// providers, with health tracking so a sick provider stops receiving
// traffic before it wastes the caller's time.
//
// The hard constraint here is streaming. A fallback is only possible
// while nothing has been written to the client yet: once the first byte
// of an SSE body is on the wire, switching providers would splice two
// different completions into one response. So failover happens during
// connect and header exchange only, and a failure after that is
// surfaced as truncation rather than silently patched over.
package router

import "github.com/rajeev-chaurasia/pen-stock/internal/providers"

// disposition says what the attempt loop may do about a failure.
type disposition int

const (
	// dispositionFail returns the error to the caller as is. The next
	// provider would answer the same way, so trying costs latency and
	// changes nothing.
	dispositionFail disposition = iota
	// dispositionRetry allows another attempt on the SAME provider after
	// a backoff, because the failure looks transient.
	dispositionRetry
	// dispositionFailover moves to the next provider in the chain. The
	// current one is unusable for now, but a peer may be fine.
	dispositionFailover
)

// classDisposition maps a provider failure to what the loop does next.
//
// The distinctions that matter:
//   - invalid_request is the caller's own payload, so every provider in
//     the chain rejects it identically. Failing over would multiply one
//     mistake into N upstream calls.
//   - canceled means the client hung up. Nobody is waiting for a retry.
//   - payment_required and auth are configuration faults on this
//     provider. Retrying cannot help, but a peer with working
//     credentials can, so they fail over without a retry.
//   - rate_limited fails over immediately rather than retrying, because
//     the whole point of a chain of free tiers is that another bucket
//     has room right now.
func classDisposition(c providers.ErrorClass) disposition {
	switch c {
	case providers.ErrClassInvalidRequest, providers.ErrClassCanceled:
		return dispositionFail
	case providers.ErrClassUpstream, providers.ErrClassTimeout:
		return dispositionRetry
	case providers.ErrClassRateLimited, providers.ErrClassAuth,
		providers.ErrClassPaymentRequired, providers.ErrClassModelNotFound:
		return dispositionFailover
	default:
		return dispositionFailover
	}
}

// countsAgainstHealth reports whether a failure says something about the
// provider rather than about the request. A rejected payload or a
// client cancel must never trip a breaker, or one bad caller could take
// a healthy provider out of rotation for everyone.
func countsAgainstHealth(c providers.ErrorClass) bool {
	switch c {
	case providers.ErrClassInvalidRequest, providers.ErrClassCanceled,
		providers.ErrClassModelNotFound:
		return false
	default:
		return true
	}
}
