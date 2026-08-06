# Penstock operator documentation

Penstock is a single-binary LLM gateway: one OpenAI-compatible endpoint in
front of several providers, with fallback chains, per-tenant budgets, and
cost attributed to whichever provider actually answered.

These pages are for whoever runs it. For what the project is and how to
build it, see the repository README.

| Page | What it covers |
|---|---|
| [configuration.md](configuration.md) | Every config option, by section, with types and defaults. Start with the loopback fail safe. |
| [admin-api.md](admin-api.md) | The operator API: tenant limits and spend, and what `null` remaining means. |
| [cost-accounting.md](cost-accounting.md) | How a request is priced, reserve then settle, the overshoot bound, the ledger format. |
| [observability.md](observability.md) | Metric names and labels, the compose stack, and where the Grafana dashboards land. |

## The short version

Bind loopback unless you have configured keys. The loader enforces this,
because an unauthenticated gateway is a door to the provider API keys in
the same file.

Metrics and the admin API live on `server.admin_listen`, separate from the
listener callers use, and default to loopback. Keep them there.

Budgets are enforced by reserving an estimate before the upstream call and
settling the real cost after. A tenant can finish slightly over budget, by
a bounded amount, and the admin API shows that as a negative remaining
rather than hiding it.

The shipped price table is mostly unverified. It is good for watching shape
and not for billing anyone. Check it before you trust an absolute figure.
