# Penstock and LiteLLM: measured gateway overhead

LiteLLM is the incumbent. It is the obvious thing to reach for, it has
far more features than Penstock, and any claim that Penstock is worth
using has to survive being measured next to it.

This page is that measurement. It was produced by
`bench/compare/run.sh` on Windows and by `bench/compare/linux/run.sh`
on Linux, each on one machine, against the same upstream, with both
gateways configured from their own documentation. The raw per sample
output and the hardware stanza are committed beside it in
`bench/results/`.

Read `bench/README.md` first. It is the methodology this page obeys,
including why gateway cost is only ever reported as a difference and
why a latency number quoted without its machine is not a result.

**The short version.** On the one workload where the two gateways are
doing comparable work, Penstock added about 0.8 ms of mean latency per
request and LiteLLM added about 13.6 ms. That measurement was taken on
Windows, where LiteLLM cannot use `uvloop` at all, so it was rerun on
Linux where it can. **On Linux LiteLLM added about 12.1 ms and Penstock
about 1.0 ms.** uvloop bought LiteLLM roughly 1.5 ms of its 13.6, which
is a real improvement and is not close to closing the gap. The Linux
run is the fairer of the two and is the one to quote. Almost everything
else on this page is about the ways those sentences could still mislead
you, including the one that nearly made this page overstate LiteLLM's
overhead by a factor of two.

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

Run `compare-20260807T001638Z`. 2400 samples per arm, 0 dropped
iterations, 0 failed requests, all checks passed, drift 4.1%.

| | direct | penstock | penstock delta | litellm | litellm delta |
|---|---|---|---|---|---|
| p50  | 15.48 ms | 16.28 ms | **+0.79 ms** | 29.33 ms | **+13.85 ms** |
| p95  | 43.22 ms | 43.27 ms | **+0.04 ms** | 59.97 ms | **+16.74 ms** |
| p99  | 67.01 ms | 67.12 ms | **+0.12 ms** | 85.37 ms | **+18.37 ms** |
| mean | 19.12 ms | 19.93 ms | **+0.81 ms** | 32.74 ms | **+13.62 ms** |

The **minimum** observed latency was 2.05 ms direct, 3.13 ms through
Penstock, and 11.85 ms through LiteLLM. LiteLLM's fastest request of
2400 was slower than the baseline's 25th percentile. That is the
signature of a fixed per request cost rather than a tail problem.

### It reproduces

An independent repeat, `compare-20260807T000605Z`, same settings, also
clean (0 dropped, 0 failed, drift 4.3%):

| delta | published run | repeat run |
|---|---|---|
| penstock mean | +0.81 ms | +0.86 ms |
| penstock p50  | +0.79 ms | +0.92 ms |
| litellm mean  | +13.62 ms | +12.59 ms |
| litellm p50   | +13.85 ms | +12.63 ms |

Two other runs were made and are discussed under "Runs that were
thrown away", because discarding runs silently is how benchmarks lie.

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

Arms 1 and 4 measure the identical thing: the baseline, twice, with the
gateway arms in between. Any difference between them is pure
measurement noise. That is the floor below which nothing on this page
means anything:

| | arm 1 direct | arm 4 direct | null delta |
|---|---|---|---|
| p50  | 15.48 ms | 16.12 ms | 0.64 ms |
| p95  | 43.22 ms | 44.19 ms | 0.96 ms |
| p99  | 67.01 ms | 67.98 ms | 0.97 ms |
| mean | 19.12 ms | 19.42 ms | 0.30 ms |

Put the measured deltas against that floor:

| statistic | noise floor | penstock delta | vs floor | litellm delta | vs floor |
|---|---|---|---|---|---|
| mean | 0.30 ms | +0.81 ms | 2.7x | +13.62 ms | 45x |
| p50  | 0.64 ms | +0.79 ms | 1.2x | +13.85 ms | 22x |
| p95  | 0.96 ms | +0.04 ms | **below floor** | +16.74 ms | 17x |
| p99  | 0.97 ms | +0.12 ms | **below floor** | +18.37 ms | 19x |

The honest conclusions:

- **LiteLLM's overhead is unambiguous.** Every statistic is between 17x
  and 45x the noise floor. No plausible amount of measurement error
  accounts for it.
- **Penstock's overhead is barely measurable by this harness at all.**
  Its mean delta is 2.7x the floor, which is real but not comfortable.
  Its p50 delta is 1.2x the floor. **Its p95 and p99 deltas are below
  the floor entirely**, and in the repeat run its p95 delta was
  +0.70 ms against a 1.20 ms floor, also below.
- Therefore: **quote Penstock's mean overhead of about 0.8 ms and
  nothing else.** Do not quote a Penstock tail figure from this page.
  The correct statement about Penstock's tail is that it is smaller
  than this harness can resolve, which is a weaker claim than a number
  and is the only one the data supports.
