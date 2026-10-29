#!/usr/bin/env python3
"""One native FDB host and one producer: attribution only, never a capacity result."""
import argparse
import base64
import hashlib
import json
import os
from pathlib import Path
import re
import stat
import subprocess
import time

import driver as d
import policy as p

HERE = Path(__file__).resolve().parent
MIB = 1024 ** 2
TRACE_BUDGET = 16 * MIB
FRAME_BUDGET = 16 * MIB
ROLES = {"node1", "client1"}
DISK_FIELDS = ("read_ios", "read_merges", "read_sectors", "read_ms", "write_ios", "write_merges",
               "write_sectors", "write_ms", "in_flight", "io_ms", "weighted_io_ms", "discard_ios",
               "discard_merges", "discard_sectors", "discard_ms", "flush_ios", "flush_ms")


def validate_plan(plan, *, check_assets=True):
    p.require(type(plan) is dict and plan.get("schema") == "otel_native_diagnostic_v1"
              and plan.get("approved") is True, "explicit native diagnostic approval required")
    p.require(type(plan.get("count")) is int and plan["count"] == 100000
              and type(plan.get("probe_count")) is int and plan["probe_count"] == 1000000
              and type(plan.get("probe_seconds")) is int and plan["probe_seconds"] == 120,
              "fixed 100k gate, 1M diagnostic probe and 120-second probe cap required")
    p.require(type(plan.get("producers", 1)) is int and plan.get("producers", 1) == 1, "exactly one diagnostic producer")
    p.require(not set(plan) & {"topologies", "selected_topologies", "prerequisite_topologies",
                              "measured_topologies", "measured_count"}, "diagnostic is not a comparative topology study")
    # Reuse the common hardware, source, storage, asset and absolute-time guards.
    # This validation-only projection is never provisioned, written or executed.
    common = {**plan, "schema": "otel_horizontal_v1", "topologies": [1, 2, 3],
              "measured_topologies": [3, 2, 1], "measured_count": 100000}
    p.validate_plan(common, check_assets=check_assets)
    return plan


def owned_path(campaign, path):
    p.require(type(campaign) is str and re.fullmatch(r"[a-z][a-z0-9-]{7,47}", campaign), "diagnostic campaign")
    p.require(type(path) is str, "diagnostic path required")
    value = Path(path)
    base = Path("/srv/otel-horizontal") / campaign
    p.require(value.is_absolute() and value.is_relative_to(base) and value != base
              and ".." not in value.parts, "path is not in this owned diagnostic campaign")
    return value


def no_symlinks(path, base):
    path, base = Path(path), Path(base)
    p.require(path.is_relative_to(base), "diagnostic path escaped root")
    for entry in (base, *reversed(path.relative_to(base).parents), path.relative_to(base)):
        candidate = entry if entry == base else base / entry
        p.require(not candidate.is_symlink(), "diagnostic symlink refused")
    p.require(path.resolve().is_relative_to(base.resolve()), "diagnostic resolved path escaped root")
    return path


def sampler_grant(value):
    p.require(type(value) is dict and value.get("schema") == "otel_native_sampler_v1"
              and value.get("role") in ROLES and type(value.get("node_id")) is int and value["node_id"] > 0,
              "typed diagnostic sampler identity required")
    for key in ("token", "producer_grant_sha256", "fdbserver_sha256"):
        p.require(p.hexstr(value.get(key)), "sampler seal: " + key)
    p.require(type(value.get("first_epoch")) is int and type(value.get("deadline_epoch")) is int
              and value["deadline_epoch"] == value["first_epoch"] + 7200, "sampler absolute lifetime")
    for key in ("work", "arm_root", "binary"):
        owned_path(value.get("campaign"), value.get(key))
    p.require(value.get("root") == "/srv/otel-horizontal/" + value["campaign"], "sampler exact campaign root")
    p.require(value["work"].startswith(value["root"] + "/diagnostics/")
              and value["arm_root"].startswith(value["root"] + "/arms/"), "sampler exact owned subdirectories")
    p.require(type(value.get("run")) is str and d.re_run(value["run"])
              and p.hexstr(value.get("binary_sha256")), "sampler producer binding")
    p.require(type(value.get("prefix")) is str and re.fullmatch(r"[a-z][a-z0-9-]{7,47}", value["prefix"]), "sampler prefix")
    p.require(type(value.get("processes")) is list and all(type(x) is dict for x in value["processes"]), "sampler native process list")
    expected_ports = {4500, 4501, 4502, 4503, 4504, 4510} if value["role"] == "node1" else set()
    p.require({x.get("port") for x in value["processes"]} == expected_ports
              and len(value["processes"]) == len(expected_ports), "exact diagnostic process population")
    for process in value["processes"]:
        p.require(type(process.get("port")) is int and type(process.get("args")) is list
                  and all(type(x) is str for x in process["args"]) and type(process.get("unit_name")) is str
                  and re.fullmatch(r"oh-" + re.escape(value["prefix"]) + r"-[A-Za-z0-9_-]+-\d+\.service",
                                   process.get("unit_name", "")), "sampler native process specification")
        args = process["args"]
        p.require(args and args[0] == value["root"] + "/bin/fdbserver", "sampler exact native binary path")
        for flag, expected in (("--cluster-file", value["arm_root"] + "/fdb.cluster"),
                               ("--logdir", value["arm_root"] + "/logs"),
                               ("--datadir", value["arm_root"] + "/data/" + str(process["port"])),
                               ("--public-address", process.get("address"))):
            p.require(flag in args and args.index(flag) + 1 < len(args)
                      and args[args.index(flag) + 1] == expected, "sampler native argument binding")
    return value


def kv_counters(text):
    values = {}
    for line in text.splitlines():
        pair = line.split()
        p.require(len(pair) == 2 and pair[0] not in values and re.fullmatch(r"[0-9]+", pair[1]), "malformed cumulative counters")
        values[pair[0]] = int(pair[1])
    return values


def proc_stat(text):
    match = re.fullmatch(r"([0-9]+) \((.*)\) (.*)\s*", text.strip())
    p.require(match is not None, "malformed process stat")
    fields = match[3].split()
    p.require(len(fields) >= 20, "short process stat")
    def counter(index):
        p.require(re.fullmatch(r"[0-9]+", fields[index]) is not None, "malformed process stat counter")
        return int(fields[index])
    return {"pid": int(match[1]), "comm": match[2], "state": fields[0], "utime_ticks": counter(11),
            "stime_ticks": counter(12), "starttime_ticks": counter(19),
            "blkio_ticks": counter(39) if len(fields) > 39 else None, "raw": text}


