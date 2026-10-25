#!/usr/bin/env python3
"""Wire/server-metric table for any arms: summarize3_wire.py DIR ARM,ARM,..."""
import glob, json, os, statistics, sys
d, arms = sys.argv[1], sys.argv[2].split(",")
rows = {a: [] for a in arms}
for f in sorted(glob.glob(f"{d}/r*-wirestats.json")):
    tag = os.path.basename(f).split("-wirestats")[0]
    arm = tag.split("-", 1)[1]
    if arm not in rows:
        continue
    w, t, m = json.load(open(f)), json.load(open(f"{d}/{tag}-wiretrace.json")), json.load(open(f"{d}/{tag}-srvmetrics.json"))
    g = t["kinds"]["grv"]
    rows[arm].append({
        "flows": w["client_flows"], "grv_tpr": g["transactions"] / g["requests"], "grv_lat": g["latency_ns"][0] / 1e3,
        "ss_segs": w["4753/c2s"]["segments_per_op"], "ss_fps": w["4753/c2s"]["frames_per_segment"],
        "ss_s2c_fps": w["4753/s2c"]["frames_per_segment"], "px_segs": w["4751/c2s"]["segments_per_op"],
        "d_iter": m.get("d.Network.RunLoopIterations", 0), "s_grvreq": m.get("s.GrvProxyMetrics.TxnRequestIn", 0),
        "s_iter": m.get("s.Network.RunLoopIterations", 0)})
labels = [("flows", "client connections", 0), ("grv_tpr", "transactions per GRV request", 1),
          ("grv_lat", "GRV latency at the wire, p50 µs", 0), ("ss_segs", "storage client→server packets per tx", 4),
          ("ss_fps", "storage frames per client packet", 2), ("ss_s2c_fps", "storage frames per server packet", 2),
          ("px_segs", "proxy client→server packets per tx", 4), ("d_iter", "storage RunLoopIterations per tx", 4),
          ("s_grvreq", "GRV proxy TxnRequestIn per tx", 4), ("s_iter", "stateless RunLoopIterations per tx", 4)]
med = lambda v, n: f"{statistics.median(v):,.{n}f} [{min(v):,.{n}f}, {max(v):,.{n}f}]" if v else "-"
print("| per transaction | " + " | ".join(arms) + " |\n|---|" + "---:|" * len(arms))
for k, lab, n in labels:
    print(f"| {lab} | " + " | ".join(med([r[k] for r in rows[a]], n) for a in arms) + " |")
