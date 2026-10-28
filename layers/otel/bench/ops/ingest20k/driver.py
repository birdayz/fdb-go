#!/usr/bin/env python3
"""Explicitly approved, foreground, fresh-only COW trial. Import is inert."""
import argparse
import fcntl
import hashlib
import json
import os
from pathlib import Path
import re
import signal
import stat
import subprocess
import sys
import time
import uuid

sys.dont_write_bytecode = True
import policy as p

HERE = Path(__file__).resolve().parent
P = {}
OLD = {
    "otel283-fdb4-vxnar": "d04595f1a404f0245574fc39b9c7922794fd5b43f39470c03e1e03836f997e06",
    "otel283-ch16-vxnar": "c350fc38ea191a134df09947386b691b8ecd232cf21b289ac3d3cec158487844",
    "otel283-ch16-resume02": "7df4e34f3c11cd4a751c492194d6221accebec302afe993493446e1574c66762",
    "otel283-ch16-resume03": "19d145ce807487b3e4258c856a6271e17faabc907e7c88d4a0ec0fcee77480a1",
}
LABELS = {}

# Production callers supply fixed container paths; offline fixtures supply private trees.
MEMORY_SCRIPT = 'set -euo pipefail; cat "$1/memory.current"; cat "$1/memory.events"; code=0; pids=$(pgrep -x fdbserver) || code=$?; (( code <= 1 )); printf "%s\\n" "$pids" | sort -n'
COUNTERS_SCRIPT = 'set -euo pipefail; membership=$(cat "$2"); [[ "$membership" == 0::/ ]]; [[ -r "$1/cgroup.controllers" ]]; printf "scope=whole_container_including_bootstrap_DD_monitor_and_workload\\nFILE=%s\\n%s\\n" "$2" "$membership"; for f in cpu.stat io.stat; do printf "FILE=%s/%s\\n" "$1" "$f"; cat "$1/$f"; done'
LAYOUT_SCRIPT = 'set -euo pipefail; [[ $(cat "$1/.otel283-owner") == "$3" ]]; [[ $(sha256sum "$1/cluster/fdb.cluster" | cut -d" " -f1) == "$2" ]]; for port in 14883 14884 14885 14886; do [[ -s "$1/processes/$port/processId" ]]; done; existing=$(find "$1/processes" -type f \\( -name "storage-*.redwood-v1" -o -name "log2-*.sqlite*" -o -name "logqueue-*.fdq" \\) -print -quit); [[ -z "$existing" ]]'


def select_arm(approved):
    global P, LABELS
    P = p.arm(approved["run"], approved["batch"], approved["workers"], approved["count"])
    LABELS = {"otel283.owner": P["owner"], "otel283.profile": P["profile"], "otel283.session": P["session"], "otel283.run": P["run"]}

INSPECT = '{"id":{{json .Id}},"name":{{json .Name}},"running":{{json .State.Running}},"status":{{json .State.Status}},"exit_code":{{json .State.ExitCode}},"oom":{{json .State.OOMKilled}},"restarts":{{json .RestartCount}},"pid":{{json .State.Pid}},"image":{{json .Config.Image}},"labels":{{json .Config.Labels}},"memory":{{json .HostConfig.Memory}},"swap":{{json .HostConfig.MemorySwap}},"cpu":{{json .HostConfig.NanoCpus}},"network":{{json .HostConfig.NetworkMode}},"ports":{{json .HostConfig.PortBindings}},"mounts":{{json .Mounts}},"logdriver":{{json .HostConfig.LogConfig.Type}},"cmd":{{json .Config.Cmd}}}'


def write(path, value, exclusive=True):
    text = json.dumps(value, sort_keys=True) + "\n"
    if exclusive and path.name == "complete.json":
        temp = path.with_name(".complete.json." + uuid.uuid4().hex + ".pending")
        published = False
        try:
            with open(temp, "x") as stream:
                stream.write(text)
                stream.flush()
                os.fsync(stream.fileno())
            # Hard-link publication is atomic and refuses an existing marker.
            os.link(temp, path)
            published = True
        finally:
            try:
                temp.unlink(missing_ok=True)
            except OSError:
                if published:
                    path.unlink()
                raise
    elif exclusive:
        with open(path, "x") as stream:
            stream.write(text)
            stream.flush()
            os.fsync(stream.fileno())
    else:
        temp = path.with_suffix(path.suffix + ".pending")
        with open(temp, "w") as stream:
            stream.write(text)
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(temp, path)


def read(path):
    return p.loads(Path(path).read_text())


def allocated_tree(root):
    total = 0
    def failed(error):
        raise error
    for directory, dirs, files in os.walk(root, followlinks=False, onerror=failed):
        for name in ["."] + dirs + files:
            path = Path(directory) if name == "." else Path(directory) / name
            info = path.lstat()
            p.require(not stat.S_ISLNK(info.st_mode), "symlink in receipt root")
            # Directories are counted once as '.', not again in their parent.
            if name == "." or name in files:
                total += info.st_blocks * 512
    return total


def final_inventory(root, commitments):
    """Seal every retained output only after all output writers have joined."""
    root = Path(root)
    for name, expected in commitments.items():
        path = root / name
        p.require(path.is_file() and not path.is_symlink() and p.sha(path) == expected,
                  "validated receipt changed/missing: " + name)
    rows = []
    def failed(error):
        raise error
    for directory, dirs, files in os.walk(root, followlinks=False, onerror=failed):
        for name in dirs:
            p.require(not (Path(directory) / name).is_symlink(), "symlink in final inventory")
        for name in files:
            path = Path(directory) / name
            relative = str(path.relative_to(root))
            p.require(relative not in ("final-inventory.json", "complete.json"), "completion output already exists")
            info = path.lstat()
            p.require(stat.S_ISREG(info.st_mode), "nonregular final receipt")
            rows.append({"path": relative, "size": info.st_size, "sha256": p.sha(path)})
            if name.endswith(".exit.json"):
                receipt = read(path)
                for field, suffix in (("stdout_sha256", ".stdout"), ("stderr_sha256", ".stderr")):
                    if field in receipt:
                        raw = path.with_name(name[:-len(".exit.json")] + suffix)
                        p.require(raw.is_file() and not raw.is_symlink() and p.sha(raw) == receipt[field],
                                  "raw command receipt changed/missing")
    p.require(rows and set(commitments) <= {row["path"] for row in rows}, "empty/incomplete final inventory")
    rows.sort(key=lambda row: row["path"])
    # Re-read the population and bytes before publishing the commitment.
    current = sorted(str(path.relative_to(root)) for path in root.rglob("*") if not path.is_dir())
    p.require(current == [row["path"] for row in rows], "final inventory population changed")
    for row in rows:
        path = root / row["path"]
        info = path.lstat()
        p.require(stat.S_ISREG(info.st_mode) and info.st_size == row["size"] and p.sha(path) == row["sha256"],
                  "final inventory bytes changed")
    return {"files": rows, "count": len(rows)}


