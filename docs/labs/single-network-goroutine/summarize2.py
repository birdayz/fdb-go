#!/usr/bin/env python3
"""Round-2 tables from raw2/: cells (4 arms), set100/8 convoy modes, wire and
server-metric captures, and the coalescing-delay confirmation.

Cells: median [min, max] over rounds. X/A is the median [min, max] of the
within-round ratios to arm A. set100/8 runs are classified by measured p50
latency: one convoy (p50 < MODE_SPLIT_US) or split.
"""

import collections
import glob
import json
import os
import statistics
import sys

HERE = sys.argv[1] if len(sys.argv) > 1 else "raw2"
ARMS = ["A", "B", "W", "S"]
NAMES = {"A": "A today", "B": "B net goroutine", "W": "W one writer (a)", "S": "S sender clock (b)"}


def med(v, d):
    return f"{statistics.median(v):,.{d}f} [{min(v):,.{d}f}, {max(v):,.{d}f}]" if v else "-"


def ratio(cell, arm, key):
    by = {(r["round"], r["arm"]): r[key] for r in cell}
    rs = [by[(k, arm)] / by[(k, "A")] for k in sorted({k for k, _ in by}) if (k, arm) in by and (k, "A") in by]
    return f"{statistics.median(rs):.3f} [{min(rs):.3f}, {max(rs):.3f}]" if rs else "-"


METRICS = [("ops_per_s", "ops/s", 0), ("p50_us", "p50 µs", 0), ("p99_us", "p99 µs", 0),
           ("client_cpu_us_per_op", "client CPU µs/op", 1), ("server_kcycles", "server kcycles/tx", 1)]


def cells():
    rows = [json.loads(l) for l in open(f"{HERE}/cells.jsonl")]
    for r in rows:
        r["server_kcycles"] = r["server_cycles_per_tx"] / 1e3
    by = collections.defaultdict(list)
    for r in rows:
        by[(r["workload"], r["workers"])].append(r)
    for (wl, w), cell in by.items():
        loads = [r["load1_before_long"] for r in cell] + [r["load1_before_short"] for r in cell]
        rounds = len({r["round"] for r in cell})
        print(f"\n#### {wl}, {w} workers ({rounds} rounds; load1 {min(loads):.1f}–{max(loads):.1f})\n")
        print("| metric | " + " | ".join(NAMES[a] for a in ARMS) + " | B/A | W/A | S/A |")
        print("|---|" + "---:|" * 7)
        for key, label, d in METRICS:
            vals = [med([r[key] for r in cell if r["arm"] == a], d) for a in ARMS]
            print(f"| {label} | " + " | ".join(vals) + " | " + " | ".join(ratio(cell, a, key) for a in "BWS") + " |")
        errs = [f"{sum(r['errors'] for r in cell if r['arm'] == a)} / {sum(r['retries'] for r in cell if r['arm'] == a)}" for a in ARMS]
        print("| errors / retries | " + " | ".join(errs) + " | | | |")
        if (wl, w) == ("set100", 8):
            modes(cell)


def modes(cell):
    bands = [("~7 ms", 0, 8500), ("~9-11 ms", 8500, 12500), ("~16 ms", 12500, 1e9)]
    print("\nset100/8 by long-run p50 band (one convoy ~7 ms; split convoys ~9-11 and ~16 ms):\n")
    print("| arm | " + " | ".join(f"{n} runs" for n, _, _ in bands) + " | " +
          " | ".join(f"{n}: ops/s, server kcycles/tx, client µs/op" for n, _, _ in bands) + " |")
    print("|---|" + "---:|" * 3 + "---|" * 3)
    for a in ARMS:
        rs = [r for r in cell if r["arm"] == a]
        groups = [[r for r in rs if lo <= r["p50_us"] < hi] for _, lo, hi in bands]
        f = lambda g: ", ".join(med([r[k] for r in g], d) for k, d in
                                (("ops_per_s", 0), ("server_kcycles", 0), ("client_cpu_us_per_op", 1))) if g else "-"
        print(f"| {a} | " + " | ".join(str(len(g)) for g in groups) + " | " + " | ".join(f(g) for g in groups) + " |")
    print("\nPer run (round: arm p50 ms / server kcycles):\n")
    for k in sorted({r["round"] for r in cell}):
        rs = sorted((r for r in cell if r["round"] == k), key=lambda r: r["slot"])
        print(f"- round {k}: " + ", ".join(f"{r['arm']} {r['p50_us'] / 1e3:.1f}/{r['server_kcycles']:.0f}" for r in rs))