def diskstats(text, devices):
    p.require(type(devices) is list and devices and len(set(devices)) == len(devices)
              and all(type(x) is str and re.fullmatch(r"[0-9]+:[0-9]+", x) for x in devices), "exact device identities required")
    result = {}
    for line in text.splitlines():
        fields = line.split()
        if len(fields) < 3:
            continue
        key = fields[0] + ":" + fields[1]
        if key not in devices:
            continue
        p.require(key not in result and len(fields) >= 14 and all(re.fullmatch(r"[0-9]+", x) for x in fields[3:]),
                  "malformed selected disk statistics")
        numbers = list(map(int, fields[3:]))
        result[key] = {"name": fields[2], "raw": line,
                       **{name: numbers[i] if i < len(numbers) else None for i, name in enumerate(DISK_FIELDS)}}
    p.require(set(result) == set(devices), "missing exact filesystem device statistics")
    return result


def read_small(path, limit=65536):
    with open(path, "rb") as stream:
        raw = stream.read(limit + 1)
    p.require(len(raw) <= limit, "diagnostic proc file exceeds bound")
    return raw.decode()


def process_snapshot(pid, *, proc_root=Path("/proc")):
    p.require(type(pid) is int and pid > 0, "exact process PID required")
    root = Path(proc_root) / str(pid)
    value = proc_stat(read_small(root / "stat"))
    p.require(value["pid"] == pid, "process stat PID mismatch")
    io = {}
    for line in read_small(root / "io").splitlines():
        key, raw = line.split(":", 1)
        p.require(key not in io and re.fullmatch(r"-?[0-9]+", raw.strip()), "malformed process I/O")
        io[key] = int(raw)
        p.require(io[key] >= 0 or key == "cancelled_write_bytes", "negative process I/O counter")
    threads = {}
    for path in sorted((root / "task").iterdir(), key=lambda x: int(x.name)):
        try:
            thread = proc_stat(read_small(path / "stat"))
            thread["wchan"] = read_small(path / "wchan", 1024).strip()
            threads[path.name] = thread
        except FileNotFoundError:
            continue  # EIO workers may exit; a missing main thread is not accepted.
    p.require(str(pid) in threads, "missing main thread")
    return {**value, "io": io, "threads": threads,
            "exe": os.readlink(root / "exe"), "cmdline": read_small(root / "cmdline").rstrip("\0").split("\0")}


def command(args, timeout=5):
    done = subprocess.run(args, capture_output=True, text=True, timeout=timeout)
    p.require(done.returncode == 0, "diagnostic command failed: " + repr(args) + ": " + done.stderr)
    return done.stdout.strip()


def native_bindings(grant):
    result = {}
    if grant["processes"]:
        p.require(p.sha(grant["root"] + "/bin/fdbserver") == grant["fdbserver_sha256"], "sampler native binary seal")
    for spec in grant["processes"]:
        pid = int(command(["systemctl", "show", spec["unit_name"], "--property=MainPID", "--value"]))
        snapshot = process_snapshot(pid)
        p.require(snapshot["cmdline"] == spec["args"] and snapshot["exe"] == spec["args"][0], "native process command changed")
        result[spec["unit_name"]] = {"pid": pid, "starttime_ticks": snapshot["starttime_ticks"],
                                     "address": spec["address"], "port": spec["port"], "args": spec["args"]}
    return result


def device_identity(root):
    device = os.stat(root).st_dev
    key = str(os.major(device)) + ":" + str(os.minor(device))
    sysdev = (Path("/sys/dev/block") / key).resolve(strict=True)
    devices = [key]
    if (sysdev / "partition").exists():
        parent = (sysdev.parent / "dev").read_text().strip()
        p.require(re.fullmatch(r"[0-9]+:[0-9]+", parent), "malformed parent block device")
        if parent != key:
            devices.append(parent)
    return {"filesystem_device": key, "devices": devices}


def producer_process(grant):
    unit = "otel-native-" + grant["run"] + "-ingest.scope"
    group = subprocess.run(["systemctl", "show", unit, "--property=ControlGroup", "--value"], capture_output=True, text=True, timeout=5)
    if group.returncode or not group.stdout.strip():
        return None
    root = (Path("/sys/fs/cgroup") / group.stdout.strip().lstrip("/")).resolve()
    p.require(root.is_relative_to("/sys/fs/cgroup"), "producer cgroup escaped")
    for raw in (root / "cgroup.procs").read_text().split():
        try:
            snapshot = process_snapshot(int(raw))
        except FileNotFoundError:
            continue
        if snapshot["exe"] == grant["binary"]:
            args = snapshot["cmdline"]
            p.require("--run" in args and args[args.index("--run") + 1] == grant["run"], "foreign producer in owned scope")
            return snapshot
    return None


def sample_frame(grant, bindings, device, sequence, phase):
    native = {}
    for unit, bound in bindings.items():
        snapshot = process_snapshot(bound["pid"])
        p.require(snapshot["starttime_ticks"] == bound["starttime_ticks"] and snapshot["cmdline"] == bound["args"],
                  "native process PID/starttime changed")
        native[unit] = snapshot
    kind = "fdb" if grant["role"] == "node1" else "client"
    group = command(["systemctl", "show", "horizontal-" + kind + ".slice", "--property=ControlGroup", "--value"])
    cg = (Path("/sys/fs/cgroup") / group.lstrip("/")).resolve()
    p.require(group and cg.is_relative_to("/sys/fs/cgroup"), "diagnostic cgroup escaped")
    limits = {key: read_small(cg / key).strip() for key in
              ("cpu.max", "cpuset.cpus.effective", "memory.max", "memory.swap.max", "memory.current", "memory.events")}
    quota, period = map(int, limits["cpu.max"].split())
    p.require(quota == period * 4 and limits["cpuset.cpus.effective"] == "0-3"
              and limits["memory.max"] == "17179869184" and limits["memory.swap.max"] == "0", "diagnostic cgroup cap changed")
    cpu = kv_counters(read_small(cg / "cpu.stat"))
    p.require({"usage_usec", "user_usec", "system_usec", "nr_periods", "nr_throttled", "throttled_usec"} <= set(cpu),
              "missing CPU throttling counters")
    io_raw = read_small(cg / "io.stat")
    io = {}
    for line in io_raw.splitlines():
        fields = line.split(); counters = {}
        p.require(fields and re.fullmatch(r"[0-9]+:[0-9]+", fields[0]), "malformed cgroup I/O device")
        for part in fields[1:]:
            key, value = part.split("=", 1)
            p.require(key not in counters and re.fullmatch(r"[0-9]+", value), "malformed cgroup I/O counters")
            counters[key] = int(value)
        p.require(fields[0] not in io, "duplicate cgroup I/O device")
        io[fields[0]] = counters
    return {"schema": "otel_native_frame_v1", "node_id": grant["node_id"], "role": grant["role"], "run": grant["run"],
            "token": grant["token"], "producer_grant_sha256": grant["producer_grant_sha256"], "sequence": sequence,
            "phase": phase, "epoch": time.time(), "monotonic_ns": time.monotonic_ns(),
            "boot_id": read_small("/proc/sys/kernel/random/boot_id", 128).strip(), "clock_ticks": os.sysconf("SC_CLK_TCK"),
            "cgroup": {"path": str(cg), "limits": limits, "cpu": cpu, "io": io, "io_raw": io_raw},
            "device": {**device, "counters": diskstats(read_small("/proc/diskstats", MIB), device["devices"])},
            "native": native, "producer": producer_process(grant) if grant["role"] == "client1" else None,
            "pressure": {kind: read_small("/proc/pressure/" + kind) for kind in ("cpu", "memory", "io")}}


