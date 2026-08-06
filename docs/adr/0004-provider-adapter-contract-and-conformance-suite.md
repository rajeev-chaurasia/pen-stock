# 0004. The provider adapter contract, enforced by an executable conformance suite

## Status

Accepted.

## Context

Penstock fronts providers that do not agree on much. Some speak the
OpenAI wire, some do not. They differ on how streams terminate
([ADR 0002](0002-stream-completeness-is-provider-specific.md)), on how
errors are shaped, on where the system prompt goes, and on whether usage
arrives once at the end or cumulatively on every chunk.

There are two places that difference can be absorbed: in the ingress, or
in the adapters. If it lives in the ingress, then the request path grows
a branch per provider, and every new feature has to be implemented once
per wire format. If it lives in the adapters, the ingress sees one
shape.

The second question is how the contract is stated. A `Provider`
interface says an adapter has `Chat` and `ChatStream`. It says nothing
about whether a truncated stream is reported as truncated, whether the
API key leaks into an error message, or whether `Close` can be called
twice. Those are the properties the rest of the gateway actually depends
on, and an interface cannot express them.

## Decision

Adapters translate into the OpenAI shape. The gateway serves that shape
to clients, so `chat.completion` and `chat.completion.chunk` envelopes
with `choices[].message` and `choices[].delta` are what every adapter
produces regardless of what its upstream speaks. The Gemini and
Anthropic adapters each carry a `translate.go` that converts in both
directions. The ingress never learns a wire format.

The contract is written as an executable specification in
[internal/providers/conformance](../../internal/providers/conformance/conformance.go).
An adapter supplies a `Suite` describing the bytes its upstream would
send, and `Run` asserts the behaviour the gateway depends on. Eight
subtests, identical for every adapter:

- `auth_header`, that credentials go where the provider expects them
- `non_stream`, that the response is an OpenAI `chat.completion` with a
  role of `assistant`, and that usage is mapped
- `stream`, that assembled deltas equal the expected content and the
  stream ends in `io.EOF`
- `stream_truncated`, that a body ending without the provider's
  completion marker yields `providers.ErrStreamTruncated` and never
  `io.EOF`
- `stream_split_reads`, the same stream re-served in 1, 3, and 17 byte
  writes, because network writes do not respect event boundaries
- `errors`, that upstream failures map to the right `ErrorClass`, that
  the status is preserved, that the API key never appears in the error
  text, and that a secret planted in the upstream body never reaches
  `ProviderError.Message`
- `cancel_releases_body`, that cancelling the context returns from
  `Recv` and releases the upstream connection
- `close_is_idempotent`, that a second `Close` succeeds and that `Recv`
  after `Close` returns promptly instead of blocking

The suite was written and made to pass against the reference
`openaiwire` adapter before the Anthropic and Gemini adapters existed,
so it could gate them rather than be retrofitted to them.

A separate registry test builds every provider kind declared in config
([internal/providers/registry_test.go](../../internal/providers/registry_test.go)),
which is the only way to catch an adapter that exists but was never
imported into the factory.

## Consequences

The ingress is provider agnostic. Caching, budgeting, cost attribution,
and the stream pump are written once against one shape.

A new adapter has a definition of done that is not a matter of opinion.
Supplying a `Suite` is most of the work of proving it correct, and the
properties that are easy to forget, such as key leakage in an error
message and idempotent `Close`, are checked by default rather than
remembered.

The suite tests recorded response shapes, not the live provider. An
adapter that passes conformance can still be wrong about what its
provider actually sends. The README is explicit about which adapters
have been exercised against live traffic and which have only passed the
suite, and the two live findings that conformance could not have caught
are recorded in the git history: Cerebras answering HTTP 402 for a tier
that was never activated, and Gemini reporting reasoning tokens
separately from the visible answer while charging them at the output
rate.

Translation is lossy in one direction. A provider feature with no OpenAI
equivalent has nowhere to go. The Gemini adapter honours both spellings
of the output token cap for exactly this reason, because dropping the
newer one would silently hand the caller Gemini's default ceiling and
bill them for it.

Non stream and stream paths in each adapter both have to produce the
shape, so the translation logic exists twice per translated provider.

## Alternatives considered

**A `Provider` interface and prose documentation of what implementations
must do.** Rejected. Prose is not run. The properties at issue here,
truncation reporting and secret redaction in particular, are precisely
the ones an implementer skips when they are only written down.

**Let the ingress branch on provider kind and speak each wire format.**
Rejected. It puts N branches on the hottest path in the system and makes
every subsequent feature an N way change. It also spreads the knowledge
of a provider's quirks across the request path instead of confining it
to one package.

**A neutral internal representation rather than the OpenAI shape.**
Rejected. The gateway serves an OpenAI compatible API, so a neutral form
would be translated to OpenAI on the way out anyway, adding a hop and a
second place for a field to get lost. The OpenAI shape is not neutral,
but it is the shape the product commits to.

**Per adapter tests written by each adapter's author.** Rejected. They
drift. Three sets of tests written at three times cover three different
sets of properties, and the gaps are invisible until a provider hits
one.
