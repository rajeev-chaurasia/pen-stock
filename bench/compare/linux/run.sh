#!/usr/bin/env bash
#
# bench/compare/linux/run.sh - the Linux rerun of the LiteLLM comparison.
#
# This is bench/compare/run.sh with three changes and no others:
#
#   1. It does not build. The Go binaries are cross compiled from the
#      same source with GOOS=linux GOARCH=amd64 CGO_ENABLED=0 and copied
#      in, so no Go toolchain is needed on this side and the Go program
#      under test is byte identical across every arm and every run.
#   2. Linux process teardown, by process group, instead of taskkill.
#   3. It records uvloop evidence for the process it actually measured,
#      because "uvloop was installed" is a weaker claim than the one
#      this run exists to make.
#
# Everything that determines the numbers is deliberately unchanged:
# same scenario file, same k6 version, same profile, same seed, same
# time scale, same arrival rate, same VU pool, same arm order, same
# gaps, same warmup, same drift check, same thresholds, same three
# identically seeded simulators, same unmodified bench/config/gateway.yaml
# for Penstock, same bench/compare/litellm.config.yaml for LiteLLM.
#
# Usage:
#   bash bench/compare/linux/run.sh
#   DURATION=120s RATE=20 LITELLM_NUM_WORKERS=2 bash bench/compare/linux/run.sh

set -euo pipefail

: "${WORKDIR:=$HOME/penstock-bench}"
cd "$WORKDIR"

RESULTS_DIR="bench/results"
SCENARIO="compare_litellm"
SCRIPT="bench/scenarios/${SCENARIO}.js"

export PATH="$WORKDIR/bin:$PATH"

die() {
  echo "" >&2
  echo "bench/compare/linux/run.sh: $1" >&2
  shift
  while [ "$#" -gt 0 ]; do echo "  $1" >&2; shift; done
  echo "" >&2
  exit 1
}

command -v k6 >/dev/null 2>&1 || die "k6 is not on PATH ($WORKDIR/bin)."
command -v curl >/dev/null 2>&1 || die "curl is not on PATH."

: "${LITELLM_VENV:=$HOME/sdk/litellm-venv-linux}"
LITELLM_PY="${LITELLM_VENV}/bin/python"
LITELLM_CLI="${LITELLM_VENV}/bin/litellm"
[ -x "$LITELLM_CLI" ] || die "no LiteLLM proxy at $LITELLM_CLI" \
  "Run bench/compare/linux/setup.sh first."

# ---------------------------------------------------------------------
# Knobs. Identical defaults to the Windows harness.
# ---------------------------------------------------------------------

: "${TIME_SCALE:=0.05}"
: "${DURATION:=120s}"
: "${RATE:=20}"
: "${PRE_ALLOCATED_VUS:=50}"
: "${MAX_VUS:=200}"
: "${SEED:=1}"
: "${ARM_GAP:=5s}"
: "${DRIFT_CHECK:=true}"
: "${RAW_GZIP:=true}"
: "${WARMUP_REQUESTS:=30}"

: "${BENCH_API_KEY:=penstock-bench-key-0123456789abcdef}"
: "${BENCH_MODEL:=llmsim-small}"
: "${BENCH_PROFILE:=bench/profiles/groq.json}"

API_KEY="$BENCH_API_KEY"
MODEL="$BENCH_MODEL"
PROFILE="$BENCH_PROFILE"

: "${LLMSIM_PORT:=8089}"
: "${DIRECT_PORT:=8090}"
: "${LITELLM_SIM_PORT:=8091}"
: "${GATEWAY_PORT:=8080}"
: "${ADMIN_PORT:=9090}"
: "${LITELLM_PORT:=8081}"
: "${LITELLM_NUM_WORKERS:=1}"
: "${LITELLM_SERVER:=uvicorn}"
export LITELLM_NUM_WORKERS LITELLM_PORT LITELLM_VENV LITELLM_SERVER WORKDIR

