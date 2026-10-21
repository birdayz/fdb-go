#!/usr/bin/env python3
"""Recompute sealed RFC-263 historical tails; never run a workload or pool cohorts."""

import argparse
import csv
import hashlib
import json
import math
from pathlib import Path
import statistics


BASELINE = "fa8725f1dcf6ff3ac66825ab6e3e819efbc0abcc"
PHASE1 = "tree:50432211dc325d7854dbdf72b24c986f424ccbd5"
REV4 = "tree:61d2597e0231dfbedcd898a27c078c3412efaf6a"
HARNESS = "cfc3a33191ec10b4e0955b3f0f192b1ba3be554104ea61016c2a6e60aabbf400"
BINARIES = {
    BASELINE: "2dd943fdc9d58ff136449a181189ba5843465e86e6ae696d616a9b1bac4a439e",
    PHASE1: "56a78c0d7454a0222db4d5f1f1de863cdb0b9d75672a4d741a376d90480a4e3c",
    REV4: "2f0993bd332a9d7085b1c1053659b68867286de95ad1cbe2bf91fe327f088e95",
}
QUANTILES = {"p50_us": 0.5, "p90_us": 0.9, "p99_us": 0.99, "p999_us": 0.999}


def require(condition, message):
    if not condition:
        raise ValueError(message)


def read_json(path):
    return json.loads(path.read_text())


def artifact(path):
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(chunk)
    return {"path": str(path), "sha256": digest.hexdigest()}


def load_metrics(root):
    result = {}
    for line, text in enumerate((root / "run-metrics.jsonl").read_text().splitlines(), 1):
        row = json.loads(text)
        require(row["run"] not in result, "duplicate metric run: " + row["run"])
        result[row["run"]] = (row, line)
    return result


def load_run(root, metrics, prefix, pair, arm, source, count, profile):
    run = f"{prefix}-range100-p{pair}-{arm}"
    directory = root / "timed" / run
    summary = read_json(directory / "summary.json")
    manifest = read_json(directory / "manifest.json")
    row, line = metrics[run]
    config = summary["config"]
    require(config == manifest["config"], run + ": summary/manifest configuration mismatch")
    for key, expected in {
        "run_id": run, "workload": "range100", "workers": 32,
        "iterations": count, "warmup": 2000, "source_revision": source,
        "harness_id": HARNESS, "transaction_timeout_ms": 30000,
        "retry_limit": 10, "profile": profile,
    }.items():
        require(config[key] == expected, f"{run}: unexpected {key}: {config[key]}")
    require(summary["profile"] == profile, run + ": unexpected summary profile")
    require(summary["success"] and summary["failure"] == "", run + ": unsuccessful run")
    require(summary["completed"] == summary["requested"] == count, run + ": incomplete run")
    require(summary["errors"] == summary["retries"] == 0, run + ": errors or retries")
    require(summary["warmup_completed"] == 2000, run + ": incomplete warmup")
    require(manifest["go_version"] == "go1.26.9" and manifest["gomaxprocs"] == 4,
            run + ": unexpected Go toolchain or GOMAXPROCS")
    require(manifest["environment"] == {
        "GODEBUG": "", "GOGC": "100", "GOMAXPROCS": "4", "GOMEMLIMIT": "off",
    }, run + ": unexpected environment")
    expected_sha = BINARIES[source]
    require(summary["backend"]["executable_sha256"] ==
            manifest["backend"]["executable_sha256"] == row["sha"] == expected_sha,
            run + ": executable identity mismatch")
    require(row["phase"] == prefix and row["workload"] == "range100" and
            row["pair"] == pair and row["arm"] == arm, run + ": metric row mismatch")

    latencies = []
    workers = set()
    with (directory / "samples.csv").open(newline="") as stream:
        for sample in csv.DictReader(stream):
            require(sample["phase"] == "measure" and not sample["error"] and
                    sample["check_status"] == "ok", run + ": invalid measured sample")
            latency = int(sample["latency_ns"])
            require(latency >= 0, run + ": negative latency")
            latencies.append(latency)
            workers.add(int(sample["worker"]))
    require(len(latencies) == count and count > 0, run + ": missing/extra sample rows")
    require(workers == set(range(32)), run + ": missing worker population")
    latencies.sort()
    quantiles = {
        name: latencies[int((count - 1) * probability)] / 1000
        for name, probability in QUANTILES.items()
    }
    for name in ("p50_us", "p99_us"):
        require(quantiles[name] == row[name], run + ": retained quantile mismatch: " + name)
    mean_us = statistics.mean(latencies) / 1000
    require(math.isclose(mean_us, row["mean_us"], abs_tol=1e-6, rel_tol=0),
            run + ": retained mean mismatch")
    before, after = summary["stats_before"], summary["stats_after"]
    cpu_us = sum(after[key] - before[key] for key in
                 ("process_user_cpu_us", "process_system_cpu_us")) / count
    require(math.isclose(cpu_us, row["cpu_us"], abs_tol=1e-9, rel_tol=0),
            run + ": retained measured-window CPU mismatch")
    return {
        "run": run, "pair": pair, "arm": arm, "source": source,
        "executable_sha256": expected_sha, "profile": profile,
        "completed": count, "warmup_excluded": 2000, "errors": 0, "retries": 0,
        "workers": 32, "started_utc": manifest["started_utc"],
        "timing_scope": manifest["timing"], "go_version": manifest["go_version"],
        "environment": manifest["environment"], "harness_id": HARNESS,
        "cluster_file": config["cluster_file"], "loadavg": row["loadavg"],
        "idle_fraction": row["idle"], "metrics_line": line,
        "latency": dict(quantiles, mean_us=mean_us), "cpu_us": cpu_us,
        "inputs": {name: artifact(directory / name) for name in
                   ("samples.csv", "summary.json", "manifest.json")},
    }


