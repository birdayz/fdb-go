#!/usr/bin/env python3
"""Sealed multi-implementation native campaign. Fake mode never imports a provider."""
import argparse
import copy
from decimal import Decimal
import hashlib
import gzip
import json
import os
from pathlib import Path
import re
import time

import driver as d
import policy as p
import paired_builds as builds
import paired_cost as cost
import paired_reports as reports
import paired_rows as rows

SCHEMA = "otel_native_paired_v1"
ARMS = ("go_pure", "go_native", "rust_native")
ARM_EXECUTABLES = {"go_pure": "control_pure_binary", "go_native": "control_native_binary", "rust_native": "rust_binary"}
NATIVE_SHA256 = "617572467897d0360f76ffc33f24b58090ee691e3273750d2ff334cc71aae3fa"
QUEUE_FIELDS = {"kind", "producers", "producer_batch_spans", "consumers", "queue_capacity_spans", "batch_min_spans", "batch_max_spans", "flush_ms",
                "block_on_overflow", "export_timeout_ms", "retry_enabled", "retry_initial_ms", "retry_max_ms", "retry_elapsed_ms"}
BASE_ASSETS = {"control_pure_binary", "verifier_binary", "fdbserver", "fdbcli", "corpus_100000", "corpus_375000", "layout_template", "descriptor_set"}
MAX_RAW_RECEIPT = 256 * 1024 ** 2


def digest(value):
    return hashlib.sha256(json.dumps(value, sort_keys=True, separators=(",", ":"), allow_nan=False).encode()).hexdigest()


def job_seal(job):
    # Matches driver.write's exclusive sorted JSON bytes, including its newline.
    return hashlib.sha256((json.dumps(job, sort_keys=True) + "\n").encode()).hexdigest()


def write_receipt(path, value, *, receipt_root=None):
    """Losslessly archive large receipts; never drop per-call or oracle populations."""
    path = Path(path)
    versions = (path, path.with_name(path.name + ".gz"), path.with_name(path.name + ".seal.json"))
    p.require(all(not candidate.exists() and not candidate.is_symlink() for candidate in versions), "receipt already exists")
    raw = (json.dumps(value, sort_keys=True, allow_nan=False) + "\n").encode()
    p.require(len(raw) <= MAX_RAW_RECEIPT, "bounded paired raw receipt")
    packed = gzip.compress(raw, mtime=0) if len(raw) > 65536 else None
    needed = len(raw) if packed is None else len(packed) + 1024
    if receipt_root is not None:
        used = sum(x.stat().st_size for x in Path(receipt_root).rglob("*") if x.is_file())
        p.require(used + needed < 1024 ** 3, "shared 1GiB receipt allocation exhausted")
    if packed is None:
        d.write(path, value)
        return
    target = path.with_name(path.name + ".gz")
    seal = {"schema": "otel_paired_receipt_archive_v1", "encoding": "gzip", "file": target.name,
            "raw_sha256": hashlib.sha256(raw).hexdigest(), "raw_bytes": len(raw),
            "sha256": hashlib.sha256(packed).hexdigest(), "bytes": len(packed)}
    p.require(not path.exists() and not path.is_symlink(), "receipt already exists")
    target.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    with open(target, "xb") as stream:
        os.chmod(target, 0o600)
        stream.write(packed); stream.flush(); os.fsync(stream.fileno())
    d.write(path.with_name(path.name + ".seal.json"), seal)


def workload(value):
    p.require(type(value) is dict, "typed shared workload required")
    if value.get("kind") == "writer_direct":
        p.require(set(value) == {"kind", "producers", "producer_batch_spans"}, "writer_direct requires explicit producer concurrency")
        p.require(type(value["producers"]) is int and 1 <= value["producers"] <= 32, "bounded producer concurrency")
    else:
        p.require(value.get("kind") == "collector_queue" and set(value) == QUEUE_FIELDS,
                  "complete collector_queue parameters required; no defaults")
        for key in QUEUE_FIELDS - {"kind", "block_on_overflow", "retry_enabled"}:
            p.require(p.integer(value[key], 0 if key in ("retry_initial_ms", "retry_elapsed_ms") else 1),
                      "typed queue/retry parameter: " + key)
        p.require(type(value["block_on_overflow"]) is bool and type(value["retry_enabled"]) is bool, "typed overflow/retry policy")
        p.require(not value["retry_enabled"] or value["retry_initial_ms"] > 0, "enabled retry requires positive initial interval")
        p.require(value["producer_batch_spans"] <= value["queue_capacity_spans"], "producer request exceeds queue capacity")
        p.require(value["producers"] <= 32 and value["consumers"] <= 32 and value["queue_capacity_spans"] <= 10000000
                  and value["batch_min_spans"] <= value["batch_max_spans"] <= 10000000
                  and value["batch_min_spans"] <= value["queue_capacity_spans"]
                  and value["retry_initial_ms"] <= value["retry_max_ms"], "bounded matched batching/retry")
    p.require(type(value.get("producer_batch_spans")) is int and 1 <= value["producer_batch_spans"] <= 64,
              "explicit source request grouping required")
    return value


def asset(value, *, check_assets=True):
    p.require(type(value) is dict and set(value) == {"path", "sha256"}
              and type(value["path"]) is str and Path(value["path"]).is_absolute()
              and ".." not in Path(value["path"]).parts and p.hexstr(value["sha256"]), "sealed absolute asset")
    if check_assets:
        path = Path(value["path"])
        p.require(path.is_file() and not path.is_symlink() and path.stat().st_size > 0 and p.sha(path) == value["sha256"], "changed paired asset")
    return value