def sample(path):
    grant = sampler_grant(d.read(path)); d.identity(grant["node_id"])
    base = Path("/srv/otel-horizontal") / grant["campaign"]
    work = no_symlinks(grant["work"], base)
    p.require(p.sha(path) == p.sha(work / "plan.json"), "sampler grant path mismatch")
    bindings = native_bindings(grant)
    device = device_identity(no_symlinks(grant["arm_root"], base))
    d.write(work / "identity.json", {"node_id": grant["node_id"], "grant_sha256": p.sha(path),
            "bindings": bindings, "device": device, "observational_overhead": "sampler shares the fixed role slice; not a throughput comparison"})
    sequence, size, origin = 0, 0, time.monotonic()
    with open(work / "frames.jsonl", "x") as stream:
        os.chmod(stream.name, 0o600)
        while time.time() < grant["first_epoch"] + 6000 and not (work / "stop.json").exists():
            phase = "baseline"
            if (work / "phase.json").exists():
                marker = d.read(work / "phase.json")
                p.require(marker == {"phase": "load", "token": grant["token"]}, "sampler phase binding")
                phase = "load"
            frame = sample_frame(grant, bindings, device, sequence, phase)
            encoded = json.dumps(frame, sort_keys=True, allow_nan=False) + "\n"
            size += len(encoded.encode())
            p.require(size < FRAME_BUDGET, "sampler frame byte budget exhausted")
            stream.write(encoded); stream.flush(); os.fsync(stream.fileno())
            sequence += 1
            if sequence == 2:
                p.require(phase == "baseline", "two sampler baseline frames required before release")
                d.write(work / "ready.json", {"node_id": grant["node_id"], "token": grant["token"],
                        "grant_sha256": p.sha(path), "baseline_frames": 2})
            time.sleep(max(0, origin + sequence - time.monotonic()))
    d.write(work / "sampler-exit.json", {"frames": sequence, "bytes": size, "epoch": time.time()})


def snapshot_errors(snapshot):
    if type(snapshot) is not dict:
        return ["process snapshot missing"]
    if (not all(p.integer(snapshot.get(k), 1 if k in ("pid", "starttime_ticks") else 0)
                for k in ("pid", "starttime_ticks", "utime_ticks", "stime_ticks"))
            or type(snapshot.get("io")) is not dict or not {"read_bytes", "write_bytes"} <= set(snapshot["io"])
            or any(type(v) is not int or v < 0 and k != "cancelled_write_bytes" for k, v in snapshot["io"].items())
            or type(snapshot.get("threads")) is not dict or str(snapshot["pid"]) not in snapshot["threads"]):
        return ["process CPU/I/O/thread data missing"]
    for tid, thread in snapshot["threads"].items():
        if (type(tid) is not str or re.fullmatch(r"[0-9]+", tid) is None or type(thread) is not dict
                or thread.get("pid") != int(tid) or not all(p.integer(thread.get(k), 1 if k == "starttime_ticks" else 0)
                for k in ("starttime_ticks", "utime_ticks", "stime_ticks")) or type(thread.get("wchan")) is not str):
            return ["thread counters/wchan missing"]
    return []


def frame_errors(frame, grant):
    errors = []
    if type(frame) is not dict:
        return ["frame is not an object"]
    if (frame.get("schema") != "otel_native_frame_v1" or not p.integer(frame.get("sequence"))
            or not p.integer(frame.get("monotonic_ns"), 1) or not p.number(frame.get("epoch"))
            or frame.get("phase") not in ("baseline", "load")
            or type(frame.get("boot_id")) is not str or not frame["boot_id"]):
        errors.append("frame clock/phase/schema missing")
    cg = frame.get("cgroup", {})
    limits = cg.get("limits", {}) if type(cg) is dict else {}
    quota, period = map(int, limits.get("cpu.max", "0 1").split())
    if (quota != period * 4 or period <= 0 or limits.get("cpuset.cpus.effective") != "0-3"
            or limits.get("memory.max") != "17179869184" or limits.get("memory.swap.max") != "0"):
        errors.append("frame resource cap mismatch")
    cpu = cg.get("cpu", {}) if type(cg) is dict else {}
    if not all(p.integer(cpu.get(k)) for k in ("usage_usec", "user_usec", "system_usec", "nr_periods", "nr_throttled", "throttled_usec")):
        errors.append("frame CPU counters missing")
    native = frame.get("native", {})
    if type(native) is not dict or set(native) != {x["unit_name"] for x in grant["processes"]}:
        errors.append("frame native population mismatch")
    elif any(snapshot_errors(x) for x in native.values()):
        errors.append("frame native process data missing")
    elif (len({x["pid"] for x in native.values()}) != len(native)
          or any(native[x["unit_name"]].get("cmdline") != x["args"]
                 or native[x["unit_name"]].get("exe") != x["args"][0] for x in grant["processes"])):
        errors.append("frame native process command binding changed")
    if frame.get("producer") is not None:
        producer = frame["producer"]
        args = producer.get("cmdline", []) if type(producer) is dict else []
        if (snapshot_errors(producer) or producer.get("exe") != grant["binary"]
                or "--run" not in args or args.index("--run") + 1 >= len(args)
                or args[args.index("--run") + 1] != grant["run"]):
            errors.append("frame producer process data/command binding missing")
    io = cg.get("io") if type(cg) is dict else None
    if type(io) is not dict or any(type(v) is not dict or any(not p.integer(x) for x in v.values()) for v in io.values()):
        errors.append("frame cgroup I/O counters missing")
    devices = frame.get("device", {})
    if (type(devices) is not dict or type(devices.get("devices")) is not list or not devices["devices"]
            or type(devices.get("counters")) is not dict or set(devices["devices"]) != set(devices["counters"])):
        errors.append("frame device identity missing")
    elif any(not all(p.integer(x.get(k)) for k in DISK_FIELDS) for x in devices["counters"].values()):
        errors.append("frame device/flush counters missing")
    if type(frame.get("pressure")) is not dict or set(frame["pressure"]) != {"cpu", "memory", "io"}:
        errors.append("frame host pressure missing")
    return errors


