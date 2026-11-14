"""Versioned producer/cross-reader envelopes; no provider or ingestion effects."""
import copy
import hashlib
import json
from pathlib import Path

import policy as p

TELEMETRY = ("process_cpu_ns", "peak_rss_bytes", "network_bytes", "disk_bytes", "allocations",
             "commit_latency_ns", "grv_latency_ns", "read_latency_ns", "transaction_latency_ns")
INDEXES = {"SPANS_TIME", "SPANS_SPAN_ID", "SPANS_STR_ATTR", "SPANS_LONG_STR_ATTR", "SPANS_INT_ATTR",
           "SPANS_DOUBLE_ATTR", "SPANS_BOOL_ATTR", "SPANS_REF_CHUNKS"}


def job_seal(job):
    return hashlib.sha256((json.dumps(job, sort_keys=True) + "\n").encode()).hexdigest()


def common(report, job, mode):
    p.require(type(report) is dict and report.get("schema") == "otel_control_report_v1"
              and report.get("mode") == mode and report.get("arm") == job["arm"] and report.get("status") == "ok"
              and report.get("error") in (None, "") and report.get("effective_job") == job
              and report.get("job_sha256") == job_seal(job), "genuine sealed producer path/effective job required")
    for key in ("source_revision", "source_manifest_sha256", "binary_sha256", "corpus_sha256", "layout_sha256", "client_library_sha256"):
        p.require(report.get(key) == job[key], "report/job seal mismatch: " + key)


def envelope(job, report):
    p.require(type(report) is dict, "producer report object required")
    value = {"schema": "otel_paired_result_v1", "arm": job["arm"], "workload": copy.deepcopy(job["workload"]),
             "job_sha256": job_seal(job), "raw_report": copy.deepcopy(report), "accepted": False,
             "rate_eligible": False, "telemetry": {key: {"availability": "unavailable"} for key in TELEMETRY}}
    try:
        common(report, job, "ingest")
        count = job["expected_frames"]
        for key in ("frames", "accepted_spans"):
            p.require(type(report.get(key)) is int and report[key] == count, "complete source admission population")
        for key in ("rejected_spans", "failed_spans", "queued_spans_at_exit"):
            p.require(type(report.get(key)) is int and report[key] == 0, "no rejected/failed/unsettled spans")
        p.require(p.integer(report.get("acked_span_operations"), count) and p.integer(report.get("committed_batches"), 1)
                  and p.integer(report.get("ingest_wall_ns"), 1), "nonempty commit-backed pipeline completion")
        for key in ("native_retries", "outer_retries", "queue_retries", "admission_phase_ns", "drain_ns"):
            p.require(p.integer(report.get(key)), "explicit retry/timing population required")
        samples = report.get("samples")
        p.require(type(samples) is list and samples, "nonempty raw call population")
        for sample in samples:
            p.require(type(sample) is dict and p.integer(sample.get("ns")) and p.integer(sample.get("received_spans"), 1)
                      and type(sample.get("accepted_spans")) is int and sample["accepted_spans"] == sample["received_spans"]
                      and p.integer(sample.get("acked_span_operations")) and sample.get("error") in (None, ""), "failed/uncertain call")
        p.require(sum(x["accepted_spans"] for x in samples) == count, "raw calls account for every admitted span")
        optional = report.get("telemetry", {})
        p.require(type(optional) is dict and not set(optional) - set(TELEMETRY), "known optional telemetry fields")
        for key, metric in optional.items():
            if metric is not None:
                p.require(p.integer(metric), "nonnegative exact telemetry counter")
                value["telemetry"][key] = {"availability": "available", "value": metric}
        value["accepted"] = True
    except (ValueError, KeyError, TypeError) as exc:
        value["error"] = str(exc)
    return value


def verification_job(job, output=None, results_file=None):
    return {**copy.deepcopy(job), "mode": "verify", "output": output or str(Path(job["output"]).with_name("verify.json")),
            "verification": {"query_oracle": "source_1600_v1", "query_results_file": results_file or str(Path(job["output"]).with_name("queries.ndjson")),
                             "k": 100, "max_candidates": 2000000, "max_bytes": 268435456}}


def oracle_bytes(queries):
    return "".join(json.dumps(row, sort_keys=True) + "\n" for row in queries).encode()


