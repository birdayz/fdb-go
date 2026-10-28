#!/usr/bin/env python3
"""Pure guards for one reviewed fresh 100k COW diagnostic arm."""
import hashlib
import json
import math
from pathlib import Path
import re
import sys

GiB = 1024 ** 3
PROFILE = {"owner":"otel283-vxnar", "profile":"fdb4-4cpu-16g-ingest20k-v1",
           "count":100000, "batch":32, "workers":2, "ports":[14883,14884,14885,14886],
           "image":"foundationdb/foundationdb@sha256:559b178a137bbe09272ad6e3752531aa04e2ddacab8ccfa4ffa86467db033bea",
           "total_seconds":1200, "load_seconds":300, "drain_seconds":300, "census_seconds":120, "oracle_seconds":120}
RUNTIME_FILES = ("policy.py", "driver.py", "status-check.jq", "foundationdb.conf", "fdb.cluster.seed", "container-entrypoint.sh", "deadline.sh", "inode-readonly.sh", "inode_flags.c", "build-probe.sh")


def arm(run, batch=32, workers=2):
    require(isinstance(run, str) and re.fullmatch(r"prof20k[0-9]{4}", run) is not None, "explicit reviewed fresh arm required")
    require(type(batch) is int and batch in (32, 64) and type(workers) is int and workers in (2, 4, 8), "sealed batch/worker allowlist")
    return {**PROFILE, "batch":batch, "workers":workers, "run":run, "session":run,
            "container":"otel283-fdb4-"+run+"-vxnar", "volume":"otel283-vxnar-fdb4-"+run+"16-v1"}


def require(ok, reason):
    if not ok:
        raise ValueError(reason)


def uint(value):
    return type(value) is int and 0 <= value < 10 ** 18


def number(value):
    return type(value) in (int, float) and math.isfinite(value) and value >= 0


def hexstr(value, size=64):
    return isinstance(value, str) and re.fullmatch("[0-9a-f]{%d}" % size, value) is not None


def pairs(items):
    result = {}
    for key, value in items:
        require(key not in result, "duplicate JSON key")
        result[key] = value
    return result


def finite_float(text):
    value = float(text)
    require(math.isfinite(value), "nonfinite JSON")
    return value


def loads(text):
    return json.loads(text, object_pairs_hook=pairs, parse_float=finite_float,
                      parse_constant=lambda _: require(False, "nonfinite JSON"))


def sha(path):
    digest = hashlib.sha256()
    with open(path, "rb") as stream:
        for block in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(block)
    return digest.hexdigest()


def runtime_sha(root=None):
    root = Path(root or __file__).resolve()
    if root.is_file():
        root = root.parent
    return hashlib.sha256("".join(f"{sha(root / name)}  {name}\n" for name in RUNTIME_FILES).encode()).hexdigest()


