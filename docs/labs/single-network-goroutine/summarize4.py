#!/usr/bin/env python3
"""DESIGN.md benchmark tables from rawhz4/: per cell, every arm's medians and
spread, ratios to L (libfdb_c) and to A (today), frames per write (design
writer counter) and storage frames per client packet (tcpdump)."""

import collections
import glob
import json
import os
import statistics
import sys

D = sys.argv[1] if len(sys.argv) > 1 else "rawhz4"
ARMS = ["L", "L4", "A", "D", "DNG", "D4P", "M", "M4P"]
NAMES = {"L": "L (libfdb_c)", "L4": "L4 (libfdb_c, 4 threads)", "A": "A (today)", "D": "D", "DNG": "D-NG",
         "D4P": "D, 4 Ps", "M": "D-M", "M4P": "D-M, 4 Ps"}


def med(v, n=0):
    return f"{statistics.median(v):,.{n}f} [{min(v):,.{n}f}, {max(v):,.{n}f}]" if v else "-"


def ratio(by, cell, arm, base, key):
    q = [by[(*cell, r, arm)][key] / by[(*cell, r, base)][key] for r in range(10)
         if (*cell, r, arm) in by and (*cell, r, base) in by]
    return f"{statistics.median(q):.2f}" if q else "-"


def wire(cell):
    out = {}
    for f in glob.glob(f"{D}/wire-{cell[0]}-{cell[1]}/r*-wirestats.json"):
        arm = os.path.basename(f).split("-")[1]
        w = json.load(open(f))
        t = json.load(open(f.replace("-wirestats.json", "-wiretrace.json")))
        g = t["kinds"].get("grv", {})
        out[arm] = {
            "ss_fps": w.get("4753/c2s", {}).get("frames_per_segment"),
            "px_fps": w.get("4751/c2s", {}).get("frames_per_segment"),
            "ss_frames": w.get("4753/c2s", {}).get("frames_per_op"),
            "px_frames": w.get("4751/c2s", {}).get("frames_per_op"),
            "px_bytes": w.get("4751/c2s", {}).get("bytes_per_op"),
            "ss_bytes": w.get("4753/c2s", {}).get("bytes_per_op"),
            "grv_tpr": g["transactions"] / g["requests"] if g.get("requests") else None,
            "commits": t["kinds"].get("commit", {}).get("requests"),
            "flows": w["client_flows"],
        }
    return out


def table(path, title):
    if not os.path.exists(path):
        return
    rows = [json.loads(l) for l in open(path)]
    for r in rows:
        r["server_kcycles"] = r["server_cycles_per_tx"] / 1e3
    by = {(r["workload"], r["workers"], r["round"], r["arm"]): r for r in rows}
    cells = sorted({(r["workload"], r["workers"]) for r in rows}, key=lambda c: (c[0], c[1]))
    print(f"\n## {title}\n")
    for cell in cells:
        loads = [r["load1_before_long"] for r in rows if (r["workload"], r["workers"]) == cell]
        n = len({r["round"] for r in rows if (r["workload"], r["workers"]) == cell})
        print(f"\n### {cell[0]}, {cell[1]} workers ({n} rounds; load1 {min(loads):.1f}–{max(loads):.1f})\n")
        print("| arm | ops/s | client µs/op | server kc/tx | p50 µs | p99 µs | vs L: ops, CPU, srv, p50, p99 | vs A: ops, CPU, srv, p50, p99 | frames/write | storage frames/pkt |")
        print("|---|---:|---:|---:|---:|---:|---|---|---:|---:|")
        wv = wire(cell)
        for a in ARMS:
            rs = [r for r in rows if (r["workload"], r["workers"]) == cell and r["arm"] == a]
            if not rs:
                continue
            keys = [("ops_per_s", 0), ("client_cpu_us_per_op", 1), ("server_kcycles", 1), ("p50_us", 0), ("p99_us", 0)]
            cols = [med([r[k] for r in rs], d) for k, d in keys]
            vl = ", ".join(ratio(by, cell, a, "L", k) for k, _ in keys)
            va = ", ".join(ratio(by, cell, a, "A", k) for k, _ in keys)
            fpw = [r["lab_frames_per_write"] for r in rs if "lab_frames_per_write" in r]
            ss = wv.get(a, {}).get("ss_fps")
            print(f"| {NAMES[a]} | " + " | ".join(cols) + f" | {vl} | {va} | {med(fpw, 1) if fpw else '-'} | {ss:.1f} |" if ss else
                  f"| {NAMES[a]} | " + " | ".join(cols) + f" | {vl} | {va} | {med(fpw, 1) if fpw else '-'} | - |")


def per_tx():
    w = wire(("set100", 1))
    if not w:
        return
    print("\n## set100, one worker: client requests per transaction (tcpdump)\n")
    print("| arm | GRV tx/request | proxy frames/tx | proxy bytes/tx | storage frames/tx | connections |")
    print("|---|---:|---:|---:|---:|---:|")
    for a in ARMS:
        if a in w:
            v = w[a]
            print(f"| {NAMES[a]} | {v['grv_tpr']:.2f} | {v['px_frames']:.3f} | {v['px_bytes']:.1f} | {v['ss_frames'] or 0:.3f} | {v['flows']} |")


def syscalls(path):
    if not os.path.exists(path):
        return
    rows = [json.loads(l) for l in open(path)]
    cells = sorted({(r["workload"], r["workers"]) for r in rows})
    keys = ["sys_enter_per_op", "futex_per_op", "epoll_pwait_per_op", "read_per_op", "write_per_op", "nanosleep_per_op", "context-switches_per_op"]
    print("\n## Client syscalls per operation (perf stat, medians)\n")
    print("| cell | arm | " + " | ".join(k.replace("_per_op", "").replace("sys_enter", "all") for k in keys) + " |")
    print("|---|---|" + "---:|" * len(keys))
    for cell in cells:
        for a in ARMS:
            rs = [r for r in rows if (r["workload"], r["workers"]) == cell and r["arm"] == a]
            if rs:
                print(f"| {cell[0]}/{cell[1]} | {NAMES[a]} | " + " | ".join(f"{statistics.median([r.get(k, 0) for r in rs]):.2f}" for k in keys) + " |")


table(f"{D}/ladder.jsonl", "Closed loop, equal workers")
table(f"{D}/rate.jsonl", "set100, 64 workers, rate-capped to 18,000 ops/s")
per_tx()
syscalls(f"{D}/syscalls.jsonl")
table(f"{D}/cross/ladder.jsonl", "Cross-box check: phase-5 binaries on this box")
