#!/usr/bin/env python3
"""Five-new-node horizontal capacity run. No effects without --execute-approved."""
import argparse
from concurrent.futures import ThreadPoolExecutor
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys
import threading
import time
import urllib.request

import policy as p

HERE = Path(__file__).resolve().parent


def loads(text):
    def pairs(items):
        value = {}
        for key, item in items:
            p.require(key not in value, "duplicate JSON key")
            value[key] = item
        return value
    def finite(value):
        result = float(value)
        p.require(p.number(result) or result < 0 and p.number(-result), "nonfinite JSON float")
        return result
    return json.loads(text, object_pairs_hook=pairs, parse_float=finite,
                      parse_constant=lambda _: p.require(False, "nonfinite JSON"))


def read(path):
    return loads(Path(path).read_text())


def write(path, value):
    path = Path(path)
    path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    with open(path, "x") as stream:
        os.chmod(path, 0o600)
        json.dump(value, stream, sort_keys=True)
        stream.write("\n")
        stream.flush()
        os.fsync(stream.fileno())


def remaining(plan, cap=30, *, reserve=0):
    left = plan["first_epoch"] + 6000 - time.time() - reserve
    p.require(left > 0, "100-minute controller budget exhausted; cleanup required")
    return min(cap, left)


def identity(expected):
    with urllib.request.urlopen("http://169.254.169.254/hetzner/v1/metadata/instance-id", timeout=2) as stream:
        actual = stream.read(128).decode().strip()
    p.require(actual == str(expected), "not the exact newly-created host")


def client_plan(d):
    p.require(d.get("schema") == "otel_horizontal_producer_v1" and type(d.get("node_id")) is int,
              "typed producer grant required")
    import re
    p.require(type(d.get("campaign")) is str and re.fullmatch(r"[a-z][a-z0-9-]{7,47}", d["campaign"]), "producer campaign")
    p.require(type(d.get("run")) is str and re_run(d["run"]), "sealed independent namespace")
    p.require(type(d.get("count")) is int and d["count"] in (100000, 1000000), "producer row grant")
    p.require(d.get("independent_store") is True and type(d.get("token")) is str and p.hexstr(d["token"]), "independent-store barrier grant")
    p.require(type(d.get("first_epoch")) is int and d.get("deadline_epoch") == d["first_epoch"] + 7200, "producer lifetime")
    p.require(d.get("gomaxprocs") == 4 and type(d["gomaxprocs"]) is int, "producer CPU cap")
    p.storage(d.get("storage_options"))
    base = "/srv/otel-horizontal/" + d["campaign"] + "/"
    for key in ("binary", "cluster_file", "work"):
        p.require(type(d.get(key)) is str and d[key].startswith(base) and ".." not in Path(d[key]).parts, "owned producer path")
    p.require(p.hexstr(d.get("binary_sha256")) and p.hexstr(d.get("source_revision"), 40), "producer binary/source seals")
    p.require(type(d.get("batch")) is int and d["batch"] in (32, 64)
              and type(d.get("workers")) is int and d["workers"] in (2, 4, 8), "producer bounds")
    return d


def re_run(run):
    import re
    return re.fullmatch(r"prof20k[0-9]{4}", run) is not None


def app_argv(d, mode):
    s = d["storage_options"]
    args = [d["binary"], "--backend", "fdb", "--order", "time", "--cluster-file", d["cluster_file"],
            "--run", d["run"], "--seed", "283", "--count", str(d["count"]), "--tenants", "1", "--attrs", "8",
            "--match-every", "100", "--batch", str(d["batch"]), "--save-mode", "batch", "--workers", str(d["workers"]),
            "--diagnostics", "--k", "100", "--max-candidates", "2000000", "--max-bytes", "268435456",
            "--revision", d["source_revision"], "--record-compression", s["record_compression"],
            "--posting-value-format", s["posting_value_format"], "--bulk-index-conflicts=" + str(s["bulk_index_conflicts"]).lower(),
            "--hash-bytes", str(s["hash_bytes"]), "--mode", mode, "--timeout", "300s" if mode == "ingest" else "120s",
            "--output", str(Path(d["work"]) / (mode + ".json"))]
    if mode == "query":
        args += ["--suite", "all", "--iterations", "1600"]
    return args


