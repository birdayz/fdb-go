#!/usr/bin/env python3
"""Run a preregistered counterbalanced FDB measurement plan, one process at a time.

Standard library only. Plan fields: seed, blocks (multiple of factorial(arms)),
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
import re
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
                 "gcc", "g++", "clang", "clang++", "rustc", "go", "apt", "apt-get", "dpkg", "perf", "strace"}
    found = [line for line in p.stdout.splitlines() if line.split()[-1] in forbidden or
             line.split()[-1].startswith(('candidate-', 'baseline-', 'clientbench'))]
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
    if total <= 0 or min(delta) < 0:
        raise ValueError('invalid CPU counter delta')
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


def process_record(entry, comm):
    result = {'pid': int(entry.name), 'comm': comm, 'errors': {}}
    for name in ('stat', 'status', 'io'):
        try:
            result[name] = (entry / name).read_text()
        except OSError as error:
            result['errors'][name] = str(error)
    return result


def descendants(pid):
    found, pending = set(), [pid]
    while pending:
        pid = pending.pop()
        if pid in found:
            continue
        found.add(pid)
        try:
            pending.extend(int(p) for p in Path(f'/proc/{pid}/task/{pid}/children').read_text().split())
        except OSError:
            pass
    return found


def telemetry(child_pid):
    result = {"utc": utc(), "monotonic_ns": time.monotonic_ns()}
    for path in ("/proc/stat", "/proc/loadavg", "/proc/pressure/cpu", "/proc/pressure/io",
                 "/proc/pressure/memory", "/proc/meminfo", "/proc/diskstats", "/proc/net/dev"):
        result[path] = Path(path).read_text()
    processes = []
    owned = descendants(child_pid)
    for entry in Path("/proc").iterdir():
        if not entry.name.isdecimal():
            continue
        try:
            comm = (entry / "comm").read_text().strip()
            if comm == "fdbserver" or int(entry.name) in owned or comm.startswith(('baseline-', 'candidate-', 'clientbench')):
                processes.append(process_record(entry, comm))
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


def validate_lifecycle(summary):
    life = summary.get('lifecycle', {})
    if (life.get('complete') is not True or life.get('measurement_barrier_complete') is not True or
            life.get('database_closed') is not True or
            life.get('network_stopped') is not summary['backend']['native_library']):
        raise ValueError('incomplete lifecycle accounting')
    for name in ('measurement_drain', 'shutdown_drain'):
        drain = life[name]
        before, after = drain['before'], drain['after']
        if (drain['complete'] is not True or not 2 <= drain['rounds'] <= 16 or drain['wall_ns'] <= 0 or
                any(type(v) is not int or v < 0 for counts in (before, after) for v in counts.values()) or
                any(after[k] < before[k] for k in before) or
                after['gc_cycles'] < before['gc_cycles'] + drain['rounds'] or
                after['finalizers_executed'] != after['finalizers_queued'] or
                after['cleanups_executed_estimate'] != after['cleanups_queued_estimate'] or
                after['cleanups_queued_estimate'] != before['cleanups_queued_estimate']):
            raise ValueError('incomplete finalizer fixpoint: ' + name)
    snapshots = [summary['stats_before'], summary['stats_after'], life['stats_after_measurement_drain'], life['stats_after_shutdown']]
    required = {'process_user_cpu_us', 'process_system_cpu_us', 'go_total_alloc_bytes', 'go_mallocs',
                'go_gc_cycles', 'go_gc_pause_ns', 'process_high_water_rss_kb'}
    if any(set(s) != required or any(type(v) is not int or v < 0 for v in s.values()) for s in snapshots):
        raise ValueError('invalid lifecycle resource counter')
    if any(after[k] < before[k] for before, after in zip(snapshots, snapshots[1:]) for k in required):
        raise ValueError('decreasing lifecycle resource counter')
    for drain, snapshot in [('measurement_drain', snapshots[2]), ('shutdown_drain', snapshots[3])]:
        if snapshot['go_gc_cycles'] < life[drain]['after']['gc_cycles']:
            raise ValueError('resource snapshot precedes finalizer drain')
    return life


def validate_raw_output(folder, workload):
    summary = json.loads((folder / "summary.json").read_text())
    if (summary.get("success") is not True or summary.get("persisted_validation") != "ok" or
            summary.get("requested") != workload["iterations"] or summary.get("completed") != workload["iterations"] or
            summary.get("errors") != 0 or summary.get("warmup_completed") != workload["warmup"] or
            summary.get("warmup_errors") != 0 or summary.get("measured_wall_ns", 0) <= 0):
        raise ValueError("invalid/incomplete summary or persisted validation: " + str(folder))
    validate_lifecycle(summary)
    for filename, phase, count in (("samples.csv", "measure", workload["iterations"]),
                                   ("warmup.csv", "warmup", workload["warmup"])):
        with open(folder / filename, newline="") as stream:
            reader = csv.DictReader(stream)
            if not {"phase", "index", "worker", "latency_ns", "attempts", "error", "check_status"}.issubset(reader.fieldnames or []):
                raise ValueError("invalid raw schema: " + str(folder / filename))
            rows = list(reader)
        if len(rows) != count or any(row["phase"] != phase for row in rows):
            raise ValueError("invalid raw phase/count: " + str(folder / filename))
        if sorted(int(row["index"]) for row in rows) != list(range(count)):
            raise ValueError("duplicate/missing raw index: " + str(folder / filename))
        for row in rows:
            if (not 0 <= int(row["worker"]) < workload["workers"] or
                    int(row["latency_ns"]) <= 0 or int(row["attempts"]) < 1 or row["error"] != "" or row['check_status'] != 'ok'):
                raise ValueError("invalid/failed observation: " + str(folder / filename))


def validate_identity(folder, run, plan, pins):
    manifest = json.loads((folder / 'manifest.json').read_text())
    summary = json.loads((folder / 'summary.json').read_text())
    arm = next(a for a in plan['arms'] if a['name'] == run['arm'])
    cfg, backend = manifest['config'], manifest['backend']
    expected = dict(workload=run['workload']['name'], iterations=run['workload']['iterations'],
                    warmup=run['workload']['warmup'], workers=run['workload']['workers'],
                    run_id=run['run_id'], block=run['block'], profile='none', allow_writes=True,
                    source_revision=arm.get('source_revision', plan['source_commit']), harness_id=plan['harness_id'])
    for flag, key, default in [('--transaction-timeout-ms', 'transaction_timeout_ms', 30000), ('--retry-limit', 'retry_limit', 10)]:
        expected[key] = int(run['command'][run['command'].index(flag) + 1]) if flag in run['command'] else default
    if backend['api_version'] != plan.get('fdb_api_version', 730):
        raise ValueError('FDB API version differs from plan')
    if any(cfg.get(key) != value for key, value in expected.items()) or summary['config'] != cfg:
        raise ValueError('run configuration differs from plan: ' + run['run_id'])
    if cfg['prefix'] != run['command'][run['command'].index('--prefix') + 1]:
        raise ValueError('run prefix differs from plan')
    if backend['executable'] != arm['binary'] or backend['executable_sha256'] != pins[arm['binary']]:
        raise ValueError('run binary differs from pinned arm')
    if summary['backend'] != backend:
        raise ValueError('manifest and summary backend disagree')
    native = run['arm'] == 'cgo'
    libraries = backend['loaded_fdb_libraries_sha256']
    if backend['backend'] != ('cgo' if native else 'go') or backend['native_library'] is not native or bool(libraries) != native:
        raise ValueError('backend/native linkage assertion failed')
    if native and set(libraries.values()) != {plan['native_library_sha256']}:
        raise ValueError('loaded native library differs from pinned library')
    if 'go_version' in plan and manifest['go_version'] != 'go' + plan['go_version'].removeprefix('go'):
        raise ValueError('Go toolchain differs from plan')
    if manifest['gomaxprocs'] != int(plan['environment']['GOMAXPROCS']):
        raise ValueError('GOMAXPROCS differs from plan')
    delta = summary['client_counters_delta']
    if delta['observation'] != ('unobserved' if native else 'observed'):
        raise ValueError('invalid client counter observation')
    if not native:
        counters = delta['values']
        if counters['grv_cache_hits'] != 0:
            raise ValueError('GRV cache used in uncached workload')
        versions, batches = counters['read_versions_completed'], counters['grv_successful_batch_replies']
        if cfg['workload'] == 'ryw':
            valid = versions == batches == 0
        else:
            valid = versions >= cfg['iterations'] and batches > 0
        if not valid:
            raise ValueError('invalid measured GRV evidence')


def interrupted(signum, _frame):
    raise KeyboardInterrupt("received signal " + str(signum))


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--plan", required=True)
    ap.add_argument("--output", required=True)
    ap.add_argument("--schedule-only", action="store_true")
    args = ap.parse_args()
    out = Path(args.output).resolve()
    plan = json.loads(Path(args.plan).read_text())
    campaign_deadline = time.monotonic() + plan.get('campaign_wall_cap_seconds', 180)
    schedule = make_schedule(plan, out)
    out.mkdir(parents=True, exist_ok=False)
    dump(out / "plan.json", plan)
    dump(out / "schedule.json", schedule)
    if args.schedule_only:
        print(f'{len(schedule)} prespecified runs in {out}')
        return 0
    return execute(plan, schedule, args.plan, out, campaign_deadline)


def make_schedule(plan, out):
    n = len(plan['arms'])
    if not 2 <= n <= 6 or plan['blocks'] < 2 or plan['blocks'] % math.factorial(n):
        raise ValueError('2..6 arms and exactly counterbalanced blocks required')
    if len({a['name'] for a in plan['arms']}) != n or 'cgo' not in {a['name'] for a in plan['arms']}:
        raise ValueError('unique arms including a CGo control required')
    if len({w['name'] for w in plan['workloads']}) != len(plan['workloads']):
        raise ValueError('duplicate workloads')
    if not plan['workloads'] or any(w['iterations'] <= 0 or w['warmup'] < 0 or w['workers'] <= 0 for w in plan['workloads']):
        raise ValueError('nonempty workloads with positive iterations/workers and nonnegative warmup required')
    pinned_paths = {a['binary'] for a in plan['arms']} | set(plan.get('pinned_files', []))
    pins = plan.get('expected_sha256')
    if (not isinstance(pins, dict) or set(pins) != pinned_paths or
            any(not isinstance(v, str) or not re.fullmatch(r'[0-9a-f]{64}', v) for v in pins.values())):
        raise ValueError('complete preregistered SHA256 pins required')
    artifact_hashes = {pins[p] for p in plan.get('pinned_files', [])}
    if plan.get('harness_id') not in artifact_hashes:
        raise ValueError('harness ID must identify a pinned artifact')
    if plan.get('native_library_sha256') not in artifact_hashes:
        raise ValueError('native library SHA256 must identify a pinned artifact')
    templates = []
    for arm in plan['arms']:
        command = list(arm['command'])
        if command.count('{binary}') != 1:
            raise ValueError('arm command must use the pinned {binary} token once')
        for flag, replacement in [('--prefix', '<prefix>'), ('--source-revision', '<declared-source>')]:
            if command.count(flag) != 1 or command.index(flag) + 1 >= len(command):
                raise ValueError('one command value required for ' + flag)
            index = command.index(flag) + 1
            if flag == '--source-revision' and command[index] not in ('{source_revision}', arm.get('source_revision', plan['source_commit'])):
                raise ValueError('command source revision differs from arm declaration')
            command[index] = replacement
        templates.append(command)
    if any(command != templates[0] for command in templates[1:]):
        raise ValueError('arms must have identical benchmark options and affinity')
    for obj in [*plan['arms'], *plan['workloads']]:
        if not re.fullmatch(r'[A-Za-z0-9_-]+', obj['name']):
            raise ValueError('unsafe arm/workload name')
    for key in ['timeout_seconds', 'quiescence_seconds', 'telemetry_seconds', 'campaign_wall_cap_seconds']:
        value = plan.get(key, 180) if key == 'campaign_wall_cap_seconds' else plan[key]
        if not math.isfinite(value) or value <= 0:
            raise ValueError('positive finite duration required: ' + key)
    if plan.get('campaign_wall_cap_seconds', 180) > 180:
        raise ValueError('campaign wall cap must not exceed 180 seconds')
    if not math.isfinite(plan['cooldown_seconds']) or plan['cooldown_seconds'] < 0:
        raise ValueError('nonnegative finite cooldown required')
    rng = random.Random(plan["seed"])
    order_by_workload = {}
    permutations = list(itertools.permutations(range(n)))
    for w in plan["workloads"]:
        orders = permutations * (plan["blocks"] // len(permutations))
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
                              source_revision=arm.get('source_revision', plan["source_commit"]),
                              harness_id=plan.get("harness_id", "unfrozen"))
                command = [v.format(**values) for v in arm["command"]]
                schedule.append(dict(run_id=run_id, block=block, position=position,
                                     arm=arm["name"], workload=w, command=command))
    if len({run["run_id"] for run in schedule}) != len(schedule):
        raise ValueError("duplicate run IDs")
    prefixes = [r['command'][r['command'].index('--prefix') + 1] for r in schedule]
    if len(prefixes) != len(set(prefixes)) or len({len(p.encode()) for p in prefixes}) != 1:
        raise ValueError('unique equal-length arm prefixes required')
    return schedule


def execute(plan, schedule, plan_path, out, campaign_deadline):
    pinned_paths = {a["binary"] for a in plan["arms"]} | set(plan.get("pinned_files", []))
    pinned = {path: sha(path) for path in sorted(pinned_paths)}
    dump(out / "binaries.json", pinned)
    if pinned != plan['expected_sha256']:
        raise ValueError('inputs differ from preregistered SHA256 pins')
    dump(out / "controller.json", {"utc": utc(), "python": sys.version,
         "runner_sha256": sha(__file__), "argv": sys.argv,
         "affinity": sorted(os.sched_getaffinity(0)),
         "plan_sha256": sha(plan_path)})
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
            if time.monotonic() + plan['cooldown_seconds'] + plan['quiescence_seconds'] >= campaign_deadline:
                raise TimeoutError('campaign wall cap reached before next run')
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
                deadline = min(campaign_deadline, time.monotonic() + plan["timeout_seconds"])
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
                validate_identity(run, entry, plan, pinned)
            completed.append(result)
            dump(out / "progress.json", completed)
            print(f'{len(completed)}/{len(schedule)} {entry["run_id"]} rc={returncode}', flush=True)
            if returncode != 0:
                raise RuntimeError("workload failure: " + entry["run_id"])
        after = {name: sha(name) for name in pinned}
        if after != pinned:
            raise RuntimeError("binary changed during campaign")
        if time.monotonic() >= campaign_deadline:
            raise TimeoutError('campaign wall cap exceeded')
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
