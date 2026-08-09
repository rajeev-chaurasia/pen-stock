#!/usr/bin/env bash
#
# bench/compare/linux/verify-uvloop.sh - prove uvloop is actually in use.
#
# The entire reason for the Linux rerun is that LiteLLM cannot use uvloop
# on Windows. "We installed uvloop" is not the same claim as "LiteLLM ran
# on uvloop", and publishing the second while only having checked the
# first would repeat, in the opposite direction, exactly the kind of
# error docs/comparison.md exists to avoid.
#
# So six checks, in increasing order of how hard they are to fake:
#
#   1. uvloop imports, and reports a version.
#   2. uvicorn's loop="auto" resolver, in THIS venv, selects uvloop.
#   3. a loop built the way uvicorn builds it is a uvloop.Loop.
#   4. litellm's own CLI asks for uvloop by name on this platform.
#   5. the ACTUALLY RUNNING LiteLLM process has uvloop's compiled
#      extension mapped into its address space, read from /proc/<pid>/maps.
#   6. a control process, which should NOT have uvloop mapped, does not.
#
# Check 5 is the one that matters, and check 6 is what makes check 5
# worth anything: a probe that says yes to everything proves nothing.
# Check 5 is not a statement about what the library could do, it is a
# statement about what the process under measurement did do.

set -euo pipefail

: "${LITELLM_VENV:=$HOME/sdk/litellm-venv-linux}"
: "${WORKDIR:=$HOME/penstock-bench}"
: "${LITELLM_PORT:=8098}"
: "${LITELLM_SIM_PORT:=8091}"
: "${OUT:=$WORKDIR/bench/results/uvloop-evidence.txt}"

PY="$LITELLM_VENV/bin/python"
cd "$WORKDIR"
mkdir -p "$(dirname "$OUT")"

# The home directory of whoever captured the run prints as $HOME. Paths
# are the evidence here, so what matters is that the interpreter and the
# mapped .so share a prefix, not whose account they sat under.
exec > >(sed "s|$HOME|\$HOME|g" | tee "$OUT") 2>&1

echo "======================================================================"
echo "uvloop evidence"
echo "date        $(date -u +%Y-%m-%dT%H:%M:%SZ)"
echo "venv        $LITELLM_VENV"
echo "kernel      $(uname -srm)"
echo "======================================================================"
echo ""

echo "--- 1. versions -------------------------------------------------"
"$PY" - <<'PYEOF'
import importlib.metadata as m, sys
print("python      CPython %d.%d.%d" % sys.version_info[:3])
for p in ["litellm","uvicorn","fastapi","httptools","uvloop","orjson",
          "pydantic","gunicorn"]:
    try:
        print("%-11s %s" % (p, m.version(p)))
    except Exception:
        print("%-11s absent" % p)
import uvloop
print("uvloop import OK, uvloop.__file__ =", uvloop.__file__)
PYEOF
echo ""

echo "--- 2. uvicorn loop=auto resolves to what? ----------------------"
"$PY" - <<'PYEOF'
import inspect, uvicorn.loops.auto as auto
from uvicorn.config import LOOP_FACTORIES
print("uvicorn LOOP_FACTORIES['auto'] =", LOOP_FACTORIES["auto"])
print("--- source of uvicorn/loops/auto.py ---")
print(inspect.getsource(auto).strip())
PYEOF
echo ""

echo "--- 3. the loop uvicorn would build, built --------------------"
"$PY" - <<'PYEOF'
from uvicorn.config import Config
# uvicorn 0.52.1 replaced setup_event_loop() with get_loop_factory().
# This is exactly the factory uvicorn.Server hands to asyncio.Runner.
cfg = Config(app="x:y", loop="auto")
factory = cfg.get_loop_factory()
print("config.loop            ->", cfg.loop)
print("get_loop_factory()     ->", factory)
loop = factory()
name = type(loop).__module__ + "." + type(loop).__qualname__
print("factory() built        ->", name)
print("is a uvloop loop       ->", type(loop).__module__.startswith("uvloop"))
loop.close()
PYEOF
echo ""

echo "--- 4. litellm does not merely ALLOW uvloop, it asks for it -----"
# This is the strongest static evidence available and it is in LiteLLM's
# own CLI, not in uvicorn. litellm 1.95.0 branches on the platform and
# passes loop="uvloop" to uvicorn everywhere except Windows, where it
# passes nothing and uvicorn falls back to plain asyncio. The Windows run
# was therefore not merely missing an optional accelerator: it was taking
# a different code path inside LiteLLM.
sed -n '/def _get_loop_type/,/return "uvloop"/p' \
  "$LITELLM_VENV"/lib/python3.12/site-packages/litellm/proxy/proxy_cli.py
