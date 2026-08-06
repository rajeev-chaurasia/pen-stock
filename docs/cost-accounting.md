# Cost accounting

How a request turns into a dollar figure, how that figure is enforced
before the money is spent, and what is written down afterwards.

## Read this before you trust the numbers

**30 of the 38 entries in the shipped price table are marked
`# unverified`.**

They are best-effort figures that were never checked against the provider's
published pricing page. They are the right order of magnitude and they are
useful for watching *shape*: which tenant is growing, which model dominates
spend, whether a fallback chain quietly moved traffic onto a paid provider.
They are not something to bill a customer against, or to put in front of
anyone who will read the absolute number as fact.

Before anyone relies on the absolute numbers, open
`internal/pricing/pricing.yaml`, find every line carrying `# unverified`,
check it against the provider's own pricing page, and remove the marker as
you confirm each one. Bump `version` when you do.

Two further gaps in the model, independent of verification:

- **Tiering is not modelled.** Where a provider charges differently by
  context length, only the base tier is priced. Cached-input rates, batch
  discounts, and negotiated rates are all ignored.
- **List price only.** Every figure is a published list price before any
  discount you may actually be paying.

A model with no entry at all is reported as **unpriced**, never as free.
Its tokens are still counted and it still consumes token limits; it
contributes zero USD. That is a deliberate refusal to guess, and it means a
missing entry shows up as a gap in the cost dashboard rather than as a
suspiciously cheap model.

## Pricing a request

The price table is keyed by `"<provider kind>/<model>"`. The kind is
everything before the first slash and matches a provider `kind` from the
config; the model is everything after it, so aggregator ids that themselves
contain slashes work unchanged.

```yaml
version: 1
updated: "2026-08-05"

models:
  openai/gpt-4o-mini:
    input_per_mtok: 0.15
    output_per_mtok: 0.60
  openai_compat/llmsim-small:
    free: true
```

Rates are USD per million tokens. The cost of one completion is:

```
usd = (prompt_tokens * input_per_mtok + completion_tokens * output_per_mtok) / 1000000
```

The single division at the end keeps the rounding to one step.

Details that matter in practice:

- `free: true` prices at exactly zero. A free model may not also carry
  rates, and a non-free model must carry at least one, so an entry cannot
  be accidentally silent about what it costs.
- **Reasoning tokens count as completion tokens**, because that is how
  providers charge for them.
- Token counts arrive from upstreams the gateway does not control. A
  negative count is clamped to zero rather than credited back to the
  tenant.
- The table is embedded in the binary, so the gateway prices requests with
  no external file present. There is one copy of it, because two sources of
  truth for money is not a thing anyone wants to debug.
- **Cost is attributed to whichever provider actually answered.** On a
  fallback chain that is not necessarily the provider the route names
  first.

## Reserve, then settle

The hard part of budgeting an LLM call is that its true cost is unknown
until the answer arrives. Enforcement is therefore two phase.

```
                estimate            reserve            call            settle
request  --->  predict tokens  ---> claim USD  --->  upstream  --->  replace claim
                and USD             atomically                        with real cost
                                        |
                                        +-- no room? --> Denial, nothing spent
                                        |
                                        +-- call failed? --> Release, claim returned
```

**Estimate.** Before the upstream is called, the request body is read for
what it will consume. Prompt text is charged at roughly four characters per
token plus four tokens of framing per message, which is close enough on
every tokenizer the gateway fronts. Characters are counted as bytes, so
text outside ASCII counts high, which leans the estimate the safe way. A
body that will not parse is charged as raw bytes with a floor of 1024
tokens, so a stream of malformed requests cannot be free. The completion
allowance is the client's own `max_tokens` or `max_completion_tokens`
(whichever is smaller when both are sent), or a default of 1024, and then
clamped to the operator's ceiling.

An estimate never fails. Failing to estimate would mean either refusing a
request the gateway could serve, or admitting one at a cost of zero, and a
cost of zero admits the request however little budget is left.

**Reserve.** The claim is made under a single lock that reads every counter
and updates them together. That indivisibility is the entire point:
anything finer grained lets a hundred requests arriving at once each
observe the same remaining balance, each conclude there is room, and each
spend it. If a limit refuses, nothing has been spent and the caller gets a
denial that names which limit said no.

**Settle.** When the answer arrives, the reservation is dropped and the
real cost takes its place. An estimate that was too generous is refunded;
one that was too small is made good. The token-per-minute window is
reconciled against real usage too, since leaving it on the estimate would
let a tenant whose answers routinely run long sit permanently above its
cap.

