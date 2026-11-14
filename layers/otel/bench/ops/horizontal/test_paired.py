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
