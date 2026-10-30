#!/usr/bin/env python3
"""Offline regressions for the bounded two-stage 2GiB-log follow-up."""
from contextlib import contextmanager
import copy
import tempfile
import unittest
from unittest import mock

import diagnostic as n
import driver as d
import policy as p
import scaling as s
import test_scaling as old


STAGES = [[2, 2], [1, 1]]
PRIOR_ROWS = 8425000
AUTHORIZED_ROWS = 1425000


def plan(lease=None):
    value = copy.deepcopy(old.plan() if lease is None else lease)
    value.pop("managed_watch", None)
    value.update(log_memory_gib=2, foreground_only=True, stages=copy.deepcopy(STAGES), prior_rows_upper=PRIOR_ROWS)
    return value


def measured_records():
    return [{"hosts": hosts, "producers": hosts, "status": "PASS", "log_memory_gib": 2,
             **old.stage_result(hosts)} for hosts in (2, 1)]


class FollowupHarness(old.Harness):
    def __init__(self, root, *, outcomes=(None, None), consumption=(11, 17)):
        super().__init__(root, outcomes=outcomes, consumption=consumption)
        self.plan = plan(self.plan)
        self.completed = [old.stage_result(2), old.stage_result(1)]
        self.session = s.ScalingSession(self.plan, self.out, self.provider, arm_factory=self.arm_factory)

    def arm_factory(self, lease, out, provider, **kwargs):
        arm = super().arm_factory(lease, out, provider, **kwargs)
        self.completed[arm.index] = old.stage_result(arm.hosts)
        run = arm.run_existing
        def run_existing(resources, *, helpers_ready=False):
            arm.entry_remaining_waves = arm.remaining_waves
            try:
                return run(resources, helpers_ready=helpers_ready)
            finally:
                for layout in arm.layouts:
                    layout["log_memory_gib"] = 2
        arm.run_existing = run_existing
        return arm


class FollowupPlanTests(unittest.TestCase):
    def test_exact_followup_has_two_then_one_pairs_and_fixed_cumulative_population(self):
        value = plan(); before = copy.deepcopy(value)
        self.assertIs(s.validate_plan(value, check_assets=False), value)
        self.assertEqual(p.scaling_stage_pairs(value), [[2, 2], [1, 1]])
        self.assertEqual(p.scaling_authorized_rows(value), 1425000)
        self.assertEqual(value["prior_rows_upper"] + p.scaling_authorized_rows(value), 9850000)
        self.assertEqual(value, before)
        self.assertIs(value["foreground_only"], True)
        self.assertNotIn("managed_watch", value)
        self.assertEqual(value["deadline_epoch"]-value["first_epoch"], 7200)

    def test_default_study_is_unchanged_without_the_optional_log_memory_field(self):
        value = old.plan(); before = copy.deepcopy(value)
        self.assertIs(s.validate_plan(value, check_assets=False), value)
        self.assertEqual(p.scaling_stage_pairs(value), [[1, 1], [2, 2], [3, 3]])
        self.assertEqual(p.scaling_authorized_rows(value), 2850000)
        self.assertEqual(s.STAGES, [[1, 1], [2, 2], [3, 3]])
        self.assertEqual(s.AUTHORIZED_ROWS, 2850000)
        self.assertEqual(value, before)
        self.assertNotIn("log_memory_gib", value)

    def test_log_memory_flag_is_only_exact_integer_two_and_never_a_generic_memory_sweep(self):
        for bad in (None, True, False, 0, 1, 3, 2.0, "2", [], {}):
            value = plan(); value["log_memory_gib"] = bad
            with self.subTest(log_memory_gib=bad), self.assertRaises(ValueError):
                s.validate_plan(value, check_assets=False)
        value = plan(); del value["log_memory_gib"]
        with self.assertRaises(ValueError):
            s.validate_plan(value, check_assets=False)
        value = old.plan(); value["log_memory_gib"] = 2
        with self.assertRaises(ValueError):
            s.validate_plan(value, check_assets=False)

    def test_followup_cannot_reorder_expand_or_shrink_its_exact_typed_stage_population(self):
        for stages in (None, [], [[1, 1], [2, 2]], [[2, 2]], [[1, 1]], [[2, 2], [2, 2]],
                       [[2, 2], [1, 1], [3, 3]], [[2, 1], [1, 1]], [(2, 2), (1, 1)],
                       [[2.0, 2], [1, 1]], [[2, 2], [True, 1]], [[2, 2], [1, True]]):
            value = plan(); value["stages"] = stages
            with self.subTest(stages=stages), self.assertRaises(ValueError):
                s.validate_plan(value, check_assets=False)

    def test_fixed_prior_rows_and_absolute_cumulative_ceiling_are_not_resettable(self):
        for prior in (None, True, False, -1, 0, 7000000, 8424999, 8425001, 8575000, 8575001,
                      8425000.0, "8425000"):
            value = plan(); value["prior_rows_upper"] = prior
            with self.subTest(prior=prior), self.assertRaises(ValueError):
                s.validate_plan(value, check_assets=False)
        value = plan(); del value["prior_rows_upper"]
        with self.assertRaises(ValueError):
            s.validate_plan(value, check_assets=False)
        for key, bad in (("count", 0), ("measured_count", 1000000), ("measured_seconds", 121),
                         ("deadline_epoch", 8201), ("storage_process_count", 2), ("storage_max_outstanding", 128)):
            value = plan(); value[key] = bad
            with self.subTest(key=key, bad=bad), self.assertRaises(ValueError):
                s.validate_plan(value, check_assets=False)

    def test_followup_requires_foreground_true_and_never_imports_managed_authorization(self):
        for foreground in (None, False, 0, 1, "true", True):
            value = plan(); value["foreground_only"] = foreground
            if foreground is True:
                value["managed_watch"] = {"root": "/private/native-test/watch", "output": "/private/native-test/run"}
            with self.subTest(foreground_only=foreground), self.assertRaises(ValueError):
                s.validate_plan(value, check_assets=False)
        value = plan(); del value["foreground_only"]
        with self.assertRaises(ValueError):
            s.validate_plan(value, check_assets=False)