def wire():
    files = sorted(glob.glob(f"{HERE}/wire/r*-wirestats.json"))
    if not files:
        return
    rows = collections.defaultdict(list)
    for f in files:
        tag = os.path.basename(f).split("-wirestats")[0]
        arm = tag.split("-")[1]
        w = json.load(open(f))
        t = json.load(open(f"{HERE}/wire/{tag}-wiretrace.json"))
        m = json.load(open(f"{HERE}/wire/{tag}-srvmetrics.json"))
        g = t["kinds"]["grv"]
        rows[arm].append({
            "flows": w["client_flows"],
            "grv_req": g["requests"] / g["transactions"], "grv_txn_per_req": g["transactions"] / g["requests"],
            "ss_c2s_segs": w["4753/c2s"]["segments_per_op"], "ss_c2s_fps": w["4753/c2s"]["frames_per_segment"],
            "ss_s2c_segs": w["4753/s2c"]["segments_per_op"], "ss_s2c_fps": w["4753/s2c"]["frames_per_segment"],
            "ss_c2s_bytes": w["4753/c2s"]["bytes_per_op"], "px_c2s_segs": w["4751/c2s"]["segments_per_op"],
            "px_c2s_bytes": w["4751/c2s"]["bytes_per_op"], "px_s2c_segs": w["4751/s2c"]["segments_per_op"],
            "d_iter": m["d.Network.RunLoopIterations"], "d_read": m["d.Network.ReadCalls"], "d_write": m["d.Network.WriteCalls"],
            "d_tasks": m["d.Network.TasksExecuted"], "d_queries": m["d.StorageMetrics.GetValueQueries"],
            "s_iter": m["s.Network.RunLoopIterations"], "s_grvreq": m["s.GrvProxyMetrics.TxnRequestIn"],
            "s_batches": m["s.GrvProxyMetrics.TxnStartBatch"], "s_write": m["s.Network.WriteCalls"],
        })
    labels = [("flows", "client connections", 0), ("grv_req", "GRV requests per tx (wire)", 4),
              ("grv_txn_per_req", "transactions per GRV request", 1),
              ("ss_c2s_segs", "storage: client→server packets per tx", 4), ("ss_c2s_fps", "storage: frames per client packet", 2),
              ("ss_c2s_bytes", "storage: client→server bytes per tx", 1),
              ("ss_s2c_segs", "storage: server→client packets per tx", 4), ("ss_s2c_fps", "storage: frames per server packet", 2),
              ("px_c2s_segs", "proxy: client→server packets per tx", 4), ("px_c2s_bytes", "proxy: client→server bytes per tx", 1),
              ("px_s2c_segs", "proxy: server→client packets per tx", 4),
              ("d_queries", "storage GetValueQueries per tx (trace)", 3), ("d_iter", "storage RunLoopIterations per tx", 4),
              ("d_read", "storage ReadCalls per tx", 4), ("d_write", "storage WriteCalls per tx", 4),
              ("d_tasks", "storage TasksExecuted per tx", 3),
              ("s_grvreq", "GRV proxy TxnRequestIn per tx (trace)", 4), ("s_batches", "GRV proxy TxnStartBatch per tx", 4),
              ("s_iter", "stateless RunLoopIterations per tx", 4), ("s_write", "stateless WriteCalls per tx", 4)]
    n = {a: len(rows[a]) for a in ARMS}
    print(f"\n#### get100, 64 workers: wire and server trace metrics (runs per arm: {n})\n")
    print("Median [min, max] per arm; wire from 220k-op captures, trace metrics from 2.42M-op windows.\n")
    print("| per transaction | " + " | ".join(NAMES[a] for a in ARMS) + " |")
    print("|---|" + "---:|" * 4)
    for k, label, d in labels:
        print(f"| {label} | " + " | ".join(med([r[k] for r in rows[a]], d) for a in ARMS) + " |")


def confirm():
    p = f"{HERE}/confirm/delay.jsonl"
    if not os.path.exists(p):
        return
    rows = [json.loads(l) for l in open(p)]
    print("\n#### Coalescing-delay confirmation, get100/64 (servercost.py, 1 rep per line, order as run)\n")
    print("| arm and delay | server kcycles/tx | client µs/op | tx/s | mean latency µs | p99 µs |\n|---|---:|---:|---:|---:|---:|")
    for r in rows:
        print(f"| {r['label']} | {r['server_cycles'] / 1e3:.1f} | {r['client_cpu_us']:.1f} | {r['tx_per_s']:,.0f} | {r['mean_latency_us']:.0f} | {r['p99_latency_us']:.0f} |")


if os.path.exists(f"{HERE}/cells.jsonl"):
    cells()
wire()
confirm()
