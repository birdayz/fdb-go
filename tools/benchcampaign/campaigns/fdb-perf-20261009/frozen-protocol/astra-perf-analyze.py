#!/usr/bin/env python3
"""Summarize raw latency CSVs; uncertainty resamples paired *run blocks*, not ops.

Raw schema required: latency_ns, attempts, error; optional phase=measure|warmup.
Every measured operation, including errors, must have one row. A failed or
incomplete campaign is diagnostic data, never a clean performance conclusion.
"""
import argparse
import csv
import hashlib
import json
import math
from pathlib import Path
import random
import statistics


def quantile(values, p):
    if not math.isfinite(p) or not 0 < p <= 1:
        raise ValueError("quantile probability must be finite in (0, 1]")
    if not values or any(not math.isfinite(v) for v in values):
        raise ValueError("quantile requires nonempty finite observations")
    ordered = sorted(values)
    return ordered[max(0, math.ceil(p * len(ordered)) - 1)]


def bootstrap_log_ratio(pairs, rng, resamples):
    # One observation per paired run block; all ops from that block stay together.
    if len(pairs) < 2 or resamples < 100:
        raise ValueError("bootstrap requires >=2 paired blocks and >=100 resamples")
    if any(not math.isfinite(v) or v <= 0 for pair in pairs for v in pair):
        raise ValueError("paired metrics must be positive finite values")
    logs = [math.log(a) - math.log(b) for a, b in pairs]
    n = len(logs)
    boot = [math.exp(sum(logs[rng.randrange(n)] for _ in range(n)) / n)
            for _ in range(resamples)]
    point = math.exp(statistics.mean(logs))
    if any(not math.isfinite(v) or v <= 0 for v in [point, *boot]):
        raise ValueError("ratio is outside the representable positive finite range")
    return {"ratio": point,
            "ci95_low": quantile(boot, .025), "ci95_high": quantile(boot, .975),
            "blocks": n, "paired_log_ratio_sd": statistics.stdev(logs) if n > 1 else None}


def dependence_sensitivity(pairs, rng, resamples, block_length=3):
    logs = [math.log(a) - math.log(b) for a, b in pairs]
    mean = statistics.mean(logs)
    denominator = sum((v - mean) ** 2 for v in logs)
    lag1 = (sum((a - mean) * (b - mean) for a, b in zip(logs, logs[1:])) / denominator
            if denominator > 0 else None)
    n = len(logs)
    boot = []
    for _ in range(resamples):
        sampled = []
        while len(sampled) < n:
            start = rng.randrange(n)
            sampled.extend(logs[(start + offset) % n] for offset in range(block_length))
        boot.append(math.exp(statistics.mean(sampled[:n])))
    return {"paired_log_ratio_lag1": lag1, "moving_block_length": block_length,
            "moving_block_ci95_low": quantile(boot, .025),
            "moving_block_ci95_high": quantile(boot, .975)}


