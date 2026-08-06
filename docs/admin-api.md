# Admin API

A read-only HTTP API that answers two questions: what each tenant is
allowed to spend, and what it has spent so far.

## Where it listens

The admin API and the Prometheus metrics endpoint share one listener,
`server.admin_listen`, which defaults to `127.0.0.1:9090`.

> **This listener is operator data and defaults to loopback. Keep it
> there.**
>
> A response from this API carries tenant names, configured limits, and
> money. The metrics endpoint next to it carries token spend and latency
> profiles. None of that is a caller's business, and together they are a
> readable map of what a deployment is worth and where it is soft.
>
> There is no authentication on this listener. Loopback is the control. If
> you need it reachable from elsewhere, put it behind something that
> authenticates, on a network callers cannot route to, and do not simply
> widen the bind address.

The narrowness of the API is a second line of defense rather than the
first. A response never carries a client key, a provider key, a prompt, or
a completion, and the package serving it is not given access to any of them
in the first place. Nothing here would become unsafe if the listener were
exposed. It would still be a mistake.

## The API only exists when tenants do

Budgeting is optional. If no tenant is configured, there is nothing to
report, and the tenant endpoints are not mounted at all. `/metrics` is
still served.

In that state, `GET /admin/tenants` returns the Go HTTP server's plain-text
404 rather than the JSON envelope documented below. If you are getting an
unstyled `404 page not found`, the answer is usually that `auth.tenants` is
empty, not that the path is wrong.

## Endpoints

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/admin/tenants` | Every configured tenant, sorted by name. |
| `GET` | `/admin/tenants/{name}` | One tenant. |
| `GET` | `/healthz` | Liveness of the admin listener itself. |
| `GET` | `/metrics` | Prometheus exposition. See [observability.md](observability.md). |

`HEAD` works everywhere `GET` does. Everything else gets a 405 carrying
`Allow: GET`.

The listing is sorted so that two dumps of it can be diffed. It is wrapped
in an object rather than returned as a bare array, so the schema can grow a
sibling field later without breaking a parser.

## `GET /admin/tenants`

```
curl -s http://127.0.0.1:9090/admin/tenants
```

```json
{
  "tenants": [
    {
      "name": "demo",
      "limits": {
        "requests_per_minute": 60,
        "tokens_per_minute": 100000,
        "daily_usd": 1,
        "monthly_usd": 10,
        "fail_closed": true
      },
      "daily_spent_usd": 0.184232,
      "monthly_spent_usd": 2.41007,
      "committed_usd": 0.0031,
      "daily_remaining_usd": 0.812668,
      "monthly_remaining_usd": 7.58683
    },
    {
      "name": "internal-batch",
      "limits": {
        "requests_per_minute": 0,
        "tokens_per_minute": 0,
        "daily_usd": 0,
        "monthly_usd": 0,
        "fail_closed": false
      },
      "daily_spent_usd": 3.902114,
      "monthly_spent_usd": 41.88006,
      "committed_usd": 0,
      "daily_remaining_usd": null,
      "monthly_remaining_usd": null
    }
  ]
}
```

`GET /admin/tenants/demo` returns the `demo` object above on its own, not
wrapped.

## Fields

| Field | Type | Meaning |
|---|---|---|
| `name` | string | The tenant name from config. |
| `limits` | object | The configured allowance, echoed back. `0` on any numeric field means unlimited. |
| `daily_spent_usd` | number | Settled spend in the current rolling 24 hour window. |
| `monthly_spent_usd` | number | Settled spend in the current rolling 30 day window. |
| `committed_usd` | number | Reserved but not yet settled. Requests currently in flight. |
| `daily_remaining_usd` | number or null | What one more request may cost today. |
| `monthly_remaining_usd` | number or null | The same for the month. |

Money rides as JSON numbers rather than strings, so a dashboard can plot it
without parsing.

Spend is reported as of now rather than as of the last request. Yesterday's
window shows as released even if nothing has arrived since, which is what
somebody reading this at the start of a day needs to see.

## What `null` remaining means

`null` means **no cap is configured**, so there is no such thing as
remaining. It is not an error, and it is not zero.

The distinction is the whole reason the field is a nullable number instead
of a plain one. A literal `0` there would read as "exhausted", which is the
exact opposite of what an absent cap means, and that is the kind of
misreading that gets acted on at 3am.

The key is always present, never omitted. A consumer can therefore tell "no
cap" apart from "this build did not have the field", which it could not do
if the key simply vanished.

## Negative remaining is real

`remaining` is allowed to go below zero, and it is not clamped.

The arithmetic is `limit - spent - committed`. In-flight reservations count
against headroom because the enforcer counts them too: a figure that
ignored reservations would show room the gateway will refuse to hand out.

A negative value means the tenant finished slightly over budget. That is a
bounded, documented property of reserve-then-settle rather than a bug, and
clamping at zero would hide the one number that shows it happened. The
bound is explained in [cost-accounting.md](cost-accounting.md).

## Errors

Every failure on this API answers in the same JSON envelope as the gateway
itself, so operator tooling can parse both surfaces the same way. No caller
ever meets the plain-text default.

```json
{
  "error": {
    "message": "tenant is not configured",
    "type": "invalid_request_error",
    "code": "tenant_not_found"
  }
}
```

| Status | `code` | When |
|---|---|---|
| 404 | `tenant_not_found` | `GET /admin/tenants/{name}` for a name that is not configured. |
| 404 | `not_found` | Any other unknown path under this handler. |
| 405 | `method_not_allowed` | Anything but `GET` or `HEAD`. Carries `Allow: GET`. |

The requested name is not echoed back in the 404. It is caller-controlled
text, and repeating it buys the operator nothing they did not already type.

## `GET /healthz`

```json
{ "status": "ok" }
```

This probes the admin listener specifically, so it can be checked
independently of the gateway listener, which has a `/healthz` of its own.
An admin listener that is up while the gateway one is not is a real state
and worth being able to distinguish.

## What is not here

There is no `price_version` field on any response. Pricing is not a
dependency of the package serving this API, and inventing one at the edge
would let the two drift. The version that priced a given request lives on
that request's ledger row instead.

There is no write surface. Limits come from configuration, and a limit that
could be raised over HTTP is not much of a limit.
