#!/usr/bin/env bash
#
# start-litellm.sh - the exact command the comparison benchmark uses to
# launch LiteLLM, in one file so a reader can run it by hand and check
# that LiteLLM was not hobbled.
#
# It runs LiteLLM in the FOREGROUND. bench/compare/run.sh backgrounds it
# and owns the pid. Run it directly to inspect the proxy yourself:
#
#   bash bench/compare/start-litellm.sh
#   curl -s localhost:8081/health/liveliness
#
# WHY EVERY SETTING BELOW IS HERE
#
# The claim this benchmark makes is only worth something if LiteLLM was
# configured the way its own documentation asks for. Each block cites
# what it is following. The two Windows workarounds are marked as such
# and neither of them changes LiteLLM's behaviour under load.
#
# Anything that would have made LiteLLM look worse and was left in is
# noted too, because a steelman that quietly drops the inconvenient
# parts is not a steelman.

set -euo pipefail

COMPARE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$COMPARE_DIR/../.." && pwd)"
cd "$REPO_ROOT"

: "${LITELLM_VENV:=$HOME/sdk/litellm-venv}"
: "${LITELLM_HOST:=127.0.0.1}"
: "${LITELLM_PORT:=8081}"
: "${LITELLM_CONFIG:=bench/compare/litellm.config.yaml}"

# Worker processes. LiteLLM's production guide says, for a single VM,
# to set the worker count to the vCPU count; for Kubernetes it says one
# worker per pod and scale horizontally.
#
# Neither instruction fits this machine cleanly, because the load
# generator, three simulators, Penstock and LiteLLM all share 16 cores.
# Setting 16 workers here would oversubscribe the box and make LiteLLM
# look WORSE, not better. So the value is not guessed: it is chosen by
# bench/compare/calibrate-litellm.sh, which sweeps worker counts under
# the benchmark's own load and reports which one LiteLLM is fastest at.
# The sweep output is committed beside the results. Override freely.
#
# The isolated sweep on this machine (16 logical cores, 20 req/s, the
# committed compare-calibration-*.txt) was:
#
#   workers=1  p50=42.60  p95=76.39  mean=47.14
#   workers=2  p50=40.78  p95=76.35  mean=46.01
#   workers=4  p50=40.99  p95=82.81  mean=45.98
#   workers=8  p50=40.93  p95=74.64  mean=45.69
#
# That sweep says worker count does not matter: about 1.5 ms of spread
# across an eightfold change, which is inside the harness noise floor.
#
# THAT SWEEP WAS MISLEADING, AND THE FULL RUN PROVED IT.
#
# The sweep starts LiteLLM and one simulator and nothing else. The real
# comparison also has three simulators, Penstock and k6 resident on the
# same 16 cores. Measured in that setting, in the full four arm run:
#
#   workers=8  litellm mean overhead = +24.60 ms   (compare-20260806T232554Z)
#   workers=1  litellm mean overhead = +12.59 ms   (compare-20260807T000605Z)
#
# LiteLLM is roughly twice as fast with ONE worker as with eight, the
# opposite of what the isolated sweep implied. Eight worker processes
# also produced seven requests that hung to the client's 60 second
# timeout in compare-20260806T235233Z, while the direct and Penstock
# arms of that same run had a maximum of 160 ms and no request over
# 500 ms. One worker produced no hangs in any run.
#
# So the default is 1. Publishing the eight worker number would have
# overstated LiteLLM's overhead by about a factor of two, and the only
# reason it was caught is that the worker count was swept again in the
# real configuration rather than trusted from the isolated one.
#
# The lesson generalises: calibrate a competitor's settings in the
# environment the comparison actually runs in, not in a clean room.
: "${LITELLM_NUM_WORKERS:=1}"

# ---------------------------------------------------------------------
# Environment
# ---------------------------------------------------------------------

# Production guide, "Environment Variables". litellm/proxy/proxy_cli.py
# defaults LITELLM_MODE to "DEV"; PRODUCTION disables load_dotenv.
export LITELLM_MODE="PRODUCTION"