- **The difference between the two gateways is solid; the ratio is
  not.** The difference in mean overhead, about 12.8 ms, is measured
  many times over the noise floor. The ratio, about 17x, has a small
  and noisy denominator. Say "more than an order of magnitude", not
  "17x".

## The Linux rerun, which is the fairer comparison

The list at the end of this page of ways the comparison could still be
unfair has one item marked as the most important: **LiteLLM cannot use
`uvloop` on Windows**, and the page said a Linux rerun was the single
most valuable thing anyone could do to it. This section is that rerun.
It is the honest correction to the table above, and the table above is
left in place so the two can be compared.

The handicap is worse than "an optional accelerator was missing".
LiteLLM 1.95.0 branches on the platform inside its own launcher:

```python
# litellm/proxy/proxy_cli.py
@staticmethod
def _get_loop_type():
    """Helper function to determine the event loop type based on platform"""
    if sys.platform in ("win32", "cygwin", "cli"):
        return None  # Let uvicorn choose the default loop on Windows
    return "uvloop"

# ... and at the call site:
loop_type = ProxyInitializationHelpers._get_loop_type()
if loop_type:
    uvicorn_args["loop"] = loop_type
```

On Linux LiteLLM asks uvicorn for a libuv backed event loop **by name**.
On Windows it asks for nothing and gets plain asyncio. The Windows run
was not measuring LiteLLM with a part left out by accident. It was
measuring a different code path inside LiteLLM, chosen by LiteLLM.

### uvloop was confirmed active, not assumed

"We installed uvloop" is a weaker claim than "LiteLLM ran on uvloop",
and publishing the second on the strength of the first would be the
same class of error this page exists to avoid. So it was proved, by
`bench/compare/linux/verify-uvloop.sh`, whose transcript is committed at
`bench/results/compare-linux-uvloop-evidence.txt`:

1. `uvloop` imports and reports **0.22.1**.
2. uvicorn 0.52.1's `LOOP_FACTORIES["auto"]` resolves to
   `uvicorn.loops.auto:auto_loop_factory`, whose source imports uvloop
   and returns `uvloop_loop_factory` when the import succeeds.
3. Building the loop the way uvicorn builds it, via
   `Config(loop="auto").get_loop_factory()`, yields a `uvloop.Loop`.
4. LiteLLM's own `_get_loop_type()` returns `"uvloop"` on this platform,
   as quoted above.
5. **The running proxy has uvloop's compiled extension mapped into its
   address space**, read from `/proc/<pid>/maps`:
   `.../site-packages/uvloop/loop.cpython-312-x86_64-linux-gnu.so`.
   This is a statement about the process that served the measured
   requests, not about a library on disk.
6. A control process that should not have uvloop mapped does not have it
   mapped, so check 5 is discriminating rather than vacuous.

Every run repeats check 5 for itself and writes the result beside its
own samples as `<run>.uvloop.txt`. In every one of the eight Linux runs,
**every worker process had uvloop mapped**, and the processes that did
not were the launcher and the supervisor, which fork workers and serve
no requests. At 4 uvicorn workers, for example, the file records 4 of 6
mapped, and the two unmapped pids are the launcher and the supervisor.

One precision, so the evidence is not read as saying more than it does:
uvloop being mapped in the granian run's processes is not by itself
proof that granian's own Rust runtime used it. The claim this page makes
about uvloop is about the uvicorn and gunicorn runs, which is where it
matters, and the published run is uvicorn.

### Results, beside the Windows results

Published Linux run `compare-linux-uvicorn-w1-20260807T011936Z`. 2400
samples per arm, 0 dropped iterations, 0 failed requests, all checks
passed, drift 4.7%.

| | direct | penstock | penstock delta | litellm | litellm delta |
|---|---|---|---|---|---|
| p50  | 15.68 ms | 16.67 ms | **+0.99 ms** | 28.03 ms | **+12.34 ms** |
| p95  | 44.27 ms | 44.25 ms | **-0.03 ms** | 58.36 ms | **+14.09 ms** |
| p99  | 66.72 ms | 68.41 ms | **+1.69 ms** | 79.46 ms | **+12.75 ms** |
| mean | 19.36 ms | 20.39 ms | **+1.02 ms** | 31.50 ms | **+12.14 ms** |

The two platforms side by side, deltas only, both at 1 worker so that
the platform is the only thing that changed:

| delta | Windows (no uvloop) | Linux (uvloop) | change |
|---|---|---|---|
| litellm mean  | +13.62 ms | +12.14 ms | **-1.48 ms** |
| litellm p50   | +13.85 ms | +12.34 ms | -1.51 ms |
| litellm p95   | +16.74 ms | +14.09 ms | -2.65 ms |
| litellm p99   | +18.37 ms | +12.75 ms | -5.62 ms |
| penstock mean | +0.81 ms  | +1.02 ms  | +0.21 ms |
| penstock p50  | +0.79 ms  | +0.99 ms  | +0.20 ms |

