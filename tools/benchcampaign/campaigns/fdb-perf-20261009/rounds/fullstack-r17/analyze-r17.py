#!/usr/bin/env python3
"""Offline-only R17 confirmatory analysis; refuses partial or invalid campaigns.

python3 -B analyze-r17.py INPUT_ROOT NEW_OUTPUT_DIR [--v7-scripts DIR]

Only read-only functions from hash-verified frozen V7 scripts are imported.
No subprocess, network, acquisition, repository build/test, or input write occurs.
All 80 segments and 640 runs must pass before any effect estimate is calculated.
"""
import argparse
import csv
import datetime as dt
import hashlib
import importlib.util
import json
import math
from pathlib import Path
import re
import statistics
import sys

sys.dont_write_bytecode = True
from r17_statistics import BLOCKS, BOOTSTRAPS, PRIMARY, circular_bootstrap, decisions

BASE = "7922a2e5a8e3a2d6cf17dfee8b8134181c88b298"
PATCH = "d0a6ae57ac1143aa1247ce2461f84f74264825f901a53cb7ab943a0efe83be85"
HARNESS = "48b9a56e70aa3d6c43c3760a3c8e43e6f3d70e6cd1805757de7f74518f177203"
WRAPPER = "8f340663085b961230046bf440e1e80df27e24976219fe3b6e3ae6a1193f1a6a"
LIBRARY = "e4ad7fed7b7580bf5f7b38f0a4279d37e698f69d534182e5de0b8beeef6cd1b1"
PREREG = "2c395126fc090504c21ae7fb4e1ca513aa1727c6082d55e488af851c9c616788"
ADDENDUM = "75bb7c31ef30e3133013eaed5a301ae8ea56a41d8924cd8dab14807583984dad"
FDB_SERVER = "179df8ec7292ad060fb7bd60a771ff4a8dd3989a8115638356c73feea4fc2c5f"
BINARIES = {"new": "93b8d79f05a5b1620c9c51a1e5dd7515943adcda269333a4b2561f5db11e7f2f",
            "cgo": "4c89840d7e8d83d30534747dc0ae6ab2a1fc68b504db5699d9aa6418397cdb1c"}
SOURCES = {"new": BASE + ".patch." + PATCH, "cgo": "8ab5c5a4d6ee4858a4eacd6ac58e2a7be85dbead"}
FROZEN_SCRIPTS = {"analyze.py": "5d3086d28e7e856f4b9f9033e6f6790e1e90031fa2efe19b122b27b731fe577b",
                  "run.py": "7ce9e96c4e736b2df559f62b00c4eacfd4ad4b1623ca380d94e07896d96669f2",
                  "r3_plan.py": "effa82052b4327d0998b4c914250f4bd1e66e22397003b818d55f4892904f9d3"}
ENV = {"GODEBUG": "", "GOGC": "100", "GOMAXPROCS": "8", "GOMEMLIMIT": "off", "LC_ALL": "C", "TZ": "UTC"}
WORKLOADS = [{"name": "read1000", "iterations": 5000, "warmup": 200, "workers": 1},
             {"name": "batchset50", "iterations": 2500, "warmup": 200, "workers": 1}]
CPU_SETS = [{"name": "housekeeping", "cpus": [0, 1], "max_busy_fraction": .3, "max_cpu_busy_fraction": .8},
            {"name": "server", "cpus": list(range(2, 8)), "max_busy_fraction": .15, "max_cpu_busy_fraction": .5},
            {"name": "client", "cpus": list(range(8, 16)), "max_busy_fraction": .05, "max_cpu_busy_fraction": .15}]
SNAPSHOT_KEYS = ("go_total_alloc_bytes", "go_mallocs", "go_gc_cycles", "go_gc_pause_ns",
                 "process_user_cpu_us", "process_system_cpu_us", "process_high_water_rss_kb")
CPU_SCOPES = {
    "window": "(C-B)/N: measured-window getrusage; deferred warmup/native cleanup may enter or leave this interval",
    "drained": "(D-B)/N: through finalizer fixpoint and native FIFO barrier; includes forced GC and pending setup/warmup cleanup",
    "shutdown": "(E-A)/N: includes client startup, fixture, warmup, validation, CSV, forced GC and shutdown; excludes identity hashing before A, final JSON and process exit; NOT isolated steady-state CPU",
    "absolute_E": "E/N: raw/absolute only, includes identity hashing; NEVER paired cross-arm client-cost evidence",
}
METRICS = ["mean_ns", "p50_ns", "p90_ns", "p95_ns", "p99_ns", "p999_ns", "max_ns",
           "process_cpu_ns_per_op", "user_cpu_ns_per_op", "system_cpu_ns_per_op",
           "drained_cpu_ns_per_op", "drained_user_cpu_ns_per_op", "drained_system_cpu_ns_per_op",
           "shutdown_process_cpu_ns_per_op", "shutdown_user_cpu_ns_per_op", "shutdown_system_cpu_ns_per_op",
           "go_alloc_bytes_per_op", "go_mallocs_per_op"]
ABSOLUTE_EXTRA = ["whole_process_cpu_through_shutdown_ns_per_op", "process_lifetime_high_water_rss_kib",
                  "closed_loop_ops_per_second", "go_gc_cycles", "drained_go_gc_cycles", "shutdown_go_gc_cycles"]
FORBIDDEN_COMMS = {"bazel", "bazelisk", "compile", "cc1", "cc1plus", "ninja", "make", "gcc", "g++",
                   "clang", "clang++", "rustc", "go", "apt", "apt-get", "dpkg", "perf", "strace"}


def require(condition, message):
    if not condition:
        raise ValueError(message)


def integer(value, label, minimum=0):
    require(type(value) is int and value >= minimum, f"{label}: expected integer >= {minimum}")
    return value


def digest(path):
    h = hashlib.sha256()
    with path.open("rb") as stream:
        for data in iter(lambda: stream.read(1024 * 1024), b""):
            h.update(data)
    return h.hexdigest()


