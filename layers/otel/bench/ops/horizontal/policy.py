#!/usr/bin/env python3
"""Typed guards for the independent-store, single-replica horizontal experiment."""
from datetime import datetime
from decimal import Decimal
import hashlib
import ipaddress
import math
from pathlib import Path
import re

GIB = 1024 ** 3
CLASSES = ("trace", "trace_absent", "span", "span_absent", "genai_response", "genai_scope_resource",
           "genai_scope_scope", "genai_type", "genai_absent", "genai_operation", "genai_provider",
           "genai_model", "time_narrow", "time_empty", "time_absent", "time_broad")
ASSETS = ("binary", "fdbserver", "fdbcli", "source_manifest")


def require(value, reason):
    if not value:
        raise ValueError(reason)


def integer(value, minimum=0):
    return type(value) is int and minimum <= value < 10 ** 18


def number(value):
    return type(value) in (int, float) and math.isfinite(value) and value >= 0


def sha(path):
    h = hashlib.sha256()
    with open(path, "rb") as stream:
        for block in iter(lambda: stream.read(1024 * 1024), b""):
            h.update(block)
    return h.hexdigest()


def hexstr(value, length=64):
    return type(value) is str and re.fullmatch(r"[0-9a-f]{%d}" % length, value) is not None


def storage(options):
    keys = ("record_compression", "posting_value_format", "bulk_index_conflicts", "hash_bytes")
    require(type(options) is dict and set(options) == set(keys), "exact storage profile required")
    require(tuple(type(options[k]) for k in keys) == (str, str, bool, int), "typed storage profile required")
    require(tuple(options[k] for k in keys) in (("none", "json", False, 32),
            ("zlib-1", "tuple", True, 8), ("brotli-4", "tuple", True, 8)), "unknown storage profile")
    return dict(options)


def validate_plan(plan, *, check_assets=True):
    require(type(plan) is dict and plan.get("schema") == "otel_horizontal_v1" and plan.get("approved") is True,
            "explicit horizontal approval required")
    for key in ("campaign", "prefix"):
        require(type(plan.get(key)) is str and re.fullmatch(r"[a-z][a-z0-9-]{7,47}", plan[key]), "invalid " + key)
    require(plan.get("location") == "hel1" and plan.get("server_type") == "ccx43", "sealed host catalog choice")
    require(plan.get("replication") == "single", "single-replica non-HA capacity experiment only")
    require(type(plan.get("topologies")) is list and len(plan["topologies"]) == 3
            and all(type(x) is int for x in plan["topologies"]) and plan["topologies"] == [1, 2, 3], "exact storage-host sweep")
    if "selected_topologies" in plan:
        selected = plan["selected_topologies"]
        require(type(selected) is list and len(selected) == 2 and all(type(x) is int for x in selected)
                and selected == [3, 2], "independent selected storage-host study must be exactly 3/2")
    if "prerequisite_topologies" in plan:
        order = plan["prerequisite_topologies"]
        require(type(order) is list and len(order) == 3 and all(type(x) is int for x in order)
                and order in ([1, 2, 3], [3, 2, 1]), "preregistered prerequisite storage-host order 1/2/3 or 3/2/1")
    require(type(plan.get("measured_topologies")) is list and len(plan["measured_topologies"]) == 3
            and all(type(x) is int for x in plan["measured_topologies"])
            and plan["measured_topologies"] in ([3, 2, 1], [2, 3, 1]),
            "preregistered measured storage-host order 3/2/1 or 2/3/1")
    require(type(plan.get("count")) is int and plan["count"] == 100000, "mandatory 100k prerequisite")
    require(type(plan.get("measured_count")) is int and plan["measured_count"] in (100000, 1000000), "explicit measured count")
    require(type(plan.get("batch")) is int and plan["batch"] in (32, 64)
            and type(plan.get("workers")) is int and plan["workers"] in (2, 4, 8)
            and type(plan.get("gomaxprocs")) is int and plan["gomaxprocs"] == 4, "sealed client resources")
    storage(plan.get("storage_options"))
    require(hexstr(plan.get("source_revision"), 40), "sealed application revision")
    require(integer(plan.get("first_epoch"), 1) and integer(plan.get("deadline_epoch"), 1)
            and plan["deadline_epoch"] - plan["first_epoch"] == 7200, "absolute two-hour lifetime")
    require(type(plan.get("controller_cidr")) is str, "controller CIDR required")
    cidr = ipaddress.ip_network(plan["controller_cidr"], strict=True)
    require(cidr.prefixlen == cidr.max_prefixlen, "one controller address only")
    require(type(plan.get("hcloud")) is str and Path(plan["hcloud"]).is_absolute(), "explicit hcloud context wrapper")
    for key in ("controller_public_key", "controller_private_key", "source_root"):
        require(type(plan.get(key)) is str and Path(plan[key]).is_absolute(), "explicit local path: " + key)
    hashes = {"source_manifest": "input_manifest_sha256"}
    for key in ASSETS:
        require(type(plan.get(key)) is str and Path(plan[key]).is_absolute(), "explicit asset: " + key)
        expected = plan.get(hashes.get(key, key + "_sha256"))
        require(hexstr(expected), "sealed asset hash: " + key)
        if check_assets:
            path = Path(plan[key])
            require(path.is_file() and not path.is_symlink() and sha(path) == expected, "changed asset: " + key)
    require(authorized_rows(plan) <= 10000000, "10M cumulative insertion ceiling")
    return plan