def profile(frames, grants, producer_receipt, activity, release_bound):
    reasons, populations = [], {}
    if type(grants) is not dict or set(grants) != ROLES or type(frames) is not dict or set(frames) != ROLES:
        return {"verdict": "INCONCLUSIVE", "reasons": ["exact two-role profile population required"],
                "populations": {}, "producer_process_bound": False, "positive_native_activity": False,
                "accepted_throughput": False}
    for role, grant in grants.items():
        if (type(grant) is not dict or not all(k in grant for k in
                ("node_id", "role", "run", "token", "producer_grant_sha256", "processes"))):
            return {"verdict": "INCONCLUSIVE", "reasons": ["incomplete sampler grant population"],
                    "populations": {}, "producer_process_bound": False, "positive_native_activity": False,
                    "accepted_throughput": False}
        rows = frames.get(role, [])
        if type(rows) is not list or any(type(x) is not dict for x in rows):
            reasons.append(role + ": malformed frame population")
            continue
        baseline = [x for x in rows if x.get("phase") == "baseline"]
        load = [x for x in rows if x.get("phase") == "load"]
        populations[role] = {"baseline_frames": len(baseline), "load_frames": len(load)}
        if len(baseline) < 2 or len(load) < 10:
            reasons.append(role + ": fewer than two baseline or ten load frames")
        elif (not p.integer(load[0].get("monotonic_ns"), 1) or not p.integer(load[-1].get("monotonic_ns"), 1)
              or load[-1]["monotonic_ns"] - load[0]["monotonic_ns"] < 9 * 10 ** 9):
            reasons.append(role + ": ten load frames do not span nine seconds")
        previous = None
        for frame in rows:
            try:
                malformed = frame_errors(frame, grant)
            except (AttributeError, KeyError, TypeError, ValueError):
                malformed = ["malformed frame container"]
            if malformed:
                reasons.extend(role + ": " + error for error in malformed)
                break
            if (frame.get("node_id") != grant["node_id"] or frame.get("role") != role
                    or frame.get("token") != grant["token"] or frame.get("run") != grant["run"]
                    or frame.get("producer_grant_sha256") != grant["producer_grant_sha256"]):
                reasons.append(role + ": frame identity mismatch"); break
            if previous is None and frame.get("sequence") != 0:
                reasons.append(role + ": missing initial sampler frame"); break
            if previous:
                if (previous.get("phase") == "load" and frame.get("phase") != "load"):
                    reasons.append(role + ": baseline frame after producer release"); break
                if (frame.get("cgroup", {}).get("path") != previous.get("cgroup", {}).get("path")
                        or frame.get("device", {}).get("devices") != previous.get("device", {}).get("devices")):
                    reasons.append(role + ": cgroup/device identity changed"); break
                if (frame.get("boot_id") != previous.get("boot_id")
                        or not p.integer(frame.get("monotonic_ns"), 1)
                        or frame["monotonic_ns"] <= previous.get("monotonic_ns", 0)
                        or frame.get("sequence") != previous.get("sequence", -1) + 1):
                    reasons.append(role + ": frame clock/sequence/boot identity changed"); break
                for key, value in previous.get("cgroup", {}).get("cpu", {}).items():
                    if not p.integer(frame.get("cgroup", {}).get("cpu", {}).get(key)) or frame["cgroup"]["cpu"][key] < value:
                        reasons.append(role + ": CPU counter reset or missing"); break
                for dev, old in previous.get("cgroup", {}).get("io", {}).items():
                    new = frame.get("cgroup", {}).get("io", {}).get(dev, {})
                    if any(not p.integer(new.get(k)) or new[k] < v for k, v in old.items()):
                        reasons.append(role + ": cgroup I/O counter reset or missing"); break
                for unit, old in previous.get("native", {}).items():
                    new = frame.get("native", {}).get(unit, {})
                    if (new.get("pid") != old.get("pid") or new.get("starttime_ticks") != old.get("starttime_ticks")
                            or any(not p.integer(new.get(k)) or new[k] < old[k] for k in ("utime_ticks", "stime_ticks"))):
                        reasons.append(role + ": native identity/counter changed"); break
                    if any(k != "cancelled_write_bytes" and (k not in new.get("io", {}) or new["io"][k] < v)
                           for k, v in old.get("io", {}).items()):
                        reasons.append(role + ": native I/O counter reset or missing"); break
                    for tid, thread in old.get("threads", {}).items():
                        current = new.get("threads", {}).get(tid)
                        if current and (current["starttime_ticks"] != thread["starttime_ticks"]
                                or any(current[k] < thread[k] for k in ("utime_ticks", "stime_ticks"))):
                            reasons.append(role + ": thread identity/counter changed"); break
                for dev, old in previous.get("device", {}).get("counters", {}).items():
                    new = frame.get("device", {}).get("counters", {}).get(dev, {})
                    if any(old.get(k) is not None and (not p.integer(new.get(k)) or new[k] < old[k])
                           for k in DISK_FIELDS if k != "in_flight"):
                        reasons.append(role + ": device counter reset or missing"); break
            previous = frame
    client_rows = frames.get("client1") if type(frames.get("client1")) is list else []
    pids = [x.get("producer") for x in client_rows
            if type(x) is dict and x.get("phase") == "load" and type(x.get("producer")) is dict]
    producer_known = (type(producer_receipt) is dict and producer_receipt.get("started") is True
                      and producer_receipt.get("node_id") == grants["client1"]["node_id"]
                      and producer_receipt.get("run") == grants["client1"]["run"]
                      and producer_receipt.get("token") == grants["client1"]["token"]
                      and producer_receipt.get("grant_sha256") == grants["client1"]["producer_grant_sha256"]
                      and p.integer(producer_receipt.get("pid"), 1) and p.integer(producer_receipt.get("starttime_ticks"), 1)
                      and any(x.get("pid") == producer_receipt["pid"] and x.get("starttime_ticks") == producer_receipt["starttime_ticks"] for x in pids))
    if not release_bound or not producer_known:
        reasons.append("producer ready/release/process binding is unknown")
    activity = activity if type(activity) is list else []
    counters = [x.get("storage", {}).get("node1", {}) for x in activity
                if type(x) is dict and type(x.get("storage")) is dict
                and type(x["storage"].get("node1")) is dict]
    positive = (len(counters) >= 2 and all(type(x.get("storage_id")) is str and x["storage_id"] and x["storage_id"] == counters[0].get("storage_id")
                and p.integer(x.get("input_bytes")) for x in counters)
                and all(b["input_bytes"] >= a["input_bytes"] for a, b in zip(counters, counters[1:]))
                and counters[-1]["input_bytes"] > counters[0]["input_bytes"])
    if not positive:
        reasons.append("native input activity coverage unknown, reset or zero")
    return {"verdict": "CAPTURED" if not reasons else "INCONCLUSIVE", "populations": populations,
            "producer_process_bound": bool(producer_known and release_bound), "positive_native_activity": bool(positive),
            "reasons": reasons, "accepted_throughput": False,
            "boundary": "observational profile only; native input includes MVCC/DD/background accounting, not physical I/O or causal CPU/disk proof"}