**Linux is the fairer comparison and it is the one to quote.** Every
component either gateway is designed to use is present: LiteLLM gets
uvloop, and it gets `gunicorn`, which Windows cannot run at all. Nothing
was taken away from Penstock to achieve that. The Windows table remains
above because deleting a measurement after learning it was biased, and
in which direction, is not the same as correcting it.

**How much did LiteLLM gain? About 1.5 ms of 13.6, roughly 11%.** Across
both published runs on each platform the gain is smaller still: LiteLLM
averaged +13.11 ms on Windows and +12.02 ms on Linux, about 1.1 ms or
8%. The direction the Windows page predicted was right and the magnitude
is modest. **uvloop did not close the gap.** LiteLLM's overhead remains
more than an order of magnitude above Penstock's.

Two things in that table cut against Penstock and should not be skipped:

- **Penstock got slower on Linux**, from +0.81 ms to +1.02 ms of mean
  overhead. Part of the apparent narrowing of the gap is Penstock losing
  ground, not LiteLLM gaining it. The difference between the two
  gateways' mean overhead went from 12.81 ms on Windows to 11.12 ms on
  Linux, a narrowing of 1.69 ms or 13%. Of that 1.69 ms, 1.48 ms is
  LiteLLM getting faster and 0.21 ms is Penstock getting slower.
- **The ratio moved more than the difference.** Windows put LiteLLM's
  mean overhead at about 17x Penstock's; Linux puts it at about 12x. The
  advice in the null comparison section still applies: quote "more than
  an order of magnitude", not a multiplier, because the denominator is
  small and noisy on both platforms.

### It reproduces on Linux too

An independent repeat at the same settings,
`compare-linux-uvicorn-w1-20260807T021345Z`, also clean (0 dropped, 0
failed, drift 6.5%):

| delta | published Linux run | repeat run |
|---|---|---|
| penstock mean | +1.02 ms | +1.04 ms |
| penstock p50  | +0.99 ms | +1.15 ms |
| litellm mean  | +12.14 ms | +11.90 ms |
| litellm p50   | +12.34 ms | +12.33 ms |

### The Linux noise floor, and which deltas fall below it

Same construction as before: arms 1 and 4 measure the identical thing
with the gateway arms in between, so their difference is pure
measurement noise.

| | arm 1 direct | arm 4 direct | null delta |
|---|---|---|---|
| p50  | 15.68 ms | 16.41 ms | 0.73 ms |
| p95  | 44.27 ms | 45.30 ms | 1.02 ms |
| p99  | 66.72 ms | 67.53 ms | 0.82 ms |
| mean | 19.36 ms | 19.76 ms | 0.39 ms |

The measured deltas against that floor:

| statistic | noise floor | penstock delta | vs floor | litellm delta | vs floor |
|---|---|---|---|---|---|
| mean | 0.39 ms | +1.02 ms | 2.6x | +12.14 ms | 31x |
| p50  | 0.73 ms | +0.99 ms | 1.4x | +12.34 ms | 17x |
| p95  | 1.02 ms | -0.03 ms | **below floor** | +14.09 ms | 14x |
| p99  | 0.82 ms | +1.69 ms | 2.1x | +12.75 ms | 16x |

The conclusions are the same shape as on Windows:

- **LiteLLM's overhead is unambiguous on Linux as well.** Every
  statistic is between 14x and 31x the noise floor.
- **Penstock's overhead is still barely resolvable by this harness.**
  Its mean delta is 2.6x the floor. **Its p95 delta is below the floor,
  and it was measured as slightly negative**, which is what a quantity
  smaller than the measurement error looks like and is not evidence that
  a gateway makes requests faster. In the repeat run Penstock's p95
  (+0.99 ms against a 1.62 ms floor) and p99 (+0.11 ms against a 0.29 ms
  floor) were both below the floor as well.
- Therefore, unchanged: **quote Penstock's mean overhead, now about
  1.0 ms on Linux, and nothing else.** Do not quote a Penstock tail
  figure from this page on either platform.

### How the worker count was chosen on Linux

The Windows run learned, expensively, that calibrating a competitor's
settings in isolation can invert the answer. So on Linux nothing was
calibrated in isolation and **the Windows answer was not assumed
either**: importing "1 worker is best" from Windows would have repeated
the original error with the platforms swapped, since Windows had no
gunicorn and multi-process socket sharing is exactly where the two
platforms differ most.

Instead `bench/compare/linux/sweep-workers.sh` ran the **complete four
arm comparison** at each setting, with the full process population on
the machine, the drift check on and the thresholds armed. Eight runs,
64 minutes of measured load, about 70 minutes end to end:

| server | workers | litellm mean | penstock mean | floor | drift | dropped | failed |
|---|---|---|---|---|---|---|---|
| uvicorn | 1 | **+12.14 ms** | +1.02 ms | 0.39 ms | 4.7% | 0 | 0 |
| uvicorn | 1 (repeat) | **+11.90 ms** | +1.04 ms | 0.46 ms | 6.5% | 0 | 0 |
| uvicorn | 2 | +13.94 ms | +1.14 ms | 0.45 ms | 6.3% | 0 | 0 |
| uvicorn | 4 | +11.63 ms | +0.98 ms | 0.26 ms | 4.6% | 0 | 0 |
| uvicorn | 4 (repeat) | +12.72 ms | +0.82 ms | 0.20 ms | 4.5% | 0 | 0 |
| uvicorn | 8 | +12.88 ms | +1.04 ms | 0.42 ms | 5.9% | 0 | 0 |
| gunicorn | 4 | +12.45 ms | +0.84 ms | 0.51 ms | 5.2% | 0 | 0 |
| granian | 4 | +12.25 ms | +0.83 ms | 0.28 ms | 3.2% | 0 | 0 |

**On Linux the setting barely matters.** Every configuration lands
between +11.6 ms and +13.9 ms. The spread between two repeats of the
*same* setting (uvicorn at 4 workers gave +11.63 and +12.72, a spread of
1.09 ms) is comparable to the spread across most of the settings, so
most of this table is noise rather than signal.

**uvicorn at 1 worker was published**, for two reasons that point the
same way: it has the lowest two run average (+12.02 ms, against
+12.18 ms for 4 workers, +12.25 ms for granian, +12.45 ms for gunicorn,
+12.88 ms at 8 workers and +13.94 ms at 2), and it is the same worker
count the Windows run published, so the two platform tables differ in
platform and not in configuration. Choosing 4 workers or granian instead
would move the headline by less than one millisecond and would not
change a sentence on this page.

Three findings from that sweep are worth stating on their own:

1. **The 8 worker instability was a Windows artifact.** On Windows, 8
   workers cost LiteLLM roughly double (+24.60 ms against +13.62 ms) and
   produced seven requests that hung to the client's 60 second timeout.
   On Linux, 8 workers cost **+12.88 ms**, about 6% worse than 1 worker,
   with **zero hangs, zero failed requests and zero dropped iterations**.
   The Windows page's guess that this was a multi-process socket sharing
   artifact of that platform is now supported by measurement. Nothing on
   this page should be read as evidence that LiteLLM is unstable at
   higher worker counts.
2. **gunicorn does not help.** LiteLLM's CLI calls `--run_gunicorn`
   "better for managing multiple workers" and it cannot run on Windows
   at all, so it was the most likely place for a Linux-only win. Given
   it, at 4 workers, LiteLLM measured **+12.45 ms**, against uvicorn's
   two run average of +12.18 ms at the same worker count. That is a
   difference well inside the run to run spread, so the honest reading
   is that gunicorn changes nothing rather than that it lost. It was
   offered and measured, which is the point; had it won it would have
   been published.
3. **granian does not change the answer either**, at +12.25 ms, which
   agrees with the isolated Windows measurement that put granian and
   uvicorn within noise of one another.

### Linux versions

Read back from
`bench/results/compare-linux-uvicorn-w1-20260807T011936Z.meta.json`.

| | |
|---|---|
| CPU | Intel(R) Core(TM) Ultra 7 265H (the same physical machine) |
| Logical CPUs | 16 |
| Memory visible | 31.1 GiB (the host has 63.5 GiB) |
| OS | Ubuntu 22.04.5 LTS, kernel 6.6.114.1-microsoft-standard-WSL2 |

| | |
|---|---|
| litellm | 1.95.0 (pinned to the version Windows measured) |
| Python | CPython 3.12.13 |
| uvicorn | 0.52.1 |
| fastapi | 0.139.2 (same pin, same reason) |
| httptools | 0.8.0 |
| **uvloop** | **0.22.1, present and confirmed loaded by every worker** |
| gunicorn | 23.0.0 (measured, did not win) |
| orjson | 3.11.9 |
| pydantic | 2.13.4 |
| ASGI server | uvicorn |
| Workers | 1 (measured choice, see above) |

Every LiteLLM dependency version is identical to the Windows run except
the two Windows could not provide. That is deliberate: the two platform
tables should differ in platform, not in program.

| | |
|---|---|
| Penstock binary SHA256 prefix | `62a6ea7df971b165` |
| Penstock config | `bench/config/gateway.yaml`, unmodified |
| Go | go1.26.5, `GOOS=linux GOARCH=amd64 CGO_ENABLED=0` |
| k6 | v1.3.0 (commit/5870e99ae8), linux/amd64 |

Same k6 version and commit as the Windows run, same Go version, same
source, same unmodified Penstock bench config, same profile, same seed,
same time scale, same arrival rate, same VU pool, same arm order, same
gaps, same warmup, same drift check, same thresholds.

### What is still unequal, on Linux

Everything in the general list at the end of this page still applies,
minus the Windows specific items. What is new or changed:

1. **This is WSL2, not bare metal Linux.** It is a real Linux kernel
   with real uvloop, which is what the rerun needed, but it is a
   virtual machine on the same Windows host rather than a machine
   booted from Linux. Both gateways and the load generator all run
   inside it, so the effect largely cancels in the deltas, and the
   direct baselines agree closely across the two platforms (19.36 ms on
   Linux against 19.12 ms on Windows), which is the evidence that it is
   not distorting much. It is still not a bare metal result and should
   not be quoted as one.
2. **Penstock's own number got worse here** and that is reported rather
   than explained away. See the side by side table above.
3. **The Go binaries were cross compiled from the Windows host.** Same
   Go version and same source, but the build host differs from the run
   host. `CGO_ENABLED=0`, so the result is a static binary with no
   platform libc dependency.
4. **The memory visible to the VM is half the host's.** Neither gateway
   came close to using it at this load, but it is a difference and it is
   recorded.
5. **Some of LiteLLM's cost is still features Penstock does not have.**
   This is the fairness caveat that matters most and no operating system
   fixes it. LiteLLM's per request path still includes token counting,
   cost calculation against a pricing table, callback and guardrail hook
   dispatch, and a router abstraction built for fallbacks and load
   balancing across many providers. Most of those have no off switch
   because they are what LiteLLM *is*. **Penstock is not faster because
   it is better engineered. It is substantially faster because it does
   less.** The Linux rerun removed a platform handicap; it did not and
   could not remove this one.
6. **The fastapi pin is unchanged.** 0.139.2, because LiteLLM 1.95.0
   still cannot import on 0.140 or later. Whether a newer fastapi is
   faster is still unmeasured, because it still does not run.
7. **Arm order is still fixed** and drift on Linux ran 3.2% to 6.5%,
   slightly higher than the Windows run's 4.1%. That is comfortably
   inside the 10% threshold and it is **larger than Penstock's entire
   delta**, so Penstock's 1.0 ms remains sensitive to drift in a way
   LiteLLM's 12.1 ms is not.
8. **Two clean runs per configuration, not twenty.** Eight full runs
   were made in total and all eight are committed, including the six
   that were not published as the headline.

## The mistake this page nearly published

This is the most important section for anyone judging whether to trust
the table above.

LiteLLM's production guide says, for a single VM, to set the worker
count to the vCPU count. That would be 16 here, which would obviously
oversubscribe a machine that is also running k6, three simulators and
Penstock. So the worker count was swept with
`bench/compare/calibrate-litellm.sh`, which starts LiteLLM and one
simulator and measures each setting:

| workers | p50 | p95 | mean |
|---|---|---|---|
| 1 | 42.60 ms | 76.39 ms | 47.14 ms |
| 2 | 40.78 ms | 76.35 ms | 46.01 ms |
| 4 | 40.99 ms | 82.81 ms | 45.98 ms |
| 8 | 40.93 ms | 74.64 ms | 45.69 ms |

That sweep says worker count does not matter: 1.5 ms of spread across
an eightfold change, inside the noise. On that basis 8 was chosen,
because it won the mean and the p95.

**The sweep was wrong, and it was wrong in the direction that flattered
Penstock.** The sweep runs LiteLLM alone. The real comparison runs it
alongside three simulators, Penstock and k6 on the same 16 cores.
Measured there, in the full four arm run:

| LiteLLM workers | litellm mean overhead |
|---|---|
| 8 | **+24.60 ms** (`compare-20260806T232554Z`) |
| 1 | **+13.62 ms** (`compare-20260807T001638Z`) |

LiteLLM is roughly **twice as fast with one worker as with eight**, the
opposite of what the isolated sweep implied. Eight workers also
produced seven requests that hung to the client's 60 second timeout in
`compare-20260806T235233Z`, in a run where the direct and Penstock arms
had a maximum of 160 ms and not one request over 500 ms. One worker
produced no hangs and no failures in any run.

Had the first sweep been trusted, this page would have reported
LiteLLM's overhead as 24.6 ms instead of 13.6 ms: **an overstatement of
about 2x, entirely in Penstock's favour, produced by a calibration step
that was itself an attempt to be fair.**

Two things follow, and both are worth more than the headline number:

1. **Calibrate a competitor's settings in the environment the
   comparison actually runs in**, not in a clean room. A sweep that
   isolates the thing being tuned can invert the answer.
2. The 8 worker figure is still in the committed results. It was not
   deleted, because the record of having got this wrong is part of the
   evidence that the current number is not similarly wrong.

Whether one worker is optimal on Linux was, at the time this section
was written, untested. It has since been tested: see "How the worker
count was chosen on Linux" above. The answer is that on Linux the
worker count barely matters, every setting from 1 to 8 lands within
about 2 ms, and the 8 worker instability seen here did not reproduce.
Whether one worker is optimal on a machine **not** shared with five
other processes is still untested and should still not be assumed.

## Runs that were thrown away, and why

