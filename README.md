# Penstock

A single-binary LLM gateway in Go: multi-provider routing, fallback
chains, per-provider cost accounting, and benchmarks you can reproduce.

A penstock is the pressure pipe that feeds a hydro turbine. It meters,
controls, and survives high-pressure flow. Same job here.

## Security status

This build is for local use only. Do not expose it to the internet or to
an untrusted network as it stands. Configuring `auth.client_keys` turns
on bearer authentication, and the loader refuses to bind anything but
loopback without it, but there is no per-tenant rate limiting or budget
enforcement yet. Free provider tiers generally train on submitted
prompts, so production or otherwise sensitive data does not belong here.

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

## Status

Phases 0 through 2 are done: ingress and streaming, the provider
adapters, and routing. Per-tenant budgets, semantic caching, and the
published benchmark campaign are next.

## License

Apache-2.0
