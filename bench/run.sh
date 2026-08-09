#!/usr/bin/env bash
#
# run.sh - bring up the whole benchmark stack, run one scenario, and
# leave behind everything needed to check the result.
#
# The operator starts nothing by hand. This script builds the binaries,
# starts two llmsim instances and the gateway, waits for all three to
# answer their health endpoint, runs k6, and writes the raw per sample
# JSON plus a hardware stanza into bench/results/.
#
# Usage:
#   bash bench/run.sh [scenario]
#   bash bench/run.sh gateway_overhead
#   DURATION=2m RATE=50 bash bench/run.sh gateway_overhead
#
# Scenario defaults to gateway_overhead. Run with --list to see them all.
#
# PLATFORM
#
# Written for Git Bash on Windows, which is this repository's shell, and
# kept working on Linux and macOS. The Windows specific parts are
# labelled "Git Bash:" where they appear. The differences that matter:
#
#   - Binaries get a .exe suffix, so BIN_EXT below is set from uname.
#   - Arguments handed to the Go binaries and to k6 are always relative
#     paths. Those are native Windows executables, and MSYS rewrites an
#     absolute POSIX path like /c/foo into C:/foo on the way in, which
#     is usually right and occasionally is not. Relative paths sidestep
#     the whole question.
#   - A Windows program flag such as /FI has to be written //FI, or MSYS
#     treats it as a path and mangles it.
#   - Resident memory sampling for the soak uses tasklist on Windows and
#     ps everywhere else. It is best effort and never fails a run.

set -euo pipefail

BENCH_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$BENCH_DIR/.." && pwd)"
cd "$REPO_ROOT"

RESULTS_DIR="bench/results"

# ---------------------------------------------------------------------
# Platform detection
# ---------------------------------------------------------------------

BIN_EXT=""
IS_WINDOWS=0
case "$(uname -s 2>/dev/null || echo unknown)" in
  MINGW* | MSYS* | CYGWIN*)
    BIN_EXT=".exe"
    IS_WINDOWS=1
    ;;
esac

# ---------------------------------------------------------------------
# Loud failures
#
# A benchmark harness that half starts and then produces a number is
# worse than one that refuses to start, so every precondition below
# exits with the command that fixes it.
# ---------------------------------------------------------------------

die() {
  echo "" >&2
  echo "bench/run.sh: $1" >&2
  shift
  while [ "$#" -gt 0 ]; do
    echo "  $1" >&2
    shift
  done
  echo "" >&2
  exit 1
}

require_k6() {
  command -v k6 >/dev/null 2>&1 && return 0
  die "k6 is not on PATH, and no scenario can run without it." \
    "" \
    "If you already have it under your SDK directory:" \
    "  export PATH=\"\$HOME/sdk/bin:\$PATH\"" \
    "" \
    "Otherwise install it:" \
    "  macOS         brew install k6" \
    "  Windows       winget install k6 --source winget" \
    "  Linux/other   https://github.com/grafana/k6/releases" \
    "" \
    "Verify with: k6 version"
}

require_go() {
  command -v go >/dev/null 2>&1 && return 0
  die "go is not on PATH, and the gateway and simulator have to be built." \
    "" \
    "If you already have a toolchain:" \
    "  export PATH=\"\$HOME/sdk/go/bin:\$PATH\"" \
    "" \
    "Otherwise install Go 1.24 or newer from https://go.dev/dl/" \
    "" \
    "Verify with: go version"
}

require_curl() {
  command -v curl >/dev/null 2>&1 && return 0
  die "curl is not on PATH, and it is how this script waits for health." \
    "" \
    "Git Bash ships curl, so a missing one usually means a trimmed PATH." \
    "  Linux    sudo apt install curl" \
    "  macOS    curl is preinstalled" \
    "" \
    "Verify with: curl --version"
}

# ---------------------------------------------------------------------
# Scenario selection and its defaults
#
# Each scenario gets its own defaults for the knobs whose right value
# depends on what is being measured. Every one of them is overridable
# from the environment, and the value actually used is written into the
# run's .meta.json so a result can be traced back to the run that made
# it.
# ---------------------------------------------------------------------

SCENARIO="${1:-gateway_overhead}"

