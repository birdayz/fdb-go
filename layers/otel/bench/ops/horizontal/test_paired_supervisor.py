#!/usr/bin/env python3
"""Fake procfs/USER-manager paired watch checks, not actual OS-kill survival evidence."""
import copy
import json
import os
from pathlib import Path
import unittest
from unittest import mock

import paired
import provision
import supervisor as s
from test_supervisor import BOOT_ID, SupervisorFixture, digest, write_json


class PairedSupervisorFixture(SupervisorFixture):
    profile = "pilot"
    lease_seconds = 3600
    active_seconds = 2700
    cleanup_seconds = 3000

    def setUp(self):
        super().setUp()
        old = self.plan
        for name in s.PAIRED_FILES:
            (self.repo / name).write_bytes((Path(s.__file__).parent / Path(name).name).read_bytes())
        with mock.patch.object(paired.time, "time", return_value=1000):
            self.plan = paired.fake_plan(self.root / "paired-fixtures", profile=self.profile)
        self.plan.update(campaign="native-test", prefix="native-test", managed_watch=old["managed_watch"],
                         hcloud=old["hcloud"], controller_private_key=old["controller_private_key"],
                         controller_public_key=old["controller_public_key"], controller_cidr=old["controller_cidr"],
                         inventory_dir=str(self.root / "approved-inventory"))
        write_json(self.plan_path, self.plan)

    def live(self):
        super().live()
        self.controller_argv = ["python3", "-B",
            str(self.repo / "layers/otel/bench/ops/horizontal/paired.py"),
            "--plan", str(self.plan_path), "--output", str(self.output), "--execute-approved"]
        self.set_command(200, self.controller_argv)
        remaining = self.plan["deadline_epoch"] - self.clock()
        seconds = int(remaining) - 5
        self.manager.units[self.unit]["RuntimeMaxUSec"] = str(seconds * 1000000)
        write_json(self.watch_root / "bound.json", dict(plan_sha256=self.manifest["plan_sha256"], boot_id=BOOT_ID,
            invocation_id="a" * 32, seconds=seconds, epoch=self.clock(), deadline_epoch=self.plan["deadline_epoch"],
            deadline_monotonic_us=1000000 + int(remaining * 1000000)))

    def registration(self):
        write_json(self.watch_root / "controller.json", dict(manifest_sha256=digest(self.watch_root / "manifest.json"),
            plan_sha256=self.manifest["plan_sha256"], output=str(self.output), first_epoch=self.plan["first_epoch"],
            deadline_epoch=self.plan["deadline_epoch"], controller=s.process_identity(200, self.proc.root),
            runtime=s.process_identity(100, self.proc.root), runtime_invocation="b" * 32))

    def register(self):
        with mock.patch.object(os, "getpid", return_value=200):
            return s.register(self.plan, self.output, runner=self.manager, proc_root=self.proc.root,
                              clock=self.clock, sleep=self.clock.sleep)

    def watch(self, teardown=None):
        with mock.patch.object(os, "getpid", return_value=300):
            return s.watch(self.watch_root, runner=self.manager, proc_root=self.proc.root,
                           clock=self.clock, sleep=self.clock.sleep, teardown=teardown)


