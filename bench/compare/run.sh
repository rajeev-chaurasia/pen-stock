#!/usr/bin/env bash
#
# bench/compare/run.sh - measure Penstock's and LiteLLM's overhead in
# one run, against the same upstream, on the same machine.
#
# This is bench/run.sh with a second gateway bolted on, and it keeps
# that script's shape on purpose: same platform handling, same loud
# preconditions, same hardware stanza, same "nothing is started by
# hand" contract. Read bench/README.md first; it is the methodology
# this script implements and it is not repeated here.
#
# Usage:
#   bash bench/compare/run.sh
#   DURATION=60s RATE=20 bash bench/compare/run.sh
#   LITELLM_NUM_WORKERS=1 bash bench/compare/run.sh
#
# WHAT IT BRINGS UP
#
#   llmsim  :8089  ->  upstream for Penstock
#   llmsim  :8090  ->  upstream for the direct baseline
#   llmsim  :8091  ->  upstream for LiteLLM
#   penstock :8080 (admin :9090)
#   litellm  :8081
#
# Three simulators, one --seed. llmsim derives each request's simulated
# timings from the seed and the request index, so request i is served
# with identical planned TTFT, inter-token latency and token count in
# all three arms. That is what makes the two deltas comparable to each
# other rather than merely both being differences.
#
# THE PENSTOCK CONFIG IS NOT SPECIAL
#
# Penstock runs from bench/config/gateway.yaml, unmodified: the same
# file its own flagship benchmark uses. There is no comparison-only
# Penstock config, because a config that exists only to win a
# comparison is the thing this whole directory is trying not to be.
# LiteLLM's config is bench/compare/litellm.config.yaml and its launch
# command is bench/compare/start-litellm.sh; both are commented with
# what they follow from LiteLLM's own production guide.

set -euo pipefail

COMPARE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$COMPARE_DIR/../.." && pwd)"
cd "$REPO_ROOT"

RESULTS_DIR="bench/results"
SCENARIO="compare_litellm"
SCRIPT="bench/scenarios/${SCENARIO}.js"

# ---------------------------------------------------------------------
# Platform detection. Git Bash on Windows is this repository's shell.
# ---------------------------------------------------------------------

BIN_EXT=""
IS_WINDOWS=0
case "$(uname -s 2>/dev/null || echo unknown)" in
  MINGW* | MSYS* | CYGWIN*)
    BIN_EXT=".exe"
    IS_WINDOWS=1
    ;;
esac

die() {
  echo "" >&2
  echo "bench/compare/run.sh: $1" >&2
  shift
  while [ "$#" -gt 0 ]; do
    echo "  $1" >&2
    shift
  done
  echo "" >&2
  exit 1
}

# ---------------------------------------------------------------------
# Preconditions
# ---------------------------------------------------------------------

command -v k6 >/dev/null 2>&1 || die \
  "k6 is not on PATH." \
  "  export PATH=\"\$HOME/sdk/bin:\$PATH\""
command -v go >/dev/null 2>&1 || die \
  "go is not on PATH." \
  "  export PATH=\"\$HOME/sdk/go/bin:\$PATH\""
command -v curl >/dev/null 2>&1 || die \
  "curl is not on PATH, and it is how this script waits for health."

: "${LITELLM_VENV:=$HOME/sdk/litellm-venv}"
LITELLM_PY="${LITELLM_VENV}/Scripts/python${BIN_EXT}"
LITELLM_CLI="${LITELLM_VENV}/Scripts/litellm${BIN_EXT}"
if [ "$IS_WINDOWS" != "1" ]; then
  LITELLM_PY="${LITELLM_VENV}/bin/python"
  LITELLM_CLI="${LITELLM_VENV}/bin/litellm"
fi

