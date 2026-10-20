#!/usr/bin/env python3
"""Run a preregistered counterbalanced FDB measurement plan, one process at a time.

Standard library only. Plan fields: seed, blocks (multiple of factorial(arms),
or explicit_orders with pairwise precedence balance and near-balanced positions),
workloads [{name, iterations, warmup, workers}], arms [{name, binary, command}],
cooldown_seconds, timeout_seconds, environment. A command is an argv list with
{binary}, {workload}, {iterations}, {warmup}, {workers}, {output}, {run_id} tokens.
Artifacts are written before execution; an incomplete schedule is never a pass.
"""
import argparse
import datetime
import csv
import hashlib
import itertools
import json
import math
import os
from pathlib import Path
import random
import signal
import subprocess
import sys
import time


def utc():
    return datetime.datetime.now(datetime.timezone.utc).isoformat()


def dump(path, value):
    path.write_text(json.dumps(value, indent=2, sort_keys=True) + "\n")


def sha(path):
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for part in iter(lambda: f.read(1024 * 1024), b""):
            h.update(part)
    return h.hexdigest()


def snapshot(dest):
    dest.mkdir()
    files = ["/proc/stat", "/proc/loadavg", "/proc/meminfo", "/proc/vmstat",
             "/proc/diskstats", "/proc/net/dev", "/proc/net/snmp",
             "/proc/net/netstat", "/proc/pressure/cpu", "/proc/pressure/memory",
             "/proc/pressure/io", "/sys/fs/cgroup/cpu.stat",
             "/sys/fs/cgroup/memory.events"]
    dump(dest / "time.json", {"utc": utc(), "monotonic_ns": time.monotonic_ns()})
    for name in files:
        try:
            (dest / name.lstrip("/").replace("/", "_")).write_bytes(Path(name).read_bytes())
        except OSError as e:
            (dest / (name.lstrip("/").replace("/", "_") + ".error")).write_text(str(e))
    p = subprocess.run(["ps", "-eLo", "pid,ppid,tid,psr,ni,pcpu,pmem,rss,stat,comm,args"],
                       capture_output=True, text=True)
    (dest / "processes.txt").write_text(p.stdout + p.stderr)


def check_builds():
    p = subprocess.run(["ps", "-eo", "pid=,comm="], check=True,
                       capture_output=True, text=True)
    forbidden = {"bazel", "bazelisk", "compile", "cc1", "cc1plus", "ninja", "make",
                 "gcc", "g++", "clang", "clang++", "rustc", "go", "apt", "apt-get", "dpkg"}
    found = [line for line in p.stdout.splitlines() if line.split()[-1] in forbidden]
    if found:
        raise RuntimeError("quiescence violation (build/install): " + "; ".join(found))


def cpu_counters():
    rows = {}
    for line in Path("/proc/stat").read_text().splitlines():
        fields = line.split()
        if fields[0].startswith("cpu"):
            rows[fields[0]] = [int(v) for v in fields[1:9]]
    return rows


def cpu_usage(delta):
    total = sum(delta)
    assert total > 0 and min(delta) >= 0
    # guest/guest_nice are excluded because already included in user/nice.
    return {"busy_fraction": (total - delta[3] - delta[4] - delta[7]) / total,
            "steal_fraction": delta[7] / total, "cpu_jiffy_deltas": delta}


def quiescence(plan):
    before = cpu_counters()
    time.sleep(plan["quiescence_seconds"])
    after = cpu_counters()
    delta = {cpu: [b - a for a, b in zip(before[cpu], after[cpu])] for cpu in before}
    all_cpus = {cpu: cpu_usage(values) for cpu, values in delta.items()}
    groups = {}
    for group in plan["cpu_sets"]:
        members = [all_cpus["cpu" + str(cpu)] for cpu in group["cpus"]]
        aggregate = cpu_usage([sum(m["cpu_jiffy_deltas"][i] for m in members) for i in range(8)])
        maximum = max(m["busy_fraction"] for m in members)
        passed = (aggregate["busy_fraction"] <= group["max_busy_fraction"] and
                  maximum <= group["max_cpu_busy_fraction"] and
                  max(m["steal_fraction"] for m in members) <= plan["max_steal_fraction"])
        groups[group["name"]] = dict(aggregate, maximum_cpu_busy_fraction=maximum,
                                      thresholds=group, passed=passed)
    passed = (all_cpus["cpu"]["busy_fraction"] <= plan["max_busy_fraction"] and
              all(group["passed"] for group in groups.values()))
    return {"seconds": plan["quiescence_seconds"], "cpus": all_cpus, "groups": groups,
            "max_steal_fraction": plan["max_steal_fraction"], "passed": passed}


