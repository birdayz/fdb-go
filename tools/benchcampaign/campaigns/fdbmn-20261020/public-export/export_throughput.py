#!/usr/bin/env python3
"""Post-hoc throughput from retained clean cases; never rerun a benchmark."""
import argparse
import csv
import hashlib
import importlib.util
import io
import json
import math
import os
from pathlib import Path
import random
import statistics
import sys
import tempfile

from export_public_results import VerifiedInputs, validate_public
from m2_throughput import collect as collect_m2

SEED, RESAMPLES, BLOCKS = 492817, 20000, 6
ARMS = ("before", "final", "cgo")
CONTRASTS = (("final", "before"), ("final", "cgo"), ("before", "cgo"))
CLIENT_ANALYZER = "frozen/tools/clientbench/scripts/analyze.py"
CLIENT_RUNNER = "frozen/tools/clientbench/scripts/run.py"
M2_ANALYZER = "tooling/tools/benchcampaign/singlenode/bench_analyze.py"
M2_RUNNER = "tooling/tools/benchcampaign/singlenode/bench_runner.py"


def need(condition, message):
    if not condition:
        raise ValueError(message)


def positive(value, label):
    need(isinstance(value, (int, float)) and math.isfinite(value) and value > 0,
         "nonpositive/nonfinite " + label)
    return value


def validate_samples(text, count, workers, wall):
    rows = list(csv.DictReader(io.StringIO(text)))
    need(len(rows) == count and sorted(int(r["index"]) for r in rows) == list(range(count)),
         "missing/duplicate measured sample indices")
    intervals = {w: [] for w in range(workers)}
    total, retries = 0, 0
    for r in rows:
        index, worker, start, latency, attempts = (int(r[k]) for k in
            ("index", "worker", "start_offset_ns", "latency_ns", "attempts"))
        need(r["phase"] == "measure" and r["error"] == "" and r["check_status"] == "ok"
             and worker == index % workers and latency > 0 and start >= 0
             and start + latency <= wall and attempts >= 1, "invalid measured sample")
        intervals[worker].append((start, start + latency))
        total += latency
        retries += attempts - 1
    for spans in intervals.values():
        spans.sort()
        need(all(a[1] <= b[0] for a, b in zip(spans, spans[1:])),
             "overlapping operations on a closed-loop worker")
    return total, retries


def measured_process(raw, retained, samples):
    """Use measured-phase counters, never lifecycle E-A or process lifetime."""
    ops, wall = raw["completed"], positive(raw["measured_wall_ns"], "measured wall")
    need(raw["success"] is True and raw["errors"] == raw["warmup_errors"] == 0
         and raw["persisted_validation"] == "ok"
         and ops == raw["requested"] == retained["successes"] == retained["observations"]
         and retained["errors"] == 0, "incomplete or failed measured process")
    positive(ops, "measured operations")
    config = raw["config"]
    need((config["workload"], config["workers"], config["run_id"], config["block"])
         == (retained["workload"], retained["workers"], retained["run_id"], retained["block"]),
         "summary identity differs from retained analysis")
    before, after = raw["stats_before"], raw["stats_after"]
    keys = ("process_user_cpu_us", "process_system_cpu_us")
    need(all(math.isfinite(s[k]) and s[k] >= 0 for s in (before, after) for k in keys)
         and all(after[k] >= before[k] for k in keys), "invalid measured CPU counters")
    cpu_ns = positive(1000 * sum(after[k] - before[k] for k in keys), "measured CPU")
    rate, utilization = ops * 1e9 / wall, cpu_ns / wall
    expected = {
        "measured_wall_ns": wall, "process_cpu_ns": cpu_ns,
        "closed_loop_ops_per_second": rate, "process_cpu_over_wall": utilization,
    }
    need(all(math.isclose(retained[k], v, rel_tol=1e-12, abs_tol=1e-12)
             for k, v in expected.items()), "raw/retained measured counters differ")
    workers = positive(retained["workers"], "workers")
    total_latency, retries = validate_samples(samples, ops, workers, wall)
    need(math.isclose(total_latency / ops, retained["mean_ns"], rel_tol=1e-12, abs_tol=1e-6)
         and retries == raw["retries"] == retained["retries"], "raw latency/retry aggregates differ")
    predicted = workers * ops * 1e9 / total_latency
    consistency = total_latency / (workers * wall)
    # Worker bookkeeping and end-of-window imbalance can only reduce this ratio.
    need(0 < consistency <= 1 + 1e-9, "closed-loop accounting exceeds worker-time budget")
    return dict(record_type="process", arm=retained["arm"], block=retained["block"],
                position=retained["position"], run_id=retained["run_id"], status="ok",
                runs=1, workers=retained["workers"], measured_ops=ops, measured_wall_ns=wall,
                window_cpu_ns=cpu_ns, throughput_ops_per_second=rate, cpu_util_cores=utilization,
                mean_latency_ns=total_latency / ops, measured_latency_sum_ns=total_latency,
                retries=retries, in_operation_concurrency=total_latency / wall,
                workers_over_mean_ops_per_second=predicted,
                closed_loop_throughput_ratio=consistency)