def read_json(path):
    def unique(pairs):
        result = {}
        for key, value in pairs:
            require(key not in result, f"{path}: duplicate JSON key {key}")
            result[key] = value
        return result
    def invalid(text):
        raise ValueError(f"{path}: nonfinite JSON number {text}")
    return json.loads(path.read_text(encoding="utf-8"), object_pairs_hook=unique, parse_constant=invalid)


def write_json(path, value):
    path.write_text(json.dumps(value, indent=2, sort_keys=True, allow_nan=False) + "\n", encoding="utf-8")


def csv_write(path, rows):
    require(bool(rows), f"refusing empty CSV: {path}")
    fields = list(dict.fromkeys(key for row in rows for key in row))
    with path.open("w", newline="", encoding="utf-8") as stream:
        writer = csv.DictWriter(stream, fieldnames=fields)
        writer.writeheader()
        for row in rows:
            writer.writerow({key: json.dumps(value, sort_keys=True, separators=(",", ":"), allow_nan=False)
                             if isinstance(value, (dict, list)) else value for key, value in row.items()})


def under(root, relative):
    require(isinstance(relative, str) and relative and not Path(relative).is_absolute(), f"invalid relative path: {relative}")
    path = (root / relative).resolve(strict=True)
    require(path.is_relative_to(root), f"input path escapes root: {relative}")
    return path


def timestamp(text):
    value = dt.datetime.fromisoformat(text.replace("Z", "+00:00"))
    require(value.tzinfo is not None, f"timezone missing: {text}")
    return value


def load_v7(folder):
    for name, expected in FROZEN_SCRIPTS.items():
        require(digest(folder / name) == expected, f"frozen V7 script identity mismatch: {name}")
    modules = []
    for name, filename in (("run", "run.py"), ("r17_frozen_analyze", "analyze.py")):
        spec = importlib.util.spec_from_file_location(name, folder / filename)
        module = importlib.util.module_from_spec(spec)
        sys.modules[name] = module
        spec.loader.exec_module(module)
        modules.append(module)
    return modules


def inventory(root):
    result = {}
    for path in sorted(root.rglob("*")):
        require(not path.is_symlink(), f"symlink not accepted in raw acquisition: {path}")
        require(path.is_dir() or path.is_file(), f"special raw file: {path}")
        if path.is_file():
            result[str(path.relative_to(root))] = {"sha256": digest(path), "bytes": path.stat().st_size}
    return result


def metric_scope(metric):
    descriptive_scopes = {
        "go_gc_cycles": "C-B Go GC cycle count, not divided by N; native heap excluded",
        "drained_go_gc_cycles": "D-B Go GC cycle count, not divided by N; includes forced drain cycles",
        "shutdown_go_gc_cycles": "absolute E Go GC cycle count, not an E-A interval or CPU measure",
        "process_lifetime_high_water_rss_kib": "process-lifetime high-water RSS at C, including native memory; not interval peak",
        "closed_loop_ops_per_second": "N / measured_wall_seconds, closed-loop descriptive rate; not general capacity",
    }
    if metric in descriptive_scopes:
        return descriptive_scopes[metric]
    if metric.startswith("whole_process"):
        return CPU_SCOPES["absolute_E"]
    if metric.startswith("shutdown_"):
        return CPU_SCOPES["shutdown"]
    if metric.startswith("drained_"):
        return CPU_SCOPES["drained"]
    if metric in ("process_cpu_ns_per_op", "user_cpu_ns_per_op", "system_cpu_ns_per_op"):
        return CPU_SCOPES["window"]
    if metric in ("go_alloc_bytes_per_op", "go_mallocs_per_op"):
        return "C-B GoMemStats only; native C++ heap excluded; NOT total Go/CGo memory"
    return "run-level completion latency from measured samples; no transaction pooling" if metric.endswith("_ns") else "descriptive run statistic"


def validate_plan(plan, seg, pins):
    for key, expected in (("seed", 797001 + seg), ("blocks", 2), ("workloads", WORKLOADS),
                          ("environment", ENV), ("source_commit", BASE), ("harness_id", HARNESS),
                          ("native_library_sha256", LIBRARY), ("fdb_api_version", 730),
                          ("fdb_version", "7.3.77"), ("go_version", "go1.26.9"),
                          ("campaign_wall_cap_seconds", 180), ("timeout_seconds", 180),
                          ("cooldown_seconds", 2), ("quiescence_seconds", 1), ("telemetry_seconds", 1),
                          ("max_busy_fraction", .15), ("max_steal_fraction", .01), ("cpu_sets", CPU_SETS)):
        require(plan.get(key) == expected, f"seg{seg:03d}: non-preregistered plan.{key}")
    require(plan.get("expected_sha256") == pins, f"seg{seg:03d}: common input pins differ")
    require([arm["name"] for arm in plan["arms"]] == ["new", "cgo"], f"seg{seg:03d}: arm input order differs")
    require(set(plan.get("pinned_files", [])) | {arm["binary"] for arm in plan["arms"]} == set(pins), "incomplete pin paths")
    require(all(re.fullmatch(r"[0-9a-f]{64}", value) for value in pins.values()), "invalid input SHA256")
    for required_hash in (HARNESS, WRAPPER, PATCH, LIBRARY, FDB_SERVER, *FROZEN_SCRIPTS.values()):
        require(required_hash in pins.values(), f"required frozen source/runtime pin missing: {required_hash}")
    for arm in plan["arms"]:
        require(arm["source_revision"] == SOURCES[arm["name"]], "arm source declaration changed")
        require(pins[arm["binary"]] == BINARIES[arm["name"]], "frozen executable hash changed; freshness addendum cannot authorize this")