def telemetry(child_pid):
    result = {"utc": utc(), "monotonic_ns": time.monotonic_ns()}
    for path in ("/proc/stat", "/proc/loadavg", "/proc/pressure/cpu", "/proc/pressure/io",
                 "/proc/pressure/memory", "/proc/meminfo", "/proc/diskstats", "/proc/net/dev"):
        result[path] = Path(path).read_text()
    processes = []
    for entry in Path("/proc").iterdir():
        if not entry.name.isdecimal():
            continue
        try:
            comm = (entry / "comm").read_text().strip()
            if comm == "fdbserver" or int(entry.name) == child_pid or comm.startswith("baseline-") or comm.startswith("candidate-"):
                processes.append({"pid": int(entry.name), "comm": comm,
                    **{name: (entry / name).read_text() for name in ("stat", "status", "io")}})
        except (FileNotFoundError, ProcessLookupError, PermissionError):
            pass
    result["processes"] = processes
    return result


def stop_process_group(process):
    if process is None:
        return
    try:
        os.killpg(process.pid, signal.SIGTERM)
    except ProcessLookupError:
        pass
    try:
        process.wait(timeout=5)
    except subprocess.TimeoutExpired:
        try:
            os.killpg(process.pid, signal.SIGKILL)
        except ProcessLookupError:
            pass
        process.wait()
    # The wrapper may have exited before a grandchild; the owned group still dies.
    try:
        os.killpg(process.pid, signal.SIGKILL)
    except ProcessLookupError:
        pass


def validate_raw_output(folder, workload):
    summary = json.loads((folder / "summary.json").read_text())
    if (summary.get("success") is not True or summary.get("persisted_validation") != "ok" or
            summary.get("requested") != workload["iterations"] or summary.get("completed") != workload["iterations"] or
            summary.get("errors") != 0 or summary.get("warmup_completed") != workload["warmup"] or
            summary.get("warmup_errors") != 0 or summary.get("measured_wall_ns", 0) <= 0):
        raise ValueError("invalid/incomplete summary or persisted validation: " + str(folder))
    for filename, phase, count in (("samples.csv", "measure", workload["iterations"]),
                                   ("warmup.csv", "warmup", workload["warmup"])):
        with open(folder / filename, newline="") as stream:
            reader = csv.DictReader(stream)
            if not {"phase", "index", "worker", "latency_ns", "attempts", "error"}.issubset(reader.fieldnames or []):
                raise ValueError("invalid raw schema: " + str(folder / filename))
            rows = list(reader)
        if len(rows) != count or any(row["phase"] != phase for row in rows):
            raise ValueError("invalid raw phase/count: " + str(folder / filename))
        if sorted(int(row["index"]) for row in rows) != list(range(count)):
            raise ValueError("duplicate/missing raw index: " + str(folder / filename))
        for row in rows:
            if (not 0 <= int(row["worker"]) < workload["workers"] or
                    int(row["latency_ns"]) <= 0 or int(row["attempts"]) < 1 or row["error"] != ""):
                raise ValueError("invalid/failed observation: " + str(folder / filename))


def interrupted(signum, _frame):
    raise KeyboardInterrupt("received signal " + str(signum))


