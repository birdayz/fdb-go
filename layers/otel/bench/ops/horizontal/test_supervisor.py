#!/usr/bin/env python3
"""Offline managed USER-watch safety checks; no host or provider effects."""
from contextlib import ExitStack
import copy
import hashlib
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest import mock

import supervisor as s


BOOT_ID = "11111111-2222-3333-4444-555555555555"


def digest(path):
    return hashlib.sha256(Path(path).read_bytes()).hexdigest()


def write_json(path, value):
    path = Path(path)
    path.write_text(json.dumps(value, sort_keys=True) + "\n")
    path.chmod(0o600)


class Clock:
    def __init__(self, epoch=1100):
        self.epoch, self.sleeps, self.on_sleep = epoch, [], None

    def __call__(self):
        return self.epoch

    def sleep(self, seconds):
        if not 0 < seconds <= 5:
            raise AssertionError("unbounded or zero fake sleep: " + str(seconds))
        self.sleeps.append(seconds)
        self.epoch += seconds
        if len(self.sleeps) > 100:
            raise AssertionError("watch did not stop within the fake loop bound")
        if self.on_sleep is not None:
            self.on_sleep()


class FakeManager:
    def __init__(self, units=None):
        self.units = {} if units is None else units
        self.calls, self.mutations = [], []
        self.failure = None

    def __call__(self, argv, *, timeout):
        argv = list(argv)
        self.calls.append((argv, timeout))
        if argv[0] != "/usr/bin/systemctl" or "--user" not in argv or not 0 < timeout <= 5:
            raise AssertionError("unexpected or unbounded manager operation: " + repr(argv))
        if self.failure is not None:
            return self.failure
        units = [x for x in argv if x.endswith(".service")]
        if len(units) != 1:
            raise AssertionError("manager request is not exact-unit: " + repr(argv))
        if "show" in argv:
            if units[0] not in self.units:
                return 1, "LoadState=not-found\n", "unknown fake unit"
            props = self.units[units[0]]
            return 0, "".join(key + "=" + str(value) + "\n" for key, value in props.items()), ""
        if "set-property" in argv and "--runtime" in argv:
            self.mutations.append(argv)
            return 0, "", ""
        raise AssertionError("install/start/spawn or unexpected manager effect: " + repr(argv))


class FakeProc:
    def __init__(self, root):
        self.root = Path(root)
        boot = self.root / "sys/kernel/random/boot_id"
        boot.parent.mkdir(mode=0o700, parents=True)
        boot.write_text(BOOT_ID + "\n")

    def add(self, pid, *, start=5000, ppid=1, pgid=None, uid=None,
            exe="/usr/bin/python3", argv=None, cgroup="/user.slice/watch.service"):
        directory = self.root / str(pid)
        directory.mkdir(mode=0o700)
        # Tail begins at Linux stat field 3; starttime is field 22.
        fields = ["S", str(ppid), str(pid if pgid is None else pgid), str(pid), "0", "0"] + ["0"] * 46
        fields[19] = str(start)
        (directory / "stat").write_text(str(pid) + " (python ) worker) " + " ".join(fields) + "\n")
        uid = os.getuid() if uid is None else uid
        (directory / "status").write_text("Name:\tpython\nState:\tS (sleeping)\nPPid:\t" + str(ppid)
            + "\nUid:\t" + "\t".join([str(uid)] * 4) + "\n")
        (directory / "cmdline").write_bytes(b"\0".join(x.encode() for x in (argv or [exe])) + b"\0")
        (directory / "cgroup").write_text("0::" + cgroup + "\n")
        (directory / "exe").symlink_to(exe)
        return directory