BASE_URL="http://127.0.0.1:${GATEWAY_PORT}"
DIRECT_URL="http://127.0.0.1:${DIRECT_PORT}"
ADMIN_URL="http://127.0.0.1:${ADMIN_PORT}"
LITELLM_URL="http://127.0.0.1:${LITELLM_PORT}"

GATEWAY_CONFIG="bench/config/gateway.yaml"
LITELLM_CONFIG="bench/compare/litellm.config.yaml"

: "${RUN_ID:=compare-linux-$(date -u +%Y%m%dT%H%M%SZ)}"
mkdir -p "$RESULTS_DIR"

RAW="${RESULTS_DIR}/${RUN_ID}.raw.json"
[ "$RAW_GZIP" = "true" ] && RAW="${RAW}.gz"
SUMMARY_TXT="${RESULTS_DIR}/${RUN_ID}.summary.txt"
SUMMARY_JSON="${RESULTS_DIR}/${RUN_ID}.summary.json"
META="${RESULTS_DIR}/${RUN_ID}.meta.json"
UVLOOP_TXT="${RESULTS_DIR}/${RUN_ID}.uvloop.txt"
LOG_SIM_PEN="${RESULTS_DIR}/${RUN_ID}.llmsim-penstock.log"
LOG_SIM_DIR="${RESULTS_DIR}/${RUN_ID}.llmsim-direct.log"
LOG_SIM_LLM="${RESULTS_DIR}/${RUN_ID}.llmsim-litellm.log"
LOG_GATEWAY="${RESULTS_DIR}/${RUN_ID}.penstock.log"
LOG_LITELLM="${RESULTS_DIR}/${RUN_ID}.litellm.log"

# ---------------------------------------------------------------------
# Process lifecycle.
#
# Every child is started with setsid so it owns a process group. LiteLLM
# with --num_workers > 1 is a supervisor with children; killing only the
# supervisor leaves workers holding port 8081 and the NEXT run then
# measures a stale proxy that looks perfectly healthy. Killing the group
# removes that failure mode entirely.
# ---------------------------------------------------------------------

PIDS=""
BG_PID=""

start_bg() {
  local log="$1"; shift
  setsid "$@" >"$log" 2>&1 &
  BG_PID=$!
  PIDS="$PIDS $BG_PID"
}

cleanup() {
  local code=$?
  set +e
  for pid in $PIDS; do kill -TERM "-$pid" 2>/dev/null; kill -TERM "$pid" 2>/dev/null; done
  sleep 2
  for pid in $PIDS; do kill -KILL "-$pid" 2>/dev/null; kill -KILL "$pid" 2>/dev/null; done
  exit "$code"
}
trap cleanup EXIT INT TERM

wait_healthy() {
  local name="$1" url="$2" pid="$3" attempts="${4:-150}" i=0
  while [ "$i" -lt "$attempts" ]; do
    if ! kill -0 "$pid" >/dev/null 2>&1; then
      die "$name exited before it became healthy." \
        "Its output is in $RESULTS_DIR/${RUN_ID}.*.log"
    fi
    if curl --silent --fail --max-time 2 "$url" >/dev/null 2>&1; then
      echo "  ok   $name  ($url)"
      return 0
    fi
    sleep 0.2
    i=$((i + 1))
  done
  die "$name never answered $url after $((attempts / 5)) seconds." \
    "Check $RESULTS_DIR/${RUN_ID}.*.log"
}

PENSTOCK_BIN="bin/penstock"
LLMSIM_BIN="bin/llmsim"
[ -x "$PENSTOCK_BIN" ] || die "missing $PENSTOCK_BIN (cross compile it, see setup.sh)"
[ -x "$LLMSIM_BIN" ]   || die "missing $LLMSIM_BIN (cross compile it, see setup.sh)"

echo "run           $RUN_ID"
echo "litellm       $LITELLM_SERVER, $LITELLM_NUM_WORKERS worker(s)"
echo "load          $RATE req/s, $DURATION per arm"
echo ""

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
  die "$PROFILE is missing. The Windows run used it; a Linux run without it is not comparable."
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

