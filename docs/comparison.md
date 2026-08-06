# Penstock and LiteLLM: measured gateway overhead

LiteLLM is the incumbent. It is the obvious thing to reach for, it has
far more features than Penstock, and any claim that Penstock is worth
using has to survive being measured next to it.

This page is that measurement. It was produced by
`bench/compare/run.sh` on one machine, in one run, against the same
upstream, with both gateways configured from their own documentation.
The raw per sample output and the hardware stanza are committed beside
it in `bench/results/`.

Read `bench/README.md` first. It is the methodology this page obeys,
including why gateway cost is only ever reported as a difference and
why a latency number quoted without its machine is not a result.

**The short version.** On the one workload where the two gateways are
doing comparable work, Penstock added about 1.5 ms of mean latency per
request and LiteLLM added about 24.6 ms. That difference is large and
it is real. Almost everything else on this page is about the ways that
sentence could still mislead you.

## The measurement

Four arms, run one after another in a single k6 process, 120 seconds
each at a constant arrival rate of 20 requests per second:

```
arm 1   k6 -----------------------> llmsim      the baseline
arm 2   k6 ---> penstock ---------> llmsim      baseline + penstock
arm 3   k6 ---> litellm ----------> llmsim      baseline + litellm
arm 4   k6 -----------------------> llmsim      the baseline again
```

Three separate `llmsim` instances on one `--seed`. llmsim derives each
request's simulated time to first token, inter token latency and token
count from the seed and the request index, so request *i* is served
with identical planned timings in every arm. The upstream therefore
contributes the same latency distribution to all three arms instead of
three independent draws from it.

Arm 4 repeats arm 1. It is the drift check, and it does more work here
than in a two arm run: see "The null comparison" below.

### Results

Run `compare-20260806T232554Z`. 2400 samples per arm, 0 dropped
iterations, 0 failed requests, 19206 of 19206 checks passed.

| | direct | penstock | penstock delta | litellm | litellm delta |
|---|---|---|---|---|---|
| p50  | 16.10 ms | 17.51 ms | **+1.40 ms** | 40.91 ms | **+24.81 ms** |
| p95  | 43.26 ms | 46.14 ms | **+2.89 ms** | 68.41 ms | **+25.16 ms** |
| p99  | 68.78 ms | 68.34 ms | **-0.43 ms** | 91.33 ms | **+22.55 ms** |
| mean | 19.67 ms | 21.13 ms | **+1.46 ms** | 44.26 ms | **+24.60 ms** |

Drift check: the direct p50 moved from 16.10 ms to 16.57 ms across the
run, a drift of 2.9%, comfortably inside the harness's 10% threshold.

One more number worth having: the **minimum** observed latency was
2.15 ms direct, 3.99 ms through Penstock, and 22.12 ms through LiteLLM.
LiteLLM's fastest request of 2401 was slower than the baseline's
median. That is the signature of a fixed per request cost rather than
a tail problem.

### How to read the delta columns

**The mean delta is exactly the mean per request overhead.** Means
subtract, so `E[gateway] - E[direct] = E[overhead]` even though no
single request was ever measured both ways.

**The percentile deltas are differences of quantiles, not quantiles of
the difference.** Quantiles do not subtract. The p95 row says how far
each gateway moved the 95th percentile of end to end latency, which is
a fair statement about the tail of the system. It does **not** say that
95% of requests paid less than that in gateway cost. Nothing in this
harness can say that, because measuring one request both with and
without a gateway at the same instant is not possible.

## The null comparison, and what this harness cannot resolve

Penstock's p99 delta is **negative**. A gateway cannot make a request
faster than not having the gateway there. That number is noise, and
rather than quietly dropping it, here is what the noise floor actually
is.

Arms 1 and 4 measure the identical thing: the baseline, twice, with the
gateway arms in between. Any difference between them is pure
measurement noise. That is the floor below which nothing on this page
means anything:

| | arm 1 direct | arm 4 direct | null delta |
|---|---|---|---|
| p50  | 16.10 ms | 16.57 ms | 0.47 ms |
| p95  | 43.26 ms | 45.11 ms | 1.85 ms |
| p99  | 68.78 ms | 67.42 ms | 1.36 ms |
| mean | 19.67 ms | 19.92 ms | 0.25 ms |

Put the measured deltas against that floor:

| statistic | noise floor | penstock delta | vs floor | litellm delta | vs floor |
|---|---|---|---|---|---|
| mean | 0.25 ms | 1.46 ms | 5.8x | 24.60 ms | 98x |
| p50  | 0.47 ms | 1.40 ms | 3.0x | 24.81 ms | 53x |
| p95  | 1.85 ms | 2.89 ms | 1.6x | 25.16 ms | 14x |
| p99  | 1.36 ms | -0.43 ms | below floor | 22.55 ms | 17x |

The honest conclusions from that table:

- **LiteLLM's overhead is unambiguous.** Every statistic is between 14x
  and 98x the noise floor. No plausible amount of measurement error
  accounts for it.
- **Penstock's mean overhead is well resolved**, at about 6x the floor.
  Quote the 1.46 ms mean.
- **Penstock's tail deltas are not well resolved.** The p95 delta is
  only 1.6x the floor and the p99 delta is below it entirely. Do not
  quote "Penstock adds 2.89 ms at p95" as though it were a measurement.
  The correct statement is that Penstock's tail cost is at or under the
  resolution of this harness.
- **The ratio between the two gateways is less certain than the
  difference.** The difference in mean overhead, about 23 ms, is solid.
  The ratio, about 17x, has a small and noisy denominator, so it should
  be treated as "more than an order of magnitude" rather than as 17.

## Versions

Everything below is read back from
`bench/results/compare-20260806T232554Z.meta.json`.

### Hardware

| | |
|---|---|
| CPU | Intel(R) Core(TM) Ultra 7 265H |
| Logical CPUs | 16 |
| Memory | 63.5 GiB |
| OS | Microsoft Windows 11 Enterprise 10.0.26200 |

k6, three llmsim instances, Penstock and LiteLLM all ran on this one
machine and competed for these cores.

### Penstock

| | |
|---|---|
| Binary SHA256 prefix | `2e724d8906563655` |
| Config | `bench/config/gateway.yaml`, unmodified |
| Go | go1.26.5 windows/amd64 |

Penstock has no version flag, so the binary is fingerprinted instead.
It runs from the same config its own flagship benchmark uses. There is
no comparison-only Penstock config.

### LiteLLM

| | |
|---|---|
| litellm | 1.95.0 (the latest release available) |
| Python | CPython 3.12.13, uv managed, user scoped |
| uvicorn | 0.52.1 |
| fastapi | 0.139.2 (pinned, see below) |
| httptools | 0.8.0 |
| uvloop | absent, unavailable on Windows |
| orjson | 3.11.9 |
| pydantic | 2.13.4 |
| Workers | 8 |

### Load generator

k6 v1.3.0. Upstream `llmsim` at `--seed 1 --time-scale 0.05` replaying
`bench/profiles/groq.json`.

## How LiteLLM was configured, and why that is the point

A comparison that misconfigures the other side is worth less than no
comparison. The exact config is
`bench/compare/litellm.config.yaml`, the exact launch command is
`bench/compare/start-litellm.sh`, and the install transcript is
`bench/compare/INSTALL.md`. All three are committed so this section can
be checked rather than believed.

Settings applied from LiteLLM's own production guide:

- **`LITELLM_LOG=ERROR`.** This is the most important one. LiteLLM
  1.95.0's `_logging.py` reads `os.getenv("LITELLM_LOG", "DEBUG")`, so
  the **default log level of this version is DEBUG**. Benchmarking
  LiteLLM without setting this would have measured it writing debug
  logs for every request, and would have been straightforwardly
  dishonest.