if [ "$SCENARIO" = "--list" ] || [ "$SCENARIO" = "-l" ]; then
  echo "scenarios:"
  echo "  gateway_overhead   latency the gateway adds, direct against through (default)"
  echo "  streaming_ttft     time to first token, direct against through"
  echo "  cache_hit          exact cache hit against miss, and the hit rate"
  echo "  soak               long steady run for leak watching"
  exit 0
fi

SCRIPT="bench/scenarios/${SCENARIO}.js"
[ -f "$SCRIPT" ] || die "unknown scenario \"$SCENARIO\": $SCRIPT does not exist." \
  "Run: bash bench/run.sh --list"

GATEWAY_CONFIG="bench/config/gateway.yaml"

case "$SCENARIO" in
  gateway_overhead)
    # A low time scale on purpose. At time scale 1 the simulator spends
    # around three seconds per request generating tokens, and a gateway
    # overhead of a millisecond or two is invisible underneath it: the
    # measurement would be reporting the variance of a lognormal sleep.
    # Compressing simulated time makes the gateway's own cost the
    # dominant term. bench/README.md lists what this trade costs.
    : "${TIME_SCALE:=0.05}"
    : "${RATE:=20}"
    ;;
  streaming_ttft)
    # Left at real time, because the absolute TTFT figures are only
    # meaningful if the profile is being replayed at the pace it was
    # calibrated at. The consequence is that the gateway's contribution
    # sits near the noise floor; rerun with TIME_SCALE=0.05 to resolve
    # it, and read the two results as answering different questions.
    : "${TIME_SCALE:=1.0}"
    # Lower than the default: a streamed request holds its connection
    # for the whole generation, so the VU pool has to cover the arrival
    # rate multiplied by the mean stream duration.
    : "${RATE:=10}"
    : "${MAX_VUS:=400}"
    : "${PRE_ALLOCATED_VUS:=100}"
    ;;
  cache_hit)
    GATEWAY_CONFIG="bench/config/gateway-cache.yaml"
    # Real time here, because the point of the scenario is what a hit
    # saves, and shrinking the upstream shrinks the saving being
    # measured.
    : "${TIME_SCALE:=1.0}"
    : "${RATE:=20}"
    : "${MAX_VUS:=400}"
    : "${PRE_ALLOCATED_VUS:=100}"
    ;;
  soak)
    # Compressed so hours of wall clock cover a useful number of
    # requests, but not so far that the gateway stops doing realistic
    # per request work.
    : "${TIME_SCALE:=0.2}"
    : "${SOAK_DURATION:=30m}"
    : "${SOAK_VUS:=5}"
    ;;
esac

# Shared defaults.
: "${DURATION:=30s}"
: "${RATE:=20}"
: "${PRE_ALLOCATED_VUS:=50}"
: "${MAX_VUS:=200}"
: "${SEED:=1}"
# Raw k6 output is committed as evidence, and it is enormous: a two
# minute run at 40 requests per second produced 67MB, which compresses
# to 1.3MB because the lines are nearly identical. Compressed is the
# default so a few committed runs do not turn the repository into a
# multi gigabyte download. Set RAW_GZIP=false when piping into a tool
# that cannot read gzip.
: "${RAW_GZIP:=true}"

# The three knobs below are read under BENCH_ prefixed names on purpose,
# and it is not decoration.
#
# The obvious names collide with variables that are already exported on
# real machines. This was not theoretical: the first end to end run of
# this harness was made on a Windows laptop whose vendor exports
# MODEL=MA16250, so the gateway was asked to route a model it does not
# serve, answered 404 to every request in the gateway arm, and the arm
# came back "faster" than the baseline because a 404 is cheap. API_KEY
# and PROFILE are just as easy to have exported for something else, and
# a wrong API_KEY would 401 the same way.
#
# Those are the three values that can silently turn a run into a
# measurement of an error path, so they do not inherit from the ambient
# environment. The k6 scripts themselves still read plain BASE_URL,
# API_KEY and MODEL, which is what you want when invoking k6 directly;
# this script passes them through --env, which overrides anything the
# system environment carries.
: "${BENCH_API_KEY:=benchbenchbenchbenchbench}"
: "${BENCH_MODEL:=llmsim-small}"
: "${BENCH_PROFILE:=bench/profiles/groq.json}"