def paired_comparisons(processes, bootstrap):
    observed = [(r["block"], r["arm"]) for r in processes]
    expected = {(b, arm) for b in range(BLOCKS) for arm in ARMS}
    need(len(observed) == 18 and set(observed) == expected,
         "not a complete six-block three-arm process population")
    lookup = {(r["block"], r["arm"]): r for r in processes}
    output = []
    for numerator, denominator in CONTRASTS:
        pairs = [(lookup[b, numerator]["throughput_ops_per_second"],
                  lookup[b, denominator]["throughput_ops_per_second"]) for b in range(BLOCKS)]
        # Restarting the same seed preserves the same whole-block draws in all contrasts.
        estimate = bootstrap(pairs, random.Random(SEED), RESAMPLES)
        need(estimate["blocks"] == BLOCKS, "bootstrap changed the block population")
        output.append(dict(record_type="paired", status="ok", metric="throughput_ops_per_second",
                           numerator=numerator, denominator=denominator, runs=18,
                           seed=SEED, resamples=RESAMPLES,
                           ratio_gt_1_means="numerator has more measured operations/s", **estimate))
    return output


def collect_m1(inputs, bootstrap):
    protocol = inputs.load("deploy/protocol.json")
    specs = {p["id"]: p for p in protocol["phases"] if p["id"].startswith("M1w")}
    need(set(specs) == {"M1w1", "M1w8", "M1w32", "M1w64"}, "wrong M1 phase population")
    output, factors, clean = [], [], 0
    for phase in ("M1w1", "M1w8", "M1w32", "M1w64"):
        spec = specs[phase]
        if phase == "M1w32":
            output.extend(dict(record_type="omitted", phase=phase, case=case, status="N/A",
                               reason="budget-unexecuted; no diagnostic/capacity substitution")
                          for case in spec["workloads"])
            continue
        analysis = inputs.load(f"analysis/measured-492817/{phase}/summary.json")["workloads"]
        r4 = inputs.load(f"reconcile/{phase}/report.json")
        need(r4["reconciliation_complete"] is True and r4["phase"] == phase, "incomplete R4")
        cases = {c["case"]: c for c in r4["cases"]}
        need(len(cases) == len(r4["cases"]) and set(cases) == set(analysis) == set(spec["workloads"]),
             "M1 analysis/R4/protocol case population differs")
        for case in spec["workloads"]:
            retained, rec = analysis[case], cases[case]
            if retained["status"] != "clean":
                need(rec["verdict"] != "clean", "clean R4 case missing retained estimates")
                output.append(dict(record_type="omitted", phase=phase, case=case, status="N/A",
                                   reason="R4 " + rec["verdict"] + "; no partial-arm/block salvage"))
                continue
            need(rec["verdict"] == "clean", "non-clean case cannot enter throughput")
            source = retained["per_run"]
            schedule = inputs.load(f"results-raw/{phase}/{case}/schedule.json")
            expected_ids = {r["run_id"] for r in source}
            need(len(source) == len(expected_ids) == len(schedule) == 18
                 and {r["run_id"] for r in schedule} == expected_ids
                 and len(rec["runs"]) == 18 and {r["run_id"] for r in rec["runs"]} == expected_ids,
                 "raw schedule/R4/analysis process populations differ")
            jobs = {r["run_id"]: r for r in schedule}
            processes = []
            for r in sorted(source, key=lambda r: (r["block"], r["position"])):
                job = jobs[r["run_id"]]
                need((job["block"], job["position"], job["arm"], job["workload"]["name"], job["workload"]["workers"])
                     == (r["block"], r["position"], r["arm"], case, spec["workers"])
                     and r["workload"] == case and r["workers"] == spec["workers"],
                     "raw schedule/analysis identity differs")
                raw = inputs.load(f"results-raw/{phase}/{case}/{r['run_id']}/summary.json")
                samples = inputs.text(f"results-raw/{phase}/{case}/{r['run_id']}/samples.csv")
                row = measured_process(raw, r, samples)
                need(raw["requested"] == job["workload"]["iterations"]
                     and raw["warmup_completed"] == job["workload"]["warmup"], "wrong iteration geometry")
                processes.append(row)
                factors.append(row["closed_loop_throughput_ratio"])
            paired = paired_comparisons(processes, bootstrap)
            absolutes = []
            for arm in ARMS:
                rows = [r for r in processes if r["arm"] == arm]
                absolutes.append(dict(record_type="absolute", arm=arm, status="ok", runs=6,
                    workers=spec["workers"], **{key: statistics.median(r[key] for r in rows) for key in
                    ("throughput_ops_per_second", "cpu_util_cores", "closed_loop_throughput_ratio")}))
            output.extend(dict(phase=phase, case=case, **r) for r in processes + absolutes + paired)
            clean += 1
    need(clean == 14 and len(factors) == 252, "wrong eligible M1 population")
    return output, dict(eligible_cases=clean, measured_processes=len(factors),
                        closed_loop_ratio_min=min(factors), closed_loop_ratio_max=max(factors),
                        closed_loop_ratio_median=statistics.median(factors),
                        closed_loop_ratio_definition="measured throughput / (workers * 1e9 / retained per_run.mean_ns)")