class PairedPilotSupervisorTests(PairedSupervisorFixture):
    def test_paired_config_is_pure_and_requires_exact_private_absolute_paths(self):
        before = copy.deepcopy(self.plan)
        self.assertEqual(s.validate_config(self.plan), self.plan["managed_watch"])
        self.assertEqual(self.plan, before)
        good = self.plan["managed_watch"]
        for value in (None, {}, dict(good, extra=True), dict(good, root=True), dict(good, output="relative"),
                      dict(good, root=good["output"]), dict(good, root=str(self.output / "watch")),
                      dict(good, output=str(self.watch_root / "run")), dict(good, root=str(self.repo / "watch")),
                      dict(good, root=good["root"] + "/../watch")):
            with self.subTest(value=value), self.assertRaises(ValueError):
                s.validate_config(dict(self.plan, managed_watch=value))
        with self.assertRaises(ValueError):
            s.validate_config(dict(self.plan, schema="otel_native_session_v1"))
        self.assertFalse(self.watch_root.exists())
        self.assertFalse(self.output.exists())

    def test_prepare_dispatches_paired_validation_before_creating_any_bundle(self):
        original = copy.deepcopy(self.plan)
        for changes in ({"deadline_epoch": 1000 + self.lease_seconds + 1},
                        {"deadline_epoch": 1000 + (7200 if self.profile == "pilot" else 3600)},
                        {"first_epoch": True}, {"deadline_epoch": True}, {"stage_profile": "unknown"},
                        {"stages": [True]}, {"cleanup_mode": "foreground_only"}, {"approved": False},
                        {"managed_watch": None}, {"workload": {"kind": "unknown"}}):
            write_json(self.plan_path, dict(original, **changes))
            with self.subTest(changes=changes), self.assertRaises(ValueError):
                self.prepare()
            self.assertFalse(self.watch_root.exists())
        write_json(self.plan_path, original)
        self.assertTrue(self.prepare()["prepared"])

    def test_prepare_and_load_recheck_every_arm_source_and_all_assets(self):
        source = Path(self.plan["sources"][self.plan["arms"][0]]["root"]) / "source.txt"
        original = source.read_bytes()
        source.write_bytes(original + b"changed\n")
        with self.assertRaisesRegex(ValueError, "source file"):
            self.prepare()
        self.assertFalse(self.watch_root.exists())
        source.write_bytes(original)
        # Give every arm its own manifest, so a verifier that checks only one arm fails.
        for arm in self.plan["arms"]:
            root = self.root / ("source-" + arm)
            root.mkdir(mode=0o700)
            (root / "source.txt").write_bytes(original)
            manifest = root / "manifest.sha256"
            manifest.write_text(digest(root / "source.txt") + "  source.txt\n")
            self.plan["sources"][arm].update(root=str(root), manifest=str(manifest), manifest_sha256=digest(manifest))
        write_json(self.plan_path, self.plan)
        self.prepare()
        for name, sealed in self.plan["assets"].items():
            path = Path(sealed["path"])
            raw = path.read_bytes()
            path.write_bytes(raw + b"changed\n")
            with self.subTest(asset=name), self.assertRaisesRegex(ValueError, "paired asset"):
                s.load_bundle(self.plan)
            self.assertEqual(s.load_bundle(self.plan, check_source=False)[1], self.watch_root)
            path.write_bytes(raw)
        for arm in self.plan["arms"]:
            path = Path(self.plan["sources"][arm]["root"]) / "source.txt"
            path.write_bytes(original + b"changed\n")
            with self.subTest(arm=arm), self.assertRaisesRegex(ValueError, "source file"):
                s.load_bundle(self.plan)
            self.assertEqual(s.load_bundle(self.plan, check_source=False)[1], self.watch_root)
            path.write_bytes(original)

    def test_preparation_is_inert_and_cannot_extend_original_admission_window(self):
        cutoff = 1000 + min(4000, self.active_seconds)
        for epoch in (999, cutoff, self.plan["deadline_epoch"]):
            self.clock.epoch = epoch
            with self.subTest(epoch=epoch), self.assertRaisesRegex(ValueError, "original admission window"):
                self.prepare()
            self.assertFalse(self.watch_root.exists())
        self.clock.epoch = cutoff - 1
        result = self.prepare()
        self.assertFalse(result["activated"])
        self.assertTrue(result["operator_permission_required"])
        manifest, _ = s.load_bundle(self.plan)
        self.assertEqual(manifest["first_epoch"], 1000)
        self.assertEqual(manifest["deadline_epoch"], 1000 + self.lease_seconds)
        self.assertFalse(Path(manifest["unit_path"]).exists())
        self.assertFalse(self.output.exists())
        before = (self.watch_root / "manifest.json").read_bytes()
        with self.assertRaises((ValueError, FileExistsError)):
            self.prepare()
        self.assertEqual((self.watch_root / "manifest.json").read_bytes(), before)

    def test_paired_only_helpers_are_sealed_without_changing_scaling_file_population(self):
        self.prepare()
        manifest, root = s.load_bundle(self.plan)
        extra = {"layers/otel/bench/ops/horizontal/paired.py", "layers/otel/bench/ops/horizontal/paired_cost.py",
                 "layers/otel/bench/ops/horizontal/paired_reports.py", "layers/otel/bench/ops/horizontal/paired_rows.py"}
        self.assertTrue(extra.isdisjoint(s.FILES))
        self.assertEqual(set(manifest["files"]), set(s.FILES) | extra)
        self.assertEqual(s._files({"schema": "otel_native_scaling_v1"}), s.FILES)
        for path in (root, *root.rglob("*")):
            self.assertEqual(path.stat().st_mode & 0o077, 0, path)
            self.assertFalse(path.is_symlink(), path)
        for name in extra:
            path = root / "bundle" / name
            original = path.read_bytes()
            path.chmod(0o600)
            path.write_bytes(original + b"# drift\n")
            with self.subTest(helper=name), self.assertRaisesRegex(ValueError, "helper bundle"):
                s.load_bundle(self.plan, check_source=False)
            path.write_bytes(original)
            source = self.repo / name
            source.write_bytes(original + b"# source drift\n")
            with self.subTest(helper=name), self.assertRaisesRegex(ValueError, "source helper"):
                s.load_bundle(self.plan)
            self.assertEqual(s.load_bundle(self.plan, check_source=False)[1], root)
            source.write_bytes(original)

    def test_load_refuses_lease_profile_replacement_and_missing_paired_bundle_entry(self):
        self.prepare()
        for changes in ({"deadline_epoch": self.plan["deadline_epoch"] + 1},
                        {"first_epoch": 1100}, {"stage_profile": "scaling" if self.profile == "pilot" else "pilot"}):
            with self.subTest(changes=changes), self.assertRaisesRegex(ValueError, "immutable"):
                s.load_bundle(dict(self.plan, **changes), check_source=False)
        path = self.watch_root / "manifest.json"
        manifest = json.loads(path.read_text())
        del manifest["files"]["layers/otel/bench/ops/horizontal/paired_cost.py"]
        manifest["seal"] = s._seal(manifest)
        path.chmod(0o600)
        write_json(path, manifest)
        with self.assertRaisesRegex(ValueError, "bundle identity"):
            s.load_bundle(self.plan, check_source=False)

    def test_exact_paired_command_registers_and_fresh_watch_proof_admits(self):
        self.live()
        self.clock.on_sleep = self.ack
        result = self.register()
        self.assertTrue(result["admitted"])
        self.assertEqual(self.clock.sleeps, [0.1])
        registration = json.loads((self.watch_root / "controller.json").read_text())
        self.assertEqual(registration["controller"]["argv"], self.controller_argv)
        self.assertNotIn("run", registration["controller"]["argv"])
        self.assertEqual(registration["deadline_epoch"], 1000 + self.lease_seconds)
        proof = self.verify()
        self.assertEqual(proof["watch"]["cgroup"], self.watch_cgroup)
        self.assertEqual(Path(proof["watch"]["cgroup"]).parent, Path(proof["manager"]["cgroup"]).parent)
        self.assertFalse(Path(proof["watch"]["cgroup"]).is_relative_to(proof["runtime"]["cgroup"]))
        self.assertTrue(self.admit()["admitted"])
        with self.assertRaisesRegex(ValueError, "already exists"):
            self.register()
        self.assertEqual(self.manager.mutations, [])

    def test_register_rejects_scaling_run_positional_unsealed_flags_and_wrong_runtime(self):
        self.live()
        good = self.controller_argv
        for argv in ([*good[:3], "run", *good[3:]], [good[0], *good[2:]],
                     [good[0], "-u", *good[1:]], [*good, "extra"], good[:-1],
                     [*good[:2], str(self.repo / "layers/otel/bench/ops/horizontal/scaling.py"), *good[3:]],
                     [*good[:4], str(self.root / "other-plan.json"), *good[5:]],
                     [*good[:6], str(self.root / "other-output"), good[-1]]):
            self.set_command(200, argv)
            with self.subTest(argv=argv), self.assertRaisesRegex(ValueError, "controller command/runtime"):
                self.register()
            self.assertFalse((self.watch_root / "controller.json").exists())
        self.set_command(200, good)
        exe = self.proc.root / "200/exe"
        exe.unlink()
        exe.symlink_to("/usr/bin/unsealed-python")
        with self.assertRaisesRegex(ValueError, "controller command/runtime"):
            self.register()
        self.assertFalse((self.watch_root / "controller.json").exists())
        exe.unlink()
        exe.symlink_to(self.manifest["python"])
        self.set_cgroup(200, self.watch_cgroup)
        with self.assertRaisesRegex(ValueError, "controller command/runtime"):
            self.register()
        self.assertFalse((self.watch_root / "controller.json").exists())
        self.assertEqual(self.clock.sleeps, [])

    def test_profile_active_deadline_blocks_registration_before_write_and_admission(self):
        self.live()
        self.assertEqual(s._active_deadline(self.plan), 1000 + self.active_seconds)
        self.clock.epoch = 1000 + self.active_seconds
        with self.assertRaisesRegex(ValueError, "active admission window"):
            self.register()
        self.assertFalse((self.watch_root / "controller.json").exists())
        self.registration()
        self.clock.epoch -= 0.01
        self.ack()
        self.assertTrue(self.admit()["admitted"])
        self.clock.epoch += 0.01
        with self.assertRaisesRegex(ValueError, "active admission window"):
            self.admit()
        self.assertEqual(self.clock.sleeps, [])

    def test_register_ack_timeout_is_bounded_by_original_active_deadline(self):
        self.live()
        self.clock.epoch = 1000 + self.active_seconds - 0.2
        with self.assertRaisesRegex(ValueError, "acknowledge"):
            self.register()
        self.assertLessEqual(len(self.clock.sleeps), 3)
        self.assertLess(self.clock(), 1000 + self.active_seconds + 0.11)
        self.assertEqual(json.loads((self.watch_root / "controller.json").read_text())["deadline_epoch"],
                         1000 + self.lease_seconds)

    def test_admission_rejects_stale_ack_pid_reuse_and_asset_drift(self):
        self.live()
        self.registration()
        self.ack()
        self.assertTrue(self.admit()["admitted"])
        for out in (self.output, self.out / "provision", self.root / "other"):
            with self.subTest(out=out), self.assertRaisesRegex(ValueError, "output mismatch"):
                self.admit(out)
        heartbeat = self.watch_root / "heartbeat.json"
        ack = json.loads(heartbeat.read_text())
        for changes in ({"healthy": False}, {"epoch": self.clock() - 3.01}, {"epoch": self.clock() + 0.01},
                        {"controller_sha256": "f" * 64}, {"proof": {}}):
            write_json(heartbeat, dict(ack, **changes))
            with self.subTest(changes=changes), self.assertRaisesRegex(ValueError, "heartbeat"):
                self.admit()
        write_json(heartbeat, ack)
        self.set_start(200, 5001)
        with self.assertRaisesRegex(ValueError, "reused/changed"):
            self.admit()
        self.set_start(200, 5000)
        asset = Path(self.plan["assets"]["go_binary"]["path"])
        asset.write_bytes(asset.read_bytes() + b"changed\n")
        with self.assertRaisesRegex(ValueError, "paired asset"):
            self.admit()

    def test_bound_uses_profile_hard_deadline_and_cannot_renew_it_on_restart(self):
        self.live()
        self.set_command(300, [*self.watch_argv[:3], "bound", *self.watch_argv[4:]])
        with mock.patch.object(os, "getpid", return_value=300), mock.patch.object(s.time, "monotonic_ns", return_value=1000000000):
            first = s.bound(self.watch_root, runner=self.manager, proc_root=self.proc.root, clock=self.clock)
        self.assertEqual(first["seconds"], self.lease_seconds - 105)
        self.clock.epoch += 100
        with mock.patch.object(os, "getpid", return_value=300), mock.patch.object(s.time, "monotonic_ns", return_value=101000000000):
            second = s.bound(self.watch_root, runner=self.manager, proc_root=self.proc.root, clock=self.clock)
        self.assertEqual(second["seconds"], self.lease_seconds - 205)
        self.assertEqual(first["deadline_epoch"], 1000 + self.lease_seconds)
        self.assertEqual(second["deadline_epoch"], first["deadline_epoch"])
        self.assertEqual(len(self.manager.mutations), 2)
        self.assertTrue(all(argv[:5] == ["/usr/bin/systemctl", "--user", "set-property", "--runtime", self.unit]
                            for argv in self.manager.mutations))

    def test_original_cleanup_epoch_fires_while_controller_is_live(self):
        self.live()
        self.registration()
        (provision._state(self.out) / "baseline").mkdir(mode=0o700, parents=True)
        calls = []

        def cleanup(plan, out, *, clock, sleep):
            calls.append((plan, Path(out), clock()))
            return {"passed": True, "within_deadline": True}

        self.clock.epoch = 1000 + self.cleanup_seconds - 1
        self.assertEqual(self.watch(teardown=cleanup), 0)
        self.assertEqual(self.clock.sleeps, [1])
        self.assertEqual(calls, [(self.plan, self.out, 1000 + self.cleanup_seconds)])
        receipt = json.loads((self.watch_root / "exit.json").read_text())
        self.assertEqual(receipt["reason"], "original absolute cleanup time")
        self.assertEqual(receipt["deadline_epoch"], 1000 + self.lease_seconds)

    def test_expired_watch_never_runs_teardown_or_renews_lease(self):
        self.live()
        self.registration()
        self.clock.epoch = 1000 + self.lease_seconds
        cleanup = mock.Mock(side_effect=AssertionError("expired watch cannot clean or renew"))
        self.assertEqual(self.watch(teardown=cleanup), 77)
        cleanup.assert_not_called()
        self.assertFalse((self.watch_root / "heartbeat.json").exists())
        receipt = json.loads((self.watch_root / "exit.json").read_text())
        self.assertFalse(receipt["passed"])
        self.assertEqual(receipt["deadline_epoch"], 1000 + self.lease_seconds)
        self.assertEqual(self.manager.mutations, [])