def compare_cohort(root, metrics, prefix, arm, source, pairs, count, profile):
    runs = []
    changes = []
    by_arm = {"baseline": [], arm: []}
    for pair in range(pairs):
        base = load_run(root, metrics, prefix, pair, "baseline", BASELINE, count, profile)
        candidate = load_run(root, metrics, prefix, pair, arm, source, count, profile)
        runs.extend((base, candidate))
        by_arm["baseline"].append(base)
        by_arm[arm].append(candidate)
        changes.append({
            "pair": pair,
            "latency_percent": {
                key: 100 * (candidate["latency"][key] / base["latency"][key] - 1)
                for key in base["latency"]
            },
        })
    medians = {
        name: {key: statistics.median(run["latency"][key] for run in rows)
               for key in rows[0]["latency"]}
        for name, rows in by_arm.items()
    }
    return {
        "phase": prefix, "candidate_arm": arm, "profile": profile,
        "pair_count": pairs, "measured_per_run": count, "runs": runs,
        "paired_changes": changes, "per_run_medians_us": medians,
        "ratio_of_per_run_medians_percent": {
            key: 100 * (medians[arm][key] / medians["baseline"][key] - 1)
            for key in medians[arm]
        },
        "median_paired_change_percent": {
            key: statistics.median(change["latency_percent"][key] for change in changes)
            for key in medians[arm]
        },
    }


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--rev4-evidence", required=True, type=Path)
    parser.add_argument("--phase1-evidence", required=True, type=Path)
    parser.add_argument("--output", required=True, type=Path,
                        help="JSON artifact path outside the source checkout")
    args = parser.parse_args()
    rev4, phase1 = args.rev4_evidence.resolve(), args.phase1_evidence.resolve()
    output = args.output.resolve()
    repository = Path(__file__).resolve().parents[4]
    require(repository not in output.parents, "raw JSON must be outside the source checkout")
    metrics = {root: load_metrics(root) for root in (rev4, phase1)}
    identities = []
    for root, name, source, archive_sha, binary in (
        (phase1, "baseline-source.tar.gz", BASELINE,
         "1dc00ff709355227c05c6f5b7317674c49e97f2274b68a72914a74436450e697", "baseline"),
        (phase1, "candidate-source.tar.gz", PHASE1,
         "4ded661e094092314474e14b9020874b9a32011efbaf8fe6bd239b7c317861b9", "candidate"),
        (rev4, "reclaim-source.tar.gz", REV4,
         "4c3ec931e75bbece2e8e4556766e18360d785435f732d861ed6c1c4760b5f175", "reclaim"),
    ):
        archive = artifact(root / name)
        executable = artifact(root / "bin" / binary)
        require(archive["sha256"] == archive_sha, "changed sealed source archive: " + name)
        require(executable["sha256"] == BINARIES[source], "changed executable: " + binary)
        identities.append({"source": source, "archive": archive, "executable": executable})
    # Rev4's baseline is a separate retained copy of the same executable.
    rev4_baseline = artifact(rev4 / "bin" / "baseline")
    require(rev4_baseline["sha256"] == BINARIES[BASELINE], "changed Rev4 baseline executable")
    cohorts = [
        compare_cohort(rev4, metrics[rev4], "rev4-range4-w32", "reclaim", REV4, 3, 500000, "none"),
        compare_cohort(rev4, metrics[rev4], "rev4-mutex-draining4-w32", "reclaim", REV4, 1, 500000, "mutex"),
        compare_cohort(phase1, metrics[phase1], "mutex-w32", "candidate", PHASE1, 1, 100000, "mutex"),
        compare_cohort(phase1, metrics[phase1], "mutex-draining-w32", "candidate", PHASE1, 1, 500000, "mutex"),
    ]
    # A nonempty search population precedes the negative unprofiled-Phase1 claim.
    phase1_range32 = sorted(
        path.name for path in (phase1 / "timed").iterdir()
        if path.is_dir() and "w32-range100" in path.name
    )
    selected_phase1 = sorted(run["run"] for cohort in cohorts[2:] for run in cohort["runs"])
    require(phase1_range32 == selected_phase1,
            "Phase1 32-worker range population changed; inspect before extending the comparison")
    result = {
        "schema_version": 1, "scope": "retained historical data only; no new workload or attribution",
        "quantile_definition": "sorted_latency_ns[int((n - 1) * p)] / 1000",
        "aggregation": "per-run quantiles; never pool samples, phases, or profiling modes",
        "script": artifact(Path(__file__).resolve()), "source_identities": identities,
        "rev4_baseline_executable": rev4_baseline,
        "metric_indexes": [artifact(root / "run-metrics.jsonl") for root in (rev4, phase1)],
        "phase1_range32_directory_census": phase1_range32,
        "cohorts": cohorts,
        "caveats": [
            "Rev4 unprofiled p99 has two adverse pairs and one favorable pair, not an all-pairs regression.",
            "Phase1 has only two separate, single-pair mutex-profiled 32-worker range populations.",
            "Phase1 short candidate idle fraction is about 0.899 versus baseline 0.981.",
            "No causal attribution to the later Rev5 renewal change follows from these separate campaigns.",
        ],
    }
    output.write_text(json.dumps(result, indent=2, sort_keys=True, allow_nan=False) + "\n")
    print(json.dumps({"output": artifact(output), "runs": sum(len(c["runs"]) for c in cohorts),
                      "measured_samples": sum(r["completed"] for c in cohorts for r in c["runs"]),
                      "cohorts": [{"phase": c["phase"], "profile": c["profile"],
                                   "p99_ratio_of_medians_percent": c["ratio_of_per_run_medians_percent"]["p99_us"],
                                   "p99_median_paired_percent": c["median_paired_change_percent"]["p99_us"]}
                                  for c in cohorts]}, indent=2))


if __name__ == "__main__":
    main()
