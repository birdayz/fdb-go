#!/usr/bin/env python3
"""Read sealed Rev4 eight-worker range metric rows only; do not rescan samples."""

import argparse
import hashlib
import json
from pathlib import Path
import statistics

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument("--rev4-evidence", required=True, type=Path)
parser.add_argument("--output", required=True, type=Path)
args = parser.parse_args()
root = args.rev4_evidence.resolve()
index = root / "run-metrics.jsonl"
rows = [json.loads(line) for line in index.read_text().splitlines()]
rows = [row for row in rows if row["phase"] == "rev4-screen4-w8" and
        row["workload"] == "range100" and row["arm"] in ("baseline", "reclaim")]
if len(rows) != 6 or {(row["pair"], row["arm"]) for row in rows} != {
    (pair, arm) for pair in range(3) for arm in ("baseline", "reclaim")
}:
    raise ValueError("expected exactly three nonempty baseline/reclaim pairs")
for row in rows:
    summary = json.loads((root / "timed" / row["run"] / "summary.json").read_text())
    if not (summary["success"] and summary["completed"] == summary["requested"] == 20000
            and summary["errors"] == summary["retries"] == 0
            and summary["config"]["workers"] == 8 and summary["profile"] == "none"
            and summary["backend"]["executable_sha256"] == row["sha"]):
        raise ValueError("unexpected run population or identity: " + row["run"])
    row["source"] = summary["config"]["source_revision"]
    row["profile"] = summary["profile"]
    row["completed"] = summary["completed"]
    row["warmup_excluded"] = summary["warmup_completed"]
    row["summary_sha256"] = hashlib.sha256(
        (root / "timed" / row["run"] / "summary.json").read_bytes()).hexdigest()
changes = []
for pair in range(3):
    base = next(row for row in rows if row["pair"] == pair and row["arm"] == "baseline")
    candidate = next(row for row in rows if row["pair"] == pair and row["arm"] == "reclaim")
    changes.append({"pair": pair, **{
        key: 100 * (candidate[key] / base[key] - 1) for key in ("mean_us", "p50_us", "p99_us")
    }})
medians = {arm: {key: statistics.median(row[key] for row in rows if row["arm"] == arm)
                 for key in ("mean_us", "p50_us", "p99_us")}
           for arm in ("baseline", "reclaim")}
result = {
    "scope": "separate historical Rev4 eight-worker primary range cohort; metrics/summary only",
    "samples_rescanned": False, "phase": "rev4-screen4-w8", "profile": "none",
    "metric_index": str(index), "metric_index_sha256": hashlib.sha256(index.read_bytes()).hexdigest(),
    "script_sha256": hashlib.sha256(Path(__file__).read_bytes()).hexdigest(),
    "rows": rows, "paired_changes_percent": changes, "per_run_medians_us": medians,
    "ratio_of_per_run_medians_percent": {
        key: 100 * (medians["reclaim"][key] / medians["baseline"][key] - 1)
        for key in medians["baseline"]
    },
    "caveat": "Not pooled with 32-worker, profiled, or later Rev5 populations; no causal attribution.",
}
args.output.write_text(json.dumps(result, indent=2, sort_keys=True) + "\n")
print(json.dumps({"output": str(args.output), "sha256": hashlib.sha256(args.output.read_bytes()).hexdigest(),
                  "changes_percent": changes, "medians_us": medians,
                  "ratio_of_medians_percent": result["ratio_of_per_run_medians_percent"]}, indent=2))
