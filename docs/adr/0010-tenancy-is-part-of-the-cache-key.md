# 0010. Tenancy is part of the cache key, not a filter applied afterwards

## Status

Accepted.

## Context

The cache stores prompts and completions. Both are the most sensitive
data flowing through the gateway: a prompt is whatever the tenant chose
to send, and a completion is a model's answer to it. Two tenants asking
the same question is not a coincidence to be exploited, it is a
coincidence that has to be prevented from mattering.

There are two shapes the isolation could take. Store everything in one
map and filter results by tenant, or store each tenant's entries
separately so a lookup only ever reaches its own.

They are equivalent when the code is correct. They are not equivalent
when it is not. A filter is a line of code that can be forgotten, moved,
short circuited by an early return, or skipped on a path added later. The
failure mode is one tenant's prompt or completion appearing in another
tenant's response, which is a data leak that looks exactly like a cache
hit.

## Decision

Tenancy is structural. Entries live under their tenant, and a lookup
reaches one tenant's map before it ever sees a hash.

The exact tier holds `byTenant map[string]map[exactKey]*exactNode`. A
`Get` indexes by tenant first, so the only map it can search is the one
its own tenant name selected
([internal/cache/exact.go](../../internal/cache/exact.go)). The semantic
tier holds vectors the same way, one independent set per tenant, and
`scanTenant` takes that set as its only input: no other tenant's vectors
are reachable from the function, so a cross tenant answer is not a value
it could return even if called wrongly
([internal/cache/semantic.go](../../internal/cache/semantic.go)).

`Key` carries `Tenant` as a field, and the tenant is deliberately *not*
folded into the hash
([internal/cache/key.go](../../internal/cache/key.go)). Isolation is
enforced by where an entry is stored, not by the digest. Keeping the
tenant out of the preimage lets a test prove that by inspecting the key,
and it means the guarantee does not rest on a hash property.

The digest itself is SHA-256 rather than a faster non cryptographic hash,
because its input is attacker influenced: a caller writes its own prompt.
With a hash that can be collided on demand, one caller could craft a
request landing on another key. That risk is confined within a tenant by
the structure above, but the cost of SHA-256 is invisible next to the
model call it avoids, so there was no reason to accept it at all.

This is not configurable. The package doc says why: the failure it
prevents is a data leak rather than a performance regression.

## Consequences

Two tenants asking the same question get two entries and two upstream
calls. That is a real hit rate cost, paid on every deployment, and it is
the trade being bought.

The guarantee is checkable by reading the type. `map[tenant]map[key]` is
an argument that a reviewer can complete in one step, where "every
lookup path filters correctly" is an argument that has to be re-verified
whenever a lookup path is added.

Memory is held per tenant. The exact tier's `MaxEntries` is a ceiling
across all tenants with a global LRU order, so one noisy tenant can still
evict another's entries, but it cannot read them. The semantic tier
bounds per tenant instead, at 1024 vectors, and deletes an emptied
tenant's map so a gateway seeing many short lived tenants does not
accumulate one map per tenant it has ever met.

A future shared cache tier, where a deliberately public prompt could be
shared across tenants, is not reachable from here without changing the
data structure. That is intentional.

## Alternatives considered

**One global map, filtered by tenant on read.** Rejected. Correct today,
one forgotten check away from a leak tomorrow, and the check has to be
right on every path that ever reads the map. The bug it enables is
silent: a wrong hit looks identical to a right one.

**Fold the tenant into the hash preimage and keep one flat map.**
Rejected. It is much better than filtering, since a cross tenant hit
would then require a hash collision. But the isolation becomes a property
of a hash function rather than of a data structure, it cannot be
demonstrated by inspecting a key, and a future change to the
canonicalization scheme would silently change what isolation depends on.

**Make cross tenant sharing configurable, off by default.** Rejected. A
default is a thing that gets changed by someone chasing a hit rate
number. The failure mode here is not a slower gateway, it is one
tenant's data in another tenant's response, and that is not an operator's
call to make.

**Key on the client API key rather than the tenant.** Rejected. A tenant
can hold several keys, so keying on the key would fragment one tenant's
cache across its own credentials for no isolation benefit. `TenantID` is
also a label derived from configuration and never the key itself, which
is what makes it safe to log.
