#!/usr/bin/env python3
"""Results table from runs/<campaign>/results.json (+ per-wave FDB sample files). Measured wave only."""
import json, os, sys
from pathlib import Path
H = Path(os.environ.get("LEAN_STATE_DIR") or Path(__file__).resolve().parent / "runs")
camp = sys.argv[1] if len(sys.argv) > 1 else "lean-measured-1"
files = sys.argv[2:] or ["results.json"]
res = {"runs": []}
for f in files:   # later files supply repeat runs; a passed repeat replaces an earlier failed run of the same stage/variant
    for r in json.load(open(H / camp / f))["runs"]:
        r = dict(r, source=f)
        res["runs"] = [x for x in res["runs"] if not ((x["stage"], x["arm"], x.get("label")) == (r["stage"], r["arm"], r.get("label")) and x["status"] != "passed" and r["status"] == "passed")]
        res["runs"].append(r)
LABEL = {"hc32": "32 in flight per load generator (4x the baseline 8)", "hc64_go_workers2": "Go-only: 32 consumers x 2 workers = 64 in flight per load generator",
         "hc128_go_workers4": "Go-only: 32 consumers x 4 workers = 128 in flight", "hc256_go_workers8": "Go-only: 32 consumers x 8 workers = 256 in flight", "high_concurrency": "32 in flight"}
NAME = {"go_pure": "Go own client", "go_native": "Go C client", "rust_native": "Rust (C client)"}
# a sample file name was shared by repeated runs of the same stage/variant: only the latest run owns it; the earlier ones use
# <name>.baseline-copy.ndjson if one was saved in time, otherwise their database figures are n/a
claims = {}
for r in res["runs"]:
    for w in r["waves"]:
        if w["wave"] == "measured" and w.get("fdb_sample_file"):
            claims.setdefault(w["fdb_sample_file"], []).append((r["started"], id(w)))
for r in res["runs"]:
    for w in r["waves"]:
        f = w.get("fdb_sample_file")
        if w["wave"] == "measured" and f and len(claims.get(f, [])) > 1 and max(claims[f])[1] != id(w):
            alt = f.replace(".ndjson", ".baseline-copy.ndjson")
            w["fdb_sample_file"] = alt if Path(alt).exists() else "/nonexistent"
rows = []
for r in res["runs"]:
    w = next((x for x in r["waves"] if x["wave"] == "measured"), None)
    row = {"stage": r["stage"], "arm": r["arm"], "label": r.get("label"), "status": r["status"], "conditioned": r.get("conditioned"), "reason": r.get("unconditioned_reason"),
           "error": r.get("error"), "seconds": r.get("seconds")}
    if w and w["status"] == "passed":
        cl = w["clients"]; c = w["controller"]
        distinct = sum(x["verify_report"]["distinct_spans"] for x in cl.values())
        persum = sum(x["verify_report"]["distinct_spans"] / (x["ingest_report"]["ingest_wall_ns"] / 1e9) for x in cl.values())
        cpu = sum(x["ingest_report"]["ingest_resources"]["cpu_seconds"] for x in cl.values())
        rss = max(x["ingest_report"]["ingest_resources"]["peak_rss_bytes"] for x in cl.values())
        # database CPU during the ingest window (sum over all FDB processes, cores), from the 1 Hz samples
        db = {"mean_cores": None, "peak_cores": None, "storage_mean": None, "samples": 0}
        try:
            smp = [json.loads(l) for l in open(w["fdb_sample_file"])][1:]
            lo, hi = c.get("t_release"), c.get("t_ack_end")
            win = [s for s in smp if (lo is None or s["t"] >= lo) and (hi is None or s["t"] <= hi)] or smp
            tot = [sum((p["cpu_cores"] or 0) for p in s["processes"]) for s in win]
            sto = [sum((p["cpu_cores"] or 0) for p in s["processes"] if "storage" in p["roles"]) for s in win]
            db = {"mean_cores": sum(tot) / len(tot), "peak_cores": max(tot), "storage_mean": sum(sto) / len(sto), "samples": len(win),
                  "max_rss_mb": max((p["rss_bytes"] or 0) for s in win for p in s["processes"]) / 1e6}
            busiest = {}
            for sm in win:
                for p in sm["processes"]:
                    role = (p["roles"] or [p.get("class") or "?"])[0]
                    busiest[role] = max(busiest.get(role, 0), p["cpu_cores"] or 0)
            db["busiest_by_role"] = {k: round(v, 2) for k, v in sorted(busiest.items(), key=lambda kv: -kv[1])}
            lim = {}
            for sm in win:
                for key in ("limited_by", "batch_limited_by"):
                    lim.setdefault(key, {})
                    lim[key][sm.get(key)] = lim[key].get(sm.get(key), 0) + 1
            db["limiter_samples"] = lim
            db["max_lag_s"] = max(sm["lag_seconds"] for sm in win)
            db["tps_limit_min"] = min([sm["tps_limit"] for sm in win if sm.get("tps_limit") is not None] or [None]) if any(sm.get("tps_limit") is not None for sm in win) else None
        except Exception as e:
            db["error"] = "database samples unavailable (%s)" % type(e).__name__
        cores = [x["ingest_report"]["ingest_resources"]["cpu_seconds"] / (x["ingest_report"]["ingest_wall_ns"] / 1e9) for x in cl.values()]
        def counter(name):
            vals = [x["ingest_report"].get(name) for x in cl.values()]
            return sum(v["value"] for v in vals if isinstance(v, dict) and v.get("availability") == "available") if any(isinstance(v, dict) and v.get("availability") == "available" for v in vals) else None
        skipped = all(x["verify_report"].get("status") == "skipped" for x in cl.values())
        census_ok = skipped or all(x["verify_report"].get("census", {}).get("matched") is True and (x["verify_report"].get("census", {}).get("actual") or {}).get("unexpected_kvs") == 0
                        and x["verify_report"].get("status") == "ok" for x in cl.values())
        row.update(verified_spans=distinct, spans_per_s_aggregate=c["ack_spans_per_s"], spans_per_s_sum_of_producers=persum,
                   drained_spans_per_s=c["drained_spans_per_s"], cpu_ms_per_span=cpu * 1000 / distinct, peak_rss_mib=rss / 2**20,
                   db=db, client_cores_mean=sum(cores) / len(cores), client_cores_peak=max(cores), retries={"native": counter("native_retries"), "queue": counter("queue_retries"), "export_attempts": counter("export_attempts")},
                   verify="NOT VERIFIED (skipped; ingest-accepted spans)" if skipped else ("full read-back + census passed" if census_ok else "FAILED"), committed=c.get("fdb_committed_delta"),
                   conflicted=c.get("fdb_conflicted_delta"), unhealthy=c.get("unhealthy_samples"), max_queue_mb=(c.get("max_storage_queue_bytes") or 0) / 1e6,
                   conditioning=w.get("conditioning"))
    rows.append(row)