def floor_bytes(total):
    require(uint(total) and total > 0, "invalid filesystem total")
    # Exact integer ceil to 4096; no binary-float rounding of the six-percent reserve.
    numerator = 200 * GiB * 100 + 6 * total
    return ((numerator + 409599) // 409600) * 4096


def approval(d):
    require(isinstance(d, dict), "one approval object required")
    for key, value in arm(d.get("run"), d.get("batch"), d.get("workers")).items():
        require(type(d.get(key)) is type(value) and d[key] == value, "literal arm mismatch: " + key)
    require(all(type(port) is int for port in d["ports"]), "integer ports required")
    require(d.get("schema") == "ingest20k_fresh100k_v1" and d.get("approved") is True, "explicit arm approval required")
    require(hexstr(d.get("source_revision"),40) and d.get("source_approved_revision") == d["source_revision"], "explicitly approved application source required")
    for key in ("binary_sha256", "input_manifest_sha256", "policy_sha256", "probe_sha256", "probe_source_sha256"):
        require(hexstr(d.get(key)), "invalid immutable hash: " + key)
    require(d.get("policy_approved_sha256") == d["policy_sha256"], "explicitly approved current policy required")
    require(d.get("quiet_thresholds_approved") is True and d.get("growth_overshoot_accepted") is True, "quiet and observational-cap approvals required")
    for key in ("binary", "source_root", "source_manifest", "probe_binary", "out"):
        require(isinstance(d.get(key),str) and Path(d[key]).is_absolute(), "explicit absolute path: " + key)
    require(Path(d["out"]).name == "ingest20k-"+d["run"], "fresh exact output identity")
    return {"approved":True}


def capacity(d):
    for key in ("total_bytes", "available_bytes", "allocated_bytes", "host_available_bytes", "cgroup_bytes", "host_receipt_bytes"):
        require(uint(d.get(key)), "missing/invalid capacity: " + key)
    floor = floor_bytes(d["total_bytes"])
    require(d["available_bytes"] <= d["total_bytes"], "impossible available space")
    require(d["available_bytes"] >= floor, "global conservative free-space floor")
    require(d["allocated_bytes"] < 64 * GiB, "observed allocated-growth stop threshold")
    require(d["host_receipt_bytes"] < GiB and d["host_receipt_bytes"] <= d["allocated_bytes"], "observed one-GiB host-receipt budget")
    require(d["host_available_bytes"] >= 8 * GiB, "host below 8 GiB")
    require(d["cgroup_bytes"] < 16 * GiB * 90 // 100, "cgroup reaches 90 percent")
    return {"floor_bytes": floor, "allocated_bytes": d["allocated_bytes"]}


class QuietBusy(ValueError):
    """Valid accounting that does not admit a quiet launch window."""


def quiet_observations(samples):
    require(isinstance(samples, list) and 1 <= len(samples) <= 7, "one to seven quiet observations required")
    limits = {"cpu_psi_some_avg10": 5.0, "memory_psi_some_avg10": 0.1,
              "memory_psi_full_avg10": 0.01, "io_psi_full_avg10": 1.0}
    for s in samples:
        require(isinstance(s, dict), "invalid quiet sample")
        require(uint(s.get("epoch")) and uint(s.get("mem_available_bytes")), "quiet time/RAM missing")
        for key in limits:
            value = s.get(key)
            require(type(value) in (int, float) and value >= 0
                    and (type(value) is int or math.isfinite(value)), "quiet PSI accounting invalid: " + key)
        require(uint(s.get("pswpin")) and uint(s.get("pswpout")), "swap counters missing")
    for a, b in zip(samples, samples[1:]):
        require(10 <= b["epoch"] - a["epoch"] <= 15, "quiet samples not stable ten-second intervals")
        require(b["pswpin"] >= a["pswpin"] and b["pswpout"] >= a["pswpout"], "swap counters decreased")
    # Busy classification follows validation of the entire window, never masks a failed observation.
    reasons = []
    memory = [s["mem_available_bytes"] for s in samples]
    if min(memory) < 24 * GiB:
        reasons.append("launch below 24 GiB")
    for key, limit in limits.items():
        if any(s[key] > limit for s in samples):
            reasons.append("quiet PSI threshold: " + key)
    if max(memory) - min(memory) > GiB:
        reasons.append("host RAM unstable")
    if any(b[key] > a[key] for a, b in zip(samples, samples[1:]) for key in ("pswpin", "pswpout")):
        reasons.append("active swap I/O")
    if reasons:
        raise QuietBusy("; ".join(reasons))


def quiet(d):
    samples = d.get("samples")
    require(isinstance(samples, list) and len(samples) == 7, "exact seven quiet samples required")
    quiet_observations(samples)
    return {"quiet_samples": 7, "span_seconds": samples[-1]["epoch"] - samples[0]["epoch"]}


def readiness(d):
    samples = d.get("samples")
    require(isinstance(samples, list) and len(samples) == 3, "exact three strict samples required")
    for s in samples:
        require(isinstance(s, dict) and uint(s.get("epoch")), "invalid strict sample")
        require(s.get("aggregate_healthy_original") is True, "strict original health required")
        require(uint(s.get("queue_bytes")) and s["queue_bytes"] < 16777216, "strict queue")
        require(number(s.get("durability_lag_seconds")) and s["durability_lag_seconds"] < 10, "strict lag")
    require(all(b["epoch"] - a["epoch"] >= 5 for a, b in zip(samples, samples[1:])), "strict samples need five real seconds")
    return {"strict_samples": 3}


def bootstrap_get(d):
    require(isinstance(d, dict) and d.get("path") == "/var/fdb/data", "initial GET root identity")
    for key in ("flags", "uid", "dev", "ino", "nlink", "size", "allocated_bytes", "mount_id"):
        require(uint(d.get(key)), "invalid initial GET fact: " + key)
    require(d["flags"] < 2 ** 32 and d.get("nocow") is False and d["flags"] & 0x00800000 == 0, "initial GET NOCOW rejected")
    require(type(d.get("compression_flag")) is bool and d["compression_flag"] == bool(d["flags"] & 4), "initial GET compression inconsistency")
    require(d["uid"] == 0 and all(d[key] > 0 for key in ("dev", "ino", "mount_id", "nlink")), "initial GET ownership/inode identity")
    require(d.get("filesystem") == "btrfs" and d.get("nodatasum") == "NOT_EXPOSED", "initial GET filesystem facts")
    return {"bootstrap_get_valid": True}


def inode_proof(d):
    rows = d.get("rows")
    require(isinstance(rows, list) and len(rows) >= 18, "nonempty complete GET proof required")
    tail = rows[-1]
    require(tail.get("schema") == "ingest20k_inode_proof_v1" and type(tail.get("regular_files_verified")) is int and tail["regular_files_verified"] >= 6 and type(tail.get("primary_sets_verified")) is int and tail["primary_sets_verified"] == 3, "inode proof summary")
    facts, paths = rows[:-1], set()
    for row in facts:
        require(isinstance(row, dict) and isinstance(row.get("path"), str) and row["path"] not in paths, "duplicate/malformed inode path")
        path = row["path"]
        directory = path == "/var/fdb/data" or path in ("/var/fdb/data/cluster", "/var/fdb/data/processes", "/var/fdb/data/traces") or re.fullmatch(r"/var/fdb/data/(processes|traces)/1488[3-6]", path)
        regular = path in ("/var/fdb/data/cluster/fdb.cluster", "/var/fdb/data/.otel283-owner") or re.fullmatch(r"/var/fdb/data/(processes|traces)/1488[3-6]/[A-Za-z0-9_.-]+", path)
        require((directory or regular) and len(Path(path).name) <= 255 and Path(path).name not in (".", ".."), "unexpected inode path")
        for key in ("flags", "uid", "dev", "ino", "nlink", "size", "allocated_bytes", "mount_id"):
            require(uint(row.get(key)), "invalid inode fact: " + key)
        require(row["flags"] < 2 ** 32 and row.get("nocow") is False and row["flags"] & 0x00800000 == 0, "NOCOW inode rejected")
        require(type(row.get("compression_flag")) is bool and row["compression_flag"] == bool(row["flags"] & 4), "compression flag inconsistency")
        require(row["uid"] == 4059 and row["ino"] > 0 and row["mount_id"] > 0 and row["nlink"] > 0 and row.get("filesystem") == "btrfs" and row.get("nodatasum") == "NOT_EXPOSED", "inode ownership/filesystem facts")
        require((row["dev"], row["mount_id"]) == (facts[0]["dev"], facts[0]["mount_id"]), "inode mount/device mismatch")
        paths.add(path)
    required = {"/var/fdb/data", "/var/fdb/data/cluster", "/var/fdb/data/processes", "/var/fdb/data/traces", "/var/fdb/data/cluster/fdb.cluster", "/var/fdb/data/.otel283-owner"}
    required.update(f"/var/fdb/data/{area}/{port}" for area in ("processes", "traces") for port in range(14883, 14887))
    require(required <= paths, "required directories/metadata absent")
    for port in (14883, 14886):
        require(sum(bool(re.fullmatch(fr"/var/fdb/data/processes/{port}/storage-[0-9a-f]{{32}}\.redwood-v1", path)) for path in paths) == 1, "exact Redwood primary required")
    logs = [re.fullmatch(r"/var/fdb/data/processes/14884/log2-V_6-([0-9a-f]{32})\.sqlite", path) for path in paths]
    ids = [match[1] for match in logs if match]
    require(len(ids) == 1, "exact SQLite primary required")
    prefix, uid = "/var/fdb/data/processes/14884/", ids[0]
    require({prefix+f"log2-V_6-{uid}.sqlite-wal", prefix+f"logqueue-V_6-{uid}-0.fdq", prefix+f"logqueue-V_6-{uid}-1.fdq"} <= paths, "matching tlog WAL/queues required")
    regular_count = sum(path not in required or path in ("/var/fdb/data/cluster/fdb.cluster", "/var/fdb/data/.otel283-owner") for path in paths)
    require(regular_count == tail["regular_files_verified"], "regular-file population mismatch")
    return {"inodes_verified": len(facts)}


def backlog(d):
    require(uint(d.get("queue_bytes")) and number(d.get("lag_seconds")), "invalid backlog accounting")
    require(uint(d.get("previous_streak")) and d["previous_streak"] <= 5, "invalid prior backlog streak")
    streak = d["previous_streak"] + 1 if d["queue_bytes"] > 536870912 and d["lag_seconds"] > 60 else 0
    require(streak < 6, "six consecutive queue AND lag violations")
    return {"streak": streak}


def phase(d):
    expected = arm(d.get("run"))
    for key in ("run", "session", "profile", "count"):
        require(type(d.get(key)) is type(expected[key]) and d[key] == expected[key], "phase identity: "+key)
    require(hexstr(d.get("source_revision"),40), "phase source revision")
    for key in ("instance", "binary_sha256", "input_manifest_sha256", "policy_sha256"):
        require(hexstr(d.get(key)), "phase hash: "+key)
    for key in ("started_epoch", "deadline_epoch", "now", "stage_started_epoch", "stage_deadline_epoch"):
        require(uint(d.get(key)), "phase time: "+key)
    limits = {"load":300, "drain":300, "census":120, "oracle":120}
    require(d.get("stage") in limits, "unknown stage")
    require(d["deadline_epoch"]-d["started_epoch"] == 1200, "absolute 1200-second deadline")
    require(d["started_epoch"] <= d["stage_started_epoch"] <= d["now"] < d["stage_deadline_epoch"] <= d["deadline_epoch"], "expired/outside phase")
    require(0 < d["stage_deadline_epoch"]-d["stage_started_epoch"] <= limits[d["stage"]], "stage cap")
    return {"phase_valid":True}


def evidence(d, approved=None):
    if approved is None:
        approved, d = d["approved"], d["evidence"]
    approval(approved)
    require(d.get("tool") == "otelbench-v2" and d.get("backend") == "fdb" and d.get("revision") == approved["source_revision"], "wrong evidence source/backend")
    require(d.get("failures") == 0 and type(d.get("failures")) is int and ("error" not in d or type(d["error"]) is str and d["error"] == ""), "failed evidence")
    config = d.get("config", {})
    expected = {"run": approved["run"], "backend": "fdb", "count": 100000, "tenants": 1,
                "attributes": 8, "seed": 283, "match_every": 100, "batch": approved["batch"], "save_mode": "batch",
                "workers": approved["workers"], "diagnostics": True, "tenant": 0, "k": 100, "max_candidates": 2000000,
                "max_bytes": 268435456, "hash_bytes": 32, "order": "time"}
    require(all(type(config.get(k)) is type(v) and config.get(k) == v for k, v in expected.items()), "wrong evidence config")
    mode = d.get("mode")
    require(mode in ("ingest", "audit", "query"), "wrong evidence mode")
    if mode in ("ingest", "audit"):
        stats = d.get("stats", {})
        require(type(stats.get("rows")) is int and stats["rows"] == 100000, "incomplete load/source records")
        require(type(stats.get("attributes")) is int and stats["attributes"] == 800000, "incomplete attributes")
        if mode == "audit":
            structures = stats.get("structures", {})
            counts = {"records": 100000, "eq_secondary": 800000, "span_id_v2": 100000,
                      "span_time_v2": 100000, "owned_total": 1100002, "eq_primary": 0}
            require(all(type(structures.get(k, {}).get("keys")) is int and structures[k]["keys"] == v for k, v in counts.items()), "incomplete structure census")
    else:
        require(d.get("suite") == "all" and type(config.get("iterations")) is int and config["iterations"] == 1600, "exact independent oracle suite required")
        classes = ("trace", "trace_absent", "span", "span_absent", "genai_response", "genai_scope_resource",
                   "genai_scope_scope", "genai_type", "genai_absent", "genai_operation", "genai_provider", "genai_model",
                   "time_narrow", "time_empty", "time_absent", "time_broad")
        samples = d.get("samples")
        require(isinstance(samples, list) and len(samples) == 1600, "oracle population missing")
        totals = {name: 0 for name in classes}
        for i, sample in enumerate(samples):
            require(isinstance(sample, dict) and type(sample.get("iteration")) is int and sample["iteration"] == i, "oracle iteration identity")
            require(sample.get("class") == classes[i % 16] and ("error" not in sample or type(sample["error"]) is str and sample["error"] == ""), "oracle class/error")
            require(uint(sample.get("ns")) and hexstr(sample.get("result_digest")), "oracle sample receipt missing")
            totals[sample["class"]] += 1
        require(all(value == 100 for value in totals.values()), "oracle class population")
    return {"mode": mode, "verified": True}


def startup_pending(d):
    # 7.3.77 Status.actor.cpp:54-68 and DD initializing state at 1825-1830.
    # Only clean, typed native progress snapshots may wait; warnings never do.
    c, client = d["cluster"], d["client"]
    name = c["recovery_state"]["name"]
    progress = {"reading_coordinated_state", "locking_coordinated_state", "locking_old_transaction_servers",
                "reading_transaction_system_state", "recruiting_transaction_servers", "initializing_transaction_servers",
                "recovery_transaction", "writing_coordinated_state", "accepting_commits", "all_logs_recruited", "storage_recovered"}
    state = c.get("data", {}).get("state")
    initializing = isinstance(state, dict) and state.get("name") == "initializing" and "healthy" not in state
    require(name in progress or name == "fully_recovered" and initializing, "not a recognized startup progress state")
    require(c.get("connection_string") == "otel20k:otel20k@127.0.0.1:14883", "startup seed mismatch")
    require(client["coordinators"].get("quorum_reachable") is True and client["cluster_file"].get("up_to_date") is True, "startup quorum/file")
    require(client.get("messages") == [] and type(client.get("messages")) is list and c.get("messages") == [] and type(c.get("messages")) is list, "startup messages are not waived")
    if "configuration" in client:
        require(isinstance(client["configuration"], dict), "startup client configuration shape")
        require("messages" not in client["configuration"] or client["configuration"]["messages"] == [], "startup configuration issues")
    require(type(c.get("database_available")) is bool and type(client["database_status"].get("available")) is bool and type(client["database_status"].get("healthy")) is bool, "startup availability/health shape")
    available = name in ("accepting_commits", "all_logs_recruited", "storage_recovered", "fully_recovered") and c["database_available"]
    require(client["database_status"]["available"] == available, "startup availability contradiction")
    require(type(c.get("degraded_processes")) is int and c["degraded_processes"] == 0, "startup degraded process")
    configuration = c.get("configuration", {})
    require(isinstance(configuration, dict), "startup server configuration shape")
    expected = {"storage_engine":"ssd-redwood-1", "log_engine":"ssd-2", "redundancy_mode":"single", "logs":1, "commit_proxies":1, "grv_proxies":1, "resolvers":1}
    for key, value in expected.items():
        require(key not in configuration or type(configuration[key]) is type(value) and configuration[key] == value, "startup configuration mismatch")
    processes = c.get("processes")
    require(isinstance(processes, dict) and 1 <= len(processes) <= 4, "startup process population")
    addresses = set()
    for process in processes.values():
        address = process["address"]
        classes = {"127.0.0.1:14883":"storage", "127.0.0.1:14884":"transaction", "127.0.0.1:14885":"stateless", "127.0.0.1:14886":"storage"}
        require(address in classes and address not in addresses and process.get("class_type") == classes[address], "startup process identity")
        addresses.add(address)
        require(process["locality"].get("machineid") == "otel283-vxnar-ingest20k" and process["locality"].get("zoneid") == "otel283-vxnar-ingest20k", "startup locality")
        require(process.get("messages") == [] and type(process.get("messages")) is list and isinstance(process.get("roles"), list), "startup process messages/roles")
        require(all(isinstance(role, dict) and isinstance(role.get("role"), str) and role["role"] for role in process["roles"]), "startup role shape")
        require(uint(process["memory"].get("rss_bytes")) and uint(process["memory"].get("used_bytes")) and type(process["memory"].get("limit_bytes")) is int and process["memory"]["limit_bytes"] == 4294967296, "startup process memory")
    require("state" not in c.get("data", {}) or state is not None, "startup data state shape")
    require(client["database_status"]["healthy"] == (available and state is not None), "startup aggregate health contradiction")
    if state is not None:
        require(isinstance(state, dict) and (initializing or state.get("healthy") is True and state.get("name") in {"healthy", "healthy_repartitioning", "optimizing_team_collections", "healthy_rebalancing", "healthy_perpetual_wiggle", "healthy_removing_server", "healthy_populating_region"}), "startup data issue is not waived")
    probes = c.get("latency_probe", {})
    require(isinstance(probes, dict) and all(number(value) and value < 5 for value in probes.values()), "startup probe shape/limit")
    qos = c.get("qos", {})
    require(isinstance(qos, dict), "startup qos shape")
    if "worst_queue_bytes_storage_server" in qos:
        require(uint(qos["worst_queue_bytes_storage_server"]), "startup queue shape")
    if "worst_durability_lag_storage_server" in qos:
        require(number(qos["worst_durability_lag_storage_server"]["seconds"]), "startup lag shape")
    return {"startup_pending": True, "aggregate_healthy_original": client["database_status"]["healthy"], "recovery_state": name}


def clean_stop(d):
    clean = (d.get("running") is False and d.get("status") == "exited"
             and type(d.get("exit_code")) is int and d["exit_code"] == 0
             and d.get("oom") is False and type(d.get("restarts")) is int and d["restarts"] == 0
             and d.get("shutdown_ok") is True)
    return {"clean_stop": clean}


def check(kind, d):
    require(isinstance(d,dict), "one JSON object required")
    functions = {"approval":approval,"capacity":capacity,"quiet":quiet,"phase":phase,"readiness":readiness,"backlog":backlog,"evidence":evidence,"bootstrap_get":bootstrap_get,"inode_proof":inode_proof,"clean_stop":clean_stop,"startup_pending":startup_pending}
    require(kind in functions,"unknown predicate")
    return functions[kind](d)


if __name__ == "__main__":
    try:
        require(len(sys.argv)==3,"usage: policy.py KIND JSON_FILE")
        print(json.dumps(check(sys.argv[1],loads(Path(sys.argv[2]).read_text())),sort_keys=True))
    except (ValueError,KeyError,TypeError,AttributeError,IndexError,OSError) as error:
        print("policy rejected: "+str(error),file=sys.stderr)
        sys.exit(1)
