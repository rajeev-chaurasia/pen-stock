# 0001. Go over Python for the gateway core

## Status

Accepted.

## Context

Penstock sits in the request path of every model call. The work it does
per request is small: parse an envelope, choose a provider, hold an SSE
connection open for as long as the model takes to answer, count tokens,
append a ledger row. Almost all of the wall clock time is spent waiting
on an upstream that is generating text one token at a time.

That makes the cost of a unit of concurrency the property that decides
the language. A gateway holding a thousand streams is holding a thousand
mostly idle sockets, each with a small amount of per stream state: an
idle timer, a write deadline, a last usage record, a frame recorder.

Most LLM tooling is written in Python, so Python was the choice that had
to be argued out of rather than into.

## Decision

The gateway core is Go, pinned at `go 1.26` in
[go.mod](../../go.mod) and in the CI matrix.

Three properties settled it.

Concurrency is structural. Every request gets a goroutine, and the
streaming path gets a second one that pumps upstream chunks into a
channel while the handler selects over that channel, the idle timer, and
client disconnect ([internal/ingress/handlers.go](../../internal/ingress/handlers.go)).
Cancellation propagates through `context.Context` into the provider's
HTTP request, so a client that hangs up releases the upstream
connection. The adapter conformance suite asserts exactly that
(`cancel_releases_body`, see [ADR 0004](0004-provider-adapter-contract-and-conformance-suite.md)).
Expressing the same thing in Python means committing to an async
framework and keeping every provider call inside it, and a single
blocking call in the wrong place stalls the loop.

Deployment is one artifact. `CGO_ENABLED=0` and a build of `./cmd/...`
produce static binaries that are copied into
`gcr.io/distroless/static-debian12:nonroot` with no interpreter, no
package manager, and no shell in the final image
([Dockerfile](../../Dockerfile)). The gateway holds provider API keys,
so the size of the thing running beside those keys is a security
property and not just an ergonomic one.

Data races are detectable. The budget enforcer, the router health
tracker, and both cache tiers are shared mutable state under concurrent
traffic. `go test -race ./...` runs on every push
([.github/workflows/ci.yml](../../.github/workflows/ci.yml)), and the
overshoot bound test in [ADR 0008](0008-two-phase-budget-enforcement.md)
is written specifically to be meaningful under it. There is no
equivalent for a Python service beyond reasoning carefully.

## Consequences

The runtime footprint is one process. There is no worker pool to size,
no event loop to keep unblocked, and no separate model for "the part
that streams".

The provider ecosystem is not Go first. Every adapter here is written by
hand against the wire format rather than against a vendor SDK
(`internal/providers/openaiwire`, `internal/providers/anthropic`,
`internal/providers/gemini`). That is more code to own, and a wire
format change is the gateway's problem to notice. The conformance suite
in [ADR 0004](0004-provider-adapter-contract-and-conformance-suite.md)
exists partly to make that cost bounded.

Token counting is a heuristic rather than a real tokenizer. The
estimator assumes four characters per token plus four tokens of framing
per message ([internal/budget/estimator.go](../../internal/budget/estimator.go)),
because the maintained tokenizer implementations are Python and Rust.
The two phase design in [ADR 0008](0008-two-phase-budget-enforcement.md)
tolerates that by replacing the estimate with real usage at settle time,
but the estimate is what the overshoot bound is written in terms of, so
a worse estimator is a wider bound.

Anything model side that is genuinely research shaped, such as an
evaluation harness or a tokenizer study, does not belong in this process
and will have to live somewhere else.

## Alternatives considered

**Python with FastAPI and asyncio.** Rejected. The provider SDK
ecosystem is the real argument for it, and it is a good one, but it buys
convenience at the layer this project deliberately writes by hand. What
it costs is the two properties above: one accidental blocking call
stalls every stream sharing the loop, and there is no race detector for
the shared budget and health state. Packaging is also worse: an
interpreter, a virtualenv, and a base image with a package manager in
it, next to the operator's provider keys.

**Rust.** Rejected on cost, not capability. It would have given the same
concurrency and deployment story with a smaller runtime. Nothing in this
gateway is CPU bound, so the extra performance is not collectable, and
the async ecosystem's complexity is a real tax on a project whose value
is in the routing, budgeting, and correctness decisions rather than in
the byte pushing.

**Node.js.** Rejected. It has the deployment problem of Python without
Python's provider ecosystem advantage, and single threaded concurrency
means the same "do not block the loop" discipline with fewer libraries
worth having.