API_KEY="$BENCH_API_KEY"
MODEL="$BENCH_MODEL"
PROFILE="$BENCH_PROFILE"

# Ports. These have to agree with bench/config/*.yaml, which is where
# the gateway reads its own listeners from, so changing one means
# changing both.
: "${LLMSIM_PORT:=8089}"
: "${DIRECT_PORT:=8090}"
: "${GATEWAY_PORT:=8080}"
: "${ADMIN_PORT:=9090}"

BASE_URL="http://127.0.0.1:${GATEWAY_PORT}"
DIRECT_URL="http://127.0.0.1:${DIRECT_PORT}"
ADMIN_URL="http://127.0.0.1:${ADMIN_PORT}"

require_k6
require_go
require_curl

RUN_ID="${SCENARIO}-$(date -u +%Y%m%dT%H%M%SZ)"
mkdir -p "$RESULTS_DIR"

RAW="${RESULTS_DIR}/${RUN_ID}.raw.json"
if [ "$RAW_GZIP" = "true" ]; then
  RAW="${RAW}.gz"
fi
SUMMARY_TXT="${RESULTS_DIR}/${RUN_ID}.summary.txt"
SUMMARY_JSON="${RESULTS_DIR}/${RUN_ID}.summary.json"
META="${RESULTS_DIR}/${RUN_ID}.meta.json"
RSS_CSV="${RESULTS_DIR}/${RUN_ID}.rss.csv"
LOG_SIM_A="${RESULTS_DIR}/${RUN_ID}.llmsim-a.log"
LOG_SIM_B="${RESULTS_DIR}/${RUN_ID}.llmsim-b.log"
LOG_GATEWAY="${RESULTS_DIR}/${RUN_ID}.gateway.log"

# ---------------------------------------------------------------------
# Process lifecycle
# ---------------------------------------------------------------------

PIDS=""
RSS_PID=""

cleanup() {
  local code=$?
  set +e
  if [ -n "$RSS_PID" ]; then
    kill "$RSS_PID" >/dev/null 2>&1
  fi
  if [ -n "$PIDS" ]; then
    for pid in $PIDS; do
      kill "$pid" >/dev/null 2>&1
    done
    # Give each one its shutdown grace before insisting.
    sleep 2
    for pid in $PIDS; do
      kill -9 "$pid" >/dev/null 2>&1
    done
  fi
  exit "$code"
}
trap cleanup EXIT INT TERM

# start_bg <logfile> <command...>
#
# The started pid comes back in the global BG_PID rather than on stdout.
# Returning it through a command substitution would run the whole
# function in a subshell, and the PIDS list it appends to would be
# discarded along with that subshell, leaving the cleanup trap with
# nothing to kill.
BG_PID=""
start_bg() {
  local log="$1"
  shift
  "$@" >"$log" 2>&1 &
  BG_PID=$!
  PIDS="$PIDS $BG_PID"
}

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
    "A likely cause is a port already in use by an earlier run that did" \
    "not shut down. Check with: curl -sv $url"
}

# ---------------------------------------------------------------------
# Build
# ---------------------------------------------------------------------

PENSTOCK_BIN="bin/penstock${BIN_EXT}"
LLMSIM_BIN="bin/llmsim${BIN_EXT}"

echo "building"
go build -o "$PENSTOCK_BIN" ./cmd/penstock \
  || die "go build ./cmd/penstock failed. Fix the build before benchmarking it."
go build -o "$LLMSIM_BIN" ./cmd/llmsim \
  || die "go build ./cmd/llmsim failed."
echo "  ok   $PENSTOCK_BIN"
echo "  ok   $LLMSIM_BIN"

# ---------------------------------------------------------------------
# The latency profile
#
# This is the whole argument the harness rests on. llmsim's timings come
# from a profile calibrated against recorded traffic from a real
# provider. Without that file it falls back to a built in guess, and a
# result produced from a guess is not a calibrated benchmark. The run is
# allowed to proceed, because the harness has to be usable before the
# recorder exists, but it says so at the top and it says so again in the
# metadata written beside the result.
# ---------------------------------------------------------------------