[ -x "$LITELLM_CLI" ] || die \
  "no LiteLLM proxy found at $LITELLM_CLI" \
  "" \
  "This machine has no system Python, so LiteLLM lives in a user scoped" \
  "venv created with uv. No admin rights are needed. To build it:" \
  "" \
  "  export PATH=\"\$HOME/.local/bin:\$PATH\"" \
  "  uv venv --python 3.12 \$HOME/sdk/litellm-venv" \
  "  uv pip install --python \$HOME/sdk/litellm-venv/Scripts/python.exe \\" \
  "      'litellm[proxy]' httptools 'fastapi<0.140'" \
  "" \
  "The fastapi pin is not optional: litellm 1.95.0 declares" \
  "fastapi>=0.136.3,<1.0 but imports get_flat_dependant, which fastapi" \
  "removed in 0.140. Without the pin the proxy cannot import at all." \
  "" \
  "See bench/compare/INSTALL.md for the full transcript."

# ---------------------------------------------------------------------
# Knobs
#
# Defaults are gateway_overhead.js's defaults, deliberately. A Penstock
# number produced here should be directly comparable to the one its own
# flagship scenario produces, which it is not if the load differs.
# ---------------------------------------------------------------------

: "${TIME_SCALE:=0.05}"
: "${DURATION:=30s}"
: "${RATE:=20}"
: "${PRE_ALLOCATED_VUS:=50}"
: "${MAX_VUS:=200}"
: "${SEED:=1}"
: "${ARM_GAP:=5s}"
: "${DRIFT_CHECK:=true}"
: "${RAW_GZIP:=true}"

# Requests pushed through each of the three paths before k6 starts.
#
# This is not padding. LiteLLM is Python: its first requests pay for
# lazy imports, httpx pool construction and connection setup, and a cold
# LiteLLM measured against a warm baseline would be a rigged result in
# Penstock's favour. Penstock and the baseline get exactly the same
# number of warmup requests so no arm is warmer than another, and
# because each path has its own simulator, every simulator's request
# counter advances identically and the seeded pairing survives.
: "${WARMUP_REQUESTS:=30}"

# Prefixed for the reason bench/run.sh documents at length: the obvious
# names collide with variables real machines already export, and a
# wrong model or key turns the run into a measurement of an error path,
# which produces a plausible number instead of an error.
: "${BENCH_API_KEY:=benchbenchbenchbenchbench}"
: "${BENCH_MODEL:=llmsim-small}"
: "${BENCH_PROFILE:=bench/profiles/groq.json}"

API_KEY="$BENCH_API_KEY"
MODEL="$BENCH_MODEL"
PROFILE="$BENCH_PROFILE"

# Ports. 8089/8090/8080/9090 have to agree with bench/config/gateway.yaml.
# 8091/8081 have to agree with bench/compare/litellm.config.yaml and
# bench/compare/start-litellm.sh.
: "${LLMSIM_PORT:=8089}"
: "${DIRECT_PORT:=8090}"
: "${LITELLM_SIM_PORT:=8091}"
: "${GATEWAY_PORT:=8080}"
: "${ADMIN_PORT:=9090}"
: "${LITELLM_PORT:=8081}"
: "${LITELLM_NUM_WORKERS:=1}"
export LITELLM_NUM_WORKERS LITELLM_PORT LITELLM_VENV

BASE_URL="http://127.0.0.1:${GATEWAY_PORT}"
DIRECT_URL="http://127.0.0.1:${DIRECT_PORT}"
ADMIN_URL="http://127.0.0.1:${ADMIN_PORT}"
LITELLM_URL="http://127.0.0.1:${LITELLM_PORT}"

GATEWAY_CONFIG="bench/config/gateway.yaml"
LITELLM_CONFIG="bench/compare/litellm.config.yaml"

RUN_ID="compare-$(date -u +%Y%m%dT%H%M%SZ)"
mkdir -p "$RESULTS_DIR"

