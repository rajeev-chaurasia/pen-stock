# Observability

What Penstock exposes, where it exposes it, and how to get a Grafana in
front of it in one command.

## The metrics endpoint

Prometheus exposition is served at `GET /metrics` on the **admin
listener**, `server.admin_listen`, default `127.0.0.1:9090`. Not on the
listener callers use.

Token spend and latency profiles describe what a deployment is worth and
where it is soft. That is operator data. It shares a listener with the
[admin API](admin-api.md), and the same warning applies: there is no
authentication there, loopback is the control.

Metrics live on a private registry rather than the client library's default
global one, so tests and multiple instances in one process never collide.

## Metrics

### `penstock_cost_usd_total`

Counter. Labels: `tenant`, `provider`, `model`. Unit: USD.

Settled spend. Incremented once per request, after the answer arrives and
the real usage is known, with the figure the request was actually billed
at. The flagship signal, and the reason for the whole project.

`provider` is whoever actually answered, which on a fallback chain is not
necessarily the provider the route names first. Anonymous `client_keys`
carry no tenant, so their spend lands under an empty `tenant` label rather
than disappearing.

A model absent from the price table contributes zero here while still
counting tokens. Zero means unpriced, never free. See
[cost-accounting.md](cost-accounting.md).

### `penstock_denials_total`

Counter. Labels: `tenant`, `reason`.

Requests refused by a tenant limit before anything was spent. `reason` is
one of:

| `reason` | HTTP | Meaning |
|---|---|---|
| `request_rate` | 429 | Over `requests_per_minute`. Refills by waiting; the client is told how long. |
| `token_rate` | 429 | Over `tokens_per_minute`. Also refills by waiting. |
| `daily_budget` | 402 | Out of money for the rolling day. Waiting does not help. |
| `monthly_budget` | 402 | Out of money for the rolling month. Waiting does not help. |
| `accounting_unavailable` | 503 | The store could not answer and this tenant is `fail_closed: true`. |
| `unknown_tenant` | 500 | A key authenticated but names a tenant the enforcer never received. Always a config fault on this side. |

The 429 versus 402 split matters more than it looks. A budget answered as
429 sends a well-behaved client into a retry loop that can never succeed,
so it is answered as 402 instead: payment required, retrying will not fix
it.

### `penstock_requests_total`

Counter. Labels: `path`, `provider`, `code`.

Every request the gateway answered, by normalized path, answering provider,
and HTTP status.

`path` is collapsed to a fixed set (`/v1/chat/completions`, `/v1/models`,
`/healthz`, and `other`) before it becomes a label. A scanner walking
random URLs would otherwise mint unbounded cardinality, and unbounded
cardinality is how a metrics backend dies.

### `penstock_request_duration_seconds`

Histogram. Labels: `path`, `provider`, `stream`.

End to end duration, gateway edge to gateway edge. Buckets are exponential
from 5ms to 120s in 14 steps, which spans a fast cache hit through a long
streamed completion.

The `stream` label separates the two populations, and it is worth using. A
streamed request's duration is dominated by how long the answer takes to
finish, not by how responsive it felt.

Query the `_bucket` series through `histogram_quantile`. An average latency
over a mix of sub-second and minute-long requests describes no request that
ever happened.

### `penstock_ttft_seconds`

Histogram. Label: `provider`.

Time to first token on a streamed response. Buckets run 25ms to 30s in 12
steps.

This is the latency a human actually notices, and the one worth routing on.
A provider with a slow total duration but a fast first token feels
responsive; the reverse feels broken. There is no `path` label here, so
path filters do not apply to this metric.

### `penstock_tokens_total`

Counter. Labels: `provider`, `direction`, where `direction` is `prompt` or
`completion`.

Throughput in the unit providers bill in. Reasoning tokens are counted as
`completion`, because that is how providers charge for them.

Streaming requests are opted into upstream token usage wherever the
provider supports it, because usage that is never reported cannot be billed
or budgeted.

### `penstock_cache_events_total`

Counter. Label: `event`.

Registered ahead of the caching phase and **not yet emitted by anything**.
It will always read zero on this build. Deliberately, no dashboard panel
plots it: a permanently empty graph teaches operators to ignore panels.