def native_geometry(value):
    p.require(type(value) is dict and set(value) == {"storage", "log", "control", "fdb_slice", "client_slice", "gomaxprocs"},
              "complete shared native geometry required")
    storage = value["storage"]
    p.require(type(storage) is dict and set(storage) == {"processes_per_host", "memory_mib", "cache_mib", "max_outstanding"},
              "exact native storage geometry")
    p.require(type(storage["processes_per_host"]) is int and storage["processes_per_host"] == 3
              and type(storage["max_outstanding"]) is int and storage["max_outstanding"] == 64, "three native storage processes/MAX64")
    for name in ("storage", "log", "control"):
        spec = value[name]
        if name != "storage":
            p.require(type(spec) is dict and set(spec) == {"memory_mib", "cache_mib"}, "exact native process memory geometry")
        p.require(p.integer(spec.get("memory_mib"), 1) and spec["memory_mib"] <= 56 * 1024
                  and p.integer(spec.get("cache_mib"), 1) and spec["cache_mib"] < spec["memory_mib"], "bounded native process memory/cache")
    for name in ("fdb_slice", "client_slice"):
        spec = value[name]
        p.require(type(spec) is dict and set(spec) == {"allowed_cpus", "cpu_quota_percent", "memory_max_mib", "swap_max_bytes"},
                  "exact native role slice")
        p.require(spec.get("allowed_cpus") == "0-3" and type(spec.get("cpu_quota_percent")) is int and spec["cpu_quota_percent"] == 400
                  and type(spec.get("swap_max_bytes")) is int and spec["swap_max_bytes"] == 0
                  and p.integer(spec.get("memory_max_mib"), 1024) and spec["memory_max_mib"] <= 56 * 1024,
                  "fixed CPU4/swap0 bounded role memory")
    configured = storage["processes_per_host"] * storage["memory_mib"] + value["log"]["memory_mib"] + 4 * value["control"]["memory_mib"]
    p.require(configured < value["fdb_slice"]["memory_max_mib"], "configured FDB process memory must leave slice headroom")
    p.require(type(value["gomaxprocs"]) is int and value["gomaxprocs"] == 4, "same client CPU4 and Go GOMAXPROCS4")
    return value


def stages(plan):
    profile = plan.get("stage_profile")
    p.require(profile in ("pilot", "scaling"), "explicit paired stage profile")
    expected = [1] if profile == "pilot" else [1, 2, 3]
    actual = plan.get("stages")
    p.require(type(actual) is list and all(type(x) is int for x in actual) and actual == expected,
              "exact typed paired stages")
    return expected


def authorized_rows(plan):
    arms = plan.get("arms")
    p.require(type(arms) is list and 2 <= len(arms) <= 3 and all(type(x) is str and x in ARMS for x in arms)
              and len(set(arms)) == len(arms), "sealed distinct comparison arm set")
    rows = len(arms) * sum(stages(plan)) * 475000 * (2 if plan["stage_profile"] == "pilot" else 1)
    p.require(rows <= 15000000, "paired named-family allocation ceiling")
    p.require("prior_rows_upper" not in plan and plan.get("row_budget_name") == "otel_paired_go_rust_v1",
              "named persisted row budget required; caller prior is forbidden")
    p.require(type(plan.get("row_ledger")) is str and Path(plan["row_ledger"]).is_absolute(), "persisted row ledger path required")
    return rows


def held_roles(plan):
    hosts = max(stages(plan))
    return [kind + str(i) for kind in ("node", "client") for i in range(1, hosts + 1)]


