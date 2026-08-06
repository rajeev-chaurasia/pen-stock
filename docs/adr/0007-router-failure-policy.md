# 0007. The router failure policy: fail, retry, or fail over

## Status

Accepted.

## Context

When an upstream call fails, the router has exactly three useful moves:
return the error, try the same provider again, or move to the next
provider in the chain. Choosing wrongly is expensive in both directions.
Retrying something permanent burns latency for nothing. Failing over on
something the caller caused multiplies one mistake into one upstream call
per provider in the chain.

A second decision rides along with it. The health tracker takes providers
out of rotation after repeated failures. That is only correct if the
failures counted actually say something about the provider. A failure
caused by the request would let one bad caller take a healthy provider
out of rotation for every other tenant.

The gateway's target deployment sharpens this. A chain over several
independent free tiers exists precisely because their rate limit buckets
are separate, so what to do about a 429 is not a matter of taste.

## Decision

Failures are classified into a small vocabulary at the provider boundary
(`providers.ErrorClass` in
[internal/providers/errors.go](../../internal/providers/errors.go)), and
the router keys on the class and never on a status code. The mapping
lives in one function, `classDisposition` in
[internal/router/policy.go](../../internal/router/policy.go):

| Class | Disposition | Why |
|---|---|---|
| `invalid_request` | fail | The caller's own payload. Every provider rejects it identically. |
| `canceled` | fail | The client hung up. Nobody is waiting for a retry. |
| `upstream_unavailable` | retry | Looks transient. The same provider may answer next time. |
| `timeout` | retry | Same. |
| `rate_limited` | fail over | Another bucket has room right now. Waiting on this one wastes the chain. |
| `auth` | fail over | A configuration fault on this provider. Retrying cannot fix credentials. |
| `payment_required` | fail over | The key is valid and waiting does not help, but a peer with credit can answer. |
| `model_not_found` | fail over | This provider does not have the model. A peer may. |
| anything else | fail over | An unclassified failure is treated as the provider's fault, which keeps it eligible for a peer. |

`payment_required` is its own class rather than being folded into auth or
rate limiting. It was added after live traffic: Cerebras answers HTTP 402
when a tier was never activated, which had been falling through to the
internal bucket and reaching the caller as a 500. The credentials are
fine, so it is not auth, and waiting does not help, so it is not a rate
limit.

Health is tracked separately from disposition, in `countsAgainstHealth`.
`invalid_request`, `canceled`, and `model_not_found` do not count.
Everything else does. A rejected payload and a client cancel say nothing
about the provider, and letting them trip a breaker means one caller
sending malformed JSON in a loop can evict a healthy provider for
everyone else. `model_not_found` is excluded for a related reason: it is
a statement about this route's configuration for this provider, not about
the provider's health.

Retries are budgeted so they cannot starve the chain. `runChain` reserves
one attempt for every provider not yet tried, and declines to retry when
`used + reserved >= MaxAttempts`
([internal/router/orchestrator.go](../../internal/router/orchestrator.go)).
A sick provider cannot consume the whole attempt budget and strand a
healthy peer further down the chain. Backoff between retries is
exponential with full jitter, so a fleet of gateways retrying together
does not resynchronize.

A 404 with no error envelope in the body is classified as
`upstream_unavailable`, not `model_not_found`
(`ClassFromStatusAndBody`). A bare 404 is what a mistyped `base_url`
returns, and reporting it as a missing model sends the operator hunting
the wrong bug.

The contract tests for this loop were written before the implementation
and passed without being edited.

## Consequences

One table describes the whole policy, and adding a class means adding a
row rather than finding every branch that switches on an error.

A bad request costs exactly one upstream call, and the caller gets the
provider's own rejection rather than the last provider in the chain's.

A chain over free tiers actually multiplies headroom, because a 429 moves
immediately rather than backing off on an exhausted bucket.

The distinctions are only as good as the classification. Everything here
depends on `ClassFromStatus` and each adapter's error mapping being right,
which is why error classification is a required subtest in the
conformance suite
([ADR 0004](0004-provider-adapter-contract-and-conformance-suite.md)).
A provider that returns 400 for something that is really its own fault
will be reported to the caller as their mistake.

The default for an unrecognized failure is fail over, which means an
unclassified error can cost one call per provider in the chain. That is
the deliberate direction: an unknown failure is more likely to be the
provider's than the caller's, and the cost is bounded by `MaxAttempts`.

Excluding `model_not_found` from health means a route misconfigured for
one provider fails over on every request forever without ever taking that
provider out of rotation. The failure is visible in metrics rather than
self healing.

## Alternatives considered

**Retry everything with backoff.** Rejected. It retries rejected
payloads, which cannot succeed, and it retries rate limits, which is the
opposite of what a chain of independent free tiers is for.

**Fail over on everything.** Rejected. An invalid payload would be sent
to every provider in the chain, turning one client mistake into N
upstream calls and N error responses, and the caller would receive the
last provider's complaint rather than the first one's.

**Retry on rate limit with `Retry-After`.** Rejected for chained routes.
Honouring `Retry-After` means holding the request open for seconds while
a peer with a fresh bucket sits idle. For a single provider route it
would be defensible, but a policy that behaves differently depending on
chain length is harder to reason about than one that always prefers the
peer.

**Count every failure against provider health.** Rejected. It hands any
caller a denial of service against every other tenant: send enough
malformed requests and the provider is evicted for everyone.

**Key routing decisions on HTTP status codes directly.** Rejected. It
spreads provider specific knowledge into the router, and it cannot
express the two cases where the status alone is ambiguous, namely a bare
404 from a mistyped base URL and a 402 for an unactivated tier.
