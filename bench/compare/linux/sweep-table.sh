#!/usr/bin/env bash
#
# bench/compare/linux/sweep-table.sh - one line per committed Linux run.
#
# Prints the LiteLLM and Penstock mean deltas, the run's own noise floor,
# the drift check, the guard results and the uvloop evidence, so the
# whole sweep can be read at once and the published choice checked
# against every alternative that was measured.

set -uo pipefail
: "${WORKDIR:=$HOME/penstock-bench}"
cd "$WORKDIR/bench/results"

printf '%-46s %10s %10s %8s %7s %8s %6s %s\n' \
  run litellm penstock floor drift dropped failed uvloop
printf '%s\n' "--------------------------------------------------------------------------------------------------------"

for f in compare-linux-*.summary.json; do
  r="${f%.summary.json}"
  read -r ll pp floor dropped failed <<EOF
$(python3 - "$f" <<'PYEOF'
import json, sys
m = json.load(open(sys.argv[1]))["metrics"]
def v(n, k):
    return m.get(n, {}).get("values", {}).get(k)
d, p, l = v("direct_latency","avg"), v("penstock_latency","avg"), v("litellm_latency","avg")
b = v("direct_recheck_latency","avg")
print("%.2f %.2f %.2f %s %s" % (
    l-d, p-d, abs(b-d),
    m.get("dropped_iterations",{}).get("values",{}).get("count",0),
    m.get("http_req_failed",{}).get("values",{}).get("rate",0)))
PYEOF
)
EOF
  drift="$(grep -E '^drift +:' "$r.summary.txt" 2>/dev/null | awk '{print $NF}')"
  uv="$(grep '^uvloop_mapped_processes' "$r.uvloop.txt" 2>/dev/null | sed 's/uvloop_mapped_processes  *//')"
  printf '%-46s %10s %10s %8s %7s %8s %6s %s\n' \
    "$r" "+$ll" "+$pp" "$floor" "$drift" "$dropped" "$failed" "$uv"
done