def host_sample():
    fields = dict(line.split(":", 1) for line in Path("/proc/meminfo").read_text().splitlines())
    vm = dict(line.split() for line in Path("/proc/vmstat").read_text().splitlines())
    result = {"epoch": int(time.time()), "mem_available_bytes": int(fields["MemAvailable"].split()[0]) * 1024,
              "pswpin": int(vm["pswpin"]), "pswpout": int(vm["pswpout"])}
    for name, kind, key in (("cpu", "some", "cpu_psi_some_avg10"), ("memory", "some", "memory_psi_some_avg10"),
                            ("memory", "full", "memory_psi_full_avg10"), ("io", "full", "io_psi_full_avg10")):
        lines = [line.split() for line in Path("/proc/pressure/" + name).read_text().splitlines()]
        values = [dict(pair.split("=") for pair in line[1:]) for line in lines if line[0] == kind]
        p.require(len(values) == 1, "missing PSI accounting")
        result[key] = float(values[0]["avg10"])
    return result


def lease_path(environment=None):
    environment = os.environ if environment is None else environment
    value = environment.get("FDB_LANE_LOCK") or str(Path(environment.get("TMPDIR") or "/tmp") / "fdb-heavy-lane.lock")
    return Path(value).absolute()


def check_lease(fd, lease):
    info = os.fstat(fd)
    path = Path(lease["path"])
    p.require(path.is_absolute(), "global lease path must be absolute")
    current = path.lstat()
    expected = (lease["device"], lease["inode"])
    p.require(stat.S_ISREG(info.st_mode) and stat.S_ISREG(current.st_mode)
              and info.st_nlink > 0 and current.st_nlink > 0
              and (info.st_dev, info.st_ino) == expected == (current.st_dev, current.st_ino),
              "global lease descriptor/path identity lost")


def parent_start(pid):
    return Path(f"/proc/{pid}/stat").read_text().rsplit(")", 1)[1].split()[19]


