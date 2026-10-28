#!/usr/bin/env python3
"""Offline foreground lifecycle: real driver/guards/files, no external processes."""
import argparse
import builtins
import copy
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import types
import unittest
from unittest import mock

import driver
import policy as p
import test_policy as fixtures


class FakeProcess:
    def __init__(self, harness, kind, code=0):
        self.harness, self.kind, self.code = harness, kind, code
        self.pid = 41000 + len(harness.processes)
        self.returncode = None
        self.terminated = self.killed = self.joined = False
        harness.processes.append(self)

    def poll(self):
        h = self.harness
        if self.kind in ("monitor", "attached"):
            if h.fault == self.kind + "_exited":
                self.returncode = 9
            return self.returncode
        if h.fault in (self.kind + "_hang", "cleanup_wait", "cleanup_kill"):
            return self.returncode
        self.returncode = self.code
        self.joined = True  # poll() reaps a completed real Popen child.
        return self.returncode

    def wait(self, timeout=None):
        h = self.harness
        if self.kind == "monitor":
            if h.fault == "monitor_wait":
                raise subprocess.TimeoutExpired("offline monitor", timeout)
            h.running = False
            h.stopping = True
            if h.fault == "final_ingest_corrupt":
                path = h.out / "ingest.json"
                result = json.loads(path.read_text())
                result["stats"]["rows"] = 1
                path.write_text(json.dumps(result))
            if h.fault and h.fault.startswith(("rewrite:", "remove:")):
                action, name = h.fault.split(":", 1)
                path = h.out / name
                if action == "rewrite":
                    path.write_bytes(path.read_bytes() + b" \n")
                else:
                    path.unlink()
                h.injected = True
            self.returncode = 8 if h.fault == "monitor_nonzero" else 0
        elif self.kind == "attached":
            if h.fault == "attached_wait":
                raise subprocess.TimeoutExpired("offline attach", timeout)
            self.returncode = 8 if h.fault == "attached_nonzero" else 0
        else:
            if h.fault == "cleanup_wait" and self.terminated and not self.killed:
                raise subprocess.TimeoutExpired("offline workload", timeout)
            if h.fault == "cleanup_kill" and self.terminated:
                raise OSError("offline cleanup wait failed")
            self.returncode = self.code
        self.joined = True
        return self.returncode

    def terminate(self):
        self.terminated = True

    def kill(self):
        self.killed = True