- **`LITELLM_MODE=PRODUCTION`**, which disables `load_dotenv`.
- **`set_verbose: false`** and **`json_logs: true`**.
- **`disable_spend_logs: true`**, **`disable_error_logs: true`**.
- **`request_timeout: 600`**, generous on purpose so that no timeout
  fires and converts a slow request into a fast error.
- **`--num_workers 8`**, chosen by measurement rather than by guess.
  See below.
- **Telemetry off** and **`LITELLM_LOCAL_MODEL_COST_MAP=True`**, which
  stops LiteLLM fetching its pricing table from GitHub at import.

Things deliberately given to LiteLLM that make it faster:

- **`httptools` installed.** Without it uvicorn falls back to its pure
  Python `h11` parser. Leaving it out would have been the easy way to
  quietly win.
- **No database and no Redis.** LiteLLM only needs a database for
  virtual keys, spend tracking and the admin UI. Requiring one would
  have put a Postgres round trip in the request path that Penstock has
  no equivalent of.
- **No `--limit_concurrency`.** LiteLLM therefore never sheds a request
  and is never credited with a cheap 503 in place of a real completion.
- **No `--max_requests_before_restart`.** The production guide suggests
  10000 for memory recycling; omitting it removes any chance of a
  worker restart landing inside the measured window.

Both gateways authenticate a bearer token against an in memory
constant, and both are sent identical request bytes. Penstock's bench
config keeps auth on deliberately, and LiteLLM is held to the same
standard rather than being run open.

### The worker count was swept, not guessed

"You gave LiteLLM the wrong worker count" is the best objection to a
comparison like this, and it cannot be answered by assertion.
`bench/compare/calibrate-litellm.sh` sweeps it under the benchmark's
own load. The committed sweep
(`bench/results/compare-calibration-20260806T231950Z.txt`):

| workers | p50 | p95 | mean |
|---|---|---|---|
| 1 | 42.60 ms | 76.39 ms | 47.14 ms |
| 2 | 40.78 ms | 76.35 ms | 46.01 ms |
| 4 | 40.99 ms | 82.81 ms | 45.98 ms |
| 8 | 40.93 ms | 74.64 ms | **45.69 ms** |

Read that honestly: the spread is about 1.5 ms across an eightfold
change in worker count, which is inside this harness's own noise. At 20
requests per second against a roughly 28 ms upstream, fewer than one
request is in flight on average, so there is very little for extra
processes to do. **Worker count is not a material variable for this
workload**, and no value in this range would have hobbled LiteLLM. 8
was chosen because it won the mean and the p95.

## Why llmsim and not the local llama.cpp server

A real llama.cpp server was available on this machine and was
deliberately **not** used as the upstream for the headline numbers.
The reasoning:

1. **Queueing would swamp the signal and destroy attribution.** The
   local server runs with `--parallel 4`, so it has four decode slots.
   Above roughly four concurrent requests, additional requests wait in
   its scheduler. That wait is hundreds of milliseconds against a
   gateway cost of one to twenty five, and it is **nonlinear in arrival
   timing**: a gateway that adds a millisecond can push a request into
   a different batch and change its latency by an arbitrary amount. The
   measured "delta" would then be partly a property of the backend's
   scheduler rather than of the gateway. This gets worse, not better,
   in a three arm comparison, because the slower gateway shifts arrival
   timing more and the queue amplifies that difference.

2. **A real model is stateful across requests.** KV cache reuse and
   batch composition mean request *i* is not served identically in two
   different arms. llmsim's seeded, per index determinism is exactly
   the property that makes the three arms paired, and it is the
   property a real backend cannot offer.

3. **Inference jitter is variance the delta does not need.** The
   quantity being measured is a difference. Upstream realism adds
   nothing to a difference and costs resolution.

4. **`TIME_SCALE` only exists with a simulator.** At 0.05 the simulated
   upstream is roughly 28 ms, so a 1.5 ms gateway cost is resolvable.
   At real speed it would sit under three seconds of token generation.