PROFILE_ARGS=""
PROFILE_STATUS="calibrated"
PROFILE_USED="$PROFILE"
if [ -f "$PROFILE" ]; then
  PROFILE_ARGS="--profile $PROFILE"
else
  PROFILE_STATUS="uncalibrated-builtin-default"
  PROFILE_USED="llmsim built in DefaultProfile"
  echo ""
  echo "======================================================================"
  echo "WARNING: $PROFILE is missing."
  echo ""
  echo "llmsim will fall back to its built in DefaultProfile, which is an"
  echo "approximation of a mid tier hosted model and was not calibrated"
  echo "against anything. The harness still works and the gateway overhead it"
  echo "reports is still a real measurement, but the upstream it is measured"
  echo "against is invented. Do not publish numbers from this run as being"
  echo "against a calibrated profile."
  echo ""
  echo "Produce the profile with the calibration recorder, then rerun."
  echo "======================================================================"
  echo ""
fi

# ---------------------------------------------------------------------
# Bring the stack up
#
# Two simulators, not one. The gateway talks to the first; the direct
# arm of the comparison talks to the second. They carry the same --seed,
# and llmsim derives each request's simulated latency from the seed and
# the request's index, so request i in the direct arm and request i in
# the gateway arm are served with the identical planned TTFT, inter
# token latency and token count. That turns the comparison from two
# independent draws out of the same distribution into a paired one, and
# the difference between the arms stops carrying the upstream's sampling
# noise. Sharing one simulator between the arms would give that up.
# ---------------------------------------------------------------------

echo "starting stack"

# Word splitting on PROFILE_ARGS is intended: it is either empty or two
# arguments.
# shellcheck disable=SC2086
start_bg "$LOG_SIM_A" "./$LLMSIM_BIN" \
  --listen "127.0.0.1:${LLMSIM_PORT}" --seed "$SEED" --time-scale "$TIME_SCALE" $PROFILE_ARGS
SIM_A_PID="$BG_PID"
# shellcheck disable=SC2086
start_bg "$LOG_SIM_B" "./$LLMSIM_BIN" \
  --listen "127.0.0.1:${DIRECT_PORT}" --seed "$SEED" --time-scale "$TIME_SCALE" $PROFILE_ARGS
SIM_B_PID="$BG_PID"

wait_healthy "llmsim (gateway upstream)" "http://127.0.0.1:${LLMSIM_PORT}/healthz" "$SIM_A_PID"
wait_healthy "llmsim (direct baseline)" "${DIRECT_URL}/healthz" "$SIM_B_PID"

start_bg "$LOG_GATEWAY" "./$PENSTOCK_BIN" --config "$GATEWAY_CONFIG"
GATEWAY_PID="$BG_PID"
wait_healthy "penstock gateway" "${BASE_URL}/healthz" "$GATEWAY_PID"
wait_healthy "penstock admin" "${ADMIN_URL}/metrics" "$GATEWAY_PID"

# ---------------------------------------------------------------------
# Preflight
#
# A health endpoint answering 200 proves a process is listening. It does
# not prove the gateway will route the model this run is about to ask
# for, or that it will accept the key this run is about to present. Both
# of those fail per request rather than at startup, and both fail FAST:
# a 404 for an unrouted model and a 401 for a bad key are cheaper than a
# real completion, so a run that gets them comes back with the gateway
# arm looking dramatically faster than the baseline. That is the most
# dangerous failure this harness has, because it produces a number
# instead of an error.
#
# So one real request goes through each path before k6 starts, and
# anything other than 200 stops the run here.
# ---------------------------------------------------------------------

