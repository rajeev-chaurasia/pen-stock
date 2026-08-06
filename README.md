# Penstock

A single-binary LLM gateway in Go: multi-provider routing, fallback chains,
semantic caching, per-tenant token budgets, and benchmarks you can reproduce.

A penstock is the pressure pipe that feeds a hydro turbine. It meters,
controls, and survives high-pressure flow. Same job here.

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