def counterbalanced_orders(plan):
    arms = [a["name"] for a in plan["arms"]]
    n, blocks = len(arms), plan["blocks"]
    if "explicit_orders" not in plan:
        assert blocks % math.factorial(n) == 0, "exact counterbalancing required"
        return list(itertools.permutations(range(n))) * (blocks // math.factorial(n))
    orders = plan["explicit_orders"]
    assert len(orders) == blocks and blocks % 2 == 0
    assert all(len(order) == n and set(order) == set(arms) for order in orders)
    assert len({tuple(order) for order in orders}) == blocks
    for arm in arms:
        counts = [sum(order.index(arm) == position for order in orders) for position in range(n)]
        assert max(counts) - min(counts) <= 1, "unbalanced positions"
    for a, b in itertools.combinations(arms, 2):
        assert sum(order.index(a) < order.index(b) for order in orders) == blocks // 2, "unbalanced pairwise precedence"
    return [tuple(arms.index(arm) for arm in order) for order in orders]


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--plan", required=True)
    ap.add_argument("--output", required=True)
    ap.add_argument("--schedule-only", action="store_true")
    args = ap.parse_args()
    out = Path(args.output).resolve()
    out.mkdir(parents=True, exist_ok=False)
    plan = json.loads(Path(args.plan).read_text())
    n = len(plan["arms"])
    assert n >= 2 and plan["blocks"] >= 2
    assert len({a["name"] for a in plan["arms"]}) == n
    assert len({w["name"] for w in plan["workloads"]}) == len(plan["workloads"])
    if not plan["workloads"] or any(w["iterations"] <= 0 or w["warmup"] < 0 or w["workers"] <= 0 for w in plan["workloads"]):
        raise ValueError("nonempty workloads with positive iterations/workers and nonnegative warmup required")
    rng = random.Random(plan["seed"])
    order_by_workload = {}
    balanced_orders = counterbalanced_orders(plan)
    for w in plan["workloads"]:
        orders = list(balanced_orders)
        rng.shuffle(orders)
        order_by_workload[w["name"]] = orders
    schedule = []
    for block in range(plan["blocks"]):
        workloads = list(plan["workloads"])
        rng.shuffle(workloads)
        for w in workloads:
            for position, arm_i in enumerate(order_by_workload[w["name"]][block]):
                arm = plan["arms"][arm_i]
                run_id = f'b{block:03d}-{w["name"]}-p{position}-{arm["name"]}'
                values = dict(w, workload=w["name"], binary=arm["binary"],
                              output=str(out / run_id), run_id=run_id, block=block,
                              source_revision=plan["source_commit"],
                              harness_id=plan.get("harness_id", "unfrozen"))
                command = [v.format(**values) for v in arm["command"]]
                schedule.append(dict(run_id=run_id, block=block, position=position,
                                     arm=arm["name"], workload=w, command=command))
    if len({run["run_id"] for run in schedule}) != len(schedule):
        raise ValueError("duplicate run IDs")
    dump(out / "plan.json", plan)
    dump(out / "schedule.json", schedule)
    if args.schedule_only:
        print(f'{len(schedule)} prespecified runs in {out}')
        return 0
    pinned_paths = {a["binary"] for a in plan["arms"]} | set(plan.get("pinned_files", []))
    pinned = {path: sha(path) for path in sorted(pinned_paths)}
    dump(out / "binaries.json", pinned)
    dump(out / "controller.json", {"utc": utc(), "python": sys.version,
         "runner_sha256": sha(__file__), "argv": sys.argv,
         "affinity": sorted(os.sched_getaffinity(0)),
         "plan_sha256": sha(args.plan)})
    env = {"PATH": "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
           "HOME": str(Path.home())}
    env.update({k: str(v) for k, v in plan.get("environment", {}).items()})
    # Clean allowlisted environment: no inherited tracing knobs or credentials.
    dump(out / "benchmark-environment.json", env)
    completed = []
    p = None
    handlers = {sig: signal.signal(sig, interrupted) for sig in (signal.SIGTERM, signal.SIGINT)}
    try:
        for entry in schedule:
            check_builds()
            run = out / entry["run_id"]
            run.mkdir()
            dump(run / "invocation.json", entry)
            time.sleep(plan["cooldown_seconds"])
            gate = quiescence(plan)
            dump(run / "quiescence.json", gate)
            if not gate["passed"]:
                raise RuntimeError("quiescence CPU/steal threshold exceeded: " + entry["run_id"])
            snapshot(run / "before")
            started = utc()
            start = time.monotonic_ns()
            command = ["/usr/bin/time", "-v", "-o", str(run / "process-time.txt"), "--"] + entry["command"]
            with open(run / "stdout.txt", "w") as stdout, open(run / "stderr.txt", "w") as stderr, open(run / "telemetry.jsonl", "w") as samples:
                p = subprocess.Popen(command, env=env, stdout=stdout, stderr=stderr, start_new_session=True)
                deadline = time.monotonic() + plan["timeout_seconds"]
                while True:
                    samples.write(json.dumps(telemetry(p.pid), sort_keys=True) + "\n")
                    samples.flush()
                    try:
                        remaining = deadline - time.monotonic()
                        if remaining <= 0:
                            raise TimeoutError("workload process timeout: " + entry["run_id"])
                        p.wait(timeout=min(plan["telemetry_seconds"], remaining))
                        break
                    except subprocess.TimeoutExpired:
                        if time.monotonic() >= deadline:
                            raise TimeoutError("workload process timeout: " + entry["run_id"])
            elapsed = time.monotonic_ns() - start
            returncode = p.returncode
            stop_process_group(p)
            p = None
            snapshot(run / "after")
            result = dict(run_id=entry["run_id"], started_utc=started, ended_utc=utc(),
                          process_wall_ns=elapsed, returncode=returncode)
            dump(run / "result.json", result)
            if returncode == 0:
                validate_raw_output(run, entry["workload"])
            completed.append(result)
            dump(out / "progress.json", completed)
            print(f'{len(completed)}/{len(schedule)} {entry["run_id"]} rc={returncode}', flush=True)
            if returncode != 0:
                raise RuntimeError("workload failure: " + entry["run_id"])
        after = {name: sha(name) for name in pinned}
        if after != pinned:
            raise RuntimeError("binary changed during campaign")
        dump(out / "complete.json", {"utc": utc(), "scheduled": len(schedule),
             "completed": len(completed), "binaries": after})
        return 0
    except BaseException as exc:
        stop_process_group(p)
        hashes_after = {}
        for binary in pinned:
            try:
                hashes_after[binary] = sha(binary)
            except OSError as error:
                hashes_after[binary] = {"error": repr(error)}
        dump(out / "ABORTED.json", {"utc": utc(), "error": repr(exc),
             "scheduled": len(schedule), "completed": len(completed),
             "binaries_after": hashes_after, "binaries_unchanged": hashes_after == pinned})
        raise
    finally:
        stop_process_group(p)
        for sig, handler in handlers.items():
            signal.signal(sig, handler)


if __name__ == "__main__":
    sys.exit(main())
