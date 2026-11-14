"""Versioned producer/cross-reader envelopes; no provider or ingestion effects."""
import copy
import hashlib
import json
import math
from pathlib import Path

import driver as d
import policy as p

TELEMETRY = ("process_cpu_ns", "peak_rss_bytes", "network_bytes", "disk_bytes", "allocations",
             "commit_latency_ns", "grv_latency_ns", "read_latency_ns", "transaction_latency_ns")
INDEXES = {"SPANS_TIME", "SPANS_SPAN_ID", "SPANS_STR_ATTR", "SPANS_LONG_STR_ATTR", "SPANS_INT_ATTR",
           "SPANS_DOUBLE_ATTR", "SPANS_BOOL_ATTR", "SPANS_REF_CHUNKS"}
DISTINCT_BASIS = "census_verified_distinct_spans"


def unavailable(reason):
    return {"availability": "unavailable", "reason": reason}


COUNTERS = ("acked_span_operations", "committed_batches", "native_retries", "outer_retries", "export_attempts", "queue_retries")
GO_UNOBSERVABLE = frozenset(COUNTERS[:4])


def counter(value, name, arm):
    p.require(type(value) is dict, "explicit counter availability required: " + name)
    if value.get("availability") == "available":
        p.require(set(value) == {"availability", "value"} and p.integer(value["value"]), "exact nonnegative counter required: " + name)
        p.require(arm == "rust_native" or name not in GO_UNOBSERVABLE, "Go cannot expose complete counter: " + name)
    else:
        p.require(set(value) == {"availability", "reason"} and value.get("availability") == "unavailable"
                  and type(value["reason"]) is str and value["reason"].strip(), "unavailable counter requires a reason: " + name)
        p.require(name != "acked_span_operations" or arm != "rust_native", "Rust acknowledged operations must be available")
        if arm != "rust_native" and name in GO_UNOBSERVABLE:
            reason = value["reason"].lower()
            if arm == "go_pure" and name == "native_retries":
                p.require("not applicable" in reason, "pure-Go native retries are not applicable, not an observed zero")
            else:
                p.require(("not exposed" in reason or "does not expose" in reason) and "go collector" in reason, "Go counter reason must identify unexposed collector telemetry")
    return copy.deepcopy(value)


def ack_operations(value, arm):
    return counter(value, "acked_span_operations", arm)


def ingest_cpu_seconds(report):
    value = report.get("ingest_resources")
    p.require("ingest_resources_unavailable_reason" not in report and type(value) is dict and value.get("scope") == "first_read_through_flush"
              and all(p.number(value.get(k)) for k in ("user_cpu_seconds", "system_cpu_seconds", "cpu_seconds"))
              and math.isclose(value["cpu_seconds"], value["user_cpu_seconds"] + value["system_cpu_seconds"], rel_tol=1e-12, abs_tol=1e-12)
              and p.integer(value.get("peak_rss_bytes"), 1) and type(value.get("measurement_source")) is str
              and value["measurement_source"].strip(), "matching ingest-window process CPU required")
    return value["cpu_seconds"]


def ingest_resource_evidence(report):
    """Validate measured resources or an explicit early-failure absence, never a fallback."""
    p.require("ingest_resources" in report, "explicit ingest-window measurement or early-failure absence required")
    if report["ingest_resources"] is None:
        p.require(report.get("status") == "error" and type(report.get("error")) is str and report["error"].strip()
                  and type(report.get("ingest_resources_unavailable_reason")) is str
                  and report["ingest_resources_unavailable_reason"].strip(), "only a failed receipt may explain an absent ingest-window measurement")
        return None
    return ingest_cpu_seconds(report)


def cpu_per_distinct(seconds, distinct):
    return {"availability": "available", "value": seconds * 1000 / distinct}


def job_seal(job):
    return hashlib.sha256((json.dumps(job, sort_keys=True) + "\n").encode()).hexdigest()


def native_mapping(report, job, *, paid_host=False):
    native = job["mode"] == "ingest" and job["arm"] != "go_pure"
    p.require(type(report.get("native_library_mapped")) is bool and report["native_library_mapped"] is native,
              "executing binary must attest actual native-library mapping or absence")
    if native:
        p.require(report.get("client_library_sha256") == job["client_library_sha256"]
                  and report.get("native_mapping_rule") in (("exact_stat",) if paid_host else ("exact_stat", "btrfs_superblock")),
                  "sealed native mapping required; paid hosts require exact_stat")
    else:
        p.require("native_mapping_rule" not in report, "pure executable must not claim a native mapping check")
    return True