RAW="${RESULTS_DIR}/${RUN_ID}.raw.json"
[ "$RAW_GZIP" = "true" ] && RAW="${RAW}.gz"
SUMMARY_TXT="${RESULTS_DIR}/${RUN_ID}.summary.txt"
SUMMARY_JSON="${RESULTS_DIR}/${RUN_ID}.summary.json"
META="${RESULTS_DIR}/${RUN_ID}.meta.json"
LOG_SIM_PEN="${RESULTS_DIR}/${RUN_ID}.llmsim-penstock.log"
LOG_SIM_DIR="${RESULTS_DIR}/${RUN_ID}.llmsim-direct.log"
LOG_SIM_LLM="${RESULTS_DIR}/${RUN_ID}.llmsim-litellm.log"
LOG_GATEWAY="${RESULTS_DIR}/${RUN_ID}.penstock.log"
LOG_LITELLM="${RESULTS_DIR}/${RUN_ID}.litellm.log"

# ---------------------------------------------------------------------
# Process lifecycle
#
# Git Bash: LiteLLM with --num_workers > 1 is a supervisor that spawns
# child worker processes. Killing the MSYS pid leaves those children
# holding port 8081, which poisons the next run with a stale proxy that
# looks healthy. So the teardown resolves the Windows pid and kills the
# whole tree with taskkill //T. The leading double slash stops MSYS
# rewriting the flag as a path.
# ---------------------------------------------------------------------

PIDS=""
BG_PID=""

start_bg() {
  local log="$1"
  shift
  "$@" >"$log" 2>&1 &
  BG_PID=$!
  PIDS="$PIDS $BG_PID"
}

kill_tree() {
  local pid="$1" winpid
  if [ "$IS_WINDOWS" = "1" ] && command -v taskkill >/dev/null 2>&1; then
    winpid="$(ps -p "$pid" 2>/dev/null | awk -v p="$pid" '$1 == p {print $4; exit}' || true)"
    [ -n "$winpid" ] || winpid=""
    if [ -n "$winpid" ]; then
      taskkill //PID "$winpid" //T //F >/dev/null 2>&1 || true
    fi
  fi
  kill "$pid" >/dev/null 2>&1 || true
}

cleanup() {
  local code=$?
  set +e
  for pid in $PIDS; do
    kill_tree "$pid"
  done
  sleep 2
  for pid in $PIDS; do
    kill -9 "$pid" >/dev/null 2>&1
  done
  exit "$code"
}
trap cleanup EXIT INT TERM

wait_healthy() {
  # wait_healthy <name> <url> <pid> [attempts]
  local name="$1" url="$2" pid="$3" attempts="${4:-150}"
  local i=0
  while [ "$i" -lt "$attempts" ]; do
    if ! kill -0 "$pid" >/dev/null 2>&1; then
      die "$name exited before it became healthy." \
        "Its output is in the log beside this run:" \
        "  $RESULTS_DIR/${RUN_ID}.*.log"
    fi
    if curl --silent --fail --max-time 2 "$url" >/dev/null 2>&1; then
      echo "  ok   $name  ($url)"
      return 0
    fi
    sleep 0.2
    i=$((i + 1))
  done
  die "$name never answered $url after $((attempts / 5)) seconds." \
    "Check the log beside this run:" \
    "  $RESULTS_DIR/${RUN_ID}.*.log" \
    "A likely cause is a port already held by an earlier run that did not" \
    "shut down. Check with: curl -sv $url"
}

# ---------------------------------------------------------------------
# Build
# ---------------------------------------------------------------------

PENSTOCK_BIN="bin/penstock${BIN_EXT}"
LLMSIM_BIN="bin/llmsim${BIN_EXT}"

echo "building"
go build -o "$PENSTOCK_BIN" ./cmd/penstock || die "go build ./cmd/penstock failed."
go build -o "$LLMSIM_BIN" ./cmd/llmsim || die "go build ./cmd/llmsim failed."
echo "  ok   $PENSTOCK_BIN"
echo "  ok   $LLMSIM_BIN"

# ---------------------------------------------------------------------
# The latency profile
# ---------------------------------------------------------------------

