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

    def __init__(self, root, fault=None):
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
        self.approved = fixtures.approval()
        self.approved.update(binary=str(self.binary), probe_binary=str(self.probe),
                             source_root=str(self.source), source_manifest=str(self.manifest),
                             out=str(self.out), binary_sha256=p.sha(self.binary),
                             probe_sha256=p.sha(self.probe), input_manifest_sha256=p.sha(self.manifest),
                             policy_sha256=p.runtime_sha(driver.HERE),
                             policy_formal_ack_sha256=p.runtime_sha(driver.HERE),
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
            return 0, a["volume"] + "\n"
        if argv[:3] == ["docker", "volume", "inspect"]:
            request = json.loads((self.out / "request.json").read_text())
            return 0, json.dumps({"name": a["volume"], "driver": "local", "scope": "local", "options": {},
                                  "labels": {**driver.LABELS, "otel283.nonce": request["nonce"]}})
        if argv[:2] == ["docker", "create"]:
            self.assert_admitted()
            return 0, self.instance + "\n"
        if argv[:2] == ["docker", "inspect"]:
            return 0, json.dumps(self.inspect())
        if argv[:2] == ["docker", "exec"]:
            command = argv[-1]
            if "ingest20k-layout" in argv:
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
        assert self.quiet_count == 7, "create before all quiet samples"
        lease = json.loads((self.out / "lease.json").read_text())
        info = os.fstat(lease["descriptor"])
        assert (info.st_dev, info.st_ino) == (lease["device"], lease["inode"])
        assert (self.out / "request.json").exists()
        assert len(list(self.out.glob("quiet-*.json"))) == 7
        assert sum(x[0] == "ss" for x in self.calls) == 4
        assert sum(x[0] == "git" and "rev-parse" in x for x in self.calls) >= 1
        assert any(x[:3] == ["docker", "volume", "ls"] for x in self.calls)
        assert any(x[:2] == ["docker", "ps"] for x in self.calls)

    def external_run(self, argv, **kwargs):
        if argv[0] == "jq":
            summary = {"aggregate_healthy_original": True,
                       "queue_bytes": 0, "durability_lag_seconds": 0}
            if self.fault == "bootstrap_health":
                return subprocess.CompletedProcess(argv, 1, "", "offline strict health failure")
            if self.fault == "drain_backlog" and (self.out / "phase-drain.json").exists():
                summary["queue_bytes"] = 16777216
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
            self.running = True
            self.had_started = True
            self.calls.append(list(argv))
            return FakeProcess(self, "attached")
        if str(argv[0]) == str(self.binary):
            self.calls.append(list(argv))
            mode = argv[argv.index("--mode") + 1]
            self.events.append(("workload", mode))
            result = fixtures.evidence(mode)["evidence"]
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
            elif self.fault == mode + "_hang":
                self.now += int(argv[argv.index("--timeout") + 1][:-1])
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
    def harness(self, fault=None):
        previous = getattr(self, "active_harness", None)
        if previous is not None:
            previous.close()
        temp = tempfile.TemporaryDirectory()
        self.addCleanup(temp.cleanup)
        harness = Lifecycle(temp.name, fault)
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
                      "heartbeat_instance", "total_deadline", "ingest_hang", "audit_hang", "query_hang", "lease_load",
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
        for name in ("request.json", "bootstrap.unconfigured.json", "bootstrap.attempted.json", "bootstrap.ready.json", "cow.ready.json",
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

    def test_main_failure_has_no_completion(self):
        self.rejected("configure", main=True)


if __name__ == "__main__":
    unittest.main()