class Lifecycle:
    """Respond at the OS/process boundary, retaining Runtime and policy guards."""
    instance = "a" * 64

    def __init__(self, root, fault=None, batch=32, workers=2, revision="1" * 40):
        self.root, self.fault = Path(root), fault
        self.now = 1000.0
        self.processes, self.calls, self.events = [], [], []
        self.running = False
        self.had_started = False
        self.stopping = False
        self.closed = False
        self.configured = False
        self.injected = False
        self.quiet_count = 0
        self.source = self.root / "source"
        self.source.mkdir()
        source = self.source / "app.go"
        source.write_text("package fixture\n")
        self.manifest = self.root / "source.sha256"
        self.manifest.write_text(p.sha(source) + "  app.go\n")
        self.binary = self.root / "application"
        self.probe = self.root / "inode-flags"
        for binary in (self.binary, self.probe):
            binary.write_text("offline bytes; never executable by this fixture\n")
            binary.chmod(0o555)
        self.out = self.root / "ingest20k-prof20k0290"
        self.approved = fixtures.approval(batch=batch, workers=workers, revision=revision)
        self.approved.update(binary=str(self.binary), probe_binary=str(self.probe),
                             source_root=str(self.source), source_manifest=str(self.manifest),
                             out=str(self.out), binary_sha256=p.sha(self.binary),
                             probe_sha256=p.sha(self.probe), input_manifest_sha256=p.sha(self.manifest),
                             policy_sha256=p.runtime_sha(driver.HERE),
                             policy_approved_sha256=p.runtime_sha(driver.HERE),
                             probe_source_sha256=p.sha(driver.HERE / "inode_flags.c"))
        self.approval = self.root / "approval.json"
        self.approval.write_text(json.dumps(self.approved))
        self.original_write = driver.write
        self.original_open = builtins.open
        self.original_umask = os.umask(0o077)
        self.stack = None

    def enter(self):
        from contextlib import ExitStack
        self.stack = ExitStack()
        self.stack.enter_context(mock.patch.dict(os.environ, {"FDB_LANE_LOCK": str(self.root / "lane.lock")}))
        for name, replacement in (
                ("subprocess.Popen", self.popen), ("subprocess.run", self.external_run),
                ("time.time", lambda: self.now), ("time.monotonic", lambda: self.now),
                ("time.monotonic_ns", lambda: int(self.now * 1000000000)),
                ("time.sleep", self.sleep), ("host_sample", self.host),
                ("os.statvfs", self.statvfs), ("write", self.write)):
            self.stack.enter_context(mock.patch("driver." + name, replacement))
        self.stack.enter_context(mock.patch.object(driver, "open", self.open_file, create=True))
        return self

    def close(self):
        if self.closed:
            return
        self.closed = True
        if self.stack:
            self.stack.close()
        os.umask(self.original_umask)
        # Failures before Runtime construction need not reach the driver's finally.
        lease = self.out / "lease.json"
        if lease.exists():
            fd = json.loads(lease.read_text())["descriptor"]
            try:
                info = os.fstat(fd)
                saved = json.loads(lease.read_text())
                if (info.st_dev, info.st_ino) == (saved["device"], saved["inode"]):
                    os.close(fd)
            except OSError:
                pass

    def open_file(self, path, mode="r", *args, **kwargs):
        name = Path(path).name if not isinstance(path, int) else ""
        fail = ((self.fault == "open:" + name)
                or (self.fault == "stop_event" and self.stopping and name.startswith("events-"))
                or (self.fault == "raw_stdout" and name.startswith("cmd-") and name.endswith(".stdout")))
        if fail and not self.injected:
            self.injected = True
            raise OSError("offline raw/event receipt failure")
        return self.original_open(path, mode, *args, **kwargs)

    def lose_lease(self):
        lock = self.root / "lane.lock"
        lock.rename(self.root / "lost-lane.lock")
        lock.write_text("replaced lock inode\n")

    def statvfs(self, path):
        if self.fault == "lease_before_volume" and (self.out / "quiet.accepted.json").exists() and not self.injected:
            self.lose_lease()
            self.injected = True
        available = 100 if self.fault == "reservation" else 900 * p.GiB
        return types.SimpleNamespace(f_frsize=1, f_blocks=1000 * p.GiB, f_bavail=available)

    def sleep(self, seconds):
        self.now += seconds
        self.heartbeat()

    def heartbeat(self):
        if not self.out.exists():
            return
        watchers = [x for x in self.processes if x.kind == "monitor"]
        if not watchers or self.fault == "heartbeat_absent":
            return
        beat = {"pid": watchers[0].pid, "instance": self.instance, "monotonic": self.now}
        if self.fault == "heartbeat_stale":
            beat["monotonic"] -= 61
        elif self.fault == "heartbeat_pid":
            beat["pid"] += 1
        elif self.fault == "heartbeat_instance":
            beat["instance"] = "b" * 64
        self.original_write(self.out / "heartbeat.json", beat, exclusive=False)

    def host(self):
        self.quiet_count += 1
        if self.fault == "lease_quiet" and self.quiet_count == 3:
            self.lose_lease()
        value = copy.deepcopy(fixtures.quiet()["samples"][0])
        value["epoch"] = int(self.now)
        if self.fault == "quiet":
            value["mem_available_bytes"] = 0
        elif self.fault == "quiet_cpu":
            value["cpu_psi_some_avg10"] = 5.001
        elif self.fault == "quiet_memory_psi":
            value["memory_psi_some_avg10"] = 0.1001
        elif self.fault == "quiet_full_psi":
            value["memory_psi_full_avg10"] = 0.0101
        elif self.fault == "quiet_io":
            value["io_psi_full_avg10"] = 1.001
        elif self.fault == "quiet_variation":
            value["mem_available_bytes"] += (self.quiet_count % 2) * (p.GiB + 1)
        elif self.fault == "quiet_swap":
            value["pswpin"] += self.quiet_count
        elif self.fault in ("quiet_then_clean", "quiet_wait_lease", "quiet_wait_deadline"):
            if self.quiet_count <= 7:
                value["cpu_psi_some_avg10"] = 5.001
            elif self.fault == "quiet_wait_lease":
                self.lose_lease()
            elif self.fault == "quiet_wait_deadline":
                self.now += 1200
        elif self.fault == "quiet_slow_first":
            self.now += 181
        elif self.fault == "quiet_missing":
            del value["cpu_psi_some_avg10"]
        elif self.fault == "quiet_nonfinite":
            value["cpu_psi_some_avg10"] = float("inf")
        elif self.fault == "quiet_decreasing_swap":
            value["pswpin"] = 2 - self.quiet_count
        elif self.fault == "quiet_bad_interval" and self.quiet_count == 2:
            value["epoch"] += 9
        return value

    def write(self, path, value, exclusive=True):
        path = Path(path)
        self.events.append((path.name, copy.deepcopy(value)))
        fail = (self.fault == "receipt:" + path.name
                or self.fault == "raw_exit" and path.name.startswith("cmd-") and path.name.endswith(".exit.json"))
        if fail and not self.injected:
            self.injected = True
            raise OSError("offline receipt failure: " + path.name)
        result = self.original_write(path, value, exclusive)
        if path.name == "cow.ready.json" and self.fault == "budget":
            # Not expired yet: the complete 300s stage plus 120s stop margin cannot fit.
            self.now = 1781
            self.heartbeat()
        if path.name == "active-phase.json":
            self.events.append(("stage", value["stage"]))
            if value["stage"] == "load" and self.fault in ("cleanup_wait", "cleanup_kill"):
                self.now += 1
        return result

    def inspect(self):
        request = json.loads((self.out / "request.json").read_text())
        a = self.approved
        d = {"id": self.instance, "name": "/" + a["container"], "running": self.running,
             "status": "running" if self.running else "exited", "exit_code": 0,
             "oom": False, "restarts": 0, "pid": 123 if self.running else 0,
             "image": a["image"], "memory": 16 * p.GiB, "swap": 16 * p.GiB,
             "cpu": 4000000000, "network": "bridge", "logdriver": "none",
             "labels": {**driver.LABELS, "otel283.nonce": request["nonce"]},
             "cmd": ["-g", "--", "/bin/bash", "/etc/fdb4/container-entrypoint.sh", str(request["started_epoch"] + 1200)],
             "ports": {f"{port}/tcp": [{"HostIp": "127.0.0.1", "HostPort": str(port)}] for port in a["ports"]},
             "mounts": [{"Type": "volume", "Name": a["volume"], "Destination": "/var/fdb/data", "RW": True},
                        {"Type": "bind", "Source": str(driver.HERE), "Destination": "/etc/fdb4", "RW": False},
                        {"Type": "bind", "Source": str(self.probe), "Destination": "/etc/ingest20k/inode-flags", "RW": False}]}
        if self.had_started and not self.running:
            if self.fault == "dirty_exit":
                d["exit_code"] = 9
            elif self.fault == "dirty_oom":
                d["oom"] = True
            elif self.fault == "dirty_restart":
                d["restarts"] = 1
        return d

    def response(self, argv):
        argv = [str(x) for x in argv]
        self.calls.append(argv)
        a = self.approved
        if argv[0] == "git":
            return 0, a["source_revision"] + "\n" if "rev-parse" in argv else ""
        if argv[0] == "ss":
            return 0, "listening\n" if self.fault == "port" else ""
        if argv[:2] == ["docker", "info"]:
            return (9 if self.fault == "docker_info" else 0), str(self.root) + "\n"
        if argv[:2] == ["docker", "ps"]:
            return 0, a["container"] + "\n" if self.fault == "existing_container" else ""
        if argv[:3] == ["docker", "volume", "ls"]:
            return 0, a["volume"] + "\n" if self.fault == "existing_volume" else ""
        if argv[:3] == ["docker", "image", "inspect"]:
            return 0, json.dumps([] if self.fault == "image" else [a["image"]])
        if argv[:3] == ["docker", "volume", "create"]:
            self.assert_admitted()
            request = driver.read(self.out / "request.json")
            labels = {"otel283.owner": a["owner"], "otel283.profile": a["profile"],
                      "otel283.session": a["session"], "otel283.run": a["run"], "otel283.nonce": request["nonce"]}
            expected = ["docker", "volume", "create"] + [item for key, value in labels.items()
                        for item in ("--label", key + "=" + value)] + [a["volume"]]
            assert argv == expected, "Volume launch argv differs from sealed profile"
            return 0, a["volume"] + "\n"
        if argv[:3] == ["docker", "volume", "inspect"]:
            request = json.loads((self.out / "request.json").read_text())
            result = json.dumps({"name": a["volume"], "driver": "local", "scope": "local", "options": {},
                                 "labels": {**driver.LABELS, "otel283.nonce": request["nonce"]}})
            if self.fault == "lease_before_create":
                self.lose_lease()
            return 0, result
        if argv[:2] == ["docker", "create"]:
            self.assert_admitted()
            self.assert_docker_create(argv)
            return 0, self.instance + "\n"
        if argv[:2] == ["docker", "inspect"]:
            result = json.dumps(self.inspect())
            if self.fault == "lease_before_start" and argv[-1] == a["container"] and (self.out / "instance.json").exists() and not self.injected:
                self.lose_lease()
                self.injected = True
            return 0, result
        if argv[:2] == ["docker", "exec"]:
            command = argv[-1]
            if "ingest20k-layout" in argv:
                index = argv.index("ingest20k-layout")
                assert argv[index - 1] == driver.LAYOUT_SCRIPT
                assert argv[index + 1:] == ["/var/fdb/data", p.sha(driver.HERE / "fdb.cluster.seed"),
                                            a["owner"] + " " + a["session"] + " " + a["run"]]
                self.events.append(("layout", None))
                return (9 if self.fault == "layout" else 0), ""
            if command == "status json":
                raw = fixtures.status() if self.configured else fixtures.unconfigured()
                if not self.configured:
                    self.events.append(("unconfigured", None))
                    if self.fault == "already_configured":
                        raw = fixtures.status()
                    elif self.fault == "unconfigured_message":
                        raw["client"]["messages"] = [{"name": "unexpected"}]
                return 0, json.dumps(raw)
            if command.startswith("configure new "):
                self.events.append(("configure", None))
                self.configured = True
                return 0, "not created\n" if self.fault == "configure" else "Database created\n"
            if "/etc/fdb4/inode-readonly.sh" in argv:
                proof = fixtures.inode_proof()
                if self.fault == "inode":
                    proof["rows"][0]["nocow"] = True
                return 0, "\n".join(json.dumps(row) for row in proof["rows"]) + "\n"
        raise AssertionError("unexpected external command: " + repr(argv))

    def assert_admitted(self):
        assert 7 <= self.quiet_count <= 19, "create outside bounded quiet admission"
        accepted = driver.read(self.out / "quiet.accepted.json")
        assert accepted["window_indices"] == list(range(self.quiet_count - 7, self.quiet_count))
        window = []
        for receipt in accepted["samples"]:
            raw = self.out / receipt["raw_path"]
            assert p.sha(raw) == receipt["raw_sha256"]
            window.append(driver.read(raw))
        p.quiet({"samples": window})
        lease = json.loads((self.out / "lease.json").read_text())
        info = os.fstat(lease["descriptor"])
        assert (info.st_dev, info.st_ino) == (lease["device"], lease["inode"])
        assert (self.out / "request.json").exists()
        assert len(list(self.out.glob("quiet-*.json"))) == self.quiet_count
        assert sum(x[0] == "ss" for x in self.calls) == 4
        assert sum(x[0] == "git" and "rev-parse" in x for x in self.calls) >= 1
        assert any(x[:3] == ["docker", "volume", "ls"] for x in self.calls)
        assert any(x[:2] == ["docker", "ps"] for x in self.calls)

    def assert_docker_create(self, argv):
        a = self.approved
        request = driver.read(self.out / "request.json")
        labels = {"otel283.owner": a["owner"], "otel283.profile": a["profile"],
                  "otel283.session": a["session"], "otel283.run": a["run"],
                  "otel283.nonce": request["nonce"]}
        expected = ["docker", "create", "--name", a["container"], "--pull", "never", "--restart", "no",
                    "--network", "bridge", "--cpus", "4", "--memory", "16g", "--memory-swap", "16g", "--log-driver", "none"]
        expected += [item for key, value in labels.items() for item in ("--label", key + "=" + value)]
        expected += ["--mount", "type=volume,source=" + a["volume"] + ",target=/var/fdb/data,volume-nocopy",
                     "--mount", "type=bind,source=" + str(driver.HERE) + ",target=/etc/fdb4,readonly",
                     "--mount", "type=bind,source=" + str(self.probe) + ",target=/etc/ingest20k/inode-flags,readonly"]
        for port in (14883, 14884, 14885, 14886):
            expected += ["--publish", f"127.0.0.1:{port}:{port}"]
        expected += ["--env", "FDB4_OWNER=" + a["owner"], "--env", "FDB4_SESSION=" + a["session"],
                     "--env", "FDB4_RUN=" + a["run"], "--env", "FDB_CLUSTER_FILE=/var/fdb/data/cluster/fdb.cluster",
                     "--entrypoint", "/usr/bin/tini", a["image"], "-g", "--", "/bin/bash",
                     "/etc/fdb4/container-entrypoint.sh", str(request["started_epoch"] + 1200)]
        assert list(argv) == expected, "Docker launch argv differs from sealed profile"

    def application_result(self, argv):
        a = self.approved
        mode = argv[argv.index("--mode") + 1]
        assert mode in ("ingest", "audit", "query"), "unknown workload mode"
        cap = 300 if mode == "ingest" else 120
        expected = [str(self.binary), "--backend", "fdb", "--order", "time", "--cluster-file",
                    str(driver.HERE / "fdb.cluster.seed"), "--run", a["run"], "--seed", "283", "--count", "100000",
                    "--tenants", "1", "--attrs", "8", "--match-every", "100", "--batch", str(a["batch"]),
                    "--save-mode", "batch", "--workers", str(a["workers"]), "--diagnostics", "--k", "100",
                    "--max-candidates", "2000000", "--max-bytes", "268435456", "--revision", a["source_revision"],
                    "--mode", mode, "--timeout", str(cap) + "s", "--output", str(self.out / (mode + ".json"))]
        if mode == "query":
            expected += ["--suite", "all", "--iterations", "1600"]
        assert list(argv) == expected, "Application launch argv differs from sealed profile"
        options = {}
        arguments = iter(argv[1:])
        for key in arguments:
            options[key] = True if key == "--diagnostics" else next(arguments)
        result = fixtures.evidence(mode, run=options["--run"], revision=options["--revision"],
                                   batch=int(options["--batch"]), workers=int(options["--workers"]))["evidence"]
        result["backend"], result["mode"] = options["--backend"], options["--mode"]
        for flag, key in (("--backend", "backend"), ("--order", "order"), ("--save-mode", "save_mode")):
            result["config"][key] = options[flag]
        for flag, key in (("--seed", "seed"), ("--count", "count"), ("--tenants", "tenants"), ("--attrs", "attributes"),
                          ("--match-every", "match_every"), ("--k", "k"), ("--max-candidates", "max_candidates"), ("--max-bytes", "max_bytes")):
            result["config"][key] = int(options[flag])
        result["config"]["diagnostics"] = options["--diagnostics"]
        if mode == "query":
            result["suite"] = options["--suite"]
            result["config"]["iterations"] = int(options["--iterations"])
        else:
            result["stats"]["rows"] = int(options["--count"])
            result["stats"]["attributes"] = int(options["--count"]) * int(options["--attrs"])
        return result

    def external_run(self, argv, **kwargs):
        if argv[0] == "jq":
            summary = {"aggregate_healthy_original": True,
                       "queue_bytes": 0, "durability_lag_seconds": 0}
            if self.fault == "bootstrap_health":
                return subprocess.CompletedProcess(argv, 1, "", "offline strict health failure")
            if self.fault == "drain_backlog" and (self.out / "phase-drain.json").exists():
                summary["queue_bytes"] = 16777216
            if self.fault == "drain_deadline" and (self.out / "phase-drain.json").exists() and not self.injected:
                self.now += 300
                self.heartbeat()
                self.injected = True
            return subprocess.CompletedProcess(argv, 0, json.dumps(summary), "")
        if list(argv[:2]) == ["docker", "stop"]:
            self.calls.append(list(argv))
            self.running = False
            return subprocess.CompletedProcess(argv, 9 if self.fault == "stop_nonzero" else 0, "stopped\n", "")
        code, text = self.response(argv)
        if "stdout" in kwargs:
            kwargs["stdout"].write(text.encode())
            kwargs["stdout"].flush()
        result = subprocess.CompletedProcess(argv, code, text, "")
        if kwargs.get("check") and code:
            raise subprocess.CalledProcessError(code, argv, text, "")
        return result

    def popen(self, argv, **kwargs):
        if "_monitor" in argv:
            self.calls.append(list(argv))
            process = FakeProcess(self, "monitor")
            self.heartbeat()
            return process
        if list(argv[:2]) == ["docker", "start"]:
            self.assert_admitted()
            assert list(argv) == ["docker", "start", "--attach", self.instance], "Unexpected attached start argv"
            self.running = True
            self.had_started = True
            self.calls.append(list(argv))
            return FakeProcess(self, "attached")
        if str(argv[0]) == str(self.binary):
            self.calls.append(list(argv))
            mode = argv[argv.index("--mode") + 1]
            self.events.append(("workload", mode))
            result = self.application_result(argv)
            if self.fault == "evidence:" + mode:
                result["stats" if mode != "query" else "config"]["rows" if mode != "query" else "iterations"] = 1
            if self.fault == "mode:" + mode:
                result["mode"] = "wrong"
            Path(argv[argv.index("--output") + 1]).write_text(json.dumps(result))
            process = FakeProcess(self, mode, 7 if self.fault == "exit:" + mode else 0)
            if self.fault == "source_during_load":
                (self.source / "app.go").write_text("source replaced during workload\n")
            elif self.fault == "manifest_during_load":
                self.manifest.write_text("manifest replaced during workload\n")
            elif self.fault == "approval_during_load":
                self.approval.write_text("{}\n")
            if self.fault == "lease_load":
                self.lose_lease()
            if self.fault == "total_deadline":
                self.now += 1201
                self.heartbeat()
            elif self.fault == mode + "_hang":
                self.now += int(argv[argv.index("--timeout") + 1][:-1])
                self.heartbeat()
            elif self.fault in ("cleanup_wait", "cleanup_kill"):
                self.now += 301
            elif self.fault == "stop_nonzero":
                # The independent foreground fallback must actually issue stop.
                monitor = next(x for x in self.processes if x.kind == "monitor")
                def monitor_wait(timeout=None):
                    monitor.joined = True
                    monitor.returncode = 0
                    return 0
                monitor.wait = monitor_wait
            return process
        code, text = self.response(argv)
        if "stdout" in kwargs:
            kwargs["stdout"].write(text.encode())
            kwargs["stdout"].flush()
        return FakeProcess(self, "command", code)

    def invoke(self, main=False):
        if main:
            with mock.patch.object(sys, "argv", ["driver.py", "run", "--approval", str(self.approval), "--execute-approved"]), mock.patch.object(driver.signal, "signal"):
                return driver.main()
        return driver.run(argparse.Namespace(approval=str(self.approval), execute_approved=True))