def validate_plan(plan, *, check_assets=True):
    p.require(type(plan) is dict and plan.get("schema") == SCHEMA and plan.get("approved") is True,
              "explicit new paired schema approval required")
    authorized_rows(plan)
    for key in ("campaign", "prefix"):
        p.require(type(plan.get(key)) is str and re.fullmatch(r"[a-z][a-z0-9-]{7,47}", plan[key]), "invalid " + key)
    p.require(plan.get("location") == "hel1" and plan.get("server_type") == "ccx43"
              and plan.get("replication") == "single", "fixed native hardware and single replication")
    for key, expected in (("count", 100000), ("measured_count", 375000), ("measured_seconds", 120),
                          ("batch", 64), ("workers", 8), ("gomaxprocs", 4), ("storage_process_count", 3),
                          ("storage_max_outstanding", 64)):
        p.require(type(plan.get(key)) is int and plan[key] == expected, "fixed paired field: " + key)
    p.require(type(plan.get("writer_batch_size")) is int and 1 <= plan["writer_batch_size"] <= 64
              and type(plan.get("writer_workers")) is int and plan["writer_workers"] == 1,
              "matched internal writer concurrency must be exactly one; batch is bounded")
    p.require(plan.get("storage_memory_profile") == "scaling3ss3g", "fixed native memory geometry")
    p.require(p.storage(plan.get("storage_options")) == {"record_compression": "zlib-1", "bulk_index_conflicts": False},
              "matched native ordinary index conflict profile required")
    workload(plan.get("workload"))
    native_geometry(plan.get("native_geometry"))
    p.require(not set(plan) & {"topologies", "measured_topologies", "prerequisite_topologies", "selected_topologies",
                              "probe_count", "probe_seconds", "cases", "log_memory_gib", "foreground_only", "arm_geometry", "arm_workloads"},
              "paired study cannot inherit other schema overrides")
    orders = plan.get("arm_order")
    p.require(type(orders) is list and len(orders) == len(stages(plan)), "per-stage preregistered arm order required")
    for order in orders:
        repeats = 2 if plan["stage_profile"] == "pilot" else 1
        p.require(type(order) is list and len(order) == len(plan["arms"]) * repeats and all(type(x) is str for x in order)
                  and set(order) == set(plan["arms"]) and all(order.count(arm) == repeats for arm in plan["arms"]),
                  "every stage runs the preregistered repetition population")
        if plan["stage_profile"] == "pilot":
            p.require(order == plan["arms"] + list(reversed(plan["arms"])), "pilot balanced reversed second repetition required")
    p.require(plan.get("load_profile") == "finite_queued_v1",
              "only finite profile implemented; offered-load600 requires sealed scheduling/window contract")
    p.require(plan.get("oracle") == "source_1600_v1", "paid profiles require complete independent source oracle")
    first, end = plan.get("first_epoch"), plan.get("deadline_epoch")
    p.require(p.integer(first, 1) and p.integer(end, 1)
              and end - first == (3600 if plan["stage_profile"] == "pilot" else 7200), "one original fixed lease")
    p.require(plan.get("cleanup_mode") == "independent_watch", "paired campaigns require independent cleanup admission")
    assets = plan.get("assets")
    required_executables = {"control_pure_binary", "verifier_binary"} | {ARM_EXECUTABLES[x] for x in plan["arms"]}
    required = BASE_ASSETS | required_executables | {builds.deployment_asset(name) for name in required_executables}
    if any(x.endswith("native") for x in plan["arms"]):
        required.add("libfdb_c")
    p.require(type(assets) is dict and set(assets) == required, "complete exact paired asset set")
    for name, item in assets.items():
        if name in required_executables:
            p.require(type(item) is dict and set(item) == {"path", "sha256", "build_receipt"}, "executable requires sealed build receipt")
            asset(item["build_receipt"], check_assets=check_assets)
            asset({key: item[key] for key in ("path", "sha256")}, check_assets=check_assets)
        else:
            asset(item, check_assets=check_assets)
    executables = {"control_pure_binary", "verifier_binary"} | {ARM_EXECUTABLES[x] for x in plan["arms"]}
    p.require(len({assets[key]["sha256"] for key in executables}) == len(executables)
              and len({assets[key]["path"] for key in executables}) == len(executables), "distinct executable identity per arm, pure control and verifier required")
    if "libfdb_c" in assets:
        p.require(assets["libfdb_c"]["sha256"] == NATIVE_SHA256 or plan.get("fake_provider") is True,
                  "pinned FDB 7.3.77 native client required")
    sources = plan.get("sources")
    p.require(type(sources) is dict and set(sources) == set(plan["arms"]) | {"go_pure"}, "separate per-arm source seals")
    for arm in sources:
        src = sources[arm]
        p.require(type(src) is dict and set(src) == {"revision", "root", "manifest", "manifest_sha256"}
                  and p.hexstr(src["revision"], 40), "typed per-arm source identity")
        asset({"path": src["manifest"], "sha256": src["manifest_sha256"]}, check_assets=check_assets)
        p.require(type(src["root"]) is str and Path(src["root"]).is_absolute(), "per-arm source root")
    if "go_native" in plan["arms"]:
        p.require(all(sources["go_pure"][key] == sources["go_native"][key] for key in ("revision", "manifest_sha256")),
                  "Go pure/native executables must share the same collector source, not the same binary")
    if check_assets:
        for name in required_executables:
            builds.validate(plan, name, inspect_elf=not plan.get("fake_provider", False))
    p.require(p.hexstr(plan.get("pricing_seal_sha256")) and type(plan.get("pricing")) is dict,
              "sealed admission pricing required")
    p.require(type(plan.get("prior_cost_upper_eur")) is str and re.fullmatch(r"(?:0|[1-9][0-9]*)(?:\.[0-9]{1,18})?", plan["prior_cost_upper_eur"])
              and Decimal(plan["prior_cost_upper_eur"]) <= 10, "explicit cumulative prior paid-cost upper bound")
    p.require(type(plan.get("prior_cost_basis")) is str and plan["prior_cost_basis"].strip(), "prior paid cost evidence required")
    p.require(type(plan.get("stage_budget_eur")) is str and plan["stage_budget_eur"] == ("2" if plan["stage_profile"] == "pilot" else "8"),
              "sealed EUR2 pilot / EUR8 sweep allocations")
    p.require(type(plan.get("fake_provider")) is bool, "explicit provider mode required")
    import supervisor
    supervisor.validate_config(plan)
    if not plan["fake_provider"]:
        source = plan["sources"]["go_pure"]
        aliases = {"binary": plan["assets"]["control_pure_binary"]["path"], "binary_sha256": plan["assets"]["control_pure_binary"]["sha256"],
                   "source_root": source["root"], "source_manifest": source["manifest"],
                   "input_manifest_sha256": source["manifest_sha256"], "source_revision": source["revision"]}
        for name in ("fdbserver", "fdbcli"):
            aliases[name] = plan["assets"][name]["path"]
            aliases[name + "_sha256"] = plan["assets"][name]["sha256"]
        p.require(all(plan.get(key) == value for key, value in aliases.items()), "legacy transport aliases must match executing Go control seals")
        common = {key: value for key, value in plan.items() if key not in
                  ("storage_process_count", "storage_max_outstanding", "storage_memory_profile")}
        common.update(schema="otel_horizontal_v1", topologies=[1, 2, 3], measured_topologies=[3, 2, 1],
                      measured_count=100000, deadline_epoch=plan["first_epoch"] + 7200)
        p.validate_plan(common, check_assets=check_assets)
        proofs = plan.get("preflight_verifications")
        p.require(type(proofs) is dict and set(proofs) == set(plan["arms"]), "paid admission requires sealed Stage0 proof for every selected arm")
        for arm, proof in proofs.items():
            p.require(type(proof) is dict and set(proof) == {"ingest_job", "ingest_report", "verification"}, "complete preflight evidence set")
            for item in proof.values():
                asset(item, check_assets=check_assets)
            if check_assets:
                job = validate_job(d.read(proof["ingest_job"]["path"]))
                source = plan["sources"][arm]
                binary = ARM_EXECUTABLES[arm]
                p.require(job["arm"] == arm and job["mode"] == "ingest" and job["expected_frames"] == 100000
                          and job["binary_sha256"] == plan["assets"][binary]["sha256"]
                          and job["source_revision"] == source["revision"] and job["source_manifest_sha256"] == source["manifest_sha256"]
                          and job["corpus_sha256"] == plan["assets"]["corpus_100000"]["sha256"]
                          and job["client_library_sha256"] == (None if arm == "go_pure" else plan["assets"]["libfdb_c"]["sha256"])
                          and job["record_compression"] == plan["storage_options"]["record_compression"]
                          and job["writer_batch_size"] == plan["writer_batch_size"] and job["writer_workers"] == plan["writer_workers"]
                          and job["workload"] == plan["workload"],
                          "Stage0 proof must exercise current source/binary/corpus/client and matched pipeline parameters")
                p.require(reports.envelope(job, d.read(proof["ingest_report"]["path"]))["accepted"], "Stage0 ingest failed")
                reports.verify_audit(d.read(proof["verification"]["path"]), job, "local", control_identity=verifier_identity(plan))
    return plan


def verify_sources(plan):
    return {arm: p.verify_source({"source_root": src["root"], "source_manifest": src["manifest"],
                                 "input_manifest_sha256": src["manifest_sha256"]}) for arm, src in plan["sources"].items()}


def arm_id(hosts, arm, repeat=1):
    p.require(type(hosts) is int and hosts in (1, 2, 3) and arm in ARMS, "typed stage/arm identity")
    p.require(type(repeat) is int and repeat in (1, 2), "bounded preregistered repetition")
    return "pairs%d-%s-rep%d" % (hosts, arm, repeat)


def namespace(plan, hosts, arm, phase, client, repeat=1):
    p.require(hosts in stages(plan) and arm in plan["arms"] and phase in ("prerequisite", "measured")
              and type(client) is int and 1 <= client <= hosts, "sealed producer population")
    return "/".join((plan["campaign"], arm_id(hosts, arm, repeat), phase, "client" + str(client)))


def deployment(plan, name, remote_bin):
    receipt = builds.read_receipt(plan, name)
    root = remote_bin + "/" + name + ".bundle"
    return {"asset": name, "root": root, "executable_relative_path": receipt["executable_relative_path"],
            "client_library_relative_path": receipt["client_library_relative_path"]}


def deployed_assets(plan, remote_bin):
    values = {name: remote_bin + "/" + ("libfdb_c.so" if name == "libfdb_c" else name) for name in plan["assets"]}
    for name in builds.EXECUTABLES:
        if name in plan["assets"]:
            info = deployment(plan, name, remote_bin)
            values[name] = info["root"] + "/" + info["executable_relative_path"]
    return values


