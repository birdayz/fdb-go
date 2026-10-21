#!/usr/bin/env python3
"""Sequential, explicitly authorized local clientbench campaigns (stdlib only)."""
from __future__ import annotations

import argparse
import contextlib
import csv
import datetime as dt
import fcntl
import gzip
import hashlib
import json
import math
import os
from pathlib import Path
import random
import re
import signal
import statistics
import subprocess
import sys
import uuid

SCHEMA = 1
LOCK_PATH = Path(os.environ.get("FDB_PERF_HOME", Path.home() / ".local/share/fdb-perf")) / "validation/fast.lock"
TOKEN = re.compile(r"[A-Za-z0-9_-]+\Z")
SHA256 = re.compile(r"[0-9a-f]{64}\Z")
RAW_FIELDS = {"phase", "worker", "index", "latency_ns", "attempts", "error",
              "successful", "completion_offset_ns"}
POSITIVE_METRICS = ("success_p50_ns", "success_p99_ns", "error_p99_ns",
                    "successful_ops_per_second", "cores_busy",
                    "cpu_ns_per_successful_op")
RATIO_METRICS = POSITIVE_METRICS + ("terminal_errors_per_1000_completed", "retries_per_1000_completed")
ATTEMPTS_SCOPE = ("transaction callback entries only; retries=sum(max(attempts-1,0)) for completed "
                  "logical operations in the observed window; internal RPC retries and hedges unobserved")
CONTROLLED_ENV = {"GOGC": "100", "GOMEMLIMIT": "off", "GODEBUG": ""}


class Invalid(ValueError):
    pass


def require(condition, message):
    if not condition:
        raise Invalid(message)


def read_json(path):
    with Path(path).open(encoding="utf-8") as stream:
        return json.load(stream)


def save_json(path, data):
    path = Path(path)
    temporary = path.with_name(path.name + ".tmp")
    temporary.write_text(json.dumps(data, indent=2, sort_keys=True, allow_nan=False) + "\n",
                         encoding="utf-8")
    temporary.replace(path)


def utc_now():
    return dt.datetime.now(dt.timezone.utc).isoformat()


def integer(value, name, minimum=0):
    require(isinstance(value, int) and not isinstance(value, bool) and value >= minimum,
            f"{name}: expected integer >= {minimum}")
    return value


def positive(value, name):
    require(isinstance(value, (int, float)) and not isinstance(value, bool)
            and math.isfinite(value) and value > 0, f"{name}: expected finite positive number")
    return value


def cpu_set(text):
    result = set()
    for part in text.split(","):
        require(bool(re.fullmatch(r"[0-9]+(?:-[0-9]+)?", part)), "invalid client_cpus")
        ends = [int(x) for x in part.split("-")]
        lo, hi = ends[0], ends[-1]
        require(lo <= hi, "reversed CPU range")
        result.update(range(lo, hi + 1))
    require(bool(result), "empty client CPU set")
    return result


def validate_config(config):
    require(config.get("schema") == SCHEMA, "config schema must be 1")
    require(isinstance(config.get("compiler_under_test", False), bool),
            "compiler_under_test must be boolean")
    for name in ("build_manifest", "cluster_file", "fdbcli"):
        require(isinstance(config.get(name), str) and Path(config[name]).is_absolute(),
                f"{name} must be absolute")
    for name in ("arm_ids", "workloads", "workers", "orders", "server_cpus"):
        require(isinstance(config.get(name), list) and bool(config[name]), f"{name} must be nonempty")
    arms = config["arm_ids"]
    for name in arms + config["workloads"]:
        require(isinstance(name, str) and TOKEN.fullmatch(name), "unsafe arm/workload ID")
    require(len(set(arms)) == len(arms) and len(arms) >= 2, "need distinct baseline and candidate arms")
    require(config.get("baseline") in arms, "baseline absent from arm_ids")
    require(len(set(config["workloads"])) == len(config["workloads"]), "duplicate workloads")
    require(len(set(config["workers"])) == len(config["workers"]), "duplicate worker counts")
    for workers in config["workers"]:
        integer(workers, "workers", 1)
    require(len(config["orders"]) >= 2, "at least two paired blocks required")
    for order in config["orders"]:
        require(isinstance(order, list) and len(order) == len(arms) and set(order) == set(arms),
                "each block order must be an arm_ids permutation")
    for name in ("warmup_seconds", "measure_seconds", "process_timeout_seconds"):
        positive(config.get(name), name)
    require(config["process_timeout_seconds"] > config["warmup_seconds"] + config["measure_seconds"],
            "process timeout must exceed warmup + measure")
    integer(config.get("bootstrap_samples"), "bootstrap_samples", 1)
    integer(config.get("bootstrap_seed"), "bootstrap_seed")
    clients = cpu_set(config["client_cpus"])
    for cpu in config["server_cpus"]:
        integer(cpu, "server_cpus")
    require(len(set(config["server_cpus"])) == len(config["server_cpus"]), "duplicate server CPU")
    require(not clients.intersection(config["server_cpus"]), "client and server CPUs overlap")
    gate = config.get("promotion", {})
    variant = gate.get("variant", "noninferiority")
    require(variant in ("noninferiority", "significant_regression"), "unknown promotion.variant")
    required = ("throughput_lower95_min", "cpu_per_op_upper95_max") if variant == "noninferiority" else (
        "throughput_point_max", "throughput_upper95_lt", "cpu_per_op_point_min", "cpu_per_op_lower95_gt")
    for name in required:
        positive(gate.get(name), "promotion." + name)
    intended = config.get("intended_success_p50", {})
    require(isinstance(intended, dict) and set(intended).issubset(set(arms) - {config["baseline"]}),
            "invalid intended_success_p50 arms")
    for arm, scope in intended.items():
        require(isinstance(scope, dict) and bool(scope.get("workloads")) and bool(scope.get("workers")),
                f"{arm}: empty intended p50 population")
        require(set(scope["workloads"]).issubset(config["workloads"])
                and set(scope["workers"]).issubset(config["workers"]), f"{arm}: unknown intended cell")
    return config