def validate_command(run, folder):
    command = run["command"]
    require(command[:3] == ["taskset", "--cpu-list", "8-15"], f"{folder}: client affinity command differs")
    flags = command[4:]
    required = {"--cluster-file", "--workload", "--iterations", "--warmup", "--workers", "--output", "--run-id",
                "--block", "--source-revision", "--harness-id", "--allow-writes", "--transaction-timeout-ms", "--retry-limit", "--prefix"}
    observed = [value for value in flags if value.startswith("--")]
    require(set(observed) == required and len(observed) == len(required), f"{folder}: unexpected or duplicate benchmark flags")
    require(command[command.index("--transaction-timeout-ms") + 1] == "30000"
            and command[command.index("--retry-limit") + 1] == "10", f"{folder}: retry/timeout configuration changed")
    prefix = command[command.index("--prefix") + 1]
    require(len(prefix.encode("utf-8")) == 68, f"{folder}: prefix is not 68 bytes")
    return prefix


def cpu_table(text, where):
    rows = {}
    for line in text.splitlines():
        fields = line.split()
        if fields and re.fullmatch(r"cpu\d*", fields[0]):
            require(len(fields) >= 9 and fields[0] not in rows, f"{where}: invalid /proc/stat CPU row")
            require(all(value.isdigit() for value in fields[1:9]), f"{where}: invalid CPU counter")
            rows[fields[0]] = [int(value) for value in fields[1:9]]
    require(set(rows) == {"cpu", *(f"cpu{i}" for i in range(16))}, f"{where}: expected pinned 16-CPU topology")
    return rows


def quiescence_check(folder, plan, v7):
    gate = read_json(folder / "quiescence.json")
    require(gate.get("passed") is True and gate.get("seconds") == 1 and gate.get("max_steal_fraction") == .01,
            f"{folder}: quiescence failed/schema differs")
    cpus = gate["cpus"]
    require(set(cpus) == {"cpu", *(f"cpu{i}" for i in range(16))}, f"{folder}: incomplete quiescence CPU evidence")
    for name, row in cpus.items():
        delta = row["cpu_jiffy_deltas"]
        require(len(delta) == 8 and all(type(value) is int and value >= 0 for value in delta), "invalid quiescence jiffies")
        require(v7.cpu_usage(delta) == row, f"{folder}: recorded quiescence CPU arithmetic differs: {name}")
    require(set(gate["groups"]) == {group["name"] for group in plan["cpu_sets"]}, "quiescence groups differ")
    for group in plan["cpu_sets"]:
        members = [cpus[f"cpu{cpu}"] for cpu in group["cpus"]]
        aggregate = v7.cpu_usage([sum(member["cpu_jiffy_deltas"][i] for member in members) for i in range(8)])
        maximum = max(member["busy_fraction"] for member in members)
        passed = (aggregate["busy_fraction"] <= group["max_busy_fraction"] and maximum <= group["max_cpu_busy_fraction"]
                  and max(member["steal_fraction"] for member in members) <= plan["max_steal_fraction"])
        expected = dict(aggregate, maximum_cpu_busy_fraction=maximum, thresholds=group, passed=passed)
        require(passed and gate["groups"][group["name"]] == expected, f"{folder}: recomputed quiescence failure: {group['name']}")
    require(cpus["cpu"]["busy_fraction"] <= plan["max_busy_fraction"], f"{folder}: global quiescence failure")
    return gate


def host_evidence(folder, plan, manifest, v7):
    gate = quiescence_check(folder, plan, v7)
    telemetry_path = folder / "telemetry.jsonl"
    telemetry = [json.loads(line, parse_constant=lambda text: (_ for _ in ()).throw(ValueError(text)))
                 for line in telemetry_path.read_text().splitlines()]
    require(telemetry, f"{folder}: empty telemetry")
    before = read_json(folder / "before" / "time.json")
    after = read_json(folder / "after" / "time.json")
    times = [before["monotonic_ns"], *[item["monotonic_ns"] for item in telemetry], after["monotonic_ns"]]
    require(all(type(value) is int and value > 0 for value in times) and all(a < b for a, b in zip(times, times[1:])),
            f"{folder}: telemetry monotonic sequence incomplete/reordered")
    tables = [cpu_table((folder / "before" / "proc_stat").read_text(), folder),
              *[cpu_table(item["/proc/stat"], folder) for item in telemetry],
              cpu_table((folder / "after" / "proc_stat").read_text(), folder)]
    max_steal, positive_intervals = 0., 0
    diagnostics = []
    for left, right in zip(tables, tables[1:]):
        for cpu in left:
            delta = [b - a for a, b in zip(left[cpu], right[cpu])]
            if min(delta) < 0:
                diagnostics.append({"kind": "decreasing_telemetry_cpu_counter", "cpu": cpu, "delta": delta})
                continue
            if sum(delta) == 0:
                continue
            positive_intervals += 1
            steal = v7.cpu_usage(delta)["steal_fraction"]
            max_steal = max(max_steal, steal)
    # No during-run cutoff was preregistered: telemetry is descriptive, never a new rejection rule.
    observed_client, observed_server = 0, 0
    for item in telemetry:
        require(isinstance(item["processes"], list), f"{folder}: telemetry process schema")
        for proc in item["processes"]:
            if proc["comm"] != "fdbserver" and proc["pid"] != manifest["pid"]:
                continue
            expected = "2-7" if proc["comm"] == "fdbserver" else "8-15"
            match = re.search(r"^Cpus_allowed_list:\s*(\S+)\s*$", proc.get("status", ""), re.MULTILINE)
            if match is None or match[1] != expected:
                diagnostics.append({"kind": "telemetry_affinity_observation", "pid": proc["pid"], "comm": proc["comm"],
                                    "expected": expected, "observed": None if match is None else match[1]})
            if proc["comm"] == "fdbserver":
                observed_server += 1
            else:
                observed_client += 1
    if observed_server == 0 or observed_client == 0:
        diagnostics.append({"kind": "no_sampled_affinity_observation", "server": observed_server, "client": observed_client})
    for stage in ("before", "after"):
        lines = (folder / stage / "processes.txt").read_text().splitlines()
        require(lines and lines[0].split()[:3] == ["PID", "PPID", "TID"], f"{folder}: process snapshot schema")
        for line in lines[1:]:
            fields = line.split(None, 10)
            require(len(fields) >= 10, f"{folder}: truncated process snapshot")
            comm = fields[9]
            if comm in FORBIDDEN_COMMS or comm.startswith(("candidate-", "baseline-", "clientbench")):
                diagnostics.append({"kind": "snapshot_process_observation", "stage": stage, "comm": comm, "raw_line": line})
    return {"quiescence": gate, "telemetry_samples": len(telemetry), "telemetry_max_steal_fraction": max_steal,
            "telemetry_positive_cpu_intervals": positive_intervals,
            "client_affinity_observations": observed_client, "server_affinity_observations": observed_server,
            "before_monotonic_ns": times[0], "after_monotonic_ns": times[-1],
            "telemetry_descriptive_only": True, "telemetry_diagnostics": diagnostics}


