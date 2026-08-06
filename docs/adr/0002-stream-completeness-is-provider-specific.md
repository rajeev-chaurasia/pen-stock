# 0002. Stream completeness is provider specific, and truncation is never a clean end

## Status

Accepted.

## Context

A streamed completion arrives as an SSE body that ends. The gateway has
to decide what "ended" means, and the honest answer is that it cannot
tell from the transport. A severed connection, a killed upstream, a
proxy timeout, and a finished answer all look the same at the socket:
the body stops.

Go's `io.EOF` is the natural thing to return when a body stops, and it
is the wrong thing, because every caller upstack treats `io.EOF` as "the
answer is whole". The gateway then writes `data: [DONE]` to the client,
the client SDK returns a complete looking response, and a sentence that
stops mid-clause is indistinguishable from a model that chose to stop
there. The bill is charged for a partial answer, and if caching is on
the partial answer is stored and replayed forever.

Each provider signals completion differently, and none of them signal it
in the transport:

- The OpenAI wire ends with a `data: [DONE]` sentinel and nothing else.
- Gemini sends no sentinel at all. The one promise it makes is that a
  finished turn carries a non-empty `finishReason` on its candidate.
- Anthropic's SSE is event typed, and completion is the `message_stop`
  event name rather than anything in the payload.

## Decision

Completeness is a per adapter judgement, and every adapter reports it
through the same two values.

`io.EOF` from `StreamReader.Recv` means the upstream sent its own
completion signal and the answer is whole.
`providers.ErrStreamTruncated` means the body ended without it
([internal/providers/types.go](../../internal/providers/types.go)).
There is no third state and no way for an adapter to be vague.

Each adapter implements its own signal:

| Adapter | Completion signal |
|---|---|
| `openaiwire` | `data: [DONE]` sentinel ([stream.go](../../internal/providers/openaiwire/stream.go)) |
| `gemini` | a non-empty `finishReason` on any candidate, latched, with reading continued past it so a trailing `usageMetadata` event is still collected ([stream.go](../../internal/providers/gemini/stream.go)) |
| `anthropic` | the `message_stop` event name ([stream.go](../../internal/providers/anthropic/stream.go)) |

The reader latches its terminal error, so a second `Recv` after a
failure cannot read as a clean end.

The ingress writes `data: [DONE]` to the client only on `io.EOF`. On
anything else it writes an error frame with code `stream_truncated` and
logs the cause (`finishStream` in
[internal/ingress/handlers.go](../../internal/ingress/handlers.go)).
The terminal frame gets a fresh write deadline, because the last chunk's
deadline may already be spent and a sunk terminal frame leaves the
client guessing.

Two downstream systems key off the same distinction. Only a stream that
ended in `io.EOF` is stored in the cache, so a truncated answer is never
replayed as a whole one. And the conformance suite makes truncation a
required subtest: `stream_truncated` feeds each adapter a body that
stops without its marker and fails the adapter unless the error is
`ErrStreamTruncated` ([ADR 0004](0004-provider-adapter-contract-and-conformance-suite.md)).

## Consequences

A client can now tell a cut off answer from a finished one, which is the
whole point.

A provider that changes its completion signal breaks loudly rather than
quietly. That is the intended failure direction, but it does mean a
false truncation report is possible: if a provider stops sending
`finishReason` on some path, the gateway will call a good answer
truncated. The conformance suite catches the shape, not the provider's
future behaviour.

Every adapter carries its own SSE parser rather than sharing one. They
share the split read requirement and the 1 MiB per event ceiling, but
the parsing is duplicated three times because the completion rule is
entangled with the framing. That is real duplicated code and the
conformance suite is what keeps the three honest.

Truncation surfaces late. The error frame is written after some content
has already reached the client, because by definition the stream started
successfully. That is unavoidable and it is the same constraint that
drives [ADR 0006](0006-fallback-only-before-the-first-byte.md).

## Alternatives considered

**Return `io.EOF` when the body ends and let callers infer the rest.**
Rejected. This is the default behaviour of every HTTP body reader and it
is exactly the bug. The gateway would relay a partial answer as a whole
one, bill it, and cache it, and nothing in the system would record that
anything went wrong.

**One shared completeness rule, such as "the last chunk with a
finish_reason wins".** Rejected. It does not exist on the Gemini wire in
the same shape and it is not what Anthropic signals, so the rule would
have to be approximated per provider anyway, with the approximation
hidden in a place nobody thought of as provider specific.

**Trust the HTTP layer, for example a clean `Content-Length` or a
well terminated chunked body.** Rejected. Streaming responses are
chunked with no length, and a proxy in between can terminate a chunked
body cleanly while the upstream was still generating. The transport
being well formed says nothing about the answer being complete.

**Report truncation only in a log or a metric and still send `[DONE]`.**
Rejected. The operator would see it and the caller would not, and the
caller is the one holding a half answer. An error frame in the stream is
the only place the information reaches the party who needs it.