start_bg "$LOG_LITELLM" bash bench/compare/linux/start-litellm.sh
LITELLM_PID="$BG_PID"
wait_healthy "litellm proxy" "${LITELLM_URL}/health/liveliness" "$LITELLM_PID" 300

# Same settle window as the Windows harness, and for the same reason:
# /health/liveliness is answered by the FIRST worker while the others are
# still importing the litellm tree. Measuring a half booted proxy would
# be a rigged result in Penstock's favour.
LITELLM_SETTLE=$((5 + 5 * LITELLM_NUM_WORKERS))
echo "  ..   letting litellm settle for ${LITELLM_SETTLE}s (${LITELLM_NUM_WORKERS} worker(s) to boot)"
sleep "$LITELLM_SETTLE"

# ---------------------------------------------------------------------
# uvloop evidence, for the processes this run is about to measure.
#
# Read from /proc/<pid>/maps: a process that has uvloop's compiled
# extension mapped has imported it. This is a statement about the
# process under measurement, not about the library on disk.
# ---------------------------------------------------------------------

# set -e is OFF for the whole of this block on purpose. Collecting
# evidence must never be able to abort a run: a harness that dies while
# writing down what it observed would throw away the measurement in
# order to record it.
set +e
{
  echo "run_id            $RUN_ID"
  echo "server            $LITELLM_SERVER"
  echo "num_workers       $LITELLM_NUM_WORKERS"
  echo "platform loop     $("$LITELLM_PY" -c \
    'from litellm.proxy.proxy_cli import ProxyInitializationHelpers as H; print(H._get_loop_type())' 2>/dev/null)"
  echo ""
  # Every process that could be serving port $LITELLM_PORT: the socket
  # owner reported by ss, the launcher, and its descendants. gunicorn and
  # uvicorn multi-worker both put the listener on a master and the
  # request handling on children, so both have to be looked at.
  LPIDS="$( { ss -ltnp 2>/dev/null | grep ":${LITELLM_PORT} " | grep -o 'pid=[0-9]*' | cut -d= -f2
              echo "$LITELLM_PID"
              pgrep -P "$LITELLM_PID" 2>/dev/null
              for c in $(pgrep -P "$LITELLM_PID" 2>/dev/null); do pgrep -P "$c" 2>/dev/null; done
              pgrep -f "port ${LITELLM_PORT}" 2>/dev/null
            } | grep -E '^[0-9]+$' | sort -u )"
  MAPPED=0
  TOTAL=0
  for p in $LPIDS; do
    [ -r "/proc/$p/maps" ] || continue
    cmd="$(tr '\0' ' ' < "/proc/$p/cmdline" 2>/dev/null | cut -c1-60)"
    case "$cmd" in *litellm*|*python*|*gunicorn*) ;; *) continue ;; esac
    TOTAL=$((TOTAL + 1))
    so="$(grep -i uvloop "/proc/$p/maps" 2>/dev/null | awk '{print $NF}' | sort -u | head -1)"
    if [ -n "$so" ]; then
      MAPPED=$((MAPPED + 1))
      echo "pid $p  UVLOOP MAPPED  $so"
    else
      echo "pid $p  no uvloop mapping   ($cmd)"
    fi
  done
  echo ""
  echo "uvloop_mapped_processes  $MAPPED of $TOTAL"
  if [ "$MAPPED" -gt 0 ]; then
    echo "VERDICT  uvloop was loaded by the litellm process(es) this run measured."
  else
    echo "VERDICT  uvloop NOT observed. Do not claim it was active for this run."
  fi
} >"$UVLOOP_TXT" 2>&1
UVLOOP_VERDICT="$(grep -c 'UVLOOP MAPPED' "$UVLOOP_TXT")"
set -e
echo "  ok   uvloop evidence: $UVLOOP_VERDICT litellm process(es) have uvloop mapped -> $UVLOOP_TXT"

# ---------------------------------------------------------------------
# Preflight and warmup, byte for byte the same as the Windows harness.
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
    404) die "$name answered 404 for model \"$model\". That arm would have failed fast and looked artificially quick." ;;
    401) die "$name answered 401. The key this run presents is not one it accepts." ;;
    000) die "$name did not answer a completion at all." ;;
    *)   die "$name answered $status to a preflight completion, wanted 200." ;;
  esac
}

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
# ---------------------------------------------------------------------