class LifecycleTests(unittest.TestCase):
    def harness(self, fault=None, batch=32, workers=2, revision="1" * 40):
        previous = getattr(self, "active_harness", None)
        if previous is not None:
            previous.close()
        temp = tempfile.TemporaryDirectory()
        self.addCleanup(temp.cleanup)
        harness = Lifecycle(temp.name, fault, batch=batch, workers=workers, revision=revision)
        self.addCleanup(harness.close)
        self.active_harness = harness
        return harness.enter()

    def rejected(self, fault, *, main=False):
        h = self.harness(fault)
        with self.assertRaises((ValueError, OSError, KeyError, subprocess.SubprocessError)):
            h.invoke(main)
        self.assertFalse((h.out / "complete.json").exists(), fault)
        if any(x.kind == "attached" for x in h.processes):
            self.assertFalse(h.running, "failure abandoned the owned server: " + fault)
        return h

    def test_real_run_positive_ordered_one_arm_and_final_receipts(self):
        h = self.harness()
        h.invoke()
        self.assertTrue((h.out / "complete.json").is_file())
        self.assertFalse((h.out / "FAILED.json").exists())
        events = [name for name, _ in h.events]
        self.assertLess(events.index("layout"), events.index("unconfigured"))
        self.assertLess(events.index("unconfigured"), events.index("configure"))
        self.assertEqual(events.count("configure"), 1)
        self.assertLess(events.index("bootstrap.ready.json"), events.index("cow.ready.json"))
        self.assertEqual([value for name, value in h.events if name == "stage"], ["load", "drain", "census", "oracle"])
        self.assertEqual([value for name, value in h.events if name == "workload"], ["ingest", "audit", "query"])
        self.assertLess(events.index("load.exited.json"), events.index("drain.certificate.json"))
        self.assertLess(events.index("drain.certificate.json"), events.index("census.command.json"))
        self.assertLess(events.index("census.exit.json"), events.index("oracle.command.json"))
        self.assertLess(events.index("monitor.exit.json"), events.index("complete.json"))
        self.assertLess(events.index("server.exit.json"), events.index("complete.json"))
        self.assertTrue(all(x.joined for x in h.processes))
        for name, cap in (("load", 300), ("drain", 300), ("census", 120), ("oracle", 120)):
            record = driver.read(h.out / ("phase-" + name + ".json"))
            self.assertEqual(record["stage_deadline_epoch"] - record["stage_started_epoch"], cap)
            self.assertGreaterEqual(record["deadline_epoch"] - record["stage_started_epoch"], cap + 120)
        complete = driver.read(h.out / "complete.json")
        self.assertEqual(complete["verification_sha256"], p.sha(h.out / "verification.complete.json"))
        verification = driver.read(h.out / "verification.complete.json")
        self.assertEqual(verification["ingest_sha256"], p.sha(h.out / "ingest.json"))
        self.assertEqual(verification["load_ack_sha256"], p.sha(h.out / "load.exited.json"))
        inventory_path = h.out / "final-inventory.json"
        self.assertEqual(complete["inventory_sha256"], p.sha(inventory_path))
        inventory = driver.read(inventory_path)["files"]
        expected = {str(path.relative_to(h.out)) for path in h.out.rglob("*")
                    if path.is_file() and path.name not in ("final-inventory.json", "complete.json")}
        self.assertTrue(inventory)
        self.assertEqual({item["path"] for item in inventory}, expected)
        self.assertEqual(len(inventory), len(expected), "inventory contains duplicate paths")
        for item in inventory:
            path = h.out / item["path"]
            self.assertEqual(item["size"], path.stat().st_size)
            self.assertEqual(item["sha256"], p.sha(path))
        self.assertTrue({"ingest.json", "audit.json", "query.json", "load.exited.json",
                         "server.stdout", "server.stderr", "monitor.exit.json", "server.exit.json",
                         "lease.json", "request.json", "admission-docker-info.stdout"} <= expected)

    def test_real_lifecycle_nondefault_approved_pairs_bind_launched_evidence(self):
        for batch, workers in ((32, 4), (32, 8), (64, 2), (64, 4), (64, 8)):
            with self.subTest(batch=batch, workers=workers):
                revision = "2" * 40 if (batch, workers) == (64, 8) else "1" * 40
                h = self.harness(batch=batch, workers=workers, revision=revision)
                h.invoke()
                self.assertTrue((h.out / "complete.json").exists())
                launched = [argv for argv in h.calls if argv[0] == str(h.binary)]
                self.assertEqual(len(launched), 3)
                for argv in launched:
                    self.assertEqual(argv[argv.index("--batch") + 1], str(batch))
                    self.assertEqual(argv[argv.index("--workers") + 1], str(workers))
                    result = driver.read(Path(argv[argv.index("--output") + 1]))
                    self.assertEqual((result["config"]["batch"], result["config"]["workers"]), (batch, workers))
                    self.assertEqual(result["revision"], revision)
                    p.evidence(result, h.approved)

    def test_launch_boundary_rejects_changed_or_missing_sealed_arguments(self):
        h = self.harness(batch=64, workers=8)
        h.invoke()
        docker = next(argv for argv in h.calls if argv[:2] == ["docker", "create"])
        application = next(argv for argv in h.calls if argv[0] == str(h.binary))
        for original, validate, flags in (
                (docker, h.assert_docker_create, ("--memory", "--memory-swap", "--cpus", "--entrypoint", "--mount", "--label")),
                (application, h.application_result, ("--count", "--batch", "--workers", "--revision", "--timeout", "--max-bytes", "--cluster-file"))):
            for flag in flags:
                with self.subTest(flag=flag):
                    index = original.index(flag)
                    changed = list(original)
                    changed[index + 1] = "wrong"
                    with self.assertRaisesRegex(AssertionError, "argv differs from sealed profile"):
                        validate(changed)
                    missing = list(original)
                    del missing[index:index + 2]
                    with self.assertRaisesRegex(AssertionError, "argv differs from sealed profile"):
                        validate(missing)
        changed = list(docker)
        changed[-1] = str(int(changed[-1]) + 1)
        with self.assertRaisesRegex(AssertionError, "Docker launch argv"):
            h.assert_docker_create(changed)
        changed = [value for value in application if value != "--diagnostics"]
        with self.assertRaisesRegex(AssertionError, "Application launch argv"):
            h.application_result(changed)

    def test_quiet_wait_accepts_only_seven_clean_consecutive_samples_and_retains_busy_raw(self):
        h = self.harness("quiet_then_clean")
        h.invoke()
        self.assertEqual(h.quiet_count, 14)
        certificate = driver.read(h.out / "quiet.accepted.json")
        self.assertEqual(certificate["window_indices"], list(range(7, 14)))
        self.assertEqual(certificate["budget_seconds"], 180)
        self.assertEqual(certificate["accepted_monotonic"] - certificate["started_monotonic"], 130)
        self.assertEqual([row["index"] for row in certificate["prior_busy"]], list(range(13)))
        for row in certificate["prior_busy"] + certificate["samples"]:
            self.assertEqual(row["raw_sha256"], p.sha(h.out / row["raw_path"]))
        raw = [driver.read(h.out / f"quiet-{index}.json") for index in range(14)]
        self.assertEqual([row["epoch"] for row in raw], list(range(1000, 1140, 10)))
        with self.assertRaises(p.QuietBusy):
            p.quiet({"samples": raw[:7]})
        p.quiet({"samples": raw[-7:]})
        retained = {row["path"] for row in driver.read(h.out / "final-inventory.json")["files"]}
        self.assertTrue({f"quiet-{index}.json" for index in range(14)} <= retained)
        self.assertIn("quiet.accepted.json", retained)
        self.assertTrue((h.out / "complete.json").exists())
        for stage, cap in (("load", 300), ("drain", 300), ("census", 120), ("oracle", 120)):
            record = driver.read(h.out / ("phase-" + stage + ".json"))
            self.assertEqual(record["stage_deadline_epoch"] - record["stage_started_epoch"], cap)
            self.assertEqual(record["deadline_epoch"] - record["started_epoch"], 1200)

    def test_quiet_wait_persistent_busy_thresholds_stop_after_nineteen_samples(self):
        for fault in ("quiet", "quiet_cpu", "quiet_memory_psi", "quiet_full_psi", "quiet_io", "quiet_variation", "quiet_swap"):
            with self.subTest(fault=fault):
                h = self.harness(fault)
                with self.assertRaisesRegex(p.QuietBusy, "19 samples/180 seconds"):
                    h.invoke()
                self.assertEqual(h.quiet_count, 19)
                raw = list(h.out.glob("quiet-*.json"))
                self.assertEqual(len(raw), 19)
                self.assertEqual(driver.read(h.out / "quiet-18.json")["epoch"] - driver.read(h.out / "quiet-0.json")["epoch"], 180)
                self.assertFalse((h.out / "quiet.accepted.json").exists())
                self.assertFalse(any(call[:3] == ["docker", "volume", "create"] or call[:2] in (["docker", "create"], ["docker", "start"]) for call in h.calls))

    def test_quiet_wait_invalid_accounting_is_fatal_without_retry(self):
        for fault, samples in (("quiet_missing", 1), ("quiet_nonfinite", 1),
                               ("quiet_decreasing_swap", 2), ("quiet_bad_interval", 2)):
            with self.subTest(fault=fault):
                h = self.harness(fault)
                with self.assertRaises(ValueError) as error:
                    h.invoke()
                self.assertNotIsInstance(error.exception, p.QuietBusy)
                self.assertEqual(h.quiet_count, samples)
                self.assertEqual(len(list(h.out.glob("quiet-*.json"))), samples)
                self.assertFalse((h.out / "quiet.accepted.json").exists())
                self.assertFalse(any(call[:3] == ["docker", "volume", "create"] or call[:2] in (["docker", "create"], ["docker", "start"]) for call in h.calls))

    def test_quiet_wait_lease_total_deadline_and_first_sample_budget_fail_before_create(self):
        for fault, expected, samples in (("quiet_wait_lease", "global lease descriptor/path identity lost", 8),
                                        ("quiet_wait_deadline", "total deadline during quiet admission", 8),
                                        ("quiet_slow_first", "quiet admission 180-second budget exhausted", 1)):
            with self.subTest(fault=fault):
                h = self.harness(fault)
                with self.assertRaisesRegex(ValueError, "^" + expected + "$"):
                    h.invoke()
                self.assertEqual(h.quiet_count, samples)
                self.assertFalse((h.out / "quiet.accepted.json").exists())
                self.assertFalse(any(call[:3] == ["docker", "volume", "create"] or call[:2] in (["docker", "create"], ["docker", "start"]) for call in h.calls))

    def test_main_dispatch_executes_real_positive_lifecycle(self):
        h = self.harness()
        h.invoke(main=True)
        self.assertTrue((h.out / "complete.json").exists())

    def test_admission_rejects_before_create_or_start(self):
        for fault in ("docker_info", "existing_container", "existing_volume", "image", "port", "quiet", "reservation", "lease_quiet", "raw_stdout", "raw_exit"):
            with self.subTest(fault=fault):
                h = self.rejected(fault)
                self.assertFalse(any(x[:2] in (["docker", "create"], ["docker", "start"]) for x in h.calls))

    def test_sealed_input_and_explicit_permission_fail_before_create(self):
        for fault in ("approval", "binary_hash", "source_hash", "writable_binary", "existing_root", "permission"):
            with self.subTest(fault=fault):
                h = self.harness()
                if fault == "approval":
                    value = dict(h.approved, approved=False)
                    h.approval.write_text(json.dumps(value))
                elif fault == "binary_hash":
                    h.binary.chmod(0o755)
                    h.binary.write_text("changed immutable application\n")
                    h.binary.chmod(0o555)
                elif fault == "source_hash":
                    (h.source / "app.go").write_text("changed source input\n")
                elif fault == "writable_binary":
                    h.binary.chmod(0o755)
                elif fault == "existing_root":
                    h.out.mkdir()
                with self.assertRaises((ValueError, OSError)):
                    if fault == "permission":
                        driver.run(argparse.Namespace(approval=str(h.approval), execute_approved=False))
                    else:
                        h.invoke()
                self.assertFalse((h.out / "complete.json").exists())
                self.assertFalse(any(x[:2] in (["docker", "create"], ["docker", "start"]) for x in h.calls))

    def test_bootstrap_guards_are_connected_and_configure_is_never_retried(self):
        for fault in ("layout", "already_configured", "unconfigured_message", "configure", "inode", "bootstrap_health"):
            with self.subTest(fault=fault):
                h = self.rejected(fault)
                configured = sum(name == "configure" for name, _ in h.events)
                self.assertEqual(configured, 0 if fault in ("layout", "already_configured", "unconfigured_message") else 1)
                self.assertFalse(any(name == "workload" for name, _ in h.events))

    def test_full_next_stage_budget_rejects_unexpired_partial_admission(self):
        h = self.rejected("budget")
        self.assertTrue((h.out / "cow.ready.json").exists())
        self.assertFalse((h.out / "phase-load.json").exists())
        self.assertLess(h.now, 2200)

    def test_supervision_and_deadline_failures_never_complete(self):
        for fault in ("monitor_exited", "attached_exited", "heartbeat_absent", "heartbeat_stale", "heartbeat_pid",
                      "heartbeat_instance", "lease_load",
                      "drain_backlog", "source_during_load", "manifest_during_load", "approval_during_load"):
            with self.subTest(fault=fault):
                self.rejected(fault)

    def test_all_workload_exit_mode_and_real_evidence_guards(self):
        for mode in ("ingest", "audit", "query"):
            for category in ("exit:", "mode:", "evidence:"):
                with self.subTest(mode=mode, category=category):
                    self.rejected(category + mode)

    def test_cleanup_monitor_attachment_stop_and_clean_stop_failures(self):
        for fault in ("cleanup_wait", "cleanup_kill", "monitor_wait", "monitor_nonzero", "attached_wait", "attached_nonzero",
                      "stop_nonzero", "stop_event", "dirty_exit", "dirty_oom", "dirty_restart", "final_ingest_corrupt"):
            with self.subTest(fault=fault):
                h = self.rejected(fault)
                if fault.startswith("cleanup_"):
                    work = next(x for x in h.processes if x.kind == "ingest")
                    self.assertTrue(work.terminated)

    def test_receipt_write_failures_never_complete(self):
        for name in ("request.json", "quiet.accepted.json", "bootstrap.unconfigured.json", "bootstrap.attempted.json", "bootstrap.ready.json", "cow.ready.json",
                     "phase-load.json", "active-phase.json", "load.command.json", "load.exit.json",
                     "load.exited.json", "drain.certificate.json", "census.command.json", "census.exit.json",
                     "oracle.command.json", "oracle.exit.json", "verification.complete.json", "stop.request.json",
                     "monitor.exit.json", "server.exit.json", "final-inventory.json", "complete.json"):
            with self.subTest(receipt=name):
                self.rejected("receipt:" + name)

    def test_raw_attachment_and_workload_log_open_failures_never_complete(self):
        for name in ("server.stdout", "server.stderr", "load.stdout", "load.stderr",
                     "census.stdout", "census.stderr", "oracle.stdout", "oracle.stderr"):
            with self.subTest(raw=name):
                self.rejected("open:" + name)

    def test_lease_loss_at_each_immediate_create_start_boundary(self):
        for fault, forbidden, required in (
                ("lease_before_volume", ["docker", "volume", "create"], None),
                ("lease_before_create", ["docker", "create"], ["docker", "volume", "inspect"]),
                ("lease_before_start", ["docker", "start"], ["docker", "create"])):
            with self.subTest(fault=fault):
                h = self.harness(fault)
                with self.assertRaisesRegex(ValueError, "^global lease descriptor/path identity lost$"):
                    h.invoke()
                self.assertEqual(h.quiet_count, 7)
                if required:
                    self.assertTrue(any(call[:len(required)] == required for call in h.calls))
                self.assertFalse(any(call[:len(forbidden)] == forbidden for call in h.calls))
                self.assertFalse((h.out / "complete.json").exists())
                self.assertFalse(h.running)

    def test_healthy_monitor_does_not_mask_total_or_each_stage_deadline(self):
        for fault, expected in (("total_deadline", "total deadline"), ("ingest_hang", "stage deadline"),
                                ("drain_deadline", "stage deadline"), ("audit_hang", "stage deadline"),
                                ("query_hang", "stage deadline")):
            with self.subTest(fault=fault):
                h = self.harness(fault)
                with self.assertRaisesRegex(ValueError, "^" + expected + "$"):
                    h.invoke()
                heartbeat = driver.read(h.out / "heartbeat.json")
                self.assertLess(h.now - heartbeat["monotonic"], 60)
                self.assertFalse((h.out / "complete.json").exists())
                self.assertFalse(h.running)

    def test_validation_time_evidence_ack_raw_and_verification_bytes_are_bound(self):
        names = ["ingest.json", "audit.json", "query.json", "load.exited.json", "verification.complete.json"]
        names += [stage + suffix for stage in ("load", "census", "oracle")
                  for suffix in (".command.json", ".stdout", ".stderr", ".exit.json")]
        cases = [("rewrite", name) for name in names]
        cases += [("remove", name) for name in ("ingest.json", "load.exited.json", "load.stdout",
                                                "oracle.stderr", "verification.complete.json")]
        for action, name in cases:
            with self.subTest(action=action, receipt=name):
                h = self.harness(action + ":" + name)
                with self.assertRaises((ValueError, OSError)) as error:
                    h.invoke()
                self.assertTrue(h.injected, "mutation never reached writer join")
                if action == "rewrite" or name not in ("ingest.json",):
                    self.assertEqual(str(error.exception), "validated receipt changed/missing: " + name)
                if action == "rewrite" and name in ("ingest.json", "audit.json", "query.json"):
                    # The rewrite is semantically valid; only the original-byte commitment rejects it.
                    p.evidence(driver.read(h.out / name), h.approved)
                self.assertFalse((h.out / "complete.json").exists())
                self.assertFalse(h.running)

    def test_completion_fsync_and_close_errors_after_real_bytes_never_publish(self):
        for fault in ("fsync", "close"):
            with self.subTest(fault=fault):
                h = self.harness()
                observed = []
                pending_fds = {}
                original_fsync = os.fsync

                def observe(path):
                    value = driver.read(path)
                    self.assertEqual(value["verification_sha256"], p.sha(h.out / "verification.complete.json"))
                    self.assertEqual(value["inventory_sha256"], p.sha(h.out / "final-inventory.json"))
                    self.assertFalse((h.out / "complete.json").exists(), "success was visible before persistence")
                    observed.append(path.read_bytes())

                class CloseFailure:
                    def __init__(self, stream, path):
                        self.stream, self.path = stream, path
                    def __enter__(self):
                        self.stream.__enter__()
                        return self.stream
                    def __exit__(self, *args):
                        self.stream.__exit__(*args)
                        observe(self.path)
                        raise OSError("offline completion close failure")

                def open_completion(path, mode="r", *args, **kwargs):
                    stream = h.open_file(path, mode, *args, **kwargs)
                    path = Path(path)
                    if path.name.startswith(".complete.json.") and mode == "x":
                        pending_fds[stream.fileno()] = path
                        if fault == "close":
                            return CloseFailure(stream, path)
                    return stream

                def fsync(fd):
                    original_fsync(fd)
                    if fault == "fsync" and fd in pending_fds:
                        observe(pending_fds[fd])
                        raise OSError("offline completion fsync failure")

                with mock.patch.object(driver, "open", side_effect=open_completion), mock.patch.object(driver.os, "fsync", side_effect=fsync):
                    with self.assertRaisesRegex(OSError, "offline completion " + fault + " failure"):
                        h.invoke()
                self.assertEqual(len(observed), 1)
                self.assertFalse((h.out / "complete.json").exists())
                self.assertFalse(list(h.out.glob(".complete.json.*.pending")))
                self.assertFalse(h.running)
        # Exclusive publication must not replace a prior marker either.
        path = h.root / "complete.json"
        path.write_text("prior marker\n")
        with self.assertRaises(FileExistsError):
            h.original_write(path, {"new": True})
        self.assertEqual(path.read_text(), "prior marker\n")
        self.assertFalse(list(h.root.glob(".complete.json.*.pending")))

    def test_main_failure_has_no_completion(self):
        self.rejected("configure", main=True)


if __name__ == "__main__":
    unittest.main()