**Release.** A request that never reached the upstream returns its claim
untouched. A failed call costs the tenant nothing.

Settle and Release are each idempotent per reservation. A repeat of either
is ignored rather than counted twice, so a retry in the request path cannot
double-bill.

## The overshoot bound, in plain terms

A tenant can finish slightly over its budget. The amount is bounded, not
arbitrary:

```
overshoot <= sum over in-flight requests of (actual cost - estimated cost)
```

In words: **a tenant can only exceed its budget by as much as the estimates
for the requests currently running turned out to be too low.**

Nothing is ever admitted once the balance is gone, because reserving is
atomic. But requests already in flight when the balance ran out were
admitted against a prediction, and predictions run low sometimes. If ten
requests are running and each answer comes back one cent more expensive
than predicted, the tenant can end up ten cents over. It cannot end up ten
dollars over.

Two things keep that sum small. Capping the estimate's completion allowance
bounds each individual term, so no single request can run far past what was
reserved for it. Bounding concurrency with `server.max_inflight` keeps the
number of terms finite.

This is a property with a test behind it, not an aspiration: a test drives
concurrent requests at a nearly exhausted budget and asserts the bound
holds.

The practical consequence is that `daily_remaining_usd` on the
[admin API](admin-api.md) can read negative. That is the bound being
visible, and it is not clamped, because clamping would hide the one number
that shows it happened.

## The ledger

Running totals say what a tenant has spent. The ledger says which requests
it was spent on, which is the difference between a number on a dashboard
and one an operator can check.

Set `accounting.ledger_path` to turn it on. See
[configuration.md](configuration.md).

### Format

JSON Lines: one JSON object per line, appended, never rewritten. An audit
can replay it from the beginning.

```json
{"timestamp":"2026-08-06T14:22:07.418293Z","tenant":"demo","provider_kind":"groq","model":"llama-3.3-70b-versatile","prompt_tokens":812,"completion_tokens":344,"usd":0.00029744,"price_version":1,"cache_hit":false,"request_id":"4127"}
```

| Field | Type | Meaning |
|---|---|---|
| `timestamp` | RFC3339Nano string | When the request happened, supplied by the caller rather than read from the clock in the write path, so a replayed entry keeps its real time. |
| `tenant` | string | Configured tenant name. Never a key. |
| `provider_kind` | string | Whichever provider actually answered. |
| `model` | string | The model as routed upstream. |
| `prompt_tokens` | int | Real usage, as reported by the provider. |
| `completion_tokens` | int | Real usage. Includes reasoning tokens. |
| `usd` | float | What this request was billed at. |
| `price_version` | int | The price table version that produced `usd`. |
| `cache_hit` | bool | Reserved for the caching phase. Currently always `false`. |
| `request_id` | string | The reservation id, unique per admitted request, tying the row back to its claim. |

`price_version` is why spend recorded before a price change stays
explainable after it. A row priced at version 1 can always be traced back
to the numbers that were in force, which is exactly what you need when
someone asks why last month's figure does not reproduce.

### Operational notes

- The file is created with mode `0600`. It holds per-tenant spend, which is
  nobody else's business.
- **Writes are not fsynced.** An fsync costs single-digit milliseconds,
  which on a fast completion is a large slice of the whole request, and
  paying it per request would make the ledger the gateway's latency
  ceiling. Entries land in the OS page cache and reach disk on the kernel's
  schedule. A crash of the gateway process loses nothing, since the kernel
  still owns those buffers. A kernel panic or a power cut can lose the last
  few seconds of entries. Shutdown syncs before closing.
- A failed ledger write does not fail the request. The answer is already on
  its way to the client, and losing an audit row is better than losing the
  response. The failure is logged at error level rather than swallowed, so
  a silently unwritable ledger is noticed instead of assumed empty.
- Concurrent requests are serialized on write, and each entry plus its
  newline lands in one call, so a line either appears whole or not at all.
  There are no torn rows to parse around.

### Reading it

Every line is independent, so the usual text tools work.

```
# Spend by tenant, whole file
cat cost.jsonl | jq -s 'group_by(.tenant) | map({tenant: .[0].tenant, usd: (map(.usd) | add)})'

# Every request that cost more than a cent
jq 'select(.usd > 0.01)' cost.jsonl
```

If a total here disagrees with the cost dashboard, the ledger is the one to
believe. It records what was settled; the metric records what was observed,
and a scrape interval can straddle a restart.
