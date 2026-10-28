#!/usr/bin/env python3
"""Real observation adapters and monitor, with temporary files and OS/process edges."""
import contextlib
import copy
import fcntl
import hashlib
import io
import json
import os
from pathlib import Path
import stat
import subprocess
import tempfile
import types
import unittest
from unittest import mock

import driver
import test_policy as fixtures

GIB = 1024 ** 3
INSTANCE = "a" * 64
PROTECTED = {
    "otel283-fdb4-vxnar": "d04595f1a404f0245574fc39b9c7922794fd5b43f39470c03e1e03836f997e06",
    "otel283-ch16-vxnar": "c350fc38ea191a134df09947386b691b8ecd232cf21b289ac3d3cec158487844",
    "otel283-ch16-resume02": "7df4e34f3c11cd4a751c492194d6221accebec302afe993493446e1574c66762",
    "otel283-ch16-resume03": "19d145ce807487b3e4258c856a6271e17faabc907e7c88d4a0ec0fcee77480a1",
}


def digest(path):
    return hashlib.sha256(Path(path).read_bytes()).hexdigest()


def store(path, value):
    Path(path).write_text(json.dumps(value, sort_keys=True) + "\n")


class FakeProcess:
    def __init__(self, owner, argv, stdout, stderr, **_kwargs):
        self.owner, self.argv, self.killed = owner, argv, False
        text, self.returncode = owner.respond(argv)
        stdout.write(text.encode())
        stderr.write(owner.command_stderr.encode())

    def wait(self, timeout=None):
        if self.owner.wait_failure:
            failure, self.owner.wait_failure = self.owner.wait_failure, None
            raise failure
        return self.returncode

    def kill(self):
        self.killed = True
        self.owner.killed.append(self.argv)