class Runtime:
    def __init__(self, out):
        os.umask(0o077)
        self.out = Path(out)
        info = self.out.stat()
        p.require(stat.S_IMODE(info.st_mode) == 0o700 and info.st_uid == os.getuid(), "private owned 0700 runtime root required")
        self.request = read(self.out / "request.json")
        self.request_sha256 = p.sha(self.out / "request.json")
        self.approval = read(self.request["approval"])
        p.approval(self.approval)
        select_arm(self.approval)
        p.require(str(self.out) == self.approval["out"], "sealed output path mismatch")
        self.nonce = self.request["nonce"]
        instance_file = self.out / "instance.json"
        self.instance = read(instance_file)["instance"] if instance_file.exists() else ""
        self.sequence = 0
        self.initial_pids = None
        self.backlog = 0
        self.phase_seen = False
        self.shutdown_ok = not any(self.out.glob("shutdown.failed-*.json"))
        self.shutdown_failures = [] if self.shutdown_ok else ["persisted shutdown failure"]
        self.frozen = self.approval["policy_sha256"]
        self.root = Path(self.request["docker_root"])
        self.total = self.request["total_bytes"]
        self.started = self.request["started_epoch"]
        self.started_mono = self.request["started_monotonic"]
        self.check_frozen()

    def event(self, value):
        with open(self.out / (f"events-{os.getpid()}.jsonl"), "a") as stream:
            stream.write(json.dumps({"epoch": int(time.time()), **value}, sort_keys=True) + "\n")
            stream.flush()

    def command(self, argv, timeout=15, check=True):
        self.sequence += 1
        stem = self.out / (f"cmd-{os.getpid()}-{self.sequence:06d}")
        # Never capture environment/full inspect/host network listings. Commands are fixed.
        write(stem.with_suffix(".command.json"), {"argv": [str(a) for a in argv], "timeout": timeout,
                                                 "epoch": time.time(), "monotonic": time.monotonic()})
        with open(stem.with_suffix(".stdout"), "xb") as stdout, open(stem.with_suffix(".stderr"), "xb") as stderr:
            process = subprocess.Popen(argv, stdout=stdout, stderr=stderr)
            try:
                code = process.wait(timeout=timeout)
            except BaseException:
                process.kill()
                process.wait()
                raise
        write(stem.with_suffix(".exit.json"), {"exit": code, "epoch": time.time(), "monotonic": time.monotonic(), "stdout_sha256": p.sha(stem.with_suffix(".stdout")), "stderr_sha256": p.sha(stem.with_suffix(".stderr"))})
        self.last_stdout = stem.with_suffix(".stdout")
        result = self.last_stdout.read_text()
        p.require(not check or code == 0, "command failed; retained " + str(stem))
        return result

    def inspect(self, name):
        return p.loads(self.command(["docker", "inspect", "--format", INSPECT, name]))

    def identity(self, running=True):
        d = self.inspect(P["container"])
        expected = read(self.out / "instance.json")["instance"]
        p.require(p.hexstr(expected) and d["id"] == expected and (not self.instance or self.instance == expected), "exact new instance mismatch; refusing adoption/stop")
        p.require(all(d["labels"].get(k) == v for k, v in {**LABELS, "otel283.nonce": self.nonce}.items()), "owned labels changed")
        p.require(d["image"] == P["image"] and d["memory"] == 16 * p.GiB and d["swap"] == 16 * p.GiB and d["cpu"] == 4000000000, "resource/image identity changed")
        p.require(d["network"] == "bridge" and d["logdriver"] == "none", "network/logging profile changed")
        p.require(d["cmd"] == ["-g", "--", "/bin/bash", "/etc/fdb4/container-entrypoint.sh", str(self.started + 1200)], "sealed in-container deadline/entrypoint changed")
        ports = {f"{port}/tcp": [{"HostIp": "127.0.0.1", "HostPort": str(port)}] for port in P["ports"]}
        p.require(d["ports"] == ports, "port bindings changed")
        mounts = d["mounts"]
        p.require(len(mounts) == 3 and any(m["Type"] == "volume" and m.get("Name") == P["volume"] and m["Destination"] == "/var/fdb/data" and m["RW"] is True for m in mounts)
                  and any(m["Type"] == "bind" and m["Source"] == str(HERE) and m["Destination"] == "/etc/fdb4" and m["RW"] is False for m in mounts)
                  and any(m["Type"] == "bind" and m["Source"] == self.approval["probe_binary"] and m["Destination"] == "/etc/ingest20k/inode-flags" and m["RW"] is False for m in mounts), "volume/config/probe mount changed")
        p.require(d["oom"] is False and d["restarts"] == 0, "OOM/restart observed")
        if running:
            p.require(d["running"] is True and d["status"] == "running" and d["pid"] > 0, "new server not running")
        self.instance = expected
        return d

    def volume(self):
        form = '{"name":{{json .Name}},"driver":{{json .Driver}},"scope":{{json .Scope}},"options":{{json .Options}},"labels":{{json .Labels}}}'
        d = p.loads(self.command(["docker", "volume", "inspect", "--format", form, P["volume"]]))
        p.require(d["name"] == P["volume"] and d["driver"] == "local" and d["scope"] == "local" and d["options"] in (None, {}), "new default local volume metadata mismatch")
        p.require(all(d["labels"].get(k) == v for k, v in {**LABELS, "otel283.nonce": self.nonce}.items()), "new volume label/nonce mismatch; never adopt")

    def sequential(self):
        # One name per line; store only the list, never container environments or IPs.
        retained = OLD
        if "new_host_inventory" in self.approval:
            inventory = self.approval["new_host_inventory"]
            daemon_id = self.command(["docker", "info", "--format", "{{.ID}}"]).strip()
            p.require(daemon_id == inventory["docker_daemon_id"], "new-host Docker daemon identity mismatch")
            retained = inventory["retained"]
        all_names = self.command(["docker", "ps", "-a", "--filter", "name=^/otel283-", "--format", "{{.Names}}"])
        owned_ids = self.command(["docker", "ps", "-aq", "--filter", "label=otel283.owner=" + P["owner"]]).split()
        targets = set(retained) | {name for name in all_names.splitlines() if name.startswith(("otel283-fdb", "otel283-ch"))} | set(owned_ids)
        for name in sorted(targets):
            d = self.inspect(name)
            if d["name"] == "/" + P["container"]:
                continue
            if name in retained:
                p.require(d["id"] == retained[name], "protected retained instance identity changed")
            p.require(d["running"] is False and d["status"] == "exited" and d["exit_code"] == 0 and d["oom"] is False and d["restarts"] == 0, "another retained owned database is not cleanly stopped")

    def check_frozen(self, full=False):
        p.require(p.sha(self.out / "request.json") == self.request_sha256, "sealed request/deadline changed")
        p.require(p.runtime_sha(HERE) == self.frozen, "policy/config changed")
        p.require(p.sha(self.request["approval"]) == self.request["approval_sha256"], "approval changed")
        for key, pathkey in (("binary_sha256", "binary"), ("input_manifest_sha256", "source_manifest"), ("probe_sha256", "probe_binary")):
            p.require(p.sha(self.request[pathkey]) == self.approval[key], "immutable application/source manifest changed")
        p.require(p.sha(HERE / "inode_flags.c") == self.approval["probe_source_sha256"], "probe source hash changed")
        if full:
            revision = self.command(["git", "-C", self.request["source_root"], "rev-parse", "HEAD"]).strip()
            source_head = self.last_stdout
            clean = self.command(["git", "-C", self.request["source_root"], "status", "--porcelain=v1"])
            source_clean = self.last_stdout
            p.require(revision == self.approval["source_revision"] and not clean, "chosen application source tree changed")
            seen = set()
            lines = Path(self.request["source_manifest"]).read_text().splitlines()
            p.require(0 < len(lines) <= 100000, "empty/oversized source manifest")
            for line in lines:
                match = re.fullmatch(r"([0-9a-f]{64})  (.+)", line)
                p.require(match is not None, "invalid source manifest record")
                path = Path(match[2])
                if not path.is_absolute():
                    path = Path(self.request["source_root"]) / path
                path = path.resolve(strict=True)
                p.require(str(path) not in seen and path.is_file(), "duplicate/non-file source input")
                seen.add(str(path))
                p.require(p.sha(path) == match[1], "actual source input changed")
            return {"source_head": source_head, "source_clean": source_clean}

    def bounded_exec(self, args, seconds=10, user=None):
        argv = ["docker", "exec"]
        if user:
            argv += ["--user", user]
        return self.command(argv + [self.instance, "/usr/bin/timeout", "--kill-after=1s", str(seconds) + "s"] + args, seconds + 3)

    def phase(self):
        current = self.out / "active-phase.json"
        if not current.exists():
            p.require(not self.phase_seen, "active phase removed after declaration")
            return
        self.phase_seen = True
        pointer = read(current)
        p.require(pointer["stage"] in ("load", "drain", "census", "oracle"), "unknown active stage")
        path = self.out / ("phase-" + pointer["stage"] + ".json")
        p.require(p.sha(path) == pointer["sha256"], "phase changed")
        phase = read(path)
        p.require(phase["instance"] == self.instance and all(phase[k] == self.approval[k] for k in ("count", "source_revision", "binary_sha256", "input_manifest_sha256", "policy_sha256")), "phase binding changed")
        phase["now"] = int(time.time())
        p.phase(phase, count=self.approval["count"])

    def status(self, *, bootstrap=False):
        raw = self.bounded_exec(["/usr/bin/fdbcli", "-C", "/var/fdb/data/cluster/fdb.cluster", "--exec", "status json"])
        # Parsing rejects a stream even if jq could return a successful first item.
        document = p.loads(raw)
        result = subprocess.run(["jq", "-ce", "-f", str(HERE / "status-check.jq")], input=raw, text=True, capture_output=True, timeout=3)
        self.event({"type": "status-validator", "exit": result.returncode, "stderr": result.stderr})
        if result.returncode != 0 and bootstrap:
            return p.startup_pending(document)
        p.require(result.returncode == 0, "status failed closed; command raw retained")
        return p.loads(result.stdout)

    def resources(self):
        host = host_sample()
        fs = os.statvfs(self.root)
        inside_fsid = self.bounded_exec(["/usr/bin/stat", "-f", "-c", "%i", "/var/fdb/data"], seconds=3).strip()
        host_fsid = self.command(["stat", "-f", "-c", "%i", str(self.root)], timeout=3).strip()
        p.require(inside_fsid and inside_fsid == host_fsid, "data volume must share the actual Docker backing filesystem")
        p.require(fs.f_frsize * fs.f_blocks == self.total and os.stat(self.out).st_dev == os.stat(self.root).st_dev, "same-filesystem identity/total changed")
        output = self.bounded_exec(["/bin/bash", "-c", MEMORY_SCRIPT, "ingest20k-memory", "/sys/fs/cgroup"], seconds=5)
        rows = output.splitlines()
        usage = int(rows[0])
        events, pids = {}, []
        for row in rows[1:]:
            if not row:
                continue
            if " " in row:
                k, v = row.split()
                events[k] = int(v)
            else:
                pids.append(int(row))
        p.require(all(events.get(k) == 0 for k in ("oom", "oom_kill", "oom_group_kill")), "OOM accounting absent/nonzero")
        if len(pids) == 4 and self.initial_pids is None:
            self.initial_pids = pids
        if self.initial_pids is not None:
            p.require(pids == self.initial_pids, "server PID loss/replacement")
        else:
            p.require(time.monotonic() - self.started_mono < 300, "four servers not observed within five minutes")
        # Count all volume data/logs/traces, the entire container root (including
        # shared image blocks, deliberately conservative), and host receipts.
        du = []
        for path in ("/", "/var/fdb/data"):
            du.extend(self.bounded_exec(["/usr/bin/du", "-sx", "-B1", path], seconds=8).splitlines())
        p.require(len(du) == 2, "allocation census incomplete")
        pairs = [row.split() for row in du]
        p.require([row[1] for row in pairs] == ["/", "/var/fdb/data"], "allocation census identities")
        values = [int(row[0]) for row in pairs]
        p.require(all(value >= 0 for value in values), "invalid allocated blocks")
        receipts = allocated_tree(self.out)
        data = {"total_bytes": self.total, "available_bytes": fs.f_frsize * fs.f_bavail,
                "allocated_bytes": sum(values) + receipts, "host_receipt_bytes": receipts,
                "host_available_bytes": host["mem_available_bytes"], "cgroup_bytes": usage}
        p.capacity(data)
        self.event({"type": "capacity", **data, "server_pids": pids, "oom_events": events})
        # Raw counters are whole-container accounting, including bootstrap, DD,
        # monitoring and this workload; they are not per-run NAND/write amplification.
        self.bounded_exec(["/bin/bash", "-c", COUNTERS_SCRIPT, "ingest20k-counters", "/sys/fs/cgroup", "/proc/self/cgroup"], seconds=5)
        self.event({"type": "raw_cpu_io", "scope": "whole_container_including_bootstrap_DD_monitor_and_workload",
                    "raw_path": self.last_stdout.name, "raw_sha256": p.sha(self.last_stdout)})
        return data

    def shutdown_failure(self, reason):
        self.shutdown_ok = False
        self.shutdown_failures = [*getattr(self, "shutdown_failures", []), reason]
        try:
            write(self.out / (f"shutdown.failed-{os.getpid()}.json"), {"failures": self.shutdown_failures}, exclusive=False)
        except OSError:
            pass

    def stop(self):
        # Emergency stop must work even when the receipt filesystem is full or
        # a resource/health guard failed. Only immutable ID + ownership authorize
        # stopping; OOM/resource failures must not veto our own shutdown.
        if any(self.out.glob("shutdown.failed-*.json")) and getattr(self, "shutdown_ok", True):
            self.shutdown_ok = False
            self.shutdown_failures = [*getattr(self, "shutdown_failures", []), "persisted shutdown failure"]
        if not self.instance and not (self.out / "instance.json").exists():
            return
        expected = self.instance or read(self.out / "instance.json")["instance"]
        p.require(p.hexstr(expected), "invalid exact stop instance")
        expected_labels = {**LABELS, "otel283.nonce": self.nonce}
        def exact():
            result = subprocess.run(["docker", "inspect", "--format", INSPECT, expected], capture_output=True, text=True, timeout=10, check=True)
            d = p.loads(result.stdout)
            p.require(d["id"] == expected and d["name"] == "/" + P["container"] and all(d["labels"].get(k) == v for k, v in expected_labels.items()), "stop identity mismatch; FAILED/UNKNOWN, never stop a replacement")
            return d
        before = exact()
        result = None
        if before["running"]:
            result = subprocess.run(["docker", "stop", "--time", "60", expected], capture_output=True, text=True, timeout=75)
        if result is not None and result.returncode != 0:
            self.shutdown_failure("docker stop returned nonzero: " + str(result.returncode))
        after = exact()
        confirmed = after["running"] is False and after["status"] == "exited"
        try:
            self.event({"type": "stop", "instance": expected, "confirmed": confirmed,
                        "command_exit": result.returncode if result else None,
                        "stdout": result.stdout if result else "", "stderr": result.stderr if result else ""})
        except Exception:
            self.shutdown_failure("stop receipt persistence failed")
            print("STOP receipt persistence failed; inspect retained instance", file=sys.stderr)
        p.require(confirmed, "STOP NOT CONFIRMED; FAILED/UNKNOWN; keep lease and intervene")
        return {**after, "shutdown_ok": getattr(self, "shutdown_ok", True),
                "shutdown_failures": list(getattr(self, "shutdown_failures", []))}

    def stop_until_confirmed(self):
        while True:
            try:
                return self.stop()
            except BaseException as error:
                self.shutdown_failure("stop confirmation failed: " + str(error))
                print("FAILED/UNKNOWN stop: " + str(error), file=sys.stderr)
                try:
                    self.event({"type": "stop-failed", "error": str(error)})
                except BaseException:
                    pass
                time.sleep(5)