def trace_tail(arm_root, campaign, bindings, budget=TRACE_BUDGET):
    p.require(type(budget) is int and 0 <= budget <= TRACE_BUDGET, "aggregate native trace budget")
    arm = Path(arm_root)
    p.require(not any(x.is_symlink() for x in (arm, *arm.parents)) and arm.is_dir(), "native arm is not a real directory")
    logs = no_symlinks(arm / "logs", arm)
    p.require(logs.is_dir(), "native log directory missing")
    targets = {x["address"]: x for x in bindings.values() if x["port"] in (4504, 4510)}
    p.require(len(targets) == 2 and {x["port"] for x in targets.values()} == {4504, 4510}, "exact storage/log trace targets required")
    result = {"files": [], "errors": [], "bytes_read": 0, "budget": budget,
              "boundary": "raw bounded NDJSON tails, all event types; Machine/LogGroup map via verified native PID/start/cmdline, not trace ThreadID"}
    observed = {}
    for path in logs.iterdir():
        match = re.fullmatch(r"trace\.(\d+\.\d+\.\d+\.\d+)\.(\d+)\.(\d+)\.([A-Za-z0-9]{6})\.(\d+)\.(\d+)\.json", path.name)
        if match and match[1] + ":" + match[2] in targets:
            p.require(not path.is_symlink(), "native trace symlink refused")
            address = match[1] + ":" + match[2]
            key = (int(match[3]), int(match[6]))
            if address not in observed or key > observed[address][0]:
                observed[address] = (key, path)
    entries = [x[1] for _, x in sorted(observed.items())]
    result["selection"] = "latest observed file for each exact storage/log address"
    for index, path in enumerate(entries):
        match = re.fullmatch(r"trace\.(\d+\.\d+\.\d+\.\d+)\.(\d+)\.(\d+)\.([A-Za-z0-9]{6})\.(\d+)\.(\d+)\.json", path.name)
        if not match:
            continue
        address = match[1] + ":" + match[2]
        if address not in targets:
            continue
        p.require(not path.is_symlink(), "native trace symlink refused")
        remaining = budget - result["bytes_read"]
        if remaining <= 0:
            break
        fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
        try:
            info = os.fstat(fd)
            p.require(stat.S_ISREG(info.st_mode), "native trace is not a regular file")
            size = info.st_size
            allowance = remaining // (len(entries) - index)
            offset = max(0, size - allowance)
            os.lseek(fd, offset, os.SEEK_SET)
            raw = os.read(fd, size - offset)
        finally:
            os.close(fd)
        result["bytes_read"] += len(raw)
        complete = raw
        if offset:
            split = complete.find(b"\n")
            complete = complete[split + 1:] if split >= 0 else b""
        if complete and not complete.endswith(b"\n"):
            complete = complete.rsplit(b"\n", 1)[0] + b"\n" if b"\n" in complete else b""
        for line in complete.splitlines():
            event = d.loads(line.decode())
            p.require(type(event) is dict and event.get("Machine") == address and event.get("LogGroup") == campaign,
                      "native trace Machine/LogGroup binding mismatch")
        bound = targets[address]
        result["files"].append({"name": path.name, "address": address, "pid": bound["pid"],
                                "starttime_ticks": bound["starttime_ticks"], "args": bound["args"],
                                "observed_size": size, "offset": offset, "bytes_read": len(raw),
                                "sha256": hashlib.sha256(raw).hexdigest(), "raw_base64": base64.b64encode(raw).decode(),
                                "complete_records": len(complete.splitlines()), "complete_ndjson": complete.decode()})
    result["coverage"] = "CAPTURED" if {x["address"] for x in result["files"] if x["complete_records"]} == set(targets) else "INCONCLUSIVE"
    return result


def producer_grant(value):
    d.client_plan(value)
    p.require(value.get("diagnostic_only") is True and type(value.get("diagnostic_seconds")) is int
              and value["diagnostic_seconds"] == (300 if value["count"] == 100000 else 120),
              "diagnostic producer stage bound")
    return value


def run_app(grant, mode):
    d.identity(grant["node_id"])
    p.require(p.sha(grant["binary"]) == grant["binary_sha256"], "diagnostic producer binary changed")
    work = Path(grant["work"])
    cap = grant["diagnostic_seconds"] if mode == "ingest" else 120
    argv = d.app_argv(grant, mode)
    argv[argv.index("--timeout") + 1] = str(cap) + "s"
    unit = "otel-native-" + grant["run"] + "-" + mode
    args = ["systemd-run", "--scope", "--quiet", "--collect", "--unit=" + unit,
            "--slice=horizontal-client.slice", "--property=CPUQuota=400%", "--property=MemoryMax=17179869184",
            "--property=MemorySwapMax=0", "--property=AllowedCPUs=0-3", "env", "GOMAXPROCS=4", *argv]
    d.write(work / (mode + ".command.json"), {"argv": args, "grant_sha256": p.sha(work / "plan.json")})
    started, error, code = time.monotonic_ns(), None, None
    with open(work / (mode + ".stdout"), "xb") as stdout, open(work / (mode + ".stderr"), "xb") as stderr:
        child = subprocess.Popen(args, stdout=stdout, stderr=stderr)
        limit = time.monotonic() + d.remaining(grant, cap)
        try:
            if mode == "ingest":
                until = min(limit, time.monotonic() + 3)
                process = None
                while child.poll() is None and time.monotonic() < until:
                    process = producer_process(grant)
                    if process:
                        break
                    time.sleep(0.05)
                receipt = {"started": process is not None, "node_id": grant["node_id"], "run": grant["run"],
                           "token": grant["token"], "grant_sha256": p.sha(work / "plan.json"),
                           "pid": process["pid"] if process else None,
                           "starttime_ticks": process["starttime_ticks"] if process else None,
                           "epoch": time.time(), "monotonic_ns": time.monotonic_ns()}
                d.write(work / "producer-started.json", receipt)
            while child.poll() is None:
                p.require(time.monotonic() < limit and not (work / "abort.json").exists(), "diagnostic stage aborted/expired")
                time.sleep(0.1)
            code = child.wait()
        except BaseException as exc:
            error = exc
            try:
                subprocess.run(["systemctl", "stop", unit + ".scope"], capture_output=True, timeout=3)
            except (OSError, subprocess.SubprocessError):
                pass  # Still reap the local scope wrapper; owned-node teardown is independent.
            if child.poll() is None:
                child.terminate()
            try:
                child.wait(timeout=3)
            except subprocess.TimeoutExpired:
                child.kill(); child.wait(timeout=3)
            code = child.returncode
        finally:
            d.write(work / (mode + ".exit.json"), {"exit": code, "aborted": error is not None,
                    "error": str(error) if error else "", "started_ns": started, "ended_ns": time.monotonic_ns()})
    if error:
        raise error
    p.require(code == 0, "diagnostic application failed: " + mode)
    return d.read(work / (mode + ".json"))