def common(report, job, mode):
    p.require(type(report) is dict and report.get("schema") == "otel_control_report_v1"
              and report.get("mode") == mode and report.get("arm") == job["arm"] and report.get("status") == "ok"
              and "error" in report and report["error"] in (None, "") and report.get("effective_job") == job
              and report.get("job_sha256") == job_seal(job), "genuine sealed producer path/effective job required")
    for key in ("source_revision", "source_manifest_sha256", "binary_sha256", "corpus_sha256", "layout_sha256", "client_library_sha256"):
        p.require(report.get(key) == job[key], "report/job seal mismatch: " + key)
    native_mapping(report, job)


def envelope(job, report, *, paid_host=False):
    p.require(type(report) is dict, "producer report object required")
    value = {"schema": "otel_paired_ingest_envelope_v1", "arm": job["arm"], "workload": copy.deepcopy(job["workload"]),
             "job_sha256": job_seal(job), "raw_report": copy.deepcopy(report), "accepted": False,
             "rate_eligible": False, "paid_host": paid_host, "telemetry": {key: {"availability": "unavailable"} for key in TELEMETRY}}
    try:
        ingest_resource_evidence(report)
        common(report, job, "ingest")
        p.require(type(paid_host) is bool, "explicit host mapping-policy context required")
        native_mapping(report, job, paid_host=paid_host)
        p.require(all(report.get(key) is None for key in ("spans_per_second", "rate_numerator", "cpu_ms_per_distinct_span", "cpu_denominator")),
                  "ingest alone cannot assert census-verified rates or CPU/span")
        count = job["expected_frames"]
        for key in ("frames", "received_spans", "accepted_spans"):
            p.require(type(report.get(key)) is int and report[key] == count, "complete source admission population")
        for key in ("rejected_spans", "failed_spans", "queued_spans_at_exit"):
            p.require(type(report.get(key)) is int and report[key] == 0, "no rejected/failed/unsettled spans")
        operations = ack_operations(report.get("acked_span_operations"), job["arm"])
        p.require((operations["availability"] == "unavailable" or operations["value"] >= count)
                  and p.integer(report.get("ingest_wall_ns"), 1), "nonempty completed pipeline population")
        for key in COUNTERS:
            metric = counter(report.get(key), key, job["arm"])
            if key == "committed_batches" and metric["availability"] == "available":
                p.require(metric["value"] > 0, "nonempty committed batch population")
            if job["arm"] != "rust_native" and job["workload"]["kind"] == "collector_queue" and key in ("export_attempts", "queue_retries"):
                p.require(metric["availability"] == "available", "Go public collector Exports/Retries required")
            value[key] = metric
        ingest_cpu_seconds(report)
        for key in ("admission_phase_ns", "drain_ns"):
            p.require(p.integer(report.get(key)), "explicit timing population required")
        samples = report.get("samples")
        p.require(type(samples) is list and samples, "nonempty raw call population")
        for sample in samples:
            keys = {"ns", "received_spans", "accepted_spans", "error"}
            direct = job["workload"]["kind"] == "writer_direct"
            p.require(type(sample) is dict and keys <= set(sample) <= (keys | {"direct_write_acked_operations"} if direct else keys)
                      and p.integer(sample.get("ns")) and p.integer(sample.get("received_spans"), 1)
                      and type(sample.get("accepted_spans")) is int and sample["accepted_spans"] == sample["received_spans"]
                      and sample.get("error") in (None, ""), "failed/uncertain call or ambiguous admission/ACK sample")
            if "direct_write_acked_operations" in sample:
                p.require(p.integer(sample["direct_write_acked_operations"], sample["received_spans"]), "known direct-write ACK population required")
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