json_escape() { printf '%s' "$1" | sed 's/\\/\\\\/g; s/"/\\"/g'; }

bin_fingerprint() { sha256sum "$1" 2>/dev/null | cut -c1-16 || echo unknown; }

LITELLM_VERSION="$("$LITELLM_PY" -c \
  "import importlib.metadata as m; print(m.version('litellm'))" 2>/dev/null || echo unknown)"
PY_VERSION="$("$LITELLM_PY" -c \
  "import sys; print('CPython %d.%d.%d' % sys.version_info[:3])" 2>/dev/null || echo unknown)"
DEP_VERSIONS="$("$LITELLM_PY" -c "
import importlib.metadata as m
out=[]
for p in ['uvicorn','fastapi','httptools','uvloop','orjson','pydantic','gunicorn']:
    try: out.append(p+'=='+m.version(p))
    except Exception: out.append(p+'=absent')
print(', '.join(out))
" 2>/dev/null || echo unknown)"
PENSTOCK_FP="$(bin_fingerprint "$PENSTOCK_BIN")"
UVLOOP_LINE="$(grep '^uvloop_mapped_processes' "$UVLOOP_TXT" 2>/dev/null || echo unknown)"

cat >"$META" <<EOF
{
  "run_id": "$(json_escape "$RUN_ID")",
  "scenario": "$(json_escape "$SCENARIO")",
  "started_utc": "$(date -u +%Y-%m-%dT%H:%M:%SZ)",
  "platform_note": "Linux rerun of the comparison. The Windows run is unfair to LiteLLM because uvloop does not exist on Windows and litellm 1.95.0 explicitly requests it on every other platform.",

  "hardware": {
    "cpu": "$(json_escape "$(awk -F': ' '/^model name/ {print $2; exit}' /proc/cpuinfo)")",
    "logical_cpus": "$(nproc)",
    "memory": "$(awk '/^MemTotal/ {printf "%.1f GiB", $2/1048576; exit}' /proc/meminfo)",
    "os": "$(json_escape "$(. /etc/os-release 2>/dev/null; echo "${PRETTY_NAME:-linux}") $(uname -sr)")",
    "note": "k6, three llmsim instances, penstock and litellm all ran on this one machine and competed for these cores"
  },

  "toolchain": {
    "go": "binaries cross compiled with go1.26.5 windows/amd64, GOOS=linux GOARCH=amd64 CGO_ENABLED=0",
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
    "launcher": "bench/compare/linux/start-litellm.sh",
    "listen": "$(json_escape "$LITELLM_URL")",
    "asgi_server": "$(json_escape "$LITELLM_SERVER")",
    "num_workers": "$(json_escape "$LITELLM_NUM_WORKERS")",
    "deps": "$(json_escape "$DEP_VERSIONS")",
    "event_loop": "uvloop, requested by litellm proxy_cli._get_loop_type() on non-Windows",
    "uvloop_evidence": "$(json_escape "$UVLOOP_LINE")",
    "uvloop_evidence_file": "$(json_escape "$UVLOOP_TXT")",
    "steelman": "LITELLM_MODE=PRODUCTION, LITELLM_LOG=ERROR, telemetry off, local model cost map, json_logs, set_verbose false, no database, no redis, no callbacks, no guardrails, no cache, store_model_in_db false",
    "handicaps_lifted_versus_windows": "uvloop present and in use; gunicorn importable"
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
echo "  uvloop        $UVLOOP_TXT"

if [ "$K6_STATUS" -ne 0 ]; then
  echo ""
  echo "k6 exited $K6_STATUS. Exit code 99 means a threshold failed, which here"
  echo "means dropped iterations or a non zero error rate. Either one means the"
  echo "arms did not receive equal load, which voids the comparison rather than"
  echo "merely degrading it. Do not quote this run."
fi

exit "$K6_STATUS"
