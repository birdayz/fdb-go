#!/usr/bin/env python3
"""Partition existing rendered raw CPU profiles; never runs pprof or a workload.

This preserves the parser, frame regexes, and first-match category precedence
used in phase1-code-review's RFC-263 attribution. Each sample record contributes
its CPU weight exactly once. Categories match any resolved frame in a stack,
including inline frames: they are caller-context partitions, not flat/self-time
function groups. Descendants of a matched frame belong to that category too.

Run with: uv run --no-project python exclusive-cpu-analysis.py
The input/output directory defaults to this script's directory.
"""

import argparse
import hashlib
import json
import re
from collections import defaultdict
from pathlib import Path

# Exact category regexes and precedence used for the original reported table.
CATEGORY_PATTERNS = [
    (
        "GC_mark_assist_sweep",
        r"^runtime\.(?:gcDrain|gcBgMarkWorker|gcAssistAlloc|gcStart|gcMarkDone|gcSweep|sweepone|bgsweep)",
    ),
    (
        "write_barrier_stacks",
        r"^(?:runtime\.(?:wbBufFlush|bulkBarrierPreWrite|wbMove)|gcWriteBarrier)",
    ),
    (
        "timeout_and_cleanup_admin",
        r"(?:\(\*timeoutQueue(?:Root)?\)|\(\*realTimer\)|\(\*Transaction\)\.(?:armTimeoutLocked|armReadTimeoutLocked|configureReadTimeout|armTimebombLocked|disarmTimebombLocked|stopTimebombLocked|fireTimebomb|fireTimeout|fireReadTimeout)|runtime\.(?:runCleanups|runFinalizers|runfinq|addCleanup)|runtime\.AddCleanup)",
    ),
    (
        "other_allocation_stacks",
        r"^runtime\.(?:mallocgc|newobject|growslice|makeslice|\(\*mcache\)|\(\*mheap\)|\(\*mcentral\))",
    ),
]
REMAINDER = "all_other_including_unknown"
# This does not change top-level precedence. It subdivides only records whose
# first-match category is write_barrier_stacks.
QUEUE_BARRIER_PATTERN = r"\(\*timeoutQueue(?:Root)?\)"
BARRIER_SUBCATEGORIES = ("queue_context", "other_mutator_context")
EXPECTED = {
    "baseline": {"sample_records": 1844, "cpu_ns": 30120000000},
    "merged": {"sample_records": 2025, "cpu_ns": 33090000000},
}
TRANSACTIONS = 500000