def validate_samples(folder, filename, phase, count):
    with (folder / filename).open(newline="") as stream:
        reader = csv.DictReader(stream)
        require(reader.fieldnames == ["phase", "index", "worker", "start_offset_ns", "latency_ns", "attempts", "error", "check_status"],
                f"{folder}/{filename}: unexpected V7 schema")
        rows = list(reader)
    require(len(rows) == count, f"{folder}/{filename}: missing sample rows")
    previous = -1
    for i, row in enumerate(rows):
        require(None not in row and None not in row.values(), f"{folder}/{filename}: nonrectangular CSV")
        require(row["phase"] == phase and row["index"] == str(i) and row["worker"] == "0" and row["attempts"] == "1"
                and row["error"] == "" and row["check_status"] == "ok", f"{folder}/{filename}: failed/nonzero-retry/unordered sample {i}")
        require(row["start_offset_ns"].isdigit() and int(row["start_offset_ns"]) >= previous
                and row["latency_ns"].isdigit() and int(row["latency_ns"]) > 0, f"{folder}/{filename}: invalid latency/offset")
        previous = int(row["start_offset_ns"])
    return rows


def validate_acquisition(root, out, args, v7, analyzer, input_inventory):
    require(not (root / "ABORTED.json").exists(), "outer acquisition aborted")
    execution = read_json(root / "execution-manifest.json")
    complete = read_json(root / "complete.json")
    require(execution.get("schema") == complete.get("schema") == "r17-acquisition-v1", "outer schema mismatch")
    require(complete.get("completed_segments") == complete.get("scheduled_segments") == 80
            and complete.get("completed_runs") == 640 and execution.get("blocks_per_workload") == 160, "partial R17 campaign")
    require(complete.get("execution_manifest_sha256") == digest(root / "execution-manifest.json"), "execution-manifest seal mismatch")
    prereg = Path(args.preregistration).resolve() if args.preregistration else root / "R17-preregistration-v1.md"
    addendum = Path(args.addendum).resolve() if args.addendum else root / "R17-preregistration-addendum-v2.md"
    require(digest(prereg) == execution.get("preregistration_sha256") == PREREG, "frozen preregistration hash mismatch")
    require(digest(addendum) == execution.get("addendum_sha256") == ADDENDUM,
            "frozen owner freshness addendum missing/hash mismatch")
    segments = execution.get("segments")
    require(isinstance(segments, list) and len(segments) == 80, "exactly 80 preregistered segments required")
    require({path.name for path in root.glob("seg*") if path.is_dir()} == {f"seg{i:03d}" for i in range(80)}, "missing/extra segment path")
    pins = execution["input_sha256"]
    require(isinstance(pins, dict) and pins, "execution input pins missing")
    for path in (Path(__file__), Path(__file__).with_name("r17_statistics.py")):
        require(digest(path) in pins.values(), f"analysis implementation not in pre-acquisition input pins: {path.name}")
    validated, prefixes, previous_end, previous_mono = [], set(), None, None
    segment_records = []
    # Completeness is checked for all 80 segments before inspecting any outcomes.
    for segment in range(80):
        folder = root / f"seg{segment:03d}"
        c = read_json(folder / "complete.json")
        require(not (folder / "ABORTED.json").exists() and c.get("scheduled") == c.get("completed") == 8,
                f"{folder}: aborted/incomplete segment")
        schedule = read_json(folder / "schedule.json")
        require(len(schedule) == 8, f"{folder}: wrong scheduled count")
        require({path.name for path in folder.glob("b*") if path.is_dir()} == {run["run_id"] for run in schedule},
                f"{folder}: missing/extra run directory")
        for run in schedule:
            require((folder / run["run_id"] / "result.json").is_file(), f"{folder}: missing result")
    for segment, entry in enumerate(segments):
        name = f"seg{segment:03d}"
        folder = root / name
        require(entry["index"] == segment and entry["name"] == name and entry["global_block_offset"] == 2 * segment,
                f"{name}: segment/global-block mapping differs")
        require(entry["plan"] == f"plans/{name}-plan.json" and entry["schedule"] == f"plans/{name}-schedule.json", "prepin paths differ")
        plan_path, schedule_path = under(root, entry["plan"]), under(root, entry["schedule"])
        require(digest(plan_path) == entry["plan_sha256"] and digest(schedule_path) == entry["schedule_sha256"], f"{name}: prepin hash mismatch")
        require(digest(folder / "plan.json") == entry["plan_sha256"] and digest(folder / "schedule.json") == entry["schedule_sha256"],
                f"{name}: actual plan/schedule differs from prepin bytes")
        plan, schedule = read_json(plan_path), read_json(schedule_path)
        validate_plan(plan, segment, pins)
        expected = v7.make_schedule(plan, folder.resolve())
        require(analyzer.normalized_schedule(schedule) == analyzer.normalized_schedule(expected), f"{name}: frozen scheduler mismatch")
        require(read_json(folder / "binaries.json") == read_json(folder / "complete.json")["binaries"] == pins, f"{name}: binary pin drift")
        controller = read_json(folder / "controller.json")
        require(controller.get("affinity") == [0, 1] and controller.get("runner_sha256") == FROZEN_SCRIPTS["run.py"]
                and controller.get("plan_sha256") == entry["plan_sha256"], f"{name}: runner/controller identity mismatch")
        environment = read_json(folder / "benchmark-environment.json")
        require(set(environment) == set(ENV) | {"HOME", "PATH"} and all(environment[key] == value for key, value in ENV.items()),
                f"{name}: unexpected environment/override")
        require(not any(key.startswith("FDB_NETWORK_OPTION_") for key in environment), f"{name}: FDB override")
        seen, progress = set(), read_json(folder / "progress.json")
        require(len(progress) == 8, f"{name}: incomplete progress records")
        for position, run in enumerate(schedule):
            run_id = run["run_id"]
            require(re.fullmatch(r"b00[01]-(read1000|batchset50)-p[01]-(new|cgo)", run_id) is not None, "unsafe/unexpected run ID")
            raw = folder / run_id
            key = (run["workload"]["name"], run["block"], run["arm"])
            require(key not in seen, f"{name}: duplicate paired run")
            seen.add(key)
            require(read_json(raw / "invocation.json") == run, f"{raw}: invocation differs from schedule")
            prefix = validate_command(run, raw)
            require(prefix not in prefixes, f"{raw}: reused prefix")
            prefixes.add(prefix)
            # Frozen V7 validation includes exact integer A..E counters and fixpoint lifecycle checks.
            v7.validate_raw_output(raw, run["workload"])
            v7.validate_identity(raw, run, plan, pins)
            summary, manifest, result = [read_json(raw / filename) for filename in ("summary.json", "manifest.json", "result.json")]
            require(result == progress[position] and result["run_id"] == run_id and type(result["returncode"]) is int
                    and result["returncode"] == 0, f"{raw}: result/progress failure")
            integer(result["process_wall_ns"], f"{raw}: process_wall_ns", 1)
            require(result["process_wall_ns"] <= 180_000_000_000, f"{raw}: process timeout")
            start, end = timestamp(result["started_utc"]), timestamp(result["ended_utc"])
            require(start < end and (previous_end is None or previous_end <= start), f"{raw}: nonserial/reversed process timestamps")
            previous_end = end
            integer(manifest["pid"], f"{raw}: PID", 1)
            require(manifest["environment"] == {key: ENV[key] for key in ("GODEBUG", "GOGC", "GOMAXPROCS", "GOMEMLIMIT")},
                    f"{raw}: actual runtime environment differs")
            for field in ("errors", "warmup_errors", "retries", "warmup_retries"):
                require(integer(summary.get(field), f"{raw}: {field}") == 0, f"{raw}: nonzero callback retry/error")
            require(summary.get("failure") == "" and summary.get("warmup_requested") == 200, f"{raw}: failure/warmup mismatch")
            validate_samples(raw, "samples.csv", "measure", run["workload"]["iterations"])
            validate_samples(raw, "warmup.csv", "warmup", 200)
            if run["arm"] == "new":
                counters = summary["client_counters_delta"]["values"]
                require(all(integer(counters.get(k), f"{raw}: {k}") == 0 for k in ("transaction_retries", "connection_failures", "grv_cache_hits")),
                        f"{raw}: NEW counter validity failure")
                n = run["workload"]["iterations"]
                require(counters["read_versions_completed"] == counters["grv_successful_batch_replies"] == n, f"{raw}: expected exact GRV count")
                require(counters["commit_started"] == counters["commit_completed"] == (0 if run["workload"]["name"] == "read1000" else n),
                        f"{raw}: expected exact commit count")
            host = host_evidence(raw, plan, manifest, v7)
            require(previous_mono is None or previous_mono < host["before_monotonic_ns"], f"{raw}: inter-run monotonic overlap/reboot")
            previous_mono = host["after_monotonic_ns"]
            validated.append({"segment": segment, "segment_name": name, "global_block": 2 * segment + run["block"],
                              "segment_schedule_index": position, "global_process_index": 8 * segment + position,
                              "folder": raw, "run": run, "summary": summary, "manifest": manifest, "result": result, "host": host})
        require(seen == {(w["name"], b, arm) for w in WORKLOADS for b in (0, 1) for arm in ("new", "cgo")}, f"{name}: incomplete paired segment")
        for w in WORKLOADS:
            require(sorted(run["arm"] for run in schedule if run["workload"]["name"] == w["name"] and run["position"] == 0) == ["cgo", "new"],
                    f"{name}: arm order not exactly balanced")
        completion = read_json(folder / "complete.json")
        require(timestamp(controller["utc"]) <= timestamp(validated[-8]["result"]["started_utc"])
                and timestamp(completion["utc"]) >= previous_end, f"{name}: controller/completion time bounds")
        require((timestamp(completion["utc"]) - timestamp(controller["utc"])).total_seconds() <= 180, f"{name}: campaign cap exceeded")
        segment_records.append({"segment": segment, "name": name, "global_block_offset": segment * 2,
                                "plan_sha256": entry["plan_sha256"], "schedule_sha256": entry["schedule_sha256"],
                                "controller_utc": controller["utc"], "completed_utc": completion["utc"], "completed_runs": 8})
    require(len(validated) == 640 and len(prefixes) == 640, "R17 must have exactly 640 valid unique runs")
    require(timestamp(complete["finished_utc"]) >= previous_end, "outer completion precedes final run")
    observed_paths = {str(item["folder"].relative_to(root) / "summary.json") for item in validated}
    require({name for name in input_inventory if name.endswith("/summary.json")} == observed_paths, "extra/missing raw run summaries")
    require(not any(Path(name).name == "ABORTED.json" for name in input_inventory), "abort evidence exists in acquisition")
    return validated, segment_records, {"execution_manifest": execution, "complete": complete,
            "preregistration_path": str(prereg), "preregistration_sha256": digest(prereg),
            "addendum_path": str(addendum), "addendum_sha256": digest(addendum)}