def verify_audit(value, job, namespace):
    p.require(type(value) is dict, "complete verifier wrapper required")
    expected_job = verification_job(job)
    report, queries = value.get("report"), value.get("queries")
    common(report, expected_job, "verify")
    count = job["expected_frames"]
    for key in ("source_records_expected", "source_records_read_standard_serializer", "source_records_exact_match",
                "scanned_distinct_spans", "distinct_spans"):
        p.require(type(report.get(key)) is int and report[key] == count, "every distinct source row must be cross-read")
    p.require(all(report.get(key) is True for key in ("otlp_bytes_equal", "full_rows_equal", "layout_seals_equal")),
              "full source bytes/rows/layout verification required")
    census = report.get("census")
    p.require(type(census) is dict and census.get("matched") is True and census.get("expected") == census.get("actual"),
              "exact independently derived census required")
    counts = census.get("actual")
    fields = {"header_kvs", "span_record_identities", "physical_span_record_kvs", "baseline_record_identities",
              "baseline_record_kvs", "index_entries", "baseline_index_entries", "unexpected_kvs"}
    p.require(type(counts) is dict and set(counts) == fields and all(p.integer(counts[k]) for k in fields - {"index_entries", "baseline_index_entries"})
              and counts["span_record_identities"] == count and counts["physical_span_record_kvs"] >= count
              and counts["header_kvs"] == 1 and counts["baseline_record_identities"] == 1
              and counts["baseline_record_kvs"] >= 1 and counts["unexpected_kvs"] == 0, "nonvacuous tenant/baseline census")
    indexes = counts.get("index_entries")
    p.require(type(indexes) is dict and set(indexes) == INDEXES and all(p.integer(x) for x in indexes.values())
              and indexes["SPANS_TIME"] == count and indexes["SPANS_SPAN_ID"] == count, "complete eight-index census")
    baseline = counts.get("baseline_index_entries")
    p.require(type(baseline) is dict and baseline == {"STORE_META_NAME": 1}
              and type(baseline["STORE_META_NAME"]) is int, "production metadata baseline index required")
    oracle = report.get("oracle")
    p.require(type(oracle) is dict and oracle.get("kind") == "source_1600_v1" and oracle.get("status") == "passed"
              and all(type(oracle.get(k)) is int and oracle[k] == v for k, v in
                      (("required_queries", 1600), ("executed_queries", 1600), ("passed_queries", 1600), ("failed_queries", 0))),
              "paid gate requires all1600 source-oracle queries")
    p.require(type(queries) is list and len(queries) == 1600, "complete raw independent oracle sidecar")
    by_class = {name: 0 for name in p.CLASSES}
    for i, sample in enumerate(queries):
        p.require(type(sample) is dict and type(sample.get("ordinal")) is int and sample["ordinal"] == i
                  and sample.get("class") in by_class and sample.get("matched") is True
                  and p.integer(sample.get("ns")) and sample.get("error") in (None, "")
                  and type(sample.get("expected_results")) is list and sample["expected_results"] == sample.get("actual_results")
                  and p.hexstr(sample.get("expected_digest")) and sample["expected_digest"] == sample.get("actual_digest"),
                  "source-oracle ordered results mismatch")
        by_class[sample["class"]] += 1
    p.require(all(n == 100 for n in by_class.values()) and oracle.get("classes") ==
              {name: {"expected": 100, "executed": 100, "passed": 100} for name in p.CLASSES}, "16 classes x100 required")
    p.require(oracle.get("results_sha256") == hashlib.sha256(oracle_bytes(queries)).hexdigest()
              and oracle.get("results_file") == expected_job["verification"]["query_results_file"], "sealed raw oracle artifact required")
    return True


def fake_common(job):
    return {"schema": "otel_control_report_v1", "mode": job["mode"], "arm": job["arm"], "status": "ok", "error": None,
            "effective_job": copy.deepcopy(job), "job_sha256": job_seal(job),
            **{key: job[key] for key in ("source_revision", "source_manifest_sha256", "binary_sha256", "corpus_sha256", "layout_sha256", "client_library_sha256")}}


def fake_ingest(job):
    count = job["expected_frames"]
    size = job["workload"]["producer_batch_spans"]
    samples = [{"ns": 1, "received_spans": min(size, count-offset), "accepted_spans": min(size, count-offset),
                "acked_span_operations": min(size, count-offset), "error": None} for offset in range(0, count, size)]
    return {**fake_common(job), "frames": count, "accepted_spans": count, "rejected_spans": 0,
            "acked_span_operations": count, "committed_batches": (count+63)//64, "export_attempts": (count+63)//64,
            "native_retries": 0, "outer_retries": 0, "queue_retries": 0, "failed_spans": 0, "queued_spans_at_exit": 0,
            "ingest_wall_ns": 1000000000, "admission_phase_ns": 900000000, "drain_ns": 100000000, "samples": samples}


def fake_verify(job):
    verify = verification_job(job)
    count = job["expected_frames"]
    queries = [{"ordinal": i, "class": p.CLASSES[i % 16], "iteration_in_class": i // 16, "query": {"fixture": i},
                "expected_results": [], "actual_results": [], "expected_digest": "c" * 64, "actual_digest": "c" * 64,
                "matched": True, "error": None, "ns": 1} for i in range(1600)]
    fixture = p.census(count)
    counts = {"header_kvs": 1, "span_record_identities": count, "physical_span_record_kvs": count,
              "baseline_record_identities": 1, "baseline_record_kvs": 1, "unexpected_kvs": 0,
              "index_entries": {key: fixture.get(key, 0) for key in INDEXES}, "baseline_index_entries": {"STORE_META_NAME": 1}}
    return {"report": {**fake_common(verify), **{key: count for key in ("source_records_expected", "source_records_read_standard_serializer",
                       "source_records_exact_match", "scanned_distinct_spans", "distinct_spans")}, "otlp_bytes_equal": True,
                       "full_rows_equal": True, "layout_seals_equal": True, "census": {"expected": counts, "actual": copy.deepcopy(counts), "matched": True},
                       "oracle": {"kind": "source_1600_v1", "status": "passed", "required_queries": 1600, "executed_queries": 1600,
                                  "passed_queries": 1600, "failed_queries": 0, "classes": {key: {"expected": 100, "executed": 100, "passed": 100} for key in p.CLASSES},
                                  "results_file": verify["verification"]["query_results_file"], "results_sha256": hashlib.sha256(oracle_bytes(queries)).hexdigest()}}, "queries": queries}