def _job(plan, arm, hosts, phase, client, layout, cluster, remote_assets, output, *, repeat=1):
    count = 100000 if phase == "prerequisite" else 375000
    ns = namespace(plan, hosts, arm, phase, client, repeat)
    source = plan["sources"][arm]
    binary = ARM_EXECUTABLES[arm]
    library = None if arm == "go_pure" else remote_assets["libfdb_c"]
    if arm == "go_native":
        receipt = builds.read_receipt(plan, binary)
        suffix = "/" + receipt["executable_relative_path"]
        p.require(remote_assets[binary].endswith(suffix), "preserved deployment layout required")
        library = remote_assets[binary][:-len(suffix)] + "/" + receipt["client_library_relative_path"]
    p.require(type(output) is str and Path(output).is_absolute() and "/" + ns + "/" in output,
              "immutable implementation-specific output path")
    for key in ("path", "sha256"):
        p.require(key in layout and key in cluster, "sealed dynamic cluster/layout required")
    return {"schema": "otel_paired_job_v1", "arm": arm,
            "client_backend": "pure_go" if arm == "go_pure" else "native_c",
            "client_library": library,
            "client_library_sha256": None if arm == "go_pure" else plan["assets"]["libfdb_c"]["sha256"],
            "cluster_file": cluster["path"], "cluster_sha256": cluster["sha256"],
            "layout_file": layout["path"], "layout_sha256": layout["sha256"],
            "corpus_file": remote_assets["corpus_" + str(count)], "corpus_sha256": plan["assets"]["corpus_" + str(count)]["sha256"],
            "binary_sha256": plan["assets"][binary]["sha256"], "source_revision": source["revision"],
            "source_manifest_sha256": source["manifest_sha256"], "expected_frames": count, "row_budget": count,
            "writer_batch_size": plan["writer_batch_size"], "writer_workers": plan["writer_workers"], "record_compression": "zlib-1",
            "mode": "ingest", "enable_message_deltas": False, "enable_message_full_text": False, "bulk_index_conflicts": False,
            "run": hashlib.sha256(ns.encode()).hexdigest()[:24],
            "corpus_config": {"generator": "otel_fixture_v2", "seed": 283, "count": count,
                              "tenants": 1, "attributes": 8, "match_every": 100},
            "timeout_seconds": 300 if phase == "prerequisite" else 120, "output": output,
            "workload": copy.deepcopy(plan["workload"])}


def go_job(plan, arm, *args, **kwargs):
    p.require(arm in ("go_pure", "go_native"), "Go builder requires a Go arm")
    return _job(plan, arm, *args, **kwargs)


def rust_job(plan, arm, *args, **kwargs):
    p.require(arm == "rust_native", "Rust builder requires the Rust arm")
    return _job(plan, arm, *args, **kwargs)


def validate_job(job):
    p.require(type(job) is dict and job.get("schema") == "otel_paired_job_v1"
              and job.get("mode") in ("corpus", "setup", "ingest", "verify") and job.get("arm") in ARMS,
              "versioned typed producer job required")
    arm, mode = job["arm"], job["mode"]
    common = {"schema", "mode", "arm", "binary_sha256", "source_revision", "source_manifest_sha256", "timeout_seconds", "output",
              "corpus_config", "expected_frames", "row_budget", "corpus_file"}
    live = {"run", "cluster_file", "cluster_sha256", "client_backend", "client_library", "client_library_sha256", "corpus_sha256"}
    pipeline = {"workload", "writer_batch_size", "writer_workers", "record_compression", "enable_message_deltas", "enable_message_full_text", "bulk_index_conflicts"}
    expected = common | (live | pipeline | {"layout_file"} if mode != "corpus" else set())
    if mode in ("ingest", "verify"):
        expected.add("layout_sha256")
    if mode == "verify":
        expected.add("verification")
    p.require(set(job) == expected, "exact mode-specific job fields; no defaults or unknown fields")
    for key, length in (("binary_sha256", 64), ("source_revision", 40), ("source_manifest_sha256", 64)):
        p.require(p.hexstr(job.get(key), length), "producer source/binary seals required")
    p.require(type(job.get("timeout_seconds")) is int and 1 <= job["timeout_seconds"] <= 3600, "bounded producer deadline")
    paths = [job.get("output")]
    if mode != "corpus":
        p.require(type(job.get("run")) is str and re.fullmatch(r"[a-z0-9_]{8,48}", job["run"]), "fresh production run identity")
        for name in ("cluster", "corpus") + (("layout",) if mode in ("ingest", "verify") else ()):
            paths.append(job.get(name + "_file"))
            p.require(p.hexstr(job.get(name + "_sha256")), "sealed producer input: " + name)
        if arm == "go_pure":
            p.require(job.get("client_backend") == "pure_go" and job.get("client_library") is None
                      and job.get("client_library_sha256") is None, "pure Go must not load native C")
        else:
            paths.append(job.get("client_library"))
            p.require(job.get("client_backend") == "native_c" and p.hexstr(job.get("client_library_sha256")), "native client seal required")
    if mode == "corpus":
        paths.append(job["corpus_file"])
    if mode == "setup":
        paths.append(job["layout_file"])
    for path in paths:
        p.require(type(path) is str and Path(path).is_absolute() and str(Path(path)) == path and ".." not in Path(path).parts
                  and "\x00" not in path, "clean absolute producer paths")
    p.require(len(paths) == len(set(paths)), "producer input/output paths must be distinct")
    p.require(type(job.get("expected_frames")) is int and 1 <= job["expected_frames"] <= 1000000
              and type(job.get("row_budget")) is int and job["expected_frames"] <= job["row_budget"] <= 10000000,
              "bounded complete corpus population")
    config = job.get("corpus_config")
    p.require(type(config) is dict and set(config) == {"generator", "seed", "count", "tenants", "attributes", "match_every"}
              and config.get("generator") == "otel_fixture_v2" and type(config.get("count")) is int
              and 2 <= config["count"] == job["expected_frames"] <= 1000000 and type(config.get("seed")) is int and 0 <= config["seed"] <= 2 ** 64 - 1
              and type(config.get("tenants")) is int and config["tenants"] == 1
              and type(config.get("attributes")) is int and config["attributes"] == 8
              and p.integer(config.get("match_every"), 1), "complete source-oracle corpus configuration")
    if mode == "verify":
        check = job["verification"]
        p.require(type(check) is dict and set(check) == {"query_oracle", "query_results_file", "k", "max_candidates", "max_bytes"}
                  and check["query_oracle"] in ("none", "source_1600_v1") and all(p.integer(check[key], 1) for key in ("k", "max_candidates", "max_bytes")),
                  "complete independent verifier parameters")
        p.require((check["query_oracle"] == "none" and check["query_results_file"] is None)
                  or (check["query_oracle"] == "source_1600_v1" and type(check["query_results_file"]) is str
                      and Path(check["query_results_file"]).is_absolute() and ".." not in Path(check["query_results_file"]).parts
                      and check["query_results_file"] not in paths), "exclusive source-oracle output required")
    if mode in ("setup", "ingest", "verify"):
        workload(job.get("workload"))
        p.require(type(job.get("writer_batch_size")) is int and 1 <= job["writer_batch_size"] <= 64
                  and type(job.get("writer_workers")) is int and job["writer_workers"] == 1
                  and job.get("record_compression") in ("none", "zlib-1", "zlib-default")
                  and job.get("enable_message_deltas") is False and job.get("enable_message_full_text") is False
                  and job.get("bulk_index_conflicts") is False, "matched span-only writer configuration")
    return job