PROFILE_ARGS=""
PROFILE_STATUS="calibrated"
PROFILE_USED="$PROFILE"
PROFILE_SAMPLES="unknown"
if [ -f "$PROFILE" ]; then
  PROFILE_ARGS="--profile $PROFILE"
  PROFILE_SAMPLES="$(tr -d ' \n' <"$PROFILE" | sed -n 's/.*"samples":\([0-9]*\).*/\1/p')"
  [ -n "$PROFILE_SAMPLES" ] || PROFILE_SAMPLES="unknown"
else
  PROFILE_STATUS="uncalibrated-builtin-default"
  PROFILE_USED="llmsim built in DefaultProfile"
  echo ""
  echo "======================================================================"
  echo "WARNING: $PROFILE is missing. llmsim falls back to an invented"
  echo "default. The DELTAS this run reports are still real measurements,"
  echo "because both gateways face the same upstream either way, but the"
  echo "absolute latencies are not a calibrated result."
  echo "======================================================================"
  echo ""
fi

# ---------------------------------------------------------------------
# Bring the stack up
# ---------------------------------------------------------------------

echo "starting stack"

# shellcheck disable=SC2086
start_bg "$LOG_SIM_PEN" "./$LLMSIM_BIN" \
  --listen "127.0.0.1:${LLMSIM_PORT}" --seed "$SEED" --time-scale "$TIME_SCALE" $PROFILE_ARGS
SIM_PEN_PID="$BG_PID"
# shellcheck disable=SC2086
start_bg "$LOG_SIM_DIR" "./$LLMSIM_BIN" \
  --listen "127.0.0.1:${DIRECT_PORT}" --seed "$SEED" --time-scale "$TIME_SCALE" $PROFILE_ARGS
SIM_DIR_PID="$BG_PID"
# shellcheck disable=SC2086
start_bg "$LOG_SIM_LLM" "./$LLMSIM_BIN" \
  --listen "127.0.0.1:${LITELLM_SIM_PORT}" --seed "$SEED" --time-scale "$TIME_SCALE" $PROFILE_ARGS
SIM_LLM_PID="$BG_PID"

wait_healthy "llmsim (penstock upstream)" "http://127.0.0.1:${LLMSIM_PORT}/healthz" "$SIM_PEN_PID"
wait_healthy "llmsim (direct baseline)"   "${DIRECT_URL}/healthz" "$SIM_DIR_PID"
wait_healthy "llmsim (litellm upstream)"  "http://127.0.0.1:${LITELLM_SIM_PORT}/healthz" "$SIM_LLM_PID"

start_bg "$LOG_GATEWAY" "./$PENSTOCK_BIN" --config "$GATEWAY_CONFIG"
GATEWAY_PID="$BG_PID"
wait_healthy "penstock gateway" "${BASE_URL}/healthz" "$GATEWAY_PID"
wait_healthy "penstock admin"   "${ADMIN_URL}/metrics" "$GATEWAY_PID"

# LiteLLM takes appreciably longer than Penstock to become healthy: it
# is a Python process importing a large dependency tree, and with
# several workers each one pays that cost. 300 attempts is 60 seconds.
start_bg "$LOG_LITELLM" bash bench/compare/start-litellm.sh
LITELLM_PID="$BG_PID"
wait_healthy "litellm proxy" "${LITELLM_URL}/health/liveliness" "$LITELLM_PID" 300

# /health/liveliness is answered as soon as the FIRST uvicorn worker is
# serving, but each additional worker is a separate Python process
# importing the whole litellm tree, which took roughly 20 seconds each
# on this machine. Without this settle window the harness would start
# warming and then measuring while LiteLLM was still booting, which
# would be a rigged result in Penstock's favour.
#
# Measured on run compare-20260806T235233Z: with 8 workers the last
# worker finished initialising 13 seconds AFTER k6 had already started.
# The LiteLLM arm does not begin until well after that, so that run was
# not affected, but relying on the arm ordering for correctness is not
# a property worth keeping.
LITELLM_SETTLE=$((5 + 5 * LITELLM_NUM_WORKERS))
echo "  ..   letting litellm settle for ${LITELLM_SETTLE}s (${LITELLM_NUM_WORKERS} workers to boot)"
sleep "$LITELLM_SETTLE"