Four full runs were made. Two are published above. The other two were
discarded, and here is exactly why, so that "we ran it until it looked
good" can be ruled out by inspection rather than by trust.

**`compare-20260806T233800Z`: void, machine contention.** Exit code 99.
The harness's own guards failed it: 2 dropped iterations and 13.1%
drift, over the 10% threshold. Every arm degraded, not just LiteLLM
(direct mean rose from 19.7 ms to 24.5 ms, Penstock's from 21.1 ms to
45.9 ms, and one LiteLLM request took 20 seconds). The cause was
self inflicted: files were being written and scripts syntax checked on
the same machine while it ran. **A benchmark run concurrently with any
other work is not a benchmark**, and the guards caught it rather than
quietly folding it into an average.

**`compare-20260806T235233Z`: void, failed requests.** Exit code 99 on
the error rate threshold. 7 of 2400 LiteLLM requests hung to k6's 60
second timeout, dragging LiteLLM's mean to 223 ms while its p50 stayed
at a normal 40 ms. The direct and Penstock arms of the same run were
pristine. This run used 8 workers and is the evidence, described above,
that 8 workers were unstable on this platform.

Both discarded runs used the rejected 8 worker configuration. Neither
discarded run was replaced by a run that "looked better" under the same
settings: the settings were changed, for a stated reason, and both runs
under the new settings are reported.

## Versions

This section describes the **Windows** run. The Linux run's versions and
hardware are in "Linux versions" above, and the two are deliberately
identical except for the components Windows cannot provide.

Read back from `bench/results/compare-20260807T001638Z.meta.json`.

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
| Binary SHA256 prefix | `de9c4198ade12960` |
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
| Workers | 1 (measured choice, see above) |

### Load generator

k6 v1.3.0. Upstream `llmsim` at `--seed 1 --time-scale 0.05` replaying
`bench/profiles/groq.json`.

## How LiteLLM was configured, and why that is the point

A comparison that misconfigures the other side is worth less than no
comparison. The exact config is `bench/compare/litellm.config.yaml`,
the exact launch command is `bench/compare/start-litellm.sh`, and the
install transcript is `bench/compare/INSTALL.md`. All three are
committed so this section can be checked rather than believed.

Settings applied from LiteLLM's own production guide:

- **`LITELLM_LOG=ERROR`.** The most important one. LiteLLM 1.95.0's
  `_logging.py` reads `os.getenv("LITELLM_LOG", "DEBUG")`, so the
  **default log level of this version is DEBUG**. Benchmarking LiteLLM
  without setting this would have measured it writing debug logs for
  every request and would have been straightforwardly dishonest.
- **`LITELLM_MODE=PRODUCTION`**, which disables `load_dotenv`.
- **`set_verbose: false`** and **`json_logs: true`**.
- **`disable_spend_logs: true`**, **`disable_error_logs: true`**.
- **`request_timeout: 600`**, generous on purpose so no timeout fires
  and converts a slow request into a fast error.
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
- **A settle window after health.** `/health/liveliness` is answered by
  the first worker while others are still importing. The harness now
  waits past that, because measuring LiteLLM while it is still booting
  would be a rigged result.
- **30 warmup requests through every path** before k6 starts, so no arm
  enters the measurement colder than another.

Both gateways authenticate a bearer token against an in memory
constant, and both are sent identical request bytes. Penstock's bench
config keeps auth on deliberately, and LiteLLM is held to the same
standard rather than being run open.

### Granian was offered to LiteLLM too, and measured

LiteLLM's CLI exposes `--run_granian`, a Rust ASGI server its
production guide does not mention. "You never tried their fast server"
deserves a number rather than an excuse, so
`bench/compare/compare-servers.sh` measured it
(`compare-servers-20260807T002939Z.txt`, 1 worker, 20 req/s):

| server | p50 | p95 | p99 | mean |
|---|---|---|---|---|
| uvicorn | 29.04 ms | 59.46 ms | 119.85 ms | 32.92 ms |
| granian | 28.76 ms | 58.00 ms | 111.49 ms | 32.94 ms |

Granian is marginally ahead in the tail and identical in the mean.
Nothing here is outside the noise, so **the choice of ASGI server does
not materially change LiteLLM's overhead**, and uvicorn was kept
because it is what LiteLLM's own Docker image and production guide use.
Had granian won, it would have been used.

That measurement was taken in isolation, which is the same setup that
produced the misleading worker sweep above, so it carries the same
caveat. One thing does corroborate it: at 1 worker the isolated LiteLLM
mean (32.92 ms) and the full four arm run's LiteLLM mean (32.74 ms)
agree closely, which is what you would expect if the 1 worker
configuration is not contention sensitive, and is precisely what was
*not* true at 8 workers.

`gunicorn`, which LiteLLM's own CLI calls "better for managing multiple
workers", cannot run on Windows at all because it needs `fcntl`.

## Why llmsim and not the local llama.cpp server

A real llama.cpp server was available on this machine and was
deliberately **not** used as the upstream for the headline numbers.

