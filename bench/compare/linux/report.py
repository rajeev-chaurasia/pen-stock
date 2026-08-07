#!/usr/bin/env python3
#
# bench/compare/linux/report.py - turn committed run artifacts into the
# tables docs/comparison.md publishes.
#
# It reads only <run>.summary.json, which k6 wrote, and prints:
#
#   - the three arm table with both deltas
#   - the null comparison (arm 1 direct against arm 4 direct), which is
#     this harness's noise floor
#   - each delta expressed as a multiple of that floor
#
# The null comparison is the reason this script exists rather than a
# reader eyeballing the summary text. arm 1 and arm 4 measure the
# identical thing with the gateway arms in between, so their difference
# is pure measurement noise, and any delta smaller than it means
# nothing. The Windows page reports Penstock's p95 and p99 deltas as
# below the floor on exactly this basis.
#
# Usage:
#   python3 bench/compare/linux/report.py bench/results/compare-linux-*.summary.json

import json
import sys
import glob

STATS = [("p50", "p(50)"), ("p95", "p(95)"), ("p99", "p(99)"), ("mean", "avg")]


def load(path):
    with open(path) as fh:
        return json.load(fh)["metrics"]


def val(metrics, name, key):
    m = metrics.get(name)
    if not m:
        return None
    return m.get("values", {}).get(key)


def fmt(v):
    return "n/a" if v is None else "%.2f ms" % v


def main(paths):
    for path in paths:
        m = load(path)
        run = path.split("/")[-1].replace(".summary.json", "")
        d_n = val(m, "direct_latency", "count")
        p_n = val(m, "penstock_latency", "count")
        l_n = val(m, "litellm_latency", "count")
        print("=" * 78)
        print(run)
        print("samples  direct=%s penstock=%s litellm=%s" % (d_n, p_n, l_n))
        print("")
        print("%-6s %12s %12s %12s %12s %12s" %
              ("", "direct", "penstock", "delta", "litellm", "delta"))
        for label, key in STATS:
            d = val(m, "direct_latency", key)
            p = val(m, "penstock_latency", key)
            l = val(m, "litellm_latency", key)
            dp = None if d is None or p is None else p - d
            dl = None if d is None or l is None else l - d
            print("%-6s %12s %12s %12s %12s %12s" %
                  (label, fmt(d), fmt(p), fmt(dp), fmt(l), fmt(dl)))
        print("")
        print("null comparison (arm 1 direct vs arm 4 direct) = the noise floor")
        print("%-6s %12s %12s %12s" % ("", "arm 1", "arm 4", "null delta"))
        for label, key in STATS:
            a = val(m, "direct_latency", key)
            b = val(m, "direct_recheck_latency", key)
            n = None if a is None or b is None else abs(b - a)
            print("%-6s %12s %12s %12s" % (label, fmt(a), fmt(b), fmt(n)))
        print("")
        print("deltas against the floor")
        print("%-6s %12s %12s %10s %12s %10s" %
              ("", "floor", "penstock", "vs floor", "litellm", "vs floor"))
        for label, key in STATS:
            d = val(m, "direct_latency", key)
            p = val(m, "penstock_latency", key)
            l = val(m, "litellm_latency", key)
            a = val(m, "direct_latency", key)
            b = val(m, "direct_recheck_latency", key)
            if None in (d, p, l, a, b):
                continue
            floor = abs(b - a)
            dp, dl = p - d, l - d
            def ratio(delta):
                if floor == 0:
                    return "inf"
                r = delta / floor
                return "below" if r < 1 else "%.1fx" % r
            print("%-6s %12s %12s %10s %12s %10s" %
                  (label, fmt(floor), fmt(dp), ratio(dp), fmt(dl), ratio(dl)))
        print("")
        print("min      direct=%s penstock=%s litellm=%s" % (
            fmt(val(m, "direct_latency", "min")),
            fmt(val(m, "penstock_latency", "min")),
            fmt(val(m, "litellm_latency", "min"))))
        print("max      direct=%s penstock=%s litellm=%s" % (
            fmt(val(m, "direct_latency", "max")),
            fmt(val(m, "penstock_latency", "max")),
            fmt(val(m, "litellm_latency", "max"))))
        failed = m.get("http_req_failed", {}).get("values", {})
        dropped = m.get("dropped_iterations", {}).get("values", {})
        print("guards   http_req_failed_rate=%s dropped_iterations=%s" % (
            failed.get("rate"), dropped.get("count", 0)))
        print("")


if __name__ == "__main__":
    args = sys.argv[1:]
    paths = []
    for a in args:
        paths.extend(sorted(glob.glob(a)) or [a])
    main(paths)