def invocation(plan, job, job_path, remote_assets):
    validate_job(job)
    arm = job["arm"]
    p.require((arm in plan["arms"] or job["mode"] == "corpus" and arm == "go_pure")
              and (job["mode"] == "corpus" or job["workload"] == plan["workload"]) and job["schema"] == "otel_paired_job_v1",
              "implementation/workload job binding")
    p.require(type(job_path) is str and Path(job_path).is_absolute() and ".." not in Path(job_path).parts,
              "absolute producer job path required")
    binary = executable_for_mode(arm, job["mode"])
    identity = {"binary_sha256": plan["assets"][binary]["sha256"],
                "source_revision": plan["sources"][arm if job["mode"] == "ingest" else "go_pure"]["revision"],
                "source_manifest_sha256": plan["sources"][arm if job["mode"] == "ingest" else "go_pure"]["manifest_sha256"]}
    p.require(all(job[key] == value for key, value in identity.items()), "wrong executable/source identity for arm or control mode")
    return [remote_assets[binary], job_path]


def envelope(job, report, *, paid_host=False):
    return reports.envelope(job, report, paid_host=paid_host)


def executable_for_mode(arm, mode):
    return ARM_EXECUTABLES[arm] if mode == "ingest" else "verifier_binary" if mode == "verify" else "control_pure_binary"


def verifier_identity(plan):
    source = plan["sources"]["go_pure"]
    return {"binary_sha256": plan["assets"]["verifier_binary"]["sha256"], "source_revision": source["revision"],
            "source_manifest_sha256": source["manifest_sha256"]}


def control_identity(plan):
    source = plan["sources"]["go_pure"]
    return {"binary_sha256": plan["assets"]["control_pure_binary"]["sha256"], "source_revision": source["revision"],
            "source_manifest_sha256": source["manifest_sha256"]}


def verify_audit(value, job, ns, *, plan=None):
    return reports.verify_audit(value, job, ns, control_identity=verifier_identity(plan) if plan is not None else None)


def joined_envelope(job, report, verification, ns, plan):
    value = reports.joined_envelope(job, report, verification, ns, control_identity=verifier_identity(plan), paid_host=not plan["fake_provider"])
    value["fake_provider"] = plan["fake_provider"]
    return value


def wave_metrics(values, started, ack_end, drained_end):
    return reports.wave_metrics(values, started, ack_end, drained_end)


