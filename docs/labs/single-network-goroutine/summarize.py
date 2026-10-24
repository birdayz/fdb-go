#!/usr/bin/env python3
"""Markdown tables from raw/cells.jsonl and raw/syscalls.jsonl.

Per cell and arm: median [min, max] over repetitions. B/A is the median of the
within-pair ratios (slots 0-1, 2-3, ...), with its range.
"""

import collections
import json
import statistics
import sys

HERE = sys.argv[1] if len(sys.argv) > 1 else "raw"


def rows(name):
    try:
        return [json.loads(l) for l in open(f"{HERE}/{name}")]
    except FileNotFoundError:
        return []


def fmt(v, digits):
    return f"{v:,.{digits}f}"


def spread(vals, digits):
    return f"{fmt(statistics.median(vals), digits)} [{fmt(min(vals), digits)}, {fmt(max(vals), digits)}]"


def ratios(cell, key):
    by = {(r["slot"] // 2, r["arm"]): r[key] for r in cell}
    rs = [by[(p, "B")] / by[(p, "A")] for p in sorted({p for p, _ in by}) if (p, "A") in by and (p, "B") in by and by[(p, "A")]]
    return f"{statistics.median(rs):.3f} [{min(rs):.3f}, {max(rs):.3f}]" if rs else "-"


METRICS = [("ops_per_s", "ops/s", 0), ("p50_us", "p50 µs", 0), ("p99_us", "p99 µs", 0),
           ("client_cpu_us_per_op", "client CPU µs/op", 1), ("user_cpu_us_per_op", "client user µs/op", 1),
           ("sys_cpu_us_per_op", "client sys µs/op", 1), ("client_cores_busy", "client cores busy", 2),
           ("server_cycles_per_tx", "server kcycles/tx", 1)]

cells = collections.defaultdict(list)
for r in rows("cells.jsonl"):
    if "server_cycles_per_tx" in r:
        r["server_cycles_per_tx"] /= 1e3
    cells[(r["workload"], r["workers"])].append(r)
for (wl, w), cell in cells.items():
    loads = [r["load1_before_long"] for r in cell] + [r["load1_before_short"] for r in cell]
    n = collections.Counter(r["arm"] for r in cell)
    print(f"\n### {wl}, {w} workers (n = {n['A']} A, {n['B']} B; load1 before runs {min(loads):.1f}–{max(loads):.1f})\n")
    print("| metric | A: today | B: network goroutine | B/A (paired) |\n|---|---:|---:|---:|")
    for key, label, d in METRICS:
        a = [r[key] for r in cell if r["arm"] == "A"]
        b = [r[key] for r in cell if r["arm"] == "B"]
        print(f"| {label} | {spread(a, d)} | {spread(b, d)} | {ratios(cell, key)} |")
    errs = {arm: (sum(r["errors"] for r in cell if r["arm"] == arm), sum(r["retries"] for r in cell if r["arm"] == arm),
                  sum(r["completed"] for r in cell if r["arm"] == arm)) for arm in "AB"}
    print("| errors / retries (of completed) | " + " | ".join(f"{e} / {t} (of {c:,})" for e, t, c in errs.values()) + " | |")

sc = collections.defaultdict(list)
for r in rows("syscalls.jsonl"):
    sc[(r["workload"], r["workers"])].append(r)
if sc:
    keys = ["sys_enter_per_op", "futex_per_op", "epoll_pwait_per_op", "epoll_wait_per_op", "nanosleep_per_op",
            "read_per_op", "write_per_op", "timerfd_settime_per_op", "context-switches_per_op"]
    print("\n### Client syscalls per operation (median of 2 runs per arm, long-minus-short)\n")
    print("| cell | arm | " + " | ".join(k.replace("_per_op", "").replace("sys_enter", "all syscalls") for k in keys) + " |")
    print("|---|---|" + "---:|" * len(keys))
    for (wl, w), cell in sc.items():
        for arm in "AB":
            vals = [statistics.median(r[k] for r in cell if r["arm"] == arm) for k in keys]
            print(f"| {wl}/{w} | {arm} | " + " | ".join(f"{v:.2f}" for v in vals) + " |")