def describe_run(item, root, hashes, analyzer):
    run, summary, manifest = item["run"], item["summary"], item["manifest"]
    raw = item["folder"]
    with (raw / "samples.csv").open(newline="") as stream:
        latencies = [int(row["latency_ns"]) for row in csv.DictReader(stream)]
    n = len(latencies)
    A, B, C, D, E = (summary["stats_after_identity"], summary["stats_before"], summary["stats_after"],
                      summary["lifecycle"]["stats_after_measurement_drain"], summary["lifecycle"]["stats_after_shutdown"])
    relative = str(raw.relative_to(root))
    row = {"run_id": relative, "local_run_id": run["run_id"], "segment": item["segment"],
           "block": item["global_block"], "local_block": run["block"], "position": run["position"],
           "segment_schedule_index": item["segment_schedule_index"], "global_process_index": item["global_process_index"],
           "workload": run["workload"]["name"], "arm": run["arm"], "workers": 1, "observations": n,
           "warmup_observations": 200, "errors": 0, "retries": 0, "warmup_errors": 0, "warmup_retries": 0,
           "mean_ns": statistics.mean(latencies),
           **{key: analyzer.quantile(latencies, probability) for key, probability in (("p50_ns", .5), ("p90_ns", .9),
               ("p95_ns", .95), ("p99_ns", .99), ("p999_ns", .999))}, "max_ns": max(latencies),
           "measured_wall_ns": summary["measured_wall_ns"], "closed_loop_ops_per_second": n * 1e9 / summary["measured_wall_ns"],
           "go_alloc_bytes_per_op": (C["go_total_alloc_bytes"] - B["go_total_alloc_bytes"]) / n,
           "go_mallocs_per_op": (C["go_mallocs"] - B["go_mallocs"]) / n,
           "go_gc_cycles": C["go_gc_cycles"] - B["go_gc_cycles"], "drained_go_gc_cycles": D["go_gc_cycles"] - B["go_gc_cycles"],
           "shutdown_go_gc_cycles": E["go_gc_cycles"], "shutdown_interval_go_gc_cycles": E["go_gc_cycles"] - A["go_gc_cycles"],
           "go_gc_pause_ns": C["go_gc_pause_ns"] - B["go_gc_pause_ns"],
           "process_lifetime_high_water_rss_kib": C["process_high_water_rss_kb"],
           "whole_process_cpu_through_shutdown_ns_per_op": (E["process_user_cpu_us"] + E["process_system_cpu_us"]) * 1000 / n,
           "started_utc": item["result"]["started_utc"], "ended_utc": item["result"]["ended_utc"],
           "process_wall_ns": item["result"]["process_wall_ns"], "prefix": summary["config"]["prefix"],
           "source_revision": summary["config"]["source_revision"], "harness_id": summary["config"]["harness_id"],
           "executable_sha256": manifest["backend"]["executable_sha256"], "loaded_fdb_libraries_sha256": manifest["backend"]["loaded_fdb_libraries_sha256"],
           "persisted_validation": summary["persisted_validation"], "lifecycle_complete": summary["lifecycle"]["complete"],
           "database_closed": summary["lifecycle"]["database_closed"], "network_stopped": summary["lifecycle"]["network_stopped"],
           "client_counters_delta": summary["client_counters_delta"], "command": run["command"], "config": summary["config"],
           "quiescence": item["host"]["quiescence"],
           **{key: value for key, value in item["host"].items() if key != "quiescence"},
           "raw_files": {name[len(relative) + 1:]: record for name, record in hashes.items() if name.startswith(relative + "/")}}
    for label, start, end, total_key, user_key, system_key in (
            ("window", B, C, "process_cpu_ns_per_op", "user_cpu_ns_per_op", "system_cpu_ns_per_op"),
            ("drained", B, D, "drained_cpu_ns_per_op", "drained_user_cpu_ns_per_op", "drained_system_cpu_ns_per_op"),
            ("shutdown", A, E, "shutdown_process_cpu_ns_per_op", "shutdown_user_cpu_ns_per_op", "shutdown_system_cpu_ns_per_op")):
        user = (end["process_user_cpu_us"] - start["process_user_cpu_us"]) * 1000
        system = (end["process_system_cpu_us"] - start["process_system_cpu_us"]) * 1000
        require(user >= 0 and system >= 0 and user + system > 0, f"{relative}: invalid {label} CPU")
        row.update({total_key: (user + system) / n, user_key: user / n, system_key: system / n,
                    label + "_user_cpu_ns": user, label + "_system_cpu_ns": system, label + "_process_cpu_ns": user + system,
                    label + "_cpu_scope": CPU_SCOPES[label]})
    for label, snapshot in zip("ABCDE", (A, B, C, D, E)):
        row.update({label + "_" + key: snapshot[key] for key in SNAPSHOT_KEYS})
    for label in ("measurement_drain", "shutdown_drain"):
        drain = summary["lifecycle"][label]
        row[label] = drain
        row[label + "_rounds"] = drain["rounds"]
        row[label + "_wall_ns"] = drain["wall_ns"]
    return row


