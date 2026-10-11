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

from run import make_schedule, validate_identity, validate_raw_output


WINDOW_CPU_SCOPE = (
    'Window CPU is not complete-lifecycle per-transaction CPU: the Apple binding releases native transactions/futures '
    'through Go GC finalizers, which enqueue further network-thread teardown. Warmup cleanup can enter the window '
    'and late measured-transaction cleanup can fall after it. The size/direction of that unequal deferred-cleanup '
    'effect is unmeasured by the window alone; GC counts do not establish that all native cleanup completed.'
)
LIFECYCLE_CPU_SCOPE = (
    'Drained CPU is (getrusage after the post-measurement finalizer fixpoint and native FIFO barrier minus '
    'the pre-measurement snapshot)/measured transactions. It includes forced GC and pending setup/warmup cleanup; '
    'it is not isolated steady-state transaction CPU. Shutdown process CPU is absolute RUSAGE_SELF user+system '
    'through the final fixpoint, database close and native StopNetwork, divided by measured transactions. '
    'It includes startup, fixture, warmup, validation and CSV output, but excludes final JSON output and process exit. '
    'Go finalizers are drained using exact runtime counters; cleanups are runtime estimates and must not increase '
    'during either drain. The terminal shutdown, not GC counts alone, establishes that native network work has stopped.'
)


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


def write_tables(out, per_run, comparisons):
    times = ['mean_ns', 'p50_ns', 'p90_ns', 'p99_ns', 'p999_ns', 'process_cpu_ns_per_op',
             'user_cpu_ns_per_op', 'system_cpu_ns_per_op', 'drained_cpu_ns_per_op', 'shutdown_process_cpu_ns_per_op']
    absolute = []
    for workload, arm in sorted({(r['workload'], r['arm']) for r in per_run}):
        rows = [r for r in per_run if r['workload'] == workload and r['arm'] == arm]
        absolute.append(dict(workload=workload, arm=arm, runs=len(rows), transactions=sum(r['observations'] for r in rows),
                             **{m: statistics.median(r[m] for r in rows) for m in times + ['go_alloc_bytes_per_op', 'go_mallocs_per_op']},
                             go_gc_cycles=sum(r['go_gc_cycles'] for r in rows),
                             drained_go_gc_cycles=sum(r['drained_go_gc_cycles'] for r in rows),
                             shutdown_go_gc_cycles=sum(r['shutdown_go_gc_cycles'] for r in rows)))
    text = ['# Client benchmark\n\nAbsolute times are medians of run-level statistics in µs/transaction. '
            'Ratios are geometric means of within-block ratios, NOT ratios of these medians. '
            'Window CPU is measured-phase getrusage user+system; drained and shutdown scopes are defined below. Go allocation/GC counters exclude the native C++ heap; '
            'do not compare total Go/CGo memory from these columns. Native client counters are unobserved.\n\n',
            '|Workload|Arm|Runs|Transactions|Mean|p50|p90|p99|p99.9|Window CPU|Window User CPU|Window System CPU|Drained CPU|Shutdown process CPU|Go B/tx|Go allocs/tx|Go GC cycles: window/drained/shutdown|\n',
            '|---|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|\n']
    for r in absolute:
        text.append(f'|{r["workload"]}|{r["arm"]}|{r["runs"]}|{r["transactions"]}|' +
                    '|'.join(f'{r[m]/1000:.3f}' for m in times) +
                    f'|{r["go_alloc_bytes_per_op"]:.3f}|{r["go_mallocs_per_op"]:.3f}|{r["go_gc_cycles"]}/{r["drained_go_gc_cycles"]}/{r["shutdown_go_gc_cycles"]}|\n')
    text.append('\n|Workload|Ratio|Metric|Paired GM|Nominal 95% block-bootstrap CI|Pairs <1|Numerator Go GC: window/drained/shutdown|Denominator Go GC: window/drained/shutdown|\n|---|---|---|---:|---|---:|---:|---:|\n')
    for r in comparisons:
        text.append(f'|{r["workload"]}|{r["numerator"]}/{r["denominator"]}|{r["metric"]}|{r["ratio"]:.6f}|'
                    f'[{r["ci95_low"]:.6f}, {r["ci95_high"]:.6f}]|{r["pairs_below_one"]}/{r["blocks"]}|'
                    f'{r["numerator_go_gc_cycles"]}/{r["numerator_drained_go_gc_cycles"]}/{r["numerator_shutdown_go_gc_cycles"]}|'
                    f'{r["denominator_go_gc_cycles"]}/{r["denominator_drained_go_gc_cycles"]}/{r["denominator_shutdown_go_gc_cycles"]}|\n')
    text.append('\nExploratory same-host result, not a noninferiority, global-native or capacity claim. '
                'Bootstrap resamples paired run blocks, not individual operations. Independence and CI coverage '
                'are not established on a continuously running server; no multiplicity correction. '
                'analysis.json includes order/dependence diagnostics, raw-file hashes and any CPU-component ratios '
                'omitted because a counter was zero.\n')
    text.append('\n' + WINDOW_CPU_SCOPE + '\n\n' + LIFECYCLE_CPU_SCOPE + '\n')
    if any(r['workload'] in ('set100', 'batchset50', 'batchset50x1000') for r in per_run):
        text.append('\nThe imported master590145 baseline omits C++ causal-read-risky on write-only commit GRVs '
                    '(NativeAPI.actor.cpp:6577-6578 versus client/transaction.go:1980-1985,3398-3411). '
                    'Without it the server waits for updateLastCommit (GrvProxyServer.actor.cpp:647-648). '
                    'Identify whether each tested revision retains this pre-existing difference; results measure '
                    'the actual implementations, not algorithmically identical clients.\n')
    (out / 'TABLES.md').write_text(''.join(text))
    for filename, rows in [('absolute.csv', absolute), ('paired.csv', comparisons)]:
        with (out / filename).open('w', newline='') as f:
            writer = csv.DictWriter(f, fieldnames=list(rows[0]))
            writer.writeheader()
            writer.writerows(rows)