def load_build_manifest(config):
    compiler_under_test = config.get("compiler_under_test", False)
    require(isinstance(compiler_under_test, bool), "compiler_under_test must be boolean")
    manifest = read_json(config["build_manifest"])
    require(manifest.get("schema") == SCHEMA, "build manifest schema must be 1")
    require(isinstance(manifest.get("base_commit"), str) and manifest["base_commit"], "missing base_commit")
    if config.get("expected_base_commit"):
        require(manifest["base_commit"] == config["expected_base_commit"], "base commit mismatch")
    require(isinstance(manifest.get("harness_patch_sha256"), str)
            and SHA256.fullmatch(manifest["harness_patch_sha256"]), "invalid harness patch digest")
    compiler_versions = set()
    for arm in config["arm_ids"]:
        data = manifest.get("arms", {}).get(arm, {})
        for name in ("binary", "worktree"):
            require(isinstance(data.get(name), str) and Path(data[name]).is_absolute(),
                    f"{arm}.{name} must be absolute")
        for name in ("binary_sha256", "prototype_patch_sha256"):
            require(isinstance(data.get(name), str) and SHA256.fullmatch(data[name]), f"{arm}.{name} invalid")
        for name in ("source_revision", "go_version"):
            require(isinstance(data.get(name), str) and data[name], f"{arm}.{name} missing")
        compiler_versions.add(runtime_version(data["go_version"]))
    require(compiler_under_test or len(compiler_versions) == 1,
            "selected arms must use the same Go compiler version unless compiler_under_test is true")
    return manifest


def binary_identities(config, manifest):
    identities = {}
    for arm in config["arm_ids"]:
        binary = Path(manifest["arms"][arm]["binary"])
        require(binary.is_file() and os.access(binary, os.X_OK), f"{arm}: binary missing/not executable")
        digest = hashlib.sha256()
        with binary.open("rb") as stream:
            for chunk in iter(lambda: stream.read(1024 * 1024), b""):
                digest.update(chunk)
        require(digest.hexdigest() == manifest["arms"][arm]["binary_sha256"], f"{arm}: binary digest mismatch")
        identities[arm] = {"binary": str(binary), "binary_sha256": digest.hexdigest()}
    return identities


def schedule(config):
    cells = [(workload, workers) for workload in config["workloads"] for workers in config["workers"]]
    result = []
    arm_width = max(3, len(str(len(config["arm_ids"]) - 1)))
    for block, order in enumerate(config["orders"]):
        shift = block % len(cells)
        for workload, workers in cells[shift:] + cells[:shift]:
            cell = cells.index((workload, workers))
            for arm in order:
                arm_index = config["arm_ids"].index(arm)
                result.append({"block": block, "cell": cell, "workload": workload,
                               "workers": workers, "arm": arm, "arm_index": arm_index,
                               "run_id": f"b{block:03d}_c{cell:03d}_a{arm_index:0{arm_width}d}"})
    return result


def campaign_prefix(token):
    require(isinstance(token, str) and TOKEN.fullmatch(token), "unsafe ownership token")
    return "clientbench/" + token + "/"


def prefix_end(prefix):
    require(bool(re.fullmatch(r"clientbench/[A-Za-z0-9_-]+/", prefix)), "not an owned campaign prefix")
    return prefix[:-1] + "0"


def harness_command(config, manifest, run, prefix, destination):
    arm = manifest["arms"][run["arm"]]
    command = ["taskset", "-c", config["client_cpus"], arm["binary"],
               "-cluster-file", config["cluster_file"], "-workload", run["workload"],
               "-workers", str(run["workers"]), "-rate", "0", "-allow-writes",
               "-duration", f"{config['measure_seconds']:g}s",
               "-warmup-duration", f"{config['warmup_seconds']:g}s",
               "-prefix", prefix + run["run_id"] + "/", "-output", str(destination),
               "-source-revision", arm["source_revision"],
               "-harness-id", manifest["harness_patch_sha256"]]
    environment = {key: value for key, value in os.environ.items()
                   if key not in ("GOEXPERIMENT", "GOFLAGS") and not key.startswith("FDB_NETWORK_OPTION_")}
    environment.update(CONTROLLED_ENV)
    environment["GOMAXPROCS"] = str(len(cpu_set(config["client_cpus"])))
    return command, environment


