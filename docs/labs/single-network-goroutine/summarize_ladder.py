#!/usr/bin/env python3
"""Lever ladder tables from raw2/ladder.jsonl.

Per cell and arm: median [min, max] over rounds of each metric, and the median
within-round ratio to arm A. "srv cores" is server task-clock per tx times ops/s:
how many of the three server cores were busy.
"""

import collections
import json
import statistics
import sys

PATH = sys.argv[1] if len(sys.argv) > 1 else "raw2/ladder.jsonl"
ARMS_FILE = sys.argv[2] if len(sys.argv) > 2 else "ladder_arms.json"
arms = list(json.load(open(ARMS_FILE)))
rows = [json.loads(l) for l in open(PATH)]
for r in rows:
    r["server_kcycles"] = r["server_cycles_per_tx"] / 1e3
    r["server_cores"] = r["server_task_clock_us_per_tx"] * r["ops_per_s"] / 1e6
cells = collections.defaultdict(list)
for r in rows:
    cells[(r["workload"], r["workers"])].append(r)

KEYS = [("ops_per_s", 0), ("client_cpu_us_per_op", 1), ("server_kcycles", 1), ("p50_us", 0), ("p99_us", 0)]


def med(v, d):
    return f"{statistics.median(v):,.{d}f} [{min(v):,.{d}f}, {max(v):,.{d}f}]"


for (wl, w), cell in cells.items():
    loads = [r["load1_before_long"] for r in cell]
    rounds = sorted({r["round"] for r in cell})
    print(f"\n#### {wl}, {w} workers ({len(rounds)} rounds; load1 {min(loads):.1f}–{max(loads):.1f})\n")
    print("| arm | ops/s | client CPU µs/op | server kcycles/tx | p50 µs | p99 µs | ratios vs A: ops, CPU, server, p50, p99 | client / srv cores | err/retry | clean runs, foreign cores |")
    print("|---|---:|---:|---:|---:|---:|---|---|---|---|")
    by = {(r["round"], r["arm"]): r for r in cell}
    for a in arms:
        rs = [r for r in cell if r["arm"] == a]
        if not rs:
            continue
        cols = [med([r[k] for r in rs], d) for k, d in KEYS]
        ratios = []
        for k, _ in KEYS:
            q = [by[(n, a)][k] / by[(n, "A")][k] for n in rounds if (n, a) in by and (n, "A") in by]
            ratios.append(f"{statistics.median(q):.3f}" if q else "-")
        cores = f"{statistics.median(r['client_cores_busy'] for r in rs):.2f} / {statistics.median(r['server_cores'] for r in rs):.2f}"
        er = f"{sum(r['errors'] for r in rs)}/{sum(r['retries'] for r in rs)}"
        fc = [r["foreign_cores"] for r in rs if "foreign_cores" in r]
        cl = f"{sum(1 for r in rs if r.get('clean', True))}/{len(rs)}, {statistics.median(fc):.1f}" if fc else "-"
        fpw = [r["lab_frames_per_write"] for r in rs if "lab_frames_per_write" in r]
        cl += f", {statistics.median(fpw):.1f} frames/write" if fpw else ""
        print(f"| {a} | " + " | ".join(cols) + f" | {', '.join(ratios)} | {cores} | {er} | {cl} |")
