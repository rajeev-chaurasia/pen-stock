# Configuration

Penstock reads one YAML file, named with `--config` and defaulting to
`config.yaml`. `config.example.yaml` in the repository root is a working
copy of it with every section annotated.

Two things are worth knowing before the tables below.

**Unknown fields are rejected.** A misspelled key fails the load rather
than being ignored. That is deliberate: a typo in `daily_usd` that
silently defaulted to unlimited would be discovered by an invoice.

**Validation reports everything at once.** The loader collects every
problem it finds and returns them joined into a single error, so fixing a
config is one pass rather than a sequence of restarts.

## The loopback fail safe

This is the one rule that will stop a gateway from starting, so it comes
first.

If `auth.client_keys` is empty **and** no tenant declares any keys, then
`server.listen` must bind loopback. `127.0.0.1:8080` and `localhost:8080`
pass. `:8080`, `0.0.0.0:8080`, and any routable address are refused at load
time with an error naming the address you gave.

The reasoning is short. An unauthenticated gateway is a door to the
provider API keys configured further down the same file. Anyone who can
reach the port can spend them. Binding wider than loopback is therefore a
deliberate act that has to be paired with a credential.

A tenant key satisfies the rule exactly as a client key does, so a
deployment whose only credentials are tenant keys may bind a public
address.

## Environment variable references

`${VAR}` references are expanded at load time in three places:
`providers[].api_key`, `auth.client_keys[]`, and `auth.tenants[].keys[]`.
Secrets never have to live in the file.

A referenced variable that is unset **or set to empty** fails the load.
Empty counts as missing on purpose: a `${VAR:-}` default in a compose file
would otherwise boot a gateway whose every upstream call fails
authentication, which is a much worse failure than not booting.

This is why the `tenants` block and most providers are commented out in
`config.example.yaml`. That file has to load in a bare shell with nothing
exported.

## `server`

| Option | Type | Default | If omitted |
|---|---|---|---|
| `listen` | string `host:port` | `127.0.0.1:8080` | Binds loopback. See the fail safe above. |
| `admin_listen` | string `host:port` | `127.0.0.1:9090` | Metrics and the tenant API bind loopback only. |
| `max_inflight` | int | `256` | 256 concurrent requests, then a 503. Negative is rejected. |
| `read_timeout_ms` | int (ms) | `30000` | 30s to read a request. |
| `upstream_timeout_ms` | int (ms) | `120000` | 120s for the whole upstream call. |
| `stream_idle_timeout_ms` | int (ms) | `60000` | A stream may sit 60s without a byte before the gateway gives up. |

Every `*_timeout_ms` field is capped at 3600000 (one hour). A larger value
is almost certainly a units mistake, and a negative one would disable the
deadline entirely rather than shortening it, so both are rejected instead
of being defaulted.

`admin_listen` carries the metrics endpoint and the tenant budget API.
Token spend and latency profiles are operator data, not caller data, which
is why they are not on the listener callers use. See
[admin-api.md](admin-api.md).

`max_inflight` is a memory ceiling as much as a load control. Each request
in flight can hold a request body plus an upstream response, so unbounded
concurrency means unbounded memory. Requests beyond the limit are shed with
a 503 rather than queued.

## `auth`

Two kinds of credential, both presented as `Authorization: Bearer <key>`.
Both open the gateway. Only one of them has an identity.

### `auth.client_keys`

| Option | Type | Default | If omitted |
|---|---|---|---|
| `client_keys` | list of strings | `[]` | No anonymous keys. Combined with no tenants, this triggers the loopback fail safe. Cannot be combined with `tenants`. |

Each key must be at least 16 characters. Generate them with something like
`openssl rand -hex 32`.

Client keys are anonymous. They authenticate, but spend is attributed to
nobody and no limit applies to them. They are the right choice for a
single-operator local deployment and the wrong choice for anything you need
to bill.

The two kinds do not mix. Setting `client_keys` and `tenants` together
fails the load, because an anonymous key names no account to reserve
against: it would run beside the budgeted keys with no cap on it at all,
and the budgets would read as enforced while a way around them stayed
open. Pick one. Either move every client key under a tenant, or drop the
tenants and run without budgets.

### `auth.tenants`

A tenant gives a set of keys an identity and limits of its own.

| Option | Type | Default | If omitted |
|---|---|---|---|
| `name` | string | required | Load fails. The name is how spend is attributed. |
| `keys` | list of strings | required, non-empty | Load fails. A tenant with no keys can never be reached. |
| `requests_per_minute` | int | `0` | Unlimited. |
| `tokens_per_minute` | int | `0` | Unlimited. Counts prompt plus completion. |
| `daily_usd` | float | `0` | Unlimited. Rolling 24 hour window. |
| `monthly_usd` | float | `0` | Unlimited. Rolling 30 day window. |
| `fail_closed` | bool | `false` | The request is allowed when accounting cannot answer. |