echo "  ... and the call site:"
grep -n -A2 '_get_loop_type()' \
  "$LITELLM_VENV"/lib/python3.12/site-packages/litellm/proxy/proxy_cli.py | tail -4
echo ""
"$PY" - <<'PYEOF'
from litellm.proxy.proxy_cli import ProxyInitializationHelpers as H
print("ProxyInitializationHelpers._get_loop_type() on this platform ->",
      repr(H._get_loop_type()))
PYEOF
echo ""

echo "--- 5. RUNNING proxy: is uvloop mapped into the process? --------"
export LITELLM_MODE=PRODUCTION LITELLM_LOG=ERROR \
       LITELLM_DONT_SHOW_FEEDBACK_BOX=True LITELLM_LOCAL_MODEL_COST_MAP=True

"$WORKDIR/bin/llmsim" --listen "127.0.0.1:${LITELLM_SIM_PORT}" --seed 1 \
  --time-scale 0.05 --profile bench/profiles/groq.json >/tmp/uvloop-sim.log 2>&1 &
SIM_PID=$!
"$LITELLM_VENV/bin/litellm" --config bench/compare/litellm.config.yaml \
  --host 127.0.0.1 --port "$LITELLM_PORT" --num_workers 1 \
  --telemetry False >/tmp/uvloop-litellm.log 2>&1 &
LL_PID=$!
trap 'kill $SIM_PID $LL_PID 2>/dev/null || true; pkill -P $LL_PID 2>/dev/null || true' EXIT

for i in $(seq 1 200); do
  curl -sf --max-time 2 "http://127.0.0.1:${LITELLM_PORT}/health/liveliness" >/dev/null 2>&1 && break
  sleep 0.3
done
sleep 3

# A real completion first, so the loop has definitely run request code.
code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 30 \
  -H 'Content-Type: application/json' \
  -H 'Authorization: Bearer benchbenchbenchbenchbench' \
  -d '{"model":"llmsim-small","messages":[{"role":"user","content":"uvloop check"}],"stream":false}' \
  "http://127.0.0.1:${LITELLM_PORT}/v1/chat/completions")
echo "live completion through the proxy: HTTP $code"

# Every python process holding port $LITELLM_PORT, plus the supervisor's
# children. ss gives the pid that owns the listening socket, which is the
# process that actually served the request above.
PIDS="$(ss -ltnp 2>/dev/null | grep ":${LITELLM_PORT} " | grep -o 'pid=[0-9]*' | cut -d= -f2 | sort -u)"
CHILDREN="$(pgrep -P "$LL_PID" 2>/dev/null || true)"
ALL="$(printf '%s\n%s\n%s\n' "$LL_PID" "$PIDS" "$CHILDREN" | tr ' ' '\n' | grep -E '^[0-9]+$' | sort -u)"
echo "litellm pids: $(echo $ALL)"
echo ""
found=0
for p in $ALL; do
  cmd="$(tr '\0' ' ' < /proc/$p/cmdline 2>/dev/null | cut -c1-90)"
  maps="$(grep -i uvloop /proc/$p/maps 2>/dev/null | awk '{print $NF}' | sort -u || true)"
  echo "pid $p  cmd: $cmd"
  if [ -n "$maps" ]; then
    echo "         UVLOOP MAPPED:"
    echo "$maps" | sed 's/^/           /'
    found=1
  else
    echo "         no uvloop mapping in this process"
  fi
done
echo ""
if [ "$found" = "1" ]; then
  echo "RESULT: uvloop IS loaded by the running LiteLLM proxy."
else
  echo "RESULT: uvloop was NOT found mapped in any LiteLLM process."
  echo "        Do not claim uvloop was active."
fi
echo ""
echo "--- 6. same probe against a deliberately uvloop-free process ----"
echo "(a control: if this ALSO showed uvloop, check 5 would prove nothing)"
"$PY" -c "
import time,os
print('control pid', os.getpid(), flush=True)
time.sleep(6)
" &
CTRL=$!
sleep 2
if grep -qi uvloop "/proc/$CTRL/maps" 2>/dev/null; then
  echo "control pid $CTRL: uvloop mapped -- the probe is not discriminating"
else
  echo "control pid $CTRL: no uvloop mapping, as expected. The probe discriminates."
fi
wait $CTRL 2>/dev/null || true
echo ""
echo "evidence written to $OUT"