def verify_source(plan):
    """Hash the actual sealed source files, not merely the manifest text."""
    root = Path(plan["source_root"]).resolve()
    require(root.is_dir() and not Path(plan["source_root"]).is_symlink(), "sealed source root required")
    require(sha(plan["source_manifest"]) == plan["input_manifest_sha256"], "changed source manifest")
    seen = set()
    for line in Path(plan["source_manifest"]).read_text().splitlines():
        require(len(line) > 66 and hexstr(line[:64]) and line[64:66] == "  ", "malformed source manifest")
        relative = Path(line[66:])
        require(not relative.is_absolute() and ".." not in relative.parts and str(relative) not in seen,
                "unsafe or duplicate source manifest path")
        seen.add(str(relative))
        path = root / relative
        require(path.resolve().is_relative_to(root) and not path.is_symlink() and path.is_file()
                and sha(path) == line[:64], "changed source file: " + str(relative))
    require(len(seen) > 0, "empty source manifest")
    return {"source_files": len(seen), "input_manifest_sha256": plan["input_manifest_sha256"]}


def authorized_rows(plan):
    return len(plan.get("selected_topologies", plan["topologies"])) * 2 * (100000 + plan["measured_count"])


def require_promotion(gate, hosts):
    require(type(gate) is dict and gate.get("storage_hosts") == hosts and type(gate.get("storage_hosts")) is int
            and gate.get("count_per_producer") == 100000 and gate.get("healthy") is True
            and gate.get("both_censuses") is True and gate.get("both_oracles") is True,
            "1M promotion requires this topology's complete healthy 100k pair")


def common_throughput(rows, started_ns, ended_ns):
    require(integer(rows, 1) and integer(started_ns, 1) and integer(ended_ns, 1) and ended_ns > started_ns,
            "nonempty controller common window required")
    return rows * 10 ** 9 / (ended_ns - started_ns)


def topology_cost(resources, storage_hosts, rate):
    require(type(storage_hosts) is int and storage_hosts in (1, 2, 3) and number(rate) and rate > 0,
            "nonempty topology cost population")
    net, gross = Decimal(resources["price_hourly_net"]), Decimal(resources["price_hourly_gross"])
    require(net.is_finite() and gross.is_finite() and net > 0 and gross >= net, "live catalog cost required")
    active = storage_hosts + 2
    factor = Decimal(1000000) / Decimal(str(rate)) / Decimal(3600)
    return {"scope": "SERVER-ONLY", "excludes": ["IPv4", "traffic"],
            "active_topology_nodes": active, "held_billable_nodes": 5, "currency": resources.get("currency"),
            "hourly_net": str(net * active), "hourly_gross": str(gross * active),
            "per_million_ack_rows_net": str(net * active * factor),
            "per_million_ack_rows_gross": str(gross * active * factor),
            "pricing_receipt_sha256": resources["pricing_receipt_sha256"],
            "boundary": "server-only logical active topology; IPv4 and traffic charges excluded; all five reserved servers are billed separately"}


def bill_bound(resources, cleanup):
    start, end = resources.get("first_create_epoch"), cleanup.get("finished_epoch")
    require(number(start) and number(end) and end >= start, "bill lifetime receipts required")
    hours = math.ceil((end - start) / 3600)
    return {"currency": resources.get("currency"), "held_servers": 5, "rounded_hours_per_server_upper_bound": hours,
            "net_upper_bound": str(Decimal(resources["price_hourly_net"]) * 5 * hours),
            "gross_upper_bound": str(Decimal(resources["price_hourly_gross"]) * 5 * hours),
            "elapsed_seconds_upper_bound": end - start,
            "boundary": "conservative rounded server-charge bound, first resource command through verified deletion; excludes IPv4, not an invoice"}