# Production guide, "Logging Configuration". This is the single most
# important setting in this file. litellm/_logging.py in 1.95.0 reads
#     log_level = os.getenv("LITELLM_LOG", "DEBUG")
# so the DEFAULT log level of this version is DEBUG. Benchmarking
# LiteLLM without setting this would measure it writing debug logs for
# every request and would be a straightforwardly dishonest result.
export LITELLM_LOG="ERROR"

# litellm/__init__.py has `telemetry = True`. The config file turns it
# off too; this is the belt to that config file's braces, because a
# background HTTP call to a third party has nothing to do with proxying
# and could only add noise to a latency measurement.
export LITELLM_DONT_SHOW_FEEDBACK_BOX="True"

# litellm/__init__.py fetches its model pricing table from
# raw.githubusercontent.com at import time unless this is set. That is a
# network round trip in startup, it makes cold start non deterministic,
# and it made the first request through a freshly started proxy take
# tens of seconds on this machine. The package ships the same table
# locally. Using it is faster and reproducible, and it costs LiteLLM
# nothing that this benchmark measures.
export LITELLM_LOCAL_MODEL_COST_MAP="True"

# Windows workaround, not a performance setting. LiteLLM prints a
# startup banner containing characters that cp1252 cannot encode, and
# Python on Windows defaults stdout to cp1252, so the proxy died during
# ASGI startup with UnicodeEncodeError before serving anything. Forcing
# UTF-8 is the standard fix and has no effect on request handling.
export PYTHONIOENCODING="utf-8"
export PYTHONUTF8="1"

# ---------------------------------------------------------------------
# Launch
#
# Flags, and why:
#
#   --num_workers      per the production guide, see the note above.
#
#   --telemetry False  same reason as LITELLM_DONT_SHOW_FEEDBACK_BOX.
#
#   --config           the model list and settings, all of which are
#                      documented in bench/compare/litellm.config.yaml.
#
# Flags deliberately NOT passed:
#
#   --detailed_debug, --debug   would be self sabotage.
#
#   --run_gunicorn     gunicorn does not run on Windows; it needs fcntl.
#                      The production guide's own wording prefers it for
#                      "managing multiple workers", so this is a real
#                      Windows handicap for LiteLLM and it is listed as
#                      a caveat in docs/comparison.md rather than hidden.
#
#   --max_requests_before_restart   the production guide recommends
#                      10000 for memory recycling. Omitted because a
#                      worker restart mid run could only hurt LiteLLM,
#                      and at this benchmark's request volume the
#                      counter would never reach 10000 anyway. Omitting
#                      it is the LiteLLM-favourable choice.
#
#   --limit_concurrency  uvicorn returns 503 past this limit. Left
#                      unset, so LiteLLM never sheds a request and is
#                      never credited with a cheap 503 in place of a
#                      real completion.
# ---------------------------------------------------------------------

# LITELLM_SERVER selects the ASGI server. "uvicorn" is the default and
# is what LiteLLM's own Docker image and production guide use, so it is
# what the published run uses.
#
# "granian" selects the Rust ASGI server LiteLLM exposes through
# --run_granian. It is not mentioned in the production guide, but it is
# offered by the CLI and is plausibly faster, so it was measured rather
# than dismissed. bench/compare/compare-servers.sh runs both and the
# result is recorded in docs/comparison.md. If granian had won, the
# published LiteLLM number would have used it: the steelman is whatever
# LiteLLM is fastest at, not whatever is most convenient.
: "${LITELLM_SERVER:=uvicorn}"

SERVER_ARGS=""
if [ "$LITELLM_SERVER" = "granian" ]; then
  SERVER_ARGS="--run_granian"
fi

# shellcheck disable=SC2086
exec "${LITELLM_VENV}/Scripts/litellm.exe" \
  --config "$LITELLM_CONFIG" \
  --host "$LITELLM_HOST" \
  --port "$LITELLM_PORT" \
  --num_workers "$LITELLM_NUM_WORKERS" \
  --telemetry False $SERVER_ARGS