def parse_profile(path, arm, patterns):
    raw = path.read_bytes()
    text = raw.decode("utf-8")
    sampletext, loctext = text.split("Locations\n", 1)
    loctext = loctext.split("Mappings\n", 1)[0]
    period = int(re.search(r"^Period: (\d+)$", sampletext, re.M)[1])
    assert period == 10000000, (arm, "unexpected sampling period", period)

    # Location IDs map to all functions at that PC, leaf inline frame first.
    loc = {}
    current = None
    for line in loctext.splitlines():
        match = re.match(r"\s*(\d+):\s+\S+\s+M=\d+\s+(\S+)", line)
        if match:
            current = int(match[1])
            loc[current] = [match[2]]
        elif current is not None and line.strip():
            loc[current].append(line.split()[0])

    totals = defaultdict(int)
    records_by_category = defaultdict(int)
    barrier_subtotals = defaultdict(int)
    barrier_subrecords = defaultdict(int)
    queue_barrier_pattern = re.compile(QUEUE_BARRIER_PATTERN)
    records = sample_count = unknown_records = unknown_cpu_ns = 0
    for line in sampletext.splitlines():
        # Empty location lists are genuine unknown-stack samples, not omissions.
        match = re.match(r"^\s*(\d+)\s+(\d+):\s*([\d ]*)$", line)
        if not match:
            assert not re.match(r"\s*\d+\s+\d+:", line), (
                arm, "unparsed sample record", line
            )
            continue
        count, ns = int(match[1]), int(match[2])
        assert ns == count * period, (arm, "sample weight/period mismatch", line)
        stack = [function for ident in match[3].split() for function in loc[int(ident)]]
        records += 1
        sample_count += count
        group = next(
            (
                name
                for name, pattern in patterns
                if any(pattern.search(function) for function in stack)
            ),
            REMAINDER,
        )
        totals[group] += ns
        records_by_category[group] += 1
        if group == "write_barrier_stacks":
            subgroup = (
                "queue_context"
                if any(queue_barrier_pattern.search(function) for function in stack)
                else "other_mutator_context"
            )
            barrier_subtotals[subgroup] += ns
            barrier_subrecords[subgroup] += 1
        if not stack:
            unknown_records += 1
            unknown_cpu_ns += ns

    assert records == EXPECTED[arm]["sample_records"], (arm, records)
    assert sum(totals.values()) == EXPECTED[arm]["cpu_ns"], (arm, dict(totals))
    assert sum(records_by_category.values()) == records, (arm, "record conservation")
    assert sample_count * period == sum(totals.values()), (arm, "CPU conservation")
    assert sum(barrier_subtotals.values()) == totals["write_barrier_stacks"], (
        arm, "write-barrier subpartition CPU conservation"
    )
    assert sum(barrier_subrecords.values()) == records_by_category["write_barrier_stacks"], (
        arm, "write-barrier subpartition record conservation"
    )
    return {
        "path": str(path),
        "sha256": hashlib.sha256(raw).hexdigest(),
        "sample_records": records,
        "sample_count": sample_count,
        "period_ns": period,
        "cpu_ns": sum(totals.values()),
        "cpu_seconds": sum(totals.values()) / 1e9,
        "unknown_stack_records": unknown_records,
        "unknown_stack_cpu_ns": unknown_cpu_ns,
        "category_cpu_ns": dict(totals),
        "category_sample_records": dict(records_by_category),
        "write_barrier_subpartition_cpu_ns": dict(barrier_subtotals),
        "write_barrier_subpartition_sample_records": dict(barrier_subrecords),
        "assertions_passed": [
            "expected_nonempty_record_population",
            "all_sample_records_parsed",
            "per_record_CPU_equals_count_times_period",
            "record_count_conserved_across_exclusive_categories",
            "expected_full_CPU_total_conserved_across_exclusive_categories",
            "write_barrier_subpartition_CPU_and_records_conserved",
        ],
    }


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--directory", type=Path, default=Path(__file__).resolve().parent)
    args = parser.parse_args()
    directory = args.directory.resolve()
    patterns = [(name, re.compile(pattern)) for name, pattern in CATEGORY_PATTERNS]
    profiles = {
        arm: parse_profile(directory / f"attribution-{arm}-range32-cpu-raw.txt", arm, patterns)
        for arm in ("baseline", "merged")
    }
    rows = []
    for group in [*[name for name, _ in patterns], REMAINDER]:
        baseline = profiles["baseline"]["category_cpu_ns"].get(group, 0)
        merged = profiles["merged"]["category_cpu_ns"].get(group, 0)
        delta = merged - baseline
        rows.append({
            "category": group,
            "baseline_cpu_ns": baseline,
            "merged_cpu_ns": merged,
            "delta_cpu_ns": delta,
            "baseline_cpu_seconds": baseline / 1e9,
            "merged_cpu_seconds": merged / 1e9,
            "delta_cpu_seconds": delta / 1e9,
            "delta_us_per_transaction": delta / TRANSACTIONS / 1000,
        })
    delta_ns = profiles["merged"]["cpu_ns"] - profiles["baseline"]["cpu_ns"]
    assert delta_ns == 2970000000, delta_ns
    assert sum(row["delta_cpu_ns"] for row in rows) == delta_ns
    assert sum(row["baseline_cpu_ns"] for row in rows) == profiles["baseline"]["cpu_ns"]
    assert sum(row["merged_cpu_ns"] for row in rows) == profiles["merged"]["cpu_ns"]

    lines = []
    for arm, profile in profiles.items():
        lines.append(
            f"{arm}: records={profile['sample_records']}, total_ns={profile['cpu_ns']}, "
            f"total_s={profile['cpu_seconds']:.2f}"
        )
    lines.append("group baseline_s merged_s delta_s delta_us_per_tx")
    for row in rows:
        lines.append(
            f"{row['category']} {row['baseline_cpu_seconds']:.2f} "
            f"{row['merged_cpu_seconds']:.2f} {row['delta_cpu_seconds']:+.2f} "
            f"{row['delta_us_per_transaction']:+.2f}"
        )
    lines.append(
        f"TOTAL {profiles['baseline']['cpu_seconds']:.2f} "
        f"{profiles['merged']['cpu_seconds']:.2f} {delta_ns / 1e9:+.2f} "
        f"{delta_ns / TRANSACTIONS / 1000:+.2f}"
    )
    barrier_rows = []
    lines.append("WRITE_BARRIER_SUBPARTITION baseline_s merged_s delta_s delta_us_per_tx")
    for subgroup in BARRIER_SUBCATEGORIES:
        baseline = profiles["baseline"]["write_barrier_subpartition_cpu_ns"].get(subgroup, 0)
        merged = profiles["merged"]["write_barrier_subpartition_cpu_ns"].get(subgroup, 0)
        delta = merged - baseline
        barrier_rows.append({
            "category": subgroup,
            "baseline_cpu_ns": baseline,
            "merged_cpu_ns": merged,
            "delta_cpu_ns": delta,
            "baseline_cpu_seconds": baseline / 1e9,
            "merged_cpu_seconds": merged / 1e9,
            "delta_cpu_seconds": delta / 1e9,
            "delta_us_per_transaction": delta / TRANSACTIONS / 1000,
        })
        lines.append(
            f"{subgroup} {baseline / 1e9:.2f} {merged / 1e9:.2f} "
            f"{delta / 1e9:+.2f} {delta / TRANSACTIONS / 1000:+.2f}"
        )
    barrier_delta = next(row["delta_cpu_ns"] for row in rows if row["category"] == "write_barrier_stacks")
    assert sum(row["delta_cpu_ns"] for row in barrier_rows) == barrier_delta
    output_text = "\n".join(lines) + "\n"
    analysis = {
        "method": "First matching category wins; each full raw-profile sample record is assigned exactly once.",
        "matching_scope": "Any resolved stack frame, inline frames included; descendant CPU is included. These are caller-context groups, not flat function self-time groups.",
        "precedence": [
            {"category": name, "regex": pattern} for name, pattern in CATEGORY_PATTERNS
        ] + [{"category": REMAINDER, "regex": None}],
        "unknown_stack_handling": "Empty-stack sample weights are included in the remainder, not discarded.",
        "transactions_per_arm": TRANSACTIONS,
        "profiles": profiles,
        "rows": rows,
        "write_barrier_subpartition": {
            "parent_category": "write_barrier_stacks",
            "queue_context_regex": QUEUE_BARRIER_PATTERN,
            "method": "Within the existing write-barrier category only: any timeoutQueue/timeoutQueueRoot frame selects queue_context; all other records select other_mutator_context.",
            "caveat": "Queue context includes descendants of queue functions, not just pointer-slot/swap/grow writes. Non-queue barrier CPU is not directly removed merely by changing queue backing storage to noscan.",
            "rows": barrier_rows,
            "assertion_passed": "subcategory_CPU_and_records_reconcile_to_parent; subcategory_deltas_reconcile_to_parent_delta",
        },
        "total_delta_cpu_ns": delta_ns,
        "total_delta_cpu_seconds": delta_ns / 1e9,
        "total_delta_us_per_transaction": delta_ns / TRANSACTIONS / 1000,
        "assertions_passed": [
            "all_category_CPU_totals_reconcile_to_both_full_raw_profiles",
            "all_category_deltas_reconcile_to_2_970_000_000_ns",
        ],
        "scope_caveat": "Sampled CPU attribution does not by itself identify retained object types or establish the causal retention mechanism. Cumulative function deltas must not be added to these exclusive group totals.",
        "output_text": output_text,
    }
    output = directory / "exclusive-cpu-analysis.json"
    output.write_text(json.dumps(analysis, indent=2) + "\n", encoding="utf-8")
    print(output_text, end="")


if __name__ == "__main__":
    main()
