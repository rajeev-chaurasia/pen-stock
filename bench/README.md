# Benchmarks you can reproduce

The project README makes that claim. This directory is what backs it,
and this file is the part that matters. The scripts are only the
apparatus.

The short version of the argument: gateway benchmarks are usually run
against a mock upstream that answers instantly, which turns the headline
number into a measurement of the mock. Penstock is benchmarked against
`llmsim`, whose latency is drawn from a profile calibrated on recorded
traffic from a real provider, and the gateway's own cost is reported as
a difference between two runs rather than as an absolute.

## What is measured

**The latency Penstock adds to a request.** Nothing else.

Every scenario runs the same load twice, once with the gateway in the
path and once without, and reports the difference:

```
arm A    k6 -----------------------> llmsim        the baseline
arm B    k6 ---> penstock ---------> llmsim        the same, plus the gateway
```

The difference between those two arms is attributable to the gateway.
Neither arm on its own is.

## What is NOT measured

**No provider's real speed.** llmsim is a simulator. The absolute
latency figures in any result here describe a profile being replayed on
a loopback interface. Quoting them as Groq's time to first token, or
anyone else's, would be a claim about a service that was never called.

**Throughput ceilings.** These scenarios hold a fixed arrival rate and
measure latency. Nothing here searches for the request rate at which
the gateway falls over. That is a different test and it is not built.

**Anything about a real network.** Everything runs on loopback on one
machine. A real deployment has a network between the client and the
gateway and another between the gateway and the provider, and both of
those dwarf the numbers here.

**Multi node behavior.** One process, one machine. Penstock keeps tenant
state in memory, so a multi node deployment behaves differently in ways
this harness cannot see.

**Features that are switched off.** The bench configs leave per tenant
budgets, the accounting ledger, and OTLP trace export disabled, which
are the shipped defaults. All three cost something. See "What would make
these numbers wrong" below.

## Why llmsim exists

A gateway benchmark needs an upstream. The three usual choices are all
bad:

- **A real provider.** Non reproducible by construction. The number
  moves with their capacity, your region, and the time of day, and
  running it enough times to be meaningful costs real money.
- **A trivial mock that answers instantly.** Reproducible and useless.
  The gateway is then measured under a concurrency pattern no real
  workload produces, because nothing is ever in flight for long.
- **A trivial mock with a fixed sleep.** Reproducible, and wrong in the
  tail, which is the part anybody cares about. Real LLM latency is
  heavily right skewed. A constant delay plus jitter gets the median
  roughly right and the p99 badly wrong.

`llmsim` is the fourth option. It replays a profile: a mean and a 95th
percentile for time to first token, inter token latency, and completion
length. `internal/llmsim/profile.go` solves for the lognormal parameters
that reproduce both statistics, so the shape of the tail comes from the
recorded traffic rather than from a guess. Every request's timings are
derived from the seed and the request's index, so the same seed replays
the same run.

The profile lives at `bench/profiles/groq.json` and is produced by the
calibration recorder from real recorded provider traffic. See
`bench/profiles/README.md` for the file format and for what happens when
it is missing, which is: the run proceeds against llmsim's built in
default, prints a loud warning, and records
`"profile_status": "uncalibrated-builtin-default"` in the run metadata.
A number from such a run is not a calibrated result and must not be
presented as one.

### Two simulators, identically seeded

`bench/run.sh` starts llmsim twice: one instance behind the gateway, one
for the direct arm, both with the same `--seed`. Because llmsim derives
each request's timings from the seed and the request index, request *i*
in the direct arm and request *i* in the gateway arm are served with the
identical planned time to first token, inter token latency and token
count.

That makes the comparison paired. The upstream contributes the same
latencies to both arms instead of two independent draws from the same
distribution, so the difference between the arms carries much less of
the upstream's sampling noise. Sharing one simulator between the arms
would give that up, and running the arms concurrently against one
simulator would be worse still, because the gateway arm's traffic also
lands on the simulator and the two arms would no longer be seeing the
same upstream load.

## Hardware, and why a number without it is meaningless

Every run writes `bench/results/<run>.meta.json` before k6 starts. It
records the CPU model, logical core count, memory, operating system, Go
and k6 versions, the seed and time scale, which profile was in use and
whether it was calibrated, and every load knob.