# ---------------------------------------------------------------------
# Preflight
#
# A health endpoint proves a process is listening. It does not prove the
# gateway routes the model this run asks for or accepts the key it
# presents, and both of those fail FAST: a 404 or a 401 is cheaper than
# a completion, so a misconfigured arm comes back looking dramatically
# quicker. That is the most dangerous failure mode this harness has,
# because it produces a number rather than an error. One real request
# goes through each path and anything but 200 stops the run here.
# ---------------------------------------------------------------------

completion_status() {
  local url="$1" model="$2" content="$3"
  curl --silent --output /dev/null --write-out '%{http_code}' \
    --max-time 60 \
    --header 'Content-Type: application/json' \
    --header "Authorization: Bearer ${API_KEY}" \
    --data "{\"model\":\"${model}\",\"messages\":[{\"role\":\"user\",\"content\":\"${content}\"}],\"stream\":false}" \
    "${url}/v1/chat/completions" 2>/dev/null || echo 000
}

preflight() {
  local name="$1" url="$2" model="$3" status
  status="$(completion_status "$url" "$model" "preflight")"
  if [ "$status" = "200" ]; then
    echo "  ok   $name answered 200 to a real completion"
    return 0
  fi
  case "$status" in
    404) die "$name answered 404 for model \"$model\"." \
      "That arm would have failed fast on every request and looked artificially quick." ;;
    401) die "$name answered 401. The key this run presents is not one it accepts." ;;
    000) die "$name did not answer a completion at all. It is listening but not serving." \
      "Check $RESULTS_DIR/${RUN_ID}.*.log" ;;
    *)   die "$name answered $status to a preflight completion, wanted 200." \
      "Check $RESULTS_DIR/${RUN_ID}.*.log" ;;
  esac
}

# ---------------------------------------------------------------------
# Warmup
#
# Equal warmup for all three paths, before any preflight, so that no arm
# enters the measurement colder than another. Each path has its own
# simulator, so all three simulators advance their request counter by
# the same amount and the seeded pairing across arms is preserved.
# ---------------------------------------------------------------------

echo "warming up ($WARMUP_REQUESTS requests through each of the three paths)"
warm() {
  local url="$1" model="$2" i=0
  while [ "$i" -lt "$WARMUP_REQUESTS" ]; do
    completion_status "$url" "$model" "warmup ${i}" >/dev/null
    i=$((i + 1))
  done
}
warm "$DIRECT_URL" "$MODEL"
warm "$BASE_URL" "$MODEL"
warm "$LITELLM_URL" "$MODEL"
echo "  ok   all three paths warmed equally"

preflight "llmsim direct baseline" "$DIRECT_URL" "$MODEL"
preflight "penstock gateway"       "$BASE_URL"   "$MODEL"
preflight "litellm proxy"          "$LITELLM_URL" "$MODEL"

# ---------------------------------------------------------------------
# Hardware and version stanza
#
# A latency figure without the machine that produced it is not a result.
# For a comparison it is worse than useless, because a reader cannot
# tell whether the loser lost on merit or on core starvation. Written
# before k6 starts so it exists even for a run that fails.
# ---------------------------------------------------------------------

cpu_model() {
  if [ -r /proc/cpuinfo ]; then
    awk -F': ' '/^model name/ {print $2; exit}' /proc/cpuinfo && return 0
  fi
  if command -v sysctl >/dev/null 2>&1; then
    sysctl -n machdep.cpu.brand_string 2>/dev/null && return 0
  fi
  if [ "$IS_WINDOWS" = "1" ] && command -v powershell >/dev/null 2>&1; then
    powershell -NoProfile -Command \
      "(Get-CimInstance Win32_Processor | Select-Object -First 1).Name" 2>/dev/null \
      | tr -d '\r' && return 0
  fi
  echo "unknown"
}

