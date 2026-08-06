# 0003. Metrics and the operator API on a separate admin listener

## Status

Accepted.

## Context

The gateway's metrics are not neutral telemetry. They carry token counts
and spend labelled by tenant, provider, and model, plus latency and time
to first token profiles. Anyone who can read `/metrics` can read how much
each tenant spends, which models they use, and which upstreams the
operator has contracts with.

The caller facing listener is the one exposed to whoever is allowed to
make completion requests, which in the deployments this gateway targets
is a wider set of people than the operators. Serving `/metrics` there
means every client can read every tenant's spend.

The same argument applies to `GET /admin/tenants`, which reports
configured limits and live balances.

## Decision

Two listeners.

`server.listen` serves the OpenAI compatible proxy surface and defaults
to `127.0.0.1:8080`. `server.admin_listen` serves `GET /metrics` and the
admin API and defaults to `127.0.0.1:9090`
([internal/config/config.go](../../internal/config/config.go),
`DefaultListen` and `DefaultAdminListen`).

The admin mux is built separately in
[cmd/penstock/main.go](../../cmd/penstock/main.go) and the metrics
handler is mounted on it and nowhere else. Both listeners get bounded
header read timeouts and idle connection limits.

The admin API is written so that the listener separation is a second
line of defence and not the only one. A response carries tenant names,
configured limits, and money, and the package is not given access to a
client key, a provider key, a prompt, or a completion in the first place
([internal/admin/admin.go](../../internal/admin/admin.go)). Nothing in
it would become unsafe if the listener were exposed, which is the
property that lets the default stay loopback without making the feature
useless.

## Consequences

An operator has to configure and route to two ports. In containers that
is a real cost, and it caused a live bug: metrics moved to the admin
listener while the Prometheus scrape config still pointed at the proxy
port, so the scrape had never worked. The fix binds the admin listener
inside the compose network and leaves it unpublished
([deploy/prometheus.yml](../../deploy/prometheus.yml),
[deploy/docker-compose.yml](../../deploy/docker-compose.yml)), and
[docs/observability.md](../observability.md) leads with the change an
operator has to make.

A default of loopback means the metrics are unreachable from another
host until someone changes it deliberately. That is the intended
friction. It also means the failure mode of forgetting is "no data"
rather than "data leaked", which is the correct direction.

There is no authentication on the admin listener. The address is the
whole access control story right now, which is why the default is
loopback and why the doc says to keep it there.

## Alternatives considered

**One listener with `/metrics` protected by a path rule in front of the
gateway.** Rejected. It makes the gateway's safety depend on a reverse
proxy config living somewhere else, and the failure mode of getting that
config wrong is silent exposure rather than a broken deployment.

**One listener with bearer auth on `/metrics`.** Rejected as the
primary mechanism. Prometheus supports it, but it puts a credential in
the scrape config for data that has no reason to be reachable from the
caller network at all. Two listeners removes the question instead of
answering it.

**No metrics endpoint, push to an OTLP collector only.** Rejected. It
would have removed the surface, but it makes local development and the
bundled compose stack depend on a collector, and a pull endpoint is what
the Grafana dashboards in `deploy/grafana` are built against. Tracing
does go out over OTLP; metrics stay pull based.

**Serve the admin API on the proxy listener under a path prefix.**
Rejected for the same reason as metrics, and additionally because the
proxy surface is an OpenAI compatible API. Adding non standard paths to
it invites a client library to trip over them.