This is not bookkeeping. The numbers this harness produces are on the
order of the cost of a few syscalls and a couple of allocations, and
that cost varies by more than a factor of two across machines that are
all called "a laptop". A p95 gateway overhead quoted without the machine
it was taken on cannot be compared to anything, cannot be reproduced,
and cannot be falsified. It is not a result.

Worse, all three processes in this harness (the load generator, the
gateway, and the simulated upstream) share one machine and compete for
the same cores. That is recorded in the stanza too, because it means a
core starved machine inflates the gateway arm more than the direct arm
and biases the answer.

**Do not quote a number from this directory without the `.meta.json`
next to it.**

## How to reproduce

```
export PATH="$HOME/sdk/go/bin:$HOME/sdk/bin:$PATH"   # go and k6

make bench                       # the flagship: gateway overhead
make bench BENCH_SCENARIO=streaming_ttft
make bench BENCH_SCENARIO=cache_hit
make bench BENCH_SCENARIO=soak

make bench-all                   # all four, soak included
make bench-clean                 # clear bench/results
```

Or call the script directly, which is all the Makefile does:

```
bash bench/run.sh gateway_overhead
bash bench/run.sh --list

DURATION=2m RATE=50 bash bench/run.sh gateway_overhead
SOAK_DURATION=4h SOAK_VUS=3 bash bench/run.sh soak
TIME_SCALE=0.05 bash bench/run.sh streaming_ttft
BENCH_PROFILE=bench/profiles/mistral.json bash bench/run.sh gateway_overhead
```

Nothing has to be started by hand. `run.sh` builds both binaries, starts
two llmsim instances and the gateway, waits for all three health
endpoints, sends one real completion through each path to prove they
work, runs k6, and tears everything down on the way out even if the run
fails.

### Why three knobs are spelled BENCH_

`BENCH_MODEL`, `BENCH_API_KEY` and `BENCH_PROFILE` are read under
prefixed names because the obvious names collide with variables real
machines already export.

That is not hypothetical. The first end to end run of this harness was
made on a Windows laptop whose vendor exports `MODEL=MA16250`. `run.sh`
inherited it, the gateway was asked to route a model it does not serve,
every request in the gateway arm came back 404, and because a 404 is
cheaper than a completion the gateway arm finished twenty times
*faster* than the baseline. The harness reported a negative overhead.

Its own guards caught it (the error rate threshold failed and the run
exited non zero), but a benchmark that needs its guards to catch its
own configuration is one bad day from publishing nonsense. So those
three values, the ones that can silently turn a run into a measurement
of an error path, no longer inherit from the environment, and `run.sh`
now sends one real completion through each path before k6 starts.

The k6 scripts still read plain `BASE_URL`, `API_KEY`, `MODEL` and the
rest, which is what you want when driving k6 directly. `run.sh` passes
them with `--env`, which overrides anything the system environment
carries.

### Requirements

- **k6** on PATH. `run.sh` refuses to start without it and prints the
  install command.
- **Go 1.24 or newer** on PATH, to build the binaries.
- **curl**, which is how health is polled. Git Bash ships it.
- Ports 8080, 8089, 8090 and 9090 free. They are fixed in
  `bench/config/*.yaml`; changing them means changing both the config
  and the matching environment variable.

### Shell

Written for Git Bash on Windows, which is this repository's shell, and
kept working on Linux and macOS. The Windows specific parts are marked
"Git Bash:" in `run.sh`. The differences that matter: binaries take a
`.exe` suffix; arguments passed to the Go binaries and to k6 are always
relative paths, because MSYS rewrites absolute POSIX paths on the way
into a native Windows program; a Windows program flag like `/FI` has to
be written `//FI`; and resident memory sampling goes through `tasklist`
rather than `ps`.

## The scenarios

| Scenario | Executor | Why that executor |
|---|---|---|
| `gateway_overhead.js` | `constant-arrival-rate` | Open model. A closed model sends its next request only after the last one returns, so the slower arm would be handed less work precisely because it is slower, hiding the difference being measured. |
| `streaming_ttft.js` | `constant-arrival-rate` | Same reasoning. Streamed requests hold a connection for the whole generation, so the VU pool has to cover the arrival rate multiplied by the mean stream duration. |
| `cache_hit.js` | `constant-arrival-rate` | Same reasoning, and it matters most here: a hit is orders of magnitude cheaper than a miss, so under a closed model the two arms would run at wildly different concurrency. |
| `soak.js` | `constant-vus` | The one place a closed model is right. A soak holds one thing steady for hours rather than comparing two things. Under an open model, a gateway that slowly degraded would build an unbounded backlog and the run would end up measuring k6's queue. |