def joined_envelope(job, report, verification, namespace, *, control_identity=None, paid_host=False):
    value = envelope(job, report, paid_host=paid_host)
    p.require(value["accepted"], value.get("error", "invalid ingest"))
    verify_audit(verification, job, namespace, control_identity=control_identity)
    distinct = verification["report"]["distinct_spans"]
    value.update(schema="otel_paired_result_v1", distinct_spans=distinct, rate_numerator=DISTINCT_BASIS,
                 rate_eligible=True, spans_per_second=distinct * 10 ** 9 / report["ingest_wall_ns"],
                 cpu_denominator=DISTINCT_BASIS, cpu_ms_per_distinct_span=cpu_per_distinct(ingest_cpu_seconds(report), distinct),
                 raw_verification_report=copy.deepcopy(verification["report"]))
    validate_distinct_result(value)
    return value


def same_metric(actual, expected):
    if type(actual) is not dict or set(actual) != set(expected) or actual.get("availability") != expected["availability"]:
        return False
    if expected["availability"] == "available":
        return p.number(actual["value"]) and actual["value"] == expected["value"]
    return actual == expected


def validate_distinct_result(value):
    p.require(type(value) is dict and value.get("schema") == "otel_paired_result_v1"
              and value.get("accepted") is True and value.get("rate_eligible") is True
              and value.get("arm") in ("go_pure", "go_native", "rust_native")
              and type(value.get("raw_report")) is dict and type(value.get("raw_verification_report")) is dict
              and p.integer(value.get("distinct_spans"), 1) and value.get("rate_numerator") == DISTINCT_BASIS
              and value.get("cpu_denominator") == DISTINCT_BASIS, "accepted rates and CPU must use census-verified distinct spans")
    distinct, report, verification = value["distinct_spans"], value["raw_report"], value["raw_verification_report"]
    p.require(type(verification.get("distinct_spans")) is int and verification["distinct_spans"] == distinct
              and verification.get("census", {}).get("matched") is True
              and type(verification["census"].get("actual", {}).get("span_record_identities")) is int
              and verification["census"]["actual"]["span_record_identities"] == distinct
              and type(report.get("frames")) is int and report["frames"] == distinct,
              "nonempty independent census is the rate numerator")
    p.require(type(report.get("effective_job")) is dict and report["effective_job"].get("arm") == value["arm"]
              and report["effective_job"].get("workload") == value.get("workload")
              and report.get("job_sha256") == value.get("job_sha256") and type(value.get("paid_host")) is bool
              and envelope(report["effective_job"], report, paid_host=value["paid_host"])["accepted"],
              "joined result requires a complete admitted ingest report")
    for key in COUNTERS:
        p.require(counter(value.get(key), key, value["arm"]) == report.get(key), "original counter availability must be preserved: " + key)
    p.require(p.number(value.get("spans_per_second")) and p.integer(report.get("ingest_wall_ns"), 1)
              and value["spans_per_second"] == distinct * 10 ** 9 / report["ingest_wall_ns"],
              "rate was not computed from census-verified distinct spans")
    p.require(same_metric(value.get("cpu_ms_per_distinct_span"), cpu_per_distinct(ingest_cpu_seconds(report), distinct)),
              "CPU/span was not computed from census-verified distinct spans")
    return True


def aggregate_ack_operations(results):
    operations = [ack_operations(value["acked_span_operations"], value["arm"]) for value in results]
    if any(value["availability"] == "unavailable" for value in operations):
        return unavailable("one or more producers do not expose acknowledged write operations including replays")
    return {"availability": "available", "value": sum(value["value"] for value in operations)}


def aggregate_cpu(results):
    seconds = [ingest_cpu_seconds(value["raw_report"]) for value in results]
    return sum(seconds)


def wave_metrics(results, start_ns, ack_end_ns, drained_end_ns):
    p.require(type(results) is list and results, "nonempty verified producer population required")
    for value in results:
        validate_distinct_result(value)
    distinct = sum(value["distinct_spans"] for value in results)
    value = {"rows": distinct, "distinct_spans": distinct, "rate_numerator": DISTINCT_BASIS,
             "acked_span_operations": aggregate_ack_operations(results), "cpu_denominator": DISTINCT_BASIS,
             "cpu_ms_per_distinct_span": cpu_per_distinct(aggregate_cpu(results), distinct),
             "release_ns": start_ns, "ack_end_ns": ack_end_ns, "drained_end_ns": drained_end_ns,
             "ack_rows_per_second": p.common_throughput(distinct, start_ns, ack_end_ns),
             "drained_rows_per_second": p.common_throughput(distinct, start_ns, drained_end_ns)}
    validate_wave_metrics(value, results)
    return value