preflight() {
  local name="$1" url="$2" model="$3"
  local body status
  body="{\"model\":\"${model}\",\"messages\":[{\"role\":\"user\",\"content\":\"preflight\"}],\"stream\":false}"
  status="$(curl --silent --output /dev/null --write-out '%{http_code}' \
    --max-time 60 \
    --header 'Content-Type: application/json' \
    --header "Authorization: Bearer ${API_KEY}" \
    --data "$body" \
    "${url}/v1/chat/completions" 2>/dev/null || echo 000)"

  if [ "$status" = "200" ]; then
    echo "  ok   $name answered 200 to a real completion"
    return 0
  fi

  case "$status" in
    404)
      die "$name answered 404 for model \"$model\"." \
        "The gateway does not route that model, so every request in this run" \
        "would have failed fast and the arm would have looked artificially quick." \
        "" \
        "Set the model explicitly:" \
        "  BENCH_MODEL=llmsim-small bash bench/run.sh $SCENARIO" \
        "" \
        "Routed models are listed by:" \
        "  curl -s ${BASE_URL}/v1/models" \
        "and configured in $GATEWAY_CONFIG."
      ;;
    401)
      die "$name answered 401." \
        "The key this run presents is not one $GATEWAY_CONFIG accepts." \
        "" \
        "Set it explicitly:" \
        "  BENCH_API_KEY=<key from $GATEWAY_CONFIG> bash bench/run.sh $SCENARIO"
      ;;
    000)
      die "$name did not answer a completion request at all." \
        "Health passed, so it is listening but not serving. Check:" \
        "  $LOG_GATEWAY"
      ;;
    *)
      die "$name answered $status to a preflight completion, wanted 200." \
        "Check the logs beside this run:" \
        "  $RESULTS_DIR/${RUN_ID}.*.log"
      ;;
  esac
}

# Exactly one preflight per simulator, which is what keeps the paired
# seeding intact: each instance's request counter advances by one, so
# the two arms still line up index for index once k6 starts.
preflight "penstock gateway" "$BASE_URL" "$MODEL"
preflight "llmsim direct baseline" "$DIRECT_URL" "$MODEL"

# ---------------------------------------------------------------------
# Hardware stanza
#
# A latency figure without the machine that produced it is not a result,
# it is a rumour. This is written before k6 starts so it exists even for
# a run that fails, and it is committed alongside the raw samples.
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
  if command -v nproc >/dev/null 2>&1; then
    nproc 2>/dev/null && return 0
  fi
  if [ -n "${NUMBER_OF_PROCESSORS:-}" ]; then
    echo "$NUMBER_OF_PROCESSORS" && return 0
  fi
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

# os_desc records the OS and version. The Windows edition is dropped: it
# names the licence rather than the machine, and nothing about it changes
# a latency figure. The version, which can, is kept.
os_desc() {
  if [ "$IS_WINDOWS" = "1" ] && command -v powershell >/dev/null 2>&1; then
    powershell -NoProfile -Command \
      "(Get-CimInstance Win32_OperatingSystem).Caption + ' ' + (Get-CimInstance Win32_OperatingSystem).Version" 2>/dev/null \
      | tr -d '\r' \
      | sed -E 's/^Microsoft (Windows [0-9]+)( [A-Za-z]+)*/\1/' && return 0
  fi
  uname -srm 2>/dev/null || echo "unknown"
}

json_escape() { printf '%s' "$1" | sed 's/\\/\\\\/g; s/"/\\"/g'; }

# Read only, and only to record which tree produced the number. Set
# BENCH_COMMIT yourself to skip the lookup entirely.
COMMIT="${BENCH_COMMIT:-$(git rev-parse --short HEAD 2>/dev/null || echo unknown)}"

cat >"$META" <<EOF
{
  "run_id": "$(json_escape "$RUN_ID")",
  "scenario": "$(json_escape "$SCENARIO")",
  "started_utc": "$(date -u +%Y-%m-%dT%H:%M:%SZ)",
  "commit": "$(json_escape "$COMMIT")",

  "hardware": {
    "cpu": "$(json_escape "$(cpu_model)")",
    "logical_cpus": "$(json_escape "$(cpu_count)")",
    "memory": "$(json_escape "$(mem_total)")",
    "os": "$(json_escape "$(os_desc)")",
    "note": "load generator, gateway and simulated upstream all ran on this one machine and competed for these cores"
  },

  "toolchain": {
    "go": "$(json_escape "$(go version 2>/dev/null || echo unknown)")",
    "k6": "$(json_escape "$(k6 version 2>/dev/null | head -n 1 || echo unknown)")"
  },

  "upstream": {
    "simulator": "llmsim",
    "profile_path": "$(json_escape "$PROFILE_USED")",
    "profile_status": "$(json_escape "$PROFILE_STATUS")",
    "seed": "$(json_escape "$SEED")",
    "time_scale": "$(json_escape "$TIME_SCALE")",
    "instances": "two, identically seeded: one behind the gateway, one for the direct arm"
  },

  "gateway": {
    "config": "$(json_escape "$GATEWAY_CONFIG")",
    "listen": "$(json_escape "$BASE_URL")"
  },

  "load": {
    "duration": "$(json_escape "$DURATION")",
    "rate_per_second": "$(json_escape "$RATE")",
    "pre_allocated_vus": "$(json_escape "$PRE_ALLOCATED_VUS")",
    "max_vus": "$(json_escape "$MAX_VUS")",
    "soak_duration": "$(json_escape "${SOAK_DURATION:-n/a}")",
    "soak_vus": "$(json_escape "${SOAK_VUS:-n/a}")"
  }
}
EOF
echo "  ok   hardware stanza written to $META"