def produce(path, verify=False):
    grant = producer_grant(d.read(path)); d.identity(grant["node_id"])
    work = Path(grant["work"])
    p.require(p.sha(path) == p.sha(work / "plan.json"), "diagnostic producer grant mismatch")
    if verify:
        for mode in ("audit", "query"):
            run_app(grant, mode)
        return
    d.write(work / "ready.json", {"node_id": grant["node_id"], "run": grant["run"],
            "token": grant["token"], "grant_sha256": p.sha(path)})
    until = time.monotonic() + d.remaining(grant, 60)
    while not (work / "release.json").exists():
        p.require(time.monotonic() < until and not (work / "abort.json").exists(), "diagnostic barrier timeout/abort")
        time.sleep(0.05)
    p.require(d.read(work / "release.json") == {"token": grant["token"], "grant_sha256": p.sha(path)}, "diagnostic release binding")
    run_app(grant, "ingest")


class Diagnostic(d.Campaign):
    def __init__(self, *args, **kwargs):
        super().__init__(*args, **kwargs)
        self.trace_remaining = TRACE_BUDGET

    def install_helpers(self):
        super().install_helpers()
        for role, node in self.nodes.items():
            rc, _, _ = self.provision.copy_to_node(self.plan, self.out / "provision", node, HERE / "diagnostic.py",
                                                   self.remote_harness + "/diagnostic.py")
            p.require(rc == 0, "copy diagnostic helper failed")
            result = self.command(node, ["sha256sum", self.remote_harness + "/diagnostic.py"])
            p.require(result.split()[0] == p.sha(HERE / "diagnostic.py"), "remote diagnostic helper changed")
            unit = "horizontal-" + ("fdb" if role == "node1" else "client") + ".slice"
            accounting = self.command(node, ["systemctl", "show", unit, "--property=IOAccounting", "--value"]).strip()
            p.require(accounting == "yes", "diagnostic role slice I/O accounting is not enabled")
            group = self.command(node, ["systemctl", "show", unit, "--property=ControlGroup", "--value"]).strip()
            p.require(group == "/horizontal.slice/" + unit, "diagnostic role slice control group changed")
            path = "/sys/fs/cgroup" + group + "/io.stat"
            raw = self.command(node, ["cat", path])
            d.write(self.out / (role + "-io-accounting.json"), {"node_id": node["id"], "role": role,
                    "unit": unit, "IOAccounting": accounting, "io_stat_path": path, "io_stat_raw": raw})

    def diag(self, node, mode, path, *extra):
        return ["python3", "-B", self.remote_harness + "/diagnostic.py", mode,
                "--path", str(path), "--node-id", str(node["id"]), *extra]

    def spawn(self, node, args, directory, name):
        stdout, stderr = open(directory / (name + ".stdout"), "xb"), open(directory / (name + ".stderr"), "xb")
        argv = self.provision.ssh_argv(self.plan, self.out / "provision", node, args)
        try:
            process = subprocess.Popen(argv, stdout=stdout, stderr=stderr)
        except BaseException:
            stdout.close(); stderr.close(); raise
        return process, stdout, stderr

    def copy(self, node, local, remote):
        rc, _, _ = self.provision.copy_to_node(self.plan, self.out / "provision", node, local, remote)
        p.require(rc == 0, "diagnostic owned-file copy failed")

    def wait_file(self, node, path, cap=60, job=None):
        until = time.monotonic() + d.remaining(self.plan, cap)
        while time.monotonic() < until:
            p.require(job is None or job[0].poll() is None, "diagnostic helper exited before its ready receipt")
            rc, _, _ = self.provision.run_on_node(self.plan, self.out / "provision", node,
                                                 ["test", "-f", str(path)], timeout=d.remaining(self.plan, 5))
            if rc == 0:
                return self.read_remote(node, path)
            self.sleep(0.1)
        raise ValueError("diagnostic ready receipt timed out")

    def wave(self, layout, count, name):
        d.remaining(self.plan, reserve=1500)
        directory = self.out / name; directory.mkdir(mode=0o700)
        native_before = self.stable(layout)
        client = self.nodes["client1"]
        run = "prof20k9800" if count == 100000 else "prof20k9801"
        work = self.remote_root + "/producers/" + run
        token = hashlib.sha256((self.plan["campaign"] + run).encode()).hexdigest()
        grant = {**{k: self.plan[k] for k in ("campaign", "first_epoch", "deadline_epoch", "binary_sha256", "source_revision",
                                           "batch", "workers", "gomaxprocs", "storage_options")},
                 "schema": "otel_horizontal_producer_v1", "node_id": client["id"], "run": run, "count": count,
                 "independent_store": True, "diagnostic_only": True, "diagnostic_seconds": 300 if count == 100000 else 120,
                 "token": token, "binary": self.remote_root + "/bin/otel-demo",
                 "cluster_file": layout["remote_cluster_file"], "work": work}
        producer_grant(grant)
        self.command(client, ["mkdir", work])
        local = directory / "producer-plan.json"; d.write(local, grant)
        self.copy(client, local, work + "/plan.json")
        cluster_hash = self.command(client, ["sha256sum", layout["remote_cluster_file"]])
        p.require(cluster_hash.split()[0] == p.sha(layout["cluster_file"]), "diagnostic client cluster file changed")
        grants, samplers, frames, cursors = {}, {}, {}, {}
        producer_job, verification_job, receipt, release_bound = None, None, None, False
        activity = [native_before]
        result = {"schema": "otel_native_wave_v1", "count": count, "producers": 1,
                  "diagnostic_only": True, "accepted_throughput": False, "healthy": False,
                  "ingest_valid": False, "census_pass": False, "oracle_pass": False}
        error = None

        def poll_frames():
            for role, sample_grant in grants.items():
                node = self.nodes[role]
                value = d.loads(self.command(node, self.diag(node, "_frames", sample_grant["work"] + "/plan.json",
                                                           "--cursor", str(cursors[role]))))
                p.require(type(value) is dict and type(value.get("frames")) is list
                          and type(value.get("cursor")) is int and value["cursor"] >= cursors[role], "sampler cursor response")
                frames[role].extend(value["frames"])
                cursors[role] = value["cursor"]
                d.write(directory / (role + "-frames-" + str(cursors[role]) + "-" + str(self.clock()) + ".json"), value)
                self.receipt_budget()

        try:
            for role, node in self.nodes.items():
                sample_work = self.remote_root + "/diagnostics/" + run + "/" + role
                self.command(node, ["mkdir", "-p", str(Path(sample_work).parent)])
                self.command(node, ["mkdir", sample_work])
                sample_grant = {"schema": "otel_native_sampler_v1", "role": role, "node_id": node["id"],
                                "campaign": self.plan["campaign"], "prefix": self.plan["prefix"],
                                "first_epoch": self.plan["first_epoch"], "deadline_epoch": self.plan["deadline_epoch"],
                                "root": self.remote_root, "work": sample_work, "arm_root": layout["root"],
                                "binary": grant["binary"], "binary_sha256": grant["binary_sha256"],
                                "fdbserver_sha256": self.plan["fdbserver_sha256"], "run": run, "token": token,
                                "producer_grant_sha256": p.sha(local),
                                "processes": [x for x in layout["processes"] if x["node"] == role]}
                sampler_grant(sample_grant)
                path = directory / (role + "-sampler-plan.json"); d.write(path, sample_grant)
                self.copy(node, path, sample_work + "/plan.json")
                kind = "fdb" if role == "node1" else "client"
                args = ["systemd-run", "--scope", "--quiet", "--collect", "--unit=otel-native-sampler-" + run + "-" + role,
                        "--slice=horizontal-" + kind + ".slice", *self.diag(node, "_sample", sample_work + "/plan.json")]
                samplers[role] = self.spawn(node, args, directory, role + "-sampler")
                grants[role], frames[role], cursors[role] = sample_grant, [], 0
            for role, sample_grant in grants.items():
                ready = self.wait_file(self.nodes[role], sample_grant["work"] + "/ready.json", job=samplers[role])
                p.require(ready == {"node_id": self.nodes[role]["id"], "token": token, "baseline_frames": 2,
                          "grant_sha256": p.sha(directory / (role + "-sampler-plan.json"))}, "sampler ready identity binding")
                d.write(directory / (role + "-sampler-ready.json"), ready)
            poll_frames()
            producer_job = self.spawn(client, self.diag(client, "_produce", work + "/plan.json"), directory, "producer")
            ready = self.wait_file(client, work + "/ready.json", job=producer_job)
            p.require(ready == {"node_id": client["id"], "run": run, "token": token, "grant_sha256": p.sha(local)},
                      "single diagnostic producer ready binding")
            d.write(directory / "producer-ready.json", ready)
            for role, sample_grant in grants.items():
                self.command(self.nodes[role], self.diag(self.nodes[role], "_phase", sample_grant["work"] + "/plan.json"))
            release_before = self.clock()
            self.command(client, self.remote(client, "_release", work + "/plan.json"))
            release_bound = True
            d.write(directory / "release.json", {"node_id": client["id"], "run": run, "token": token,
                    "grant_sha256": p.sha(local), "controller_before_ns": release_before, "controller_after_ns": self.clock()})
            receipt = self.wait_file(client, work + "/producer-started.json", 10)
            limit = time.monotonic() + d.remaining(self.plan, grant["diagnostic_seconds"])
            while producer_job[0].poll() is None:
                p.require(time.monotonic() < limit, "diagnostic ingest observation cap")
                poll_frames()
                activity.append(self.monitor(layout))
                p.require(all(job[0].poll() is None for job in samplers.values()), "diagnostic sampler exited during load")
                self.sleep(0.5)
            poll_frames()
            p.require(producer_job[0].wait(timeout=2) == 0, "diagnostic producer failed")
            evidence = self.read_remote(client, work + "/ingest.json")
            p.evidence(evidence, self.plan, run, count, "ingest")
            d.write(directory / "ingest.json", evidence); result["ingest_valid"] = True
            for role, sample_grant in grants.items():
                self.command(self.nodes[role], self.diag(self.nodes[role], "_stop", sample_grant["work"] + "/plan.json"))
            self.stable(layout, 300)
            verification_job = self.spawn(client, self.diag(client, "_verify", work + "/plan.json"), directory, "verification")
            until = time.monotonic() + d.remaining(self.plan, 245)
            while verification_job[0].poll() is None:
                p.require(time.monotonic() < until, "diagnostic census/oracle cap")
                self.monitor(layout); self.sleep(0.5)
            p.require(verification_job[0].wait(timeout=2) == 0, "diagnostic census/oracle failed")
            for mode in ("audit", "query"):
                evidence = self.read_remote(client, work + "/" + mode + ".json")
                p.evidence(evidence, self.plan, run, count, mode)
                d.write(directory / (mode + ".json"), evidence)
            self.stable(layout)
            result.update(healthy=True, census_pass=True, oracle_pass=True)
        except BaseException as exc:
            error = exc; result["error"] = str(exc)
        finally:
            for job in (producer_job, verification_job):
                if job and job[0].poll() is None:
                    try:
                        self.command(client, self.remote(client, "_abort", work + "/plan.json"), 5)
                        job[0].wait(timeout=8)
                    except (ValueError, OSError, subprocess.SubprocessError):
                        job[0].terminate()
                        try: job[0].wait(timeout=3)
                        except subprocess.TimeoutExpired: job[0].kill(); job[0].wait(timeout=3)
                if job:
                    job[1].close(); job[2].close()
            for role, sample_grant in grants.items():
                try:
                    self.command(self.nodes[role], self.diag(self.nodes[role], "_stop", sample_grant["work"] + "/plan.json"), 5)
                except (ValueError, OSError, subprocess.SubprocessError) as exc:
                    result.setdefault("collection_errors", []).append(str(exc))
            try:
                poll_frames()
            except (ValueError, OSError, subprocess.SubprocessError) as exc:
                result.setdefault("collection_errors", []).append(str(exc))
            for role, job in samplers.items():
                try:
                    job[0].wait(timeout=5)
                except subprocess.TimeoutExpired:
                    job[0].terminate()
                    try: job[0].wait(timeout=3)
                    except subprocess.TimeoutExpired: job[0].kill(); job[0].wait(timeout=3)
                job[1].close(); job[2].close()
                result.setdefault("sampler_exits", {})[role] = job[0].returncode
            try:
                self.collect(client, grant, directory)
            except (ValueError, OSError, subprocess.SubprocessError) as exc:
                result.setdefault("collection_errors", []).append(str(exc))
            if "node1" in grants:
                try:
                    node = self.nodes["node1"]
                    traces = d.loads(self.command(node, self.diag(node, "_traces", grants["node1"]["work"] + "/plan.json",
                                                                 "--budget", str(self.trace_remaining))))
                    p.require(type(traces.get("bytes_read")) is int and 0 <= traces["bytes_read"] <= self.trace_remaining,
                              "aggregate native trace response budget")
                    self.trace_remaining -= traces["bytes_read"]
                    d.write(directory / "native-traces.json", traces)
                except (ValueError, OSError, subprocess.SubprocessError) as exc:
                    result.setdefault("collection_errors", []).append(str(exc))
            result["producer_process_receipt"] = receipt
            result["release_bound"] = release_bound
            result["native_activity"] = activity
            result["profile"] = profile(frames, grants, receipt, activity, release_bound)
            d.write(directory / "frames.json", frames)
            d.write(directory / "result.json", result)
        if error:
            raise error
        return result

    def run(self):
        validate_plan(self.plan)
        source = p.verify_source(self.plan)
        self.out.mkdir(mode=0o700, parents=True, exist_ok=False)
        d.write(self.out / "approved-plan.json", self.plan)
        d.write(self.out / "source-before.json", source)
        result = {"schema": "otel_native_diagnostic_result_v1", "diagnostic_only": True,
                  "accepted_throughput": False, "authorized_rows": 1100000, "producers": 1,
                  "boundary": "one FDB host and one producer; sampler overhead included in fixed slices; not comparable to two-producer throughput"}
        try:
            self.resources = self.provision.prepare(self.plan, self.out / "provision")
            self.nodes = self.resources["nodes"]
            p.require(set(self.nodes) == ROLES, "diagnostic exact two owned nodes")
            self.install_helpers(); self.quiet()
            d.remaining(self.plan, reserve=2000)
            layout = self.provision.setup_cluster(self.plan, self.out / "provision", self.nodes, 1, "diagnostic")
            self.layouts.append(layout)
            observations = self.host_samples()
            self.pid_baseline = {"node1": observations["node1"]["fdb_pids"]}
            self.stable(layout, startup=True)
            gate = self.wave(layout, 100000, "prerequisite")
            p.require(type(gate) is dict and type(gate.get("count")) is int and gate["count"] == 100000
                      and type(gate.get("producers")) is int and gate["producers"] == 1
                      and all(gate.get(k) is True for k in ("healthy", "ingest_valid", "census_pass", "oracle_pass")),
                      "one-producer 100k health/census/oracle gate required before diagnostic probe")
            result["prerequisite"] = gate
            result["probe"] = self.wave(layout, 1000000, "probe")
        except BaseException as exc:
            result["error"] = str(exc)
            raise
        finally:
            d.write(self.out / "diagnostic-result.json", result)
            if self.resources is not None:
                cleanup = self.provision.teardown(self.plan, self.out / "provision", self.resources)
                d.write(self.out / "cleanup.json", cleanup)
                p.require(cleanup.get("passed") is True, "diagnostic exact ten-kind baseline cleanup did not pass")
            d.write(self.out / "source-after.json", p.verify_source(self.plan))