This choice cuts against Penstock in one specific way that should be
said plainly: a simulator is a *cheap* upstream, and a cheap upstream
makes any gateway's fixed cost a larger fraction of the total. Against
a real provider taking two seconds, both gateways' overheads shrink to
a smaller share of end to end latency. LiteLLM's 24.6 ms is about 1% of
a 2 second request. **Whether 24.6 ms matters is a question about your
workload, not about these measurements.** See "What this does not show".

## Every way this comparison could still be unfair

Read this section before quoting anything above.

1. **One machine, shared cores.** k6, three simulators, Penstock and
   LiteLLM all competed for the same 16 cores. This is the largest
   systematic error in the harness. A core starved machine inflates a
   gateway arm more than the baseline arm. Arms ran sequentially, which
   limits but does not eliminate it.

2. **Windows, not Linux, and this specifically costs LiteLLM.** This is
   the most important item in this list.
   - **`uvloop` does not exist on Windows.** It replaces asyncio's
     event loop with a libuv backed one and is a material speedup for
     asyncio servers. On Linux, `uvicorn[standard]` installs it and
     LiteLLM would use it. LiteLLM ran here without it. Penstock has no
     equivalent component that Windows withholds, so this asymmetry is
     entirely LiteLLM's loss.
   - **`gunicorn` cannot run on Windows** (it needs `fcntl`). LiteLLM's
     own CLI calls gunicorn "better for managing multiple workers", so
     the uvicorn supervisor used here is a second choice path.
   - Go's runtime and network stack are comparatively well optimised on
     Windows, so the platform plausibly penalises the Python gateway
     more than the Go one.

   **A Linux rerun is the single most valuable thing anyone could do to
   this page**, and the LiteLLM number should be expected to improve.

3. **A pinned fastapi.** `litellm[proxy]` declares
   `fastapi>=0.136.3,<1.0`, but LiteLLM 1.95.0 imports
   `get_flat_dependant`, which fastapi removed in 0.140, so a plain
   install cannot import at all. fastapi is pinned to 0.139.2, the
   newest release LiteLLM 1.95.0 can run on. Whether 0.139.2 is faster
   or slower than 0.141.x was not measured, because 0.141.x does not
   run.

4. **One backend, one workload, one model.** A single non streaming
   chat completion, one model, one provider, no fallbacks, no retries,
   no streaming, no tool calls, no multi provider routing. That is the
   narrowest possible slice of what either gateway does, chosen because
   it is the only slice where the two are doing comparable work.

5. **Cold start was handled, but only approximately.** Every path got
   30 warmup requests before k6 started, so no arm entered the
   measurement colder than another. That is not the same as proving all
   three had reached identical steady state. Python's lazy imports and
   connection pool growth are harder to saturate than Go's, so residual
   cold start bias, if any, is against LiteLLM.

6. **Arm order is fixed.** The baseline runs first, then Penstock, then
   LiteLLM. The arms were not interleaved or randomised, so a monotonic
   machine trend lands unevenly on them. The drift check bounds this at
   2.9%, which is smaller than the effect being measured but is not
   zero, and 2.9% of LiteLLM's arm is larger in absolute terms than
   Penstock's entire delta.

7. **Some of LiteLLM's cost is features Penstock does not have.** This
   is the fairness caveat that matters most for interpretation.
   LiteLLM's per request path includes token counting, cost
   calculation against a pricing table, callback and guardrail hook
   dispatch, and a router abstraction built for fallbacks and load
   balancing across many providers. Those were not switched off,
   because for most of them there is no switch: they are what LiteLLM
   *is*. **Penstock is not faster because it is better engineered. It
   is substantially faster because it does less.** A reader choosing
   between them is trading features for latency, and this page measures
   only one side of that trade.

8. **A thin profile.** `bench/profiles/groq.json` was fitted from
   `samples: 6` recorded completions. Its p95 is an estimate from very
   few observations, so the absolute tail figures describe that thin
   profile being replayed. This affects all three arms identically and
   so mostly cancels in the deltas, but it means the absolute p95 and
   p99 columns are softer than they look.

