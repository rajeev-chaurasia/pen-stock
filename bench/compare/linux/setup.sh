#!/usr/bin/env bash
#
# bench/compare/linux/setup.sh - build the Linux side of the comparison.
#
# WHY THIS DIRECTORY EXISTS
#
# docs/comparison.md's own list of ways the comparison could be unfair
# put one item above the others: LiteLLM cannot use uvloop on Windows.
# uvloop is a libuv backed replacement for asyncio's event loop and it
# is a real component of LiteLLM's performance, so the Windows number
# overstates LiteLLM's overhead by an unknown amount. Penstock has no
# equivalent component that Windows withholds, so the asymmetry is
# entirely LiteLLM's loss.
#
# This script builds the same comparison on Linux, where uvloop exists,
# so that LiteLLM gets every part it is designed to use.
#
# WHAT IS HELD FIXED ACROSS THE TWO PLATFORMS
#
#   same litellm version (pinned to 1.95.0, what Windows measured)
#   same fastapi pin (<0.140, a hard requirement, see ../INSTALL.md)
#   same httptools
#   same k6 version (v1.3.0)
#   same penstock source, same bench/config/gateway.yaml
#   same profile, seed, time scale, arrival rate, arm order, drift check
#
# WHAT DELIBERATELY DIFFERS, BECAUSE THAT IS THE POINT
#
#   uvloop present and used
#   gunicorn importable (it needs fcntl, which Windows does not have)
#   worker counts above 1 are worth re-testing, because the Windows
#   instability at 8 workers may have been a Windows artifact
#
# Usage, from inside WSL or any Linux box with uv on PATH:
#
#   bash bench/compare/linux/setup.sh
#
# It writes a self contained working tree at $WORKDIR (default
# ~/penstock-bench) because /mnt/c is slow enough to show up in a
# millisecond scale measurement.

set -euo pipefail

: "${REPO:=/mnt/c/Users/rchaurasia/Documents/projects/pen-stock}"
: "${WORKDIR:=$HOME/penstock-bench}"
: "${LITELLM_VENV:=$HOME/sdk/litellm-venv-linux}"
: "${LITELLM_VERSION:=1.95.0}"
: "${K6_VERSION:=v1.3.0}"

echo "repo     $REPO"
echo "workdir  $WORKDIR"
echo "venv     $LITELLM_VENV"
echo ""

# ---------------------------------------------------------------------
# 1. The working tree
#
# The Go binaries are cross compiled on the Windows side with
# GOOS=linux GOARCH=amd64 CGO_ENABLED=0 from the same source tree, so
# the Go side of the comparison is the same program on both platforms.
# ---------------------------------------------------------------------

mkdir -p "$WORKDIR/bench/scenarios/lib" "$WORKDIR/bench/config" \
         "$WORKDIR/bench/profiles" "$WORKDIR/bench/compare/linux" \
         "$WORKDIR/bench/results" "$WORKDIR/bin"

cp "$REPO"/bench/scenarios/*.js          "$WORKDIR/bench/scenarios/"
cp "$REPO"/bench/scenarios/lib/common.js "$WORKDIR/bench/scenarios/lib/"
cp "$REPO"/bench/config/*.yaml           "$WORKDIR/bench/config/"
cp "$REPO"/bench/profiles/*.json         "$WORKDIR/bench/profiles/"
cp "$REPO"/bench/compare/litellm.config.yaml "$WORKDIR/bench/compare/"
cp "$REPO"/bench/compare/linux/*.sh      "$WORKDIR/bench/compare/linux/"
cp "$REPO"/bench/compare/linux/bin/penstock "$WORKDIR/bin/penstock"
cp "$REPO"/bench/compare/linux/bin/llmsim   "$WORKDIR/bin/llmsim"
chmod +x "$WORKDIR/bin/penstock" "$WORKDIR/bin/llmsim"
echo "  ok   working tree at $WORKDIR"

# ---------------------------------------------------------------------
# 2. k6, same version as the Windows run
# ---------------------------------------------------------------------

if [ ! -x "$WORKDIR/bin/k6" ]; then
  tmp="$(mktemp -d)"
  curl -sSL -o "$tmp/k6.tar.gz" \
    "https://github.com/grafana/k6/releases/download/${K6_VERSION}/k6-${K6_VERSION}-linux-amd64.tar.gz"
  tar xzf "$tmp/k6.tar.gz" -C "$tmp"
  cp "$tmp/k6-${K6_VERSION}-linux-amd64/k6" "$WORKDIR/bin/k6"
  chmod +x "$WORKDIR/bin/k6"
  rm -rf "$tmp"
fi
echo "  ok   $("$WORKDIR/bin/k6" version)"

# ---------------------------------------------------------------------
# 3. LiteLLM, with uvloop
#
# Same three install facts as ../INSTALL.md, plus the one that could not
# hold on Windows:
#
#   'fastapi<0.140'  REQUIRED. litellm 1.95.0 imports get_flat_dependant,
#                    which fastapi removed in 0.140, so a plain install
#                    cannot import the proxy at all. Not a tuning knob.
#
#   httptools        installed on purpose. Without it uvicorn falls back
#                    to its pure Python h11 parser. This makes LiteLLM
#                    FASTER, which is the point.
#
#   uvloop           the whole reason for this rerun. On Windows
#                    `uv pip install uvloop` fails outright with
#                    "uvloop does not support Windows at the moment".
#                    Here it installs, and verify-uvloop.sh proves that
#                    the running proxy actually loaded it.
#
#   gunicorn         installed so that LiteLLM's --run_gunicorn path,
#                    which its own CLI calls "better for managing
#                    multiple workers" and which cannot exist on
#                    Windows, is available to be measured rather than
#                    excused.
#
# The litellm version is pinned to whatever Windows measured so that the
# two platforms differ in platform and not in program.
# ---------------------------------------------------------------------

if [ ! -x "$LITELLM_VENV/bin/litellm" ]; then
  uv venv --python 3.12 "$LITELLM_VENV"
  uv pip install --python "$LITELLM_VENV/bin/python" \
      "litellm[proxy]==${LITELLM_VERSION}" httptools 'fastapi<0.140' uvloop gunicorn
fi
echo "  ok   $LITELLM_VENV/bin/litellm"

echo ""
echo "next:"
echo "  bash $WORKDIR/bench/compare/linux/verify-uvloop.sh"
echo "  bash $WORKDIR/bench/compare/linux/run.sh"
