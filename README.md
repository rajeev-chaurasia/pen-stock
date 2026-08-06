# Penstock

A single-binary LLM gateway in Go: multi-provider routing, fallback chains,
semantic caching, per-tenant token budgets, and benchmarks you can reproduce.

A penstock is the pressure pipe that feeds a hydro turbine. It meters,
controls, and survives high-pressure flow. Same job here.

## Security status

This build is for local use only. Do not expose it to the internet or to an
untrusted network as it stands. There is no client authentication and no rate
limiting yet, so any request that reaches the listener is forwarded upstream
using the operator's API key and billed to that account, and /v1/models will
list the models you have configured. The local compose stack in deploy/ binds
its ports to 127.0.0.1 for this reason.

Free provider tiers generally train on submitted prompts, so production or
otherwise sensitive data does not belong in this gateway. Authentication and
rate limiting are Phase 3 work.

## Status

Under active development. Phase 0: OpenAI-compatible passthrough with SSE
streaming, one provider (Groq), observability skeleton, and llmsim, a
deterministic mock provider for honest load testing.

## Quickstart

Coming with the first tagged release. Until then:

```
make build
make test
```

## License

Apache-2.0
