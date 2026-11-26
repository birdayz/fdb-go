#!/usr/bin/env python3
import json, os, sys
from pathlib import Path
H = Path(os.environ.get("LEAN_STATE_DIR") or Path(__file__).resolve().parent / "runs")
camp = sys.argv[1] if len(sys.argv) > 1 else "lean-climb-1"
r = json.load(open(H / camp / "climb-results.json"))
NAME = {"go_pure": "Go own client", "go_native": "Go C client", "rust_native": "Rust (C client)"}
for v in r["variants"]:
    print("\n### %s  (pre-split once: %s s total, %s partitions; later steps run on already-split ranges)" % (NAME.get(v["arm"], v["arm"]), v.get("presplit_total_s"), v.get("partitions_after_presplit")))
    thr = any(st.get("busiest_thread") for st in v["steps"])
    print("| step | spans | ack spans/s (sum) | durable spans/s | client cores used of 16 / CPU ms per span | client peak MiB (sum) | accept time p50/p95/p99 µs (median of processes) | busiest DB process per role (cores) | storage queue MB / lag s | conflicts | retries n/q | throttle (limiter: samples) | checked | window s |" + (" busiest generator thread (name, mean cores / highest 1 s) | its process total cores, threads |" if thr else ""))
    print("|---|---:|---:|---:|---|---:|---|---|---|---:|---|---|---|---:|" + ("---|---|" if thr else ""))
    for s in v["steps"]:
        if s.get("status") != "ok":
            print("| %s | %s | FAILED: %s |" % (s["label"], s.get("spans_per_process"), str(s.get("error"))[:200])); continue
        a = s["admission_us_median_of_processes"]
        print("| %s%s | %d | %.0f | %.0f | %.1f / %.3f | %.0f | %s | %s | %.0f / %.1f | %s | %s/%s | %s | %s | %.0f |" % (
            s["label"], " (repeat of best)" if s.get("repeat_of_best") else "", s["spans_total"], s["ack_spans_per_s"], s["durable_spans_per_s"],
            s["client_cores_used_of_16"], s["client_cpu_ms_per_span"], s["client_peak_rss_mib_sum"], "/".join("%.0f" % a[k] for k in ("p50_ns", "p95_ns", "p99_ns")),
            ", ".join("%s %.2f" % kv for kv in list(s["db_busiest_cores_by_role"].items())[:4]), s["storage_queue_max_mb"], s["storage_lag_max_s"], s["conflicts"],
            s["retries"].get("native_retries"), s["retries"].get("queue_retries"), s["limiter"].get("limited_by"),
            "full read-back + census" if s["verified"] else "counters only", s["ack_window_s"]) +
              ((" %s %.2f / %.2f | %s, %s |" % (s["busiest_thread"]["name"], s["busiest_thread"]["cores_mean"], s["busiest_thread"]["cores_peak_1s"],
                                              s["busiest_thread"]["process_cores_mean"], s["busiest_thread"]["thread_count"])) if s.get("busiest_thread") else (" - | - |" if thr else "")))
    print("best: %s, %.0f ack spans/s.  Stopped: %s" % (v.get("best_label"), v.get("best_ack_spans_per_s") or 0, v.get("stop_reason")))