cpu_count() {
  command -v nproc >/dev/null 2>&1 && { nproc 2>/dev/null && return 0; }
  [ -n "${NUMBER_OF_PROCESSORS:-}" ] && { echo "$NUMBER_OF_PROCESSORS" && return 0; }
  echo "unknown"
}

mem_total() {
  if [ -r /proc/meminfo ]; then
    awk '/^MemTotal/ {printf "%.1f GiB", $2/1048576; exit}' /proc/meminfo && return 0
  fi
  if [ "$IS_WINDOWS" = "1" ] && command -v powershell >/dev/null 2>&1; then
    powershell -NoProfile -Command \
      "'{0:N1} GiB' -f ((Get-CimInstance Win32_ComputerSystem).TotalPhysicalMemory/1GB)" 2>/dev/null \
      | tr -d '\r' && return 0
  fi
  echo "unknown"
}

os_desc() {
  if [ "$IS_WINDOWS" = "1" ] && command -v powershell >/dev/null 2>&1; then
    powershell -NoProfile -Command \
      "(Get-CimInstance Win32_OperatingSystem).Caption + ' ' + (Get-CimInstance Win32_OperatingSystem).Version" 2>/dev/null \
      | tr -d '\r' && return 0
  fi
  uname -srm 2>/dev/null || echo "unknown"
}

json_escape() { printf '%s' "$1" | sed 's/\\/\\\\/g; s/"/\\"/g'; }

# Penstock has no --version flag, so the binary is fingerprinted
# instead. That identifies the build exactly, which is what a version
# string is for. BENCH_COMMIT can carry the revision if the caller
# knows it; this script does not shell out to git.
bin_fingerprint() {
  local f="$1"
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$f" 2>/dev/null | cut -c1-16 && return 0
  fi
  if command -v certutil >/dev/null 2>&1; then
    certutil -hashfile "$f" SHA256 2>/dev/null | sed -n 2p | tr -d ' \r' | cut -c1-16 && return 0
  fi
  echo "unknown"
}

LITELLM_VERSION="$("$LITELLM_PY" -c \
  "import importlib.metadata as m; print(m.version('litellm'))" 2>/dev/null || echo unknown)"
PY_VERSION="$("$LITELLM_PY" -c \
  "import sys; print('CPython %d.%d.%d' % sys.version_info[:3])" 2>/dev/null || echo unknown)"
DEP_VERSIONS="$("$LITELLM_PY" -c "
import importlib.metadata as m
out=[]
for p in ['uvicorn','fastapi','httptools','uvloop','orjson','pydantic']:
    try: out.append(p+'=='+m.version(p))
    except Exception: out.append(p+'=absent')
print(', '.join(out))
" 2>/dev/null || echo unknown)"
PENSTOCK_FP="$(bin_fingerprint "$PENSTOCK_BIN")"