# ---------------------------------------------------------------------
# Resident memory sampling, soak only
#
# Best effort by design: it must never fail a run. internal/obs builds a
# private Prometheus registry carrying only Penstock's own instruments,
# so go_memstats_* and go_goroutines are not on /metrics and heap growth
# cannot be read in band. RSS from the operating system is the coarse
# substitute.
# ---------------------------------------------------------------------

# Every pipeline below carries "|| true". Under set -e a failing
# command substitution would abort the run, and a memory sample that
# could not be taken must never be the reason a benchmark stops.
rss_kb() {
  local pid="$1" v winpid
  # Linux and macOS report RSS in kilobytes straight from ps.
  v="$(ps -o rss= -p "$pid" 2>/dev/null | tr -cd '0-9' || true)"
  if [ -n "$v" ]; then
    printf '%s' "$v"
    return 0
  fi
  # Git Bash: the MSYS ps has no RSS column, but it does print the
  # Windows pid in column 4, which tasklist understands. The leading
  # double slash on //FI stops MSYS rewriting the flag as a path.
  if [ "$IS_WINDOWS" = "1" ] && command -v tasklist >/dev/null 2>&1; then
    winpid="$(ps -p "$pid" 2>/dev/null | awk -v p="$pid" '$1 == p {print $4; exit}' || true)"
    [ -n "$winpid" ] || winpid="$pid"
    tasklist //FI "PID eq $winpid" //NH //FO CSV 2>/dev/null \
      | awk -F'","' 'NR == 1 {gsub(/[^0-9]/, "", $5); print $5}' || true
    return 0
  fi
  printf ''
}

if [ "$SCENARIO" = "soak" ]; then
  echo "elapsed_s,gateway_rss_kb" >"$RSS_CSV"
  (
    started=$(date +%s)
    while true; do
      value="$(rss_kb "$GATEWAY_PID")"
      [ -n "$value" ] || value="unknown"
      echo "$(($(date +%s) - started)),$value" >>"$RSS_CSV"
      sleep 15
    done
  ) &
  RSS_PID=$!
  echo "  ok   resident memory sampling into $RSS_CSV (best effort)"
fi

# ---------------------------------------------------------------------
# Run
#
# Every knob is passed explicitly rather than inherited, so the command
# in the log is the complete description of the run.
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
  --env "ADMIN_URL=${ADMIN_URL}" \
  --env "API_KEY=${API_KEY}" \
  --env "MODEL=${MODEL}" \
  --env "DURATION=${DURATION}" \
  --env "RATE=${RATE}" \
  --env "PRE_ALLOCATED_VUS=${PRE_ALLOCATED_VUS}" \
  --env "MAX_VUS=${MAX_VUS}" \
  --env "SOAK_DURATION=${SOAK_DURATION:-30m}" \
  --env "SOAK_VUS=${SOAK_VUS:-5}" \
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
if [ -f "$RSS_CSV" ]; then
  echo "  memory        $RSS_CSV"
fi
echo "  gateway log   $LOG_GATEWAY"

if [ "$K6_STATUS" -ne 0 ]; then
  echo ""
  echo "k6 exited $K6_STATUS. Exit code 99 means a threshold failed, which in"
  echo "this harness usually means dropped iterations or a non zero error"
  echo "rate. Both invalidate a comparison rather than merely degrading it."
  echo "Read the summary above before quoting anything from this run."
fi

exit "$K6_STATUS"