Every script takes `BASE_URL`, `DIRECT_URL`, `API_KEY`, `MODEL`,
`DURATION`, `RATE`, `PRE_ALLOCATED_VUS` and `MAX_VUS` from the
environment, with defaults matching the loopback stack `run.sh` brings
up. The per scenario knobs are documented in each file's header.

### On percentiles and subtraction

The overhead scenario prints a delta at p50, p95 and p99, and prints
this caveat next to them on every run:

**The mean delta is exactly the mean per request overhead.** Means
subtract, so `E[gateway] - E[direct] = E[overhead]` even though no
single request was ever measured both ways.

**The percentile deltas are differences of percentiles, not percentiles
of the difference.** Quantiles do not subtract. The p95 row says how far
the gateway moved the 95th percentile of end to end latency, which is a
fair statement about the tail of the system. It does not say that 95% of
requests paid less than that in gateway cost. Nothing in this harness
can say that, because measuring one request both with and without the
gateway simultaneously is not possible.

### On time to first token

Stock k6 has no incremental reader for an HTTP response body:
`http.post` returns once the whole response has been consumed, so a
script cannot be woken at the instant the first `data:` frame lands.
`streaming_ttft.js` therefore measures `res.timings.waiting`, k6's time
to first response byte.

For these two servers that is the same instant as the first `data:`
frame, and that is a property of their code rather than an assumption.
llmsim sets its SSE headers, sleeps the simulated TTFT, then writes and
flushes the first event, and Go does not put headers on the wire until
that flush. The gateway calls `ChatStream` first and writes its own 200
only once the upstream's response headers have arrived, so it does not
answer early either. Every iteration parses the buffered body and
asserts that the first frame really is a `data:` frame and that the
stream reached `[DONE]`, so a change that broke this fails the run's
checks instead of quietly reporting a number about something else.

The residual bias is named rather than hidden: the gateway flushes its
headers on receiving the upstream's headers, one `Recv` and one write
before it emits its own first `data:` frame, so the gateway arm slightly
understates true time to first frame and the reported overhead is
correspondingly optimistic.

### On the cache

`cache_hit.js` runs against `bench/config/gateway-cache.yaml`, which
enables the exact tier and leaves the semantic tier off. That is the
shipped default and it is deliberate. Turning the semantic tier on would
raise the hit rate the scenario prints, and the reason it stays off is
in `docs/semantic-caching.md`: measured cosine similarity ranks
opposite meaning questions **above** genuine paraphrases, so some of
those extra hits answer a question nobody asked. A hit rate published
without its false hit rate beside it is not a result.

The scenario asserts the inverse too. Its miss arm sends questions that
have never been asked, and a cache hit there is a correctness alarm, not
a fast result: the run fails its threshold and the summary says not to
publish it.

## What would make these numbers wrong or misleading

Read this list before quoting anything.

1. **The profile is missing, thin, or stale.** With no
   `bench/profiles/groq.json` the simulated upstream is an invented
   default, and the run says so in its metadata. With a profile
   calibrated a year ago against a model that has since been retired,
   the run looks calibrated and is not.

   More likely in practice: the profile is real but was fitted from very
   few completions. Its `recorded.samples` field says how many. A p95
   estimated from single digit samples is a guess with a confident
   number printed beside it, and llmsim will replay that guess as the
   tail of the upstream. Check the sample count before quoting any tail
   figure. See `bench/profiles/README.md`.

2. **Everything shares one machine.** The load generator, the gateway
   and the simulated upstream compete for the same cores. On a saturated
   machine the gateway arm suffers more than the direct arm, which
   inflates the reported overhead. This is the single largest systematic
   error in the harness.

3. **The machine drifts during the run.** The arms run one after the
   other, so thermal throttling, a background build, or a laptop
   switching power profiles between arms lands entirely in the delta.
   `gateway_overhead.js` repeats the direct arm afterwards and warns
   when the two disagree by more than 10%, which detects this but does
   not fix it. Run on a machine that is otherwise idle.