`name` becomes a metrics label and a log field, so it is restricted to
letters, digits, underscore and hyphen. Whitespace and separators would
either need quoting or silently split the value downstream.

Zero means unlimited on every numeric limit. That keeps a partially
configured tenant usable instead of locked out by an omission, which is the
failure mode you want when somebody adds a tenant at 3am and forgets a
field.

`fail_closed` decides what happens when the accounting store cannot answer.
`true` denies the request, which is what a hard cap on real money needs.
`false` allows it and leaves an alert behind, which suits an advisory
limit. Denials from this path show up as `accounting_unavailable` on the
denials panel.

A key belongs to exactly one tenant. Repeating a key under a second tenant,
or in `client_keys`, fails the load: a request that could be billed to
either of two tenants can be billed to neither. Error messages name the
tenant and the index, never the key itself, because error text reaches
logs.

Negative limits are rejected. Omit the field or write `0` for unlimited.

```yaml
auth:
  client_keys: []
  tenants:
    - name: demo
      keys: ["${PENSTOCK_DEMO_KEY}"]
      requests_per_minute: 60
      tokens_per_minute: 100000
      daily_usd: 1.00
      monthly_usd: 10.00
      fail_closed: true
```

## `accounting`

| Option | Type | Default | If omitted |
|---|---|---|---|
| `ledger_path` | string | `""` | No ledger is written. |

The ledger is the append-only record of what each settled request cost.
With no path, spend is still enforced and still reported, but only the
running totals survive: a restart forgets which requests produced them.

This section is absent from `config.example.yaml`, so that a local run does
not litter the working directory with audit files nobody asked for. Format
and semantics are in [cost-accounting.md](cost-accounting.md).

```yaml
accounting:
  ledger_path: /var/lib/penstock/cost.jsonl
```

## `cache`

Answering a repeated question without calling a provider. Off by
default, because a cache is a correctness decision as much as a
performance one.

| Option | Type | Default | If omitted |
|---|---|---|---|
| `enabled` | bool | `false` | Nothing is cached. Both tiers are off. |
| `max_entries` | int | `4096` | The exact tier holds this many answers per gateway. Zero takes the default rather than meaning unlimited, since an unbounded cache is a memory leak with a friendly name. |
| `ttl_seconds` | int | `300` | How long a stored answer stays usable. |
| `max_temperature` | float | `0.0` | The highest temperature still considered reproducible. See below, because the default is stricter than it looks. |

### What is never cached

Eligibility is decided before any lookup, and a refusal is counted
separately from a miss so a low hit rate can be read correctly. In the
order the checks run:

| Refused | Reason label |
|---|---|
| A body that does not parse | `unparsable_request` |
| `tools`, `tool_choice`, `functions` or `function_call` carrying content | `tool_use` |
| Any `seed`, including `seed: 0` | `seeded` |
| `logprobs: true`, or `n` above 1 | `unsupported_options` |
| A temperature above `max_temperature`, **or no temperature at all** | `temperature_too_high` |

Two of those surprise people. **A request that names no temperature is
refused**, because the default is whatever the provider chooses and the
gateway cannot promise the same answer twice. And `seed: 0` is a
deliberately pinned seed, not an absent one, so it is honoured rather
than ignored.

Tool calls are excluded because an answer whose content is an
instruction to act is not safe to replay. That is a correctness rule,
not a tuning knob, so none of these are configurable.

Entries are keyed per tenant, so one tenant's answer cannot surface in
another's response. That is part of the key rather than a filter applied
afterwards, which is [ADR 0010](adr/0010-tenancy-is-part-of-the-cache-key.md).

### `cache.semantic`

Answering a question that is merely *similar* to one already asked. A
separate switch from the exact tier, because it is the tier that can be
wrong.

| Option | Type | Default | If omitted |
|---|---|---|---|
| `enabled` | bool | `false` | The semantic tier is off, which is the recommended setting. |
| `threshold` | float | `0.95` | Cosine similarity below which a neighbour is not the same question. Rejected at load if below `0.95` or above `1`. |
| `embed_api_key` | string | required when enabled | Load fails. Supports `${ENV_VAR}` like every other secret here. |
| `embed_url` | string | Google AI Studio v1beta | |
| `embed_model` | string | `gemini-embedding-001` | |
| `max_per_tenant` | int | `1024` | Stored vectors per tenant. |

**Read [cache-quality.md](cache-quality.md) before switching this on.**
Over 257 labelled probes, questions with opposite meanings scored higher
than genuine paraphrases often enough that no threshold separates them.
At the 0.95 floor, genuine paraphrases hit 24 percent of the time and
opposite-meaning questions hit 43.9 percent: a semantic hit there is
more likely to be answering the wrong question than the right one. The
floor is set above the highest measured opposite rather than below the
lowest paraphrase, so the tier fires rarely, which is the correct trade
when the failure mode is answering the opposite of what was asked.