1. **Queueing would swamp the signal and destroy attribution.** The
   local server runs with `--parallel 4`, so it has four decode slots.
   Above roughly four concurrent requests, additional requests wait in
   its scheduler. That wait is hundreds of milliseconds against a
   gateway cost of one to fourteen, and it is **nonlinear in arrival
   timing**: a gateway that adds a millisecond can push a request into
   a different batch and change its latency by an arbitrary amount. The
   measured "delta" would then be partly a property of the backend's
   scheduler rather than of the gateway. This gets worse in a three arm
   comparison, because the slower gateway shifts arrival timing more
   and the queue amplifies that difference.

2. **A real model is stateful across requests.** KV cache reuse and
   batch composition mean request *i* is not served identically in two
   different arms. llmsim's seeded, per index determinism is exactly
   the property that makes the three arms paired, and it is the
   property a real backend cannot offer.

3. **Inference jitter is variance the delta does not need.** The
   quantity being measured is a difference. Upstream realism adds
   nothing to a difference and costs resolution.

4. **`TIME_SCALE` only exists with a simulator.** At 0.05 the simulated
   upstream is roughly 19 ms, so a sub-millisecond gateway cost is
   near the edge of resolvable. At real speed it would sit under
   seconds of token generation and be invisible.

This choice cuts against Penstock in one specific way that should be
said plainly: a simulator is a *cheap* upstream, and a cheap upstream
makes any gateway's fixed cost a larger fraction of the total. Against
a real provider taking two seconds, both overheads shrink to a smaller
share of end to end latency. LiteLLM's 13.6 ms is under 1% of a 2
second request. **Whether 13.6 ms matters is a question about your
workload, not about these measurements.**

## Every way this comparison could still be unfair

Read this section before quoting anything above.

1. **One machine, shared cores.** k6, three simulators, Penstock and
   LiteLLM all competed for the same 16 cores. This is the largest
   systematic error in the harness, and the worker count episode above
   shows it is not theoretical: contention changed LiteLLM's answer by
   a factor of two.

2. **Windows, not Linux, and this specifically costs LiteLLM. RESOLVED:
   the rerun was done, see "The Linux rerun" above.** This was the most
   important item in this list, and it is kept here with its original
   reasoning so the prediction can be checked against the result.
   - **`uvloop` does not exist on Windows.** It replaces asyncio's
     event loop with a libuv backed one and is a material speedup for
     asyncio servers. On Linux, `uvicorn[standard]` installs it and
     LiteLLM would use it. LiteLLM ran here without it. Penstock has no
     equivalent component that Windows withholds, so this asymmetry is
     entirely LiteLLM's loss.
   - **`gunicorn` cannot run on Windows** (it needs `fcntl`).
   - The 8 worker instability described above may well be a Windows
     multi-process socket sharing artifact rather than anything
     inherent to LiteLLM.
   - Go's runtime and network stack are comparatively well optimised on
     Windows, so the platform plausibly penalises the Python gateway
     more than the Go one.

   **A Linux rerun is the single most valuable thing anyone could do to
   this page**, and the LiteLLM number should be expected to improve.

   **What the rerun found.** The direction was right and the size was
   modest. With uvloop confirmed active, LiteLLM's mean overhead fell
   from 13.62 ms to 12.14 ms, about 11%. gunicorn was given to LiteLLM
   and did not help. The 8 worker instability did not reproduce, which
   supports the guess above that it was a Windows artifact. Penstock's
   own overhead rose from 0.81 ms to 1.02 ms, so the gap between the
   two gateways narrowed by 13% rather than by the 11% LiteLLM gained.
   **Quote the Linux numbers.** This item is no longer a reason to
   distrust the conclusion, and it is still a reason to distrust the
   Windows table specifically.

3. **A pinned fastapi.** `litellm[proxy]` declares
   `fastapi>=0.136.3,<1.0`, but LiteLLM 1.95.0 imports
   `get_flat_dependant`, which fastapi removed in 0.140, so a plain
   install cannot import at all. fastapi is pinned to 0.139.2, the
   newest release LiteLLM 1.95.0 can run on. Whether 0.139.2 is faster
   or slower than 0.141.x was not measured, because 0.141.x does not
   run.

4. **One backend, one workload, one model.** A single non streaming
   chat completion, one model, one provider, no fallbacks, no retries,
   no streaming, no tool calls, no multi provider routing. The
   narrowest possible slice of what either gateway does, chosen because
   it is the only slice where the two are doing comparable work.

5. **Arm order is fixed.** Baseline, then Penstock, then LiteLLM. The
   arms were not interleaved or randomised, so a monotonic machine
   trend lands unevenly on them. The drift check bounds this at 4.1%,
   which is small relative to LiteLLM's delta but is **larger than
   Penstock's entire delta**. Penstock's 0.8 ms is therefore sensitive
   to drift in a way LiteLLM's 13.6 ms is not.

