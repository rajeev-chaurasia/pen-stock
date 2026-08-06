# 0008. Two phase budget enforcement with a bounded overshoot

## Status

Accepted.

## Context

A tenant has a spend cap. The gateway is supposed to stop spending when
the cap is reached. The problem is that the cost of a request is not
known until the upstream has answered, because the cost is a function of
the completion length, and the completion length is chosen by the model.

So at the moment the enforcement decision has to be made, the number the
decision depends on does not exist yet.

Concurrency makes it worse. Even with a perfect cost oracle, a hundred
requests arriving together would each read the same remaining balance,
each conclude there is room for one more, and each spend it. The cap is
then exceeded by a factor of a hundred and no individual request did
anything wrong.

## Decision

Enforcement is two phase: reserve an estimate before the upstream call,
settle the real usage after it
([internal/budget](../../internal/budget/types.go) package doc).

`Reserve` estimates the request's cost, checks it against the tenant's
rate limits and remaining budget, and claims it, all under one lock
([internal/budget/enforcer.go](../../internal/budget/enforcer.go)).
`MemEnforcer` uses a mutex rather than atomics on purpose: admitting a
request means reading several counters and updating them together, and
that has to be one indivisible step. The lock covers the admission
decision only and is released before the upstream is called, so a slow
provider does not serialize a tenant's traffic.

`Settle` returns the claim and puts the real cost in its place, so an
estimate that was too generous is refunded and one that was too small is
made good. It also reconciles the token rate window against real usage,
because leaving it on the estimate would let a tenant whose answers
routinely run long sit above its tokens per minute cap. Settlement is
idempotent: a reservation id that is already terminal is ignored rather
than counted twice.

`Release` returns a claim whose request never produced an answer, so a
failed upstream call costs the tenant nothing. A stream that never
reported usage is released rather than settled, because billing an
estimate nobody measured is billing a guess.

The overshoot bound is written down and asserted:

```
overshoot <= sum over in flight requests of (actual - estimate)
```

A tenant can exceed its budget only by how badly the estimates for
currently running requests undershot. Capping the estimate's completion
allowance keeps each term small, and the in flight ceiling keeps the sum
finite. `TestEnforcerOvershootStaysWithinTheDocumentedBound` releases 200
goroutines at once against a 3.00 USD daily cap where each request is
estimated at 1.00 and settles at 1.50, and asserts three things: that no
more than three were admitted, which is the signature of atomic
admission, that final spend sits inside `budget + admitted *
underestimate`, and that nothing goes negative. With those numbers the
assertion is exactly tight, so a bound off by one request's worth of
underestimate fails the test. It runs under `-race` in CI.

Denials answer honestly. A rate limit is 429 with a `Retry-After`,
because waiting helps. An exhausted budget is 402 without one, because
it does not
([internal/ingress/accounting.go](../../internal/ingress/accounting.go)).

The daily and monthly windows roll over independently, so a fresh day
does not forgive the month. When the accounting store cannot answer, a
tenant marked `fail_closed` is denied and one that is not is allowed:
a hard cap on real money refuses rather than guesses, a soft advisory
limit would rather serve and be told later.

## Consequences

A cap is enforced before the money is spent rather than after, which is
the only ordering under which it is a cap at all.

A tenant can still finish slightly over budget. This is real and it is
documented rather than hidden. The admin API reports remaining after
subtracting outstanding reservations and can read negative, which is the
bound being visible rather than a bug
([docs/cost-accounting.md](../cost-accounting.md)).

The bound is only as tight as the estimator. The estimator is a
heuristic, four characters per token plus four tokens of framing per
message, with a 1024 token default completion allowance
([internal/budget/estimator.go](../../internal/budget/estimator.go)). A
worse estimator is a wider bound, not a broken invariant, but it is
still worse.

Reservations are held in memory. A restart forgets every outstanding
claim and every window, which the README states plainly. This is the
largest gap in the design and it is why the gateway is not yet suitable
for a public address or a multi node deployment.

All tenants share one enforcer mutex. It is held for a few map lookups
and some arithmetic, so it is not a throughput concern at this scale, but
it is a single point of contention that a sharded or per tenant design
would not have.

Every successful `Reserve` must be followed by exactly one `Settle` or
`Release`. Missing one leaks a claim that stays committed and eats into
the tenant's headroom until the process restarts. The ingress uses a
deferred `settleOrAbort` so no exit path from the stream pump can skip
it.

## Alternatives considered

**Check spend after the fact and deny the next request.** Rejected. It is
not a limit, it is a report. A tenant with a 1 USD cap and enough
concurrency spends whatever the fleet can generate before the first
settlement lands.

**Lock the tenant for the duration of the request, so only one request
per tenant is ever in flight.** Rejected. It makes the overshoot zero and
makes the tenant's throughput one request per generation, which is
seconds. The whole point of a gateway in front of several providers is
concurrency, and this alternative removes it to solve a problem the
bounded overshoot already solves acceptably.

**Atomics or lock free counters instead of a mutex.** Rejected. Admission
reads the daily window, the monthly window, the committed total, the
request count, and the token count, then updates several of them. Any
granularity finer than one lock lets two requests both observe room only
one of them can have. This is the exact bug the 200 goroutine test is
built to catch.

**Reserve the maximum possible cost, so overshoot is zero.** Rejected. It
means reserving the model's full context window on every request, which
would deny almost everything against any realistic cap. A tenant with a
1 USD daily budget could make roughly one request.

**Skip reservation and bill only what actually happened.** Rejected. It
is the same as checking after the fact, with the extra property that the
gateway would keep serving a tenant who is already over.