class PairedCampaign:
    """Backend contract: no methods may renew leases or retry a released wave.

    Every backend is responsible for pinned-host transport, exact identity, strict
    health and bounded monitoring inside each blocking method. cleanup is never
    cost/admission gated. The fake backend supplies no live acceptance evidence.
    """
    def __init__(self, plan, output, backend):
        self.plan, self.output, self.backend = plan, Path(output), backend
        self.ledger, self.resources = None, None
        self.released, self.charged_rows = set(), 0
        self.sequence = 0
        self.active_issue = None

    def write(self, name, value):
        if self.backend.fake:
            d.write(self.output / name, value)
        else:
            write_receipt(self.output / name, value, receipt_root=self.output)

    def guard(self, *, active=True):
        now = self.backend.now()
        if active:
            p.require(now < self.plan["deadline_epoch"] - (900 if self.plan["stage_profile"] == "pilot" else 1200),
                      "original active deadline exhausted; cleanup required")
        if self.ledger is not None:
            self.ledger = cost.checkpoint(self.ledger, now_epoch=now, owned_ids=self.backend.owned_ids(),
                                          deadline_epoch=self.plan["deadline_epoch"])
            report = cost.assess(self.ledger, now_epoch=now, owned_ids=self.backend.owned_ids())
            total = Decimal(report["total_upper_eur"])
            p.require(total <= Decimal(self.plan["stage_budget_eur"]) and total + Decimal(self.plan["prior_cost_upper_eur"]) <= 10,
                      "stage allocation or shared EUR10 ceiling exceeded")
            if self.resources is not None and hasattr(self.backend, "update_cost_ledger"):
                self.backend.update_cost_ledger(self.ledger)
            self.sequence += 1
            self.write("cost/%04d.json" % self.sequence, {"ledger": self.ledger, "assessment": report})
        p.require(sum(x.stat().st_size for x in self.output.rglob("*") if x.is_file()) < 1024 ** 3,
                  "shared 1GiB receipt ceiling")

    def call(self, method, *args):
        self.guard()
        result = getattr(self.backend, method)(*args)
        self.guard()
        return result

    def run(self):
        validate_plan(self.plan)
        p.require(self.plan["fake_provider"] is self.backend.fake, "provider mode cannot be substituted")
        before = verify_sources(self.plan)
        if self.output.exists():
            raise FileExistsError("paired output already exists; no replay or reset")
        population = rows.inspect(self.plan["row_ledger"])
        p.require(population["remaining_rows"] >= authorized_rows(self.plan), "named family row budget insufficient")
        self.output.mkdir(mode=0o700, parents=True, exist_ok=False)
        self.write("approved-plan.json", self.plan)
        self.write("source-before.json", before)
        result = {"schema": "otel_paired_campaign_result_v1", "status": "FAILED", "fake_provider": self.backend.fake,
                  "accepted_comparison": False, "arms": [], "authorized_rows": authorized_rows(self.plan),
                  "row_budget_name": rows.BUDGET_NAME, "previous_issued_rows": population["issued_rows"], "first_epoch": self.plan["first_epoch"],
                  "deadline_epoch": self.plan["deadline_epoch"], "held_host_count": len(held_roles(self.plan))}
        failure = None
        try:
            self.guard()
            quote = self.backend.capture_pricing(self.plan)
            p.require(digest(quote) == digest(self.plan["pricing"]), "fresh price capture differs from approved quote")
            self.ledger = cost.admit(quote, first_epoch=self.plan["first_epoch"], deadline_epoch=self.plan["deadline_epoch"],
                                     now_epoch=self.backend.now(), expected_seal=self.plan["pricing_seal_sha256"],
                                     held_host_count=len(held_roles(self.plan)))
            self.guard()
            # prepare must internally enforce the ledger on every provider mutation;
            # reserving all held slots first also accounts for ambiguous creates.
            self.resources = self.backend.prepare(self.plan, self.ledger)
            p.require(type(self.resources) is dict and set(self.resources) == set(held_roles(self.plan)), "exact newly held host set")
            for node in self.resources.values():
                self.ledger = cost.record_creation(self.ledger, host_id=node["id"], created_epoch=node["created_epoch"],
                                                   now_epoch=self.backend.now())
            self.guard()
            transfers = self.call("transfer_assets", self.plan, self.resources)
            self.write("asset-transfer.json", transfers)
            validate_transfers(self.plan, self.resources, transfers)
            for hosts, order in zip(stages(self.plan), self.plan["arm_order"]):
                occurrences = {arm: 0 for arm in self.plan["arms"]}
                for arm in order:
                    occurrences[arm] += 1
                    repeat = occurrences[arm]
                    identity = arm_id(hosts, arm, repeat)
                    directory = "arms/" + identity
                    self.call("quiet", self.plan, self.resources)
                    cluster = self.call("fresh_cluster", self.plan, self.resources, hosts, identity)
                    asset(cluster, check_assets=False)
                    self.write(directory + "/cluster.json", cluster)
                    record = {"hosts": hosts, "arm": arm, "status": "FAILED", "waves": []}
                    result["arms"].append(record)
                    for phase in ("prerequisite", "measured"):
                        count = 100000 if phase == "prerequisite" else 375000
                        wave = directory + "/" + phase
                        jobs, ready = {}, {}
                        for client in range(1, hosts + 1):
                            ns = namespace(self.plan, hosts, arm, phase, client, repeat)
                            layout = self.call("setup", self.plan, cluster, ns, count)
                            asset(layout, check_assets=False)
                            remote_assets = self.backend.remote_assets(self.plan)
                            builder = go_job if arm.startswith("go_") else rust_job
                            job = builder(self.plan, arm, hosts, phase, client, layout, cluster, remote_assets,
                                          "/srv/otel-paired/" + ns + "/ingest.json", repeat=repeat)
                            path = "/srv/otel-paired/" + ns + "/job.json"
                            argv = invocation(self.plan, job, path, remote_assets)
                            self.write(wave + "/client%d-job.json" % client, {"job": job, "argv": argv, "namespace": ns})
                            jobs[client] = job
                            receipt = self.call("ready", self.plan, self.resources["client" + str(client)], ns, job, argv)
                            p.require(receipt == {"namespace": ns, "job_sha256": job_seal(job), "node_id": self.resources["client" + str(client)]["id"]},
                                      "all producers must bind ready to exact host/job before release")
                            ready[client] = receipt
                        key = (hosts, arm, repeat, phase)
                        p.require(key not in self.released, "duplicate release refused")
                        self.charged_rows += hosts * count
                        p.require(self.charged_rows <= authorized_rows(self.plan), "no row reclamation or extra release")
                        self.active_issue = rows.issue(self.plan["row_ledger"], campaign=self.plan["campaign"],
                            stage="pilot" if self.plan["stage_profile"] == "pilot" else "pairs" + str(hosts),
                            arm=arm, repeat=repeat, phase=phase, rows=hosts * count)
                        self.released.add(key)
                        self.write(wave + "/release.json", {"ready": ready, "charged_rows": self.charged_rows,
                                                           "lease_deadline_epoch": self.plan["deadline_epoch"]})
                        started = self.backend.monotonic_ns()
                        reports = self.call("release_and_wait", self.plan, ready, jobs)
                        ack_end = self.backend.monotonic_ns()
                        p.require(type(reports) is dict and set(reports) == set(jobs), "every released producer report required")
                        for client, job in jobs.items():
                            value = envelope(job, reports[client], paid_host=not self.backend.fake)
                            self.write(wave + "/client%d-ingest-envelope.json" % client, value)
                            if not value["accepted"]:
                                self.write(wave + "/client%d-result.json" % client, value)
                            p.require(value["accepted"], value.get("error", "producer refused"))
                        samples = self.call("drain", self.plan, cluster)
                        drained_end = self.backend.monotonic_ns()
                        p.require(type(samples) is list and len(samples) >= 3 and all(type(x) is dict and x.get("healthy") is True
                                  and p.integer(x.get("queue_bytes")) and x["queue_bytes"] < 16777216
                                  and p.number(x.get("lag_seconds")) and x["lag_seconds"] < 10 for x in samples[-3:]),
                                  "three strict healthy stable drain observations")
                        verified = []
                        for client, job in jobs.items():
                            ns = namespace(self.plan, hosts, arm, phase, client, repeat)
                            verification = self.call("verify", self.plan, ns, job)
                            self.write(wave + "/client%d-verification.json" % client, verification)
                            value = joined_envelope(job, reports[client], verification, ns, self.plan)
                            self.write(wave + "/client%d-result.json" % client, value)
                            verified.append(value)
                        metrics = {**wave_metrics(verified, started, ack_end, drained_end), "phase": phase, "drain_samples": samples,
                                   "fake_provider": self.backend.fake}
                        record["waves"].append(metrics)
                        self.write(wave + "/wave-result.json", metrics)
                        rows.outcome(self.plan["row_ledger"], issue_id=self.active_issue["issue_id"], status="passed")
                        self.active_issue = None
                    stopped = self.call("quiesce", self.plan, cluster)
                    self.write(directory + "/quiescence.json", stopped)
                    p.require(type(stopped) is dict and stopped.get("quiescent") is True and stopped.get("passed") is True,
                              "exact old-arm units/scopes/PIDs must be quiescent")
                    record["status"] = "PASS"
            result["status"] = "FAKE_PASSED" if self.backend.fake else "PASSED"
        except BaseException as exc:
            failure = exc
            result["error"] = str(exc)
            if self.active_issue is not None:
                rows.outcome(self.plan["row_ledger"], issue_id=self.active_issue["issue_id"], status="failed")
                self.active_issue = None
        finally:
            try:
                cleanup = self.backend.cleanup(self.plan)
                self.write("cleanup.json", cleanup)
                p.require(type(cleanup) is dict and cleanup.get("passed") is True
                          and cleanup.get("exact_baseline") is True, "verified exact-baseline deletion required")
                if self.ledger is not None:
                    incoming = cleanup.get("cost_ledger")
                    if incoming is not None:
                        scope = ("schema", "first_epoch", "deadline_epoch", "budget_eur", "held_hosts", "pricing", "pricing_seal")
                        p.require(type(incoming) is dict and all(incoming.get(k) == self.ledger[k] for k in scope)
                                  and incoming.get("last_epoch", -1) >= self.ledger["last_epoch"], "cleanup cannot reset original cost scope")
                        existing = {host["host_id"]: host for host in incoming.get("hosts", [])}
                        p.require(all(host["host_id"] in existing and existing[host["host_id"]]["created_epoch"] == host["created_epoch"]
                                      and (host["deletion"] is None or existing[host["host_id"]]["deletion"] == host["deletion"])
                                      for host in self.ledger["hosts"]), "cleanup cannot erase cost ownership history")
                        self.ledger = incoming
                    for proof in cleanup.get("deletions", []):
                        self.ledger = cost.record_deletion(self.ledger, verification=proof, now_epoch=self.backend.now())
                    self.guard(active=False)
                result["cleanup_passed"] = True
            except BaseException as exc:
                failure = failure or exc
                result.update(status="FAILED", cleanup_passed=False, cleanup_error=str(exc))
            try:
                self.write("source-after.json", verify_sources(self.plan))
            except BaseException as exc:
                failure = failure or exc
                result.update(status="FAILED", source_error=str(exc))
            result.update(charged_rows=self.charged_rows, released_waves=len(self.released),
                          accepted_comparison=result["status"] == "PASSED" and result.get("cleanup_passed") is True)
            self.write("paired-result.json", result)
        if failure:
            raise failure
        return result


