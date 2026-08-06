# Architecture decision records

One file per load bearing decision, in roughly the order the decisions
were made. Each records what was chosen, what it costs, and what was
rejected and why.

| ADR | Decision |
|---|---|
| [0001](0001-go-over-python-for-the-gateway-core.md) | Go over Python for the gateway core: a goroutine per stream, one static binary, and a race detector for the shared budget and health state. |
| [0002](0002-stream-completeness-is-provider-specific.md) | Stream completeness is provider specific (`[DONE]`, a non-empty `finishReason`, `message_stop`), and truncation is never reported as a clean end. |
| [0003](0003-metrics-on-a-separate-admin-listener.md) | Metrics and the operator API live on a separate admin listener that defaults to loopback, because per tenant spend is not caller facing data. |
| [0004](0004-provider-adapter-contract-and-conformance-suite.md) | One executable conformance suite every adapter satisfies, and adapters translate into the OpenAI shape so the ingress never learns a wire format. |
| [0005](0005-one-embedded-versioned-price-table.md) | One embedded price table with a version stamped on every ledger row, and rates that could not be confirmed marked unverified. |
| [0006](0006-fallback-only-before-the-first-byte.md) | Fallback is legal only before the first byte reaches the client, so a mid stream failure surfaces as truncation rather than splicing two answers. |
| [0007](0007-router-failure-policy.md) | The router failure policy: which classes fail, retry, or fail over, and why a bad payload must not fail over and must not open a circuit breaker. |
| [0008](0008-two-phase-budget-enforcement.md) | Two phase budget enforcement, reserving an estimate and settling the actual, with a documented and asserted overshoot bound. |
| [0009](0009-one-client-error-envelope.md) | The client error envelope is defined once in `internal/httperr`, so both listeners cannot answer with two different shapes. |
| [0010](0010-tenancy-is-part-of-the-cache-key.md) | Tenancy is structural in the cache, not a filter applied after a lookup, because the failure it prevents is a data leak. |
| [0011](0011-semantic-cache-tier-is-opt-in-with-a-0.95-floor.md) | The semantic cache tier is opt in with a 0.95 similarity floor, because measurement showed opposite meaning questions scoring higher than paraphrases. |

For the operator facing documentation, see [docs/index.md](../index.md).