def load_module(inputs, source, dependency):
    """Import only SHA-pinned retained numerical helpers, with their pinned dependency."""
    dep_name = Path(dependency).stem
    for name, path in ((dep_name, dependency), ("throughput_" + Path(source).stem, source)):
        inputs.digest(path)
        spec = importlib.util.spec_from_file_location(name, inputs.path(path))
        module = importlib.util.module_from_spec(spec)
        sys.modules[name] = module
        spec.loader.exec_module(module)
    return module


def csv_text(rows):
    rows = [dict(post_hoc="true", preregistered="false", **row) for row in rows]
    columns = list(dict.fromkeys(k for row in rows for k in row))
    stream = io.StringIO(newline="")
    writer = csv.DictWriter(stream, fieldnames=columns, lineterminator="\n")
    writer.writeheader()
    for row in rows:
        writer.writerow({k: json.dumps(v, sort_keys=True, separators=(",", ":"))
                         if isinstance(v, (dict, list)) else v for k, v in row.items()})
    return stream.getvalue()


def render_table(rows):
    out = ["|Case|Ops/s previous / new / CGo|New/previous [95%]|New/CGo [95%]|Previous/CGo [95%]|Busy cores previous / new / CGo|\n",
           "|---|---:|---|---|---|---:|\n"]
    cells = list(dict.fromkeys((r["phase"], r["case"]) for r in rows if r["record_type"] == "absolute"))
    for phase, case in cells:
        cell = [r for r in rows if (r["phase"], r["case"]) == (phase, case)]
        absolute = {r["arm"]: r for r in cell if r["record_type"] == "absolute"}
        paired = {(r["numerator"], r["denominator"]): r for r in cell if r["record_type"] == "paired"}
        rates = " / ".join(f"{absolute[a]['throughput_ops_per_second']:,.0f}" for a in ARMS)
        cores = " / ".join(f"{absolute[a]['cpu_util_cores']:.3f}" for a in ARMS)
        ratios = []
        for contrast in CONTRASTS:
            r = paired[contrast]
            ratios.append(f"{r['ratio']:.3f} [{r['ci95_low']:.3f}, {r['ci95_high']:.3f}]")
        out.append(f"|{phase[2:]}/{case}|{rates}|" + "|".join(ratios) + f"|{cores}|\n")
    return "".join(out)