cat >"$META" <<EOF
{
  "run_id": "$(json_escape "$RUN_ID")",
  "scenario": "$(json_escape "$SCENARIO")",
  "started_utc": "$(date -u +%Y-%m-%dT%H:%M:%SZ)",
  "commit": "$(json_escape "${BENCH_COMMIT:-unknown}")",

  "hardware": {
    "cpu": "$(json_escape "$(cpu_model)")",
    "logical_cpus": "$(json_escape "$(cpu_count)")",
    "memory": "$(json_escape "$(mem_total)")",
    "os": "$(json_escape "$(os_desc)")",
    "note": "k6, three llmsim instances, penstock and litellm all ran on this one machine and competed for these cores"
  },

  "toolchain": {
    "go": "$(json_escape "$(go version 2>/dev/null || echo unknown)")",
    "k6": "$(json_escape "$(k6 version 2>/dev/null | head -n 1 || echo unknown)")",
    "python": "$(json_escape "$PY_VERSION")"
  },

  "penstock": {
    "binary_sha256_prefix": "$(json_escape "$PENSTOCK_FP")",
    "config": "$(json_escape "$GATEWAY_CONFIG")",
    "listen": "$(json_escape "$BASE_URL")",
    "note": "unmodified bench/config/gateway.yaml, the same file its own flagship benchmark uses"
  },

  "litellm": {
    "version": "$(json_escape "$LITELLM_VERSION")",
    "config": "$(json_escape "$LITELLM_CONFIG")",
    "launcher": "bench/compare/start-litellm.sh",
    "listen": "$(json_escape "$LITELLM_URL")",
    "num_workers": "$(json_escape "$LITELLM_NUM_WORKERS")",
    "deps": "$(json_escape "$DEP_VERSIONS")",
    "steelman": "LITELLM_MODE=PRODUCTION, LITELLM_LOG=ERROR, telemetry off, local model cost map, json_logs, set_verbose false, no database, no redis, no callbacks",
    "known_handicaps_on_this_platform": "uvloop unavailable on Windows; gunicorn unavailable on Windows so uvicorn supervises the workers"
  },

  "upstream": {
    "simulator": "llmsim",
    "profile_path": "$(json_escape "$PROFILE_USED")",
    "profile_status": "$(json_escape "$PROFILE_STATUS")",
    "profile_recorded_samples": "$(json_escape "$PROFILE_SAMPLES")",
    "seed": "$(json_escape "$SEED")",
    "time_scale": "$(json_escape "$TIME_SCALE")",
    "instances": "three, identically seeded: one for the baseline, one behind penstock, one behind litellm"
  },

  "load": {
    "duration_per_arm": "$(json_escape "$DURATION")",
    "arms": "direct_a, via_penstock, via_litellm, direct_b",
    "rate_per_second": "$(json_escape "$RATE")",
    "pre_allocated_vus": "$(json_escape "$PRE_ALLOCATED_VUS")",
    "max_vus": "$(json_escape "$MAX_VUS")",
    "arm_gap": "$(json_escape "$ARM_GAP")",
    "warmup_requests_per_path": "$(json_escape "$WARMUP_REQUESTS")",
    "drift_check": "$(json_escape "$DRIFT_CHECK")"
  }
}
EOF
echo "  ok   hardware and version stanza written to $META"

# ---------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------

echo ""
echo "running $SCENARIO"
echo ""

set +e
k6 run \
  --out "json=${RAW}" \
  --tag "run_id=${RUN_ID}" \
  --tag "scenario=${SCENARIO}" \
  --env "BASE_URL=${BASE_URL}" \
  --env "DIRECT_URL=${DIRECT_URL}" \
  --env "LITELLM_URL=${LITELLM_URL}" \
  --env "ADMIN_URL=${ADMIN_URL}" \
  --env "API_KEY=${API_KEY}" \
  --env "MODEL=${MODEL}" \
  --env "LITELLM_MODEL=${MODEL}" \
  --env "DURATION=${DURATION}" \
  --env "RATE=${RATE}" \
  --env "PRE_ALLOCATED_VUS=${PRE_ALLOCATED_VUS}" \
  --env "MAX_VUS=${MAX_VUS}" \
  --env "ARM_GAP=${ARM_GAP}" \
  --env "DRIFT_CHECK=${DRIFT_CHECK}" \
  --env "SUMMARY_TXT=${SUMMARY_TXT}" \
  --env "SUMMARY_JSON=${SUMMARY_JSON}" \
  "$SCRIPT"
K6_STATUS=$?
set -e

echo ""
echo "results"
echo "  raw samples   $RAW"
echo "  summary       $SUMMARY_TXT"
echo "  metadata      $META"
echo "  penstock log  $LOG_GATEWAY"
echo "  litellm log   $LOG_LITELLM"

if [ "$K6_STATUS" -ne 0 ]; then
  echo ""
  echo "k6 exited $K6_STATUS. Exit code 99 means a threshold failed, which here"
  echo "means dropped iterations or a non zero error rate. Either one means the"
  echo "arms did not receive equal load, which voids the comparison rather than"
  echo "merely degrading it. Do not quote this run."
fi

exit "$K6_STATUS"
