#!/usr/bin/env python3
"""Compact cross-cell ratio table: summarize3_compact.py LADDER.jsonl ARMS.json
Each cell: ops/s ratio, client CPU/op ratio, server kcycles/tx ratio, p99 ratio to arm A,
median of within-round ratios."""
import collections, json, statistics, sys
rows = [json.loads(l) for l in open(sys.argv[1])]
arms = list(json.load(open(sys.argv[2])))
cells = sorted({(r["workload"], r["workers"]) for r in rows}, key=lambda c: (c[0] != "get100", c[0], c[1]))
by = {(r["workload"], r["workers"], r["round"], r["arm"]): r for r in rows}
def ratio(c, a, k):
    q = [by[(*c, n, a)][k] / by[(*c, n, "A")][k] for n in range(10) if (*c, n, a) in by and (*c, n, "A") in by]
    return statistics.median(q) if q else None
print("| arm | " + " | ".join(f"{w}/{n}" for w, n in cells) + " |")
print("|---|" + "---|" * len(cells))
for a in arms:
    out = []
    for c in cells:
        v = [ratio(c, a, k) for k in ("ops_per_s", "client_cpu_us_per_op", "server_cycles_per_tx", "p99_us")]
        out.append("-" if v[0] is None else " / ".join(f"{x:.2f}" for x in v))
    print(f"| {a} | " + " | ".join(out) + " |")
