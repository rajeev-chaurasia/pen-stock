# Latency profiles

A profile is what makes llmsim worth benchmarking against. It is a small
JSON file describing how a real provider actually behaved, and llmsim
replays it:

```json
{
  "name": "groq-llama-3.3-70b",
  "ttft_ms":       { "mean": 355.31, "p95": 836.82 },
  "itl_ms":        { "mean": 5.24,   "p95": 8.5 },
  "output_tokens": { "mean": 39,     "p95": 39 },
  "recorded": {
    "model": "llama-3.3-70b-versatile",
    "provider_base_url": "https://api.groq.com/openai/v1",
    "prompt": "Count slowly from one to twenty, one number per line.",
    "recorded_at": "2026-08-06T20:09:30Z",
    "samples": 6,
    "chunk_samples": 228
  }
}
```

Three distributions: time to first token, inter token latency, and
completion length. Each is given as a mean and a 95th percentile, and
`internal/llmsim/profile.go` solves for the lognormal that reproduces
both, so the tail is shaped like the traffic it came from rather than
being a fixed sleep with jitter bolted on.

The `recorded` block is provenance. `LoadProfile` ignores it, because
`encoding/json` skips fields the struct does not declare, so it costs
nothing at load time and answers the only questions that matter about a
profile: which model, from which endpoint, when, and over how many
samples.

## Read the sample count before trusting the tail

`samples` is how many completions the profile was fitted from, and it is
the first thing to check. A p95 estimated from single digit samples is
barely an estimate: the mean will be roughly right and the tail is a
guess with a confident number printed next to it.

The profiles committed here today were recorded from a handful of
completions each. That is enough to make llmsim behave far more like a
real provider than a fixed sleep does, and it is not enough to defend a
p95. Re-record with more samples before quoting a tail figure from a run
against them.

`output_tokens` with an identical mean and p95, as above, means every
recorded completion returned the same length, which is what a fixed
counting prompt produces. llmsim will then emit exactly that many tokens
every time. Real traffic has a spread here, and a profile recorded from
one prompt does not.

## What belongs here

Profiles produced by the calibration recorder from real traffic against
a real provider. `bench/run.sh` defaults to `groq.json` and passes it to
both llmsim instances with `--profile`. Point it elsewhere with
`BENCH_PROFILE=bench/profiles/mistral.json`.

## What happens when it is absent

The run still works. llmsim falls back to `DefaultProfile`, which is an
approximation of a mid tier hosted model that was not calibrated against
anything, and `bench/run.sh` prints a loud warning and records
`"profile_status": "uncalibrated-builtin-default"` in the run's
`.meta.json`.

A gateway overhead figure from such a run is still a real measurement of
the gateway. What it is not is a measurement against a realistic
upstream, and it must not be presented as one.

## Validation

`LoadProfile` rejects a profile whose mean or p95 is not positive, or
whose p95 is below its mean. A profile that fails validation stops
llmsim at startup rather than being silently replaced by the default,
which is the right way round: a benchmark that quietly substitutes a
different upstream than the one you asked for is worse than one that
refuses to start.