4. **`TIME_SCALE` is not 1.** The overhead scenario defaults to 0.05,
   compressing simulated time so the gateway's own cost is not buried
   under three seconds of simulated token generation. That is necessary
   to resolve the signal, and it means the gateway is being exercised at
   a concurrency far below what the same arrival rate produces at real
   speed. A gateway whose cost is dominated by per connection state
   would look better here than it deserves.

5. **Dropped iterations.** If k6 cannot deliver the requested arrival
   rate it drops iterations, and the two arms then received different
   workloads. This voids a comparison rather than degrading it, so it is
   a hard threshold and the summary says the delta is void.

6. **The disabled features.** Per tenant budgets, the accounting ledger
   and OTLP export are off, matching the shipped defaults. A deployment
   using tenants pays for a reservation before the upstream call and a
   settlement after; one with a ledger pays an append per settled
   request; one exporting traces pays per span. None of that is in these
   numbers. To include it, add a `tenants` block and an
   `accounting.ledger_path` to the bench config and rerun.

7. **Where the logs go.** `log_level` is left at `info`, the shipped
   default, which writes one structured line per request, and that cost
   belongs in the overhead. But `run.sh` redirects it to a file. The
   same gateway logging to a terminal is measurably slower.

8. **Percentile subtraction.** Covered above. The mean delta is exact;
   the percentile deltas describe the system's tail, not any request's
   overhead.

9. **Small samples in the tail.** A 30 second arm at 20 requests per
   second is 600 samples, so p99 rests on about six of them. Treat p99
   from a default run as an indication and raise `DURATION` before
   quoting it.

   The same noise trips the drift check: on a short arm the two direct
   measurements can differ by more than 10% purely from sampling, and
   the scenario will warn about a machine that never moved. Arms shorter
   than about 30 seconds make that warning uninformative in both
   directions.

10. **k6 is a client too.** Its own scheduling, connection reuse and
    JSON encoding are inside every measurement. They are inside both
    arms, so they largely cancel in the delta, but they set the noise
    floor below which nothing here can resolve anything.

11. **Not measuring what the soak claims.** `soak.js` compares the first
    and last quarter of a run and calls a creeping tail evidence of a
    leak. That catches leaks that degrade latency. A leak that only
    grows memory while latency stays flat is invisible to it, which is
    why `run.sh` samples resident set size separately. `internal/obs`
    registers only Penstock's own instruments on its Prometheus
    registry, so `go_memstats_*` and `go_goroutines` are not available
    on `/metrics` and cannot be watched in band.

    A short soak cannot support the comparison at all. The scenario
    needs at least 500 samples in each edge window before it will call
    a drift either way, and below that it prints UNDER POWERED rather
    than a verdict. That bar exists because the first test run of this
    scenario, over 40 seconds, confidently reported a 28% leak signal
    that was entirely sampling noise. Do not lower it to make a short
    run produce an answer.

## Results

`bench/results/` holds committed output, and `bench/results/.gitkeep`
describes the files. The important part:

**The raw per sample k6 JSON is committed on purpose.** A summary is a
set of choices about which statistics to show, made by whoever wrote the
summary. Committing every sample means a reader who distrusts those
choices, or who wants a percentile nobody printed, can recompute from
the same data instead of taking the summary on faith. Samples are tagged
with the run id, the scenario, and which arm they came from.

Server logs and the soak's memory samples are gitignored: they are
debugging aids, not evidence.

**Committed evidence has exactly one substitution applied.** The home
directory of whoever captured a run prints as `$HOME`, and
`verify-uvloop.sh` and the Linux `run.sh` now apply the same
substitution at capture time. Nothing else in these transcripts is
edited. The uvloop evidence turns on the interpreter and the mapped
`.so` sharing a prefix, which a single consistent token preserves.

**Toolchains are expected under `$HOME/sdk` by default.** That is one
author's layout, not a requirement: every path is an overridable
variable, so `LITELLM_VENV`, `WORKDIR`, `REPO` and `PATH` will move any
of it.

**A result is `<run>.raw.json` plus `<run>.meta.json`.** Neither is worth
anything alone.