class RuntimeFixture(unittest.TestCase):
    """Nothing replaces Runtime.command/inspect/observation/guard implementations."""
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name).resolve()
        self.out = self.root / "ingest20k-prof20k0290"
        self.out.mkdir(mode=0o700)
        self.source = self.root / "source"
        self.source.mkdir()
        self.source_file = self.source / "input.go"
        self.source_file.write_text("package fixture\n")
        self.binary = self.root / "application"
        self.binary.write_bytes(b"fixture application bytes\n")
        self.probe = self.root / "inode-flags"
        self.probe.write_bytes(b"fixture static probe bytes\n")
        self.manifest = self.root / "source.sha256"
        self.manifest.write_text(digest(self.source_file) + "  input.go\n")
        self.approved = fixtures.approval()
        self.approved.update(binary=str(self.binary), source_root=str(self.source),
                             source_manifest=str(self.manifest), probe_binary=str(self.probe), out=str(self.out),
                             binary_sha256=digest(self.binary), input_manifest_sha256=digest(self.manifest),
                             probe_sha256=digest(self.probe), probe_source_sha256=digest(driver.HERE / "inode_flags.c"),
                             policy_sha256=driver.p.runtime_sha(driver.HERE))
        self.approved["policy_formal_ack_sha256"] = self.approved["policy_sha256"]
        self.approval_path = self.root / "approval.json"
        store(self.approval_path, self.approved)
        self.docker_root = self.root / "docker-root"
        self.docker_root.mkdir()
        self.request = {"approval": str(self.approval_path), "approval_sha256": digest(self.approval_path),
                        "binary": str(self.binary), "probe_binary": str(self.probe),
                        "source_manifest": str(self.manifest), "source_root": str(self.source),
                        "docker_root": str(self.docker_root), "total_bytes": 1000 * GIB,
                        "started_epoch": 2000, "started_monotonic": 1000, "nonce": "runtime-fixture"}
        store(self.out / "request.json", self.request)
        store(self.out / "instance.json", {"instance": INSTANCE})
        self.clock = 1001
        self.epoch = 2001
        self.proc = {
            "/proc/meminfo": "MemTotal: 67108864 kB\nMemAvailable: 33554432 kB\n",
            "/proc/vmstat": "pswpin 17\npswpout 23\npgfault 1234\n",
            "/proc/pressure/cpu": "some avg10=0.25 avg60=0.00 avg300=0.00 total=100\n",
            "/proc/pressure/memory": "some avg10=0.02 avg60=0.00 avg300=0.00 total=10\nfull avg10=0.001 avg60=0.00 avg300=0.00 total=1\n",
            "/proc/pressure/io": "some avg10=0.0 avg60=0.0 avg300=0.0 total=0\nfull avg10=0.5 avg60=0.0 avg300=0.0 total=50\n",
            "/proc/123/stat": "123 (fixture parent) S " + " ".join(["0"] * 18 + ["77"] + ["0"] * 4),
        }
        self.fs = types.SimpleNamespace(f_frsize=4096, f_blocks=1000 * GIB // 4096,
                                        f_bavail=500 * GIB // 4096)
        self.command_stderr = ""
        self.wait_failure = None
        self.killed, self.calls, self.stops = [], [], []
        self.git_head, self.git_dirty = self.approved["source_revision"], ""
        self.inside_fsid = self.host_fsid = "fixture-fsid"
        self.cgroup = "2147483648\nlow 0\nhigh 0\nmax 0\noom 0\noom_kill 0\noom_group_kill 0\n101\n102\n103\n104\n"
        self.du = {"/": "1073741824\t/\n", "/var/fdb/data": "2147483648\t/var/fdb/data\n"}
        self.cpu_io = "scope=whole_container_including_bootstrap_DD_monitor_and_workload\nFILE=/proc/self/cgroup\n0::/\nFILE=/sys/fs/cgroup/cpu.stat\nusage_usec 1000\nFILE=/sys/fs/cgroup/io.stat\n8:0 rbytes=1 wbytes=2 rios=3 wios=4\n"
        self.proof = "\n".join(json.dumps(row) for row in fixtures.inode_proof()["rows"]) + "\n"
        self.status_raw = json.dumps(fixtures.status())
        self.status_summary = {"aggregate_healthy_original": True,
                               "queue_bytes": 0, "durability_lag_seconds": 0}
        self.qj_exit, self.stop_rc, self.exec_failure = 0, 0, None
        self.get_failure = None
        self.parent_pid = 123
        self.stop_confirm_after = 1
        self.inspect_doc = self.good_inspect()
        self.owned = copy.deepcopy(self.inspect_doc)
        self.volume_doc = {"name": self.approved["volume"], "driver": "local", "scope": "local",
                           "options": None, "labels": copy.deepcopy(self.inspect_doc["labels"])}
        self.stop_check = lambda: None
        self.sleep_hook = lambda _seconds: None
        self.real_read_text = Path.read_text
        self.real_statvfs = os.statvfs
        self.real_run, self.real_popen = subprocess.run, subprocess.Popen
        previous_umask = os.umask(0o077)
        os.umask(previous_umask)
        self.addCleanup(os.umask, previous_umask)
        self.stack = contextlib.ExitStack()
        self.addCleanup(self.stack.close)
        self.stack.enter_context(mock.patch.object(Path, "read_text", lambda path, *args, **kwargs: self.read_text(path, *args, **kwargs)))
        self.stack.enter_context(mock.patch.object(driver.subprocess, "Popen", side_effect=self.popen))
        self.stack.enter_context(mock.patch.object(driver.subprocess, "run", side_effect=self.run_process))
        self.stack.enter_context(mock.patch.object(driver.os, "statvfs", side_effect=self.statvfs))
        self.stack.enter_context(mock.patch.object(driver.os, "getppid", side_effect=lambda: self.parent_pid))
        self.stack.enter_context(mock.patch.object(driver.time, "time", side_effect=lambda: self.epoch))
        self.stack.enter_context(mock.patch.object(driver.time, "monotonic", side_effect=lambda: self.clock))
        self.stack.enter_context(mock.patch.object(driver.time, "monotonic_ns", side_effect=lambda: int(self.clock * 10 ** 9)))
        self.stack.enter_context(mock.patch.object(driver.time, "sleep", side_effect=self.sleep))
        self.runtime = driver.Runtime(self.out)

    def good_inspect(self):
        a = self.approved
        return {"id": INSTANCE, "name": "/" + a["container"], "running": True, "status": "running",
                "exit_code": 0, "oom": False, "restarts": 0, "pid": 101,
                "image": a["image"], "memory": 16 * GIB, "swap": 16 * GIB, "cpu": 4000000000,
                "network": "bridge", "logdriver": "none",
                "cmd": ["-g", "--", "/bin/bash", "/etc/fdb4/container-entrypoint.sh", "3200"],
                "labels": {"otel283.owner": a["owner"], "otel283.profile": a["profile"],
                           "otel283.run": a["run"], "otel283.session": a["session"], "otel283.nonce": "runtime-fixture"},
                "ports": {f"{port}/tcp": [{"HostIp": "127.0.0.1", "HostPort": str(port)}]
                          for port in (14883, 14884, 14885, 14886)},
                "mounts": [{"Type": "volume", "Name": a["volume"], "Destination": "/var/fdb/data", "RW": True},
                           {"Type": "bind", "Source": str(driver.HERE), "Destination": "/etc/fdb4", "RW": False},
                           {"Type": "bind", "Source": a["probe_binary"], "Destination": "/etc/ingest20k/inode-flags", "RW": False}]}

    def read_text(self, path, *args, **kwargs):
        if str(path).startswith("/proc/"):
            if str(path) not in self.proc or self.proc[str(path)] is None:
                raise FileNotFoundError(str(path))
            return self.proc[str(path)]
        return self.real_read_text(path, *args, **kwargs)

    def statvfs(self, path):
        return self.fs if Path(path) == self.docker_root else self.real_statvfs(path)

    def popen(self, argv, stdout, stderr, **kwargs):
        return FakeProcess(self, argv, stdout, stderr, **kwargs)

    def respond(self, argv):
        self.calls.append(list(argv))
        if argv[:2] == ["git", "-C"]:
            return (self.git_head + "\n" if argv[3:] == ["rev-parse", "HEAD"] else self.git_dirty), 0
        if argv[:2] == ["stat", "-f"]:
            return self.host_fsid + "\n", 0
        if argv[:3] == ["docker", "volume", "inspect"]:
            return json.dumps(self.volume_doc), 0
        if argv[:2] == ["docker", "inspect"]:
            name = argv[-1]
            if name in PROTECTED:
                return json.dumps({"id": PROTECTED[name], "name": "/" + name, "running": False,
                                   "status": "exited", "exit_code": 0, "oom": False, "restarts": 0}), 0
            if name == self.approved["container"]:
                return json.dumps(self.inspect_doc), 0
        if argv[:2] == ["docker", "ps"]:
            return ("\n".join(PROTECTED) + "\n" if "{{.Names}}" in argv else ""), 0
        if argv[:2] == ["docker", "exec"]:
            if self.exec_failure:
                raise self.exec_failure
            command = argv[argv.index("/usr/bin/timeout") + 3:]
            if command[:2] == ["/usr/bin/stat", "-f"]:
                return self.inside_fsid + "\n", 0
            if command[:1] == ["/usr/bin/du"]:
                return self.du[command[-1]], 0
            if command[:1] == ["/usr/bin/fdbcli"]:
                return self.status_raw, 0
            if command == ["/bin/bash", "/etc/fdb4/inode-readonly.sh"]:
                if self.get_failure:
                    raise self.get_failure
                return self.proof, 0
            if command[:2] == ["/bin/bash", "-c"]:
                if "memory.current" in command[2]:
                    return self.cgroup, 0
                if "cpu.stat io.stat" in command[2]:
                    return self.cpu_io, 0
        raise AssertionError("unexpected external process: " + repr(argv))

    def run_process(self, argv, **kwargs):
        if argv[:1] == ["jq"]:
            return subprocess.CompletedProcess(argv, self.qj_exit, json.dumps(self.status_summary), "fixture jq error" if self.qj_exit else "")
        if argv[:2] == ["docker", "inspect"] and argv[-1] == INSTANCE:
            return subprocess.CompletedProcess(argv, 0, json.dumps(self.owned), "")
        if argv[:2] == ["docker", "stop"]:
            self.stop_check()
            self.stops.append(list(argv))
            if len(self.stops) >= self.stop_confirm_after:
                self.owned.update(running=False, status="exited")
            return subprocess.CompletedProcess(argv, self.stop_rc, "fixture stopped\n", "")
        raise AssertionError("unexpected subprocess.run: " + repr(argv))

    def sleep(self, seconds):
        self.sleep_hook(seconds)
        self.clock += seconds
        self.epoch += seconds

    def reseal(self):
        self.approved.update(binary_sha256=digest(self.binary), input_manifest_sha256=digest(self.manifest),
                             probe_sha256=digest(self.probe))
        store(self.approval_path, self.approved)
        self.request["approval_sha256"] = digest(self.approval_path)
        store(self.out / "request.json", self.request)
        sequence = self.runtime.sequence
        self.runtime = driver.Runtime(self.out)
        # Resealed scenarios share this process and its exclusive receipt namespace.
        self.runtime.sequence = sequence

    def install_phase(self):
        record = fixtures.phase()
        record.update({key: self.approved[key] for key in ("source_revision", "binary_sha256", "input_manifest_sha256", "policy_sha256")})
        record.update(started_epoch=2000, deadline_epoch=3200, now=2001,
                      stage_started_epoch=2000, stage_deadline_epoch=2300)
        path = self.out / "phase-load.json"
        store(path, record)
        store(self.out / "active-phase.json", {"stage": "load", "sha256": digest(path)})
        return path, record

    def events(self):
        paths = list(self.out.glob("events-*.jsonl"))
        return [json.loads(row) for path in paths for row in path.read_text().splitlines()]


class ObservationTests(RuntimeFixture):
    def test_host_sample_reads_raw_accounting_and_rejects_missing_malformed_psi(self):
        value = driver.host_sample()
        self.assertEqual(value, {"epoch": 2001, "mem_available_bytes": 32 * GIB, "pswpin": 17, "pswpout": 23,
                                 "cpu_psi_some_avg10": 0.25, "memory_psi_some_avg10": 0.02,
                                 "memory_psi_full_avg10": 0.001, "io_psi_full_avg10": 0.5})
        self.assertEqual(driver.parent_start(123), "77")
        baseline = dict(self.proc)
        for path in baseline:
            if path.endswith("/stat"):
                continue
            self.proc = dict(baseline)
            del self.proc[path]
            with self.assertRaises((OSError, KeyError, ValueError, IndexError)):
                driver.host_sample()
        for path, text in (("/proc/meminfo", "MemTotal: 1 kB\n"),
                           ("/proc/vmstat", "pswpin malformed\npswpout 23\n"),
                           ("/proc/pressure/memory", "some avg10=0.1\n"),
                           ("/proc/pressure/cpu", "some avg10=1\nsome avg10=2\n"),
                           ("/proc/pressure/io", "full avg10=broken\n")):
            self.proc = dict(baseline)
            self.proc[path] = text
            with self.assertRaises((OSError, KeyError, ValueError, IndexError)):
                driver.host_sample()

    def test_allocated_tree_counts_real_blocks_once_and_rejects_symlinks_walk_errors(self):
        tree = self.root / "allocation"
        tree.mkdir()
        (tree / "nested").mkdir()
        (tree / "first").write_bytes(b"a" * 8192)
        (tree / "nested" / "second").write_bytes(b"b" * 16384)
        population = [tree, tree / "nested", tree / "first", tree / "nested" / "second"]
        expected = sum(path.lstat().st_blocks * 512 for path in population)
        self.assertGreater(expected, 0)
        self.assertEqual(driver.allocated_tree(tree), expected)
        for directory in (False, True):
            link = tree / "link"
            link.symlink_to(tree / "nested" if directory else tree / "first")
            with self.assertRaises(ValueError):
                driver.allocated_tree(tree)
            link.unlink()
        def denied(_root, **kwargs):
            kwargs["onerror"](PermissionError("fixture denied traversal"))
            yield
        with mock.patch.object(driver.os, "walk", side_effect=denied):
            with self.assertRaises(PermissionError):
                driver.allocated_tree(tree)

    def test_real_command_receipts_hash_both_streams_exit_and_failed_wait(self):
        self.command_stderr = "fixture diagnostic\n"
        output = self.runtime.command(["git", "-C", str(self.source), "rev-parse", "HEAD"])
        self.assertEqual(output, self.approved["source_revision"] + "\n")
        stem = self.runtime.last_stdout.with_suffix("")
        record = json.loads(stem.with_suffix(".exit.json").read_text())
        self.assertEqual(record["exit"], 0)
        self.assertEqual(record["stdout_sha256"], digest(stem.with_suffix(".stdout")))
        self.assertEqual(record["stderr_sha256"], digest(stem.with_suffix(".stderr")))
        self.assertEqual(stem.with_suffix(".stderr").read_text(), self.command_stderr)
        self.wait_failure = subprocess.TimeoutExpired("git", 1)
        with self.assertRaises(subprocess.TimeoutExpired):
            self.runtime.command(["git", "-C", str(self.source), "rev-parse", "HEAD"], timeout=1)
        self.assertEqual(len(self.killed), 1)

    def test_constructor_requires_private_owned_root_and_exact_sealed_output(self):
        self.out.chmod(0o755)
        with self.assertRaises(ValueError):
            driver.Runtime(self.out)
        self.out.chmod(0o700)
        self.approved["out"] = str(self.root / "other" / self.out.name)
        store(self.approval_path, self.approved)
        self.request["approval_sha256"] = digest(self.approval_path)
        store(self.out / "request.json", self.request)
        with self.assertRaises(ValueError):
            driver.Runtime(self.out)

    def test_real_command_nonzero_retains_raw_receipts_and_exit(self):
        original = self.respond
        self.respond = lambda argv: (original(argv)[0], 7)
        with self.assertRaises(ValueError):
            self.runtime.command(["git", "-C", str(self.source), "rev-parse", "HEAD"])
        records = list(self.out.glob("cmd-*.exit.json"))
        self.assertEqual(len(records), 1)
        self.assertEqual(json.loads(records[0].read_text())["exit"], 7)
        self.assertTrue(list(self.out.glob("cmd-*.stdout")))

    def test_identity_positive_and_each_inspect_binding_rejection(self):
        self.assertEqual(self.runtime.identity()["id"], INSTANCE)
        self.assertTrue(any(call[:2] == ["docker", "inspect"] for call in self.calls))
        paths = {"id": "b" * 64, "image": "wrong", "memory": 1, "swap": 1, "cpu": 1,
                 "network": "host", "logdriver": "json-file", "cmd": [], "ports": {},
                 "oom": True, "restarts": 1, "running": False, "status": "dead", "pid": 0}
        for key, value in paths.items():
            self.inspect_doc = self.good_inspect()
            self.inspect_doc[key] = value
            with self.assertRaises((ValueError, KeyError, TypeError)):
                self.runtime.identity()
        for key in self.good_inspect()["labels"]:
            self.inspect_doc = self.good_inspect()
            self.inspect_doc["labels"][key] = "replacement"
            with self.assertRaises(ValueError):
                self.runtime.identity()
        for index in range(3):
            for field, value in (("Destination", "/wrong"), ("RW", index != 0),
                                 ("Type", "tmpfs"), ("Source" if index else "Name", "replacement")):
                self.inspect_doc = self.good_inspect()
                self.inspect_doc["mounts"][index][field] = value
                with self.assertRaises(ValueError):
                    self.runtime.identity()
        self.inspect_doc = self.good_inspect()
        self.inspect_doc["mounts"].pop()
        with self.assertRaises(ValueError):
            self.runtime.identity()
        self.inspect_doc = self.good_inspect()
        self.inspect_doc.update(running=False, status="created", pid=0)
        self.runtime.identity(running=False)
        store(self.out / "instance.json", {"instance": "b" * 64})
        with self.assertRaises(ValueError):
            self.runtime.identity(running=False)

    def test_volume_positive_default_metadata_and_all_label_failures(self):
        for options in (None, {}):
            self.volume_doc["options"] = options
            self.runtime.volume()
        for key, value in (("name", "old-volume"), ("driver", "other"), ("scope", "global"),
                           ("options", {"device": "old-data"})):
            baseline = copy.deepcopy(self.volume_doc)
            self.volume_doc[key] = value
            with self.assertRaises(ValueError):
                self.runtime.volume()
            self.volume_doc = baseline
        for key in list(self.volume_doc["labels"]):
            baseline = copy.deepcopy(self.volume_doc)
            del self.volume_doc["labels"][key]
            with self.assertRaises(ValueError):
                self.runtime.volume()
            self.volume_doc = baseline

    def test_frozen_bytes_source_head_manifest_and_input_population(self):
        self.runtime.check_frozen(full=True)
        self.assertEqual(sum(call[:2] == ["git", "-C"] for call in self.calls), 2)
        first_receipt = sorted(self.out.glob("cmd-*.command.json"))[0]
        first_receipt_bytes = first_receipt.read_bytes()
        for path in (self.out / "request.json", self.approval_path, self.binary, self.manifest, self.probe):
            original = path.read_bytes()
            path.write_bytes(original + b" ")
            with self.assertRaises(ValueError):
                self.runtime.check_frozen()
            path.write_bytes(original)
        self.git_head = "2" * 40
        with self.assertRaises(ValueError):
            self.runtime.check_frozen(full=True)
        self.git_head = self.approved["source_revision"]
        self.git_dirty = " M input.go\n"
        with self.assertRaises(ValueError):
            self.runtime.check_frozen(full=True)
        self.git_dirty = ""
        original_source = self.source_file.read_bytes()
        self.source_file.write_bytes(b"changed application source\n")
        self.runtime.check_frozen(full=False)
        with self.assertRaises(ValueError):
            self.runtime.check_frozen(full=True)
        self.source_file.write_bytes(original_source)
        valid = digest(self.source_file) + "  input.go\n"
        for text in ("", "not a hash record\n", valid + valid,
                     "a" * 64 + "  missing.go\n", "a" * 64 + "  .\n", "a" * 64 + "  input.go\n"):
            self.manifest.write_text(text)
            self.reseal()
            with self.assertRaises((ValueError, FileNotFoundError, IsADirectoryError)):
                self.runtime.check_frozen(full=True)
        self.manifest.write_text(digest(self.source_file) + "  " + str(self.source_file) + "\n")
        self.reseal()
        self.runtime.check_frozen(full=True)
        self.assertEqual(first_receipt.read_bytes(), first_receipt_bytes,
                         "resealing must preserve previously retained command receipts")
        # A different immutable operations/probe-source commitment is not a valid seal.
        self.runtime.frozen = "0" * 64
        with self.assertRaises(ValueError):
            self.runtime.check_frozen()
        self.runtime.frozen = driver.p.runtime_sha(driver.HERE)
        self.runtime.approval["probe_source_sha256"] = "0" * 64
        with self.assertRaises(ValueError):
            self.runtime.check_frozen()

    def test_phase_real_pointer_sealed_binding_deadline_and_removal(self):
        self.assertIsNone(self.runtime.phase())
        path, record = self.install_phase()
        self.assertIsNone(self.runtime.phase())
        original = path.read_bytes()
        path.write_bytes(original + b" ")
        with self.assertRaises(ValueError):
            self.runtime.phase()
        path.write_bytes(original)
        for key, value in (("instance", "b" * 64), ("source_revision", "2" * 40),
                           ("binary_sha256", "0" * 64), ("input_manifest_sha256", "0" * 64),
                           ("policy_sha256", "0" * 64), ("count", 10000000), ("stage_deadline_epoch", 2001)):
            changed = dict(record)
            changed[key] = value
            store(path, changed)
            store(self.out / "active-phase.json", {"stage": "load", "sha256": digest(path)})
            with self.assertRaises(ValueError):
                self.runtime.phase()
        store(self.out / "active-phase.json", {"stage": "unknown", "sha256": digest(path)})
        with self.assertRaises(ValueError):
            self.runtime.phase()
        (self.out / "active-phase.json").unlink()
        with self.assertRaises(ValueError):
            self.runtime.phase()

    def test_resources_parses_both_du_populations_cgroup_pids_and_host_blocks(self):
        receipts = driver.allocated_tree(self.out)
        value = self.runtime.resources()
        self.assertGreater(value["host_receipt_bytes"], receipts)
        self.assertEqual(value["allocated_bytes"], 3 * GIB + value["host_receipt_bytes"])
        self.assertEqual(value["cgroup_bytes"], 2 * GIB)
        self.assertEqual(value["host_available_bytes"], 32 * GIB)
        self.assertEqual(value["available_bytes"], 500 * GIB)
        self.assertEqual(self.runtime.initial_pids, [101, 102, 103, 104])
        du_calls = [call for call in self.calls if "/usr/bin/du" in call]
        self.assertEqual([call[-1] for call in du_calls], ["/", "/var/fdb/data"])
        event = [event for event in self.events() if event["type"] == "raw_cpu_io"][-1]
        self.assertEqual(event["raw_sha256"], digest(self.out / event["raw_path"]))
        self.assertEqual((self.out / event["raw_path"]).read_text(), self.cpu_io)
        self.assertEqual(event["scope"], "whole_container_including_bootstrap_DD_monitor_and_workload")

    def test_resources_rejects_missing_oom_pid_replacement_du_and_mount_accounting(self):
        original = self.cgroup
        malformed = ["", "bad\n", original.replace("oom 0\n", ""),
                     original.replace("oom_kill 0", "oom_kill 1"), original.replace("oom_group_kill 0", "oom_group_kill 1"),
                     original.replace("oom 0", "oom 1"), original.replace("low 0", "low broken"),
                     original.replace("2147483648", str(16 * GIB * 9 // 10))]
        for text in malformed:
            self.cgroup = text
            self.runtime.initial_pids = None
            with self.assertRaises((ValueError, IndexError)):
                self.runtime.resources()
        self.cgroup = original
        self.runtime.resources()
        for pids in ("101\n102\n103\n", "101\n102\n103\n105\n", "104\n103\n102\n101\n"):
            self.cgroup = original[:original.index("101\n")] + pids
            with self.assertRaises(ValueError):
                self.runtime.resources()
        self.runtime.initial_pids = None
        self.clock = 1300
        self.cgroup = original[:original.index("101\n")]
        with self.assertRaises(ValueError):
            self.runtime.resources()
        self.clock = 1001
        self.runtime.resources()  # bounded bootstrap may temporarily have fewer than four PIDs
        self.cgroup = original
        for path in ("/", "/var/fdb/data"):
            baseline = self.du[path]
            for text in ("", "malformed", "-1\t" + path + "\n", "0\t/wrong\n", baseline + baseline):
                self.du[path] = text
                with self.assertRaises((ValueError, IndexError)):
                    self.runtime.resources()
            self.du[path] = baseline
        self.inside_fsid = ""
        with self.assertRaises(ValueError):
            self.runtime.resources()
        self.inside_fsid = "different-filesystem"
        with self.assertRaises(ValueError):
            self.runtime.resources()
        self.inside_fsid = self.host_fsid
        self.fs.f_blocks -= 1
        with self.assertRaises(ValueError):
            self.runtime.resources()
        self.fs.f_blocks += 1
        self.fs.f_bavail = 1
        with self.assertRaises(ValueError):
            self.runtime.resources()

    def test_resources_host_low_ram_wrong_device_and_growth_stop(self):
        original = self.proc["/proc/meminfo"]
        self.proc["/proc/meminfo"] = "MemAvailable: 8388607 kB\n"
        with self.assertRaises(ValueError):
            self.runtime.resources()
        self.proc["/proc/meminfo"] = original
        original_stat = os.stat
        def changed_device(path, *args, **kwargs):
            info = original_stat(path, *args, **kwargs)
            return (types.SimpleNamespace(st_dev=info.st_dev + 1)
                    if isinstance(path, (str, os.PathLike)) and Path(path) == self.out else info)
        with mock.patch.object(driver.os, "stat", side_effect=changed_device):
            with self.assertRaises(ValueError):
                self.runtime.resources()
        self.du["/"] = str(64 * GIB) + "\t/\n"
        with self.assertRaises(ValueError):
            self.runtime.resources()
        self.du["/"] = "1073741824\t/\n"
        original_lstat = Path.lstat
        def receipt_blocks(path, *args, **kwargs):
            info = original_lstat(path, *args, **kwargs)
            return (types.SimpleNamespace(st_mode=info.st_mode, st_blocks=GIB // 512)
                    if path == self.out else info)
        with mock.patch.object(Path, "lstat", receipt_blocks):
            with self.assertRaises(ValueError):
                self.runtime.resources()
        self.exec_failure = OSError("fixture failed docker exec")
        with self.assertRaises(OSError):
            self.runtime.resources()


class InventoryTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name).resolve()
        (self.root / "nested").mkdir()
        (self.root / "ingest.json").write_text('{"mode":"ingest","acknowledged":100000}\n')
        (self.root / "load.stdout").write_text("acknowledged rows=100000\n")
        (self.root / "load.stderr").write_text("retained diagnostics\n")
        store(self.root / "load.exit.json", {"exit": 0, "stdout_sha256": digest(self.root / "load.stdout"),
                                            "stderr_sha256": digest(self.root / "load.stderr")})
        (self.root / "nested" / "profile.json").write_text('{"samples":3}\n')
        self.commitments = {"ingest.json": digest(self.root / "ingest.json")}

    def test_inventory_nonempty_exact_bytes_sizes_hashes_and_no_rewrites(self):
        before = {str(path.relative_to(self.root)): path.read_bytes() for path in self.root.rglob("*") if path.is_file()}
        result = driver.final_inventory(self.root, self.commitments)
        self.assertEqual(result["count"], 5)
        self.assertEqual(result["files"], [{"path": name, "size": len(data),
                                          "sha256": hashlib.sha256(data).hexdigest()}
                                         for name, data in sorted(before.items())])
        self.assertEqual(before, {str(path.relative_to(self.root)): path.read_bytes()
                                  for path in self.root.rglob("*") if path.is_file()})
        self.assertFalse((self.root / "final-inventory.json").exists())

    def test_inventory_rejects_tampered_missing_bound_evidence_and_raw_command_streams(self):
        for name in ("ingest.json", "load.stdout", "load.stderr"):
            path = self.root / name
            original = path.read_bytes()
            path.write_bytes(original + b"changed")
            with self.assertRaises(ValueError):
                driver.final_inventory(self.root, self.commitments)
            path.write_bytes(original)
            path.unlink()
            with self.assertRaises(ValueError):
                driver.final_inventory(self.root, self.commitments)
            path.write_bytes(original)
        original = (self.root / "load.exit.json").read_bytes()
        (self.root / "load.exit.json").write_text("{")
        with self.assertRaises(ValueError):
            driver.final_inventory(self.root, self.commitments)
        (self.root / "load.exit.json").write_bytes(original)

    def test_inventory_rejects_symlinks_nonregular_preexisting_completion_and_empty_population(self):
        for directory in (False, True):
            alias = self.root / "alias"
            alias.symlink_to(self.root / "nested" if directory else self.root / "load.stdout")
            with self.assertRaises(ValueError):
                driver.final_inventory(self.root, self.commitments)
            alias.unlink()
        fifo = self.root / "fifo"
        os.mkfifo(fifo)
        with self.assertRaises(ValueError):
            driver.final_inventory(self.root, self.commitments)
        fifo.unlink()
        for name in ("complete.json", "final-inventory.json"):
            path = self.root / name
            path.write_text("{}")
            with self.assertRaises(ValueError):
                driver.final_inventory(self.root, self.commitments)
            path.unlink()
        empty = self.root / "empty"
        empty.mkdir()
        with self.assertRaises(ValueError):
            driver.final_inventory(empty, {})

    def test_inventory_rechecks_population_and_bytes_before_returning(self):
        original_rglob = Path.rglob
        def population_changed(path, pattern, *args, **kwargs):
            (self.root / "late.raw").write_text("external writer\n")
            return original_rglob(path, pattern, *args, **kwargs)
        with mock.patch.object(Path, "rglob", population_changed):
            with self.assertRaises(ValueError):
                driver.final_inventory(self.root, self.commitments)
        self.assertTrue((self.root / "late.raw").exists())
        (self.root / "late.raw").unlink()
        original_sha = driver.p.sha
        calls = []
        target = self.root / "nested" / "profile.json"
        def bytes_changed(path):
            if Path(path) == target:
                calls.append(path)
                if len(calls) == 2:
                    target.write_text('{"samples":4}\n')
            return original_sha(path)
        with mock.patch.object(driver.p, "sha", side_effect=bytes_changed):
            with self.assertRaises(ValueError):
                driver.final_inventory(self.root, self.commitments)
        self.assertEqual(len(calls), 2)
        self.assertEqual(json.loads(target.read_text())["samples"], 4)


class ShutdownTests(RuntimeFixture):
    def test_nonzero_stop_command_is_contained_but_stays_failed_on_later_clean_inspect(self):
        self.stop_rc = 7
        result = self.runtime.stop()
        self.assertIs(result["running"], False)
        self.assertIs(result["shutdown_ok"], False)
        self.assertTrue(result["shutdown_failures"])
        self.assertFalse(driver.p.clean_stop(result)["clean_stop"])
        self.stop_rc = 0
        later = self.runtime.stop()
        self.assertIs(later["shutdown_ok"], False)
        self.assertEqual(len(self.stops), 1, "already exited instance must not be stopped again")
        self.assertFalse(driver.p.clean_stop(later)["clean_stop"])
        self.assertTrue(list(self.out.glob("shutdown.failed-*.json")))

    def test_transient_receipt_failure_survives_recovery_and_fresh_runtime(self):
        original_open = __import__("builtins").open
        def fail_event(path, *args, **kwargs):
            if str(path).endswith(".jsonl"):
                raise OSError("fixture ENOSPC on stop event")
            return original_open(path, *args, **kwargs)
        with mock.patch("builtins.open", side_effect=fail_event):
            with contextlib.redirect_stderr(io.StringIO()):
                result = self.runtime.stop()
        self.assertIs(result["running"], False)
        self.assertIs(result["shutdown_ok"], False)
        later = self.runtime.stop()
        self.assertIs(later["shutdown_ok"], False)
        self.assertFalse(driver.p.clean_stop(later)["clean_stop"])
        # Each observer must retain the persisted failure, not merely its local flag.
        fresh = driver.Runtime(self.out)
        observed = fresh.stop()
        self.assertIs(observed["shutdown_ok"], False)
        self.assertFalse(driver.p.clean_stop(observed)["clean_stop"])


class LeaseTests(unittest.TestCase):
    def test_default_path_matches_shell_fallback_including_empty_overrides(self):
        self.assertEqual(driver.lease_path({}), Path("/tmp/fdb-heavy-lane.lock"))
        self.assertEqual(driver.lease_path({"FDB_LANE_LOCK": "", "TMPDIR": ""}), Path("/tmp/fdb-heavy-lane.lock"))
        with tempfile.TemporaryDirectory() as name:
            root = Path(name).resolve()
            settings = [{"TMPDIR": str(root)}, {"TMPDIR": str(root), "FDB_LANE_LOCK": ""},
                        {"TMPDIR": str(root / "unused"), "FDB_LANE_LOCK": str(root / "explicit.lock")}]
            for selected in settings:
                expected = Path(selected.get("FDB_LANE_LOCK") or str(root / "fdb-heavy-lane.lock"))
                self.assertEqual(driver.lease_path(selected), expected)
                fd = os.open(expected, os.O_RDWR | os.O_CREAT, 0o600)
                try:
                    fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
                    environment = dict(os.environ, PATH=os.defpath)
                    environment.pop("FDB_LANE_LOCK", None)
                    environment.pop("TMPDIR", None)
                    environment.update(selected)
                    shell = 'lock=${FDB_LANE_LOCK:-${TMPDIR:-/tmp}/fdb-heavy-lane.lock}; printf "%s\\n" "$lock"; exec 9>"$lock"; flock -n 9'
                    blocked = subprocess.run(["bash", "-c", shell], env=environment, capture_output=True, text=True, timeout=5)
                    self.assertEqual(blocked.stdout.strip(), str(expected))
                    self.assertEqual(blocked.returncode, 1, blocked.stderr)
                    fcntl.flock(fd, fcntl.LOCK_UN)
                    free = subprocess.run(["bash", "-c", shell], env=environment, capture_output=True, text=True, timeout=5)
                    self.assertEqual(free.returncode, 0, free.stderr)
                finally:
                    os.close(fd)

    def test_check_lease_real_descriptor_path_and_sealed_tuple(self):
        with tempfile.TemporaryDirectory() as name:
            root = Path(name).resolve()
            path = root / "lease"
            path.write_text("")
            with path.open() as file:
                info = os.fstat(file.fileno())
                lease = {"path": str(path), "device": info.st_dev, "inode": info.st_ino}
                driver.check_lease(file.fileno(), lease)
                for key, value in (("path", "relative-lock"), ("device", info.st_dev + 1), ("inode", info.st_ino + 1)):
                    bad = dict(lease)
                    bad[key] = value
                    with self.assertRaises(ValueError):
                        driver.check_lease(file.fileno(), bad)
                for key in lease:
                    bad = dict(lease)
                    del bad[key]
                    with self.assertRaises(KeyError):
                        driver.check_lease(file.fileno(), bad)
                with self.assertRaises(OSError):
                    driver.check_lease(-1, lease)
                alias = root / "symlink"
                alias.symlink_to(path)
                with self.assertRaises(ValueError):
                    driver.check_lease(file.fileno(), dict(lease, path=str(alias)))
                other = root / "other"
                other.write_text("")
                with other.open() as wrong:
                    with self.assertRaises(ValueError):
                        driver.check_lease(wrong.fileno(), lease)
                path.unlink()
                with self.assertRaises(FileNotFoundError):
                    driver.check_lease(file.fileno(), lease)
                path.write_text("")
                with self.assertRaises(ValueError):
                    driver.check_lease(file.fileno(), lease)
            directory_fd = os.open(root, os.O_RDONLY)
            try:
                info = os.fstat(directory_fd)
                with self.assertRaises(ValueError):
                    driver.check_lease(directory_fd, {"path": str(root), "device": info.st_dev, "inode": info.st_ino})
            finally:
                os.close(directory_fd)


def prepare_monitor(case):
    path = case.root / "lease.lock"
    fd = os.open(path, os.O_RDWR | os.O_CREAT, 0o600)
    real_flock = fcntl.flock
    real_flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
    case.addCleanup(os.close, fd)
    alias = case.root / "leased-inode"
    os.link(path, alias)
    info = os.fstat(fd)
    lease = {"path": str(path), "pid": 123, "start_ticks": "77",
             "device": info.st_dev, "inode": info.st_ino, "descriptor": fd}
    store(case.out / "lease.json", lease)
    store(case.out / "supervisor-heartbeat.json", {"pid": 123, "instance": INSTANCE, "monotonic": 1000})
    store(case.out / "bootstrap.ready.json", {"fixture": True})
    case.install_phase()
    case.held_checks = 0
    def held():
        case.assertEqual(os.fstat(fd).st_ino, info.st_ino, "inherited lease closed before containment")
        contender = os.open(alias, os.O_RDWR)
        try:
            with case.assertRaises(BlockingIOError):
                real_flock(contender, fcntl.LOCK_EX | fcntl.LOCK_NB)
        finally:
            os.close(contender)
        case.held_checks += 1
    case.stop_check = held
    return fd, path, lease, held


class MonitorTests(unittest.TestCase):
    @contextlib.contextmanager
    def scenario(self):
        case = RuntimeFixture()
        try:
            case.setUp()
            yield case
        finally:
            case.doCleanups()

    def assert_contained(self, case, fd, held):
        self.assertEqual(case.stops[-1], ["docker", "stop", "--time", "60", INSTANCE])
        self.assertIs(case.owned["running"], False)
        held()
        self.assertGreaterEqual(case.held_checks, 2)
        self.assertGreaterEqual(os.fstat(fd).st_ino, 1)
        self.assertFalse((case.out / "complete.json").exists())
        stops = [event for event in case.events() if event["type"] == "stop"]
        self.assertTrue(stops)
        self.assertIs(stops[-1]["confirmed"], True)

    def test_real_monitor_positive_iteration_observes_all_raw_populations_and_stops_owned(self):
        with self.scenario() as case:
            fd, _path, _lease, held = prepare_monitor(case)
            case.sleep_hook = lambda _seconds: store(case.out / "stop.request.json", {})
            with mock.patch.object(driver.fcntl, "flock", side_effect=AssertionError("monitor reacquired lease")):
                driver.monitor(case.out, fd)
            beat = json.loads((case.out / "heartbeat.json").read_text())
            self.assertEqual(beat, {"monotonic": 1001, "pid": os.getpid(), "instance": INSTANCE})
            self.assert_contained(case, fd, held)
            for event_type in ("capacity", "raw_cpu_io", "status-validator", "status"):
                self.assertTrue([event for event in case.events() if event["type"] == event_type], event_type)
            self.assertTrue(any("/etc/fdb4/inode-readonly.sh" in argv for argv in case.calls))
            self.assertTrue(any(argv[:3] == ["docker", "volume", "inspect"] for argv in case.calls))
            self.assertEqual(len([event for event in case.events() if event["type"] == "status"]), 1)

    def test_monitor_initial_lease_input_failures_always_contain_under_original_lock(self):
        for fault in ("missing-record", "malformed-record", "missing-path", "replaced-path", "device", "inode", "invalid-fd", "wrong-fd"):
            with self.subTest(fault=fault), self.scenario() as case:
                fd, path, lease, held = prepare_monitor(case)
                passed_fd = fd
                if fault == "missing-record":
                    (case.out / "lease.json").unlink()
                elif fault == "malformed-record":
                    (case.out / "lease.json").write_text("{")
                elif fault in ("missing-path", "replaced-path"):
                    path.unlink()
                    if fault == "replaced-path":
                        path.write_text("")
                elif fault in ("device", "inode"):
                    lease[fault] += 1
                    store(case.out / "lease.json", lease)
                elif fault == "invalid-fd":
                    passed_fd = -1
                else:
                    wrong = case.root / "wrong.lock"
                    wrong.write_text("")
                    passed_fd = os.open(wrong, os.O_RDWR)
                    case.addCleanup(os.close, passed_fd)
                with self.assertRaises((ValueError, OSError, KeyError, TypeError)):
                    driver.monitor(case.out, passed_fd)
                self.assertFalse((case.out / "heartbeat.json").exists())
                self.assert_contained(case, fd, held)

    def test_monitor_continuously_rechecks_lease_parent_clocks_and_present_heartbeats(self):
        faults = ("lost-path", "replaced-path", "parent-pid", "parent-start", "missing-parent",
                  "deadline", "clock-jump", "missing-heartbeat", "malformed-heartbeat", "stale-heartbeat",
                  "future-heartbeat", "heartbeat-pid", "heartbeat-instance", "heartbeat-field")
        for fault in faults:
            with self.subTest(fault=fault), self.scenario() as case:
                fd, path, _lease, held = prepare_monitor(case)
                prior = []
                def fail_after_success(_seconds):
                    self.assertFalse(prior, "failure must not reach another successful iteration")
                    prior.append((case.out / "heartbeat.json").read_bytes())
                    heartbeat = {"pid": 123, "instance": INSTANCE, "monotonic": 1001}
                    if fault in ("lost-path", "replaced-path"):
                        path.unlink()
                        if fault == "replaced-path":
                            path.write_text("")
                    elif fault == "parent-pid":
                        case.parent_pid = 124
                    elif fault == "parent-start":
                        case.proc["/proc/123/stat"] = case.proc["/proc/123/stat"].replace("77", "78")
                    elif fault == "missing-parent":
                        del case.proc["/proc/123/stat"]
                    elif fault == "deadline":
                        case.clock, case.epoch = 2190, 3190
                    elif fault == "clock-jump":
                        case.epoch += 3
                    elif fault == "missing-heartbeat":
                        (case.out / "supervisor-heartbeat.json").unlink()
                    elif fault == "malformed-heartbeat":
                        (case.out / "supervisor-heartbeat.json").write_text("{")
                    else:
                        if fault == "stale-heartbeat":
                            heartbeat["monotonic"] = 921
                        elif fault == "future-heartbeat":
                            heartbeat["monotonic"] = 1012
                        elif fault == "heartbeat-pid":
                            heartbeat["pid"] = 124
                        elif fault == "heartbeat-instance":
                            heartbeat["instance"] = "b" * 64
                        elif fault == "heartbeat-field":
                            del heartbeat["monotonic"]
                        store(case.out / "supervisor-heartbeat.json", heartbeat)
                case.sleep_hook = fail_after_success
                with self.assertRaises((ValueError, OSError, KeyError, TypeError)):
                    driver.monitor(case.out, fd)
                self.assertEqual(len(prior), 1)
                self.assertEqual((case.out / "heartbeat.json").read_bytes(), prior[0], "failed guard renewed admission heartbeat")
                self.assert_contained(case, fd, held)

    def test_monitor_real_observation_phase_status_and_get_failure_preserve_last_heartbeat(self):
        for fault in ("frozen-binary", "identity", "volume", "resources", "phase", "status", "batch-warning", "get-command", "get-proof"):
            with self.subTest(fault=fault), self.scenario() as case:
                fd, _path, _lease, held = prepare_monitor(case)
                prior = []
                def fail_after_success(_seconds):
                    self.assertFalse(prior)
                    prior.append((case.out / "heartbeat.json").read_bytes())
                    if fault == "frozen-binary":
                        case.binary.write_bytes(b"changed application bytes")
                    elif fault == "identity":
                        case.inspect_doc["memory"] = 1
                    elif fault == "volume":
                        case.volume_doc["labels"]["otel283.nonce"] = "replacement"
                    elif fault == "resources":
                        case.cgroup = case.cgroup.replace("oom_kill 0", "oom_kill 1")
                    elif fault == "phase":
                        (case.out / "active-phase.json").unlink()
                    elif fault in ("status", "batch-warning"):
                        case.qj_exit = 1
                        if fault == "batch-warning":
                            raw = fixtures.status()
                            raw["cluster"]["messages"] = [{"name": "batch_priority_transaction_start_probe_timeout"}]
                            case.status_raw = json.dumps(raw)
                    else:
                        case.clock += 51
                        case.epoch += 51
                        if fault == "get-command":
                            case.get_failure = OSError("fixture GET command failed")
                        else:
                            case.proof = "{}\n"
                case.sleep_hook = fail_after_success
                with self.assertRaises((ValueError, OSError, KeyError, TypeError)):
                    driver.monitor(case.out, fd)
                self.assertEqual(len(prior), 1)
                self.assertEqual((case.out / "heartbeat.json").read_bytes(), prior[0])
                self.assert_contained(case, fd, held)

    def test_monitor_six_raw_backlog_observations_reject_before_sixth_heartbeat(self):
        with self.scenario() as case:
            fd, _path, _lease, held = prepare_monitor(case)
            case.status_summary.update(queue_bytes=536870913, durability_lag_seconds=61)
            iterations = []
            def bounded(_seconds):
                iterations.append((case.out / "heartbeat.json").read_bytes())
                self.assertLessEqual(len(iterations), 5, "backlog guard failed to terminate")
            case.sleep_hook = bounded
            with self.assertRaisesRegex(ValueError, "six consecutive"):
                driver.monitor(case.out, fd)
            statuses = [event for event in case.events() if event["type"] == "status"]
            self.assertEqual([event["backlog_streak"] for event in statuses], [1, 2, 3, 4, 5])
            self.assertEqual((case.out / "heartbeat.json").read_bytes(), iterations[-1])
            self.assertEqual(sum(event["type"] == "status-validator" for event in case.events()), 6)
            self.assert_contained(case, fd, held)

    def test_monitor_holds_inherited_lock_through_unconfirmed_stop_retry(self):
        with self.scenario() as case:
            fd, _path, _lease, held = prepare_monitor(case)
            case.stop_confirm_after = 2
            sleeps = []
            def request_then_verify(seconds):
                sleeps.append(seconds)
                self.assertLessEqual(len(sleeps), 3, "unbounded confirmation retry")
                held()
                store(case.out / "stop.request.json", {})
            case.sleep_hook = request_then_verify
            with contextlib.redirect_stderr(io.StringIO()):
                # Retrying containment cannot turn its failed first confirmation into success.
                with self.assertRaises(ValueError):
                    driver.monitor(case.out, fd)
            self.assertEqual(len(case.stops), 2)
            self.assertIn(5, sleeps)
            self.assert_contained(case, fd, held)


if __name__ == "__main__":
    unittest.main()