def export(inputs, redactions_path):
    inputs.verify_all()
    client = load_module(inputs, CLIENT_ANALYZER, CLIENT_RUNNER)
    testingb = load_module(inputs, M2_ANALYZER, M2_RUNNER)
    m1, m1_meta = collect_m1(inputs, client.bootstrap_log_ratio)
    m2, m2_meta = collect_m2(inputs, testingb.paired_gm)
    input_pins = "".join(f"{digest}  {name}\n" for name, digest in sorted(inputs.digests.items()))
    method = dict(post_hoc=True, preregistered=False, seed=SEED, whole_paired_block_resamples=RESAMPLES,
        input_manifest_sha256=hashlib.sha256(input_pins.encode()).hexdigest(),
        inference="Exploratory nominal 95% paired-block percentile intervals; six blocks, no multiplicity adjustment, no capacity/scaling/equivalence claim.",
        M1=dict(m1_meta, throughput="summary.completed * 1e9 / summary.measured_wall_ns; all eligible runs have completed=requested=successful measured CSV rows and zero errors",
            cpu_util_cores="1000 * sum(stats_after[k] - stats_before[k], k=process_user_cpu_us,process_system_cpu_us) / measured_wall_ns",
            CPU_scope="RUSAGE_SELF user+system bracket around the measured phase, including Go and native threads; CPU reads precede/follow the exact wall fences slightly. Not lifecycle E-A; deferred native cleanup can fall outside this window.",
            absolutes="Median of six process rates or core-utilization values per arm; ratios are geometric means of six within-block rate ratios, not ratios of absolute medians.",
            closed_loop="workers / mean latency is an approximate prediction; worker bookkeeping and end-of-window imbalance reduce actual rate. Integer raw latency sums, complete indices, worker assignment, nonoverlap and wall containment are checked; every process factor is retained in the CSV."),
        M2=m2_meta, numerical_helper_sha256={p: inputs.digest(p) for p in (CLIENT_ANALYZER, CLIENT_RUNNER, M2_ANALYZER, M2_RUNNER)})
    files = {"analysis/M1-throughput.csv": csv_text(m1), "analysis/M2-throughput.csv": csv_text(m2),
             "analysis/throughput-method.json": json.dumps(method, indent=2, sort_keys=True) + "\n"}
    table = render_table(m1)
    validate_public(dict(files, **{"throughput-table.md": table}), inputs.load(redactions_path))
    inputs.verify_all()
    return files, table


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--campaign", type=Path, default=os.environ.get("FDB_PERF_HOME"),
                        help="retained campaign directory (not its parent)")
    parser.add_argument("--inputs-manifest", type=Path, required=True, help="SHA256 paths relative to campaign")
    parser.add_argument("--output-dir", type=Path, required=True, help="fresh output directory")
    parser.add_argument("--redactions", default="provenance/public-export-redactions.json")
    args = parser.parse_args()
    if args.campaign is None:
        parser.error("set FDB_PERF_HOME or --campaign to the retained campaign directory")
    try:
        need(not args.output_dir.exists(), "output directory must not exist")
        files, table = export(VerifiedInputs(args.campaign, args.inputs_manifest), args.redactions)
        args.output_dir.parent.mkdir(parents=True, exist_ok=True)
        with tempfile.TemporaryDirectory(prefix="throughput-", dir=args.output_dir.parent) as temp:
            staging = Path(temp) / "public"
            for name, text in files.items():
                path = staging / name
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_text(text, encoding="utf-8")
            staging.rename(args.output_dir)
        sys.stdout.write(table)
    except (ValueError, OSError, KeyError, AssertionError) as exc:
        parser.exit(1, f"throughput export refused: {exc}\n")


if __name__ == "__main__":
    main()