6. **Some of LiteLLM's cost is features Penstock does not have.** The
   fairness caveat that matters most for interpretation. LiteLLM's per
   request path includes token counting, cost calculation against a
   pricing table, callback and guardrail hook dispatch, and a router
   abstraction built for fallbacks and load balancing across many
   providers. Those were not switched off, because for most of them
   there is no switch: they are what LiteLLM *is*. **Penstock is not
   faster because it is better engineered. It is substantially faster
   because it does less.** A reader choosing between them is trading
   features for latency, and this page measures only one side of that
   trade.

7. **A thin profile.** `bench/profiles/groq.json` was fitted from
   `samples: 6` recorded completions. Its p95 is an estimate from very
   few observations. This affects all three arms identically and so
   mostly cancels in the deltas, but the absolute p95 and p99 columns
   are softer than they look.

8. **`TIME_SCALE` is 0.05.** Simulated time is compressed so the
   gateways' own cost is not buried under token generation. The
   gateways are therefore exercised at a concurrency far below what the
   same arrival rate produces at real speed. A gateway whose cost is
   dominated by per connection state would look better here than it
   deserves.

9. **Low concurrency generally.** At 20 requests per second against a
   19 ms upstream, well under one request is in flight on average. This
   measures per request overhead, which is what it claims to measure,
   but it says nothing about saturation, where LiteLLM's process model
   and the GIL matter most. Not measured.

10. **Penstock's result is near the noise floor.** Quantified above.
    This is a limitation of the harness, and it means Penstock's
    advantage is established as "at least an order of magnitude" rather
    than as a precise figure.

11. **k6 is a client too.** Its scheduling, connection reuse and JSON
    encoding are inside every measurement. Inside all arms, so they
    largely cancel, but they set the noise floor quantified above.

12. **Two clean runs, not twenty.** The deltas reproduce to within
    about 1 ms, which is enough to rule out a fluke and not enough to
    put a confidence interval on anything.

13. **This was written by Penstock's authors.** The apparatus, the
    settings, the upstream and the workload were all chosen by the side
    that benefits from the result. Every input is committed and the
    discarded runs are described, which is the only real answer to this
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
  about 13.6 ms of mean latency per request in this configuration on
  this platform. Against a real provider taking one to three seconds
  that is well under 1% of end to end latency, which for many
  deployments is irrelevant. It matters if you are serving a fast
  model, chaining many calls, or paying for the gateway's CPU at scale.

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

- **It does not show that LiteLLM is unstable.** The 60 second hangs
  appeared only in the rejected 8 worker configuration on Windows. One
  worker was clean across every run. **On Linux, 8 workers produced no
  hangs, no failed requests and no dropped iterations**, which is direct
  evidence that the hangs were a property of that platform and that
  configuration rather than of LiteLLM. Do not cite the hangs as a
  property of LiteLLM.

- **It does not show a precise Penstock tail figure.** Penstock's p95
  and p99 deltas are below this harness's noise floor.

## Reproducing this

### The Windows run

```bash
export PATH="$HOME/sdk/go/bin:$HOME/sdk/bin:$PATH"   # go and k6

# LiteLLM lives in a user scoped venv; no admin needed.
# See bench/compare/INSTALL.md if it is not built yet.
DURATION=120s RATE=20 bash bench/compare/run.sh

# The worker count sweep, and the ASGI server comparison
bash bench/compare/calibrate-litellm.sh
bash bench/compare/compare-servers.sh
```

### The Linux run, which is the one to quote

Cross compile the two Go binaries first, from any host with Go:

```bash
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
  go build -o bench/compare/linux/bin/penstock ./cmd/penstock
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
  go build -o bench/compare/linux/bin/llmsim   ./cmd/llmsim
```

Then, on the Linux side (this run used WSL2 Ubuntu 22.04; `/mnt/c` is
slow enough to show up in a millisecond scale measurement, so `setup.sh`
copies everything it needs into the Linux filesystem):

```bash
bash bench/compare/linux/setup.sh          # k6, the venv, the tree
bash bench/compare/linux/verify-uvloop.sh  # prove uvloop is actually used
bash bench/compare/linux/run.sh            # one four arm run

# The worker and ASGI server sweep, each point a FULL four arm run
CONFIGS="uvicorn:1 uvicorn:2 uvicorn:4 uvicorn:8 gunicorn:4 granian:4" \
  bash bench/compare/linux/sweep-workers.sh

bash bench/compare/linux/sweep-table.sh    # one line per run
python3 bench/compare/linux/report.py "bench/results/compare-linux-*.summary.json"
bash bench/compare/linux/collect.sh        # copy artifacts into the repo
```

A result is `<run>.raw.json.gz` plus `<run>.meta.json`. Neither is
worth anything alone, and neither is the table on this page without
both. The Linux runs add `<run>.uvloop.txt`, which is what turns "we
installed uvloop" into "this process ran on uvloop".