def execute_app(d, mode):
    identity(d["node_id"])
    p.require(p.sha(d["binary"]) == d["binary_sha256"], "remote application binary changed")
    work = Path(d["work"])
    args = ["systemd-run", "--scope", "--quiet", "--collect", "--unit=otel-horizontal-" + d["run"] + "-" + mode,
            "--slice=horizontal-client.slice", "--property=CPUQuota=400%", "--property=MemoryMax=17179869184",
            "--property=AllowedCPUs=0-3", "env", "GOMAXPROCS=4", *app_argv(d, mode)]
    write(work / (mode + ".command.json"), {"argv": args, "grant_sha256": p.sha(work / "plan.json")})
    started = time.monotonic_ns()
    with open(work / (mode + ".stdout"), "xb") as stdout, open(work / (mode + ".stderr"), "xb") as stderr:
        child = subprocess.Popen(args, stdout=stdout, stderr=stderr)
        limit = time.monotonic() + remaining(d, 300 if mode == "ingest" else 120)
        try:
            while child.poll() is None:
                p.require(time.monotonic() < limit and not (work / "abort.json").exists(), "producer stage aborted/expired")
                time.sleep(0.1)
            code = child.wait()
        except BaseException:
            child.terminate()
            try:
                child.wait(timeout=3)
            except subprocess.TimeoutExpired:
                child.kill()
                child.wait(timeout=3)
            raise
    write(work / (mode + ".exit.json"), {"exit": code, "started_ns": started, "ended_ns": time.monotonic_ns()})
    p.require(code == 0, "application stage failed: " + mode)
    return read(work / (mode + ".json"))


def producer(path, verify=False):
    d = client_plan(read(path))
    identity(d["node_id"])
    work = Path(d["work"])
    p.require(p.sha(path) == p.sha(work / "plan.json"), "producer grant path mismatch")
    if verify:
        for mode in ("audit", "query"):
            execute_app(d, mode)
        return
    p.require(p.sha(d["binary"]) == d["binary_sha256"], "producer binary seal")
    write(work / "ready.json", {"token": d["token"], "node_id": d["node_id"], "run": d["run"], "grant_sha256": p.sha(path)})
    until = time.monotonic() + remaining(d, 60)
    while not (work / "release.json").exists():
        p.require(time.monotonic() < until and not (work / "abort.json").exists(), "barrier timeout/abort")
        time.sleep(0.05)
    release = read(work / "release.json")
    p.require(release == {"token": d["token"], "grant_sha256": p.sha(path)}, "wrong barrier release")
    execute_app(d, "ingest")


def observe(node_id, root):
    identity(node_id)
    root = Path(root)
    p.require(str(root).startswith("/srv/otel-horizontal/") and ".." not in root.parts, "owned observation path")
    mem = dict(line.split(":", 1) for line in Path("/proc/meminfo").read_text().splitlines())
    vm = dict(line.split() for line in Path("/proc/vmstat").read_text().splitlines())
    pressure = {}
    for kind in ("cpu", "memory", "io"):
        for line in Path("/proc/pressure/" + kind).read_text().splitlines():
            fields = line.split()
            pressure[kind + "_" + fields[0]] = float(dict(x.split("=", 1) for x in fields[1:])["avg10"])
    fs = os.statvfs(root)
    du = subprocess.run(["du", "-sx", "-B1", str(root)], capture_output=True, text=True, check=True, timeout=5)
    filesystem = subprocess.run(["stat", "-f", "-c", "%T", str(root)], capture_output=True, text=True, check=True, timeout=5).stdout.strip()
    slices = {}
    for name in ("horizontal-fdb.slice", "horizontal-client.slice"):
        group = subprocess.run(["systemctl", "show", name, "--property=ControlGroup", "--value"], capture_output=True, text=True, timeout=5)
        if group.returncode == 0 and group.stdout.strip():
            location = (Path("/sys/fs/cgroup") / group.stdout.strip().lstrip("/")).resolve()
            p.require(location.is_relative_to("/sys/fs/cgroup"), "cgroup path escaped")
            slices[name] = {key: (location / key).read_text().strip() for key in
                            ("cpu.max", "cpuset.cpus.effective", "memory.max", "memory.swap.max", "memory.current", "memory.events")}
    processes = {}
    for entry in Path("/proc").iterdir():
        if entry.name.isdigit():
            try:
                if (entry / "comm").read_text().strip() == "fdbserver":
                    processes[entry.name] = (entry / "stat").read_text().rsplit(")", 1)[1].split()[19]
            except FileNotFoundError:
                continue
    return {"node_id": node_id, "epoch": time.time(), "mem_available_bytes": int(mem["MemAvailable"].split()[0]) * 1024,
            "pswpin": int(vm["pswpin"]), "pswpout": int(vm["pswpout"]), "pressure": pressure,
            "total_bytes": fs.f_frsize * fs.f_blocks, "available_bytes": fs.f_frsize * fs.f_bavail,
            "allocated_bytes": int(du.stdout.split()[0]), "filesystem": filesystem, "slices": slices, "fdb_pids": processes}


