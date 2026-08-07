#!/usr/bin/env bash
#
# bench/compare/linux/sweep-workers.sh - choose LiteLLM's worker count
# by measuring it in the environment the comparison actually runs in.
#
# THIS SCRIPT EXISTS BECAUSE THE WINDOWS RUN GOT THIS WRONG ONCE.
#
# On Windows the worker count was first swept by
# bench/compare/calibrate-litellm.sh, which starts LiteLLM and one
# simulator and nothing else. That sweep said the worker count did not
# matter, so 8 was chosen. In the real four arm run, with k6, three
# simulators and Penstock also resident, 8 workers cost LiteLLM roughly
# twice what 1 worker cost, and produced request hangs. Publishing the
# isolated sweep's choice would have overstated LiteLLM's overhead by
# about 2x, entirely in Penstock's favour.
#
# So this script does not sweep LiteLLM alone. Every point in the sweep
# is a COMPLETE four arm run: baseline, Penstock, LiteLLM, baseline
# again, with the full process population on the machine, the drift
# check on and the same thresholds armed. It is slower by a large factor
# and it is the only version of the measurement that has been shown to
# give the right answer.
#
# The Windows answer (1 worker) is NOT assumed here. Windows had no
# gunicorn, so uvicorn's own supervisor shared the listening socket, and
# multi-process socket sharing is exactly the area where the two
# platforms differ most. Importing the Windows conclusion would repeat
# the original error with the platforms swapped.
#
# Usage:
#   bash bench/compare/linux/sweep-workers.sh
#   CONFIGS="uvicorn:1 uvicorn:2 gunicorn:4" bash .../sweep-workers.sh

set -uo pipefail

: "${WORKDIR:=$HOME/penstock-bench}"
cd "$WORKDIR"

: "${CONFIGS:=uvicorn:1 uvicorn:2 uvicorn:4 uvicorn:8}"
: "${DURATION:=120s}"
: "${RATE:=20}"
: "${COOLDOWN:=20}"

LEDGER="bench/results/compare-linux-sweep.txt"
mkdir -p bench/results

{
  echo "======================================================================"
  echo "LiteLLM worker sweep, measured inside the full four arm comparison"
  echo "started   $(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo "configs   $CONFIGS"
  echo "load      $RATE req/s, $DURATION per arm, 4 arms per point"
  echo "======================================================================"
} | tee -a "$LEDGER"

for cfg in $CONFIGS; do
  server="${cfg%%:*}"
  workers="${cfg##*:}"
  stamp="$(date -u +%Y%m%dT%H%M%SZ)"
  run_id="compare-linux-${server}-w${workers}-${stamp}"

  echo "" | tee -a "$LEDGER"
  echo "---- $server, $workers worker(s)  ->  $run_id" | tee -a "$LEDGER"

  RUN_ID="$run_id" LITELLM_SERVER="$server" LITELLM_NUM_WORKERS="$workers" \
    DURATION="$DURATION" RATE="$RATE" \
    bash bench/compare/linux/run.sh >"bench/results/${run_id}.console.log" 2>&1
  status=$?

  sum="bench/results/${run_id}.summary.txt"
  if [ -f "$sum" ]; then
    grep -E '^(p50|p95|p99|mean|samples) ' "$sum" | tee -a "$LEDGER"
    grep -E '^drift ' "$sum" | tee -a "$LEDGER"
  fi
  {
    echo "k6_exit   $status  (99 = a threshold failed: dropped iterations or errors; run is void)"
    grep -E '^uvloop_mapped_processes|^VERDICT' "bench/results/${run_id}.uvloop.txt" 2>/dev/null
  } | tee -a "$LEDGER"

  # Let the machine settle and the ports drain before the next point.
  sleep "$COOLDOWN"
done

echo "" | tee -a "$LEDGER"
echo "sweep finished $(date -u +%Y-%m-%dT%H:%M:%SZ)" | tee -a "$LEDGER"
echo "ledger: $WORKDIR/$LEDGER"
