#!/usr/bin/env bash
#
# bench/compare/linux/collect.sh - copy the Linux run artifacts out of
# the Linux working tree and into the repository.
#
# What is copied, and why exactly this set:
#
#   .raw.json.gz    every per sample measurement. Committed on purpose:
#                   a summary is a set of choices about which statistics
#                   to show, and a reader who distrusts those choices
#                   has to be able to recompute from the same data.
#   .summary.json   what k6 computed, including the drift arm, which is
#                   where the noise floor comes from.
#   .summary.txt    the human readable table.
#   .meta.json      hardware, versions, every load knob. A latency
#                   figure without this is not a result.
#   .uvloop.txt     the uvloop evidence for the processes THAT run
#                   measured, which is the whole point of the rerun.
#
# Server logs are deliberately NOT copied. bench/README.md classes them
# as debugging aids rather than evidence, and they are gitignored.

set -euo pipefail

: "${WORKDIR:=$HOME/penstock-bench}"
# REPO is the checkout to copy results back into, defaulting to the one
# this script lives in.
script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
: "${REPO:=$(git -C "$script_dir" rev-parse --show-toplevel 2>/dev/null || (cd "$script_dir/../../.." && pwd))}"

SRC="$WORKDIR/bench/results"
DST="$REPO/bench/results"
mkdir -p "$DST"

n=0
for ext in raw.json.gz summary.json summary.txt meta.json uvloop.txt; do
  for f in "$SRC"/compare-linux-*."$ext"; do
    [ -e "$f" ] || continue
    cp "$f" "$DST/"
    n=$((n + 1))
  done
done
cp "$SRC/compare-linux-sweep.txt" "$DST/" && n=$((n + 1))
[ -f "$SRC/uvloop-evidence.txt" ] && \
  cp "$SRC/uvloop-evidence.txt" "$DST/compare-linux-uvloop-evidence.txt" && n=$((n + 1))

echo "copied $n files to $DST"
ls -1 "$DST"/compare-linux-* | sed 's|.*/|  |'