def monitor(out, lease_fd):
    runtime = Runtime(out)
    # Never acquire another flock: validate the original inherited descriptor
    # inside containment cleanup, initially and again on every monitored cycle.
    last_inode = 0
    try:
        lease = read(Path(out) / "lease.json")
        check_lease(lease_fd, lease)
        while not (Path(out) / "stop.request.json").exists():
            tick = time.monotonic()
            check_lease(lease_fd, lease)
            p.require(os.getppid() == lease["pid"] and parent_start(lease["pid"]) == lease["start_ticks"], "supervisor died/replaced")
            p.require(tick - runtime.started_mono < 1200 and abs((time.time() - runtime.started) - (tick - runtime.started_mono)) < 3, "1200-second monitor deadline/clock discontinuity")
            runtime.check_frozen()
            runtime.identity()
            runtime.sequential()
            runtime.volume()
            runtime.resources()
            heartbeat = read(Path(out) / "supervisor-heartbeat.json")
            p.require(heartbeat["pid"] == lease["pid"] and heartbeat["instance"] == runtime.instance and 0 <= time.monotonic() - heartbeat["monotonic"] < 90, "supervisor heartbeat stale/mismatched")
            if (Path(out) / "bootstrap.ready.json").exists():
                runtime.phase()
                summary = runtime.status()
                runtime.phase()
                runtime.backlog = p.backlog({"queue_bytes": summary["queue_bytes"], "lag_seconds": summary["durability_lag_seconds"], "previous_streak": runtime.backlog})["streak"]
                runtime.event({"type": "status", "status": summary, "backlog_streak": runtime.backlog})
                if tick - last_inode >= 60:
                    proof = runtime.bounded_exec(["/bin/bash", "/etc/fdb4/inode-readonly.sh"], 20, user="fdb")
                    p.inode_proof({"rows": [p.loads(line) for line in proof.splitlines()]})
                    last_inode = tick
            check_lease(lease_fd, lease)
            write(Path(out) / "heartbeat.json", {"monotonic": time.monotonic(), "pid": os.getpid(), "instance": runtime.instance}, exclusive=False)
            time.sleep(max(0, 10 - (time.monotonic() - tick)))
    finally:
        # If the daemon cannot stop, retain the global lease and retry. There is
        # no safe automatic unlock with an unconfirmed live database.
        stopped = runtime.stop_until_confirmed()
        p.require(stopped is not None and stopped.get("shutdown_ok") is True, "shutdown failed after containment")


