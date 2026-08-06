# 0009. The client error envelope is defined once

## Status

Accepted.

## Context

The gateway serves an OpenAI compatible API, so its errors have to arrive
in the envelope client SDKs already know how to parse: an `error` object
with `message`, `type`, and `code`. That is a wire contract, not a
formatting preference.

It had been declared twice. The proxy surface in `internal/ingress` had
one copy and the operator surface in `internal/admin` had another. They
matched, but only by coincidence. A change to either would have made the
gateway answer with two different error shapes depending on which port a
caller reached, and nothing in the build or the tests would have noticed.

The JSON media type had the same problem, sitting as a free standing
constant in a third place while being part of the same contract.

## Decision

One definition, in [internal/httperr](../../internal/httperr/httperr.go).

The package holds `Body`, `Envelope`, the media type constant
`ContentTypeJSON`, the error type vocabulary that clients branch on
(`invalid_request_error`, `rate_limit_error`, `api_error`), and the two
writers `WriteJSON` and `WriteError`. Field order is part of the
serialized form, so it is pinned by a test on the exact bytes rather than
left to whoever edits the struct next.

Both surfaces alias the shared names rather than re-declaring them.
`internal/ingress` keeps `errTypeInvalidRequest`, `errTypeRateLimit`, and
`errTypeAPI` as aliases so its call sites read as they always did, and
type aliases `errorBody` and `errorEnvelope` let the streaming path
compose a truncation frame directly
([internal/ingress/errors.go](../../internal/ingress/errors.go)).
`internal/admin` does the same and routes every failure through one
`writeError`, so no caller ever meets `net/http`'s plain text default
([internal/admin/errors.go](../../internal/admin/errors.go)).

## Consequences

The two listeners cannot drift. A change to the envelope is a change to
one struct, and the byte level test fails if the serialized form moves.

The admin surface answers in the same shape as the proxy surface, so an
operator's tooling can parse both the same way.

The aliases keep the diff small but they also mean the shared names are
not obvious at the call sites. Someone reading `internal/ingress` sees
`errTypeAPI` and has to follow one hop to learn it is a contract value
shared with another package. The alias declarations carry a comment
saying so.

`internal/httperr` is now a dependency of both surfaces, which is a
small coupling between packages that otherwise share nothing. That is
correct here, since what they share is precisely a wire contract.

The vocabulary is fixed at three types. Adding a fourth is a contract
change that both surfaces inherit whether or not either wanted it.

## Alternatives considered

**Leave the two copies and add a test asserting they match.** Rejected.
It encodes the duplication instead of removing it, and the test has to be
remembered when a third surface appears.

**Put the envelope in `internal/ingress` and have `internal/admin`
import it.** Rejected. It makes the operator API depend on the proxy
package for a type, dragging in the request path's dependencies for one
struct, and it implies the proxy owns a contract that both surfaces are
equally bound by.

**Use a third party problem details or error envelope library.**
Rejected. The shape is fixed by OpenAI compatibility, not by a standard,
so a library would be configured into exactly this shape anyway, at the
cost of a dependency in the error path.

**Let each surface define its own errors, on the grounds that the admin
API is not an OpenAI compatible surface.** Rejected. It is the same
gateway, and the reason to give the operator surface a different error
shape from the proxy surface would be an accident of package layout
rather than anything a caller benefits from.