def paired_tables(rows):
    by = {(row["workload"], row["block"], row["arm"]): row for row in rows}
    require(len(by) == 640, "duplicate global pairing key")
    per_block, secondary, absolute = [], [], []
    for workload in ("read1000", "batchset50"):
        require(sum(by[workload, block, "new"]["position"] == 0 for block in range(BLOCKS)) == 80, "global arm order imbalance")
        for block in range(BLOCKS):
            new, cgo = by[workload, block, "new"], by[workload, block, "cgo"]
            row = {"workload": workload, "global_block": block, "segment": block // 2, "local_block": block % 2,
                   "new_run_id": new["run_id"], "cgo_run_id": cgo["run_id"], "new_position": new["position"], "cgo_position": cgo["position"],
                   "first_arm": "new" if new["position"] == 0 else "cgo"}
            require(abs(new["global_process_index"] - cgo["global_process_index"]) == 1, "paired arms not adjacent")
            for metric in METRICS:
                n, c = new[metric], cgo[metric]
                require(math.isfinite(n) and math.isfinite(c) and n >= 0 and c >= 0, "nonfinite/negative secondary metric")
                row.update({"new_" + metric: n, "cgo_" + metric: c, "ratio_" + metric: n / c if n > 0 and c > 0 else None})
            per_block.append(row)
        for metric in METRICS:
            pairs = [(by[workload, block, "new"][metric], by[workload, block, "cgo"][metric]) for block in range(BLOCKS)]
            positive = all(n > 0 and c > 0 for n, c in pairs)
            secondary.append({"workload": workload, "metric": metric, "paired_blocks": BLOCKS,
                              "new_cgo_geometric_mean": math.exp(math.fsum(math.log(n / c) for n, c in pairs) / BLOCKS) if positive else None,
                              "pairs_below_one": sum(n < c for n, c in pairs),
                              "status": "descriptive only; no confirmatory CI/claim" if positive else "undefined log ratio: at least one zero; NO pair excluded",
                              "scope": metric_scope(metric)})
        for arm in ("new", "cgo"):
            records = [by[workload, block, arm] for block in range(BLOCKS)]
            for metric in METRICS + ABSOLUTE_EXTRA:
                absolute.append({"workload": workload, "arm": arm, "metric": metric, "runs": BLOCKS,
                                 "transactions": sum(row["observations"] for row in records),
                                 "median_of_run_statistics": statistics.median(row[metric] for row in records),
                                 "minimum_run_statistic": min(row[metric] for row in records),
                                 "maximum_run_statistic": max(row[metric] for row in records),
                                 "scope": metric_scope(metric), "paired_comparison_permitted": metric != "whole_process_cpu_through_shutdown_ns_per_op"})
    logs, primary_pairs = [], []
    for block in range(BLOCKS):
        values = []
        pair = {"global_block": block, "segment": block // 2, "local_block": block % 2}
        for column, (workload, metric) in enumerate(PRIMARY):
            n, c = by[workload, block, "new"][metric], by[workload, block, "cgo"][metric]
            require(n > 0 and c > 0 and math.isfinite(n) and math.isfinite(c), "primary metric must be positive finite")
            ratio = n / c
            values.append(math.log(ratio))
            pair.update({f"endpoint_{column + 1}_new": n, f"endpoint_{column + 1}_cgo": c,
                         f"endpoint_{column + 1}_ratio": ratio, f"endpoint_{column + 1}_log_ratio": values[-1]})
        logs.append(values)
        primary_pairs.append(pair)
    return logs, primary_pairs, per_block, secondary, absolute


def markdown(primary, absolute, secondary, provenance):
    text = ["# R17 confirmatory results\n", "640 serial processes; 160 paired blocks per workload; no R16 observations pooled. "
            "Owner-authorized fresh-image rebuild of the same instance, not a new independent VM allocation; see the pinned v2 addendum.\n",
            "Paired effect = exp(mean of 160 within-block NEW/CGo log ratios), NOT a ratio of displayed medians. "
            "All five endpoint vectors are jointly resampled using the same global-block indices. "
            "Two-sided 99% percentile intervals, 100,000 resamples each: circular MBB L8/seed797707 and L16/seed797708. "
            "Nearest-rank zero-based indices 499 and 99499. Both upper bounds must be strictly below 1.\n",
            "|Primary endpoint|Workload|Metric|GM NEW/CGo|L8 99% CI|L16 99% CI|Decision|\n|---|---|---|---:|---|---|---|\n"]
    for row in primary:
        text.append(f"|{row['endpoint']}|{row['workload']}|{row['metric']}|{row['geometric_mean_ratio']:.6f}|"
                    f"[{row['L8_ci99_low']:.6f}, {row['L8_ci99_high']:.6f}]|[{row['L16_ci99_low']:.6f}, {row['L16_ci99_high']:.6f}]|{row['decision']}|\n")
    passed = [str(row["endpoint"]) for row in primary if row["improvement_confirmed"]]
    failed = [str(row["endpoint"]) for row in primary if not row["improvement_confirmed"]]
    text.append(f"\nConfirmed endpoints: {', '.join(passed) or 'none'}. Not confirmed/inconclusive: {', '.join(failed) or 'none'}. "
                "No equivalence/noninferiority conclusion follows from an inconclusive endpoint.\n")
    text.append("\n## CPU and allocation scopes\n\n" + "\n".join(f"- **{name}:** {scope}." for name, scope in CPU_SCOPES.items()) + "\n")
    text.append("\nGo allocation/GC counters exclude the native C++ heap. GC-drain costs may differ by arm. "
                "Absolute E/N appears only in raw/absolute tables and is never paired. "
                "All nonprimary metrics are secondary/descriptive; they cannot substitute for a failed primary endpoint.\n")
    text.append("\n## Absolute run medians\n\nCPU/latency values in ns/transaction; other metric units follow names. "
                "These are medians of 160 process statistics, not pooled transaction estimates.\n\n"
                "|Workload|Arm|Metric|Median|\n|---|---|---|---:|\n")
    for row in absolute:
        text.append(f"|{row['workload']}|{row['arm']}|{row['metric']}|{row['median_of_run_statistics']:.6f}|\n")
    text.append("\n## Descriptive paired GMs — no secondary inferential claims\n\n|Workload|Metric|GM NEW/CGo|Status|\n|---|---|---:|---|\n")
    for row in secondary:
        value = row["new_cgo_geometric_mean"]
        text.append(f"|{row['workload']}|{row['metric']}|{value if value is None else format(value, '.6f')}|{row['status']}|\n")
    text.append("\n## Provenance and scope\n\nAll 80 predeclared schedules, identities, exact raw counts, zero retries/errors, lifecycle "
                "fixpoints and frozen V7 pre-run quiescence gates passed. During-run telemetry is descriptive only, "
                "not a new reject/exclusion rule. See validation.json, input-files.csv, "
                "per-run.csv, per-block.csv, primary-per-block.csv, and provenance.json. No outcome-based exclusions, "
                "optional stopping, favorable replacement, or R16 pooling.\n\n"
                "Coverage is conditional on block-resampling assumptions on this one rebuilt instance; not independent "
                "VM/fleet replication, universal Go-vs-C++ superiority, general capacity, or a causal decomposition of PRs. "
                "Benchmark success does not establish source correctness, review/CI approval, or permission to merge.\n")
    return "\n".join(text)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("input_root")
    parser.add_argument("new_output_dir")
    parser.add_argument("--v7-scripts", default=str(Path(__file__).with_name("frozen-v7-scripts")))
    parser.add_argument("--preregistration")
    parser.add_argument("--addendum")
    parser.add_argument("--raw-archive", help="optional already-sealed raw archive; never extracted or modified")
    parser.add_argument("--raw-archive-sha256", help="required expected digest when --raw-archive is supplied")
    args = parser.parse_args()
    out = None
    try:
        root = Path(args.input_root).resolve(strict=True)
        target = Path(args.new_output_dir).absolute()
        require(root.is_dir() and not target.exists() and not target.is_symlink(), "input must exist; output must be new")
        require(not target.resolve().is_relative_to(root), "output may not be inside raw input root")
        # Do not create an analysis directory or load samples while acquisition is incomplete.
        complete = read_json(root / "complete.json")
        require(complete.get("completed_segments") == complete.get("scheduled_segments") == 80 and complete.get("completed_runs") == 640,
                "refusing analysis before all 80 segments/640 runs are complete")
        target.mkdir(parents=True, exist_ok=False)
        out = target
        write_json(out / "status.json", {"status": "validating", "input_root": str(root)})
        v7, analyzer = load_v7(Path(args.v7_scripts).resolve(strict=True))
        hashes = inventory(root)
        validated, segments, registration = validate_acquisition(root, out, args, v7, analyzer, hashes)
        archive = None
        require(bool(args.raw_archive) == bool(args.raw_archive_sha256), "supply both raw archive path and expected digest, or neither")
        if args.raw_archive:
            path = Path(args.raw_archive).resolve(strict=True)
            require(digest(path) == args.raw_archive_sha256, "raw archive SHA256 differs from supplied expected digest")
            archive = {"path": str(path), "sha256": args.raw_archive_sha256, "bytes": path.stat().st_size,
                       "verification_scope": "archive bytes hashed; raw-file binding is separately recorded by input-files.csv"}
        # Statistical analysis begins only after ALL validity gates for ALL 640 runs pass.
        rows = [describe_run(item, root, hashes, analyzer) for item in validated]
        logs, primary_pairs, block_rows, secondary, absolute = paired_tables(rows)
        bootstraps = [circular_bootstrap(logs, length, seed) for length, seed in BOOTSTRAPS]
        primary = decisions(logs, bootstraps, validity=True)
        require(inventory(root) == hashes, "raw acquisition changed while analysis was running")
        for path in (Path(__file__), Path(__file__).with_name("r17_statistics.py")):
            require(digest(path) in registration["execution_manifest"]["input_sha256"].values(), "analysis implementation changed during execution")
        provenance = {"input_root": str(root), "raw_archive": archive, "raw_file_count": len(hashes),
                      "execution_manifest_sha256": digest(root / "execution-manifest.json"),
                      "frozen_v7_script_sha256": FROZEN_SCRIPTS, "preregistration": registration,
                      "analyzer_sha256": digest(Path(__file__)), "statistics_module_sha256": digest(Path(__file__).with_name("r17_statistics.py")),
                      "python_version": sys.version, "raw_input_modified": False, "R16_observations_used": 0,
                      "archive_note": "If run before sealing, archive hash is null; bind this analysis and input inventory when the owner seals/downloads the archive."}
        validation = {"all_data_and_host_gates_passed": True, "segments": segments, "valid_runs": len(rows),
                      "paired_blocks_per_workload": 160, "arms_first_per_workload": {"new": 80, "cgo": 80},
                      "excluded_runs": [], "replaced_runs": [], "callback_retries": 0, "callback_errors": 0,
                      "validation_functions": ["frozen run.validate_raw_output", "frozen run.validate_identity", "frozen run.validate_lifecycle",
                                               "frozen run.make_schedule", "frozen analyze.normalized_schedule", "frozen pre-run quiescence recomputation"],
                      "during_run_telemetry_rejection_thresholds": "none; retained and reported descriptively, no added exclusion rule",
                      "host_observation_limit": "Recorded snapshots/telemetry are discrete evidence; they cannot prove absence of every transient concurrent process."}
        report = {"schema": "r17-confirmatory-analysis-v1", "validity": validation, "primary_endpoints": primary,
                  "bootstrap": bootstraps, "cpu_scopes": CPU_SCOPES, "per_run": rows, "per_block": block_rows,
                  "primary_per_block": primary_pairs, "secondary_descriptive": secondary, "absolute": absolute,
                  "provenance": provenance}
        write_json(out / "analysis.json", report)
        write_json(out / "validation.json", validation)
        write_json(out / "provenance.json", provenance)
        write_json(out / "bootstrap.json", bootstraps)
        for name, data in (("per-run.csv", rows), ("per-block.csv", block_rows), ("primary-per-block.csv", primary_pairs),
                           ("primary-decisions.csv", primary), ("secondary-descriptive.csv", secondary), ("absolute.csv", absolute),
                           ("segments.csv", segments), ("input-files.csv", [{"path": path, **record} for path, record in hashes.items()])):
            csv_write(out / name, data)
        (out / "TABLES.md").write_text(markdown(primary, absolute, secondary, provenance), encoding="utf-8")
        write_json(out / "status.json", {"status": "complete", "valid_runs": 640, "paired_blocks_per_workload": 160,
                   "primary_improvements_confirmed": [row["endpoint"] for row in primary if row["improvement_confirmed"]],
                   "primary_not_confirmed": [row["endpoint"] for row in primary if not row["improvement_confirmed"]]})
        print(f"R17 complete: 640 valid runs, 160 paired blocks/workload; outputs: {out}")
        return 0
    except (ValueError, OSError, KeyError, TypeError, OverflowError, csv.Error) as error:
        if out is not None:
            write_json(out / "status.json", {"status": "failed", "error": str(error), "error_type": type(error).__name__,
                       "confirmatory_claim_permitted": False, "note": "No exclusions/reruns performed. Inputs untouched. Preserve and disclose the failed attempt."})
        print(f"R17 FAILED: {type(error).__name__}: {error}", file=sys.stderr)
        return 2


if __name__ == "__main__":
    sys.exit(main())