def validate_wave_metrics(value, results):
    p.require(type(value) is dict and type(results) is list and results
              and all(type(entry) is dict and entry.get("arm") in ("go_pure", "go_native", "rust_native") for entry in results)
              and len({entry["arm"] for entry in results}) == 1
              and all(p.hexstr(entry.get("job_sha256")) for entry in results)
              and len({entry["job_sha256"] for entry in results}) == len(results)
              and all(entry["workload"] == results[0]["workload"] for entry in results), "one matched implementation wave with distinct producer jobs required")
    for entry in results:
        validate_distinct_result(entry)
    distinct = sum(entry["distinct_spans"] for entry in results)
    p.require(type(value.get("distinct_spans")) is int and value["distinct_spans"] == distinct
              and type(value.get("rows")) is int and value["rows"] == distinct
              and value.get("rate_numerator") == DISTINCT_BASIS and value.get("cpu_denominator") == DISTINCT_BASIS,
              "wave rate/CPU denominators must be census-verified distinct spans")
    p.require(p.integer(value.get("ack_end_ns"), 1) and p.integer(value.get("drained_end_ns"), 1)
              and value["ack_end_ns"] <= value["drained_end_ns"], "ordered common release/ACK/drain clocks required")
    for key, end in (("ack_rows_per_second", value["ack_end_ns"]), ("drained_rows_per_second", value["drained_end_ns"])):
        expected = p.common_throughput(distinct, value["release_ns"], end)
        p.require(p.number(value.get(key)) and value[key] == expected, "wave rate used a non-distinct numerator")
    p.require(value.get("acked_span_operations") == aggregate_ack_operations(results)
              and same_metric(value.get("cpu_ms_per_distinct_span"), cpu_per_distinct(aggregate_cpu(results), distinct)),
              "wave ACK operations and CPU/distinct span must remain separate")
    return True


def verification_job(job, output=None, results_file=None):
    return {**copy.deepcopy(job), "mode": "verify", "output": output or str(Path(job["output"]).with_name("verify.json")),
            "verification": {"query_oracle": "source_1600_v1", "query_results_file": results_file or str(Path(job["output"]).with_name("queries.ndjson")),
                             "k": 100, "max_candidates": 2000000, "max_bytes": 268435456}}


def oracle_bytes(queries):
    return "".join(json.dumps(row, sort_keys=True) + "\n" for row in queries).encode()


def verify_audit(value, job, namespace, *, control_identity=None):
    p.require(type(value) is dict, "complete verifier wrapper required")
    expected_job = verification_job(job)
    if control_identity is not None:
        expected_job.update(control_identity)
        p.require(value.get("verification_job") == expected_job and value.get("ingest_job_sha256") == job_seal(job),
                  "Go verifier/original ingest job seal join required")
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
    seen = set()
    for i, sample in enumerate(queries):
        p.require(type(sample) is dict and type(sample.get("ordinal")) is int and sample["ordinal"] == i
                  and sample.get("class") in by_class and sample.get("matched") is True
                  and p.integer(sample.get("ns")) and sample.get("error") in (None, "")
                  and type(sample.get("iteration_in_class")) is int and 0 <= sample["iteration_in_class"] < 100
                  and type(sample.get("query")) is dict and type(sample.get("expected_results")) is list
                  and all(type(result) is dict for result in sample["expected_results"])
                  and sample["expected_results"] == sample.get("actual_results")
                  and p.hexstr(sample.get("expected_digest")) and sample["expected_digest"] == sample.get("actual_digest"),
                  "source-oracle ordered results mismatch")
        key = (sample["class"], sample["iteration_in_class"])
        p.require(key not in seen, "duplicate source-oracle class/iteration")
        seen.add(key)
        by_class[sample["class"]] += 1
    p.require(any(sample["expected_results"] for sample in queries), "nonempty source-oracle answer population required")
    p.require(all(n == 100 for n in by_class.values()) and oracle.get("classes") ==
              {name: {"expected": 100, "executed": 100, "passed": 100} for name in p.CLASSES}, "16 classes x100 required")
    raw = value.get("query_raw")
    if raw is not None:
        p.require(type(raw) is str and raw.endswith("\n") and [d.loads(line) for line in raw.splitlines()] == queries,
                  "original NDJSON bytes must match retained parsed queries")
        raw = raw.encode()
    else:
        raw = oracle_bytes(queries)
    p.require(oracle.get("results_sha256") == hashlib.sha256(raw).hexdigest()
              and oracle.get("results_file") == expected_job["verification"]["query_results_file"], "sealed raw oracle artifact required")
    return True