def normalized_schedule(schedule):
    normalized = []
    for entry in schedule:
        command = list(entry['command'])
        if command.count('--output') != 1:
            raise ValueError('schedule differs from counterbalanced plan: output flag')
        index = command.index('--output') + 1
        if index >= len(command) or Path(command[index]).name != entry['run_id']:
            raise ValueError('schedule differs from counterbalanced plan: output run ID')
        # Archived campaigns may move; only the artifact directory is relocatable.
        command[index] = '<campaign-output>/' + entry['run_id']
        normalized.append(dict(entry, command=command))
    return normalized


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("campaign")
    ap.add_argument("--denominator", default="cgo")
    ap.add_argument("--resamples", type=int, default=20000)
    ap.add_argument("--seed", type=int, default=391706)
    args = ap.parse_args()
    out = Path(args.campaign)
    if (out / 'ABORTED.json').exists():
        raise ValueError('aborted campaign is diagnostic data, not a clean result')
    plan = json.loads((out / "plan.json").read_text())
    schedule = json.loads((out / "schedule.json").read_text())
    complete = json.loads((out / "complete.json").read_text())
    if not schedule or complete['scheduled'] != complete['completed'] or complete['completed'] != len(schedule):
        raise ValueError('incomplete campaign')
    expected_schedule = make_schedule(plan, out.resolve())
    if normalized_schedule(schedule) != normalized_schedule(expected_schedule):
        raise ValueError('schedule differs from counterbalanced plan')
    pins = json.loads((out / 'binaries.json').read_text())
    if complete['binaries'] != pins or plan['expected_sha256'] != pins:
        raise ValueError('input identities differ from the completed campaign')
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
        if result['returncode'] != 0:
            raise ValueError('nonzero process status: ' + run_id)
        validate_raw_output(out / run_id, run['workload'])
        validate_identity(out / run_id, run, plan, pins)
        path = out / run_id / 'samples.csv'
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
        if any(n < 1 for n in attempts):
            raise ValueError('invalid callback count: ' + run_id)
        latencies = [int(row["latency_ns"]) for row in successes]
        if not latencies or min(latencies) <= 0:
            raise ValueError('invalid latency population: ' + run_id)
        if failures:
            issues.append({"run_id": run_id, "error_count": len(failures)})
        s = {"run_id": run_id, "block": run["block"], "position": run["position"],
             "arm": run["arm"], "workload": run["workload"]["name"],
             "workers": run["workload"]["workers"], "observations": len(rows),
             "successes": len(successes), "errors": len(failures),
             "transaction_attempts": sum(attempts), "retries": sum(n - 1 for n in attempts),
             "mean_ns": statistics.mean(latencies), "p50_ns": quantile(latencies, .5),
             "p90_ns": quantile(latencies, .9), "p95_ns": quantile(latencies, .95),
             "p99_ns": quantile(latencies, .99), "p999_ns": quantile(latencies, .999), "max_ns": max(latencies),
             "measured_wall_ns": summary["measured_wall_ns"],
             "closed_loop_ops_per_second": len(successes) * 1e9 / summary["measured_wall_ns"]}
        if 'stats_before' not in summary or 'stats_after' not in summary:
            raise ValueError('measured-phase resource snapshots missing: ' + run_id)
        if "stats_before" in summary and "stats_after" in summary:
            before, after = summary["stats_before"], summary["stats_after"]
            if any(not math.isfinite(v) or v < 0 for s in (before, after) for v in s.values()) or any(after[key] < before[key] for key in before):
                raise ValueError('invalid or decreasing resource counter: ' + run_id)
            cpu_ns = 1000 * sum(after[key] - before[key] for key in ("process_user_cpu_us", "process_system_cpu_us"))
            if cpu_ns <= 0:
                raise ValueError('nonpositive measured process CPU: ' + run_id)
            s.update(process_cpu_ns=cpu_ns, process_cpu_ns_per_op=cpu_ns / len(rows),
                     user_cpu_ns_per_op=1000 * (after['process_user_cpu_us'] - before['process_user_cpu_us']) / len(rows),
                     system_cpu_ns_per_op=1000 * (after['process_system_cpu_us'] - before['process_system_cpu_us']) / len(rows),
                     process_cpu_over_wall=cpu_ns / summary["measured_wall_ns"],
                     go_alloc_bytes_per_op=(after["go_total_alloc_bytes"] - before["go_total_alloc_bytes"]) / len(rows),
                     go_mallocs_per_op=(after["go_mallocs"] - before["go_mallocs"]) / len(rows),
                     go_gc_cycles=after["go_gc_cycles"] - before["go_gc_cycles"],
                     go_gc_pause_ns=after['go_gc_pause_ns'] - before['go_gc_pause_ns'],
                     process_lifetime_high_water_rss_kib=after["process_high_water_rss_kb"])
        life = summary['lifecycle']
        drained, shutdown = life['stats_after_measurement_drain'], life['stats_after_shutdown']
        cpu_keys = ('process_user_cpu_us', 'process_system_cpu_us')
        s.update(drained_cpu_ns_per_op=1000 * sum(drained[k] - before[k] for k in cpu_keys) / len(rows),
                 shutdown_process_cpu_ns_per_op=1000 * sum(shutdown[k] for k in cpu_keys) / len(rows),
                 drained_go_gc_cycles=drained['go_gc_cycles'] - before['go_gc_cycles'],
                 shutdown_go_gc_cycles=shutdown['go_gc_cycles'])
        stats[(s["workload"], s["block"], s["arm"])] = s
        all_latencies.setdefault((s["workload"], s["arm"]), []).extend(latencies)
        per_run.append(s)
    arms = [a["name"] for a in plan["arms"]]
    if args.denominator not in arms:
        raise ValueError('denominator arm missing')
    comparisons = []
    unavailable = []
    rng = random.Random(args.seed)
    for w in plan["workloads"]:
        workload = w["name"]
        for numerator in arms:
            if numerator == args.denominator:
                continue
            for metric in ("mean_ns", "p50_ns", "p90_ns", "p99_ns", "p999_ns", "process_cpu_ns_per_op", "user_cpu_ns_per_op", "system_cpu_ns_per_op", "drained_cpu_ns_per_op", "shutdown_process_cpu_ns_per_op"):
                pairs = [(stats[(workload, b, numerator)][metric],
                          stats[(workload, b, args.denominator)][metric])
                         for b in range(plan["blocks"])]
                if any(v == 0 for pair in pairs for v in pair):
                    unavailable.append(dict(workload=workload, numerator=numerator, denominator=args.denominator,
                                            metric=metric, reason='zero counter; positive log-ratio undefined'))
                    continue
                s = bootstrap_log_ratio(pairs, rng, args.resamples)
                s['pairs_below_one'] = sum(a < b for a, b in pairs)
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
                         numerator_go_gc_cycles=sum(stats[(workload, b, numerator)]['go_gc_cycles'] for b in range(plan['blocks'])),
                         denominator_go_gc_cycles=sum(stats[(workload, b, args.denominator)]['go_gc_cycles'] for b in range(plan['blocks'])),
                         ratio_gt_1_means="numerator slower",
                         first_half_ratio=math.exp(statistics.mean(math.log(a / b) for a, b in pairs[:half])),
                         second_half_ratio=math.exp(statistics.mean(math.log(a / b) for a, b in pairs[half:])))
                for field in ('drained_go_gc_cycles', 'shutdown_go_gc_cycles'):
                    s['numerator_' + field] = sum(stats[(workload, b, numerator)][field] for b in range(plan['blocks']))
                    s['denominator_' + field] = sum(stats[(workload, b, args.denominator)][field] for b in range(plan['blocks']))
                comparisons.append(s)
    pooled = [{"workload": w, "arm": a, "successes": len(v),
               "mean_ns": statistics.mean(v), "p50_ns": quantile(v, .5),
               "p99_ns": quantile(v, .99), "max_ns": max(v)}
              for (w, a), v in sorted(all_latencies.items())]
    report = {"valid_zero_error_campaign": not issues, "issues": issues,
              "inference": "Geometric mean of paired run-level statistics (NOT ratio of pooled quantiles or medians). Window CPU is the measured-phase getrusage user+system delta per transaction; drained and shutdown CPU have separate explicit scopes. Go memory/GC excludes the native C++ heap; RSS is process-lifetime high water. Native counters are unobserved. Nominal 95% percentile bootstrap over run-block replicates; statistical independence is not established on a continuously running server. Circular moving-block bootstrap length3, lag1 and order effects are sensitivity diagnostics, not a guarantee of CI coverage. One guest/server configuration, not independent physical hosts. Pooled quantiles are descriptive only. No multiplicity correction.",
              "window_cpu_scope": WINDOW_CPU_SCOPE,
              "lifecycle_cpu_scope": LIFECYCLE_CPU_SCOPE,
              "bootstrap_seed": args.seed, "bootstrap_resamples": args.resamples,
              "plan_sha256": digest(out / "plan.json"), "raw_sha256": hashes,
              "per_run": per_run, "pooled_descriptive": pooled,
              "comparisons": comparisons, "unavailable_comparisons": unavailable}
    (out / "analysis.json").write_text(json.dumps(report, indent=2, sort_keys=True) + "\n")
    write_tables(out, per_run, comparisons)
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