def validate_transfers(plan, resources, receipts):
    p.require(type(receipts) is dict and set(receipts) == set(resources), "per-held-host asset receipts required")
    for role, node in resources.items():
        receipt = receipts[role]
        p.require(type(receipt) is dict and receipt.get("node_id") == node["id"] and type(receipt.get("assets")) is dict
                  and set(receipt["assets"]) == set(plan["assets"]), "exact asset receipt host/population binding")
        expected = {name for name in builds.EXECUTABLES if name in plan["assets"]}
        p.require(type(receipt.get("deployments")) is dict and set(receipt["deployments"]) == expected, "per-host executable deployments required")
        for name in expected:
            built = builds.read_receipt(plan, name)
            deployed = receipt["deployments"][name]
            p.require(type(deployed) is dict and deployed.get("schema") == "otel_paired_deployment_receipt_v1"
                      and deployed.get("sha256") == built["deployment_sha256"] and deployed.get("files") == built["files"],
                      "complete executable/runfiles deployment proof required")
        for name, sealed in plan["assets"].items():
            entry = receipt["assets"][name]
            p.require(type(entry) is dict and entry.get("sha256") == sealed["sha256"]
                      and p.hexstr(entry.get("transfer_sha256")) and p.integer(entry.get("bytes"), 1), "sealed asset transfer mismatch")


class FakeBackend:
    """In-memory provider/producer stand-in; reports are synthetic, never benchmarks."""
    fake = True

    def __init__(self, first_epoch):
        self.epoch, self.ns = first_epoch, 1000000000
        self.nodes, self.events = {}, []
        self.active, self.ready_jobs = None, {}

    def now(self):
        return self.epoch

    def monotonic_ns(self):
        self.ns += 1000000000
        return self.ns

    def owned_ids(self):
        return sorted(node["id"] for node in self.nodes.values())

    def capture_pricing(self, plan):
        return copy.deepcopy(plan["pricing"])

    def prepare(self, plan, ledger):
        self.events.append("prepare")
        self.nodes = {role: {"id": 100 + i, "created_epoch": self.epoch} for i, role in enumerate(held_roles(plan))}
        return copy.deepcopy(self.nodes)

    def remote_assets(self, plan):
        return deployed_assets(plan, "/srv/otel-paired/" + plan["campaign"] + "/assets")

    def transfer_assets(self, plan, resources):
        return {role: {"node_id": node["id"], "assets": {name: {"sha256": value["sha256"],
                        "transfer_sha256": digest({"synthetic": value["sha256"]}), "bytes": Path(value["path"]).stat().st_size}
                        for name, value in plan["assets"].items()}, "deployments": {
                    name: {"schema": "otel_paired_deployment_receipt_v1", "sha256": builds.read_receipt(plan, name)["deployment_sha256"],
                           "files": builds.read_receipt(plan, name)["files"]} for name in builds.EXECUTABLES if name in plan["assets"]}}
                for role, node in resources.items()}

    def quiet(self, plan, resources):
        self.events.append("quiet")

    def fresh_cluster(self, plan, resources, hosts, identity):
        p.require(self.active is None, "old arm still active")
        self.active = identity
        self.events.append(identity)
        return {"path": "/srv/otel-paired/" + plan["campaign"] + "/" + identity + "/fdb.cluster",
                "sha256": digest(identity)}

    def setup(self, plan, cluster, ns, count):
        return {"path": "/srv/otel-paired/" + ns + "/layout.json", "sha256": digest(ns)}

    def ready(self, plan, node, ns, job, argv):
        p.require(ns not in self.ready_jobs, "duplicate ready namespace")
        self.ready_jobs[ns] = job_seal(job)
        return {"namespace": ns, "job_sha256": job_seal(job), "node_id": node["id"]}

    def release_and_wait(self, plan, ready, jobs):
        self.events.append("release")
        self.epoch += 1
        return {client: reports.fake_ingest(job) for client, job in jobs.items()}

    def drain(self, plan, cluster):
        self.epoch += 10
        return [{"healthy": True, "queue_bytes": 0, "lag_seconds": 0} for _ in range(3)]

    def verify(self, plan, ns, job):
        return reports.fake_verify(job, control_identity=verifier_identity(plan))

    def quiesce(self, plan, cluster):
        self.active = None
        return {"passed": True, "quiescent": True}

    def cleanup(self, plan):
        self.events.append("cleanup")
        self.epoch += 1
        proofs = []
        for node in self.nodes.values():
            proof = {"host_id": node["id"], "observed_epoch": self.epoch,
                     "source": "https://api.hetzner.cloud/v1/servers/" + str(node["id"]), "status": "not_found"}
            proof["seal_sha256"] = cost.seal_receipt(proof)
            proofs.append(proof)
        self.nodes = {}
        return {"passed": True, "exact_baseline": True, "deletions": proofs}