class FollowupSessionTests(unittest.TestCase):
    def setUp(self):
        for name in ("run", "Popen"):
            guard = mock.patch.object(n.subprocess, name, side_effect=AssertionError("real subprocess forbidden"))
            guard.start(); self.addCleanup(guard.stop)

    @contextmanager
    def running(self, h):
        with mock.patch.object(p, "verify_source", side_effect=h.verify_source), \
             mock.patch.object(d, "remaining", return_value=30), \
             mock.patch.object(s.supervisor, "register", side_effect=AssertionError("managed registration forbidden")) as register, \
             mock.patch.object(s.supervisor, "admit", side_effect=AssertionError("managed admission forbidden")) as admit:
            yield
        register.assert_not_called(); admit.assert_not_called()

    def assert_finalized(self, h):
        h.provider.prepare.assert_called_once_with(h.plan, h.out/"provision")
        h.provider.teardown.assert_called_once_with(h.plan, h.out/"provision", h.resources)
        self.assertEqual([event[0] for event in h.events[-2:]], ["cleanup", "source"])
        self.assertEqual(d.read(h.out/"source-after.json")["source_files"], 1)
        self.assertEqual(d.read(h.out/"approved-plan.json"), h.plan)

    def test_six_nodes_are_prepared_once_reused_in_two_then_one_order_and_cleaned_once(self):
        with tempfile.TemporaryDirectory() as tmp:
            h = FollowupHarness(tmp); before = copy.deepcopy(h.plan)
            self.assertEqual(h.session.remaining_waves, 4)
            with self.running(h):
                result = h.session.run()
            self.assert_finalized(h)
            self.assertEqual(h.plan, before)
            self.assertEqual(result["authorized_rows"], 1425000)
            self.assertEqual(result["cumulative_rows_upper"], 9850000)
            self.assertEqual(result["held_host_count"], 6)
            self.assertEqual(result["log_memory_gib"], 2)
            self.assertEqual(result["tested_hosts"], [1, 2])
            self.assertEqual(result["execution_mode"], "bounded_foreground")
            self.assertIs(result["foreground_only"], True)
            self.assertIs(result["all_stages_passed"], True)
            self.assertEqual([(record["hosts"], record["producers"], record["status"]) for record in result["stages"]],
                             [(2, 2, "PASS"), (1, 1, "PASS")])
            self.assertEqual([event for event in h.events if event[0] == "helpers"], [("helpers", 2, False), ("helpers", 1, True)])
            self.assertEqual(h.provider.stop_owned_arm.call_count, 2)
            self.assertEqual(len(h.arms), 2)
            for arm in h.arms:
                self.assertIs(arm.nodes, h.resources["nodes"])
                self.assertEqual(len(arm.nodes), 6)
                self.assertEqual(arm.plan["log_memory_gib"], 2)
            self.assertEqual(result["scaling"]["tested_hosts"], [1, 2])
            self.assertEqual([record["hosts"] for record in result["scaling"]["stages"]], [1, 2])
            self.assertEqual(d.read(h.out/"scaling-result.json"), result)

    def test_only_four_wave_reservations_share_trace_budget_and_history_without_reset(self):
        with tempfile.TemporaryDirectory() as tmp:
            h = FollowupHarness(tmp, consumption=(11, 17))
            with self.running(h):
                result = h.session.run()
            first, second = h.arms
            self.assertEqual((first.entry_remaining_waves, second.entry_remaining_waves), (4, 2))
            self.assertEqual((first.entry_budget, second.entry_budget), (16*1024**2, 16*1024**2-11))
            self.assertEqual(s.wave_budget(first.entry_budget, first.entry_remaining_waves), 4194304)
            self.assertEqual(s.wave_budget(second.entry_budget, second.entry_remaining_waves), (16*1024**2-11)//2)
            self.assertIs(first.observation_history, second.observation_history)
            self.assertEqual(set(second.entry_history), {"stage-2"})
            self.assertEqual(result["trace_bytes_remaining"], 16*1024**2-28)
            self.assertEqual(h.session.remaining_waves, 0)

    def test_native_oom_or_unknown_journal_fault_is_session_fatal_and_never_reuses_the_lease(self):
        for reason in ("OutOfMemory monitor process 4504 exit 20", "owned native journal UNKNOWN",
                       "native process RSS exceeded memory budget"):
            with self.subTest(reason=reason), tempfile.TemporaryDirectory() as tmp:
                h = FollowupHarness(tmp); h.host_error = ValueError(reason)
                with self.running(h), mock.patch.object(s, "classify_stage_failure", return_value=True) as classify:
                    with self.assertRaisesRegex(ValueError, reason):
                        h.session.run()
                self.assert_finalized(h)
                classify.assert_not_called()
                self.assertEqual(len(h.arms), 1)
                h.provider.stop_owned_arm.assert_not_called()
                result = d.read(h.out/"scaling-result.json")
                self.assertEqual(result["status"], "SESSION_FATAL")
                self.assertEqual(result["stages"][0]["status"], "SESSION_FATAL")
                self.assertIs(result["all_stages_passed"], False)
                self.assertEqual(result["scaling"]["verdict"], "INCONCLUSIVE")


class FollowupSummaryTests(unittest.TestCase):
    def test_run_order_is_not_baseline_identity_and_two_pair_efficiency_uses_drained_rate(self):
        values = measured_records(); before = copy.deepcopy(values)
        for records in (values, list(reversed(values))):
            result = s.summarize_scaling(records, expected_hosts=(1, 2))
            self.assertEqual(result["verdict"], "NEAR_LINEAR")
            self.assertEqual(result["tested_hosts"], [1, 2])
            self.assertEqual([(row["hosts"], row["scale_factor"], row["efficiency"]) for row in result["stages"]],
                             [(1, 1, 1), (2, 2, 1)])
            self.assertEqual(result["primary_metric"], "drained_rows_per_second")
        self.assertEqual(values, before)
        values[0]["measured"]["drained_rows_per_second"] = 1800
        values[0]["measured"]["ack_rows_per_second"] = 1800
        self.assertEqual(s.summarize_scaling(values, expected_hosts=(1, 2))["verdict"], "NEAR_LINEAR")
        values[0]["measured"]["drained_rows_per_second"] = 1799
        values[0]["measured"]["ack_rows_per_second"] = 1000000
        result = s.summarize_scaling(values, expected_hosts=(1, 2))
        self.assertEqual(result["verdict"], "BELOW_NEAR_LINEAR")
        self.assertIs(result["near_linear"], False)
        self.assertAlmostEqual(result["stages"][1]["efficiency"], 1799/2000)

    def test_fresh_one_x_baseline_must_have_the_same_exact_two_gib_log_profile(self):
        for bad in (None, 1, True, False, 2.0, "2"):
            records = measured_records()
            if bad is None:
                del records[1]["log_memory_gib"]
            else:
                records[1]["log_memory_gib"] = bad
            with self.subTest(baseline_log_memory_gib=bad):
                result = s.summarize_scaling(records, expected_hosts=(1, 2))
                self.assertEqual(result["verdict"], "INCONCLUSIVE")
                self.assertIs(result["near_linear"], False)
                self.assertTrue(result["reasons"])

    def test_no_historical_one_x_fallback_no_duplicates_and_no_untested_three_x_claim(self):
        values = measured_records()
        for records in ([], values[:1], values[1:], [values[0], values[0]], values+[old.records()[2]]):
            with self.subTest(hosts=[record["hosts"] for record in records]):
                result = s.summarize_scaling(records, expected_hosts=(1, 2))
                self.assertEqual(result["verdict"], "INCONCLUSIVE")
                self.assertIs(result["near_linear"], False)
                self.assertTrue(result["reasons"])
        for key, bad in (("status", "ARM_FAILURE"), ("status", "INCONCLUSIVE"), ("status", "SESSION_FATAL")):
            records = measured_records(); records[1][key] = bad
            self.assertEqual(s.summarize_scaling(records, expected_hosts=(1, 2))["verdict"], "INCONCLUSIVE")
        for key, bad in (("rows", 0), ("rows", True), ("measurement_eligible", False),
                         ("healthy", False), ("census_pass", False), ("oracle_pass", False),
                         ("drained_rows_per_second", 0), ("drained_rows_per_second", True)):
            records = measured_records(); records[1]["measured"][key] = bad
            with self.subTest(baseline_field=key, bad=bad):
                self.assertEqual(s.summarize_scaling(records, expected_hosts=(1, 2))["verdict"], "INCONCLUSIVE")
        result = s.summarize_scaling(measured_records(), expected_hosts=(1, 2))
        self.assertNotIn(3, [row["hosts"] for row in result["stages"]])
        self.assertEqual(s.summarize_scaling(measured_records())["verdict"], "INCONCLUSIVE")
        for expected in ([1, 2], (2, 1), (True, 2), (1.0, 2), (1, 2, 2), (1,), None):
            with self.subTest(expected_hosts=expected):
                result = s.summarize_scaling(measured_records(), expected_hosts=expected)
                self.assertEqual(result["verdict"], "INCONCLUSIVE")
                self.assertTrue(result["reasons"])


class FollowupWaveTests(unittest.TestCase):
    def setUp(self):
        for name in ("run", "Popen"):
            guard = mock.patch.object(n.subprocess, name, side_effect=AssertionError("real subprocess forbidden"))
            guard.start(); self.addCleanup(guard.stop)

    def test_real_wave_carries_log_profile_into_every_sampler_and_uses_four_wave_reservation(self):
        with tempfile.TemporaryDirectory() as tmp:
            h = old.WaveHarness(tmp, 2)
            h.arm.plan = plan(h.arm.plan)
            h.arm.remaining_waves = 4
            h.layout["log_memory_gib"] = 2
            for spec in h.layout["processes"]:
                if spec["port"] == 4504:
                    spec["memory_limit_bytes"] = 2*p.GIB
                    spec["args"][spec["args"].index("--memory")+1] = "2GiB"
            with h.running():
                result = h.arm.wave(h.layout, 375000, "measured")
            self.assertIs(result["measurement_eligible"], True)
            self.assertEqual(result["forensic_allocation"], 4194304)
            self.assertEqual(h.arm.remaining_waves, 3)
            self.assertEqual(set(h.sampler_plans), {"node1", "node2", "client1", "client2"})
            for role, (grant, _) in h.sampler_plans.items():
                self.assertEqual(grant["log_memory_gib"], 2)
                self.assertEqual(grant["scaling_hosts"], 2)
                for spec in grant["processes"]:
                    expected = "2GiB" if spec["port"] == 4504 else "3GiB" if spec["port"] >= 4510 else "1GiB"
                    self.assertEqual(spec["args"][spec["args"].index("--memory")+1], expected)


class FollowupFailureClassifierTests(unittest.TestCase):
    def classify(self, document, layout, wave, error=None):
        return s.classify_stage_failure(ValueError("strict client health") if error is None else error,
                                        document, layout, wave, fatal_guards_passed=True)

    def test_actual_native_oom_error_sampler_death_or_trace_fault_never_becomes_arm_failure(self):
        document, layout, wave = old.scaling_failure(2)
        layout["log_memory_gib"] = 2
        for spec in layout["processes"]:
            if spec["port"] == 4504:
                spec["memory_limit_bytes"] = 2*p.GIB
                spec["args"][spec["args"].index("--memory")+1] = "2GiB"
                document["cluster"]["processes"]["node1-4504"]["memory"]["limit_bytes"] = 2*p.GIB
        before = copy.deepcopy((document, layout, wave))
        for error in (ValueError("OutOfMemory monitor process 4504 exit 20"),
                      ValueError("native process RSS exceeded memory budget"),
                      OSError("strict client health")):
            self.assertIs(self.classify(document, layout, wave, error), False)
        changed = copy.deepcopy(document)
        changed["cluster"]["processes"]["node1-4504"]["messages"] = [{"name": "OutOfMemory", "description": "monitor exit 20"}]
        self.assertIs(self.classify(changed, layout, wave), False)
        changed = copy.deepcopy(wave); changed["error"] = "native PID/starttime changed"
        self.assertIs(self.classify(document, layout, changed), False)
        changed = copy.deepcopy(wave); changed["sampler_exits"]["node1"] = 137
        self.assertIs(self.classify(document, layout, changed), False)
        changed = copy.deepcopy(wave); changed["collection_errors"] = ["native trace capture failed"]
        self.assertIs(self.classify(document, layout, changed), False)
        self.assertEqual((document, layout, wave), before)


if __name__ == "__main__":
    unittest.main()
