#!/usr/bin/env bash
#
# calibrate-litellm.sh - find the --num_workers value LiteLLM is
# fastest at on this machine, under this benchmark's own load.
#
# WHY THIS EXISTS
#
# "You gave LiteLLM the wrong worker count" is the first and best
# objection to a comparison like this one, and it cannot be answered by
# asserting that a reasonable value was chosen. LiteLLM's production
# guide gives two different instructions (one worker per pod on
# Kubernetes; workers equal to vCPU count on a single VM) and neither
# fits a 16 core laptop that is simultaneously running the load
# generator, three simulators and a second gateway. Setting 16 workers
# here would oversubscribe the machine and make LiteLLM look worse.
#
# So the value is swept and measured, and the sweep is committed. If
# the number picked for the published run is not the winner of this
# sweep, that is visible.
#
# Usage:
#   bash bench/compare/calibrate-litellm.sh
#   WORKER_COUNTS="1 2 4 8" DURATION=20s bash bench/compare/calibrate-litellm.sh
#
# Output goes to bench/results/compare-calibration-<utc>.txt

set -euo pipefail

COMPARE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$COMPARE_DIR/../.." && pwd)"
cd "$REPO_ROOT"

BIN_EXT=""
IS_WINDOWS=0
case "$(uname -s 2>/dev/null || echo unknown)" in
  MINGW* | MSYS* | CYGWIN*) BIN_EXT=".exe"; IS_WINDOWS=1 ;;
esac

: "${LITELLM_VENV:=$HOME/sdk/litellm-venv}"
: "${WORKER_COUNTS:=1 2 4 8}"
: "${DURATION:=20s}"
: "${RATE:=20}"
: "${TIME_SCALE:=0.05}"
: "${SEED:=1}"
: "${WARMUP_REQUESTS:=30}"
: "${BENCH_PROFILE:=bench/profiles/groq.json}"
: "${BENCH_MODEL:=llmsim-small}"
: "${BENCH_API_KEY:=benchbenchbenchbenchbench}"
: "${LITELLM_SIM_PORT:=8091}"
: "${LITELLM_PORT:=8081}"

RESULTS_DIR="bench/results"
mkdir -p "$RESULTS_DIR"
OUT="${RESULTS_DIR}/compare-calibration-$(date -u +%Y%m%dT%H%M%SZ).txt"

LLMSIM_BIN="bin/llmsim${BIN_EXT}"
go build -o "$LLMSIM_BIN" ./cmd/llmsim

PROFILE_ARGS=""
[ -f "$BENCH_PROFILE" ] && PROFILE_ARGS="--profile $BENCH_PROFILE"

PIDS=""
kill_tree() {
  local pid="$1" winpid
  if [ "$IS_WINDOWS" = "1" ] && command -v taskkill >/dev/null 2>&1; then
    winpid="$(ps -p "$pid" 2>/dev/null | awk -v p="$pid" '$1 == p {print $4; exit}' || true)"
    [ -n "$winpid" ] && taskkill //PID "$winpid" //T //F >/dev/null 2>&1 || true
  fi
  kill "$pid" >/dev/null 2>&1 || true
}
cleanup() {
  local code=$?
  set +e
  for p in $PIDS; do kill_tree "$p"; done
  exit "$code"
}
trap cleanup EXIT INT TERM

{
  echo "LiteLLM --num_workers sweep"
  echo "started_utc  : $(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo "logical_cpus : ${NUMBER_OF_PROCESSORS:-$(nproc 2>/dev/null || echo unknown)}"
  echo "load         : ${RATE} req/s for ${DURATION}, constant arrival rate"
  echo "upstream     : llmsim seed=${SEED} time-scale=${TIME_SCALE}"
  echo "warmup       : ${WARMUP_REQUESTS} requests before each measured sweep point"
  echo ""
  echo "Everything else on this machine is competing for the same cores,"
  echo "so a high worker count can lose here and win on a dedicated box."
  echo ""
} | tee "$OUT"

for W in $WORKER_COUNTS; do
  echo "sweeping workers=$W" >&2

  # A fresh simulator per sweep point, so every point starts from the
  # same request index and sees the same planned upstream latencies.
  # shellcheck disable=SC2086
  "./$LLMSIM_BIN" --listen "127.0.0.1:${LITELLM_SIM_PORT}" --seed "$SEED" \
    --time-scale "$TIME_SCALE" $PROFILE_ARGS >/dev/null 2>&1 &
  SIM_PID=$!
  PIDS="$PIDS $SIM_PID"
  for i in $(seq 1 100); do
    curl -s --fail --max-time 2 "http://127.0.0.1:${LITELLM_SIM_PORT}/healthz" >/dev/null 2>&1 && break
    sleep 0.2
  done

  LITELLM_NUM_WORKERS="$W" LITELLM_PORT="$LITELLM_PORT" LITELLM_VENV="$LITELLM_VENV" \
    bash bench/compare/start-litellm.sh >/dev/null 2>&1 &
  LL_PID=$!
  PIDS="$PIDS $LL_PID"

  healthy=0
  for i in $(seq 1 400); do
    if curl -s --fail --max-time 2 "http://127.0.0.1:${LITELLM_PORT}/health/liveliness" >/dev/null 2>&1; then
      healthy=1; break
    fi
    sleep 0.25
  done
  if [ "$healthy" != "1" ]; then
    echo "workers=$W  FAILED TO START" | tee -a "$OUT"
    kill_tree "$LL_PID"; kill_tree "$SIM_PID"
    continue
  fi

  # Same warmup the real run gives every arm. A cold sweep point would
  # lose for a reason that has nothing to do with its worker count.
  i=0
  while [ "$i" -lt "$WARMUP_REQUESTS" ]; do
    curl -s -o /dev/null --max-time 30 \
      -H 'Content-Type: application/json' \
      -H "Authorization: Bearer ${BENCH_API_KEY}" \
      -d "{\"model\":\"${BENCH_MODEL}\",\"messages\":[{\"role\":\"user\",\"content\":\"warm $i\"}],\"stream\":false}" \
      "http://127.0.0.1:${LITELLM_PORT}/v1/chat/completions" || true
    i=$((i + 1))
  done

  LINE="$(k6 run --quiet \
    --env "TARGET_URL=http://127.0.0.1:${LITELLM_PORT}" \
    --env "MODEL=${BENCH_MODEL}" \
    --env "API_KEY=${BENCH_API_KEY}" \
    --env "RATE=${RATE}" \
    --env "DURATION=${DURATION}" \
    --env "LABEL=${W}" \
    bench/compare/calibrate_load.js 2>/dev/null | grep '^CALIBRATION' || echo "CALIBRATION workers=$W FAILED")"
  echo "$LINE" | tee -a "$OUT"

  kill_tree "$LL_PID"
  kill_tree "$SIM_PID"
  sleep 3
done

{
  echo ""
  echo "Lowest p50 and p95 wins. Put that value in LITELLM_NUM_WORKERS in"
  echo "bench/compare/start-litellm.sh and note it in docs/comparison.md."
} | tee -a "$OUT"

echo ""
echo "sweep written to $OUT"