def rounded_server_bill(resources, cleanup):
    """Receipt-derived rounded server charges; provider invoice remains authoritative."""
    nodes = resources.get("nodes", {})
    charges = []
    for node in nodes.values():
        created = node.get("created")
        deletions = [x for x in cleanup.get("actions", []) if x.get("kind") == "server"
                     and x.get("id") == node["id"] and x.get("returncode") == 0]
        if type(created) is not str or not deletions:
            return {"known": False, "reason": "missing server creation or successful deletion timing", "charges": charges}
        start = datetime.fromisoformat(created.replace("Z", "+00:00"))
        require(start.tzinfo is not None, "server creation timestamp must include timezone")
        end = min(x["finished_epoch"] for x in deletions)
        require(number(end) and end >= start.timestamp(), "server bill timestamp order")
        hours = max(1, math.ceil((end - start.timestamp()) / 3600))
        charges.append({"id": node["id"], "created": created, "delete_ack_epoch": end, "rounded_hours": hours})
    require(len(charges) == 5, "bill population must be all five new servers")
    hours = sum(x["rounded_hours"] for x in charges)
    return {"known": True, "currency": resources.get("currency"), "charges": charges, "server_hours": hours,
            "net": str(Decimal(resources["price_hourly_net"]) * hours),
            "gross": str(Decimal(resources["price_hourly_gross"]) * hours),
            "boundary": "rounded server estimate from API creation/delete-ACK receipts; excludes IPv4; not an invoice"}


def storage_counter(role, name):
    if type(role) is not dict:
        return None
    metric = role.get(name)
    value = metric.get("counter") if type(metric) is dict else None
    if integer(value):
        return value
    if type(value) is float and number(value) and value.is_integer() and value <= 2 ** 53 - 1:
        return int(value)
    return None


def strict_status(d, layout):
    """Same strict health/probe conditions as ingest20k; only process identities differ."""
    c, client = d["cluster"], d["client"]
    require(client["database_status"]["available"] is True and client["database_status"]["healthy"] is True
            and client["coordinators"]["quorum_reachable"] is True and client["cluster_file"]["up_to_date"] is True,
            "strict client health")
    require(type(client["messages"]) is list and not client["messages"], "client status warning")
    if "configuration" in client and "messages" in client["configuration"]:
        require(type(client["configuration"]["messages"]) is list and not client["configuration"]["messages"], "configuration warning")
    require(type(c["messages"]) is list and not c["messages"] and c["database_available"] is True
            and c["data"]["state"]["healthy"] is True and c["recovery_state"]["name"] == "fully_recovered"
            and type(c["degraded_processes"]) is int and c["degraded_processes"] == 0, "strict native cluster health")
    for key, value in {"storage_engine": "ssd-redwood-1", "log_engine": "ssd-2", "redundancy_mode": "single",
                       "logs": 1, "commit_proxies": 1, "grv_proxies": 1, "resolvers": 1}.items():
        require(type(c["configuration"].get(key)) is type(value) and c["configuration"][key] == value, "native configuration: " + key)
    for key in ("transaction_start_seconds", "immediate_priority_transaction_start_seconds", "read_seconds", "commit_seconds"):
        require(number(c["latency_probe"][key]) and c["latency_probe"][key] < 5, "strict latency probe")
    qos = c["qos"]
    require(integer(qos["worst_queue_bytes_storage_server"]) and number(qos["worst_durability_lag_storage_server"]["seconds"])
            and type(qos["performance_limited_by"]) is dict
            and type(qos["performance_limited_by"].get("name")) is str and qos["performance_limited_by"]["name"], "native QoS shape")
    expected = {x["address"]: x for x in layout["processes"]}
    actual = c["processes"]
    require(type(actual) is dict and len(actual) == len(expected), "exact native process population")
    require({x["address"] for x in actual.values()} == set(expected), "unexpected native process address")
    data = {}
    exclusive_roles = {"storage", "log", "commit_proxy", "grv_proxy", "resolver"}
    for process in actual.values():
        spec = expected[process["address"]]
        roles = process["roles"]
        require(type(roles) is list and roles and all(type(x.get("role")) is str and x["role"] for x in roles), "native roles shape")
        names = {x["role"] for x in roles}
        required = set(spec["required_roles"])
        require(required <= names and not (exclusive_roles - required) & names, "exact process role placement")
        require(process["class_type"] == spec["process_class"]
                and process["locality"]["machineid"] == spec["machineid"] and process["locality"]["zoneid"] == spec["zoneid"], "native physical host identity")
        memory = process["memory"]
        require(integer(memory["rss_bytes"]) and integer(memory["used_bytes"])
                and type(memory["limit_bytes"]) is int and memory["limit_bytes"] == spec["memory_limit_bytes"], "native process memory")
        require(type(process["messages"]) is list and not process["messages"], "process status warning")
        if "storage" in names:
            role = next(x for x in roles if x["role"] == "storage")
            stored = role.get("stored_bytes")
            shards = role.get("shards")
            data[spec["node"]] = {"input_bytes": storage_counter(role, "input_bytes"),
                                  "durable_bytes": storage_counter(role, "durable_bytes"),
                                  "stored_bytes": stored if integer(stored) else None,
                                  "shards": shards if integer(shards) else None, "storage_id": role.get("id")}
    require(set(data) == set(layout["storage_hosts"]), "all selected storage hosts must be active")
    return {"healthy": True, "queue_bytes": qos["worst_queue_bytes_storage_server"],
            "lag_seconds": qos["worst_durability_lag_storage_server"]["seconds"], "storage": data}


