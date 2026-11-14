#!/usr/bin/env python3
"""Fake-only paired orchestration, typed arm and nonvacuous report regressions."""
import builtins
import copy
import json
import io
from pathlib import Path
import tempfile
import unittest
from unittest import mock

import driver as d
import paired as q
import paired_reports as r


def early_error(job, failure="corpus reopen failed"):
    value = r.fake_ingest(job)
    value.update(status="error", error=failure, ingest_resources=None,
                 ingest_resources_unavailable_reason="ingest window not started: " + failure, samples=[])
    for key in ("frames", "received_spans", "accepted_spans", "rejected_spans", "failed_spans", "queued_spans_at_exit",
                "ingest_wall_ns", "admission_phase_ns", "drain_ns"):
        value[key] = 0
    for key in ("native_library_mapped", "native_mapping_rule"):
        value.pop(key, None)
    for key in r.COUNTERS:
        if value[key]["availability"] == "available":
            value[key]["value"] = 0
    return value


class Fixture(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.plan = q.fake_plan(self.root / "fixtures")
        self.backend = q.FakeBackend(self.plan["first_epoch"])
        self.assets = self.backend.remote_assets(self.plan)
        self.cluster = {"path": "/srv/fdb.cluster", "sha256": "a" * 64}
        self.layout = {"path": "/srv/layout.json", "sha256": "b" * 64}

    def job(self, arm="rust_native", phase="prerequisite"):
        ns = q.namespace(self.plan, 1, arm, phase, 1)
        build = q.rust_job if arm == "rust_native" else q.go_job
        return build(self.plan, arm, 1, phase, 1, self.layout, self.cluster, self.assets,
                     "/srv/otel-paired/" + ns + "/ingest.json")


class PlanTests(Fixture):
    def test_three_and_two_arm_stage_accounting_and_shared_original_clock(self):
        before = copy.deepcopy(self.plan)
        self.assertIs(q.validate_plan(self.plan), self.plan)
        self.assertEqual(self.plan, before)
        self.assertEqual(q.authorized_rows(self.plan), 8550000)
        for profile, expected in (("pilot", 1900000), ("scaling", 5700000)):
            value = q.fake_plan(self.root / profile, profile=profile, arms=("go_native", "rust_native"))
            self.assertEqual(q.authorized_rows(value), expected)
            self.assertEqual(len(q.held_roles(value)), 2 if profile == "pilot" else 6)
            self.assertEqual(value["deadline_epoch"] - value["first_epoch"], 3600 if profile == "pilot" else 7200)
            q.validate_plan(value)

    def test_required_typed_fields_reject_missing_boolean_float_and_changed_profiles(self):
        for key in ("count", "measured_count", "measured_seconds", "batch", "workers", "gomaxprocs",
                    "storage_process_count", "storage_max_outstanding", "writer_batch_size", "writer_workers"):
            for bad in (None, True, 0, 1.0, "64"):
                value = copy.deepcopy(self.plan); value[key] = bad
                with self.subTest(key=key, bad=bad), self.assertRaises(ValueError):
                    q.validate_plan(value)
        for key, bad in (("schema", "otel_native_scaling_v1"), ("approved", 1), ("location", "fsn1"),
                         ("replication", "double"), ("cleanup_mode", "foreground_only"),
                         ("oracle", "none"), ("load_profile", "fixed_offered_load_600_v1"),
                         ("deadline_epoch", self.plan["deadline_epoch"] + 1), ("arms", ["go_native"]),
                         ("arms", ["go_native", "go_native"]), ("arm_order", [["rust_native"]])):
            value = copy.deepcopy(self.plan); value[key] = bad
            with self.subTest(key=key, bad=bad), self.assertRaises(ValueError):
                q.validate_plan(value)

    def test_shared_workload_has_no_per_arm_override_or_parameter_defaults(self):
        value = q.fake_plan(self.root / "queue", kind="collector_queue")
        q.validate_plan(value)
        for key in value["workload"]:
            bad = copy.deepcopy(value); del bad["workload"][key]
            with self.subTest(missing=key), self.assertRaises(ValueError):
                q.validate_plan(bad)
        for changes in ({"producers": 33}, {"consumers": True}, {"queue_capacity_spans": 0},
                        {"batch_min_spans": 257, "batch_max_spans": 256}, {"writer_workers": 1},
                        {"block_on_overflow": 1}, {"retry_enabled": 0}, {"retry_max_ms": 1}):
            bad = copy.deepcopy(value); bad["workload"].update(changes)
            with self.subTest(changes=changes), self.assertRaises(ValueError):
                q.validate_plan(bad)
        bad = copy.deepcopy(value); bad["sources"]["rust_native"]["workload"] = {"kind": "writer_direct"}
        with self.assertRaises(ValueError):
            q.validate_plan(bad)

    def test_queue_retry_and_request_bounds_match_portable_schema_at_every_request_size(self):
        value = q.fake_plan(self.root / "queue-constraints", kind="collector_queue")
        schema = d.read(Path(q.__file__).with_name("paired-job-v1.schema.json"))["properties"]["workload"]["oneOf"][1]
        rules = schema["allOf"]
        self.assertEqual(len(rules), 64)
        self.assertEqual([rule["if"]["properties"]["producer_batch_spans"]["const"] for rule in rules[1:]], list(range(2, 65)))
        self.assertEqual(schema["properties"]["producer_batch_spans"]["maximum"], 64)
        self.assertEqual(schema["properties"]["queue_capacity_spans"]["minimum"], 1)
        # Evaluate the standard if/properties/const -> then/minimum subset used
        # by these constraints, without an undeclared JSON Schema dependency.
        def schema_allows(block):
            for rule in rules:
                self.assertEqual(set(rule), {"if", "then"})
                matches = all(block[key] == predicate["const"] for key, predicate in rule["if"]["properties"].items())
                if matches and any(block[key] < predicate["minimum"] for key, predicate in rule["then"]["properties"].items()):
                    return False
            return True
        for request in range(1, 65):
            for capacity in {1, max(1, request - 1), request, request + 1, 10000000}:
                for enabled, initial in ((False, 0), (True, 0), (True, 1), (True, 500)):
                    block = {**value["workload"], "producer_batch_spans": request, "queue_capacity_spans": capacity,
                             "batch_min_spans": 1, "retry_enabled": enabled, "retry_initial_ms": initial}
                    expected = request <= capacity and (not enabled or initial > 0)
                    with self.subTest(request=request, capacity=capacity, enabled=enabled, initial=initial):
                        self.assertEqual(schema_allows(block), expected)
                        if expected:
                            self.assertIs(q.workload(block), block)
                        else:
                            with self.assertRaises(ValueError):
                                q.workload(block)
        for block in ({**value["workload"], "producer_batch_spans": 1},
                      {**value["workload"], "producer_batch_spans": 1, "producers": 1, "consumers": 1, "batch_max_spans": 64}):
            self.assertIs(q.validate_plan({**value, "workload": block})["workload"], block)
        for changes in ({"retry_initial_ms": 0}, {"producer_batch_spans": 64, "queue_capacity_spans": 63, "batch_min_spans": 1}):
            with self.assertRaises(ValueError):
                q.validate_plan({**value, "workload": {**value["workload"], **changes}})

    def test_sources_assets_and_additional_artifacts_have_real_nonempty_seals(self):
        for name in self.plan["assets"]:
            path = Path(self.plan["assets"][name]["path"])
            original = path.read_bytes(); path.write_bytes(original + b"changed")
            with self.subTest(name=name), self.assertRaisesRegex(ValueError, "changed paired asset"):
                q.validate_plan(self.plan)
            path.write_bytes(original)
        empty = copy.deepcopy(self.plan)
        path = Path(empty["assets"]["descriptor_set"]["path"]); original = path.read_bytes(); path.write_bytes(b"")
        empty["assets"]["descriptor_set"]["sha256"] = q.p.sha(path)
        with self.assertRaisesRegex(ValueError, "changed paired asset"):
            q.validate_plan(empty)
        path.write_bytes(original)
        for changes in ({"assets": {}}, {"sources": {}}, {"source_revision": "old"}):
            if "source_revision" in changes:
                value = copy.deepcopy(self.plan); value["sources"]["rust_native"]["revision"] = "old"
            else:
                value = dict(self.plan, **changes)
            with self.assertRaises(ValueError):
                q.validate_plan(value)
        q.verify_sources(self.plan)
        (self.root / "fixtures/source.txt").write_text("changed")
        with self.assertRaisesRegex(ValueError, "source file"):
            q.verify_sources(self.plan)

    def test_four_executable_seals_and_dedicated_verifier_exist_without_pure_ingest(self):
        for name in q.builds.EXECUTABLES:
            self.assertIn(name, self.plan["assets"])
        self.assertEqual(len({self.plan["assets"][name]["sha256"] for name in q.builds.EXECUTABLES}), 4)
        subset = q.fake_plan(self.root / "without-pure-ingest", arms=("go_native", "rust_native"))
        q.validate_plan(subset)
        self.assertIn("go_pure", subset["sources"])
        self.assertIn("control_pure_binary", subset["assets"])
        self.assertEqual(q.control_identity(subset)["binary_sha256"], subset["assets"]["control_pure_binary"]["sha256"])
        self.assertEqual(q.verifier_identity(subset)["binary_sha256"], subset["assets"]["verifier_binary"]["sha256"])
        for field, key in (("assets", "control_pure_binary"), ("assets", "verifier_binary"), ("sources", "go_pure")):
            bad = copy.deepcopy(subset); del bad[field][key]
            with self.assertRaises(ValueError):
                q.validate_plan(bad)
        for name in q.builds.EXECUTABLES:
            bad = copy.deepcopy(self.plan); del bad["assets"][name]["build_receipt"]
            with self.assertRaisesRegex(ValueError, "build receipt"):
                q.validate_plan(bad)
        bad = copy.deepcopy(self.plan)
        bad["assets"]["control_native_binary"] = copy.deepcopy(bad["assets"]["control_pure_binary"])
        with self.assertRaisesRegex(ValueError, "distinct executable"):
            q.validate_plan(bad)
        bad = copy.deepcopy(subset); bad["sources"]["go_native"]["revision"] = "b" * 40
        with self.assertRaisesRegex(ValueError, "same collector source"):
            q.validate_plan(bad)

    def test_paid_validation_replays_each_arm_preflight_oracle_before_effects(self):
        self.plan = q.fake_plan(self.root / "local-queue", kind="collector_queue")
        self.assets = self.backend.remote_assets(self.plan)
        value = copy.deepcopy(self.plan)
        value["fake_provider"] = False
        source = value["sources"]["go_pure"]
        value.update(binary=value["assets"]["control_pure_binary"]["path"], binary_sha256=value["assets"]["control_pure_binary"]["sha256"],
                     source_root=source["root"], source_manifest=source["manifest"], input_manifest_sha256=source["manifest_sha256"],
                     source_revision=source["revision"], hcloud="/private/hcloud", controller_private_key="/private/key",
                     controller_public_key="/private/key.pub", controller_cidr="192.0.2.1/32")
        for name in ("fdbserver", "fdbcli"):
            value[name] = value["assets"][name]["path"]; value[name + "_sha256"] = value["assets"][name]["sha256"]
        proofs = {}
        for arm in value["arms"]:
            job = self.job(arm)
            files = {}
            ingest = r.fake_ingest(job)
            if arm != "go_pure":
                ingest["native_mapping_rule"] = "btrfs_superblock"
            for name, data in (("ingest_job", job), ("ingest_report", ingest),
                               ("verification", r.fake_verify(job, control_identity=q.verifier_identity(value)))):
                path = self.root / (arm + "-" + name + ".json"); d.write(path, data)
                files[name] = {"path": str(path), "sha256": q.p.sha(path)}
            proofs[arm] = files
        value["preflight_verifications"] = proofs
        with mock.patch.object(q, "NATIVE_SHA256", value["assets"]["libfdb_c"]["sha256"]), \
                mock.patch.object(q.builds, "readelf_dynamic", side_effect=lambda path: q.builds.fake_readelf(Path(path).name)):
            q.validate_plan(value)
            for arm in value["arms"]:
                name = q.ARM_EXECUTABLES[arm]
                ref = value["assets"][name]["build_receipt"]
                receipt_path = Path(ref["path"]); original = receipt_path.read_bytes()
                try:
                    for mode in ("fastbuild", "dbg"):
                        for field in ("compilation_mode", "configured_output_path", "build_command"):
                            built = d.loads(original.decode())
                            if field == "configured_output_path":
                                built[field] = built[field].replace("-opt/", "-" + mode + "/")
                            elif field == "build_command":
                                built[field] = [arg for arg in built[field] if arg != "--compilation_mode=opt"]
                            else:
                                built[field] = mode
                            receipt_path.write_text(json.dumps(built, sort_keys=True) + "\n")
                            bad = copy.deepcopy(value)
                            bad["assets"][name]["build_receipt"]["sha256"] = q.p.sha(receipt_path)
                            with self.subTest(arm=arm, mode=mode, field=field), self.assertRaisesRegex(ValueError, "opt"):
                                q.validate_plan(bad)
                finally:
                    receipt_path.write_bytes(original)
            missing = copy.deepcopy(value); del missing["preflight_verifications"]["rust_native"]
            with self.assertRaisesRegex(ValueError, "every selected arm"):
                q.validate_plan(missing)
            failed = d.read(proofs["rust_native"]["verification"]["path"])
            failed["report"]["oracle"]["status"] = "not_requested"
            path = self.root / "missing-oracle.json"; d.write(path, failed)
            bad = copy.deepcopy(value)
            bad["preflight_verifications"]["rust_native"]["verification"] = {"path": str(path), "sha256": q.p.sha(path)}
            with self.assertRaisesRegex(ValueError, "paid gate"):
                q.validate_plan(bad)
            wrong_binary = self.job("go_native")
            wrong_binary["binary_sha256"] = value["assets"]["control_pure_binary"]["sha256"]
            path = self.root / "wrong-native-binary-job.json"; d.write(path, wrong_binary)
            bad = copy.deepcopy(value)
            bad["preflight_verifications"]["go_native"]["ingest_job"] = {"path": str(path), "sha256": q.p.sha(path)}
            with self.assertRaisesRegex(ValueError, "current source/binary"):
                q.validate_plan(bad)
            bad = copy.deepcopy(value); bad["binary_sha256"] = "d" * 64
            with self.assertRaisesRegex(ValueError, "aliases"):
                q.validate_plan(bad)
            minimal = self.job("rust_native")
            minimal["workload"].update(producers=1, consumers=1, batch_max_spans=64)
            path = self.root / "minimal-point1-job.json"; d.write(path, minimal)
            bad = copy.deepcopy(value)
            bad["preflight_verifications"]["rust_native"]["ingest_job"] = {"path": str(path), "sha256": q.p.sha(path)}
            with self.assertRaisesRegex(ValueError, "matched pipeline"):
                q.validate_plan(bad)

    def test_arm_order_repetition_namespace_and_immutable_output(self):
        names = []
        for hosts in q.stages(self.plan):
            for arm in self.plan["arms"]:
                for phase in ("prerequisite", "measured"):
                    for client in range(1, hosts+1):
                        names.append(q.namespace(self.plan, hosts, arm, phase, client))
        self.assertEqual(len(names), 36)
        self.assertEqual(len(set(names)), 36)
        self.assertNotEqual(q.namespace(self.plan, 1, "go_native", "measured", 1, 1),
                            q.namespace(self.plan, 1, "go_native", "measured", 1, 2))
        with self.assertRaises(ValueError):
            q.go_job(self.plan, "go_native", 1, "measured", 1, self.layout, self.cluster, self.assets, "/foreign/ingest.json")

    def test_sealed_per_host_transfer_receipts_reject_each_missing_or_corrupt_arm(self):
        resources = self.backend.prepare(self.plan, {})
        receipts = self.backend.transfer_assets(self.plan, resources)
        q.validate_transfers(self.plan, resources, receipts)
        for role in resources:
            bad = copy.deepcopy(receipts); del bad[role]
            with self.assertRaises(ValueError):
                q.validate_transfers(self.plan, resources, bad)
        for key in self.plan["assets"]:
            bad = copy.deepcopy(receipts); bad["node1"]["assets"][key]["sha256"] = "c" * 64
            with self.subTest(key=key), self.assertRaises(ValueError):
                q.validate_transfers(self.plan, resources, bad)


class GeometryTests(Fixture):
    def test_pilot_geometry_and_larger_log_profile_are_data_driven_and_shared(self):
        import provision
        q.native_geometry(self.plan["native_geometry"])
        nodes = {role: {"id": i+1, "role": role, "private_ip": "10.93.0." + str(i+1)}
                 for i, role in enumerate(q.held_roles(self.plan))}
        layout = provision.render_cluster(self.plan, nodes, 1, q.arm_id(1, "rust_native"))
        self.assertEqual(layout["schema"], q.SCHEMA)
        self.assertEqual(layout["native_geometry"], self.plan["native_geometry"])
        self.assertEqual(layout["budgets"]["fdb_host"]["max_process_memory_gib"], 15)
        log = next(x for x in layout["processes"] if x["port"] == 4504)
        self.assertEqual(log["args"][log["args"].index("--memory")+1], "2048MiB")
        self.assertEqual(log["cache_mib"], 64)
        storage = [x for x in layout["processes"] if x["process_class"] == "storage"]
        self.assertEqual(len(storage), 3)
        self.assertEqual({x["memory_limit_bytes"] for x in storage}, {3 * q.p.GIB})
        larger = copy.deepcopy(self.plan)
        larger["native_geometry"]["log"]["memory_mib"] = 4096
        with self.assertRaisesRegex(ValueError, "headroom"):
            q.validate_plan(larger)
        larger["native_geometry"]["fdb_slice"]["memory_max_mib"] = 24576
        q.validate_plan(larger)
        rendered = provision.render_cluster(larger, nodes, 1, q.arm_id(1, "rust_native"))
        self.assertEqual(rendered["budgets"]["fdb_host"]["max_process_memory_gib"], 17)
        self.assertEqual(rendered["budgets"]["fdb_host"]["memory_max_gib"], 24)
        self.assertIn("MemoryMax=24576M", provision._slice("fdb", geometry=larger["native_geometry"]))
        self.assertIn("MemoryMax=16384M", provision._slice("client", geometry=larger["native_geometry"]))

    def test_geometry_refuses_untyped_unknown_oversized_or_per_arm_overrides(self):
        for changes in (("log", "memory_mib", True), ("log", "cache_mib", 4096),
                        ("storage", "processes_per_host", 4), ("storage", "max_outstanding", 128),
                        ("fdb_slice", "memory_max_mib", 65536), ("fdb_slice", "swap_max_bytes", 1),
                        ("client_slice", "cpu_quota_percent", 800), ("client_slice", "allowed_cpus", "4-7")):
            value = copy.deepcopy(self.plan); value["native_geometry"][changes[0]][changes[1]] = changes[2]
            with self.subTest(changes=changes), self.assertRaises(ValueError):
                q.validate_plan(value)
        value = copy.deepcopy(self.plan); value["arm_geometry"] = {"rust_native": value["native_geometry"]}
        with self.assertRaises(ValueError):
            q.validate_plan(value)
        for key in self.plan["native_geometry"]:
            value = copy.deepcopy(self.plan); del value["native_geometry"][key]
            with self.assertRaises(ValueError):
                q.validate_plan(value)

    def test_every_native_provider_action_requires_persisted_cost_and_stage_allocation(self):
        import provision
        import paired_cost
        state = self.root / "cost-state"; state.mkdir(mode=0o700)
        provision._json(state / "resources.json", {"ids": {"server": []}})
        with self.assertRaisesRegex(provision.ProvisionError, "ledger missing"):
            provision._paired_cost_step(self.plan, state, lambda: self.plan["first_epoch"])
        ledger = paired_cost.admit(self.plan["pricing"], expected_seal=self.plan["pricing_seal_sha256"],
                                  first_epoch=self.plan["first_epoch"], deadline_epoch=self.plan["deadline_epoch"],
                                  now_epoch=self.plan["first_epoch"])
        provision._json(state / "paired-cost-ledger.json", ledger)
        provision._paired_cost_step(self.plan, state, lambda: self.plan["first_epoch"])
        with self.assertRaisesRegex(provision.ProvisionError, "EUR10"):
            provision._paired_cost_step(dict(self.plan, prior_cost_upper_eur="9"), state, lambda: self.plan["first_epoch"])
        with self.assertRaisesRegex(paired_cost.CostError, "stale"):
            provision._paired_cost_step(self.plan, state, lambda: self.plan["first_epoch"] + 3601)
        provision._json(state / "resources.json", {"ids": {"server": [999]}}, replace=True)
        with self.assertRaisesRegex(paired_cost.CostError, "unexpected"):
            provision._paired_cost_step(self.plan, state, lambda: self.plan["first_epoch"])

    def test_paired_zstd_pack_covers_every_extra_asset_and_rejects_corruption(self):
        import provision
        from test_provision import fake_zstd, fake_unzstd
        calls = []
        def runner(argv, timeout=120):
            calls.append(argv)
            source = Path(argv[-1]); target = Path(argv[argv.index("-o")+1])
            target.write_bytes(fake_unzstd(source.read_bytes()) if "-d" in argv else fake_zstd(source.read_bytes()))
            return 0, "", ""
        state = self.root / "transfer-state"; state.mkdir(mode=0o700)
        packed = provision._pack_assets(self.plan, state, runner, lambda: self.plan["first_epoch"])
        self.assertEqual(set(packed), set(self.plan["assets"]))
        self.assertEqual(len(calls), 2 * len(self.plan["assets"]))
        for name, item in packed.items():
            self.assertEqual(item["sha256"], self.plan["assets"][name]["sha256"])
            self.assertEqual(Path(item["path"]).stat().st_mode & 0o077, 0)
        bad = self.root / "bad-transfer"; bad.mkdir(mode=0o700)
        def corrupt(argv, timeout=120):
            result = runner(argv, timeout)
            if "-d" in argv:
                Path(argv[argv.index("-o")+1]).write_bytes(b"corrupt")
            return result
        with self.assertRaisesRegex(provision.ProvisionError, "sealed SHA"):
            provision._pack_assets(self.plan, bad, corrupt, lambda: self.plan["first_epoch"])

    def test_host_runtime_guard_uses_sealed_role_memory_not_historical16g(self):
        value = copy.deepcopy(self.plan)
        value["native_geometry"]["log"]["memory_mib"] = 4096
        value["native_geometry"]["fdb_slice"]["memory_max_mib"] = 24576
        campaign = d.Campaign(value, self.root / "observation", None)
        campaign.remote_root = "/srv/otel-horizontal/" + value["campaign"]
        campaign.remote_harness = campaign.remote_root + "/harness"
        campaign.nodes = {"node1": {"id": 1}}
        sample = {"node_id": 1, "epoch": 1000, "total_bytes": 360 * q.p.GIB, "available_bytes": 300 * q.p.GIB,
                  "filesystem": "ext4", "mem_available_bytes": 32 * q.p.GIB, "allocated_bytes": q.p.GIB,
                  "pswpin": 0, "pswpout": 0, "fdb_pids": {"1": "100"},
                  "slices": {"horizontal-fdb.slice": {"cpu.max": "400000 100000", "cpuset.cpus.effective": "0-3",
                    "memory.max": str(24 * q.p.GIB), "memory.swap.max": "0", "memory.current": "1000", "memory.events": "oom 0\noom_kill 0"}}}
        campaign.command = mock.Mock(return_value=json.dumps(sample))
        self.assertEqual(campaign.host_samples()["node1"]["node_id"], 1)
        for key, bad in (("memory.max", str(16 * q.p.GIB)), ("memory.current", str(24 * q.p.GIB)),
                         ("memory.events", "oom 1\noom_kill 0"), ("memory.swap.max", "1")):
            changed = copy.deepcopy(sample); changed["slices"]["horizontal-fdb.slice"][key] = bad
            campaign.command = mock.Mock(return_value=json.dumps(changed))
            with self.subTest(key=key), self.assertRaisesRegex(ValueError, "budget/OOM"):
                campaign.host_samples()


class ProducerTests(Fixture):
    def test_every_arm_uses_own_typed_job_with_exactly_one_json_argv(self):
        for arm in self.plan["arms"]:
            job = self.job(arm)
            argv = q.invocation(self.plan, job, "/srv/job.json", self.assets)
            self.assertEqual(len(argv), 2)
            self.assertEqual(argv[0], self.assets[q.ARM_EXECUTABLES[arm]])
            self.assertFalse(any(x.startswith("--") for x in argv))
            self.assertEqual(job["mode"], "ingest")
            self.assertEqual(job["writer_workers"], 1)
            self.assertIs(job["bulk_index_conflicts"], False)
            self.assertIsNone(job["client_library"] if arm == "go_pure" else None)
            if arm == "rust_native":
                self.assertEqual(job["client_library"], self.assets["libfdb_c"])
            elif arm == "go_native":
                self.assertIn("control_native_binary.bundle/", job["client_library"])
                self.assertEqual(Path(job["client_library"]).name, "libfdb_c.so")
            q.envelope(job, r.fake_ingest(job))
        with self.assertRaises(ValueError):
            q.rust_job(self.plan, "go_native")
        with self.assertRaises(ValueError):
            q.go_job(self.plan, "rust_native")
        path = self.root / "job.json"; d.write(path, self.job())
        self.assertEqual(q.job_seal(self.job()), q.p.sha(path))

    def test_wrong_binary_per_arm_is_rejected_and_only_verify_uses_dedicated_verifier(self):
        for arm in self.plan["arms"]:
            job = self.job(arm)
            for other in q.builds.EXECUTABLES:
                if other != q.ARM_EXECUTABLES[arm]:
                    bad = copy.deepcopy(job); bad["binary_sha256"] = self.plan["assets"][other]["sha256"]
                    with self.subTest(arm=arm, wrong=other), self.assertRaisesRegex(ValueError, "wrong executable"):
                        q.invocation(self.plan, bad, "/srv/job.json", self.assets)
            for mode in ("corpus", "setup", "verify"):
                control = copy.deepcopy(job)
                control.update(q.verifier_identity(self.plan) if mode == "verify" else q.control_identity(self.plan), mode=mode)
                if mode == "corpus":
                    fields = {"schema", "mode", "arm", "binary_sha256", "source_revision", "source_manifest_sha256",
                              "timeout_seconds", "output", "corpus_config", "expected_frames", "row_budget", "corpus_file"}
                    control = {key: value for key, value in control.items() if key in fields}
                elif mode == "setup":
                    del control["layout_sha256"]
                else:
                    control = r.verification_job(control)
                executable = "verifier_binary" if mode == "verify" else "control_pure_binary"
                self.assertEqual(q.invocation(self.plan, control, "/srv/control-job.json", self.assets)[0], self.assets[executable])
                for wrong in set(q.builds.EXECUTABLES) - {executable}:
                    control["binary_sha256"] = self.plan["assets"][wrong]["sha256"]
                    with self.subTest(arm=arm, mode=mode, wrong=wrong), self.assertRaisesRegex(ValueError, "wrong executable"):
                        q.invocation(self.plan, control, "/srv/control-job.json", self.assets)

    def test_mapping_evidence_distinguishes_local_preflight_from_paid_hosts(self):
        for arm in self.plan["arms"]:
            job = self.job(arm); raw = r.fake_ingest(job)
            self.assertTrue(q.envelope(job, raw)["accepted"])
            self.assertTrue(q.envelope(job, raw, paid_host=True)["accepted"])
            for bad in (not raw["native_library_mapped"], None, 1):
                wrong = copy.deepcopy(raw); wrong["native_library_mapped"] = bad
                self.assertFalse(q.envelope(job, wrong)["accepted"])
            wrong = copy.deepcopy(raw); del wrong["native_library_mapped"]
            self.assertFalse(q.envelope(job, wrong)["accepted"])
            if arm != "go_pure":
                raw["native_mapping_rule"] = "btrfs_superblock"
                self.assertTrue(q.envelope(job, raw)["accepted"])
                self.assertEqual(q.envelope(job, raw)["raw_report"]["native_mapping_rule"], "btrfs_superblock")
                self.assertFalse(q.envelope(job, raw, paid_host=True)["accepted"])
                proof = r.fake_verify(job, control_identity=q.verifier_identity(self.plan))
                joined = q.joined_envelope(job, raw, proof, "fixture", self.plan)
                self.assertFalse(joined["paid_host"])
                joined["paid_host"] = True
                with self.assertRaises(ValueError):
                    r.validate_distinct_result(joined)
                for bad in (None, "unknown"):
                    wrong = copy.deepcopy(raw); wrong["native_mapping_rule"] = bad
                    self.assertFalse(q.envelope(job, wrong)["accepted"])
            else:
                for bad in (None, "exact_stat", "btrfs_superblock"):
                    wrong = copy.deepcopy(raw); wrong["native_mapping_rule"] = bad
                    self.assertFalse(q.envelope(job, wrong)["accepted"])
            proof = r.fake_verify(job, control_identity=q.verifier_identity(self.plan))
            self.assertIs(proof["report"]["native_library_mapped"], False)
            proof["report"]["native_library_mapped"] = True
            with self.assertRaises(ValueError):
                q.verify_audit(proof, job, "fixture", plan=self.plan)

    def test_mode_specific_jobs_reject_unknown_inputs_aliases_and_seed_overflow(self):
        job = self.job("go_pure")
        setup = {**copy.deepcopy(job), "mode": "setup"}; del setup["layout_sha256"]
        verify = r.verification_job(job)
        common = {"schema", "mode", "arm", "binary_sha256", "source_revision", "source_manifest_sha256", "timeout_seconds", "output",
                  "corpus_config", "expected_frames", "row_budget", "corpus_file"}
        corpus = {key: copy.deepcopy(value) for key, value in job.items() if key in common}; corpus["mode"] = "corpus"
        for mode in (job, setup, verify, corpus):
            q.validate_job(mode)
            bad = copy.deepcopy(mode); bad["unknown"] = True
            with self.assertRaisesRegex(ValueError, "exact mode"):
                q.validate_job(bad)
            bad = copy.deepcopy(mode); bad["corpus_config"]["seed"] = 2 ** 64 - 1
            q.validate_job(bad)
            for seed in (-1, True, 2 ** 64, 1.5):
                bad = copy.deepcopy(mode); bad["corpus_config"]["seed"] = seed
                with self.assertRaisesRegex(ValueError, "corpus configuration"):
                    q.validate_job(bad)
        for mode, field in ((setup, "layout_file"), (corpus, "corpus_file")):
            for path in ("relative", "/srv/../foreign", "/srv//not-clean", mode["output"]):
                bad = copy.deepcopy(mode); bad[field] = path
                with self.assertRaises(ValueError):
                    q.validate_job(bad)
        with self.assertRaisesRegex(ValueError, "exact mode"):
            q.validate_job(dict(corpus, cluster_file="/srv/cluster"))
        wrong = self.job("go_pure"); wrong["client_library"] = "/srv/libfdb_c.so"
        with self.assertRaisesRegex(ValueError, "pure Go"):
            q.validate_job(wrong)
        wrong = self.job("rust_native"); wrong["client_library"] = None
        with self.assertRaises(ValueError):
            q.validate_job(wrong)

    def test_report_envelope_preserves_raw_rust_identity_and_unknown_telemetry(self):
        job = self.job(); raw = r.fake_ingest(job); before = copy.deepcopy(raw)
        value = q.envelope(job, raw)
        self.assertTrue(value["accepted"])
        self.assertFalse(value["rate_eligible"])
        self.assertEqual(value["raw_report"], before)
        self.assertEqual(raw, before)
        self.assertTrue(all(x == {"availability": "unavailable"} for x in value["telemetry"].values()))
        raw["telemetry"] = {"process_cpu_ns": 0}
        self.assertEqual(q.envelope(job, raw)["telemetry"]["process_cpu_ns"], {"availability": "available", "value": 0})

    def test_each_incomplete_unsettled_or_forged_report_is_preserved_and_refused(self):
        job = self.job()
        changes = (("schema", "otelbench-v2"), ("arm", "go_native"), ("status", "error"),
                   ("frames", 0), ("accepted_spans", True), ("acked_span_operations", 0),
                   ("committed_batches", 0), ("queued_spans_at_exit", 1), ("failed_spans", 1),
                   ("rejected_spans", 1), ("samples", []), ("job_sha256", "a" * 64),
                   ("effective_job", {}), ("source_revision", "b" * 40), ("telemetry", {"allocations": -1}))
        for key, bad in changes:
            raw = r.fake_ingest(job); raw[key] = bad
            with self.subTest(key=key):
                value = q.envelope(job, raw)
                self.assertFalse(value["accepted"])
                self.assertEqual(value["raw_report"], raw)
        raw = r.fake_ingest(job); raw["samples"][0]["error"] = "uncertain"
        self.assertFalse(q.envelope(job, raw)["accepted"])

    def test_ack_availability_is_explicit_go_unavailable_and_rust_required(self):
        missing = {"availability": "unavailable", "reason": "Go collector does not expose write operations including replays"}
        for arm in self.plan["arms"]:
            job = self.job(arm); raw = r.fake_ingest(job)
            self.assertTrue(q.envelope(job, raw)["accepted"])
            raw["acked_span_operations"] = missing
            value = q.envelope(job, raw)
            self.assertEqual(value["accepted"], arm != "rust_native")
            if arm != "rust_native":
                self.assertEqual(value["acked_span_operations"], missing)
                proof = r.fake_verify(job, control_identity=q.verifier_identity(self.plan))
                joined = q.joined_envelope(job, raw, proof, "fixture", self.plan)
                self.assertIs(type(joined["distinct_spans"]), int)
                self.assertEqual(joined["distinct_spans"], job["expected_frames"])
                self.assertEqual(joined["acked_span_operations"], missing)
        job = self.job("go_pure")
        raw = r.fake_ingest(job); raw["spans_per_second"] = None
        self.assertTrue(q.envelope(job, raw)["accepted"])
        for key, rate in (("spans_per_second", 100000), ("rate_numerator", "acked_span_operations"),
                          ("cpu_ms_per_distinct_span", 0.01), ("cpu_denominator", "acked_span_operations")):
            raw = r.fake_ingest(job); raw[key] = rate
            self.assertFalse(q.envelope(job, raw)["accepted"])
        for bad in (None, 100000, {"availability": "available"}, {"availability": "available", "value": True},
                    {"availability": "available", "value": -1}, {"availability": "available", "value": 100000.0},
                    {"availability": "unavailable"}, {"availability": "unavailable", "reason": " "},
                    {"availability": "unavailable", "reason": "unknown", "value": 0}):
            raw = r.fake_ingest(job); raw["acked_span_operations"] = bad
            with self.subTest(bad=bad):
                self.assertFalse(q.envelope(job, raw)["accepted"])

    def test_direct_ack_samples_and_collector_admission_samples_have_distinct_names(self):
        direct = self.job(); raw = r.fake_ingest(direct)
        self.assertIn("direct_write_acked_operations", raw["samples"][0])
        self.assertNotIn("acked_span_operations", raw["samples"][0])
        self.assertTrue(q.envelope(direct, raw)["accepted"])
        for sample in raw["samples"]:
            del sample["direct_write_acked_operations"]
        self.assertTrue(q.envelope(direct, raw)["accepted"])
        for bad in (None, True, 1.0):
            raw["samples"][0]["direct_write_acked_operations"] = bad
            self.assertFalse(q.envelope(direct, raw)["accepted"])
        queued = copy.deepcopy(direct)
        queued["workload"] = q.fake_plan(self.root / "sample-queue", kind="collector_queue")["workload"]
        raw = r.fake_ingest(queued)
        self.assertTrue(q.envelope(queued, raw)["accepted"])
        self.assertEqual(set(raw["samples"][0]), {"ns", "received_spans", "accepted_spans", "error"})
        for name in ("acked_span_operations", "direct_write_acked_operations"):
            wrong = copy.deepcopy(raw); wrong["samples"][0][name] = 0
            self.assertFalse(q.envelope(queued, wrong)["accepted"])
        definitions = d.read(Path(q.__file__).with_name("paired-job-v1.schema.json"))["$defs"]
        self.assertNotIn("acked_span_operations", definitions["writer_direct_sample"]["properties"])
        self.assertNotIn("direct_write_acked_operations", definitions["collector_admission_sample"]["properties"])
        self.assertNotIn("direct_write_acked_operations", definitions["writer_direct_sample"]["required"])
        self.assertEqual(definitions["verified_result"]["properties"]["distinct_spans"], {"type": "integer", "minimum": 1})

    def test_joined_and_wave_rates_and_cpu_use_distinct_rows_not_replayed_operations(self):
        self.plan = q.fake_plan(self.root / "rates-queue", kind="collector_queue")
        self.assets = self.backend.remote_assets(self.plan)
        joined = []
        for client in (1, 2):
            ns = q.namespace(self.plan, 2, "rust_native", "prerequisite", client)
            job = q.rust_job(self.plan, "rust_native", 2, "prerequisite", client, self.layout, self.cluster, self.assets,
                             "/srv/otel-paired/" + ns + "/ingest.json")
            raw = r.fake_ingest(job)
            raw["acked_span_operations"]["value"] = 2 * job["expected_frames"]
            raw["ingest_resources"] = {"scope": "first_read_through_flush", "user_cpu_seconds": 1.5,
                                       "system_cpu_seconds": 0.5, "cpu_seconds": 2.0,
                                       "peak_rss_bytes": 4096, "measurement_source": "fake phase snapshot"}
            value = q.joined_envelope(job, raw, r.fake_verify(job, control_identity=q.verifier_identity(self.plan)), ns, self.plan)
            self.assertEqual(value["spans_per_second"], 100000)
            self.assertEqual(value["cpu_ms_per_distinct_span"], {"availability": "available", "value": 0.02})
            self.assertEqual(value["acked_span_operations"]["value"], 200000)
            joined.append(value)
        for key, bad in (("rate_numerator", "acked_span_operations"), ("spans_per_second", 200000),
                         ("cpu_denominator", "acked_span_operations"), ("distinct_spans", 200000),
                         ("cpu_ms_per_distinct_span", {"availability": "available", "value": 0.01})):
            wrong = copy.deepcopy(joined[0]); wrong[key] = bad
            with self.subTest(key=key), self.assertRaises(ValueError):
                r.validate_distinct_result(wrong)
        metrics = r.wave_metrics(joined, 1000000000, 2000000000, 3000000000)
        self.assertEqual(metrics["distinct_spans"], 200000)
        self.assertEqual(metrics["acked_span_operations"]["value"], 400000)
        self.assertEqual(metrics["ack_rows_per_second"], 200000)
        self.assertEqual(metrics["drained_rows_per_second"], 100000)
        self.assertEqual(metrics["cpu_ms_per_distinct_span"]["value"], 0.02)
        for key, bad in (("rate_numerator", "acked_span_operations"), ("rows", 400000),
                         ("ack_rows_per_second", 400000), ("drained_rows_per_second", 200000),
                         ("cpu_ms_per_distinct_span", {"availability": "available", "value": 0.01})):
            wrong = copy.deepcopy(metrics); wrong[key] = bad
            with self.subTest(key=key), self.assertRaises(ValueError):
                r.validate_wave_metrics(wrong, joined)
        with self.assertRaisesRegex(ValueError, "distinct producer"):
            r.wave_metrics([joined[0], joined[0]], 1, 2, 3)

    def test_unknown_go_operations_remain_unknown_but_matching_cpu_is_required(self):
        job = self.job("go_native"); raw = r.fake_ingest(job)
        raw["process_resources"] = {"scope": "argv_to_exit", "cpu_seconds": 100}
        proof = r.fake_verify(job, control_identity=q.verifier_identity(self.plan))
        joined = q.joined_envelope(job, raw, proof, "fixture", self.plan)
        self.assertEqual(joined["distinct_spans"], 100000)
        self.assertEqual(joined["acked_span_operations"]["availability"], "unavailable")
        self.assertEqual(joined["cpu_ms_per_distinct_span"], {"availability": "available", "value": 0.02})
        metrics = r.wave_metrics([joined], 1, 1000000001, 2000000001)
        self.assertEqual(metrics["acked_span_operations"]["availability"], "unavailable")
        self.assertNotIn("value", metrics["acked_span_operations"])
        self.assertEqual(metrics["cpu_ms_per_distinct_span"]["value"], 0.02)
        for bad in (None, {"scope": "argv_to_exit", "cpu_seconds": 100},
                    {"availability": "unavailable", "reason": "not measured"}):
            wrong = copy.deepcopy(raw); wrong["ingest_resources"] = bad
            self.assertFalse(q.envelope(job, wrong)["accepted"])
            with self.assertRaises(ValueError):
                q.joined_envelope(job, wrong, proof, "fixture", self.plan)
        del raw["ingest_resources"]
        self.assertFalse(q.envelope(job, raw)["accepted"])

    def test_counter_capability_matrix_refuses_invented_go_values(self):
        self.plan = q.fake_plan(self.root / "counter-queue", kind="collector_queue")
        self.assets = self.backend.remote_assets(self.plan)
        for arm in self.plan["arms"]:
            job = self.job(arm); baseline = r.fake_ingest(job)
            self.assertTrue(q.envelope(job, baseline)["accepted"])
            for key in r.COUNTERS:
                raw = copy.deepcopy(baseline); del raw[key]
                with self.subTest(arm=arm, missing=key):
                    self.assertFalse(q.envelope(job, raw)["accepted"])
                for bad in (0, None, {"availability": "unavailable", "reason": " "}):
                    raw = copy.deepcopy(baseline); raw[key] = bad
                    self.assertFalse(q.envelope(job, raw)["accepted"])
                if arm != "rust_native" and key in r.GO_UNOBSERVABLE:
                    self.assertEqual(baseline[key]["availability"], "unavailable")
                    for count in (0, 100000):
                        raw = copy.deepcopy(baseline); raw[key] = {"availability": "available", "value": count}
                        with self.subTest(arm=arm, forbidden=key, count=count):
                            self.assertFalse(q.envelope(job, raw)["accepted"])
                elif arm == "rust_native" and key != "acked_span_operations":
                    raw = copy.deepcopy(baseline); raw[key] = r.unavailable("not exposed by this Rust instrumentation")
                    self.assertTrue(q.envelope(job, raw)["accepted"])
            if arm != "rust_native":
                for key in ("export_attempts", "queue_retries"):
                    raw = copy.deepcopy(baseline); raw[key] = r.unavailable("not measured")
                    self.assertFalse(q.envelope(job, raw)["accepted"])
        raw = r.fake_ingest(self.job("go_pure"))
        self.assertIn("not applicable", raw["native_retries"]["reason"])
        raw["native_retries"] = r.unavailable("not exposed by the Go collector")
        self.assertFalse(q.envelope(self.job("go_pure"), raw)["accepted"])

    def test_early_error_may_explain_absent_resources_but_never_qualifies_for_comparison(self):
        definitions = d.read(Path(q.__file__).with_name("paired-job-v1.schema.json"))["$defs"]
        policy = definitions["ingest_resource_policy"]["oneOf"]
        self.assertEqual(policy[0]["properties"]["ingest_resources"], {"$ref": "#/$defs/ingest_resources"})
        self.assertEqual(policy[0]["not"], {"required": ["ingest_resources_unavailable_reason"]})
        self.assertEqual(policy[1]["properties"]["status"], {"const": "error"})
        self.assertEqual(policy[1]["properties"]["ingest_resources"], {"type": "null"})
        self.assertEqual(set(policy[1]["required"]), {"status", "error", "ingest_resources", "ingest_resources_unavailable_reason"})
        self.assertIn({"$ref": "#/$defs/ingest_resource_policy"}, definitions["ingest_ack_and_samples"]["allOf"])
        self.assertEqual(definitions["verified_result"]["properties"]["raw_report"]["allOf"][1]["properties"]["status"], {"const": "ok"})
        for arm in self.plan["arms"]:
            job = self.job(arm); raw = early_error(job)
            self.assertIsNone(r.ingest_resource_evidence(raw))
            value = q.envelope(job, raw)
            self.assertFalse(value["accepted"])
            self.assertFalse(value["rate_eligible"])
            self.assertEqual(value["raw_report"], raw)
            with self.assertRaises(ValueError):
                q.joined_envelope(job, raw, r.fake_verify(job), "fixture", self.plan)
            with self.assertRaises(ValueError):
                r.ingest_cpu_seconds(raw)
            for key in ("status", "error", "ingest_resources", "ingest_resources_unavailable_reason"):
                wrong = copy.deepcopy(raw); del wrong[key]
                with self.subTest(arm=arm, missing=key), self.assertRaises(ValueError):
                    r.ingest_resource_evidence(wrong)
            for key, bad in (("status", "ok"), ("error", None), ("error", " "),
                             ("ingest_resources_unavailable_reason", ""), ("ingest_resources_unavailable_reason", " "),
                             ("ingest_resources_unavailable_reason", 0)):
                wrong = copy.deepcopy(raw); wrong[key] = bad
                with self.subTest(arm=arm, key=key, bad=bad), self.assertRaises(ValueError):
                    r.ingest_resource_evidence(wrong)
        job = self.job(); raw = r.fake_ingest(job)
        raw["ingest_resources_unavailable_reason"] = "contradictory absence claim"
        self.assertFalse(q.envelope(job, raw)["accepted"])
        for bad in (float("inf"), float("nan"), -1):
            raw = r.fake_ingest(job); raw["ingest_resources"]["user_cpu_seconds"] = bad
            self.assertFalse(q.envelope(job, raw)["accepted"])

    def test_required_plain_source_timing_and_process_resources_cannot_be_omitted(self):
        job = self.job(); baseline = r.fake_ingest(job)
        for key in ("frames", "received_spans", "accepted_spans", "rejected_spans", "failed_spans", "queued_spans_at_exit",
                    "ingest_wall_ns", "admission_phase_ns", "drain_ns", "samples", "error", "ingest_resources"):
            raw = copy.deepcopy(baseline); del raw[key]
            with self.subTest(missing=key):
                self.assertFalse(q.envelope(job, raw)["accepted"])
        for key in baseline["ingest_resources"]:
            raw = copy.deepcopy(baseline); del raw["ingest_resources"][key]
            with self.subTest(missing_resource=key):
                self.assertFalse(q.envelope(job, raw)["accepted"])
        for key, bad in (("received_spans", 99999), ("cpu_seconds", 3), ("peak_rss_bytes", 0)):
            raw = copy.deepcopy(baseline)
            (raw if key == "received_spans" else raw["ingest_resources"])[key] = bad
            self.assertFalse(q.envelope(job, raw)["accepted"])

    def test_independent_full_cross_read_all_indexes_and_oracle_are_mandatory(self):
        job = self.job(); q.verify_audit(r.fake_verify(job), job, "fixture")
        for field, bad in (("source_records_exact_match", 32), ("full_rows_equal", False),
                           ("distinct_spans", 0), ("layout_seals_equal", 1)):
            value = r.fake_verify(job); value["report"][field] = bad
            with self.subTest(field=field), self.assertRaises(ValueError):
                q.verify_audit(value, job, "fixture")
        for mutator in (lambda x: x.update(queries=[]),
                        lambda x: x["report"]["oracle"].update(status="not_requested"),
                        lambda x: x["report"]["oracle"].update(passed_queries=1599),
                        lambda x: x["report"]["oracle"].update(results_sha256="d" * 64),
                        lambda x: x["report"]["census"]["actual"]["index_entries"].pop("SPANS_BOOL_ATTR"),
                        lambda x: x["queries"][0].update(actual_results=[{"foreign": True}])):
            value = r.fake_verify(job); mutator(value)
            with self.assertRaises(ValueError):
                q.verify_audit(value, job, "fixture")


    def test_oracle_rejects_empty_answers_duplicate_iterations_and_untyped_results(self):
        job = self.job()
        value = r.fake_verify(job)
        for sample in value["queries"]:
            sample["expected_results"] = []; sample["actual_results"] = []
        with self.assertRaisesRegex(ValueError, "nonempty"):
            q.verify_audit(value, job, "fixture")
        value = r.fake_verify(job)
        value["queries"][16]["iteration_in_class"] = value["queries"][0]["iteration_in_class"]
        with self.assertRaisesRegex(ValueError, "duplicate"):
            q.verify_audit(value, job, "fixture")
        value = r.fake_verify(job)
        value["queries"][0]["expected_results"] = ["not an object"]
        value["queries"][0]["actual_results"] = ["not an object"]
        with self.assertRaisesRegex(ValueError, "ordered results"):
            q.verify_audit(value, job, "fixture")


class ReceiptTests(Fixture):
    def test_large_receipts_preserve_every_raw_byte_with_private_deterministic_seals(self):
        import gzip
        import hashlib
        value = {"raw_report": {"samples": [{"ns": i, "spans": 1} for i in range(10000)]}}
        path = self.root / "archive.json"
        q.write_receipt(path, value, receipt_root=self.root)
        packed = Path(str(path) + ".gz")
        seal = d.read(str(path) + ".seal.json")
        raw = gzip.decompress(packed.read_bytes())
        self.assertEqual(json.loads(raw), value)
        self.assertEqual(seal["raw_sha256"], hashlib.sha256(raw).hexdigest())
        self.assertEqual(seal["sha256"], q.p.sha(packed))
        self.assertEqual(seal["raw_bytes"], len(raw))
        self.assertLess(seal["bytes"], seal["raw_bytes"])
        self.assertEqual(packed.stat().st_mode & 0o077, 0)
        with self.assertRaisesRegex(ValueError, "already exists"):
            q.write_receipt(path, {"smaller": "replacement"})
        second = self.root / "second.json"; q.write_receipt(second, value)
        self.assertEqual(Path(str(second) + ".gz").read_bytes(), packed.read_bytes())

    def test_receipt_archive_refuses_raw_and_shared_size_overflow_without_lossy_truncation(self):
        with mock.patch.object(q, "MAX_RAW_RECEIPT", 4):
            with self.assertRaisesRegex(ValueError, "raw receipt"):
                q.write_receipt(self.root / "too-big.json", {"field": "value"})
        huge = self.root / "sparse"
        with open(huge, "wb") as stream:
            stream.truncate(1024 ** 3)
        with self.assertRaisesRegex(ValueError, "1GiB"):
            q.write_receipt(self.root / "would-overflow.json", {"field": "value"}, receipt_root=self.root)
        self.assertFalse((self.root / "would-overflow.json").exists())


class CampaignTests(Fixture):
    def run_campaign(self, backend=None):
        return q.PairedCampaign(self.plan, self.root / "output", backend or self.backend).run()

    def test_complete_three_arm_scaling_sequence_offline_preserves_every_receipt(self):
        original = builtins.__import__
        def importing(name, *args, **kwargs):
            if name in ("provision", "paired_native"):
                raise AssertionError("dry-run imported live provider")
            return original(name, *args, **kwargs)
        with mock.patch.object(builtins, "__import__", side_effect=importing):
            result = self.run_campaign()
        self.assertEqual(result["status"], "FAKE_PASSED")
        self.assertEqual(result["authorized_rows"], 8550000)
        self.assertEqual(result["charged_rows"], 8550000)
        self.assertEqual(result["released_waves"], 18)
        self.assertEqual(len(result["arms"]), 9)
        self.assertFalse(result["accepted_comparison"])
        self.assertTrue(result["cleanup_passed"])
        self.assertEqual(self.backend.events.count("prepare"), 1)
        self.assertEqual(self.backend.events.count("release"), 18)
        self.assertEqual(self.backend.events[-1], "cleanup")
        self.assertEqual(len(list((self.root / "output").rglob("client*-result.json"))), 36)
        for path in (self.root / "output").rglob("*.json"):
            self.assertEqual(path.stat().st_mode & 0o077, 0)
        with self.assertRaises(FileExistsError):
            self.run_campaign()

    def test_dry_run_plan_never_debits_the_supplied_authoritative_ledger(self):
        original = self.plan["row_ledger"]
        path = self.root / "input-plan.json"; d.write(path, self.plan)
        output = self.root / "rehearsal"
        captured = []
        def fake_run(campaign):
            captured.append(campaign.plan)
            return {"status": "FAKE_PASSED", "fake_provider": True, "accepted_comparison": False,
                    "authorized_rows": 8550000, "charged_rows": 0, "held_host_count": 6,
                    "released_waves": 0, "cleanup_passed": True}
        with mock.patch.object(q.PairedCampaign, "run", autospec=True, side_effect=fake_run), mock.patch("sys.stdout", new=io.StringIO()):
            q.main(["--dry-run", "--plan", str(path), "--output", str(output)])
        self.assertEqual(len(captured), 1)
        self.assertNotEqual(captured[0]["row_ledger"], original)
        self.assertTrue(Path(captured[0]["row_ledger"]).is_file())
        self.assertEqual(q.rows.inspect(original)["issued_rows"], 0)
        self.assertEqual(d.read(path)["row_ledger"], original)
        live = self.root / "live-plan.json"; d.write(live, dict(self.plan, fake_provider=False))
        with self.assertRaisesRegex(ValueError, "refuses live"):
            q.main(["--dry-run", "--plan", str(live), "--output", str(self.root / "rejected")])
        self.assertFalse((self.root / "rejected-fake-ledger").exists())

    def test_failed_gate_stops_whole_campaign_and_preserves_issued_rows(self):
        old = self.backend.release_and_wait
        def failed(*args):
            value = old(*args); value[1]["failed_spans"] = 1; return value
        self.backend.release_and_wait = failed
        with self.assertRaises(ValueError):
            self.run_campaign()
        result = d.read(self.root / "output/paired-result.json")
        self.assertEqual(result["charged_rows"], 100000)
        self.assertEqual(result["released_waves"], 1)
        self.assertEqual(self.backend.events.count("release"), 1)
        self.assertTrue(result["cleanup_passed"])
        raw = list((self.root / "output").rglob("client1-result.json"))
        self.assertEqual(len(raw), 1)
        self.assertEqual(d.read(raw[0])["raw_report"]["failed_spans"], 1)

    def test_early_error_is_preserved_charged_and_stops_without_comparison(self):
        old = self.backend.release_and_wait
        def failed(plan, ready, jobs):
            value = old(plan, ready, jobs)
            value[1] = early_error(jobs[1])
            return value
        self.backend.release_and_wait = failed
        with self.assertRaises(ValueError):
            self.run_campaign()
        result = d.read(self.root / "output/paired-result.json")
        self.assertEqual(result["charged_rows"], 100000)
        self.assertEqual(result["released_waves"], 1)
        self.assertFalse(result["accepted_comparison"])
        self.assertTrue(result["cleanup_passed"])
        self.assertEqual(self.backend.events.count("release"), 1)
        self.assertNotIn("verify", self.backend.events)
        receipt = d.read(next((self.root / "output").rglob("client1-result.json")))
        self.assertFalse(receipt["accepted"])
        self.assertFalse(receipt["rate_eligible"])
        self.assertIsNone(receipt["raw_report"]["ingest_resources"])
        self.assertEqual(receipt["raw_report"]["ingest_resources_unavailable_reason"], "ingest window not started: corpus reopen failed")
        ledger = q.rows.inspect(self.plan["row_ledger"])
        self.assertEqual(ledger["issued_rows"], 100000)

    def test_no_report_or_schema_invalid_report_fails_charged_without_comparison(self):
        for missing in (True, False):
            with self.subTest(no_report=missing):
                label = "no-report" if missing else "invalid-report"
                plan = q.fake_plan(self.root / label)
                backend = q.FakeBackend(plan["first_epoch"])
                old = backend.release_and_wait
                def failed(*args):
                    value = old(*args)
                    if missing:
                        raise ValueError("producer/control process failed; collector unsettled; no schema report")
                    del value[1]["ingest_resources"]
                    return value
                backend.release_and_wait = failed
                output = self.root / (label + "-output")
                with self.assertRaises(ValueError):
                    q.PairedCampaign(plan, output, backend).run()
                result = d.read(output / "paired-result.json")
                self.assertEqual(result["charged_rows"], 100000)
                self.assertEqual(result["released_waves"], 1)
                self.assertFalse(result["accepted_comparison"])
                self.assertTrue(result["cleanup_passed"])
                self.assertEqual(backend.events.count("release"), 1)
                self.assertEqual(backend.events[-1], "cleanup")
                self.assertNotIn("verify", backend.events)
                self.assertFalse(list(output.rglob("wave-result.json")))
                ledger = q.rows.inspect(plan["row_ledger"])
                self.assertEqual(ledger["issued_rows"], 100000)
                self.assertEqual([row["status"] for row in ledger["outcomes"]], ["failed"])
                self.assertEqual(ledger["pending_issue_ids"], [])
                if not missing:
                    receipt = d.read(next(output.rglob("client1-result.json")))
                    self.assertFalse(receipt["accepted"])
                    self.assertFalse(receipt["rate_eligible"])
                    self.assertNotIn("ingest_resources", receipt["raw_report"])

    def test_wrong_ready_host_never_releases_and_cleanup_still_runs(self):
        old = self.backend.ready
        def wrong(*args):
            value = old(*args); value["node_id"] = 999; return value
        self.backend.ready = wrong
        with self.assertRaisesRegex(ValueError, "ready"):
            self.run_campaign()
        self.assertNotIn("release", self.backend.events)
        self.assertEqual(self.backend.events[-1], "cleanup")

    def test_missing_storage_drain_or_oracle_never_produces_comparison(self):
        self.backend.drain = lambda *args: []
        with self.assertRaisesRegex(ValueError, "drain"):
            self.run_campaign()
        self.assertTrue(d.read(self.root / "output/paired-result.json")["cleanup_passed"])

    def test_cleanup_failure_remains_failed_and_no_comparison(self):
        self.backend.ready = mock.Mock(side_effect=ValueError("abort before release"))
        self.backend.cleanup = lambda *args: {"passed": False, "exact_baseline": False}
        with self.assertRaisesRegex(ValueError, "abort"):
            self.run_campaign()
        result = d.read(self.root / "output/paired-result.json")
        self.assertFalse(result["cleanup_passed"])
        self.assertFalse(result["accepted_comparison"])

    def test_budget_overflow_refuses_before_any_creation(self):
        self.plan["prior_cost_upper_eur"] = "9"
        with self.assertRaisesRegex(ValueError, "EUR10"):
            self.run_campaign()
        self.assertNotIn("prepare", self.backend.events)
        self.assertNotIn("release", self.backend.events)

    def test_expired_shared_clock_has_no_new_admission(self):
        self.backend.epoch = self.plan["deadline_epoch"]
        with self.assertRaisesRegex(ValueError, "deadline"):
            self.run_campaign()
        self.assertNotIn("prepare", self.backend.events)


if __name__ == "__main__":
    unittest.main()