class PairedScalingSupervisorTests(PairedPilotSupervisorTests):
    profile = "scaling"
    lease_seconds = 7200
    active_seconds = 6000
    cleanup_seconds = 6600


class PairedControllerLossTests(PairedSupervisorFixture):
    def test_fake_midcampaign_controller_loss_runs_real_owned_cleanup_and_ten_kind_baseline_proof(self):
        self.live()
        self.registration()
        self.ack()
        state = provision._state(self.out)
        baseline_dir = state / "baseline"
        baseline_dir.mkdir(mode=0o700, parents=True)
        baseline = {kind: [{"id": i + 1, "name": "protected-" + kind, "labels": {}}]
                    for i, kind in enumerate(provision.KINDS)}
        self.assertEqual(len(baseline), 10)
        for kind, items in baseline.items():
            path = baseline_dir / ("before-" + kind + ".json")
            path.write_text(json.dumps(items) + "\n")
            path.chmod(0o600)
        write_json(state / "resources.json", {"first_create_epoch": 1001})
        cloud = copy.deepcopy(baseline)
        created = {"server": ["native-test-node1", "native-test-client1"],
                   "network": ["native-test-net"], "firewall": ["native-test-fw"],
                   "ssh-key": ["native-test-key"], "placement-group": ["native-test-spread"]}
        expected = set()
        for kind, names in created.items():
            for name in names:
                rid = 100 + len(expected)
                cloud[kind].append({"id": rid, "name": name, "labels": {"campaign": self.plan["campaign"]}})
                expected.add((kind, rid))
        deleted, provider_calls, healthy = [], [], []

        def provider(argv, *, timeout):
            # This checks the fake identity at the effect boundary; it is not an OS-kill test.
            own = s.process_identity(os.getpid(), self.proc.root)
            self.assertEqual(own["pid"], 300)
            self.assertEqual(own["cgroup"], self.watch_cgroup)
            self.assertEqual(Path(own["cgroup"]).parent, Path(s.process_identity(50, self.proc.root)["cgroup"]).parent)
            self.assertFalse(Path(own["cgroup"]).is_relative_to(self.runtime_cgroup))
            self.assertFalse((self.proc.root / "200/stat").exists())
            self.assertGreater(timeout, 0)
            self.assertLessEqual(timeout, 30)
            self.assertEqual(argv[0], self.plan["hcloud"])
            kind, operation = argv[1:3]
            provider_calls.append((list(argv), timeout))
            if operation == "list":
                self.assertEqual(argv[3:], ["-o", "json"])
                return 0, json.dumps(cloud[kind]), ""
            self.assertEqual(operation, "delete")
            self.assertEqual(len(argv), 4)
            rid = int(argv[3])
            self.assertIn((kind, rid), expected)
            self.assertNotIn(rid, [item["id"] for item in baseline[kind]])
            deleted.append((kind, rid))
            cloud[kind] = [item for item in cloud[kind] if item["id"] != rid]
            return 0, "", ""

        def disappear():
            healthy.append(json.loads((self.watch_root / "heartbeat.json").read_text()))
            (self.proc.root / "200/stat").unlink()
            self.clock.on_sleep = None

        def cleanup(plan, out, *, clock, sleep):
            return provision.teardown(plan, out, runner=provider, clock=clock, sleep=sleep)

        # _list's legacy clock default is real time; route only its clock, retaining
        # real inventory parsing, ownership classification, deletion and verification.
        def list_at_fake_time(plan, kind, runner):
            return provision._inventory.list_kind(plan["hcloud"], kind,
                lambda argv: provision._inventory_run(plan, runner, argv, self.clock))

        self.clock.on_sleep = disappear
        with mock.patch.object(provision, "_list", side_effect=list_at_fake_time):
            self.assertEqual(self.watch(teardown=cleanup), 0)
        self.assertEqual(len(healthy), 1)
        self.assertTrue(healthy[0]["healthy"])
        self.assertEqual(healthy[0]["proof"]["watch"]["cgroup"], self.watch_cgroup)
        self.assertEqual(set(deleted), expected)
        self.assertEqual(len(deleted), 6)
        self.assertEqual(cloud, baseline)
        receipt = json.loads((state / "teardown-complete.json").read_text())
        self.assertTrue(receipt["passed"])
        self.assertTrue(receipt["within_deadline"])
        self.assertEqual(set(receipt["inventory"]["kinds"]), set(provision.KINDS))
        self.assertEqual(receipt["inventory"]["protected_resource_count"], 10)
        for kind, summary in receipt["inventory"]["kinds"].items():
            self.assertEqual(summary, {"passed": True, "before": 1, "after": 1, "missing": [], "unexpected": []}, kind)
        after_dir = Path(receipt["inventory_dir"])
        self.assertTrue(all((after_dir / ("after-" + kind + ".json")).exists() for kind in provision.KINDS))
        self.assertTrue(provider_calls)
        self.assertEqual(self.clock.sleeps, [1, 2])
        exit_receipt = json.loads((self.watch_root / "exit.json").read_text())
        self.assertTrue(exit_receipt["passed"])
        self.assertIn("controller/supervision lost", exit_receipt["reason"])
        self.assertEqual(exit_receipt["deadline_epoch"], 4600)
        self.assertEqual(self.manager.mutations, [])


if __name__ == "__main__":
    unittest.main()