def strict_ready(runtime, guard, name, seconds, deadline=None, bootstrap=False):
    samples = []
    end = deadline if deadline is not None else time.monotonic() + seconds
    while time.monotonic() < end:
        guard(deadline=end)
        summary = runtime.status(bootstrap=bootstrap)
        guard(deadline=end)
        runtime.phase()
        sample = {"epoch": int(time.time()), "raw_path": runtime.last_stdout.name,
                  "raw_sha256": p.sha(runtime.last_stdout), **summary}
        write(runtime.out / (f"{name}-{time.monotonic_ns()}.json"), sample)
        if summary.get("startup_pending") is True:
            samples = []
        elif summary["queue_bytes"] < 16777216 and summary["durability_lag_seconds"] < 10:
            samples.append(sample)
        else:
            samples = []
        if len(samples) == 3:
            p.readiness({"samples": samples})
            write(runtime.out / (name + ".json"), {"instance": runtime.instance, "samples": samples, "policy_sha256": runtime.frozen})
            return
        time.sleep(5)
    raise ValueError("strict readiness/drain deadline")


def run(args):
    approval = read(args.approval)
    p.approval(approval)
    select_arm(approval)
    for key in ("binary", "source_manifest", "source_root", "out", "probe_binary"):
        setattr(args, key, approval[key])
        p.require(Path(approval[key]).absolute() == Path(approval[key]).resolve(), "canonical sealed input/output paths required")
    p.require(args.execute_approved, "implementation prep is not launch permission")
    p.require(p.runtime_sha(HERE) == approval["policy_sha256"], "approved policy bytes mismatch")
    for path, key in ((args.binary, "binary_sha256"), (args.source_manifest, "input_manifest_sha256"), (args.probe_binary, "probe_sha256")):
        p.require(p.sha(path) == approval[key], "approved application input hash mismatch")
    p.require(p.sha(HERE / "inode_flags.c") == approval["probe_source_sha256"], "approved GET probe source mismatch")
    for binary in (args.binary, args.probe_binary):
        binary_mode = Path(binary).stat().st_mode
        p.require(stat.S_ISREG(binary_mode) and binary_mode & 0o222 == 0 and os.access(binary, os.X_OK), "binaries must be regular, executable and non-writable")
    out = Path(args.out).absolute()
    p.require(out.name == "ingest20k-" + P["run"] and not out.exists() and not out.is_symlink(), "new exact root required; never reuse")
    p.require(out.parent.resolve() == out.parent, "symlinked root parent rejected")
    lock_path = lease_path()
    lock_fd = os.open(lock_path, os.O_RDWR | os.O_CREAT | os.O_NOFOLLOW, 0o600)
    fcntl.flock(lock_fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
    session_epoch, session_monotonic = int(time.time()), time.monotonic()
    # The lease covers quiet admission through confirmed stop; no child reacquires it.
    os.umask(0o077)
    out.mkdir()
    lease_info = os.fstat(lock_fd)
    lease = {"path": str(lock_path), "pid": os.getpid(), "start_ticks": parent_start(os.getpid()),
             "device": lease_info.st_dev, "inode": lease_info.st_ino, "descriptor": lock_fd}
    check_lease(lock_fd, lease)
    write(out / "lease.json", lease)
    info_args = ["docker", "info", "--format", "{{.DockerRootDir}}"]
    write(out / "admission-docker-info.command.json", {"argv": info_args})
    with open(out / "admission-docker-info.stdout", "xb") as stdout, open(out / "admission-docker-info.stderr", "xb") as stderr:
        result = subprocess.run(info_args, stdout=stdout, stderr=stderr, timeout=10)
    write(out / "admission-docker-info.exit.json", {"exit": result.returncode})
    p.require(result.returncode == 0, "Docker root accounting command failed; raw retained")
    docker_root = (out / "admission-docker-info.stdout").read_text().strip()
    fs = os.statvfs(docker_root)
    p.require(os.stat(out).st_dev == os.stat(docker_root).st_dev, "receipt root must share Docker filesystem")
    request = {"approval": str(Path(args.approval).resolve()), "approval_sha256": p.sha(args.approval),
               "binary": str(Path(args.binary).resolve()), "probe_binary": str(Path(args.probe_binary).resolve()), "source_manifest": str(Path(args.source_manifest).resolve()), "source_root": str(Path(args.source_root).resolve()),
               "docker_root": docker_root, "total_bytes": fs.f_frsize * fs.f_blocks,
               "started_epoch": session_epoch, "started_monotonic": session_monotonic, "nonce": uuid.uuid4().hex}
    write(out / "request.json", request)
    runtime = Runtime(out)
    attached = watcher = workload = None
    stop_state = None
    commitments = {}

    def remember(*names):
        for name in names:
            commitments[name] = p.sha(out / name)

    remember("request.json", "lease.json", "admission-docker-info.command.json",
             "admission-docker-info.stdout", "admission-docker-info.stderr", "admission-docker-info.exit.json")
    try:
        runtime.check_frozen(full=True)
        runtime.sequential()
        names = runtime.command(["docker", "ps", "-a", "--filter", "name=^/otel283-", "--format", "{{.Names}}"])
        volumes = runtime.command(["docker", "volume", "ls", "--filter", "name=otel283-vxnar-", "--format", "{{.Name}}"])
        p.require(P["container"] not in names.splitlines() and P["volume"] not in volumes.splitlines(), "candidate name/volume exists; never adopt")
        digests = p.loads(runtime.command(["docker", "image", "inspect", "--format", "{{json .RepoDigests}}", P["image"]]))
        p.require(P["image"] in digests, "pinned image not locally verified; no pull")
        for port in P["ports"]:
            p.require(not runtime.command(["ss", "-H", "-ltn", "sport = :" + str(port)]).strip(), "candidate port already listening")
        samples, busy = [], []
        quiet_started = time.monotonic()

        def quiet_guard():
            check_lease(lock_fd, lease)
            now = time.monotonic()
            p.require(now - request["started_monotonic"] < 1200, "total deadline during quiet admission")
            p.require(now - quiet_started <= 180, "quiet admission 180-second budget exhausted")

        for i in range(19):
            quiet_guard()
            if i:
                p.require(time.monotonic() + 10 <= quiet_started + 180, "quiet admission 180-second budget exhausted")
                time.sleep(10)
                quiet_guard()
            sample = host_sample()
            samples.append(sample)
            raw = out / (f"quiet-{i}.json")
            write(raw, sample)
            remember(raw.name)
            quiet_guard()
            window = samples[-7:]
            try:
                p.quiet_observations(window)
            except p.QuietBusy as error:
                busy.append({"index": i, "raw_path": raw.name, "raw_sha256": p.sha(raw), "reason": str(error)})
                continue
            if len(window) == 7:
                accepted = p.quiet({"samples": window})
                indices = list(range(i - 6, i + 1))
                write(out / "quiet.accepted.json", {**accepted, "window_indices": indices,
                      "samples": [{"index": index, "raw_path": f"quiet-{index}.json",
                                   "raw_sha256": p.sha(out / f"quiet-{index}.json")} for index in indices],
                      "prior_busy": busy, "budget_seconds": 180, "started_monotonic": quiet_started,
                      "accepted_monotonic": time.monotonic()})
                remember("quiet.accepted.json")
                quiet_guard()
                break
        else:
            raise p.QuietBusy("quiet admission remained busy through 19 samples/180 seconds")
        current = os.statvfs(docker_root)
        p.require(current.f_frsize * current.f_blocks == request["total_bytes"], "filesystem total changed during admission")
        # Reserve 64 GiB plus the conservative global floor before creation.
        p.require(current.f_frsize * current.f_bavail >= p.floor_bytes(request["total_bytes"]) + 64 * p.GiB, "trial reservation no longer fits")
        runtime.sequential()
        labels = {**LABELS, "otel283.nonce": request["nonce"]}
        label_args = [item for k, v in labels.items() for item in ("--label", k + "=" + v)]
        check_lease(lock_fd, lease)
        value = runtime.command(["docker", "volume", "create"] + label_args + [P["volume"]]).strip()
        p.require(value == P["volume"], "unexpected newly created volume")
        runtime.volume()
        argv = ["docker", "create", "--name", P["container"], "--pull", "never", "--restart", "no", "--network", "bridge", "--cpus", "4", "--memory", "16g", "--memory-swap", "16g", "--log-driver", "none"] + label_args
        argv += ["--mount", "type=volume,source=" + P["volume"] + ",target=/var/fdb/data,volume-nocopy", "--mount", "type=bind,source=" + str(HERE) + ",target=/etc/fdb4,readonly",
                 "--mount", "type=bind,source=" + args.probe_binary + ",target=/etc/ingest20k/inode-flags,readonly"]
        for port in P["ports"]:
            argv += ["--publish", f"127.0.0.1:{port}:{port}"]
        argv += ["--env", "FDB4_OWNER=" + P["owner"], "--env", "FDB4_SESSION=" + P["session"], "--env", "FDB4_RUN=" + P["run"], "--env", "FDB_CLUSTER_FILE=/var/fdb/data/cluster/fdb.cluster", "--entrypoint", "/usr/bin/tini", P["image"], "-g", "--", "/bin/bash", "/etc/fdb4/container-entrypoint.sh", str(request["started_epoch"] + 1200)]
        check_lease(lock_fd, lease)
        instance = runtime.command(argv).strip()
        p.require(p.hexstr(instance), "create returned invalid instance ID")
        write(out / "instance.json", {"instance": instance, **labels})
        runtime.instance = instance
        runtime.identity(running=False)
        with open(out / "server.stdout", "xb") as stdout, open(out / "server.stderr", "xb") as stderr:
            check_lease(lock_fd, lease)
            attached = subprocess.Popen(["docker", "start", "--attach", instance], stdout=stdout, stderr=stderr, pass_fds=(lock_fd,))
        started_wait = time.monotonic()
        while not runtime.inspect(P["container"])["running"]:
            check_lease(lock_fd, lease)
            p.require(attached.poll() is None and time.monotonic() - started_wait < 30, "attached server failed to start")
            time.sleep(1)
        write(out / "supervisor-heartbeat.json", {"pid": os.getpid(), "instance": instance, "monotonic": time.monotonic()}, exclusive=False)
        watcher = subprocess.Popen([sys.executable, "-B", str(HERE / "driver.py"), "_monitor", str(out), "--lease-fd", str(lock_fd)], pass_fds=(lock_fd,))

        def supervised_wait(process=None, deadline=None, receipt=None):
            write(out / "supervisor-heartbeat.json", {"pid": os.getpid(), "instance": instance, "monotonic": time.monotonic()}, exclusive=False)
            while True:
                check_lease(lock_fd, lease)
                write(out / "supervisor-heartbeat.json", {"pid": os.getpid(), "instance": instance, "monotonic": time.monotonic()}, exclusive=False)
                p.require(watcher.poll() is None and attached.poll() is None, "monitor or attached server exited")
                heartbeat = out / "heartbeat.json"
                if heartbeat.exists():
                    beat = read(heartbeat)
                    p.require(beat["instance"] == instance and beat["pid"] == watcher.pid and 0 <= time.monotonic() - beat["monotonic"] < 60, "monitor heartbeat stale/mismatched")
                else:
                    p.require(time.monotonic() - started_wait < 60, "monitor heartbeat absent")
                p.require(time.monotonic() - request["started_monotonic"] < 1200, "total deadline")
                if deadline is not None:
                    p.require(time.monotonic() < deadline, "stage deadline")
                if process is None:
                    return
                if process.poll() is not None:
                    if receipt:
                        write(out / (receipt + ".exit.json"), {"exit": process.returncode, "epoch": int(time.time())})
                    p.require(process.returncode == 0, "supervised command failed; raw retained")
                    return
                time.sleep(1)

        # Observe the initial heartbeat before any database command. No fallback
        # to an unmonitored bootstrap when the monitor failed to initialize.
        while not (out / "heartbeat.json").exists():
            supervised_wait()
            time.sleep(1)
        # Native configure NEW exclusively initializes; full status can block before configuration.
        runtime.bounded_exec(["/bin/bash", "-c", LAYOUT_SCRIPT, "ingest20k-layout", "/var/fdb/data", p.sha(HERE / "fdb.cluster.seed"), P["owner"] + " " + P["session"] + " " + P["run"]], 5)
        proofs = {"layout": runtime.last_stdout}
        runtime.identity()
        proofs["identity"] = runtime.last_stdout
        runtime.volume()
        proofs["volume"] = runtime.last_stdout
        proofs.update(runtime.check_frozen(full=True))
        with open(out / "server.stdout", "rb") as stream:
            bootstrap_get_raw = stream.readline(16385)
        p.require(bootstrap_get_raw.endswith(b"\n") and len(bootstrap_get_raw) <= 16384, "missing/incomplete bootstrap root GET receipt")
        bootstrap_get = p.loads(bootstrap_get_raw.decode())
        p.bootstrap_get(bootstrap_get)
        supervised_wait()
        proof_receipts = {}
        for name, raw in proofs.items():
            proof_receipts[name] = {"raw_path": raw.name, "raw_sha256": p.sha(raw),
                                   "command_sha256": p.sha(raw.with_suffix(".command.json")),
                                   "exit_sha256": p.sha(raw.with_suffix(".exit.json"))}
            remember(raw.name, raw.with_suffix(".command.json").name, raw.with_suffix(".exit.json").name)
        write(out / "bootstrap.authorized.json", {"instance": instance, **LABELS, "nonce": runtime.nonce,
              "image": P["image"], "volume": P["volume"], "seed_sha256": p.sha(HERE / "fdb.cluster.seed"),
              "source_revision": approval["source_revision"], "binary_sha256": approval["binary_sha256"],
              "input_manifest_sha256": approval["input_manifest_sha256"], "policy_sha256": runtime.frozen,
              "probe_binary_sha256": approval["probe_sha256"], "probe_source_sha256": approval["probe_source_sha256"],
              "proofs": proof_receipts, "root_get": {"attached_path": "server.stdout", "raw": bootstrap_get_raw.decode(),
              "raw_sha256": hashlib.sha256(bootstrap_get_raw).hexdigest(), "facts": bootstrap_get},
              "authority": "native-configure-new-exclusive-initialization", "epoch": int(time.time())})
        write(out / "bootstrap.attempted.json", {"instance": instance, "epoch": int(time.time()),
              "authorization_sha256": p.sha(out / "bootstrap.authorized.json")})
        remember("bootstrap.authorized.json", "bootstrap.attempted.json")
        configured = runtime.bounded_exec(["/usr/bin/fdbcli", "-C", "/var/fdb/data/cluster/fdb.cluster", "--exec", "configure new single ssd-redwood-1 logs:=1 commit_proxies:=1 grv_proxies:=1 resolvers:=1"], 20)
        p.require(configured == "Database created\n" and not runtime.last_stdout.with_suffix(".stderr").read_bytes(),
                  "configure did not return sole canonical Database created outcome; never retry")
        bootstrap_deadline = time.monotonic() + 180
        first_observation = time.monotonic() + 5
        while time.monotonic() < first_observation:
            supervised_wait(deadline=bootstrap_deadline)
            time.sleep(max(0, min(1, first_observation - time.monotonic())))
        supervised_wait(deadline=bootstrap_deadline)
        strict_ready(runtime, supervised_wait, "bootstrap.ready", 180, deadline=bootstrap_deadline, bootstrap=True)
        proof = runtime.bounded_exec(["/bin/bash", "/etc/fdb4/inode-readonly.sh"], 20, user="fdb")
        p.inode_proof({"rows": [p.loads(line) for line in proof.splitlines()]})
        write(out / "cow.ready.json", {"instance": instance, "volume": P["volume"], "run": P["run"], "count": approval["count"],
                                       "policy_sha256": runtime.frozen, "binary_sha256": approval["binary_sha256"],
                                       "inode_stdout_sha256": p.sha(runtime.last_stdout), "probe_binary_sha256": approval["probe_sha256"],
                                       "probe_source_sha256": approval["probe_source_sha256"], "bootstrap_sha256": p.sha(out / "bootstrap.ready.json")})
        remember("instance.json", "bootstrap.ready.json", "cow.ready.json", runtime.last_stdout.name)

        def stage(name, cap):
            runtime.check_frozen(full=True)
            supervised_wait()
            now = int(time.time())
            remaining = 1200 - (time.monotonic() - request["started_monotonic"])
            p.require(remaining >= cap + 120, "full next-stage budget plus stop margin does not fit; no partial admission")
            record = {k: P[k] for k in ("run", "session", "profile", "count")}
            record["source_revision"] = approval["source_revision"]
            record.update({k: approval[k] for k in ("binary_sha256", "input_manifest_sha256", "policy_sha256")})
            record.update(instance=instance, started_epoch=request["started_epoch"], deadline_epoch=request["started_epoch"] + 1200,
                          now=now, stage=name, stage_started_epoch=now, stage_deadline_epoch=now + cap)
            p.phase(record, count=approval["count"])
            file = out / ("phase-" + name + ".json")
            write(file, record)
            remember(file.name)
            write(out / "active-phase.json", {"stage": name, "sha256": p.sha(file)}, exclusive=False)
            return time.monotonic() + cap

        common = [request["binary"], "--backend", "fdb", "--order", "time", "--cluster-file", str(HERE / "fdb.cluster.seed"), "--run", P["run"], "--seed", "283", "--count", str(approval["count"]), "--tenants", "1", "--attrs", "8", "--match-every", "100", "--batch", str(approval["batch"]), "--save-mode", "batch", "--workers", str(approval["workers"]), "--diagnostics", "--k", "100", "--max-candidates", "2000000", "--max-bytes", "268435456", "--revision", approval["source_revision"]]
        for name, mode, cap in (("load", "ingest", 300), ("drain", None, 300), ("census", "audit", 120), ("oracle", "query", 120)):
            end = stage(name, cap)
            if mode is None:
                strict_ready(runtime, supervised_wait, "drain.certificate", cap, end)
                remember("drain.certificate.json")
            else:
                argv = common + ["--mode", mode, "--timeout", str(cap) + "s", "--output", str(out / (mode + ".json"))]
                if mode == "query":
                    argv += ["--suite", "all", "--iterations", "1600"]
                write(out / (name + ".command.json"), {"argv": argv})
                with open(out / (name + ".stdout"), "xb") as stdout, open(out / (name + ".stderr"), "xb") as stderr:
                    workload = subprocess.Popen(argv, stdout=stdout, stderr=stderr)
                supervised_wait(workload, end, name)
                if mode == "ingest":
                    write(out / "load.exited.json", {"instance": instance, "epoch": int(time.time()), "exit": workload.returncode})
                result = read(out / (mode + ".json"))
                p.require(result.get("mode") == mode, "evidence mode mismatch")
                p.evidence(result, approval)
                remember(mode + ".json", name + ".command.json", name + ".stdout", name + ".stderr", name + ".exit.json")
                if mode == "ingest":
                    remember("load.exited.json")
                workload = None
            runtime.check_frozen(full=True)
            supervised_wait(deadline=end)
            runtime.phase()
        write(out / "verification.complete.json", {"run": P["run"], "count": approval["count"], "source_revision": approval["source_revision"], "policy_sha256": runtime.frozen, "ingest_sha256": p.sha(out / "ingest.json"), "load_ack_sha256": p.sha(out / "load.exited.json"), "census_sha256": p.sha(out / "audit.json"), "oracle_sha256": p.sha(out / "query.json"), "drain_sha256": p.sha(out / "drain.certificate.json"), "cow_ready_sha256": p.sha(out / "cow.ready.json"), "claim": str(approval["count"]) + " fresh COW backpressure diagnostic only; no 20k/s, HA, production or SLO claim"})
        remember("verification.complete.json")
    except BaseException as error:
        try:
            write(out / "FAILED.json", {"type": type(error).__name__, "error": str(error), "epoch": int(time.time())})
        except BaseException:
            pass
        raise
    finally:
        try:
            if workload is not None and workload.poll() is None:
                workload.terminate()
                try:
                    workload.wait(timeout=5)
                except subprocess.TimeoutExpired:
                    workload.kill()
                    workload.wait()
        finally:
            try:
                write(out / "stop.request.json", {"epoch": int(time.time())})
                # Let the monitor finish its current bounded sample and stop the
                # server itself; racing our stop against docker exec would turn
                # normal shutdown into an artificial monitor-command failure.
                if watcher is not None:
                    monitor_exit = watcher.wait(timeout=90)
                    write(out / "monitor.exit.json", {"exit": monitor_exit})
                    p.require(monitor_exit == 0, "monitor failed; no successful completion")
            finally:
                # Failed child cleanup, monitor wait or logging cannot veto our
                # independent exact-owned fallback/confirmation of shutdown.
                stop_state = runtime.stop_until_confirmed()
        if attached is not None:
            server_exit = attached.wait(timeout=15)
            write(out / "server.exit.json", {"exit": server_exit})
            p.require(server_exit == 0, "attached server command failed; no completion")
        os.close(lock_fd)
    if not p.clean_stop(stop_state or {})["clean_stop"]:
        write(out / "FAILED_STOP.json", {"confirmed_stopped": True, "exit_code": (stop_state or {}).get("exit_code"),
                                       "oom": (stop_state or {}).get("oom"), "restarts": (stop_state or {}).get("restarts")})
        raise ValueError("server stopped but not clean exit0/noOOM/noRestart; no successful completion")
    runtime.check_frozen(full=True)
    p.require(runtime.shutdown_ok and not list(out.glob("shutdown.failed-*.json")), "sticky shutdown failure; no completion")
    for mode in ("ingest", "audit", "query"):
        result = read(out / (mode + ".json"))
        p.require(result.get("mode") == mode, "final evidence mode mismatch")
        p.evidence(result, approval)
    for name in ("bootstrap.ready.json", "drain.certificate.json"):
        certificate = read(out / name)
        p.require(certificate["instance"] == instance and certificate["policy_sha256"] == runtime.frozen,
                  "final readiness certificate identity mismatch")
        p.readiness({"samples": certificate["samples"]})
        for sample in certificate["samples"]:
            p.require(Path(sample["raw_path"]).name == sample["raw_path"]
                      and p.sha(out / sample["raw_path"]) == sample["raw_sha256"], "readiness raw receipt changed")
    with open(out / "server.stdout", "rb") as stream:
        p.require(stream.readline(16385) == bootstrap_get_raw, "bootstrap root GET receipt changed")
    inventory = final_inventory(out, commitments)
    write(out / "final-inventory.json", inventory)
    write(out / "complete.json", {"verification_sha256": p.sha(out / "verification.complete.json"),
          "inventory_sha256": p.sha(out / "final-inventory.json"), "stopped": True, "run": P["run"], "count": approval["count"]})


def main():
    signal.signal(signal.SIGTERM, lambda *_: (_ for _ in ()).throw(KeyboardInterrupt("terminated")))
    if len(sys.argv) > 1 and sys.argv[1] == "_monitor":
        parser = argparse.ArgumentParser()
        parser.add_argument("mode")
        parser.add_argument("out")
        parser.add_argument("--lease-fd", required=True, type=int)
        args = parser.parse_args()
        monitor(args.out, args.lease_fd)
        return
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("mode", choices=["run"])
    parser.add_argument("--approval", required=True)
    parser.add_argument("--execute-approved", action="store_true")
    args = parser.parse_args()
    run(args)


if __name__ == "__main__":
    try:
        main()
    except (ValueError, OSError, KeyError, TypeError, subprocess.SubprocessError, KeyboardInterrupt) as error:
        print("INGEST20K STOP/REJECT: " + str(error), file=sys.stderr)
        sys.exit(1)
