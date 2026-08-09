#!/usr/bin/env bash
#
# compare-servers.sh - measure LiteLLM under both ASGI servers it can
# run on this machine, so the published comparison uses whichever one
# LiteLLM is fastest at.
#
# WHY
#
# LiteLLM's production guide and its own Docker image use uvicorn, so
# uvicorn is the defensible default. But the CLI also offers
# --run_granian, a Rust ASGI server, and "you did not try their fast
# server" is a fair objection to a comparison that only ever ran the
# default. It costs one script to close, so here it is.
#
# gunicorn and hypercorn are not swept: gunicorn cannot run on Windows
# at all (it needs fcntl), which is recorded as a platform handicap in
# docs/comparison.md rather than worked around.
#
# Usage:
#   bash bench/compare/compare-servers.sh
#
# Output: bench/results/compare-servers-<utc>.txt

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
: "${SERVERS:=uvicorn granian}"
: "${DURATION:=20s}"
: "${RATE:=20}"
: "${TIME_SCALE:=0.05}"
: "${SEED:=1}"
: "${WARMUP_REQUESTS:=30}"
: "${LITELLM_NUM_WORKERS:=8}"
: "${BENCH_PROFILE:=bench/profiles/groq.json}"
: "${BENCH_MODEL:=llmsim-small}"
: "${BENCH_API_KEY:=benchbenchbenchbenchbench}"
: "${LITELLM_SIM_PORT:=8091}"
: "${LITELLM_PORT:=8081}"

RESULTS_DIR="bench/results"
mkdir -p "$RESULTS_DIR"
OUT="${RESULTS_DIR}/compare-servers-$(date -u +%Y%m%dT%H%M%SZ).txt"

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
  echo "LiteLLM ASGI server comparison"
  echo "started_utc : $(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo "workers     : ${LITELLM_NUM_WORKERS}"
  echo "load        : ${RATE} req/s for ${DURATION}, constant arrival rate"
  echo "upstream    : llmsim seed=${SEED} time-scale=${TIME_SCALE}"
  echo ""
} | tee "$OUT"

for S in $SERVERS; do
  echo "trying server=$S" >&2

  # shellcheck disable=SC2086
  "./$LLMSIM_BIN" --listen "127.0.0.1:${LITELLM_SIM_PORT}" --seed "$SEED" \
    --time-scale "$TIME_SCALE" $PROFILE_ARGS >/dev/null 2>&1 &
  SIM_PID=$!
  PIDS="$PIDS $SIM_PID"
  for i in $(seq 1 100); do
    curl -s --fail --max-time 2 "http://127.0.0.1:${LITELLM_SIM_PORT}/healthz" >/dev/null 2>&1 && break
    sleep 0.2
  done

  LITELLM_SERVER="$S" LITELLM_NUM_WORKERS="$LITELLM_NUM_WORKERS" \
    LITELLM_PORT="$LITELLM_PORT" LITELLM_VENV="$LITELLM_VENV" \
    bash bench/compare/start-litellm.sh >"${RESULTS_DIR}/litellm-server-${S}.log" 2>&1 &
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
    echo "server=$S  FAILED TO START (see ${RESULTS_DIR}/litellm-server-${S}.log)" | tee -a "$OUT"
    kill_tree "$LL_PID"; kill_tree "$SIM_PID"; sleep 3
    continue
  fi

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
    --env "LABEL=${S}" \
    bench/compare/calibrate_load.js 2>/dev/null | grep '^CALIBRATION' || echo "CALIBRATION workers=$S FAILED")"
  # The scenario labels its output "workers=" because it is shared with
  # the worker sweep; here that field carries the server name.
  echo "$LINE" | sed 's/workers=/server=/' | tee -a "$OUT"

  kill_tree "$LL_PID"
  kill_tree "$SIM_PID"
  sleep 3
done

{
  echo ""
  echo "Lowest mean and p50 wins. Whichever that is belongs in"
  echo "LITELLM_SERVER in bench/compare/start-litellm.sh, and the result"
  echo "belongs in docs/comparison.md whether or not it favours Penstock."
} | tee -a "$OUT"

echo ""
echo "written to $OUT"