class SupervisorFixture(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(prefix="horizontal-supervisor-test-")
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.clock = Clock()
        self.effects = ExitStack()
        self.addCleanup(self.effects.close)
        self.effects.enter_context(mock.patch.dict(os.environ, HOME=str(self.root / "home"), PATH="/usr/bin:/bin"))
        self.repo = self.root / "helper-source"
        for name in s.FILES:
            target = self.repo / name
            target.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
            target.write_bytes((s.REPO / name).read_bytes())
        self.effects.enter_context(mock.patch.object(s, "REPO", self.repo))
        for name in ("run", "Popen", "call", "check_call", "check_output"):
            self.effects.enter_context(mock.patch.object(subprocess, name,
                side_effect=AssertionError("real subprocess forbidden: " + name)))
        for name in ("kill", "killpg", "system"):
            self.effects.enter_context(mock.patch.object(os, name,
                side_effect=AssertionError("real host effect forbidden: " + name)))
        source = self.root / "source"
        source.mkdir(mode=0o700)
        (source / "sealed.py").write_text("SEALED = True\n")
        self.plan = dict(schema="otel_native_scaling_v1", approved=True,
            campaign="native-test", prefix="native-test", location="hel1", server_type="ccx43",
            replication="single", stages=[[1, 1], [2, 2], [3, 3]], count=100000,
            measured_count=375000, measured_seconds=120, prior_rows_upper=0, batch=64,
            workers=8, gomaxprocs=4, storage_process_count=3, storage_max_outstanding=64,
            storage_memory_profile="scaling3ss3g", source_revision="a" * 40,
            first_epoch=1000, deadline_epoch=8200, controller_cidr="192.0.2.1/32",
            source_root=str(source), storage_options=dict(record_compression="brotli-4",
                posting_value_format="tuple", bulk_index_conflicts=True, hash_bytes=8),
            managed_watch=dict(root=str(self.root / "watch"), output=str(self.root / "run")))
        for name in ("binary", "fdbserver", "fdbcli", "hcloud", "controller_private_key",
                     "controller_public_key"):
            path = self.root / name
            path.write_text("sealed " + name + "\n")
            path.chmod(0o700 if name in ("binary", "fdbserver", "fdbcli", "hcloud") else 0o600)
            self.plan[name] = str(path)
            if name in ("binary", "fdbserver", "fdbcli"):
                self.plan[name + "_sha256"] = digest(path)
        manifest = self.root / "source-manifest.sha256"
        manifest.write_text(digest(source / "sealed.py") + "  sealed.py\n")
        manifest.chmod(0o600)
        self.plan.update(source_manifest=str(manifest), input_manifest_sha256=digest(manifest))
        self.plan_path = self.root / "plan.json"
        write_json(self.plan_path, self.plan)
        self.watch_root = Path(self.plan["managed_watch"]["root"])
        self.output = Path(self.plan["managed_watch"]["output"])
        self.out = self.output / "provision"

    def prepare(self):
        return s.prepare(self.plan_path, clock=self.clock)

    def live(self):
        self.prepare()
        self.manifest, _ = s.load_bundle(self.plan)
        self.unit = "otel-native-watch-native-test.service"
        fragment = Path(self.manifest["unit_path"])
        fragment.parent.mkdir(mode=0o700, parents=True)
        fragment.write_bytes((self.watch_root / self.unit).read_bytes())
        fragment.chmod(0o600)
        self.proc = FakeProc(self.root / "proc")
        self.user_cgroup = "/user.slice/user-" + str(os.getuid()) + ".slice/user@" + str(os.getuid()) + ".service"
        self.runtime_cgroup = self.user_cgroup + "/app.slice/bruh-web.service"
        self.watch_cgroup = self.user_cgroup + "/" + self.unit
        python = self.manifest["python"]
        systemd = str(Path("/usr/lib/systemd/systemd").resolve())
        self.proc.add(50, ppid=1, exe=systemd, argv=[systemd, "--user"],
                      cgroup=self.user_cgroup + "/init.scope")
        self.proc.add(100, ppid=50, exe="/usr/bin/runtime", argv=["/usr/bin/runtime", "serve"], cgroup=self.runtime_cgroup)
        self.watch_argv = [python, "-B", str(self.watch_root / "bundle/layers/otel/bench/ops/horizontal/supervisor.py"),
                           "watch", "--root", str(self.watch_root), "--seal", self.manifest["seal"]]
        self.proc.add(300, ppid=50, exe=python, argv=self.watch_argv, cgroup=self.watch_cgroup)
        self.controller_argv = [python, "-B", str(self.repo / "layers/otel/bench/ops/horizontal/scaling.py"),
                                "run", "--plan", str(self.plan_path), "--output", str(self.output), "--execute-approved"]
        self.proc.add(200, ppid=100, exe=python, argv=self.controller_argv, cgroup=self.runtime_cgroup)
        common = dict(Id=self.unit, LoadState="loaded", ActiveState="active", SubState="running",
            FragmentPath=str(fragment), DropInPaths="", MainPID="300", ControlGroup=self.watch_cgroup,
            InvocationID="a" * 32, PartOf="", BindsTo="", Requires="-.slice", Requisite="", Conflicts="",
            StopWhenUnneeded="no", KillMode="control-group", RuntimeMaxUSec="7095000000",
            ExecMainStartTimestampMonotonic="1000000", ActiveEnterTimestampMonotonic="1000000")
        self.manager = FakeManager({self.unit: common,
            "bruh-web.service": dict(common, Id="bruh-web.service", MainPID="100", ControlGroup=self.runtime_cgroup,
                                     FragmentPath=str(self.root / "runtime.service"), InvocationID="b" * 32)})
        write_json(self.watch_root / "bound.json", dict(plan_sha256=self.manifest["plan_sha256"], boot_id=BOOT_ID,
            invocation_id="a" * 32, seconds=7095, epoch=1100, deadline_epoch=8200, deadline_monotonic_us=7101000000))

    def verify(self):
        return s.verify_unit(self.plan, runner=self.manager, proc_root=self.proc.root, clock=self.clock)

    def registration(self):
        write_json(self.watch_root / "controller.json", dict(manifest_sha256=digest(self.watch_root / "manifest.json"),
            plan_sha256=self.manifest["plan_sha256"],
            output=str(self.output), first_epoch=1000, deadline_epoch=8200,
            controller=s.process_identity(200, self.proc.root), runtime=s.process_identity(100, self.proc.root),
            runtime_invocation="b" * 32))

    def ack(self):
        write_json(self.watch_root / "heartbeat.json", dict(schema=s.SCHEMA, healthy=True,
            proof=self.verify(), controller_sha256=digest(self.watch_root / "controller.json"), epoch=self.clock()))

    def admit(self, out=None):
        return s.admit(self.plan, self.out if out is None else out, runner=self.manager,
                       proc_root=self.proc.root, clock=self.clock)

    def set_command(self, pid, argv):
        (self.proc.root / str(pid) / "cmdline").write_bytes(b"\0".join(x.encode() for x in argv) + b"\0")

    def set_cgroup(self, pid, cgroup):
        (self.proc.root / str(pid) / "cgroup").write_text("0::" + cgroup + "\n")

    def set_start(self, pid, start):
        path = self.proc.root / str(pid) / "stat"
        head, _, tail = path.read_text().rpartition(") ")
        fields = tail.split()
        fields[19] = str(start)
        path.write_text(head + ") " + " ".join(fields) + "\n")


class ManagedWatchConfigTests(SupervisorFixture):
    def test_exact_absolute_configuration_is_pure_and_does_not_mutate_plan(self):
        before = copy.deepcopy(self.plan)
        config = s.validate_config(self.plan)
        self.assertEqual(config, self.plan["managed_watch"])
        self.assertEqual(self.plan, before)
        self.assertFalse(self.watch_root.exists())
        self.assertFalse(self.output.exists())

    def test_configuration_rejects_missing_extra_relative_and_wrong_types(self):
        good = self.plan["managed_watch"]
        bad_configs = (None, True, [], {}, {"root": good["root"]}, {"output": good["output"]},
                       dict(good, extra=True), dict(good, root="relative"), dict(good, output="relative"),
                       dict(good, root=True), dict(good, output=1))
        for config in bad_configs:
            with self.subTest(config=config), self.assertRaises(ValueError):
                s.validate_config(dict(self.plan, managed_watch=config))
        missing = dict(self.plan)
        del missing["managed_watch"]
        with self.assertRaises(ValueError):
            s.validate_config(missing)

    def test_configuration_rejects_overlap_repository_paths_and_unsafe_spelling(self):
        for root, output in ((str(self.watch_root), str(self.watch_root)),
                             (str(self.watch_root), str(self.watch_root / "run")),
                             (str(self.output / "watch"), str(self.output)),
                             (str(self.repo / "watch"), str(self.output)),
                             (str(self.watch_root), str(self.repo / "run")),
                             (str(self.watch_root) + "/../watch", str(self.output)),
                             (str(self.watch_root) + "/", str(self.output)),
                             (str(self.watch_root) + ";true", str(self.output))):
            with self.subTest(root=root, output=output), self.assertRaises(ValueError):
                s.validate_config(dict(self.plan, managed_watch=dict(root=root, output=output)))


class ManagedWatchBundleTests(SupervisorFixture):
    def test_readonly_library_import_from_shallow_remote_path_has_no_repository_depth_requirement(self):
        source = (self.repo / "layers/otel/bench/ops/horizontal/supervisor.py").read_text()
        namespace = {"__file__": "/supervisor.py", "__name__": "offline_shallow_supervisor"}
        exec(compile(source, "/supervisor.py", "exec"), namespace)
        self.assertEqual(namespace["HERE"], Path("/"))
        self.assertEqual(namespace["REPO"], Path("/"))
        self.assertEqual(namespace["SCHEMA"], s.SCHEMA)
        self.assertEqual(namespace["RUNTIME_UNIT"], "bruh-web.service")
        self.assertEqual(namespace["FILES"], s.FILES)
        self.assertFalse(self.watch_root.exists())
        self.assertFalse(self.output.exists())

    def test_prepare_creates_only_private_inert_bundle_and_user_template(self):
        before = self.plan_path.read_bytes()
        result = self.prepare()
        manifest, root = s.load_bundle(self.plan)
        self.assertIs(result["prepared"], True)
        self.assertIs(result["activated"], False)
        self.assertIs(result["operator_permission_required"], True)
        self.assertEqual(result["unit"], "otel-native-watch-native-test.service")
        self.assertEqual(root, self.watch_root)
        self.assertEqual(self.plan_path.read_bytes(), before)
        self.assertEqual(manifest["first_epoch"], 1000)
        self.assertEqual(manifest["deadline_epoch"], 8200)
        self.assertEqual(set(manifest["files"]), set(s.FILES))
        for path in (root, *root.rglob("*")):
            self.assertFalse(path.is_symlink(), path)
            self.assertEqual(path.stat().st_mode & 0o077, 0, path)
        text = (root / result["unit"]).read_text()
        self.assertIn("ExecStartPre=", text)
        self.assertIn(" bound --root " + str(root), text)
        self.assertIn(" watch --root " + str(root), text)
        self.assertIn("RuntimeMaxSec=1s\n", text)
        self.assertIn("RestartPreventExitStatus=77\n", text)
        self.assertIn("--seal " + manifest["seal"], text)
        self.assertIn("StopWhenUnneeded=no\n", text)
        self.assertIn("KillMode=control-group\n", text)
        self.assertIn("Slice=-.slice\n", text)
        for forbidden in ("PartOf=", "BindsTo=", "Requires=", "WantedBy=", "[Install]", "bruh-web.service"):
            self.assertNotIn(forbidden, text)
        self.assertFalse(Path(manifest["unit_path"]).exists())
        self.assertFalse(self.output.exists())
        self.assertFalse((root / "controller.json").exists())
        self.assertFalse((root / "heartbeat.json").exists())

    def test_prepare_cannot_replace_an_existing_bundle_or_renew_original_lease(self):
        self.prepare()
        original = (self.watch_root / "manifest.json").read_bytes()
        self.clock.epoch += 100
        with self.assertRaises((ValueError, FileExistsError)):
            self.prepare()
        self.assertEqual((self.watch_root / "manifest.json").read_bytes(), original)
        self.assertEqual(json.loads(original)["deadline_epoch"], 8200)

    def test_prepare_refuses_not_started_or_closed_admission_window_without_writes(self):
        for epoch in (999, 5000, 8200):
            self.clock.epoch = epoch
            with self.subTest(epoch=epoch), self.assertRaises(ValueError):
                self.prepare()
            self.assertFalse(self.watch_root.exists())

    def test_prepare_refuses_changed_sealed_source_before_bundle_creation(self):
        (Path(self.plan["source_root"]) / "sealed.py").write_text("SEALED = False\n")
        with self.assertRaisesRegex(ValueError, "source file"):
            self.prepare()
        self.assertFalse(self.watch_root.exists())

    def test_load_refuses_private_bundle_and_original_source_helper_drift(self):
        self.prepare()
        relative = "layers/otel/bench/ops/horizontal/provision.py"
        copied = self.watch_root / "bundle" / relative
        original = copied.read_bytes()
        copied.chmod(0o600)
        copied.write_bytes(original + b"\n# changed bundle\n")
        with self.assertRaisesRegex(ValueError, "helper bundle"):
            s.load_bundle(self.plan)
        copied.write_bytes(original)
        source = self.repo / relative
        source.write_bytes(original + b"\n# changed source\n")
        with self.assertRaisesRegex(ValueError, "source helper"):
            s.load_bundle(self.plan)
        manifest, root = s.load_bundle(self.plan, check_source=False)
        self.assertEqual(root, self.watch_root)
        self.assertEqual(manifest["deadline_epoch"], 8200)

    def test_load_refuses_approved_plan_and_absolute_lease_replacement(self):
        self.prepare()
        for key, bad in (("deadline_epoch", 8300), ("first_epoch", 1100),
                         ("source_revision", "c" * 40), ("approved", False)):
            with self.subTest(key=key), self.assertRaises(ValueError):
                s.load_bundle(dict(self.plan, **{key: bad}))
        original = self.plan_path.read_bytes()
        self.plan_path.write_bytes(original + b" ")
        with self.assertRaisesRegex(ValueError, "immutable"):
            s.load_bundle(self.plan)

    def test_load_refuses_template_permissions_and_symlink_replacement(self):
        self.prepare()
        manifest, _ = s.load_bundle(self.plan)
        template = self.watch_root / manifest["unit"]
        original = template.read_bytes()
        template.chmod(0o600)
        template.write_bytes(original + b"PartOf=bruh-web.service\n")
        with self.assertRaisesRegex(ValueError, "template changed"):
            s.load_bundle(self.plan)
        template.write_bytes(original)
        receipt = self.watch_root / "manifest.json"
        receipt.chmod(0o644)
        with self.assertRaisesRegex(ValueError, "private bounded"):
            s.load_bundle(self.plan)
        receipt.chmod(0o400)
        target = self.root / "manifest-copy.json"
        target.write_bytes(receipt.read_bytes())
        target.chmod(0o600)
        receipt.unlink()
        receipt.symlink_to(target)
        with self.assertRaisesRegex(ValueError, "symlink"):
            s.load_bundle(self.plan)


class ManagedWatchProcessTests(SupervisorFixture):
    def test_literal_proc_fixture_captures_exact_identity_with_parentheses_in_comm(self):
        proc = FakeProc(self.root / "proc")
        args = ["/usr/bin/python3", "-B", "/private/watch.py", "watch"]
        proc.add(432, start=987654, ppid=55, pgid=777, argv=args, cgroup="/user.slice/independent.service")
        actual = s.process_identity(432, proc.root)
        self.assertEqual(actual, dict(pid=432, starttime_ticks=987654, ppid=55, pgid=777, uid=os.getuid(),
            exe="/usr/bin/python3", argv=args, cgroup="/user.slice/independent.service", boot_id=BOOT_ID))

    def test_proc_rejects_nonpositive_boolean_and_unknown_pid(self):
        proc = FakeProc(self.root / "proc")
        for pid in (True, False, 0, -1, "432", None):
            with self.subTest(pid=pid), self.assertRaises(ValueError):
                s.process_identity(pid, proc.root)
        with self.assertRaises(FileNotFoundError):
            s.process_identity(432, proc.root)

    def test_proc_rejects_uid_cgroup_argv_starttime_and_boot_ambiguity(self):
        proc = FakeProc(self.root / "proc")
        path = proc.add(432)
        bad = (("status", b"Uid:\t0\t0\t0\t0\n" if os.getuid() != 0 else b"Uid:\t1\t1\t1\t1\n"),
               ("cgroup", b"0::/first\n0::/second\n"), ("cgroup", b"1:name=systemd:/legacy\n"),
               ("cgroup", b"0::/user.slice/../foreign\n"), ("cmdline", b""),
               ("stat", b"432 (bad) S 1 432\n"))
        for name, content in bad:
            original = (path / name).read_bytes()
            (path / name).write_bytes(content)
            with self.subTest(file=name, content=content), self.assertRaises(ValueError):
                s.process_identity(432, proc.root)
            (path / name).write_bytes(original)
        boot = proc.root / "sys/kernel/random/boot_id"
        boot.write_text("unknown\n")
        with self.assertRaises(ValueError):
            s.process_identity(432, proc.root)


class ManagedWatchManagerTests(SupervisorFixture):
    def test_manager_probe_is_exact_bounded_readonly_user_show(self):
        self.live()
        props = s.manager_unit(self.unit, runner=self.manager)
        self.assertEqual(props["MainPID"], "300")
        self.assertEqual(len(self.manager.calls), 1)
        argv, timeout = self.manager.calls[0]
        self.assertEqual(argv[:4], ["/usr/bin/systemctl", "--user", "show", self.unit])
        self.assertEqual(set(argv[4:]), {"--property=" + key for key in s.PROPERTIES})
        self.assertEqual(timeout, 2)
        self.assertEqual(self.manager.mutations, [])

    def test_unknown_incomplete_duplicate_and_unbounded_manager_output_fail_closed(self):
        unit = "otel-native-watch-native-test.service"
        for result in ((1, "", "manager unavailable"), (0, "", ""),
                       (0, "LoadState=not-found\n", ""),
                       (0, "Id=" + unit + "\nId=" + unit + "\n", ""),
                       (0, "UnknownProperty=value\n", ""), (0, "x" * 65537, "")):
            manager = FakeManager()
            manager.failure = result
            with self.subTest(result=(result[0], len(result[1]))), self.assertRaises(ValueError):
                s.manager_unit(unit, runner=manager)
        manager = FakeManager()
        for unit in ("bruh-web", "../bad.service", "bruh-web.service;true", True):
            with self.subTest(unit=unit), self.assertRaises(ValueError):
                s.manager_unit(unit, runner=manager)
        self.assertEqual(manager.calls, [])


class ManagedWatchIdentityTests(SupervisorFixture):
    def test_independent_user_unit_has_positive_exact_pid_manager_runtime_and_lease_proof(self):
        self.live()
        proof = self.verify()
        self.manager.units[self.unit]["Requires"] = ""
        self.assertEqual(self.verify(), proof)
        self.manager.units[self.unit]["Requires"] = "-.slice"
        self.assertEqual(proof["watch"]["pid"], 300)
        self.assertEqual(proof["watch"]["argv"], self.watch_argv)
        self.assertEqual(proof["watch"]["cgroup"], self.watch_cgroup)
        self.assertEqual(proof["manager"]["pid"], 50)
        self.assertEqual(proof["runtime"]["pid"], 100)
        self.assertEqual(proof["runtime"]["cgroup"], self.runtime_cgroup)
        self.assertEqual(proof["unit_invocation"], "a" * 32)
        self.assertEqual(proof["runtime_invocation"], "b" * 32)
        self.assertEqual(proof["bound_sha256"], digest(self.watch_root / "bound.json"))
        self.assertEqual(proof["plan_sha256"], digest(self.plan_path))
        self.assertEqual(Path(proof["watch"]["cgroup"]).parent, Path(proof["manager"]["cgroup"]).parent)
        self.assertEqual(self.manager.mutations, [])
        ancestor_slice_cgroup = self.user_cgroup + "/app.slice/" + self.unit
        self.set_cgroup(300, ancestor_slice_cgroup)
        self.manager.units[self.unit]["ControlGroup"] = ancestor_slice_cgroup
        with self.assertRaisesRegex(ValueError, "directly in the USER manager root slice"):
            self.verify()

    def test_separate_pgids_with_shared_runtime_cgroup_are_not_independent(self):
        self.live()
        self.assertNotEqual(s.process_identity(300, self.proc.root)["pgid"],
                            s.process_identity(200, self.proc.root)["pgid"])
        self.set_cgroup(300, self.runtime_cgroup)
        self.manager.units[self.unit]["ControlGroup"] = self.runtime_cgroup
        with self.assertRaises(ValueError):
            self.verify()
        self.assertEqual(self.manager.mutations, [])
        self.assertFalse((self.watch_root / "controller.json").exists())
        self.assertFalse(self.out.exists())

    def test_unit_dependencies_shared_kill_group_or_unknown_manager_are_refused(self):
        self.live()
        props = self.manager.units[self.unit]
        for key, bad in (("PartOf", "bruh-web.service"), ("BindsTo", "bruh-web.service"),
                         ("Requires", "bruh-web.service"), ("Requisite", "bruh-web.service"),
                         ("Conflicts", "bruh-web.service"), ("StopWhenUnneeded", "yes"),
                         ("KillMode", "process"), ("ActiveState", "inactive"),
                         ("SubState", "dead"), ("InvocationID", "unknown"), ("MainPID", "999"),
                         ("DropInPaths", str(self.root / "foreign.conf"))):
            original = props[key]
            props[key] = bad
            with self.subTest(key=key), self.assertRaises((OSError, ValueError)):
                self.verify()
            props[key] = original
        self.manager.failure = (1, "", "user manager unknown")
        with self.assertRaises(ValueError):
            self.verify()

    def test_literal_fragment_with_runtime_dependency_is_refused_before_manager_proof(self):
        self.live()
        fragment = Path(self.manifest["unit_path"])
        fragment.write_text(fragment.read_text() + "\n[Unit]\nPartOf=bruh-web.service\n")
        with self.assertRaisesRegex(ValueError, "fragment or shared kill dependencies"):
            self.verify()
        self.assertEqual(len(self.manager.calls), 1)
        self.assertEqual(self.manager.mutations, [])

    def test_live_argv_executable_and_manager_parent_identity_are_required(self):
        self.live()
        for args in ([self.manifest["python"], "unrelated.py"], self.watch_argv + ["extra"],
                     [*self.watch_argv[:-1], str(self.root / "other-watch")]):
            self.set_command(300, args)
            with self.subTest(argv=args), self.assertRaises(ValueError):
                self.verify()
        self.set_command(300, self.watch_argv)
        exe = self.proc.root / "300/exe"
        exe.unlink()
        exe.symlink_to("/usr/bin/foreign-python")
        with self.assertRaises(ValueError):
            self.verify()
        exe.unlink()
        exe.symlink_to(self.manifest["python"])
        self.set_command(50, ["/usr/lib/systemd/systemd", "--system"])
        with self.assertRaisesRegex(ValueError, "USER-manager"):
            self.verify()

    def test_runtime_pid_cgroup_invocation_and_shared_pgid_must_be_known(self):
        self.live()
        props = self.manager.units["bruh-web.service"]
        for key, bad in (("MainPID", "999"), ("ControlGroup", "/foreign/bruh-web.service"),
                         ("InvocationID", ""), ("ActiveState", "unknown"), ("KillMode", "process")):
            original = props[key]
            props[key] = bad
            with self.subTest(key=key), self.assertRaises((OSError, ValueError)):
                self.verify()
            props[key] = original
        stat = self.proc.root / "300/stat"
        original = stat.read_text()
        head, _, tail = original.rpartition(") ")
        fields = tail.split()
        fields[2] = "100"
        stat.write_text(head + ") " + " ".join(fields))
        with self.assertRaises(ValueError):
            self.verify()

    def test_bound_receipt_boot_invocation_deadline_and_runtime_max_cannot_be_reset(self):
        self.live()
        path = self.watch_root / "bound.json"
        receipt = json.loads(path.read_text())
        for key, bad in (("plan_sha256", "f" * 64), ("boot_id", "99999999-2222-3333-4444-555555555555"),
                         ("invocation_id", "f" * 32), ("deadline_epoch", 8300),
                         ("seconds", 1), ("seconds", True), ("deadline_monotonic_us", 1000001)):
            write_json(path, dict(receipt, **{key: bad}))
            with self.subTest(key=key, bad=bad), self.assertRaises(ValueError):
                self.verify()
        write_json(path, receipt)
        props = self.manager.units[self.unit]
        for key, bad in (("RuntimeMaxUSec", "infinity"), ("RuntimeMaxUSec", "7200000000"),
                         ("RuntimeMaxUSec", "0"), ("ExecMainStartTimestampMonotonic", "7100000000"),
                         ("ActiveEnterTimestampMonotonic", "7100000000")):
            original = props[key]
            props[key] = bad
            with self.subTest(key=key, bad=bad), self.assertRaises(ValueError):
                self.verify()
            props[key] = original
        self.clock.epoch = 8200
        with self.assertRaisesRegex(ValueError, "lease"):
            self.verify()


class ManagedWatchAdmissionTests(SupervisorFixture):
    def test_register_captures_exact_controller_then_waits_for_fresh_watch_ack(self):
        self.live()
        self.clock.on_sleep = self.ack
        with mock.patch.object(os, "getpid", return_value=200):
            result = s.register(self.plan, self.output, runner=self.manager, proc_root=self.proc.root,
                                clock=self.clock, sleep=self.clock.sleep)
        self.assertIs(result["admitted"], True)
        self.assertEqual(self.clock.sleeps, [0.1])
        registration = json.loads((self.watch_root / "controller.json").read_text())
        self.assertEqual(registration["controller"], s.process_identity(200, self.proc.root))
        self.assertEqual(registration["runtime"], s.process_identity(100, self.proc.root))
        self.assertEqual(registration["deadline_epoch"], 8200)
        self.assertEqual(registration["first_epoch"], 1000)
        self.assertEqual(registration["output"], str(self.output))
        self.assertIs(self.admit()["admitted"], True)
        with mock.patch.object(os, "getpid", return_value=200), self.assertRaisesRegex(ValueError, "already exists"):
            s.register(self.plan, self.output, runner=self.manager, proc_root=self.proc.root,
                       clock=self.clock, sleep=self.clock.sleep)

    def test_register_without_fresh_ack_waits_at_most_five_seconds(self):
        self.live()
        start = self.clock()
        with mock.patch.object(os, "getpid", return_value=200), self.assertRaisesRegex(ValueError, "acknowledge"):
            s.register(self.plan, self.output, runner=self.manager, proc_root=self.proc.root,
                       clock=self.clock, sleep=self.clock.sleep)
        self.assertGreaterEqual(self.clock(), start + 5)
        self.assertLess(self.clock(), start + 5.11)
        self.assertLessEqual(len(self.clock.sleeps), 51)
        self.assertEqual(json.loads((self.watch_root / "controller.json").read_text())["deadline_epoch"], 8200)

    def test_register_refuses_unexpected_argv_and_foreign_controller_before_receipt(self):
        self.live()
        for args in (self.controller_argv + ["extra"], self.controller_argv[:-1],
                     [*self.controller_argv[:1], "-c", *self.controller_argv[2:]],
                     [*self.controller_argv[:-2], str(self.root / "other-output"), "--execute-approved"]):
            self.set_command(200, args)
            with self.subTest(argv=args), mock.patch.object(os, "getpid", return_value=200), self.assertRaises(ValueError):
                s.register(self.plan, self.output, runner=self.manager, proc_root=self.proc.root,
                           clock=self.clock, sleep=self.clock.sleep)
            self.assertFalse((self.watch_root / "controller.json").exists())
        self.set_command(200, self.controller_argv)
        self.set_cgroup(200, self.watch_cgroup)
        with mock.patch.object(os, "getpid", return_value=200), self.assertRaises(ValueError):
            s.register(self.plan, self.output, runner=self.manager, proc_root=self.proc.root,
                       clock=self.clock, sleep=self.clock.sleep)
        self.assertFalse((self.watch_root / "controller.json").exists())
        self.assertEqual(self.clock.sleeps, [])

    def test_admission_requires_exact_provision_output_and_all_fresh_proof_fields(self):
        self.live()
        self.registration()
        self.ack()
        self.assertIs(self.admit()["admitted"], True)
        for out in (self.output, self.root / "other", self.out / "provision"):
            with self.subTest(out=out), self.assertRaises(ValueError):
                self.admit(out)
        path = self.watch_root / "heartbeat.json"
        ack = json.loads(path.read_text())
        for key, bad in (("healthy", False), ("schema", "unknown"), ("controller_sha256", "f" * 64),
                         ("proof", {}), ("epoch", self.clock() - 3.01), ("epoch", self.clock() + 0.01)):
            write_json(path, dict(ack, **{key: bad}))
            with self.subTest(key=key, bad=bad), self.assertRaises(ValueError):
                self.admit()
        write_json(path, dict(ack, epoch=self.clock() - 3))
        self.assertIs(self.admit()["admitted"], True)

    def test_controller_registration_cannot_replace_manifest_plan_output_or_lease(self):
        self.live()
        self.registration()
        self.ack()
        path = self.watch_root / "controller.json"
        registration = json.loads(path.read_text())
        for key, bad in (("manifest_sha256", "f" * 64), ("plan_sha256", "f" * 64),
                         ("output", str(self.root / "other-output")), ("first_epoch", 1100),
                         ("deadline_epoch", 8300)):
            write_json(path, dict(registration, **{key: bad}))
            with self.subTest(key=key), self.assertRaisesRegex(ValueError, "immutable lease"):
                self.admit()
        del registration["manifest_sha256"]
        write_json(path, registration)
        with self.assertRaisesRegex(ValueError, "immutable lease"):
            self.admit()

    def test_admission_detects_controller_pid_reuse_argv_cgroup_and_boot_changes(self):
        self.live()
        self.registration()
        self.ack()
        self.set_start(200, 5001)
        with self.assertRaisesRegex(ValueError, "reused/changed"):
            self.admit()
        self.set_start(200, 5000)
        self.set_command(200, self.controller_argv + ["extra"])
        with self.assertRaisesRegex(ValueError, "reused/changed"):
            self.admit()
        self.set_command(200, self.controller_argv)
        self.set_cgroup(200, self.watch_cgroup)
        with self.assertRaises(ValueError):
            self.admit()
        self.set_cgroup(200, self.runtime_cgroup)
        (self.proc.root / "sys/kernel/random/boot_id").write_text("99999999-2222-3333-4444-555555555555\n")
        with self.assertRaises(ValueError):
            self.admit()

    def test_admission_detects_watch_pid_reuse_runtime_restart_and_source_helper_drift(self):
        self.live()
        self.registration()
        self.ack()
        self.set_start(300, 5001)
        with self.assertRaisesRegex(ValueError, "heartbeat"):
            self.admit()
        self.set_start(300, 5000)
        props = self.manager.units["bruh-web.service"]
        props["InvocationID"] = "c" * 32
        with self.assertRaisesRegex(ValueError, "runtime"):
            self.admit()
        props["InvocationID"] = "b" * 32
        helper = self.repo / "layers/otel/bench/ops/horizontal/provision.py"
        helper.write_bytes(helper.read_bytes() + b"\n# source changed\n")
        with self.assertRaisesRegex(ValueError, "source helper"):
            self.admit()


class ManagedWatchSealedSourceTests(SupervisorFixture):
    def test_admission_rechecks_sealed_application_source_not_only_manifest_text(self):
        self.live()
        self.registration()
        self.ack()
        path = Path(self.plan["source_root"]) / "sealed.py"
        path.write_text("SEALED = False\n")
        with self.assertRaisesRegex(ValueError, "source file"):
            self.admit()

    def test_admission_rechecks_asset_bytes_after_watch_ack(self):
        self.live()
        self.registration()
        self.ack()
        binary = Path(self.plan["binary"])
        binary.write_text("changed application binary\n")
        with self.assertRaisesRegex(ValueError, "asset"):
            self.admit()


class ManagedWatchSealTests(SupervisorFixture):
    def test_manifest_seal_changes_are_rejected_before_new_identity_can_be_adopted(self):
        self.live()
        path = self.watch_root / "manifest.json"
        changed = dict(self.manifest, path="/unapproved/bin")
        path.chmod(0o600)
        write_json(path, changed)
        with self.assertRaisesRegex(ValueError, "seal"):
            s.load_bundle(self.plan)

    def test_resealed_helper_and_forged_ack_cannot_replace_old_live_kernel_argv(self):
        self.live()
        self.registration()
        self.ack()
        old_ack = json.loads((self.watch_root / "heartbeat.json").read_text())
        relative = "layers/otel/bench/ops/horizontal/provision.py"
        copied = self.watch_root / "bundle" / relative
        source = self.repo / relative
        copied.chmod(0o600)
        changed = copied.read_bytes() + b"\n# forged resealed helper\n"
        copied.write_bytes(changed)
        source.write_bytes(changed)
        manifest = copy.deepcopy(self.manifest)
        manifest["files"][relative] = digest(copied)
        manifest["seal"] = s._seal(manifest)
        self.assertNotEqual(manifest["seal"], self.manifest["seal"])
        template = self.watch_root / self.unit
        template.chmod(0o600)
        template.write_text(s._template(self.plan, manifest, self.watch_root))
        manifest["template_sha256"] = digest(template)
        Path(manifest["unit_path"]).write_bytes(template.read_bytes())
        (self.watch_root / "manifest.json").chmod(0o600)
        write_json(self.watch_root / "manifest.json", manifest)
        registration_path = self.watch_root / "controller.json"
        registration = json.loads(registration_path.read_text())
        registration["manifest_sha256"] = digest(self.watch_root / "manifest.json")
        write_json(registration_path, registration)
        old_ack["proof"]["manifest_sha256"] = digest(self.watch_root / "manifest.json")
        old_ack["controller_sha256"] = digest(registration_path)
        write_json(self.watch_root / "heartbeat.json", old_ack)
        loaded, _ = s.load_bundle(self.plan)
        self.assertEqual(loaded["seal"], manifest["seal"])
        self.assertEqual(s.process_identity(300, self.proc.root)["argv"], self.watch_argv)
        with self.assertRaisesRegex(ValueError, "PID/argv/cgroup"):
            self.admit()

    def test_watch_refuses_controller_or_detached_process_before_any_cleanup(self):
        self.live()
        cleanup = mock.Mock(side_effect=AssertionError("unauthorized watch cannot invoke teardown"))
        for pid in (200, 100):
            with self.subTest(pid=pid), mock.patch.object(os, "getpid", return_value=pid), self.assertRaisesRegex(ValueError, "OS-managed unit"):
                s.watch(self.watch_root, runner=self.manager, proc_root=self.proc.root,
                        clock=self.clock, sleep=self.clock.sleep, teardown=cleanup)
        self.set_command(300, self.watch_argv[:-1] + ["f" * 64])
        with mock.patch.object(os, "getpid", return_value=300), self.assertRaisesRegex(ValueError, "OS-managed unit"):
            s.watch(self.watch_root, runner=self.manager, proc_root=self.proc.root,
                    clock=self.clock, sleep=self.clock.sleep, teardown=cleanup)
        cleanup.assert_not_called()
        self.assertEqual(self.manager.calls, [])
        self.assertEqual(self.clock.sleeps, [])
        self.assertFalse((self.watch_root / "heartbeat.json").exists())

    def test_main_bound_and_watch_require_seal_and_expiry_permanently_prevents_restart(self):
        self.live()
        with mock.patch.object(s, "bound", side_effect=AssertionError("expired bound cannot execute")), \
             mock.patch.object(s, "watch", side_effect=AssertionError("expired watch cannot execute")), \
             mock.patch.object(s.time, "time", return_value=8200):
            for mode in ("bound", "watch"):
                for seal in (None, "f" * 64):
                    args = [mode, "--root", str(self.watch_root)]
                    if seal is not None:
                        args += ["--seal", seal]
                    with self.subTest(mode=mode, seal=seal), self.assertRaisesRegex(ValueError, "bootstrap seal"):
                        s.main(args)
                with self.subTest(mode=mode), self.assertRaises(SystemExit) as stopped:
                    s.main([mode, "--root", str(self.watch_root), "--seal", self.manifest["seal"]])
                self.assertEqual(stopped.exception.code, 77)
        self.assertFalse((self.watch_root / "heartbeat.json").exists())


class ManagedWatchBoundTests(SupervisorFixture):
    def test_execstartpre_bounds_restarts_to_remaining_original_deadline_not_fresh_lifetime(self):
        self.live()
        bound_argv = [*self.watch_argv[:3], "bound", *self.watch_argv[4:]]
        self.set_command(300, bound_argv)
        with mock.patch.object(os, "getpid", return_value=300), mock.patch.object(s.time, "monotonic_ns", return_value=1000000000):
            first = s.bound(self.watch_root, runner=self.manager, proc_root=self.proc.root, clock=self.clock)
        first_receipt = json.loads((self.watch_root / "bound.json").read_text())
        self.assertEqual(first["seconds"], 7095)
        self.assertEqual(first["deadline_epoch"], 8200)
        self.assertEqual(first_receipt["deadline_monotonic_us"], 7101000000)
        self.clock.epoch = 1200
        self.manager.units[self.unit]["InvocationID"] = "c" * 32
        with mock.patch.object(os, "getpid", return_value=300), mock.patch.object(s.time, "monotonic_ns", return_value=101000000000):
            second = s.bound(self.watch_root, runner=self.manager, proc_root=self.proc.root, clock=self.clock)
        second_receipt = json.loads((self.watch_root / "bound.json").read_text())
        self.assertEqual(second["seconds"], 6995)
        self.assertLess(second["seconds"], first["seconds"])
        self.assertEqual(second_receipt["invocation_id"], "c" * 32)
        self.assertEqual(second_receipt["deadline_epoch"], first_receipt["deadline_epoch"])
        self.assertEqual(second_receipt["deadline_monotonic_us"], first_receipt["deadline_monotonic_us"])
        self.assertEqual(self.manager.mutations, [
            ["/usr/bin/systemctl", "--user", "set-property", "--runtime", self.unit, "RuntimeMaxSec=7095s"],
            ["/usr/bin/systemctl", "--user", "set-property", "--runtime", self.unit, "RuntimeMaxSec=6995s"]])
        self.assertEqual(json.loads((self.watch_root / "plan.json").read_text())["deadline_epoch"], 8200)

    def test_execstartpre_refuses_other_process_foreign_cgroup_and_expired_lease_before_effect(self):
        self.live()
        with mock.patch.object(os, "getpid", return_value=200), self.assertRaises(ValueError):
            s.bound(self.watch_root, runner=self.manager, proc_root=self.proc.root, clock=self.clock)
        self.set_command(300, [*self.watch_argv[:3], "bound", *self.watch_argv[4:]])
        self.set_cgroup(300, self.runtime_cgroup)
        with mock.patch.object(os, "getpid", return_value=300), self.assertRaises(ValueError):
            s.bound(self.watch_root, runner=self.manager, proc_root=self.proc.root, clock=self.clock)
        self.set_cgroup(300, self.watch_cgroup)
        for epoch in (999, 8195, 8200, 9000):
            self.clock.epoch = epoch
            with self.subTest(epoch=epoch), mock.patch.object(os, "getpid", return_value=300), self.assertRaises(ValueError):
                s.bound(self.watch_root, runner=self.manager, proc_root=self.proc.root, clock=self.clock)
        self.assertEqual(self.manager.mutations, [])

    def test_execstartpre_can_bound_own_cleanup_unit_when_runtime_is_inactive(self):
        self.live()
        self.set_command(300, [*self.watch_argv[:3], "bound", *self.watch_argv[4:]])
        self.manager.units["bruh-web.service"].update(ActiveState="inactive", SubState="dead", MainPID="0")
        (Path(self.plan["source_root"]) / "sealed.py").write_text("SEALED = False\n")
        self.clock.epoch = 1200
        with mock.patch.object(os, "getpid", return_value=300), mock.patch.object(s.time, "monotonic_ns", return_value=101000000000):
            result = s.bound(self.watch_root, runner=self.manager, proc_root=self.proc.root, clock=self.clock)
        self.assertIs(result["bounded"], True)
        self.assertEqual(result["seconds"], 6995)
        self.assertEqual(result["deadline_epoch"], 8200)
        self.assertEqual(self.manager.mutations, [
            ["/usr/bin/systemctl", "--user", "set-property", "--runtime", self.unit, "RuntimeMaxSec=6995s"]])
        self.assertEqual(len(self.manager.calls), 2)
        self.assertEqual(self.manager.calls[0][0][:4], ["/usr/bin/systemctl", "--user", "show", self.unit])
        self.assertTrue(all("bruh-web.service" not in argv for argv, _ in self.manager.calls))
        receipt = json.loads((self.watch_root / "bound.json").read_text())
        self.assertEqual(receipt["deadline_epoch"], 8200)
        self.assertEqual(receipt["deadline_monotonic_us"], 7101000000)

    def test_execstartpre_manager_failure_cannot_write_a_successful_new_bound(self):
        self.live()
        original = (self.watch_root / "bound.json").read_bytes()
        self.set_command(300, [*self.watch_argv[:3], "bound", *self.watch_argv[4:]])
        original_call = self.manager.__call__

        def fail_mutation(argv, *, timeout):
            if "set-property" in argv:
                return 1, "", "fake manager refused RuntimeMax"
            return original_call(argv, timeout=timeout)

        with mock.patch.object(os, "getpid", return_value=300), self.assertRaises(ValueError):
            s.bound(self.watch_root, runner=fail_mutation, proc_root=self.proc.root, clock=self.clock)
        self.assertEqual((self.watch_root / "bound.json").read_bytes(), original)


class ManagedWatchLoopTests(SupervisorFixture):
    def ready(self):
        self.live()
        self.registration()
        (self.out / "provision/baseline").mkdir(mode=0o700, parents=True)
        self.cleanup_calls = []

    def cleanup(self, plan, out, *, clock, sleep):
        self.cleanup_calls.append((copy.deepcopy(plan), Path(out), clock()))
        return {"passed": True, "within_deadline": True}

    def watch(self, teardown=None):
        with mock.patch.object(os, "getpid", return_value=300):
            return s.watch(self.watch_root, runner=self.manager, proc_root=self.proc.root, clock=self.clock,
                           sleep=self.clock.sleep, teardown=self.cleanup if teardown is None else teardown)

    def assert_cleaned_original_lease(self, status):
        self.assertEqual(status, 0)
        self.assertEqual(len(self.cleanup_calls), 1)
        plan, out, epoch = self.cleanup_calls[0]
        self.assertEqual(plan, self.plan)
        self.assertEqual(plan["first_epoch"], 1000)
        self.assertEqual(plan["deadline_epoch"], 8200)
        self.assertEqual(out, self.output / "provision")
        self.assertLess(epoch, 8200)
        heartbeat = json.loads((self.watch_root / "heartbeat.json").read_text())
        self.assertIs(heartbeat["healthy"], False)
        receipt = json.loads((self.watch_root / "exit.json").read_text())
        self.assertIs(receipt["passed"], True)
        self.assertEqual(receipt["deadline_epoch"], 8200)
        self.assertEqual(self.manager.mutations, [])

    def test_controller_disappearance_after_healthy_ack_triggers_cleanup_without_new_deadline(self):
        self.ready()
        healthy = []

        def disappear():
            healthy.append(json.loads((self.watch_root / "heartbeat.json").read_text()))
            (self.proc.root / "200/stat").unlink()

        self.clock.on_sleep = disappear
        self.assert_cleaned_original_lease(self.watch())
        self.assertEqual(len(healthy), 1)
        self.assertIs(healthy[0]["healthy"], True)
        self.assertEqual(healthy[0]["proof"]["watch"]["pid"], 300)
        self.assertEqual(healthy[0]["controller_sha256"], digest(self.watch_root / "controller.json"))
        self.assertEqual(self.clock.sleeps, [1])

    def test_controller_pid_reuse_triggers_cleanup_with_original_lease(self):
        self.ready()
        self.clock.on_sleep = lambda: self.set_start(200, 5001)
        self.assert_cleaned_original_lease(self.watch())
        self.assertIn("reused/changed", json.loads((self.watch_root / "exit.json").read_text())["reason"])

    def test_runtime_invocation_change_triggers_cleanup_with_original_lease(self):
        self.ready()
        self.clock.on_sleep = lambda: self.manager.units["bruh-web.service"].update(InvocationID="c" * 32)
        self.assert_cleaned_original_lease(self.watch())
        self.assertIn("runtime invocation", json.loads((self.watch_root / "exit.json").read_text())["reason"])

    def test_unknown_manager_triggers_cleanup_with_original_lease(self):
        self.ready()

        def unknown():
            self.manager.failure = (1, "", "fake USER manager unavailable")

        self.clock.on_sleep = unknown
        self.assert_cleaned_original_lease(self.watch())
        self.assertIn("unknown", json.loads((self.watch_root / "exit.json").read_text())["reason"])

    def test_source_helper_change_after_ack_triggers_cleanup_with_sealed_plan(self):
        self.ready()
        path = self.repo / "layers/otel/bench/ops/horizontal/provision.py"
        self.clock.on_sleep = lambda: path.write_bytes(path.read_bytes() + b"\n# changed after ACK\n")
        self.assert_cleaned_original_lease(self.watch())
        self.assertIn("source helper", json.loads((self.watch_root / "exit.json").read_text())["reason"])

    def test_startup_source_failure_or_inactive_runtime_still_cleans_original_lease(self):
        self.ready()
        source = Path(self.plan["source_root"]) / "sealed.py"
        original_source = source.read_bytes()
        runtime = self.manager.units["bruh-web.service"]
        original_runtime = copy.deepcopy(runtime)
        for fault in ("application-source", "inactive-runtime"):
            with self.subTest(fault=fault):
                self.cleanup_calls.clear()
                if fault == "application-source":
                    source.write_text("SEALED = False\n")
                else:
                    runtime.update(ActiveState="inactive", SubState="dead", MainPID="0")
                self.assert_cleaned_original_lease(self.watch())
                self.assertEqual(self.clock.sleeps, [])
                self.assertEqual(self.cleanup_calls[0][2], 1100)
                reason = json.loads((self.watch_root / "exit.json").read_text())["reason"]
                self.assertIn("source file" if fault == "application-source" else "runtime USER unit", reason)
                source.write_bytes(original_source)
                runtime.clear()
                runtime.update(original_runtime)

    def test_original_cleanup_epoch_fires_without_waiting_for_controller(self):
        self.ready()
        self.clock.epoch = 7600
        self.assert_cleaned_original_lease(self.watch())
        self.assertEqual(self.clock.sleeps, [])
        self.assertEqual(self.cleanup_calls[0][2], 7600)
        self.assertEqual(json.loads((self.watch_root / "exit.json").read_text())["reason"], "original absolute cleanup time")

    def test_cleanup_retries_stop_at_original_hard_deadline_and_do_not_restart_lifetime(self):
        self.ready()
        self.clock.epoch = 8198

        def incomplete(plan, out, *, clock, sleep):
            self.cleanup_calls.append((copy.deepcopy(plan), Path(out), clock()))
            return {"passed": False, "within_deadline": True}

        self.assertEqual(self.watch(teardown=incomplete), 77)
        self.assertEqual([item[2] for item in self.cleanup_calls], [8198, 8199])
        self.assertEqual([item[0]["deadline_epoch"] for item in self.cleanup_calls], [8200, 8200])
        self.assertEqual(self.clock.sleeps, [1, 1])
        self.assertEqual(self.clock(), 8200)
        receipt = json.loads((self.watch_root / "exit.json").read_text())
        self.assertIs(receipt["passed"], False)
        self.assertEqual(receipt["deadline_epoch"], 8200)
        self.assertEqual(self.manager.mutations, [])

    def test_expired_watch_cannot_run_cleanup_or_write_a_fresh_healthy_ack(self):
        self.ready()
        self.clock.epoch = 8200
        self.assertEqual(self.watch(), 77)
        self.assertEqual(self.cleanup_calls, [])
        self.assertEqual(self.clock.sleeps, [])
        self.assertFalse((self.watch_root / "heartbeat.json").exists())
        receipt = json.loads((self.watch_root / "exit.json").read_text())
        self.assertIs(receipt["passed"], False)
        self.assertEqual(receipt["deadline_epoch"], 8200)


if __name__ == "__main__":
    unittest.main()