@contextlib.contextmanager
def campaign_lock(path=LOCK_PATH):
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open("a+") as stream:
        try:
            fcntl.flock(stream, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError as error:
            raise Invalid(f"timing lock busy: {path}") from error
        try:
            yield
        finally:
            fcntl.flock(stream, fcntl.LOCK_UN)


class OwnedProcesses:
    """Only signal process groups created here with start_new_session=True."""
    def __init__(self):
        self.active = {}

    def _stop(self, process):
        # The group ID comes only from our own Popen, never a process-name search.
        with contextlib.suppress(ProcessLookupError):
            os.killpg(process.pid, signal.SIGKILL)
        try:
            process.wait(timeout=3)
        except subprocess.TimeoutExpired as error:
            raise Invalid(f"owned process {process.pid} did not reap after SIGKILL") from error

    def stop_all(self):
        failures = []
        for process in list(self.active.values()):
            try:
                self._stop(process)
                self.active.pop(process.pid, None)
            except Exception as error:
                failures.append(str(error))
        require(not failures, "; ".join(failures))

    def run(self, command, timeout, log, environment=None):
        with Path(log).open("wb") as output:
            process = subprocess.Popen(command, stdout=output, stderr=subprocess.STDOUT,
                                       env=environment, start_new_session=True)
            self.active[process.pid] = process
            try:
                try:
                    code = process.wait(timeout=timeout)
                except subprocess.TimeoutExpired as error:
                    raise Invalid(f"hard timeout ({timeout}s): {command[0]}; see {log}") from error
                require(code == 0, f"exit {code}: {command[0]}; see {log}")
            finally:
                self._stop(process)
                self.active.pop(process.pid, None)
        return Path(log).read_text(encoding="utf-8", errors="replace")


def fdb_command(config, command):
    return [config["fdbcli"], "-C", config["cluster_file"],
            "--timeout", str(math.ceil(config["process_timeout_seconds"])), "--exec", command]


def range_is_empty(output):
    lines = [line.strip() for line in output.splitlines() if line.strip()]
    require(bool(lines), "getrange returned no evidence")
    # fdbcli 7.3.77 emits this header and blank lines, not a literal '0 results'.
    # The runner has already required exit status zero and captured all stdout/stderr.
    require(lines == ["Range limited to 1 keys"],
            "owned prefix is nonempty or exact successful empty-range receipt is missing")


def verify_prefix(config, runner, prefix, log):
    end = prefix_end(prefix)
    output = runner.run(fdb_command(config, f'getrange "{prefix}" "{end}" 1'),
                        config["process_timeout_seconds"], log)
    range_is_empty(output)


def cleanup_prefix(config, runner, prefix, output):
    # Validation occurs before generating either command: no global/foreign range clears.
    end = prefix_end(prefix)
    runner.run(fdb_command(config, f'writemode on; clearrange "{prefix}" "{end}"'),
               config["process_timeout_seconds"], output / "cleanup-clear.log")
    verify_prefix(config, runner, prefix, output / "cleanup-verify.log")


def argument_value(arguments, names):
    for index, value in enumerate(arguments):
        if value in names and index + 1 < len(arguments):
            return arguments[index + 1]
        for name in names:
            if value.startswith(name + "="):
                return value[len(name) + 1:]
    return None


def normalized_cluster_bytes(path):
    lines = [line.strip() for line in Path(path).read_bytes().splitlines()
             if line.strip() and not line.lstrip().startswith(b"#")]
    require(len(lines) == 1 and b"@" in lines[0], "invalid cluster-file connection string")
    return lines[0]


def initial_cluster_check(config, runner, output, proc_root=Path("/proc")):
    text = runner.run(fdb_command(config, "status json"), config["process_timeout_seconds"],
                      output / "initial-status.log")
    start = text.find("{")
    require(start >= 0, "status json missing JSON object")
    status = json.JSONDecoder().raw_decode(text[start:])[0]
    database = status.get("client", {}).get("database_status", {})
    cluster = status.get("cluster", {})
    require(database.get("available") is True and database.get("healthy") is True,
            "local cluster is not available and healthy")
    require(cluster.get("database_available") is True and not cluster.get("database_locked", False),
            "cluster unavailable or locked")
    reported = cluster.get("processes", {})
    require(isinstance(reported, dict) and bool(reported), "status has no server population")
    addresses = {process.get("address", "").removesuffix(":tls") for process in reported.values()}
    require("" not in addresses and len(addresses) == len(reported), "missing/duplicate status server addresses")
    expected = set(config["server_cpus"])
    servers = []
    cluster_path = Path(config["cluster_file"]).resolve()
    connection = normalized_cluster_bytes(cluster_path)
    for entry in Path(proc_root).iterdir():
        if not entry.name.isdigit():
            continue
        try:
            if (entry / "comm").read_text().strip() != "fdbserver":
                continue
            args = [part.decode() for part in (entry / "cmdline").read_bytes().split(b"\0") if part]
            configured = argument_value(args, ("-C", "--cluster-file", "--cluster_file"))
            if configured is None:
                continue
            server_file = Path(configured)
            if not server_file.is_absolute():
                server_file = (entry / "cwd").resolve() / server_file
            server_file = server_file.resolve()
            if server_file != cluster_path and normalized_cluster_bytes(server_file) != connection:
                continue
            address = argument_value(args, ("-p", "--public-address", "--public_address"))
            require(address is not None, f"server PID {entry.name} has no explicit public address")
            address = address.removesuffix(":tls")
            require(address in addresses, f"server PID {entry.name} address is absent from status")
            affinity = set(os.sched_getaffinity(int(entry.name)))
        except (FileNotFoundError, ProcessLookupError):
            continue
        require(bool(affinity) and affinity.issubset(expected),
                f"server PID {entry.name} affinity {sorted(affinity)} outside {sorted(expected)}")
        servers.append({"pid": int(entry.name), "affinity": sorted(affinity), "argv": args,
                        "cluster_file": str(server_file), "address": address})
    require(len(servers) == len(reported) and {server["address"] for server in servers} == addresses,
            f"cannot identify every cluster server: status={len(reported)}, local={len(servers)}")
    save_json(output / "initial-cluster.json", {"status": status, "servers": servers,
              "normalized_cluster_file_sha256": hashlib.sha256(connection).hexdigest()})


def percentile(values, fraction):
    require(bool(values), "empty quantile population")
    ordered = sorted(values)
    return ordered[max(0, math.ceil(fraction * len(ordered)) - 1)]


def close_number(actual, expected, name):
    require(isinstance(actual, (int, float)) and not isinstance(actual, bool)
            and math.isfinite(actual) and math.isclose(actual, expected, rel_tol=1e-9, abs_tol=1e-6),
            f"{name}: {actual!r} != raw-derived {expected!r}")


def sha256_file(path):
    digest = hashlib.sha256()
    with Path(path).open("rb") as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def raw_path(directory):
    paths = [path for path in (Path(directory) / "timed-samples.csv",
                               Path(directory) / "timed-samples.csv.gz") if path.is_file()]
    require(len(paths) == 1, "expected exactly one plain or gzip raw sample file")
    return paths[0]


def compress_raw(directory):
    directory = Path(directory)
    source = raw_path(directory)
    require(source.suffix == ".csv", "raw sample file already compressed")
    destination = source.with_suffix(".csv.gz")
    temporary = destination.with_name(destination.name + ".tmp")
    with source.open("rb") as incoming, temporary.open("wb") as output:
        with gzip.GzipFile(filename="", fileobj=output, mode="wb", compresslevel=1, mtime=0) as compressed:
            for chunk in iter(lambda: incoming.read(1024 * 1024), b""):
                compressed.write(chunk)
    temporary.replace(destination)
    receipt = {"schema": SCHEMA, "path": destination.name, "sha256": sha256_file(destination),
               "uncompressed_bytes": source.stat().st_size, "compressed_bytes": destination.stat().st_size,
               "compression": "gzip", "compression_level": 1}
    save_json(directory / "raw-evidence.json", receipt)
    source.unlink()  # Only after the complete gzip/footer and receipt are safely closed.
    return receipt


def runtime_version(version):
    match = re.fullmatch(r"(?:go version )?(go\S+)(?: \S+)?", version)
    require(match is not None, "unrecognized manifest Go version")
    return match.group(1)


def validate_run(directory, config, run, prefix, build_manifest):
    directory = Path(directory)
    manifest = read_json(directory / "manifest.json")
    summary = read_json(directory / "timed-summary.json")
    arm = build_manifest["arms"][run["arm"]]
    require(manifest.get("go_version") == runtime_version(arm["go_version"]), "emitted Go version mismatch")
    for source, name in ((manifest, "manifest"), (summary, "timed-summary")):
        backend = source.get("backend", {})
        require(isinstance(backend, dict) and backend.get("backend") == "go"
                and backend.get("api_version") == 730 and backend.get("native_library") is False,
                f"{name}: wrong backend/API/native-library identity")
        require(backend.get("executable_sha256") == arm["binary_sha256"], f"{name}: emitted binary digest mismatch")
        require(backend.get("executable") == arm["binary"], f"{name}: emitted executable path mismatch")
    expected = {"workload": run["workload"], "workers": run["workers"],
                "source_revision": arm["source_revision"], "harness_id": build_manifest["harness_patch_sha256"],
                "duration_ns": round(config["measure_seconds"] * 1_000_000_000),
                "warmup_duration_ns": round(config["warmup_seconds"] * 1_000_000_000),
                "prefix": prefix + run["run_id"] + "/"}
    for source, name in ((manifest, "manifest"), (summary, "timed-summary")):
        require(isinstance(source.get("config"), dict), f"{name}: missing config")
        for field, value in expected.items():
            require(source["config"].get(field) == value, f"{name}: config.{field} mismatch")
    require(summary.get("success") is True and summary.get("persisted_validation") == "ok",
            "harness infrastructure/persisted validation failed")
    require(summary.get("attempts_scope") == ATTEMPTS_SCOPE, "unrecognized transaction retry scope")
    require(summary.get("requested_duration_ns") == expected["duration_ns"], "requested duration mismatch")
    wall = integer(summary.get("measured_wall_ns"), "measured_wall_ns", 1)
    lateness = integer(summary.get("timer_lateness_ns"), "timer_lateness_ns")
    require(wall == expected["duration_ns"] + lateness, "timer-lateness/window mismatch")
    integer(summary.get("boundary_drain_wall_ns"), "boundary_drain_wall_ns")
    cpu = integer(summary.get("process_cpu_ns"), "process_cpu_ns", 1)
    successes, errors, retries, drain_count = [], [], 0, 0
    next_index = {worker: worker for worker in range(run["workers"])}
    samples = raw_path(directory)
    receipt_path = directory / "raw-evidence.json"
    require(samples.suffix != ".gz" or receipt_path.is_file(), "compressed raw-evidence receipt missing")
    if receipt_path.exists():
        receipt = read_json(receipt_path)
        require(isinstance(receipt, dict) and receipt.get("schema") == SCHEMA,
                "malformed raw-evidence receipt")
        require(receipt.get("path") == samples.name and receipt.get("sha256") == sha256_file(samples),
                "raw-evidence path/digest mismatch")
        require(receipt.get("compression") == "gzip" and receipt.get("compression_level") == 1
                and receipt.get("compressed_bytes") == samples.stat().st_size,
                "raw-evidence compression/size mismatch")
        integer(receipt.get("uncompressed_bytes"), "raw-evidence uncompressed_bytes", 1)
    opener = gzip.open if samples.suffix == ".gz" else open
    with opener(samples, "rt", newline="", encoding="utf-8") as stream:
        reader = csv.DictReader(stream)
        require(reader.fieldnames is not None and len(reader.fieldnames) == len(RAW_FIELDS)
                and set(reader.fieldnames) == RAW_FIELDS, "timed-samples.csv: unexpected columns")
        for line, row in enumerate(reader, 2):
            require(None not in row and all(value is not None for value in row.values()),
                    f"raw line {line}: malformed row")
            try:
                numbers = {name: int(row[name]) for name in
                           ("worker", "index", "latency_ns", "attempts", "completion_offset_ns")}
            except ValueError as error:
                raise Invalid(f"raw line {line}: invalid integer") from error
            for name, value in numbers.items():
                integer(value, f"raw line {line} {name}")
            require(numbers["worker"] < run["workers"], f"raw line {line}: worker out of bounds")
            worker = numbers["worker"]
            require(numbers["index"] == next_index[worker],
                    f"raw line {line}: missing/duplicate/out-of-order worker index")
            next_index[worker] += run["workers"]
            require(row["phase"] in ("measure", "drain"), f"raw line {line}: invalid phase")
            require(row["successful"] in ("true", "false"), f"raw line {line}: invalid successful flag")
            successful = row["successful"] == "true"
            if successful:
                require(row["error"] == "", f"raw line {line}: success carries an error")
                require(numbers["attempts"] >= 1, f"raw line {line}: successful operation has no callback")
            require(numbers["latency_ns"] <= numbers["completion_offset_ns"],
                    f"raw line {line}: latency predates measurement start")
            if row["phase"] == "drain":
                require(numbers["completion_offset_ns"] >= wall, f"raw line {line}: drain inside window")
                drain_count += 1
                continue
            require(numbers["completion_offset_ns"] < wall, f"raw line {line}: measurement after stop")
            (successes if successful else errors).append(numbers["latency_ns"])
            retries += max(numbers["attempts"] - 1, 0)
    require(bool(successes or errors), "no measured logical completions; invalid observation population")
    for worker in range(run["workers"]):
        require(next_index[worker] > worker, f"worker {worker}: empty raw population")
    integer(summary.get("boundary_drain_raw"), "boundary_drain_raw")
    require(summary["boundary_drain_raw"] == drain_count, "drain count/raw mismatch")
    for name, value in (("successful", len(successes)), ("terminal_errors", len(errors)),
                        ("total_completed", len(successes) + len(errors)), ("retries", retries)):
        integer(summary.get(name), name)
        require(summary[name] == value, f"{name}: summary/raw mismatch")
    if successes:
        close_number(summary.get("cpu_ns_per_successful_op"), cpu / len(successes),
                     "cpu_ns_per_successful_op")
    else:
        require(summary.get("cpu_ns_per_successful_op") is None,
                "CPU/successful op must be null with zero successes")
    for name, value in (("successful_ops_per_second", len(successes) * 1e9 / wall),
                        ("cores_busy", cpu / wall),
                        ("terminal_errors_per_1000_completed_ops", len(errors) * 1000 / (len(successes) + len(errors))),
                        ("retries_per_1000_completed_ops", retries * 1000 / (len(successes) + len(errors)))):
        close_number(summary.get(name), value, name)
    for name, values, fraction in (("success_p50_ns", successes, .50),
                                   ("success_p99_ns", successes, .99),
                                   ("error_p99_ns", errors, .99)):
        if values:
            require(summary.get(name) == percentile(values, fraction), f"{name}: raw quantile mismatch")
        else:
            require(summary.get(name) is None, f"{name}: must be null without errors")
    for name in POSITIVE_METRICS:
        value = summary[name]
        if value is not None:
            require(isinstance(value, (int, float)) and not isinstance(value, bool)
                    and math.isfinite(value) and value >= 0, f"{name}: expected finite nonnegative value")
    result = {name: summary[name] for name in POSITIVE_METRICS}
    result["terminal_errors_per_1000_completed"] = len(errors) * 1000 / summary["total_completed"]
    result["retries_per_1000_completed"] = retries * 1000 / summary["total_completed"]
    result["total_completed"] = summary["total_completed"]
    result["terminal_errors"] = len(errors)
    result["error_latencies_ns"] = errors
    result["retries"] = retries
    return result


def geometric_mean(values):
    require(bool(values) and all(isinstance(x, (int, float)) and math.isfinite(x) and x > 0 for x in values),
            "geometric mean requires nonempty positive population")
    return math.exp(statistics.fmean(math.log(value) for value in values))


def interpolated_quantile(ordered, fraction):
    position = (len(ordered) - 1) * fraction
    lo = math.floor(position)
    hi = math.ceil(position)
    return ordered[lo] + (ordered[hi] - ordered[lo]) * (position - lo)


def paired_ratio(candidate, baseline, samples, seed):
    require(len(candidate) == len(baseline) and len(candidate) >= 2, "unpaired/empty block population")
    if any(x is None or not isinstance(x, (int, float)) or not math.isfinite(x) or x <= 0
           for x in candidate + baseline):
        return None  # Zero errors/retries have no ratio; never add pseudocounts.
    ratios = [a / b for a, b in zip(candidate, baseline)]
    logs = [math.log(value) for value in ratios]
    generator = random.Random(seed)
    draws = sorted(math.exp(statistics.fmean(logs[generator.randrange(len(logs))] for _ in logs))
                   for _ in range(samples))
    return {"point": geometric_mean(ratios), "lower95": interpolated_quantile(draws, .025),
            "upper95": interpolated_quantile(draws, .975), "block_ratios": ratios}


def gate_cell(promotion, throughput, cpu):
    if throughput is None or cpu is None:
        return False  # Missing/zero-success populations cannot pass either variant.
    if promotion.get("variant", "noninferiority") == "noninferiority":
        return (throughput["lower95"] >= promotion["throughput_lower95_min"]
                and cpu["upper95"] <= promotion["cpu_per_op_upper95_max"])
    reject = ((throughput["point"] <= promotion["throughput_point_max"]
               and throughput["upper95"] < promotion["throughput_upper95_lt"])
              or (cpu["point"] >= promotion["cpu_per_op_point_min"]
                  and cpu["lower95"] > promotion["cpu_per_op_lower95_gt"]))
    return not reject


def summarize_campaign(output):
    output = Path(output)
    config = validate_config(read_json(output / "config.json"))
    build_manifest = read_json(output / "build-manifest.json")
    state = read_json(output / "campaign.json")
    require(state.get("status") == "complete" and state.get("cleanup") == "verified",
            "campaign incomplete or owned-prefix cleanup unverified")
    plan = schedule(config)
    require(read_json(output / "run-plan.json") == plan, "saved schedule differs from config")
    require(state.get("completed_runs") == len(plan), "campaign run population incomplete")
    blocks = len(config["orders"])
    raw = {}
    for run in plan:
        key = (run["workload"], run["workers"], run["arm"], run["block"])
        require(key not in raw, "duplicate run population")
        raw[key] = validate_run(output / "runs" / run["run_id"] / "harness", config, run, state["prefix"],
                                build_manifest)
    cells = []
    decisions = {arm: {"all_cell_gate_pass": True, "intended_p50_point_improved": None}
                 for arm in config["arm_ids"] if arm != config["baseline"]}
    intended_points = {arm: [] for arm in decisions}
    for workload in config["workloads"]:
        for workers in config["workers"]:
            cell = {"workload": workload, "workers": workers, "arms": {}}
            for arm in config["arm_ids"]:
                counts = {metric: sum(raw[(workload, workers, arm, block)][metric] for block in range(blocks))
                          for metric in ("terminal_errors", "retries", "total_completed")}
                pooled_errors = [latency for block in range(blocks)
                                 for latency in raw[(workload, workers, arm, block)]["error_latencies_ns"]]
                complete_error_pairs = all(raw[(workload, workers, observed_arm, block)]["terminal_errors"] >= 20
                                           for block in range(blocks) for observed_arm in (arm, config["baseline"]))
                data = {"raw_counts": counts}
                for metric in RATIO_METRICS:
                    values = [raw[(workload, workers, arm, block)][metric] for block in range(blocks)]
                    baseline = [raw[(workload, workers, config["baseline"], block)][metric]
                                for block in range(blocks)]
                    if metric in ("terminal_errors_per_1000_completed", "retries_per_1000_completed"):
                        count = counts["terminal_errors" if metric.startswith("terminal") else "retries"]
                        absolute = count * 1000 / counts["total_completed"]
                    else:
                        population = [x for x in values if x is not None] if metric == "error_p99_ns" else values
                        absolute = None
                        if population and all(x is not None and x >= 0 for x in population):
                            absolute = 0 if any(x == 0 for x in population) else geometric_mean(population)
                    ratio = paired_ratio(values, baseline, config["bootstrap_samples"], config["bootstrap_seed"])
                    if metric == "error_p99_ns":
                        absolute = percentile(pooled_errors, .99) if len(pooled_errors) >= 20 else None
                        if not complete_error_pairs:
                            ratio = None
                    data[metric] = {"absolute": absolute, "blocks": values, "ratio": ratio}
                if arm != config["baseline"]:
                    passed = gate_cell(config["promotion"], data["successful_ops_per_second"]["ratio"],
                                       data["cpu_ns_per_successful_op"]["ratio"])
                    data["gate_pass"] = passed
                    decisions[arm]["all_cell_gate_pass"] &= passed
                    scope = config.get("intended_success_p50", {}).get(arm)
                    if scope and workload in scope["workloads"] and workers in scope["workers"]:
                        ratio = data["success_p50_ns"]["ratio"]
                        intended_points[arm].append(ratio is not None and ratio["point"] < 1)
                cell["arms"][arm] = data
            cells.append(cell)
    for arm, points in intended_points.items():
        if points:
            decisions[arm]["intended_p50_point_improved"] = all(points)
    result = {"schema": SCHEMA, "cells": cells, "decisions": decisions,
              "baseline": config["baseline"], "bootstrap_samples": config["bootstrap_samples"],
              "bootstrap_seed": config["bootstrap_seed"], "blocks": blocks,
              "promotion_variant": config["promotion"].get("variant", "noninferiority"),
              "automatic_promotion": False}
    save_json(output / "summary.json", result)
    write_report(output / "report.md", config, result)
    return result


def metric_text(data, metric, baseline, scale=1, unit=""):
    entry = data[metric]
    absolute = entry["absolute"]
    text = "undefined" if absolute is None else f"{absolute / scale:.5g}{unit}"
    ratio = entry["ratio"]
    if baseline:
        return text + " (reference)"
    if ratio is None:
        return text + " (ratio undefined)"
    return text + f" ({ratio['point']:.5f}× [{ratio['lower95']:.5f}, {ratio['upper95']:.5f}])"


def write_report(path, config, result):
    lines = ["# Local benchmark", "",
             f"Paired block ratios: geometric mean across {result['blocks']} blocks; "
             f"95% whole-block percentile bootstrap, {result['bootstrap_samples']} samples, seed "
             f"{result['bootstrap_seed']}. Brackets are ratio confidence intervals, baseline is reference.", "",
             "Absolute values: geometric mean across blocks for latency, traffic and saturation. "
             "Error and retry absolute rates are pooled counts/pooled completed logical operations × 1000; "
             "raw counts are sums across blocks. Their ratios still use paired per-block rates, not ratios "
             "of pooled absolutes. Error p99 is shown only for an arm/cell with ≥20 errors; its absolute "
             "value is nearest-rank p99 of pooled raw error latencies, not a mean of block maxima. "
             "Error-p99 ratios use per-block p99 and require ≥20 errors in every paired run of each arm. "
             "Zero error/retry ratios are undefined; "
             "no pseudocounts. Retries count actual transaction-callback reentries, not RPC retries or hedges.", ""]
    for cell in result["cells"]:
        lines += [f"## {cell['workload']} · {cell['workers']} workers", "",
                  "| Arm | Latency | Traffic | Errors | Saturation |",
                  "|---|---|---|---|---|"]
        for arm in config["arm_ids"]:
            data = cell["arms"][arm]
            base = arm == config["baseline"]
            def show(metric, scale=1, unit=""):
                return metric_text(data, metric, base, scale, unit)
            latency = ("success p50 " + show("success_p50_ns", 1000, "µs")
                       + "; success p99 " + show("success_p99_ns", 1000, "µs"))
            counts = data["raw_counts"]
            if counts["terminal_errors"] >= 20:
                error_blocks = sum(value is not None for value in data["error_p99_ns"]["blocks"])
                latency += ("; error p99 " + show("error_p99_ns", 1000, "µs")
                            + f" ({counts['terminal_errors']} errors; {error_blocks}/{result['blocks']} error-bearing blocks)")
            else:
                latency += f"; error p99 n/a ({counts['terminal_errors']} errors; <20)"
            traffic = "successful ops/s " + show("successful_ops_per_second")
            errors = ("terminal errors/1k completed " + show("terminal_errors_per_1000_completed")
                      + f" (raw {counts['terminal_errors']})"
                      + "; actual retries/1k completed " + show("retries_per_1000_completed")
                      + f" (raw {counts['retries']}; {counts['total_completed']} completed logical ops)")
            saturation = ("cores busy " + show("cores_busy")
                          + "; CPU/successful op " + show("cpu_ns_per_successful_op", 1000, "µs"))
            lines.append(f"| {arm} | {latency} | {traffic} | {errors} | {saturation} |")
        lines.append("")
    lines += ["## Decision", ""]
    for arm, decision in result["decisions"].items():
        if result["promotion_variant"] == "significant_regression":
            status = "PASS" if decision["all_cell_gate_pass"] else "BLOCK"
            lines.append(f"- {arm}: **{status}** under the preregistered significant-regression rule.")
        else:
            status = "PASS" if decision["all_cell_gate_pass"] else "REJECT"
            intended = decision["intended_p50_point_improved"]
            p50 = "not preregistered" if intended is None else ("yes" if intended else "no")
            lines.append(f"- {arm}: all-cell noninferiority gate **{status}**; "
                         f"p50 point ratio < 1 in every intended cell: **{p50}**.")
    if result["promotion_variant"] == "noninferiority":
        lines += ["", "No automatic selection or promotion. RFC264 additionally requires the intended mechanism "
                  "and p50 improvement; the owner may promote at most one P0/P2t candidate. If neither qualifies, "
                  "reject RFC264 and retain the data/docs outcome. No new prototypes or diagnostic campaigns."]
    lines.append("")
    Path(path).write_text("\n".join(lines), encoding="utf-8")


def run_campaign(config_path, output, allow_run):
    require(allow_run, "timing is disabled: explicit GO and --allow-run are required")
    config = validate_config(read_json(config_path))
    manifest = load_build_manifest(config)
    output = Path(output).absolute()
    require(not output.exists(), "--output must be a fresh, nonexistent directory")
    with campaign_lock():
        output.mkdir(parents=True, exist_ok=False)
        (output / "runs").mkdir()
        prefix = campaign_prefix("local_" + uuid.uuid4().hex)
        plan = schedule(config)
        state = {"schema": SCHEMA, "status": "running", "started": utc_now(), "prefix": prefix,
                 "completed_runs": 0, "cleanup": "not_needed", "lock": str(LOCK_PATH)}
        save_json(output / "config.json", config)
        save_json(output / "build-manifest.json", manifest)
        save_json(output / "run-plan.json", plan)
        save_json(output / "campaign.json", state)
        runner = OwnedProcesses()
        may_have_writes = False
        failure = None
        old_handlers = {}
        def interrupt(signum, _frame):
            raise InterruptedError(f"received signal {signum}")
        try:
            for signum in (signal.SIGINT, signal.SIGTERM):
                old_handlers[signum] = signal.signal(signum, interrupt)
            state["controller"] = {"pid": os.getpid(), "nice": os.getpriority(os.PRIO_PROCESS, 0),
                                   "affinity": sorted(os.sched_getaffinity(0))}
            require(state["controller"]["nice"] == 0, "timing controller must run at nice 0")
            state["binary_identities_before"] = binary_identities(config, manifest)
            initial_cluster_check(config, runner, output)
            verify_prefix(config, runner, prefix, output / "initial-prefix.log")
            for run in plan:
                run_dir = output / "runs" / run["run_id"]
                run_dir.mkdir()
                command, environment = harness_command(config, manifest, run, prefix, run_dir / "harness")
                save_json(run_dir / "invocation.json", {"argv": command,
                          "controlled_environment": {key: environment[key] for key in
                                                     ("GOMAXPROCS", "GOGC", "GOMEMLIMIT", "GODEBUG")},
                          "removed_environment_keys": sorted(key for key in os.environ if
                              key in ("GOEXPERIMENT", "GOFLAGS") or key.startswith("FDB_NETWORK_OPTION_")),
                          "run": run})
                may_have_writes = True
                runner.run(command, config["process_timeout_seconds"], run_dir / "stdout.log", environment)
                validate_run(run_dir / "harness", config, run, prefix, manifest)
                compress_raw(run_dir / "harness")
                state["completed_runs"] += 1
                save_json(output / "campaign.json", state)
            state["binary_identities_after"] = binary_identities(config, manifest)
            state["status"] = "complete"
        except BaseException as error:
            failure = error
            state["status"] = "failed"
            state["failure"] = f"{type(error).__name__}: {error}"
        finally:
            # Finish bounded cleanup even if another interrupt arrives during it.
            for signum in old_handlers:
                signal.signal(signum, signal.SIG_IGN)
            cleanup_failures = []
            try:
                runner.stop_all()
            except BaseException as error:
                cleanup_failures.append(error)
            # A stop/reap error must not bypass the independent prefix-cleanup attempt.
            if may_have_writes:
                try:
                    cleanup_prefix(config, runner, prefix, output)
                    state["cleanup"] = "verified"
                except BaseException as error:
                    state["cleanup"] = "failed"
                    cleanup_failures.append(error)
            try:
                runner.stop_all()
            except BaseException as error:
                cleanup_failures.append(error)
            if cleanup_failures:
                state["cleanup_failures"] = [f"{type(error).__name__}: {error}" for error in cleanup_failures]
                state["status"] = "failed"
                if failure is None:
                    failure = cleanup_failures[0]
            state["finished"] = utc_now()
            try:
                save_json(output / "campaign.json", state)
            finally:
                for signum, handler in old_handlers.items():
                    signal.signal(signum, handler)
        if failure is not None:
            raise failure
        summarize_campaign(output)
    return output


def parser():
    result = argparse.ArgumentParser(description=__doc__)
    commands = result.add_subparsers(dest="command", required=True)
    run = commands.add_parser("run", help="requires explicit owner GO; starts timing")
    run.add_argument("--config", required=True, type=Path)
    run.add_argument("--output", required=True, type=Path)
    run.add_argument("--allow-run", action="store_true", help="explicit authorization, never implied")
    summary = commands.add_parser("summarize", help="offline: revalidate retained raw files and report")
    summary.add_argument("--campaign", required=True, type=Path)
    return result


def main(argv=None):
    args = parser().parse_args(argv)
    try:
        if args.command == "run":
            output = run_campaign(args.config, args.output, args.allow_run)
        else:
            summarize_campaign(args.campaign)
            output = args.campaign
    except (Invalid, OSError, json.JSONDecodeError, InterruptedError) as error:
        print(f"ERROR: {error}", file=sys.stderr)
        return 1
    print(output / "report.md")
    return 0


if __name__ == "__main__":
    sys.exit(main())
