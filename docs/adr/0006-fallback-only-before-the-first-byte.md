# 0006. Fallback is legal only before the first byte reaches the client

## Status

Accepted.

## Context

A route can name several providers, and the router walks them in order
when one fails ([ADR 0007](0007-router-failure-policy.md)). For a non
streaming completion that is straightforward: nothing has been written
to the client, so a failed attempt is invisible and the next provider
answers instead.

Streaming breaks that. Once the first SSE frame has been flushed to the
client, part of one model's answer is already on the wire and cannot be
taken back. If the upstream then dies and the router fails over, the
client receives the first half of one model's answer followed by the
beginning of a different model's answer to the same question, in one
response, with no marker between them.

That is worse than an error. An error is visible. A spliced answer looks
like a single coherent response that happens to contradict itself, and
nothing downstream, not the client, not the logs, not the cache, records
that two models were involved.

## Decision

Failover happens during connect and header exchange only. Once a
`StreamReader` has been handed back to the ingress, the chain is over for
that request.

This is stated at the top of the router package doc and is the first
constraint the package names
([internal/router/policy.go](../../internal/router/policy.go)). It falls
out of where the attempt loop sits: `runChain` in
[internal/router/orchestrator.go](../../internal/router/orchestrator.go)
wraps the call that returns the reader, so a failure inside `Recv` is
never seen by the loop. `ChatStream` returns as soon as an attempt yields
a reader, and everything after that point is the ingress pumping chunks.

A mid stream failure therefore surfaces as truncation, using exactly the
mechanism in
[ADR 0002](0002-stream-completeness-is-provider-specific.md): the reader
returns `ErrStreamTruncated`, the ingress writes an error frame with code
`stream_truncated` instead of `[DONE]`, and the partial answer is not
cached.

One consequence is deliberate and easy to miss: health for a streaming
attempt is recorded at the first chunk rather than when the connection is
accepted. `timedReader.Recv` records success the first time a non
keepalive chunk arrives, because time to first token is what matters for
latency aware routing and a provider that accepts a connection and then
says nothing has not served anything.

## Consequences

A client never receives two models' text in one answer.

A stream that dies at 90 percent is a failed request from the client's
point of view, even though a healthy peer was sitting right there. The
work is lost, the caller retries, and the tokens already generated are
still billed. That is the price of the guarantee and it is paid in full.

The provider that failed mid stream is still marked unhealthy by the
health tracker, so the next request routes around it. The current request
does not benefit.

The window in which fallback helps is narrow for streaming. Connection
refused, auth rejected, rate limited, and a bad model name are all caught
before the first byte, which covers the common free tier failures. A
provider that dies while generating is not covered at all.

## Alternatives considered

**Splice: fail over mid stream and continue writing the new provider's
chunks into the same response.** Rejected. It produces a response that
contains two different answers with no boundary, and it is silent. The
client cannot detect it, and the cache would store the mixture.

**Buffer the whole stream server side, then decide.** Rejected. It
destroys streaming. The client's time to first token becomes the full
generation time, which is the single property a streaming API exists to
provide, and the gateway would hold every in flight answer in memory.

**Replay: on a mid stream failure, restart the whole request on the next
provider and emit a reset marker to the client.** Rejected. There is no
reset in the SSE chat completion protocol, so the client SDK would append
the retry to what it already has, which is the splice again with extra
steps. It also doubles the cost of the request.

**Report a mid stream failure as a clean end and let the client decide.**
Rejected for the reason in
[ADR 0002](0002-stream-completeness-is-provider-specific.md): the client
has no way to decide, because a truncated answer and a short answer look
identical.
