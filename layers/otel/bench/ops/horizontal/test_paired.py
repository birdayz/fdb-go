#!/usr/bin/env python3
"""Fake-only paired orchestration, typed arm and nonvacuous report regressions."""
import builtins
import copy
import json
from pathlib import Path
import tempfile
import unittest
from unittest import mock

import driver as d
import paired as q
import paired_reports as r


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

    def test_paid_validation_replays_each_arm_preflight_oracle_before_effects(self):
        value = copy.deepcopy(self.plan)
        value["fake_provider"] = False
        source = value["sources"]["go_pure"]
        value.update(binary=value["assets"]["go_binary"]["path"], binary_sha256=value["assets"]["go_binary"]["sha256"],
                     source_root=source["root"], source_manifest=source["manifest"], input_manifest_sha256=source["manifest_sha256"],
                     source_revision=source["revision"], hcloud="/private/hcloud", controller_private_key="/private/key",
                     controller_public_key="/private/key.pub", controller_cidr="192.0.2.1/32")
        for name in ("fdbserver", "fdbcli"):
            value[name] = value["assets"][name]["path"]; value[name + "_sha256"] = value["assets"][name]["sha256"]
        proofs = {}
        for arm in value["arms"]:
            job = self.job(arm)
            files = {}
            for name, data in (("ingest_job", job), ("ingest_report", r.fake_ingest(job)),
                               ("verification", r.fake_verify(job, control_identity=q.control_identity(value)))):
                path = self.root / (arm + "-" + name + ".json"); d.write(path, data)
                files[name] = {"path": str(path), "sha256": q.p.sha(path)}
            proofs[arm] = files
        value["preflight_verifications"] = proofs
        with mock.patch.object(q, "NATIVE_SHA256", value["assets"]["libfdb_c"]["sha256"]):
            q.validate_plan(value)
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
            bad = copy.deepcopy(value); bad["binary_sha256"] = "d" * 64
            with self.assertRaisesRegex(ValueError, "aliases"):
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
            self.assertEqual(argv[0], self.assets["rust_binary" if arm == "rust_native" else "go_binary"])
            self.assertFalse(any(x.startswith("--") for x in argv))
            self.assertEqual(job["mode"], "ingest")
            self.assertEqual(job["writer_workers"], 1)
            self.assertIs(job["bulk_index_conflicts"], False)
            self.assertIsNone(job["client_library"] if arm == "go_pure" else None)
            if arm != "go_pure":
                self.assertEqual(job["client_library"], self.assets["libfdb_c"])
            q.envelope(job, r.fake_ingest(job))
        with self.assertRaises(ValueError):
            q.rust_job(self.plan, "go_native")
        with self.assertRaises(ValueError):
            q.go_job(self.plan, "rust_native")
        path = self.root / "job.json"; d.write(path, self.job())
        self.assertEqual(q.job_seal(self.job()), q.p.sha(path))

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