def digest(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("campaign")
    ap.add_argument("--raw-name", default="samples.csv")
    ap.add_argument("--denominator", default="cgo")
    ap.add_argument("--resamples", type=int, default=20000)
    ap.add_argument("--seed", type=int, default=391706)
    args = ap.parse_args()
    out = Path(args.campaign)
    plan = json.loads((out / "plan.json").read_text())
    schedule = json.loads((out / "schedule.json").read_text())
    complete = json.loads((out / "complete.json").read_text())
    assert len(schedule) > 0
    assert complete["scheduled"] == complete["completed"] == len(schedule)
    if len({run["run_id"] for run in schedule}) != len(schedule):
        raise ValueError("duplicate run IDs")
    if len({w["name"] for w in plan["workloads"]}) != len(plan["workloads"]):
        raise ValueError("duplicate workload names")
    if len({a["name"] for a in plan["arms"]}) != len(plan["arms"]):
        raise ValueError("duplicate arm names")
    expected = {(w["name"], b, a["name"]) for w in plan["workloads"]
                for b in range(plan["blocks"]) for a in plan["arms"]}
    observed = [(r["workload"]["name"], r["block"], r["arm"]) for r in schedule]
    if len(observed) != len(expected) or set(observed) != expected:
        raise ValueError("schedule contains duplicate, missing or unexpected arms")
    all_latencies = {}
    stats = {}
    issues = []
    per_run = []
    hashes = {}
    for run in schedule:
        run_id = run["run_id"]
        result = json.loads((out / run_id / "result.json").read_text())
        assert result["returncode"] == 0, run_id
        path = out / run_id / args.raw_name
        summary_path = path.with_name("summary.json")
        summary = json.loads(summary_path.read_text())
        if (summary.get("success") is not True or summary.get("persisted_validation") != "ok" or
                summary.get("requested") != run["workload"]["iterations"] or summary.get("completed") != run["workload"]["iterations"] or
                summary.get("errors") != 0 or summary.get("warmup_completed") != run["workload"]["warmup"] or
                summary.get("warmup_errors") != 0 or summary.get("measured_wall_ns", 0) <= 0):
            raise ValueError("invalid/incomplete summary or persisted validation: " + run_id)
        hashes[str(path.relative_to(out))] = digest(path)
        hashes[str(summary_path.relative_to(out))] = digest(summary_path)
        with open(path, newline="") as stream:
            reader = csv.DictReader(stream)
            if not {"phase", "index", "worker", "latency_ns", "attempts", "error"}.issubset(reader.fieldnames or []):
                raise ValueError("missing raw schema fields: " + run_id)
            rows = list(reader)
        if len(rows) != run["workload"]["iterations"] or any(row["phase"] != "measure" for row in rows):
            raise ValueError("invalid phase or measured count: " + run_id)
        if sorted(int(row["index"]) for row in rows) != list(range(len(rows))):
            raise ValueError("duplicate or missing operation index: " + run_id)
        if any(not 0 <= int(row["worker"]) < run["workload"]["workers"] for row in rows):
            raise ValueError("invalid worker index: " + run_id)
        warmup_path = path.with_name("warmup.csv")
        with open(warmup_path, newline="") as stream:
            warmup = list(csv.DictReader(stream))
        if len(warmup) != run["workload"]["warmup"] or any(row.get("phase") != "warmup" or row.get("error") != "" for row in warmup):
            raise ValueError("invalid warmup phase, count or error: " + run_id)
        hashes[str(warmup_path.relative_to(out))] = digest(warmup_path)
        failures = [row for row in rows if row["error"] != ""]
        successes = [row for row in rows if row["error"] == ""]
        attempts = [int(row["attempts"]) for row in rows]
        assert all(n >= 1 for n in attempts), run_id
        latencies = [int(row["latency_ns"]) for row in successes]
        assert latencies and min(latencies) > 0, run_id
        if failures:
            issues.append({"run_id": run_id, "error_count": len(failures)})
        s = {"run_id": run_id, "block": run["block"], "position": run["position"],
             "arm": run["arm"], "workload": run["workload"]["name"],
             "workers": run["workload"]["workers"], "observations": len(rows),
             "successes": len(successes), "errors": len(failures),
             "transaction_attempts": sum(attempts), "retries": sum(n - 1 for n in attempts),
             "mean_ns": statistics.mean(latencies), "p50_ns": quantile(latencies, .5),
             "p90_ns": quantile(latencies, .9), "p95_ns": quantile(latencies, .95),
             "p99_ns": quantile(latencies, .99), "max_ns": max(latencies),
             "measured_wall_ns": summary["measured_wall_ns"],
             "closed_loop_ops_per_second": len(successes) * 1e9 / summary["measured_wall_ns"]}
        if "stats_before" in summary and "stats_after" in summary:
            before, after = summary["stats_before"], summary["stats_after"]
            cpu_ns = 1000 * sum(after[key] - before[key] for key in ("process_user_cpu_us", "process_system_cpu_us"))
            s.update(process_cpu_ns=cpu_ns, process_cpu_ns_per_op=cpu_ns / len(rows),
                     process_cpu_over_wall=cpu_ns / summary["measured_wall_ns"],
                     go_alloc_bytes_per_op=(after["go_total_alloc_bytes"] - before["go_total_alloc_bytes"]) / len(rows),
                     go_mallocs_per_op=(after["go_mallocs"] - before["go_mallocs"]) / len(rows),
                     go_gc_cycles=after["go_gc_cycles"] - before["go_gc_cycles"],
                     process_lifetime_high_water_rss_kib=after["process_high_water_rss_kb"])
        stats[(s["workload"], s["block"], s["arm"])] = s
        all_latencies.setdefault((s["workload"], s["arm"]), []).extend(latencies)
        per_run.append(s)
    arms = [a["name"] for a in plan["arms"]]
    assert args.denominator in arms
    comparisons = []
    rng = random.Random(args.seed)
    for w in plan["workloads"]:
        workload = w["name"]
        for numerator in arms:
            if numerator == args.denominator:
                continue
            for metric in ("mean_ns", "p50_ns", "p99_ns"):
                pairs = [(stats[(workload, b, numerator)][metric],
                          stats[(workload, b, args.denominator)][metric])
                         for b in range(plan["blocks"])]
                s = bootstrap_log_ratio(pairs, rng, args.resamples)
                s.update(dependence_sensitivity(pairs, rng, args.resamples))
                for numerator_first in (True, False):
                    ordered = [(a, b) for block, (a, b) in enumerate(pairs)
                               if (stats[(workload, block, numerator)]["position"] <
                                   stats[(workload, block, args.denominator)]["position"]) == numerator_first]
                    label = "numerator_first" if numerator_first else "denominator_first"
                    s[label + "_blocks"] = len(ordered)
                    s[label + "_ratio"] = (math.exp(statistics.mean(math.log(a) - math.log(b) for a, b in ordered))
                                           if ordered else None)
                half = len(pairs) // 2
                s.update(workload=workload, numerator=numerator,
                         denominator=args.denominator, metric=metric,
                         ratio_gt_1_means="numerator slower",
                         first_half_ratio=math.exp(statistics.mean(math.log(a / b) for a, b in pairs[:half])),
                         second_half_ratio=math.exp(statistics.mean(math.log(a / b) for a, b in pairs[half:])))
                comparisons.append(s)
    pooled = [{"workload": w, "arm": a, "successes": len(v),
               "mean_ns": statistics.mean(v), "p50_ns": quantile(v, .5),
               "p99_ns": quantile(v, .99), "max_ns": max(v)}
              for (w, a), v in sorted(all_latencies.items())]
    report = {"valid_zero_error_campaign": not issues, "issues": issues,
              "inference": "Geometric mean of paired run-level mean/p50/p99 ratios (NOT ratio of pooled p99s). Nominal 95% percentile bootstrap over run-block replicates; statistical independence is not established on a continuously running server. Circular moving-block bootstrap length3, lag1 and order effects are sensitivity diagnostics, not a guarantee of CI coverage. One guest/server configuration, not independent physical hosts. Pooled quantiles are descriptive only. No multiplicity correction.",
              "bootstrap_seed": args.seed, "bootstrap_resamples": args.resamples,
              "plan_sha256": digest(out / "plan.json"), "raw_sha256": hashes,
              "per_run": per_run, "pooled_descriptive": pooled,
              "comparisons": comparisons}
    (out / "analysis.json").write_text(json.dumps(report, indent=2, sort_keys=True) + "\n")
    fields = list(per_run[0])
    with open(out / "per-run.csv", "w", newline="") as stream:
        writer = csv.DictWriter(stream, fieldnames=fields)
        writer.writeheader()
        writer.writerows(per_run)
    print(json.dumps({"valid": not issues, "comparisons": comparisons}, indent=2))
    if issues:
        raise SystemExit("errors observed: do not present clean performance claims")


if __name__ == "__main__":
    main()