The gateway logs a warning at startup when this tier is enabled.

```yaml
cache:
  enabled: true
  max_entries: 4096
  ttl_seconds: 300
  max_temperature: 0.0
```

## `providers`

At least one provider is required.

| Option | Type | Default | If omitted |
|---|---|---|---|
| `name` | string | required, unique | Load fails. This is the name routes refer to. |
| `kind` | enum | required | Load fails. |
| `base_url` | string | required | Load fails. Must be a valid `http` or `https` URL with a host. |
| `api_key` | string | required, non-empty | Load fails, including when a `${VAR}` expands to empty. |
| `models` | list of strings | `[]` | Unrestricted: any route may target this provider. |

`kind` is one of `groq`, `openai_compat`, `openai`, `cerebras`, `mistral`,
`openrouter`, `gemini`, `anthropic`.

`kind` selects the adapter. `base_url` is asked for even on kinds that know
their own default endpoint, so that pointing at a proxy or a regional
endpoint is an edit rather than a rebuild. `openai_compat` is the one kind
with no default at all, because self-hosted runtimes live wherever you put
them.

`models` is a whitelist, not a declaration. When it is non-empty, a route
targeting this provider must ask for a model that appears in it, and the
load fails otherwise. Leaving it empty removes that check rather than
removing the models.

A `kind` whose adapter is not compiled into the binary fails at startup
rather than at load, since registration happens later than validation.

## `routes`

At least one route is required. A route maps an incoming model name to the
provider or chain that serves it.

| Option | Type | Default | If omitted |
|---|---|---|---|
| `model` | string | required, unique across routes | Load fails. |
| `provider` | string | one of `provider` / `providers` required | Load fails if neither is set. |
| `providers` | list of strings | one of `provider` / `providers` required | Load fails if neither is set. |
| `strategy` | enum | `priority` | Providers are tried in configured order. |
| `provider_models` | map provider to string | `{}` | Every provider is asked for the route's own model name. |

Setting both `provider` and `providers` is rejected: naming both leaves the
intended order ambiguous. A provider repeated inside one chain is rejected
for the same reason.

`strategy` is `priority` (configured order), `least_latency` (fastest
healthy provider first), or `round_robin` (spread load, which across
independent free tiers means spreading the quota).

`provider_models` renames the model per provider. A chain across different
vendors rarely shares a vocabulary, so without this the second provider in
such a chain would be asked for a model it has never heard of. A rename
naming a provider that is not in the chain is rejected, because it is
almost always a typo in one of the two names.

```yaml
routes:
  - model: auto
    providers: [groq, cerebras, openrouter]
    strategy: round_robin
    provider_models:
      groq: llama-3.3-70b-versatile
      cerebras: gpt-oss-120b
```

## `router`

How hard a route tries before it gives up, and how long a provider that
keeps failing is left alone.

| Option | Type | Default | If omitted |
|---|---|---|---|
| `max_attempts` | int | `3` | Three upstream calls per client request, across the whole chain. |
| `retry_base_delay_ms` | int | `100` | First backoff step. Later steps grow exponentially with full jitter. |
| `max_retry_delay_ms` | int | `2000` | Cap on any single backoff. |
| `breaker_threshold` | int | `5` | Consecutive failures that park a provider. |
| `breaker_cooldown_seconds` | int | `30` | How long it stays parked before one probe is let through. |

These are global, not per route, because provider health is shared. A
provider that is down is equally down for every model it serves, and
that is the point: one route learning a provider is broken spares the
others from finding out the same way. Per route breaker settings would
mean several disagreeing opinions about the same provider.

`max_attempts` counts calls, not retries, and it counts across the chain
rather than per provider. A chain of five with a budget of three tries
three of them and stops. That is deliberate: each attempt is a real
upstream call, so a generous budget aimed at reliability turns one
client request into a storm against providers that are already
struggling.

Rejected at load: any negative value, `max_attempts` above 10,
`breaker_cooldown_seconds` above 3600, and a `retry_base_delay_ms`
larger than `max_retry_delay_ms`, since that last pair would make the
cap the only value that ever applied.

Zero means "take the default" on every field, so a config that omits the
block behaves exactly as it did before the block existed.

```yaml
router:
  max_attempts: 4
  breaker_threshold: 3
  breaker_cooldown_seconds: 60
```

## `telemetry`

| Option | Type | Default | If omitted |
|---|---|---|---|
| `service_name` | string | `penstock` | Spans are attributed to `penstock`. |
| `otlp_endpoint` | string | `""` | Trace export is disabled. |
| `log_level` | enum | `info` | Info and above. |

`log_level` is one of `debug`, `info`, `warn`, `error`. Anything else fails
the load.

An empty `otlp_endpoint` disables export rather than falling back to a
default collector, so a machine with nothing listening does not spend every
request retrying an exporter.

Metric names and labels are in [observability.md](observability.md).