def main(argv=None):
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("mode", choices=("run", "_sample", "_frames", "_phase", "_stop", "_traces", "_produce", "_verify"))
    ap.add_argument("--plan"); ap.add_argument("--output"); ap.add_argument("--path")
    ap.add_argument("--node-id", type=int); ap.add_argument("--cursor", type=int, default=0)
    ap.add_argument("--budget", type=int, default=TRACE_BUDGET); ap.add_argument("--execute-approved", action="store_true")
    args = ap.parse_args(argv)
    if args.mode == "run":
        plan = validate_plan(d.read(args.plan))
        if not args.execute_approved:
            print(json.dumps({"validated": True, "nodes": 2, "rows": 1100000, "diagnostic_only": True,
                              "accepted_throughput": False, "provisioned": False}))
            return
        import provision  # Controller-only: the remote three-file harness has no repository tools.
        Diagnostic(plan, args.output, provision).run()
        return
    if args.mode in ("_produce", "_verify"):
        grant = producer_grant(d.read(args.path))
        p.require(grant["node_id"] == args.node_id, "diagnostic producer host mismatch")
        produce(args.path, args.mode == "_verify")
        return
    grant = sampler_grant(d.read(args.path))
    p.require(grant["node_id"] == args.node_id, "sampler host mismatch")
    d.identity(args.node_id)
    base = Path("/srv/otel-horizontal") / grant["campaign"]
    work = no_symlinks(grant["work"], base)
    if args.mode == "_sample":
        sample(args.path)
    elif args.mode == "_phase":
        d.write(work / "phase.json", {"phase": "load", "token": grant["token"]})
    elif args.mode == "_stop":
        if not (work / "stop.json").exists():
            d.write(work / "stop.json", {"token": grant["token"]})
    elif args.mode == "_frames":
        p.require(type(args.cursor) is int and args.cursor >= 0, "sampler cursor")
        path = no_symlinks(work / "frames.jsonl", base)
        p.require(path.stat().st_size <= FRAME_BUDGET, "sampler frame file budget")
        raw = path.read_bytes()
        lines = raw.split(b"\n")[:-1]  # A live writer's incomplete final record belongs to the next read.
        p.require(args.cursor <= len(lines), "sampler cursor beyond population")
        end = min(len(lines), args.cursor + 64)
        print(json.dumps({"cursor": end, "frames": [d.loads(x.decode()) for x in lines[args.cursor:end]]}, sort_keys=True))
    elif args.mode == "_traces":
        identity = d.read(no_symlinks(work / "identity.json", base))
        p.require(identity.get("node_id") == args.node_id and identity.get("grant_sha256") == p.sha(args.path), "trace identity receipt binding")
        for unit, bound in identity["bindings"].items():
            actual = process_snapshot(bound["pid"])
            p.require(actual["starttime_ticks"] == bound["starttime_ticks"] and actual["cmdline"] == bound["args"],
                      "trace native process identity changed")
        arm = no_symlinks(grant["arm_root"], base)
        print(json.dumps(trace_tail(arm, grant["campaign"], identity["bindings"], args.budget), sort_keys=True))


if __name__ == "__main__":
    main()