def fake_common(job):
    native = job["mode"] == "ingest" and job["arm"] != "go_pure"
    return {"native_library_mapped": native, **({"native_mapping_rule": "exact_stat"} if native else {}),
            "schema": "otel_control_report_v1", "mode": job["mode"], "arm": job["arm"], "status": "ok", "error": None,
            "effective_job": copy.deepcopy(job), "job_sha256": job_seal(job),
            **{key: job[key] for key in ("source_revision", "source_manifest_sha256", "binary_sha256", "corpus_sha256", "layout_sha256", "client_library_sha256")}}


def fake_ingest(job):
    count = job["expected_frames"]
    size = job["workload"]["producer_batch_spans"]
    samples = [{"ns": 1, "received_spans": min(size, count-offset), "accepted_spans": min(size, count-offset), "error": None,
                **({"direct_write_acked_operations": min(size, count-offset)} if job["workload"]["kind"] == "writer_direct" else {})}
               for offset in range(0, count, size)]
    counters = {key: {"availability": "available", "value": value} for key, value in
                zip(COUNTERS, (count, (count+63)//64, 0, 0, (count+63)//64, 0))}
    if job["arm"] != "rust_native":
        for key in GO_UNOBSERVABLE:
            counters[key] = unavailable("not applicable to this arm: pure Go has no native client" if key == "native_retries" and job["arm"] == "go_pure"
                                        else "not exposed by the Go collector: " + key)
    return {**fake_common(job), "frames": count, "received_spans": count, "accepted_spans": count, "rejected_spans": 0,
            **counters, "failed_spans": 0, "queued_spans_at_exit": 0,
            "ingest_resources": {"scope": "first_read_through_flush", "user_cpu_seconds": 1.5, "system_cpu_seconds": 0.5,
                                 "cpu_seconds": 2.0, "peak_rss_bytes": 4096, "measurement_source": "synthetic fake phase snapshot"},
            "ingest_wall_ns": 1000000000, "admission_phase_ns": 900000000, "drain_ns": 100000000, "samples": samples}


def fake_verify(job, *, control_identity=None):
    verify = verification_job(job)
    if control_identity is not None:
        verify.update(control_identity)
    count = job["expected_frames"]
    queries = [{"ordinal": i, "class": p.CLASSES[i % 16], "iteration_in_class": i // 16, "query": {"fixture": i},
                "expected_results": [{"fixture_result": i}] if p.CLASSES[i % 16] == "time_broad" else [],
                "actual_results": [{"fixture_result": i}] if p.CLASSES[i % 16] == "time_broad" else [],
                "expected_digest": "c" * 64, "actual_digest": "c" * 64,
                "matched": True, "error": None, "ns": 1} for i in range(1600)]
    fixture = p.census(count)
    counts = {"header_kvs": 1, "span_record_identities": count, "physical_span_record_kvs": count,
              "baseline_record_identities": 1, "baseline_record_kvs": 1, "unexpected_kvs": 0,
              "index_entries": {key: fixture.get(key, 0) for key in INDEXES}, "baseline_index_entries": {"STORE_META_NAME": 1}}
    return {"verification_job": verify, "ingest_job_sha256": job_seal(job), "report": {**fake_common(verify), **{key: count for key in ("source_records_expected", "source_records_read_standard_serializer",
                       "source_records_exact_match", "scanned_distinct_spans", "distinct_spans")}, "otlp_bytes_equal": True,
                       "full_rows_equal": True, "layout_seals_equal": True, "census": {"expected": counts, "actual": copy.deepcopy(counts), "matched": True},
                       "oracle": {"kind": "source_1600_v1", "status": "passed", "required_queries": 1600, "executed_queries": 1600,
                                  "passed_queries": 1600, "failed_queries": 0, "classes": {key: {"expected": 100, "executed": 100, "passed": 100} for key in p.CLASSES},
                                  "results_file": verify["verification"]["query_results_file"], "results_sha256": hashlib.sha256(oracle_bytes(queries)).hexdigest()}}, "queries": queries}