## Panels that do not exist, and why

Worth knowing before you go looking for them.

- **Cost per tenant per token, or tokens by tenant.**
  `penstock_tokens_total` carries no `tenant` label, so token spend cannot
  be divided by tenant. The ledger can answer this offline.
- **Cache hit ratio.** The metric is registered but never emitted.
- **Load shedding specifically.** A request rejected at `max_inflight`
  surfaces as `code="503"`, mixed in with fail-closed accounting denials.
  Only the denials metric distinguishes the latter.
- **Fallback chain hops.** Nothing counts how often a chain moved to its
  second provider. The shape is visible indirectly, as a rise in one
  provider's share on the cost dashboard.
- **Budget headroom.** Remaining USD is not a metric. It is on the
  [admin API](admin-api.md), which reads the same counters the request path
  writes.
- **Ledger write failures.** Logged at error level, not counted.

## The local stack

```
docker compose -f deploy/docker-compose.yml up
```

Paths inside the compose file are relative to it, so running it from inside
`deploy/` works too.

| Service | Host address | What it is |
|---|---|---|
| penstock | `127.0.0.1:8080` | The gateway. |
| grafana | `127.0.0.1:3000` | Dashboards, anonymous admin. |
| prometheus | `127.0.0.1:9091` | Scrapes the gateway every 5s. |
| jaeger | `127.0.0.1:16686` | Traces. |
| llmsim | not published | Deterministic mock provider, so you can generate traffic without spending quota. |

Every port is published on `127.0.0.1` only. Do not drop that prefix. The
compose gateway runs with no client authentication and Grafana runs with
anonymous admin, so a bare `8080:8080` hands both to anyone who can route
to the host.

### Making the scrape work

The gateway's metrics are on the admin listener, and by default that
listener binds `127.0.0.1:9090`, which inside a container means the
container's own loopback. Prometheus cannot reach it from the next
container over.

`deploy/penstock.yaml` therefore needs:

```yaml
server:
  listen: ":8080"
  admin_listen: ":9090"
```

That port is not published to the host, and should not be. It stays
reachable on the compose network for Prometheus and nowhere else.

`deploy/prometheus.yml` already targets `penstock:9090`.

## Dashboards

Two dashboards ship provisioned, in
`deploy/grafana/provisioning/dashboards/`:

| File | Title | For |
|---|---|---|
| `penstock-cost.json` | Penstock cost | Spend rate by tenant and by provider, rolling 24 hour spend per tenant, top models by spend, denials by reason. |
| `penstock-performance.json` | Penstock performance | Request rate by status code, duration and time-to-first-token quantiles, token throughput, error rate by provider. |

They land in Grafana automatically. `dashboards.yml` in that same directory
is the provisioning provider that loads them, sitting alongside the
existing `datasources/datasources.yml`. Both directories are already inside
the `./grafana/provisioning` volume the compose file mounts, so no volume
change is needed.

Open `http://127.0.0.1:3000` and they are under Dashboards, or go straight
to `/d/penstock-cost` and `/d/penstock-performance`.

A few things about how they are wired:

- Dashboards are provisioned read-only. Edits made in the browser are not
  persisted, because these files are the source of truth. Change the JSON.
- The Prometheus datasource has a pinned `uid` of `prometheus`. Panels
  reference their datasource by uid, and a uid Grafana invents at first
  boot differs between machines, which would leave every panel on a fresh
  stack pointing at nothing.
- Grafana rereads the directory every 30 seconds, so editing a dashboard
  JSON does not need a restart.
- Every panel carries a description explaining what a healthy shape looks
  like. Hover the title.

If a dashboard loads but every panel says No data, check Prometheus at
`http://127.0.0.1:9091/targets` first. A `penstock` target reading DOWN is
almost always the `admin_listen` change above.

## Tracing

Set `telemetry.otlp_endpoint` and traces export over OTLP. The compose
stack points the gateway at `jaeger:4318` and publishes the Jaeger UI on
`127.0.0.1:16686`.

One span opens per request. The span name uses the normalized path, for the
same cardinality reason the metric label does; the raw path rides along as
an attribute where it cannot mint span names.

An empty `otlp_endpoint` disables export rather than falling back to a
default collector.
