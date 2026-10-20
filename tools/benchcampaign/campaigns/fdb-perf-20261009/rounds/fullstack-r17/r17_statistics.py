#!/usr/bin/env python3
"""R17 preregistered joint circular moving-block bootstrap; no acquisition code."""
import hashlib
import math
import random
import struct

BLOCKS = 160
RESAMPLES = 100_000
PRIMARY = (
    ("batchset50", "shutdown_process_cpu_ns_per_op"),
    ("batchset50", "process_cpu_ns_per_op"),
    ("read1000", "mean_ns"),
    ("read1000", "p99_ns"),
    ("read1000", "shutdown_process_cpu_ns_per_op"),
)
BOOTSTRAPS = ((8, 797707), (16, 797708))


def require(condition, message):
    if not condition:
        raise ValueError(message)


def nearest_rank(sorted_values, probability):
    require(bool(sorted_values) and 0 <= probability <= 1, "invalid nearest-rank inputs")
    return sorted_values[max(0, math.ceil(probability * len(sorted_values)) - 1)]


def validate_logs(logs):
    require(len(logs) == BLOCKS, "R17 requires exactly 160 global-block vectors")
    require(all(len(row) == len(PRIMARY) for row in logs), "R17 requires exactly five jointly paired columns")
    require(all(type(value) in (int, float) and math.isfinite(value) for row in logs for value in row),
            "paired logs must be finite numeric values")


def circular_bootstrap(logs, length, seed, resamples=RESAMPLES):
    """One shared index stream for all five columns, preserving global-block identity.

    Precomputed circular window sums are algebraically the same resampled mean
    as expanding all 160 indices. math.fsum bounds accumulation roundoff.
    """
    validate_logs(logs)
    require(length in (8, 16) and BLOCKS % length == 0, "only preregistered lengths 8 and 16")
    require(type(seed) is int and type(resamples) is int and resamples > 0, "invalid seed/resample count")
    columns = len(PRIMARY)
    windows = [tuple(math.fsum(logs[(start + offset) % BLOCKS][column] for offset in range(length))
                     for column in range(columns)) for start in range(BLOCKS)]
    rng = random.Random(seed)
    samples = [[] for _ in range(columns)]
    index_digest = hashlib.sha256()
    for _ in range(resamples):
        starts = [rng.randrange(BLOCKS) for _ in range(BLOCKS // length)]
        index_digest.update(bytes(starts))
        for column in range(columns):
            value = math.exp(math.fsum(windows[start][column] for start in starts) / BLOCKS)
            require(math.isfinite(value) and value > 0, "nonfinite bootstrap ratio")
            samples[column].append(value)
    distribution_digests = []
    intervals = []
    for values in samples:
        distribution_digests.append(hashlib.sha256(b"".join(struct.pack("!d", value) for value in values)).hexdigest())
        values.sort()
        intervals.append({"ci99_low": nearest_rank(values, 0.005), "ci99_high": nearest_rank(values, 0.995)})
    return {
        "method": "joint circular moving-block percentile bootstrap of paired log ratios",
        "global_blocks": BLOCKS, "length": length, "starts_per_resample": BLOCKS // length,
        "seed": seed, "resamples": resamples, "confidence": 0.99,
        "nearest_rank_zero_based_indices": [max(0, math.ceil(p * resamples) - 1) for p in (0.005, 0.995)],
        "index_stream_sha256": index_digest.hexdigest(),
        "distribution_sha256_float64_be_before_sort": distribution_digests,
        "intervals": intervals,
    }


def decisions(logs, bootstrap_results, validity=True):
    validate_logs(logs)
    require([(result["length"], result["seed"], result["resamples"]) for result in bootstrap_results]
            == [(length, seed, RESAMPLES) for length, seed in BOOTSTRAPS], "wrong primary bootstrap protocol")
    rows = []
    for column, (workload, metric) in enumerate(PRIMARY):
        point = math.exp(math.fsum(row[column] for row in logs) / BLOCKS)
        l8, l16 = [result["intervals"][column] for result in bootstrap_results]
        confirmed = validity and l8["ci99_high"] < 1 and l16["ci99_high"] < 1
        rows.append({
            "endpoint": column + 1, "workload": workload, "metric": metric,
            "numerator": "new", "denominator": "cgo", "paired_blocks": BLOCKS,
            "geometric_mean_ratio": point,
            "L8_ci99_low": l8["ci99_low"], "L8_ci99_high": l8["ci99_high"],
            "L16_ci99_low": l16["ci99_low"], "L16_ci99_high": l16["ci99_high"],
            "pairs_below_one": sum(row[column] < 0 for row in logs),
            "all_data_and_host_gates_passed": validity,
            "improvement_confirmed": confirmed,
            "decision": "confirmed improvement" if confirmed else "not confirmed/inconclusive",
        })
    return rows
