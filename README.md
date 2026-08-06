# Penstock

A single-binary LLM gateway in Go: multi-provider routing, fallback
chains, per-provider cost accounting, and benchmarks you can reproduce.

A penstock is the pressure pipe that feeds a hydro turbine. It meters,
controls, and survives high-pressure flow. Same job here.

## Security status

Bearer authentication, per-tenant rate limits and per-tenant spend caps
all work, and the loader refuses to bind anything but loopback with no
keys configured. What is still missing before this belongs on a public
address: tenant state lives in memory, so a restart forgets every
window, and there is no TLS, no key rotation, and no multi-node story.
Run it behind something that provides those.

Free provider tiers generally train on submitted prompts, so production
or otherwise sensitive data does not belong here.

## What works today

One OpenAI-compatible endpoint in front of several providers, with
streaming passthrough, fallback chains, and cost attributed to whichever
provider actually answered.

```yaml
routes:
  - model: auto
    providers: [groq, mistral, openrouter]
    strategy: round_robin
    provider_models:
      groq: llama-3.3-70b-versatile
      mistral: mistral-small-latest
      openrouter: "inclusionai/ling-3.0-flash:free"
```

That is one model name in front of three independent free tiers. Their
rate limits are separate, so chaining them multiplies the headroom any
one of them gives you.

### Providers

| Kind | Wire format | Verified |
|---|---|---|
| `groq` | OpenAI | live |
| `mistral` | OpenAI | live |
| `openrouter` | OpenAI | live |
| `gemini` | native, translated | live |
| `openai` | OpenAI | conformance suite |
| `cerebras` | OpenAI | conformance suite |
| `anthropic` | native, translated | conformance suite |
| `openai_compat` | OpenAI | live, against the bundled simulator |

"Live" means real traffic through the gateway against that provider's
API. "Conformance suite" means it satisfies the same contract every
adapter is held to, exercised against recorded response shapes rather
than the live service.

### Behavior worth knowing

- A stream that ends without its provider's completion marker is
  reported as truncated, never as a finished answer. Each provider
  signals completion differently: `[DONE]` on the OpenAI wire, a
  `finishReason` for Gemini, `message_stop` for Anthropic.
- Streaming requests are opted into upstream token usage where the
  provider supports it, because usage that is never reported cannot be
  billed or budgeted.
- Reasoning tokens count as completion tokens, since that is how
  providers charge for them.
- Fallback happens while connecting only. Once a stream has started,
  switching providers would splice two different answers together, so a
  mid-stream failure surfaces as truncation instead.

## Quickstart

```
cp .env.example .env        # add whichever keys you have
cp config.example.yaml config.yaml
set -a; . ./.env; set +a
make build && ./bin/penstock --config config.yaml
```

`llmsim`, a deterministic mock provider, ships alongside the gateway for
development and load testing without spending quota.

## Cost control

Every key can belong to a tenant with its own limits:

```yaml
auth:
  tenants:
    - name: demo
      keys: ["${PENSTOCK_DEMO_KEY}"]
      requests_per_minute: 60
      daily_usd: 1.00
      fail_closed: true

accounting:
  ledger_path: "penstock-ledger.jsonl"
```

A request reserves an estimate before the upstream is called and settles
the real usage after, so a limit is enforced before the money is spent
rather than after. Reserving is atomic, so a tenant cannot be overspent
by requests arriving together; it can still finish slightly over, by a
bounded amount that `internal/budget` documents and a test asserts.

A rate limit answers 429 with a Retry-After. An exhausted budget answers
402 without one, because waiting does not refill it.

Each settled request appends a ledger row carrying the tenant, model,
tokens, cost and the price list version that produced it, so a figure
can be rechecked later rather than taken on faith. `GET /admin/tenants`
on the admin listener reports live balances.

Note that many entries in the shipped price table are marked
`# unverified`. The arithmetic is exact and the ledger reconciles with
the running totals, but check the rates against provider pricing pages
before trusting the absolute numbers.

## Status

Phases 0 through 3 are done: ingress and streaming, the provider
adapters, routing, and per-tenant cost control. Semantic caching and the
published benchmark campaign are next.

## License

Apache-2.0
