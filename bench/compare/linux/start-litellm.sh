#!/usr/bin/env bash
#
# bench/compare/linux/start-litellm.sh - the Linux launcher.
#
# This is bench/compare/start-litellm.sh with the Windows workarounds
# removed and the Windows handicaps lifted. Read that file first: every
# setting whose reason is unchanged is not re-argued here.
#
# WHAT IS THE SAME AS THE WINDOWS LAUNCHER
#
#   LITELLM_MODE=PRODUCTION          disables load_dotenv
#   LITELLM_LOG=ERROR                the version's default is DEBUG
#   LITELLM_DONT_SHOW_FEEDBACK_BOX   no telemetry
#   LITELLM_LOCAL_MODEL_COST_MAP     no pricing fetch from GitHub at import
#   --telemetry False
#   the same bench/compare/litellm.config.yaml: no db, no redis, no
#   callbacks, no guardrails, no cache, spend and error logs off,
#   store_model_in_db off, master key auth from memory
#   no --limit_concurrency, no --max_requests_before_restart,
#   no --detailed_debug
#
# WHAT IS DIFFERENT, AND WHY
#
#   PYTHONIOENCODING / PYTHONUTF8 are still exported but are now
#   redundant rather than load bearing. On Windows they were required:
#   LiteLLM's startup banner is not cp1252 encodable and the proxy died
#   during ASGI startup without them. Linux is UTF-8 already. They are
#   kept so that the two launchers differ in as few places as possible.
#
#   uvloop is available, and LiteLLM asks for it by name. litellm
#   1.95.0's proxy_cli.py contains:
#
#       def _get_loop_type():
#           if sys.platform in ("win32", "cygwin", "cli"):
#               return None      # let uvicorn choose the default loop
#           return "uvloop"
#
#   and then sets uvicorn_args["loop"] = loop_type. So on Linux LiteLLM
#   runs uvicorn on a libuv backed loop, and on Windows it ran on plain
#   asyncio. That is not an optional accelerator LiteLLM happened to
#   miss, it is a different branch inside LiteLLM's own launcher, and it
#   is the single reason this Linux rerun exists.
#   verify-uvloop.sh proves the running process loads it.
#
#   gunicorn is importable here. LiteLLM's own CLI calls --run_gunicorn
#   "better for managing multiple workers" and it cannot run on Windows
#   at all because it needs fcntl. LITELLM_SERVER=gunicorn selects it so
#   that "you never gave it its best multi-worker supervisor" has a
#   measurement rather than an excuse.

set -euo pipefail

: "${WORKDIR:=$HOME/penstock-bench}"
cd "$WORKDIR"

: "${LITELLM_VENV:=$HOME/sdk/litellm-venv-linux}"
: "${LITELLM_HOST:=127.0.0.1}"
: "${LITELLM_PORT:=8081}"
: "${LITELLM_CONFIG:=bench/compare/litellm.config.yaml}"

# The worker count is NOT defaulted from the Windows result.
#
# Windows measured 1 worker as roughly twice as fast as 8, and found 8
# unstable. Both of those may be Windows artifacts: Windows had no
# gunicorn, so uvicorn's own supervisor shared the listening socket, and
# multi-process socket sharing is exactly where Windows and Linux differ
# most. Assuming 1 here would import a Windows finding into a Linux run
# and would be the same error the Windows page already made once, only
# with the platforms swapped.
#
# So the caller sweeps it, with bench/compare/linux/sweep-workers.sh,
# and the sweep runs the FULL three arm comparison at each setting
# rather than LiteLLM in isolation. That is the one lesson the Windows
# run paid for: an isolated sweep inverted the answer.
: "${LITELLM_NUM_WORKERS:=1}"

export LITELLM_MODE="PRODUCTION"
export LITELLM_LOG="ERROR"
export LITELLM_DONT_SHOW_FEEDBACK_BOX="True"
export LITELLM_LOCAL_MODEL_COST_MAP="True"
export PYTHONIOENCODING="utf-8"
export PYTHONUTF8="1"

: "${LITELLM_SERVER:=uvicorn}"
SERVER_ARGS=""
case "$LITELLM_SERVER" in
  uvicorn)  SERVER_ARGS="" ;;
  granian)  SERVER_ARGS="--run_granian" ;;
  gunicorn) SERVER_ARGS="--run_gunicorn" ;;
  *) echo "unknown LITELLM_SERVER=$LITELLM_SERVER" >&2; exit 1 ;;
esac

# shellcheck disable=SC2086
exec "${LITELLM_VENV}/bin/litellm" \
  --config "$LITELLM_CONFIG" \
  --host "$LITELLM_HOST" \
  --port "$LITELLM_PORT" \
  --num_workers "$LITELLM_NUM_WORKERS" \
  --telemetry False $SERVER_ARGS