def fake_plan(root, *, profile="scaling", arms=ARMS, kind="writer_direct"):
    """Create real local hash fixtures only, never executable binaries or cloud inputs."""
    root = Path(root)
    root.mkdir(mode=0o700, parents=True, exist_ok=False)
    source = root / "source.txt"
    source.write_text("synthetic source, not a benchmark\n")
    manifest = root / "source.manifest"
    manifest.write_text(p.sha(source) + "  source.txt\n")
    assets = {}
    keys = BASE_ASSETS | {ARM_EXECUTABLES[x] for x in arms} | {"libfdb_c"}
    for key in keys:
        path = root / key
        path.write_text("synthetic asset " + key + "\n")
        path.chmod(0o600)
        assets[key] = {"path": str(path), "sha256": p.sha(path)}
    source_identity = {"revision": "a" * 40, "root": str(root), "manifest": str(manifest), "manifest_sha256": p.sha(manifest)}
    for name in builds.EXECUTABLES:
        if name in assets:
            assets.update(builds.fake_artifacts(name, assets[name], source_identity, assets["libfdb_c"]))
    ledger = root / "rows.jsonl"
    rows.init(str(ledger))
    first = int(time.time())
    quote = {"schema": "paired_pricing_v1", "provider": "hetzner", "source": "https://api.hetzner.cloud/v1/pricing",
             "captured_epoch": first, "currency": "EUR", "server_type": "ccx43", "location": "hel1",
             "server_hourly_net_eur": "0.3758" if profile == "pilot" else "0.35",
             "server_hourly_gross_eur": "0.447202" if profile == "pilot" else "0.4165",
             "ipv4_hourly_net_eur": "0.0008", "ipv4_hourly_gross_eur": "0.000952",
             "traffic_upper_eur": "0",
             "other_upper_eur": "0.10", "bounds_conservative": True,
             "provenance": {"response_sha256": digest("synthetic-not-a-quote"), "pricing_path": "pricing.server_types[ccx43].prices[hel1]", "vat_rate": "0.19"}}
    quote["seal_sha256"] = cost.seal_receipt(quote)
    order = list(arms)
    queue = {"kind": "collector_queue", "producers": 8, "producer_batch_spans": 64, "consumers": 8, "queue_capacity_spans": 10000,
             "batch_min_spans": 64, "batch_max_spans": 256, "flush_ms": 200, "block_on_overflow": True,
             "export_timeout_ms": 120000, "retry_enabled": True, "retry_initial_ms": 500,
             "retry_max_ms": 10000, "retry_elapsed_ms": 300000}
    return {"schema": SCHEMA, "approved": True, "campaign": "paired-fake-test", "prefix": "paired-fake-test",
            "location": "hel1", "server_type": "ccx43", "replication": "single", "stage_profile": profile,
            "stages": [1] if profile == "pilot" else [1, 2, 3], "arms": order,
            "arm_order": [order + list(reversed(order))] if profile == "pilot" else [order, list(reversed(order)), order],
            "load_profile": "finite_queued_v1", "oracle": "source_1600_v1",
            "row_budget_name": rows.BUDGET_NAME, "row_ledger": str(ledger),
            "prior_cost_upper_eur": "0", "prior_cost_basis": "synthetic fixture, not an authorization",
            "stage_budget_eur": "2" if profile == "pilot" else "8",
            "count": 100000, "measured_count": 375000, "measured_seconds": 120, "batch": 64, "workers": 8,
            "gomaxprocs": 4, "storage_process_count": 3, "storage_max_outstanding": 64,
            "storage_memory_profile": "scaling3ss3g", "storage_options": {"record_compression": "zlib-1", "bulk_index_conflicts": False},
            "first_epoch": first, "deadline_epoch": first + (3600 if profile == "pilot" else 7200),
            "cleanup_mode": "independent_watch", "workload": {"kind": "writer_direct", "producers": 8, "producer_batch_spans": 64} if kind == "writer_direct" else queue,
            "writer_batch_size": 64, "writer_workers": 1,
            "native_geometry": {"storage": {"processes_per_host": 3, "memory_mib": 3072, "cache_mib": 1024, "max_outstanding": 64},
                "log": {"memory_mib": 2048, "cache_mib": 64}, "control": {"memory_mib": 1024, "cache_mib": 64},
                "fdb_slice": {"allowed_cpus": "0-3", "cpu_quota_percent": 400, "memory_max_mib": 16384, "swap_max_bytes": 0},
                "client_slice": {"allowed_cpus": "0-3", "cpu_quota_percent": 400, "memory_max_mib": 16384, "swap_max_bytes": 0}, "gomaxprocs": 4},
            "managed_watch": {"root": str(root.parent / "fake-watch"), "output": str(root.parent / "fake-output")},
            "assets": assets, "sources": {arm: {"revision": "a" * 40, "root": str(root), "manifest": str(manifest),
                                                "manifest_sha256": p.sha(manifest)} for arm in set(arms) | {"go_pure"}},
            "pricing": quote, "pricing_seal_sha256": quote["seal_sha256"], "fake_provider": True}


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--plan")
    parser.add_argument("--output")
    parser.add_argument("--dry-run", action="store_true")
    parser.add_argument("--profile", choices=("pilot", "scaling"), default="scaling")
    parser.add_argument("--workload", choices=("writer_direct", "collector_queue"), default="writer_direct")
    parser.add_argument("--execute-approved", action="store_true")
    args = parser.parse_args(argv)
    p.require(not (args.dry_run and args.execute_approved), "dry-run cannot authorize paid execution")
    if args.dry_run:
        p.require(args.output and Path(args.output).is_absolute(), "new absolute dry-run output required")
        plan = d.read(args.plan) if args.plan else fake_plan(Path(args.output + "-fixtures"), profile=args.profile, kind=args.workload)
        p.require(plan.get("fake_provider") is True, "dry-run refuses live plans")
        if args.plan:
            # A fake rehearsal must never debit the supplied family's real ledger.
            directory = Path(args.output + "-fake-ledger")
            directory.mkdir(mode=0o700, parents=True, exist_ok=False)
            ledger = directory / "rows.jsonl"
            rows.init(str(ledger))
            plan = {**plan, "row_ledger": str(ledger)}
        result = PairedCampaign(plan, args.output, FakeBackend(plan["first_epoch"])).run()
        print(json.dumps({key: result[key] for key in ("status", "fake_provider", "accepted_comparison", "authorized_rows", "charged_rows",
                                                     "held_host_count", "released_waves", "cleanup_passed")}, sort_keys=True))
    else:
        p.require(args.plan, "explicit paired plan required")
        plan = validate_plan(d.read(args.plan))
        p.require(not args.execute_approved, "live paired backend not admitted: control setup/oracle and independent-watch contract required")
        print(json.dumps({"validated": True, "schema": SCHEMA, "authorized_rows": authorized_rows(plan), "provisioned": False}, sort_keys=True))


if __name__ == "__main__":
    main()