9. **`TIME_SCALE` is 0.05.** Simulated time is compressed so the
   gateways' own cost is not buried under token generation. The
   gateways are therefore exercised at a concurrency far below what the
   same arrival rate produces at real speed. A gateway whose cost is
   dominated by per connection state would look better here than it
   deserves.

10. **Low concurrency generally.** At 20 requests per second against a
    28 ms upstream, under one request is in flight on average. This
    measures per request overhead, which is what it claims to measure,
    but it says nothing about behaviour under saturation, where
    LiteLLM's multiple worker processes might close some of the gap and
    where the GIL might widen it. Neither was measured.

11. **Small samples in the tail.** 2400 samples per arm puts roughly 24
    observations behind each p99. That is better than the harness
    default but it is still thin, and it is visible in the negative
    Penstock p99 delta.

12. **k6 is a client too.** Its scheduling, connection reuse and JSON
    encoding are inside every measurement. They are inside all arms so
    they largely cancel, but they set the noise floor quantified above.

13. **One run of each.** See the reproducibility section below.

14. **This was written by Penstock's authors.** The apparatus, the
    settings, the upstream and the workload were all chosen by the side
    that benefits from the result. Every input is committed so the
    choices can be audited, which is the only real answer to this
    objection, but it is not the same as an independent test.

## What this comparison does NOT show

Stated explicitly, because the table above is easy to over read.

- **It does not show that Penstock is better than LiteLLM.** It shows
  that Penstock adds less latency on one narrow workload. LiteLLM has
  vastly more features: many providers, fallbacks, load balancing,
  virtual keys, budgets, spend tracking, an admin UI, guardrails, a
  large callback ecosystem. Penstock does not have most of that. Choose
  on features first; this page is only relevant once two options both
  do what you need.

- **It does not show that LiteLLM is slow.** It shows LiteLLM costs
  about 24.6 ms of mean latency per request in this configuration on
  this platform. Against a real provider taking one to three seconds
  that is roughly 1% to 2% of end to end latency, which for many
  deployments is irrelevant. It matters if you are serving a fast
  model, if you are chaining many calls, or if you are paying for the
  gateway's CPU at scale.

- **It does not show either gateway's real world latency.** llmsim is a
  simulator on loopback. The absolute columns describe a profile being
  replayed, not any provider's speed. A real deployment has a network
  between client and gateway and another between gateway and provider,
  and both dwarf everything here.

- **It does not show throughput or saturation behaviour.** The arrival
  rate was held constant. Nothing here searches for the rate at which
  either gateway falls over, and that is where a process based Python
  server and a goroutine based Go server differ most.

- **It does not show streaming behaviour.** Every request was non
  streaming. Time to first token through each gateway is a different
  measurement and was not made.

- **It does not show behaviour under failure.** No retries, no
  fallbacks, no provider errors, no rate limits. Fallback handling is
  one of LiteLLM's main reasons to exist and it is untested here.

- **It does not show memory or CPU cost.** Only latency was recorded.
  LiteLLM's production guide asks for 1 vCPU and 4 GiB per worker,
  which is a real operational difference this page does not quantify.

- **It does not show multi node behaviour.** One process each, one
  machine.

- **It does not show that Penstock's tail is 2.89 ms better at p95.**
  As the null comparison shows, Penstock's tail deltas are at or below
  this harness's resolution.

## Reproducing this

```bash
export PATH="$HOME/sdk/go/bin:$HOME/sdk/bin:$PATH"   # go and k6

# LiteLLM lives in a user scoped venv; no admin needed.
# See bench/compare/INSTALL.md if it is not built yet.
DURATION=120s RATE=20 bash bench/compare/run.sh

# The worker count sweep
bash bench/compare/calibrate-litellm.sh
```

A result is `<run>.raw.json.gz` plus `<run>.meta.json`. Neither is
worth anything alone, and neither is the table on this page without
both.