def distributed(before, during, hosts, after=()):
    """Unknown counters are inconclusive, never defaulted to zero or inferred from roles."""
    reasons, deltas, durable_deltas = [], {}, {}
    for node in hosts:
        start = before.get("storage", {}).get(node, {})
        counters = [x.get("storage", {}).get(node, {}).get("input_bytes") for x in during]
        all_counters = counters + [x.get("storage", {}).get(node, {}).get("input_bytes") for x in after]
        stored = [x.get("storage", {}).get(node, {}).get("stored_bytes") for x in during]
        identities = [x.get("storage", {}).get(node, {}).get("storage_id") for x in [before, *during, *after]]
        if not all(type(x) is str and x and x == identities[0] for x in identities):
            reasons.append(node + ": storage identity changed or missing")
            continue
        if not integer(start.get("input_bytes")) or not counters or not all(integer(x) for x in all_counters):
            reasons.append(node + ": missing native input-byte counters")
            continue
        if any(b < a for a, b in zip([start["input_bytes"]] + all_counters, all_counters)):
            reasons.append(node + ": storage counter decreased")
            continue
        durable = [x.get("storage", {}).get(node, {}).get("durable_bytes") for x in [before, *during, *after]]
        if any(x is not None for x in durable):
            if not all(integer(x) for x in durable):
                reasons.append(node + ": incomplete optional durable-byte counters")
                continue
            if any(b < a for a, b in zip(durable, durable[1:])):
                reasons.append(node + ": durable counter decreased")
                continue
            durable_deltas[node] = max(durable[1:1 + len(during)]) - durable[0]
        delta = max(counters) - start["input_bytes"]
        deltas[node] = delta
        if delta < 1024 ** 2 or not any(integer(x) and x >= 1024 ** 2 for x in stored):
            reasons.append(node + ": insufficient observed data during the common load window")
    return {"observed_all_hosts": not reasons, "verdict": "OBSERVED_HOST_DATA_PROCESSING" if not reasons else "INCONCLUSIVE",
            "scaling_proof": "INCONCLUSIVE: counters include data distribution and background traffic",
            "input_byte_deltas": deltas, "durable_byte_deltas": durable_deltas, "reasons": reasons,
            "post_drain_storage": [x.get("storage", {}) for x in after],
            "boundary": "native per-host data processing, not complete key placement or run-specific mutation coverage"}


def evidence(d, plan, run, count, mode):
    require(d.get("tool") == "otelbench-v2" and d.get("backend") == "fdb" and d.get("revision") == plan["source_revision"]
            and type(d.get("failures")) is int and d["failures"] == 0
            and ("error" not in d or type(d["error"]) is str and d["error"] == ""), "failed application evidence")
    expected = {"run": run, "backend": "fdb", "count": count, "tenants": 1, "attributes": 8, "seed": 283,
                "match_every": 100, "batch": plan["batch"], "save_mode": "batch", "workers": plan["workers"],
                "diagnostics": True, "tenant": 0, "k": 100, "max_candidates": 2000000,
                "max_bytes": 268435456, "order": "time", **plan["storage_options"]}
    config = d.get("config", {})
    require(d.get("mode") == mode and all(type(config.get(k)) is type(v) and config.get(k) == v for k, v in expected.items()), "application config binding")
    if mode in ("ingest", "audit"):
        stats = d["stats"]
        require(type(stats.get("rows")) is int and stats["rows"] == count
                and type(stats.get("attributes")) is int and stats["attributes"] == count * 8, "complete source population")
        if mode == "audit":
            for name, keys in {"records": count, "eq_secondary": count * 8, "span_id_v2": count,
                               "span_time_v2": count, "owned_total": count * 11 + 2, "eq_primary": 0}.items():
                require(type(stats["structures"][name]["keys"]) is int and stats["structures"][name]["keys"] == keys, "exact index census")
    elif mode == "query":
        require(d.get("suite") == "all" and type(config.get("iterations")) is int and config["iterations"] == 1600
                and type(d.get("samples")) is list and len(d["samples"]) == 1600, "complete independent oracle")
        for i, sample in enumerate(d["samples"]):
            require(type(sample.get("iteration")) is int and sample["iteration"] == i and sample.get("class") == CLASSES[i % 16]
                    and ("error" not in sample or type(sample["error"]) is str and sample["error"] == "")
                    and integer(sample.get("ns")) and hexstr(sample.get("result_digest")), "oracle sample")
    else:
        require(False, "unknown evidence mode")
    return True
