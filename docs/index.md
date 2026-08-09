# Penstock documentation

Penstock is a single-binary LLM gateway: one OpenAI-compatible endpoint in
front of several providers, with fallback chains, per-tenant budgets, and
cost attributed to whichever provider actually answered.

For what the project is and how to build it, see the
[repository README](../README.md). Everything below assumes you already
have a binary.

## Getting started

| Page | Read it when |
|---|---|
| [configuration.md](configuration.md) | You are writing your first `config.yaml` and want every option, by section, with its type and default. Start at the loopback fail safe, which decides whether the gateway will agree to start at all. |

## How it works

| Page | Read it when |
|---|---|
| [architecture.md](architecture.md) | You want the shape of the system: the two listeners, what each package owns and what it deliberately does not, which way the dependencies point, and what happens when a provider, the accounting store, the embedder, or a stream fails. |
| [request-lifecycle.md](request-lifecycle.md) | You want one request followed end to end, streamed and not, including the point after which fallback stops being legal and the reason the cache runs before the budget. |
| [adr/README.md](adr/README.md) | You want the reasoning rather than the result. Eleven records, one per load bearing decision, each naming what was rejected and why. |

## Operating it

| Page | Read it when |
|---|---|
| [admin-api.md](admin-api.md) | You need tenant limits and live spend, or you are wondering what a `null` remaining means and why a negative one is not a bug. |
| [cost-accounting.md](cost-accounting.md) | You need to know how a request is priced, how reserve-then-settle works, how far over budget a tenant can finish, and what the ledger rows look like. |
| [observability.md](observability.md) | You are wiring up Prometheus, Grafana, or tracing, and want the metric names, their labels, and the panels that deliberately do not exist. |

## The measurements

| Page | Read it when |
|---|---|
| [semantic-caching.md](semantic-caching.md) | You are about to turn the semantic cache tier on and want the short version of why it ships off. |
| [cache-quality.md](cache-quality.md) | You want the long version: 257 labelled probes, the full threshold sweep, and a false hit rate published beside the hit rate. |
| [comparison.md](comparison.md) | You want the gateway overhead measured against LiteLLM, with the method, the caveats, and the runs that were discarded. |

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