json.dump(rows, open(H / camp / "summary.json", "w"), indent=1)
out = ["| Nodes | Variant | spans/s (aggregate, verified) | spans/s (sum of producers) | drained spans/s | client CPU ms/span | client peak RSS MiB | DB CPU cores (mean / peak) | conditioned | verification | run |", "|---|---|---:|---:|---:|---:|---:|---|---|---|---|"]
for r in sorted(rows, key=lambda x: (x["stage"], x.get("label") or "", ["go_pure", "go_native", "rust_native"].index(x["arm"]), x["status"] != "passed")):
    nm = NAME[r["arm"]] + (" [%s]" % LABEL.get(r["label"], r["label"]) if r.get("label") else " [8 in flight, baseline]")
    if r["status"] == "passed" and "verified_spans" in r:
        d = r["db"]
        out.append("| %d | %s | %.0f | %.0f | %.0f | %.3f | %.0f | %s / %s | %s | %s | %.0f s |" % (
            r["stage"], nm, r["spans_per_s_aggregate"], r["spans_per_s_sum_of_producers"], r["drained_spans_per_s"] or 0, r["cpu_ms_per_span"], r["peak_rss_mib"],
            "%.2f" % d["mean_cores"] if d["mean_cores"] is not None else "n/a", "%.2f" % d["peak_cores"] if d["peak_cores"] is not None else "n/a",
            "yes" if r["conditioned"] else "NO (%s)" % (r["reason"] or "")[:60], r["verify"], r["seconds"]))
    else:
        out.append("| %d | %s | - | - | - | - | - | - | %s | FAILED: %s | %s s |" % (r["stage"], nm, "yes" if r["conditioned"] else "no", (r["error"] or "")[:100], r["seconds"]))
print("\n".join(out))
print()
print("| Nodes | Variant | acknowledged spans/s | durable (drained) spans/s | client cores mean/peak | busiest DB process per role (cores) | storage queue max MB / lag max s | conflicts | retries (native/queue) | ratekeeper limiter (samples) |")
print("|---|---|---:|---:|---|---|---|---:|---|---|")
for r in sorted(rows, key=lambda x: (x["stage"], x.get("label") or "", ["go_pure", "go_native", "rust_native"].index(x["arm"]))):
    if "verified_spans" not in r:
        continue
    d = r["db"]
    nm = NAME[r["arm"]] + (" [%s]" % LABEL.get(r["label"], r["label"]) if r.get("label") else " [8 in flight, baseline]")
    print("| %d | %s | %.0f | %.0f | %.1f / %.1f | %s | %.0f / %.1f | %s | %s/%s | %s |" % (r["stage"], nm, r["spans_per_s_aggregate"], r["drained_spans_per_s"] or 0, r["client_cores_mean"], r["client_cores_peak"],
          ", ".join("%s %.2f" % kv for kv in list((d.get("busiest_by_role") or {}).items())[:5]), r["max_queue_mb"], d.get("max_lag_s") or 0, r["conflicted"], r["retries"]["native"], r["retries"]["queue"], d.get("limiter_samples")))