class Campaign:
    def __init__(self, plan, out, provision, *, clock=time.monotonic_ns, sleep=time.sleep):
        self.plan, self.out, self.provision = plan, Path(out), provision
        self.clock, self.sleep = clock, sleep
        self.nodes, self.resources = {}, None
        self.sequence = 0
        self.sequence_lock = threading.Lock()
        self.backlog_streak = 0
        self.background = None
        self.observation_history = {}
        self.layouts = []
        self.pid_baseline = None

    def receipt_budget(self):
        size = sum(x.stat().st_size for x in self.out.rglob("*") if x.is_file())
        p.require(size < 1024 ** 3, "controller receipt allocation exceeds 1GiB")

    def command(self, node, args, cap=30):
        self.receipt_budget()
        with self.sequence_lock:
            self.sequence += 1
            sequence = self.sequence
        rc, stdout, stderr = self.provision.run_on_node(self.plan, self.out / "provision", node, args,
                                                       timeout=remaining(self.plan, cap))
        write(self.out / "commands" / (str(sequence) + ".json"), {"node_id": node["id"], "argv": args,
              "exit": rc, "stdout": stdout, "stderr": stderr, "controller_ns": self.clock()})
        self.receipt_budget()
        p.require(rc == 0, "owned-node command failed")
        return stdout

    def remote(self, node, mode, path):
        return ["python3", self.remote_harness + "/driver.py", mode, "--path", str(path), "--node-id", str(node["id"])]

    def read_remote(self, node, path):
        return loads(self.command(node, self.remote(node, "_read", path)))

    def collect(self, node, grant, directory):
        value = loads(self.command(node, self.remote(node, "_collect", grant["work"] + "/plan.json")))
        write(directory / (grant["run"] + "-receipts.json"), value)

    def host_samples(self, initial=False):
        samples = {}
        for name, node in self.nodes.items():
            raw = self.command(node, ["python3", self.remote_harness + "/driver.py", "_observe", "--node-id", str(node["id"]), "--path", self.remote_root])
            value = loads(raw)
            total, available = value["total_bytes"], value["available_bytes"]
            floor = ((200 * p.GIB * 100 + 6 * total + 409599) // 409600) * 4096
            p.require(value["filesystem"] in ("ext2/ext3", "ext4") and value["mem_available_bytes"] >= 8 * p.GIB, "native filesystem/host memory")
            if name.startswith("node"):
                p.require(available >= floor + (64 * p.GIB if initial else 0), "unchanged storage free-space floor")
                p.require(value["allocated_bytes"] < 64 * p.GIB, "observed allocation cap")
            slice_name = "horizontal-fdb.slice" if name.startswith("node") else "horizontal-client.slice"
            limits = value["slices"][slice_name]
            quota, period = map(int, limits["cpu.max"].split())
            events = dict(line.split() for line in limits["memory.events"].splitlines())
            p.require(quota == period * 4 and limits["cpuset.cpus.effective"] == "0-3"
                      and limits["memory.max"] == "17179869184" and limits["memory.swap.max"] == "0"
                      and int(limits["memory.current"]) < 16 * p.GIB * 9 // 10
                      and int(events["oom"]) == 0 and int(events["oom_kill"]) == 0, "unchanged native cgroup budget/OOM")
            if name.startswith("client"):
                p.require(not value["fdb_pids"], "client host must not run fdbserver")
            elif self.pid_baseline is not None:
                p.require(value["fdb_pids"] == self.pid_baseline[name], "FDB process PID/starttime changed")
            previous = self.observation_history.get(name)
            if previous:
                p.require(value["pswpin"] == previous["pswpin"] and value["pswpout"] == previous["pswpout"], "host swap activity")
            self.observation_history[name] = value
            samples[name] = value
        return samples

    def quiet(self):
        samples = []
        next_sample = self.clock()
        for i in range(7):
            if i:
                delay = (next_sample - self.clock()) / 10 ** 9
                if delay > 0:
                    self.sleep(delay)
            values = self.host_samples(initial=True)
            for value in values.values():
                q = value["pressure"]
                p.require(value["mem_available_bytes"] >= 24 * p.GIB and q["cpu_some"] <= 5
                          and q["memory_some"] <= 0.1 and q["memory_full"] <= 0.01 and q["io_full"] <= 1,
                          "unchanged host quiet admission")
            samples.append(values)
            # Include SSH time and leave jitter margin inside the 10–15s guard.
            next_sample += 12 * 10 ** 9
        for name in self.nodes:
            memory = [s[name]["mem_available_bytes"] for s in samples]
            p.require(max(memory) - min(memory) <= p.GIB, "quiet memory variation")
            p.require(all(10 <= b[name]["epoch"] - a[name]["epoch"] <= 15 for a, b in zip(samples, samples[1:])),
                      "quiet sample intervals outside 10–15 seconds")
        write(self.out / "quiet.json", {"samples": samples})

    def status(self, layout):
        raw = self.command(self.nodes["node1"], [self.remote_root + "/bin/fdbcli", "-C", layout["remote_cluster_file"], "--exec", "status json"], 15)
        document = loads(raw)
        write(self.out / "status" / (str(self.sequence) + ".json"), {"raw": document, "controller_ns": self.clock()})
        return p.strict_status(document, layout)

    def monitor(self, layout):
        self.host_samples()
        return self.backlog(self.status(layout))

    def backlog(self, sample):
        queue, lag = sample["queue_bytes"], sample["lag_seconds"]
        p.require(queue < 1.5 * p.GIB and lag < 90, "hard native backlog stop")
        self.backlog_streak = self.backlog_streak + 1 if queue >= 512 * 1024 ** 2 and lag >= 60 else 0
        p.require(self.backlog_streak < 6, "six consecutive native backlog violations")
        return sample

    def stable(self, layout, cap=180, *, startup=False):
        limit, streak = time.monotonic() + remaining(self.plan, cap), 0
        last = None
        while time.monotonic() < limit:
            if startup:
                self.host_samples()  # Host limits remain hard failures during initial recovery.
                try:
                    last = self.status(layout)
                except (ValueError, KeyError, TypeError) as exc:
                    write(self.out / "startup" / (str(self.clock()) + ".json"), {"strict_pass": False, "error": str(exc)})
                    streak = 0
                    self.sleep(5)
                    continue
                last = self.backlog(last)
            else:
                last = self.monitor(layout)
            streak = streak + 1 if last["queue_bytes"] < 16777216 and last["lag_seconds"] < 10 else 0
            if streak == 3:
                return last
            self.sleep(5)
        raise ValueError("strict native readiness/drain cap exhausted")

    def install_helpers(self):
        self.remote_root = "/srv/otel-horizontal/" + self.plan["campaign"]
        self.remote_harness = self.remote_root + "/harness"
        for node in self.nodes.values():
            self.command(node, ["mkdir", "-p", self.remote_harness, self.remote_root + "/producers"])
            for name in ("driver.py", "policy.py"):
                rc, _, _ = self.provision.copy_to_node(self.plan, self.out / "provision", node, HERE / name, self.remote_harness + "/" + name)
                p.require(rc == 0, "copy sealed helper failed")
                result = self.command(node, ["sha256sum", self.remote_harness + "/" + name])
                p.require(result.split()[0] == p.sha(HERE / name), "remote helper hash")

    def wave(self, layout, hosts, count, number):
        remaining(self.plan, reserve=1500)
        directory = self.out / ("hosts%d-wave%d-count%d" % (hosts, number, count))
        directory.mkdir(mode=0o700)
        before = self.stable(layout)
        jobs = []
        during, background, verify_jobs = [], [], []
        try:
            for i, name in enumerate(("client1", "client2")):
                node = self.nodes[name]
                run = "prof20k%04d" % (9000 + number * 2 + i)
                work = self.remote_root + "/producers/" + run
                token = hashlib.sha256((self.plan["campaign"] + run).encode()).hexdigest()
                grant = {**{k: self.plan[k] for k in ("campaign", "first_epoch", "deadline_epoch", "binary_sha256", "source_revision", "batch", "workers", "gomaxprocs", "storage_options")},
                         "schema": "otel_horizontal_producer_v1", "node_id": node["id"], "run": run, "count": count,
                         "independent_store": True, "token": token, "binary": self.remote_root + "/bin/otel-demo",
                         "cluster_file": layout["remote_cluster_file"], "work": work}
                client_plan(grant)
                self.command(node, ["mkdir", work])
                local = directory / (name + "-plan.json")
                write(local, grant)
                rc, _, _ = self.provision.copy_to_node(self.plan, self.out / "provision", node, local, work + "/plan.json")
                p.require(rc == 0, "copy producer grant failed")
                # Every client's cluster file is copied independently and hash checked.
                self.command(node, ["mkdir", "-p", str(Path(layout["remote_cluster_file"]).parent)])
                rc, _, _ = self.provision.copy_to_node(self.plan, self.out / "provision", node, layout["cluster_file"], layout["remote_cluster_file"])
                p.require(rc == 0, "copy client cluster file failed")
                cluster_hash = self.command(node, ["sha256sum", layout["remote_cluster_file"]])
                p.require(cluster_hash.split()[0] == p.sha(layout["cluster_file"]), "client cluster file hash changed")
                command = self.provision.ssh_argv(self.plan, self.out / "provision", node, self.remote(node, "_produce", work + "/plan.json"))
                stdout, stderr = open(directory / (name + ".stdout"), "xb"), open(directory / (name + ".stderr"), "xb")
                process = subprocess.Popen(command, stdout=stdout, stderr=stderr)
                jobs.append((node, grant, local, process, stdout, stderr))
            ready_until = time.monotonic() + remaining(self.plan, 60)
            for node, grant, local, _, _, _ in jobs:
                while time.monotonic() < ready_until:
                    # test -f is an expected nonzero while the remote helper starts.
                    rc, _, _ = self.provision.run_on_node(self.plan, self.out / "provision", node,
                                                         ["test", "-f", grant["work"] + "/ready.json"], timeout=remaining(self.plan, 5))
                    if rc == 0:
                        ready = self.read_remote(node, grant["work"] + "/ready.json")
                        p.require(ready == {"token": grant["token"], "node_id": node["id"], "run": grant["run"], "grant_sha256": p.sha(local)}, "producer ready binding")
                        break
                    self.sleep(0.1)
                else:
                    raise ValueError("two-producer ready barrier expired")
            started = self.clock()
            with ThreadPoolExecutor(max_workers=2) as executor:
                list(executor.map(lambda job: self.command(job[0], self.remote(job[0], "_release", job[1]["work"] + "/plan.json")), jobs))
            limit = time.monotonic() + remaining(self.plan, 300)
            while any(job[3].poll() is None for job in jobs):
                p.require(time.monotonic() < limit, "common load cap")
                sample = self.monitor(layout)
                during.append(sample)
                self.sleep(1)
            ended = self.clock()
            for node, grant, _, process, _, _ in jobs:
                p.require(process.wait(timeout=2) == 0, "producer failed")
                result = self.read_remote(node, grant["work"] + "/ingest.json")
                p.evidence(result, self.plan, grant["run"], count, "ingest")
                write(directory / (grant["run"] + "-ingest.json"), result)
            background.append(self.stable(layout, 300))
            for node, grant, local, _, _, _ in jobs:
                command = self.provision.ssh_argv(self.plan, self.out / "provision", node, self.remote(node, "_verify", grant["work"] + "/plan.json"))
                stdout, stderr = open(directory / (grant["run"] + "-verify.stdout"), "xb"), open(directory / (grant["run"] + "-verify.stderr"), "xb")
                process = subprocess.Popen(command, stdout=stdout, stderr=stderr)
                verify_jobs.append((node, grant, local, process, stdout, stderr))
            verify_limit = time.monotonic() + remaining(self.plan, 245)
            while any(job[3].poll() is None for job in verify_jobs):
                p.require(time.monotonic() < verify_limit, "audit/oracle cap")
                self.monitor(layout)
                self.sleep(1)
            for node, grant, _, process, _, _ in verify_jobs:
                p.require(process.wait(timeout=2) == 0, "audit/oracle process failed")
                for mode in ("audit", "query"):
                    result = self.read_remote(node, grant["work"] + "/" + mode + ".json")
                    p.evidence(result, self.plan, grant["run"], count, mode)
                    write(directory / (grant["run"] + "-" + mode + ".json"), result)
                self.collect(node, grant, directory)
            background.append(self.stable(layout))
            proof = p.distributed(before, during, layout["storage_hosts"], background)
            result = {"storage_hosts": hosts, "count_per_producer": count, "independent_stores": 2, "rows": 2 * count,
                      "controller_started_ns": started, "controller_ended_ns": ended,
                      "ack_rows_per_second": p.common_throughput(2 * count, started, ended),
                      "timing_boundary": "controller-observed common wall includes release, producer startup, SSH completion and observation overhead",
                      "cost": p.topology_cost(self.resources, hosts, p.common_throughput(2 * count, started, ended)), "healthy": True,
                      "both_censuses": True, "both_oracles": True, "before": before, "during": during,
                      "post_drain": background, "data_processing": proof, "replication": "single", "ha": False}
            write(directory / "result.json", result)
            return result
        finally:
            for node, grant, _, process, stdout, stderr in jobs + verify_jobs:
                if process.poll() is None:
                    try:
                        self.command(node, self.remote(node, "_abort", grant["work"] + "/plan.json"), 5)
                        process.wait(timeout=8)
                    except (ValueError, subprocess.TimeoutExpired):
                        process.terminate()
                        process.wait(timeout=3)
                stdout.close()
                stderr.close()
            for node, grant, _, _, _, _ in jobs:
                if not (directory / (grant["run"] + "-receipts.json")).exists():
                    try:
                        self.collect(node, grant, directory)
                    except (ValueError, OSError, subprocess.SubprocessError):
                        pass  # Raw SSH logs remain local even if a failed host cannot return its journal.

    def run(self):
        p.validate_plan(self.plan)
        source_before = p.verify_source(self.plan)
        self.out.mkdir(mode=0o700, parents=True, exist_ok=False)
        write(self.out / "approved-plan.json", self.plan)
        write(self.out / "source-before.json", source_before)
        results = []
        try:
            self.resources = self.provision.prepare(self.plan, self.out / "provision")
            self.nodes = self.resources["nodes"]
            p.require(set(self.nodes) == {"client1", "client2", "node1", "node2", "node3"}, "exact five newly created nodes")
            self.install_helpers()
            self.quiet()
            gates = {}
            # Complete every horizontal prerequisite before a possibly failing 1M
            # arm can end the campaign. Measured arms use fresh native clusters.
            for phase in ("prerequisite", "measured"):
                order = self.plan.get("prerequisite_topologies", self.plan["topologies"]) if phase == "prerequisite" else self.plan["measured_topologies"]
                for i, hosts in enumerate(order):
                    remaining(self.plan, reserve=2000)
                    if phase == "measured":
                        p.require_promotion(gates[hosts], hosts)
                    armid = "hosts%d-%s" % (hosts, phase)
                    layout = self.provision.setup_cluster(self.plan, self.out / "provision", self.nodes, hosts, armid)
                    self.layouts.append(layout)
                    self.pid_baseline = None
                    processes = self.host_samples()
                    self.pid_baseline = {name: value["fdb_pids"] for name, value in processes.items() if name.startswith("node")}
                    self.stable(layout, startup=True)
                    count = 100000 if phase == "prerequisite" else self.plan["measured_count"]
                    result = self.wave(layout, hosts, count, i * 2 + (phase == "measured"))
                    if phase == "prerequisite":
                        p.require_promotion(result, hosts)
                        gates[hosts] = result
                    else:
                        results.append(result)
            write(self.out / "measured.json", {"results": results, "inserted_rows": p.authorized_rows(self.plan),
                  "boundary": "two independent stores; native single-replica capacity only; no shared-corpus or HA claim; counters include DD/background"})
        finally:
            # Partial prepare failures clean themselves. The root-owned separately
            # tracked supervisor supplies independence; provision's child is auxiliary.
            if self.resources is not None:
                cleanup = self.provision.teardown(self.plan, self.out / "provision", self.resources)
                write(self.out / "cleanup.json", cleanup)
                write(self.out / "actual-bill-bound.json", p.bill_bound(self.resources, cleanup))
                write(self.out / "rounded-server-bill.json", p.rounded_server_bill(self.resources, cleanup))
                p.require(cleanup["passed"], "exact ten-kind baseline cleanup did not pass")
            write(self.out / "source-after.json", p.verify_source(self.plan))


def main(argv=None):
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("mode", choices=("run", "_produce", "_verify", "_release", "_abort", "_read", "_observe", "_collect"))
    ap.add_argument("--plan")
    ap.add_argument("--output")
    ap.add_argument("--path")
    ap.add_argument("--node-id", type=int)
    ap.add_argument("--execute-approved", action="store_true")
    a = ap.parse_args(argv)
    if a.mode == "run":
        plan = read(a.plan)
        p.validate_plan(plan)
        if not a.execute_approved:
            print(json.dumps({"validated": True, "nodes": 5, "rows": p.authorized_rows(plan), "replication": "single", "provisioned": False}))
            return
        import provision
        Campaign(plan, a.output, provision).run()
    elif a.mode == "_observe":
        print(json.dumps(observe(a.node_id, a.path), sort_keys=True))
    elif a.mode == "_read":
        identity(a.node_id)
        p.require(str(a.path).startswith("/srv/otel-horizontal/") and ".." not in Path(a.path).parts, "owned receipt path")
        print(json.dumps(read(a.path), sort_keys=True))
    else:
        grant = client_plan(read(a.path))
        p.require(grant["node_id"] == a.node_id, "wrong producer host")
        identity(a.node_id)
        if a.mode in ("_produce", "_verify"):
            producer(a.path, verify=a.mode == "_verify")
        elif a.mode == "_collect":
            artifacts = {}
            for mode in ("ingest", "audit", "query"):
                for suffix in ("json", "command.json", "exit.json", "stdout", "stderr"):
                    path = Path(grant["work"]) / (mode + "." + suffix)
                    if path.exists():
                        p.require(path.is_file() and not path.is_symlink() and path.stat().st_size < 16 * 1024 ** 2,
                                  "remote receipt size/type")
                        artifacts[path.name] = path.read_text()
            print(json.dumps({"run": grant["run"], "node_id": a.node_id, "artifacts": artifacts}, sort_keys=True))
        else:
            name = "release.json" if a.mode == "_release" else "abort.json"
            write(Path(grant["work"]) / name, {"token": grant["token"], "grant_sha256": p.sha(a.path)})


if __name__ == "__main__":
    main()
