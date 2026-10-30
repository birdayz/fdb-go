#!/usr/bin/env python3
"""Shared-lease session boundaries with fake providers and no host effects."""
import builtins
from contextlib import contextmanager
import copy
import json
from pathlib import Path
import tempfile
import unittest
from unittest import mock

import diagnostic as n
import driver as d
import policy as p
import session as s
from test_diagnostic import plan as diagnostic_plan
from test_horizontal import status_fixture


def plan():
    value = diagnostic_plan()
    value.update(schema="otel_native_session_v1", cases=["capacity16g", "baseline"],
                 prior_rows_upper=0, storage_process_count=2, storage_max_outstanding=64)
    return value


def completed_result():
    return {"diagnostic_only": True, "accepted_throughput": False,
            "prerequisite": {"count": 100000, "producers": 1, "healthy": True, "ingest_valid": True,
                             "census_pass": True, "oracle_pass": True},
            "probe": {"count": 1000000, "producers": 1, "healthy": True, "ingest_valid": True,
                      "census_pass": True, "oracle_pass": True}}


class SessionPlanTests(unittest.TestCase):
    def test_exact_two_cases_are_validated_without_mutating_lease(self):
        value = plan(); before = copy.deepcopy(value)
        self.assertIs(s.validate_plan(value, check_assets=False), value)
        self.assertEqual(value, before)
        self.assertEqual(s.CASES, ("capacity16g", "baseline"))
        self.assertEqual(value["count"] + value["probe_count"], 1100000)
        self.assertNotIn("storage_memory_profile", value)

    def test_cases_must_be_exact_ordered_list_not_a_sweep_or_duplicate(self):
        for cases in (None, (), ("capacity16g", "baseline"), [], ["baseline", "capacity16g"],
                      ["capacity16g"], ["capacity16g", "capacity16g"],
                      ["capacity16g", "baseline", "baseline"], [True, "baseline"],
                      [{"name": "capacity16g", "count": 100000}, {"name": "baseline"}]):
            value = plan(); value["cases"] = cases
            with self.subTest(cases=cases), self.assertRaises(ValueError):
                s.validate_plan(value, check_assets=False)

    def test_prior_rows_bound_is_exact_integer_and_cumulative_ceiling_is_inclusive(self):
        for rows in (0, 1, 7800000):
            value = plan(); value["prior_rows_upper"] = rows
            self.assertIs(s.validate_plan(value, check_assets=False), value)
        for rows in (None, True, False, -1, 0.0, 7800000.0, "0", 7800001, [], {}):
            value = plan(); value["prior_rows_upper"] = rows
            with self.subTest(rows=rows), self.assertRaises(ValueError):
                s.validate_plan(value, check_assets=False)
        value = plan(); del value["prior_rows_upper"]
        with self.assertRaises(ValueError):
            s.validate_plan(value, check_assets=False)

    def test_storage_profile_is_explicit_two_process_64_and_never_top_level_memory(self):
        for key in ("storage_process_count", "storage_max_outstanding"):
            value = plan(); del value[key]
            with self.subTest(missing=key), self.assertRaises(ValueError):
                s.validate_plan(value, check_assets=False)
        for key, bad in (("storage_process_count", 1), ("storage_process_count", True),
                         ("storage_process_count", 2.0), ("storage_process_count", "2"),
                         ("storage_max_outstanding", 128), ("storage_max_outstanding", True),
                         ("storage_max_outstanding", 64.0), ("storage_max_outstanding", "64")):
            value = plan(); value[key] = bad
            with self.subTest(key=key, bad=bad), self.assertRaises(ValueError):
                s.validate_plan(value, check_assets=False)
        for profile in (None, "baseline", "capacity16g"):
            value = plan(); value["storage_memory_profile"] = profile
            with self.subTest(profile=profile), self.assertRaises(ValueError):
                s.validate_plan(value, check_assets=False)

    def test_fixed_row_probe_and_approval_guards_are_not_relaxed(self):
        for key, bad in (("schema", "otel_native_diagnostic_v1"), ("approved", 1),
                         ("count", True), ("count", 100001), ("count", 100000.0),
                         ("probe_count", 100000), ("probe_count", 1000000.0),
                         ("probe_seconds", True), ("probe_seconds", 121),
                         ("location", "fsn1"), ("server_type", "ccx33"),
                         ("replication", "double"), ("batch", 128), ("workers", 16),
                         ("gomaxprocs", 8), ("deadline_epoch", 8201)):
            value = plan(); value[key] = bad
            with self.subTest(key=key, bad=bad), self.assertRaises(ValueError):
                s.validate_plan(value, check_assets=False)

    def test_case_plans_preserve_lease_identity_absolute_clock_and_authorizations(self):
        lease = plan(); before = copy.deepcopy(lease)
        keys = ("campaign", "prefix", "first_epoch", "deadline_epoch", "source_revision",
                "source_root", "source_manifest", "input_manifest_sha256", "binary", "binary_sha256",
                "count", "probe_count", "probe_seconds", "storage_process_count", "storage_max_outstanding")
        for profile in s.CASES:
            value = s.case_plan(lease, profile)
            self.assertIsNot(value, lease)
            self.assertEqual(value["schema"], "otel_native_diagnostic_v1")
            self.assertEqual(value["storage_memory_profile"], profile)
            self.assertNotIn("cases", value)
            self.assertNotIn("prior_rows_upper", value)
            for key in keys:
                self.assertEqual(value[key], lease[key], key)
            self.assertIs(n.validate_plan(value, check_assets=False), value)
        self.assertEqual(lease, before)
        for profile in (None, "other", "CAPACITY16G", True):
            with self.subTest(profile=profile), self.assertRaises(ValueError):
                s.case_plan(lease, profile)

    def test_common_asset_projection_retains_hash_and_symlink_guards(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp); value = plan()
            for key in p.ASSETS:
                path = root/key; path.write_text("sealed-"+key)
                value[key] = str(path)
                value["input_manifest_sha256" if key == "source_manifest" else key+"_sha256"] = p.sha(path)
            self.assertIs(s.validate_plan(value), value)
            binary = Path(value["binary"]); binary.write_text("changed")
            with self.assertRaisesRegex(ValueError, "changed asset"):
                s.validate_plan(value)
            binary.write_text("sealed-binary")
            target = root/"other"; target.write_bytes(binary.read_bytes()); binary.unlink(); binary.symlink_to(target)
            with self.assertRaisesRegex(ValueError, "changed asset"):
                s.validate_plan(value)


class FakeArm:
    def __init__(self, harness, index, arm_plan, out, provider, **kwargs):
        self.harness, self.index, self.plan, self.out = harness, index, arm_plan, Path(out)
        self.provider, self.kwargs = provider, kwargs
        self.nodes, self.layouts = {}, []
        self.trace_remaining, self.observation_history = n.TRACE_BUDGET, {}
        self.last_status_document, self.last_wave_result = None, None
        self.scopes = [{"role": "client1", "unit": "fake-producer-"+str(index)}]
        self.entry_budget, self.entry_history = None, None

    def run_existing(self, resources, *, helpers_ready=False):
        h = self.harness; profile = self.plan["storage_memory_profile"]
        self.entry_budget, self.entry_history = self.trace_remaining, dict(self.observation_history)
        h.events.append(("helpers", profile, helpers_ready))
        self.nodes = resources["nodes"]
        h.events.append(("quiet", profile))
        self.layouts.append({"root": "/srv/otel-horizontal/"+self.plan["campaign"]+"/arms/"+self.kwargs["arm_id"],
                             "storage_hosts": ["node1"], "storage_process_count": 2})
        h.events.append(("cluster", profile))
        h.events.append(("prerequisite", profile, self.kwargs["run_base"]))
        self.trace_remaining -= h.consumption[self.index]
        self.observation_history[profile] = {"pswpin": 0, "pswpout": 0, "sequence": self.index}
        error = h.outcomes[self.index]
        if error is not None:
            self.last_status_document = {"literal_failed_case": profile}
            self.last_wave_result = {"observation_phase": "load", "release_bound": True,
                                     "profile": {"producer_process_bound": True}, "error": str(error)}
            raise error
        h.events.append(("probe", profile, self.kwargs["run_base"]+1))
        return completed_result()

    def host_samples(self):
        self.harness.events.append(("fresh-hosts", self.plan["storage_memory_profile"]))
        if self.harness.host_error is not None:
            raise self.harness.host_error
        return {"node1": {"fdb_pids": {"200": "5000"}}, "client1": {"fdb_pids": {}}}

    def scope_bindings(self):
        return self.scopes


class Harness:
    def __init__(self, root, *, outcomes=(None, None), consumption=(11, 17)):
        self.plan, self.events, self.arms = plan(), [], []
        self.outcomes, self.consumption = outcomes, consumption
        self.host_error, self.stop_error, self.cleanup_error, self.prepare_error = None, None, None, None
        self.stop_result = {"passed": True, "quiescent": True}
        self.cleanup_result = {"passed": True, "protected_baseline_restored": True}
        self.out = Path(root)/"campaign"/"run"
        assets = Path(root)/"assets"; assets.mkdir()
        for key in p.ASSETS:
            path = assets/key; path.write_text("sealed-"+key)
            self.plan[key] = str(path)
            self.plan["input_manifest_sha256" if key == "source_manifest" else key+"_sha256"] = p.sha(path)
        self.resources = {"prepared": True, "campaign": self.plan["campaign"], "prefix": self.plan["prefix"],
                          "ids": {"server": [101, 102]}, "protected_ids": {"server": [1]},
                          "nodes": {"node1": {"id": 101}, "client1": {"id": 102}}}
        self.provider = mock.Mock()
        self.provider.prepare.side_effect = self.prepare
        self.provider.stop_owned_arm.side_effect = self.stop
        self.provider.teardown.side_effect = self.teardown
        self.session = s.Session(self.plan, self.out, self.provider, arm_factory=self.arm_factory)

    def prepare(self, lease, out):
        self.events.append(("prepare", lease, out))
        if self.prepare_error is not None:
            raise self.prepare_error
        return self.resources

    def arm_factory(self, arm_plan, out, provider, **kwargs):
        arm = FakeArm(self, len(self.arms), arm_plan, out, provider, **kwargs)
        self.arms.append(arm)
        return arm

    def stop(self, arm_plan, out, nodes, layout, *, scopes):
        self.events.append(("stop", arm_plan["storage_memory_profile"]))
        if self.stop_error is not None:
            raise self.stop_error
        return self.stop_result

    def teardown(self, lease, out, resources):
        self.events.append(("cleanup", lease, out, resources))
        if self.cleanup_error is not None:
            raise self.cleanup_error
        return self.cleanup_result

    def verify_source(self, lease):
        self.events.append(("source", lease))
        return {"source_files": 1, "input_manifest_sha256": lease["input_manifest_sha256"]}


class SessionTests(unittest.TestCase):
    def setUp(self):
        for name in ("run", "Popen"):
            guard = mock.patch.object(n.subprocess, name, side_effect=AssertionError("real subprocess forbidden"))
            guard.start(); self.addCleanup(guard.stop)

    @contextmanager
    def running(self, harness):
        with mock.patch.object(p, "verify_source", side_effect=harness.verify_source) as source, \
             mock.patch.object(d, "remaining", return_value=30) as remaining:
            yield source, remaining

    def assert_finalized(self, harness, *, source_calls=4):
        provider, root = harness.provider, harness.out
        provider.prepare.assert_called_once_with(harness.plan, root/"provision")
        provider.teardown.assert_called_once_with(harness.plan, root/"provision", harness.resources)
        self.assertEqual(sum(event[0] == "source" for event in harness.events), source_calls)
        self.assertEqual([event[0] for event in harness.events[-2:]], ["cleanup", "source"])
        self.assertEqual(d.read(root/"source-after.json")["source_files"], 1)
        self.assertEqual(d.read(root/"approved-plan.json"), harness.plan)

    def test_one_lease_runs_two_independent_cases_then_one_final_cleanup(self):
        with tempfile.TemporaryDirectory() as tmp:
            h = Harness(tmp)
            with self.running(h):
                result = h.session.run()
            self.assert_finalized(h)
            self.assertEqual(result["status"], "ALL_CASES_PASSED")
            self.assertTrue(result["all_cases_passed"])
            self.assertEqual(result["failed_cases"], [])
            self.assertEqual([(case["name"], case["status"]) for case in result["cases"]],
                             [("capacity16g", "PASS"), ("baseline", "PASS")])
            self.assertEqual(result["authorized_rows"], 2200000)
            self.assertEqual(result["cumulative_rows_upper"], 2200000)
            self.assertTrue(result["diagnostic_only"])
            self.assertFalse(result["accepted_throughput"])
            self.assertNotIn("ack_rows_per_second", result)
            self.assertEqual(d.read(h.out/"session-result.json"), result)
            self.assertEqual(h.provider.stop_owned_arm.call_count, 2)
            self.assertEqual([event[0] for event in h.events],
                             ["source", "prepare", "helpers", "quiet", "cluster", "prerequisite", "probe", "fresh-hosts", "source", "stop",
                              "helpers", "quiet", "cluster", "prerequisite", "probe", "fresh-hosts", "source", "stop", "cleanup", "source"])

    def test_case_ids_runs_helpers_and_receipt_roots_are_distinct_under_shared_provision(self):
        with tempfile.TemporaryDirectory() as tmp:
            h = Harness(tmp); before = copy.deepcopy(h.plan)
            with self.running(h):
                h.session.run()
            self.assertEqual(h.plan, before)
            for index, arm in enumerate(h.arms):
                profile = s.CASES[index]
                self.assertEqual(arm.out, h.out/"cases"/profile)
                self.assertEqual(arm.kwargs, {"provision_out": h.out/"provision", "receipt_root": h.out,
                                              "arm_id": "session-"+profile, "run_base": 9800+index*2})
                self.assertIs(arm.provider, h.provider)
                self.assertEqual(arm.plan, s.case_plan(h.plan, profile))
                stop = h.provider.stop_owned_arm.call_args_list[index]
                self.assertEqual(stop.args, (arm.plan, h.out/"provision", h.resources["nodes"], arm.layouts[-1]))
                self.assertEqual(stop.kwargs, {"scopes": arm.scopes})
            self.assertEqual([event for event in h.events if event[0] == "helpers"],
                             [("helpers", "capacity16g", False), ("helpers", "baseline", True)])
            self.assertEqual([event[2] for event in h.events if event[0] in ("prerequisite", "probe")],
                             [9800, 9801, 9802, 9803])

    def test_trace_budget_and_observation_history_are_shared_not_reset_per_case(self):
        for consumption in ((11, 17), (n.TRACE_BUDGET, 0)):
            with self.subTest(consumption=consumption), tempfile.TemporaryDirectory() as tmp:
                h = Harness(tmp, consumption=consumption)
                with self.running(h):
                    result = h.session.run()
                first, second = h.arms
                self.assertEqual(first.entry_budget, 16*1024**2)
                self.assertEqual(second.entry_budget, 16*1024**2-consumption[0])
                self.assertEqual(second.entry_history, {"capacity16g": {"pswpin": 0, "pswpout": 0, "sequence": 0}})
                self.assertIs(first.observation_history, second.observation_history)
                self.assertIs(h.session.observation_history, first.observation_history)
                self.assertEqual(result["trace_bytes_remaining"], n.TRACE_BUDGET-sum(consumption))
                self.assertEqual([case["trace_bytes_remaining"] for case in result["cases"]],
                                 [n.TRACE_BUDGET-consumption[0], n.TRACE_BUDGET-sum(consumption)])

    def test_known_arm_failure_stays_failed_and_never_promotes_its_probe_but_next_case_runs(self):
        for outcomes in ((ValueError("strict client health"), None),
                         (None, ValueError("strict client health")),
                         (ValueError("strict client health"), ValueError("strict client health"))):
            with self.subTest(outcomes=outcomes), tempfile.TemporaryDirectory() as tmp:
                h = Harness(tmp, outcomes=outcomes)
                with self.running(h), mock.patch.object(s, "classify_arm_failure", return_value=True) as classify:
                    result = h.session.run()
                failed = [s.CASES[i] for i, error in enumerate(outcomes) if error is not None]
                self.assert_finalized(h)
                self.assertEqual(result["status"], "COMPLETED_WITH_ARM_FAILURES")
                self.assertFalse(result["all_cases_passed"])
                self.assertEqual(result["failed_cases"], failed)
                self.assertEqual(len(h.arms), 2)
                self.assertEqual(h.provider.stop_owned_arm.call_count, 2)
                self.assertEqual(classify.call_count, len(failed))
                for index, arm in enumerate(h.arms):
                    case = result["cases"][index]; profile = s.CASES[index]
                    self.assertEqual(case["status"], "ARM_FAILURE" if profile in failed else "PASS")
                    self.assertEqual(d.read(arm.out/"session-case.json"), case)
                    if profile in failed:
                        self.assertFalse(any(event[:2] == ("probe", profile) for event in h.events))
                        saved = d.read(arm.out/"arm-failure-classification.json")
                        self.assertEqual(saved["raw_status"], arm.last_status_document)
                        self.assertFalse(saved["health_pass"])
                for call in classify.call_args_list:
                    self.assertEqual(call.kwargs, {"fatal_guards_passed": True})

    def test_unknown_exception_is_fatal_and_never_starts_next_case(self):
        for error in (ValueError("unknown status warning"), OSError("SSH transport failed"), RuntimeError("unexpected")):
            with self.subTest(error=error), tempfile.TemporaryDirectory() as tmp:
                h = Harness(tmp, outcomes=(error, None))
                with self.running(h), mock.patch.object(s, "classify_arm_failure", return_value=False):
                    with self.assertRaisesRegex(type(error), str(error)):
                        h.session.run()
                self.assert_finalized(h, source_calls=3)
                self.assertEqual(len(h.arms), 1)
                h.provider.stop_owned_arm.assert_not_called()
                result = d.read(h.out/"session-result.json")
                self.assertEqual(result["status"], "SESSION_FATAL")
                self.assertFalse(result["all_cases_passed"])
                self.assertEqual(result["cases"][0]["status"], "SESSION_FATAL")

    def test_fresh_host_fault_prevents_classifier_and_next_case_after_pass_or_failure(self):
        for arm_error in (None, ValueError("strict client health")):
            for reason in ("host swap activity", "unchanged native cgroup budget/OOM", "FDB process PID/starttime changed"):
                with self.subTest(arm_error=arm_error, reason=reason), tempfile.TemporaryDirectory() as tmp:
                    h = Harness(tmp, outcomes=(arm_error, None))
                    h.host_error = ValueError(reason)
                    with self.running(h), mock.patch.object(s, "classify_arm_failure", return_value=True) as classify:
                        with self.assertRaisesRegex(ValueError, reason):
                            h.session.run()
                    self.assert_finalized(h, source_calls=2)
                    classify.assert_not_called()
                    self.assertEqual(len(h.arms), 1)
                    h.provider.stop_owned_arm.assert_not_called()
                    self.assertEqual(d.read(h.out/"session-result.json")["status"], "SESSION_FATAL")

    def test_owned_arm_stop_failure_is_fatal_even_after_pass_or_known_arm_failure(self):
        for arm_error in (None, ValueError("strict client health")):
            for receipt in ({"passed": False, "quiescent": False}, {"passed": True},
                            {"passed": True, "quiescent": 1}, None):
                with self.subTest(arm_error=arm_error, receipt=receipt), tempfile.TemporaryDirectory() as tmp:
                    h = Harness(tmp, outcomes=(arm_error, None)); h.stop_result = receipt
                    with self.running(h), mock.patch.object(s, "classify_arm_failure", return_value=True):
                        with self.assertRaisesRegex(ValueError, "quiescence"):
                            h.session.run()
                    self.assert_finalized(h, source_calls=3)
                    self.assertEqual(len(h.arms), 1)
                    self.assertEqual(h.provider.stop_owned_arm.call_count, 1)
                    self.assertEqual(d.read(h.out/"session-result.json")["status"], "SESSION_FATAL")
        with tempfile.TemporaryDirectory() as tmp:
            h = Harness(tmp); h.stop_error = OSError("owned-arm stop failed")
            with self.running(h), self.assertRaisesRegex(OSError, "owned-arm stop failed"):
                h.session.run()
            self.assert_finalized(h, source_calls=3)
            self.assertEqual(len(h.arms), 1)

    def test_source_after_is_verified_even_when_final_cleanup_fails_or_raises(self):
        for failure in ("false", "exception"):
            with self.subTest(failure=failure), tempfile.TemporaryDirectory() as tmp:
                h = Harness(tmp)
                if failure == "false": h.cleanup_result = {"passed": False, "external_ids_missing": [999]}
                else: h.cleanup_error = OSError("cleanup command failed")
                with self.running(h), self.assertRaisesRegex((ValueError, OSError), "cleanup"):
                    h.session.run()
                self.assert_finalized(h)
                result = d.read(h.out/"session-result.json")
                self.assertEqual(result["status"], "SESSION_FATAL")
                self.assertFalse(result["all_cases_passed"])
                self.assertFalse(result["cleanup_passed"])
                if failure == "false":
                    self.assertFalse(d.read(h.out/"cleanup.json")["passed"])

    def test_zero_receipt_budget_fails_before_provision_but_still_checks_source_after(self):
        with tempfile.TemporaryDirectory() as tmp:
            h = Harness(tmp)
            with self.running(h), mock.patch.object(s, "RECEIPT_BUDGET", 0):
                with self.assertRaisesRegex(ValueError, "receipt allocation"):
                    h.session.run()
            h.provider.prepare.assert_not_called(); h.provider.teardown.assert_not_called()
            self.assertEqual(h.arms, [])
            self.assertEqual(sum(event[0] == "source" for event in h.events), 2)
            self.assertEqual(d.read(h.out/"session-result.json")["status"], "SESSION_FATAL")

    def test_trace_budget_increase_negative_or_untyped_remaining_is_fatal(self):
        for bad in (n.TRACE_BUDGET+1, -1, True, 1.0, None):
            with self.subTest(remaining=bad), tempfile.TemporaryDirectory() as tmp:
                h = Harness(tmp)
                original = h.session.arm_factory
                def factory(*args, **kwargs):
                    arm = original(*args, **kwargs); run = arm.run_existing
                    def changed(*run_args, **run_kwargs):
                        result = run(*run_args, **run_kwargs); arm.trace_remaining = bad; return result
                    arm.run_existing = changed
                    return arm
                h.session.arm_factory = factory
                with self.running(h), self.assertRaisesRegex(ValueError, "trace budget"):
                    h.session.run()
                self.assert_finalized(h, source_calls=3)
                self.assertEqual(len(h.arms), 1)
                result = d.read(h.out/"session-result.json")
                self.assertEqual(result["status"], "SESSION_FATAL")
                self.assertFalse(result["all_cases_passed"])
                self.assertEqual(result["failed_cases"], ["capacity16g"])
                self.assertEqual(result["cases"][0]["status"], "SESSION_FATAL")
                self.assertEqual(d.read(h.arms[0].out/"session-case.json"), result["cases"][0])

    def test_observation_history_reset_or_equal_copy_is_fatal_and_still_finalizes(self):
        for replacement in ("empty", "equal-copy"):
            with self.subTest(replacement=replacement), tempfile.TemporaryDirectory() as tmp:
                h = Harness(tmp); original = h.session.arm_factory
                shared_history = h.session.observation_history
                def factory(*args, **kwargs):
                    arm = original(*args, **kwargs); run = arm.run_existing
                    def changed(*run_args, **run_kwargs):
                        result = run(*run_args, **run_kwargs)
                        arm.observation_history = {} if replacement == "empty" else dict(arm.observation_history)
                        return result
                    arm.run_existing = changed
                    return arm
                h.session.arm_factory = factory
                with self.running(h), self.assertRaisesRegex(ValueError, "host observation history reset"):
                    h.session.run()
                self.assert_finalized(h, source_calls=3)
                self.assertEqual(len(h.arms), 1)
                self.assertIs(h.session.observation_history, shared_history)
                self.assertIsNot(h.arms[0].observation_history, shared_history)
                result = d.read(h.out/"session-result.json")
                self.assertEqual(result["status"], "SESSION_FATAL")
                self.assertFalse(result["all_cases_passed"])
                self.assertTrue(result["cleanup_passed"])
                self.assertEqual(result["failed_cases"], ["capacity16g"])
                self.assertEqual(result["cases"][0]["status"], "SESSION_FATAL")
                self.assertEqual(d.read(h.arms[0].out/"session-case.json"), result["cases"][0])

    def test_session_rejects_unprepared_wrong_role_duplicate_or_protected_resources_before_arm_creation(self):
        changes = (
            lambda value: value.update(prepared=False),
            lambda value: value.update(campaign="other-test"),
            lambda value: value.update(prefix="other-test"),
            lambda value: value["nodes"].pop("client1"),
            lambda value: value["nodes"].update(node2={"id": 103}),
            lambda value: value["nodes"]["client1"].update(id=101),
            lambda value: value["nodes"]["client1"].update(id=True),
            lambda value: value["protected_ids"].update(server=[101]),
            lambda value: value["ids"].update(server=[101, 103]),
        )
        for index, change in enumerate(changes):
            with self.subTest(change=index), tempfile.TemporaryDirectory() as tmp:
                h = Harness(tmp); change(h.resources)
                with self.running(h), self.assertRaises(ValueError):
                    h.session.run()
                self.assert_finalized(h, source_calls=2)
                self.assertEqual(h.arms, [])
                h.provider.stop_owned_arm.assert_not_called()

    def test_real_arms_repeat_quiet_cluster_and_own_gate_without_reprovisioning(self):
        for fail_second_gate in (False, True):
            with self.subTest(fail_second_gate=fail_second_gate), tempfile.TemporaryDirectory() as tmp:
                h = Harness(tmp); arms = []
                def setup(arm_plan, out, nodes, hosts, arm_id):
                    profile = arm_plan["storage_memory_profile"]
                    self.assertEqual(h.events[-1], ("quiet", profile))
                    self.assertEqual((out, nodes, hosts, arm_id),
                                     (h.out/"provision", h.resources["nodes"], 1, "session-"+profile))
                    h.events.append(("cluster", profile))
                    return {"root": "/owned/"+arm_id, "storage_hosts": ["node1"],
                            "remote_cluster_file": "/owned/"+arm_id+"/fdb.cluster"}
                h.provider.setup_cluster.side_effect = setup
                def factory(arm_plan, out, provider, **kwargs):
                    profile = arm_plan["storage_memory_profile"]
                    arm = n.Diagnostic(arm_plan, out, provider, **kwargs)
                    arm.install_helpers = mock.Mock(side_effect=lambda: h.events.append(("install", profile)))
                    arm.verify_helpers = mock.Mock(side_effect=lambda: h.events.append(("verify", profile)))
                    arm.quiet = mock.Mock(side_effect=lambda: h.events.append(("quiet", profile)))
                    arm.stable = mock.Mock()
                    arm.host_samples = mock.Mock(return_value={"node1": {"fdb_pids": {"200": "5000"}},
                                                               "client1": {"fdb_pids": {}}})
                    def wave(layout, count, name):
                        h.events.append((name, profile))
                        if name == "probe":
                            self.assertTrue(any(event == ("prerequisite", profile) for event in h.events))
                            self.assertEqual(count, 1000000)
                            return completed_result()["probe"]
                        self.assertEqual((count, name), (100000, "prerequisite"))
                        return {"count": 100000, "producers": 1, "healthy": not (fail_second_gate and profile == "baseline"),
                                "ingest_valid": True, "census_pass": True, "oracle_pass": True}
                    arm.wave = mock.Mock(side_effect=wave)
                    arms.append(arm)
                    return arm
                h.session.arm_factory = factory
                with self.running(h):
                    if fail_second_gate:
                        with self.assertRaisesRegex(ValueError, "100k"):
                            h.session.run()
                        result = d.read(h.out/"session-result.json")
                    else:
                        result = h.session.run()
                self.assert_finalized(h, source_calls=8)
                self.assertEqual(h.provider.setup_cluster.call_count, 2)
                self.assertEqual(h.provider.stop_owned_arm.call_count, 1 if fail_second_gate else 2)
                self.assertEqual(len(arms), 2)
                arms[0].install_helpers.assert_called_once_with(); arms[0].verify_helpers.assert_not_called()
                arms[1].verify_helpers.assert_called_once_with(); arms[1].install_helpers.assert_not_called()
                expected = [("install", "capacity16g"), ("quiet", "capacity16g"), ("cluster", "capacity16g"),
                            ("prerequisite", "capacity16g"), ("probe", "capacity16g"), ("verify", "baseline"),
                            ("quiet", "baseline"), ("cluster", "baseline"), ("prerequisite", "baseline")]
                if not fail_second_gate:
                    expected.append(("probe", "baseline"))
                self.assertEqual([event for event in h.events if event[0] in
                                  ("install", "verify", "quiet", "cluster", "prerequisite", "probe")], expected)
                self.assertEqual([call.args[1:] for call in arms[0].wave.call_args_list],
                                 [(100000, "prerequisite"), (1000000, "probe")])
                self.assertEqual([call.args[1:] for call in arms[1].wave.call_args_list],
                                 [(100000, "prerequisite")] if fail_second_gate else [(100000, "prerequisite"), (1000000, "probe")])
                self.assertEqual(result["cases"][0]["status"], "PASS")
                self.assertEqual(result["cases"][1]["status"], "SESSION_FATAL" if fail_second_gate else "PASS")
                saved = d.read(arms[1].out/"diagnostic-result.json")
                self.assertNotIn("probe", saved) if fail_second_gate else self.assertIn("probe", saved)

    def test_fresh_source_or_deadline_failure_is_fatal_after_pass_or_failure(self):
        for arm_error in (None, ValueError("strict client health")):
            for guard in ("source", "deadline"):
                with self.subTest(arm_error=arm_error, guard=guard), tempfile.TemporaryDirectory() as tmp:
                    h = Harness(tmp, outcomes=(arm_error, None))
                    with self.running(h) as (source, remaining), \
                         mock.patch.object(s, "classify_arm_failure", return_value=True) as classify:
                        if guard == "source":
                            def verify(lease):
                                value = h.verify_source(lease)
                                if source.call_count == 2:
                                    raise ValueError("changed source file")
                                return value
                            source.side_effect = verify
                        else:
                            remaining.side_effect = [30, ValueError("controller budget exhausted")]
                        with self.assertRaisesRegex(ValueError, "changed source|budget exhausted"):
                            h.session.run()
                    self.assert_finalized(h, source_calls=3)
                    classify.assert_not_called()
                    self.assertEqual(len(h.arms), 1)
                    h.provider.stop_owned_arm.assert_not_called()

    def test_partial_untyped_or_unhealthy_arm_return_never_promotes_to_pass(self):
        invalid = [None, {}, {"diagnostic_only": True, "accepted_throughput": False}]
        for key, value in (("diagnostic_only", False), ("diagnostic_only", 1),
                           ("accepted_throughput", True), ("accepted_throughput", 0)):
            candidate = completed_result(); candidate[key] = value; invalid.append(candidate)
        for phase in ("prerequisite", "probe"):
            candidate = completed_result(); del candidate[phase]; invalid.append(candidate)
            for key, value in (("count", True), ("count", 100000.0), ("count", 99999),
                               ("producers", True), ("producers", 1.0), ("producers", 2),
                               ("healthy", False), ("healthy", 1), ("ingest_valid", False),
                               ("census_pass", False), ("oracle_pass", False)):
                candidate = completed_result(); candidate[phase][key] = value; invalid.append(candidate)
        for index, value in enumerate(invalid):
            with self.subTest(result=index), tempfile.TemporaryDirectory() as tmp:
                h = Harness(tmp); original = h.session.arm_factory
                def factory(*args, **kwargs):
                    arm = original(*args, **kwargs); run = arm.run_existing
                    def incomplete(*run_args, **run_kwargs):
                        run(*run_args, **run_kwargs)
                        return value
                    arm.run_existing = incomplete
                    return arm
                h.session.arm_factory = factory
                with self.running(h), self.assertRaisesRegex(ValueError, "session"):
                    h.session.run()
                self.assert_finalized(h, source_calls=3)
                self.assertEqual(len(h.arms), 1)
                h.provider.stop_owned_arm.assert_not_called()
                result = d.read(h.out/"session-result.json")
                self.assertFalse(result["all_cases_passed"])
                self.assertEqual(result["status"], "SESSION_FATAL")
                self.assertEqual(result["cases"][0]["status"], "SESSION_FATAL")
                self.assertEqual(result["failed_cases"], ["capacity16g"])


class SessionCLITests(unittest.TestCase):
    def test_readonly_cli_validates_without_provider_import_or_session_effects(self):
        with tempfile.TemporaryDirectory() as tmp:
            h = Harness(tmp); path = Path(tmp)/"plan.json"; path.write_text(json.dumps(h.plan))
            original_import = builtins.__import__
            def guarded_import(name, *args, **kwargs):
                if name == "provision":
                    raise AssertionError("readonly CLI imported provider")
                return original_import(name, *args, **kwargs)
            with mock.patch("builtins.__import__", guarded_import), \
                 mock.patch.object(s, "Session", side_effect=AssertionError("controller started")), \
                 mock.patch.object(p, "verify_source", side_effect=AssertionError("source effect")), \
                 mock.patch("builtins.print") as output:
                s.main(["run", "--plan", str(path), "--output", str(h.out)])
            value = json.loads(output.call_args.args[0])
            self.assertEqual({key: value[key] for key in ("nodes", "cases", "rows", "cumulative_rows")},
                             {"nodes": 2, "cases": 2, "rows": 2200000, "cumulative_rows": 2200000})
            self.assertFalse(value["provisioned"])
            self.assertFalse(value["accepted_throughput"])
            self.assertFalse(h.out.exists())

    def test_approved_cli_exits_one_for_kept_arm_failure_and_passes_output_directly(self):
        for passed in (True, False):
            with self.subTest(all_cases_passed=passed), tempfile.TemporaryDirectory() as tmp:
                h = Harness(tmp); path = Path(tmp)/"plan.json"; path.write_text(json.dumps(h.plan))
                with mock.patch.dict("sys.modules", {"provision": h.provider}), mock.patch.object(s, "Session") as session:
                    session.return_value.run.return_value = {"all_cases_passed": passed}
                    argv = ["run", "--plan", str(path), "--output", str(h.out), "--execute-approved"]
                    if passed:
                        self.assertIsNone(s.main(argv))
                    else:
                        with self.assertRaises(SystemExit) as error:
                            s.main(argv)
                        self.assertEqual(error.exception.code, 1)
                    session.assert_called_once_with(h.plan, str(h.out), h.provider)
                    session.return_value.run.assert_called_once_with()
                self.assertFalse(h.out.exists())

    def test_reset_flag_is_not_an_authorization_to_restart_lease_clock_or_row_budget(self):
        with tempfile.TemporaryDirectory() as tmp:
            h = Harness(tmp); path = Path(tmp)/"plan.json"; path.write_text(json.dumps(h.plan))
            with mock.patch.object(s, "Session") as session, mock.patch("sys.stderr"):
                with self.assertRaises(SystemExit) as error:
                    s.main(["run", "--plan", str(path), "--output", str(h.out), "--execute-approved", "--reset"])
                self.assertEqual(error.exception.code, 2)
                session.assert_not_called()
            self.assertFalse(h.out.exists())

    def test_execution_requires_explicit_output_before_importing_provider(self):
        with tempfile.TemporaryDirectory() as tmp:
            h = Harness(tmp); path = Path(tmp)/"plan.json"; path.write_text(json.dumps(h.plan))
            with mock.patch.object(s, "Session", side_effect=AssertionError("controller started")):
                with self.assertRaisesRegex(ValueError, "output required"):
                    s.main(["run", "--plan", str(path), "--execute-approved"])


def canonical_failure():
    document, layout = status_fixture()
    base = document["cluster"]["processes"]["one"]
    layout.update(storage_process_count=2,
                  remote_cluster_file="/srv/otel-horizontal/native-test/arms/session-capacity16g/fdb.cluster")
    specs, processes = [], {}
    assignments = ((4500, "stateless", ["master", "cluster_controller"]),
                   (4501, "commit_proxy", ["commit_proxy"]), (4502, "grv_proxy", ["grv_proxy"]),
                   (4503, "resolution", ["resolver"]), (4504, "transaction", ["log"]),
                   (4510, "storage", ["storage"]), (4511, "storage", ["storage"]))
    for port, cls, roles in assignments:
        address = "10.93.0.2:"+str(port); memory = (4 if cls == "storage" else 1)*p.GIB
        specs.append({"node": "node1", "port": port, "address": address, "process_class": cls,
                      "required_roles": roles, "machineid": "node1", "zoneid": "node1", "memory_limit_bytes": memory})
        process = copy.deepcopy(base)
        process.update(address=address, class_type=cls, excluded=False, version="7.3.77",
                       memory={"rss_bytes": 100, "used_bytes": 50, "limit_bytes": memory},
                       roles=[{"role": role, "id": "role-"+str(port)+"-"+role} for role in roles])
        if cls == "storage":
            process["roles"][0].update(input_bytes={"counter": 100}, durable_bytes={"counter": 10}, stored_bytes=20, shards=1)
        processes[str(port)] = process
    layout["processes"] = specs
    cluster, client = document["cluster"], document["client"]
    cluster.update(processes=processes, full_replication=True, incompatible_connections=[],
                   messages=[{"name": "batch_priority_transaction_start_probe_timeout",
                              "description": "Unable to start batch priority transaction after 5 seconds."}])
    cluster["data"]["state"]["name"] = "healthy"
    cluster["data"]["team_trackers"] = [{"state": {"healthy": True}, "unhealthy_servers": 0}]
    reason = {"name": "storage_server_write_queue_size", "description": "Storage server performance (storage queue).",
              "reason_id": 1, "reason_server_id": "role-4510-storage"}
    cluster["qos"].update(performance_limited_by=copy.deepcopy(reason), batch_performance_limited_by=copy.deepcopy(reason),
                         worst_queue_bytes_storage_server=512*1024**2)
    client["database_status"]["healthy"] = False
    client["cluster_file"]["path"] = layout["remote_cluster_file"]
    client["coordinators"]["coordinators"] = [{"address": "10.93.0.2:4500", "reachable": True}]
    wave = {"schema": "otel_native_wave_v1", "count": 1000000, "producers": 1, "diagnostic_only": True,
            "accepted_throughput": False, "observation_phase": "load", "release_bound": True,
            "error": "strict client health", "profile": {"producer_process_bound": True},
            "producer_process_receipt": {"started": True, "pid": 900, "starttime_ticks": 9000},
            "sampler_exits": {"node1": 0, "client1": 0},
            "native_activity": [{"storage_processes": {
                "10.93.0.2:4510": {"node": "node1", "storage_id": "role-4510-storage", "input_bytes": value,
                                    "durable_bytes": 10, "stored_bytes": 20},
                "10.93.0.2:4511": {"node": "node1", "storage_id": "role-4511-storage", "input_bytes": value,
                                    "durable_bytes": 10, "stored_bytes": 20}}} for value in (90, 95, 100)]}
    return document, layout, wave


class FailureClassifierTests(unittest.TestCase):
    def classify(self, document, layout, wave, *, error=None, guards=True):
        return s.classify_arm_failure(ValueError("strict client health") if error is None else error,
                                      document, layout, wave, fatal_guards_passed=guards)

    def test_only_canonical_raw_batch_queue_failure_is_reusable_never_healthy(self):
        document, layout, wave = canonical_failure(); before = copy.deepcopy((document, layout, wave))
        with self.assertRaisesRegex(ValueError, "strict client health"):
            p.strict_status(document, layout)
        self.assertIs(self.classify(document, layout, wave), True)
        self.assertEqual((document, layout, wave), before)
        with self.assertRaisesRegex(ValueError, "strict client health"):
            p.strict_status(document, layout)
        self.assertFalse(document["client"]["database_status"]["healthy"])
        self.assertFalse(wave["accepted_throughput"])

    def test_classifier_requires_exact_error_load_release_process_binding_and_fresh_guards(self):
        document, layout, wave = canonical_failure()
        for error in (ValueError("strict client health "), ValueError("client status warning"),
                      RuntimeError("strict client health"), OSError("strict client health")):
            with self.subTest(error=error):
                self.assertIs(self.classify(document, layout, wave, error=error), False)
        for guards in (False, None, 1, "true"):
            with self.subTest(guards=guards):
                self.assertIs(self.classify(document, layout, wave, guards=guards), False)
        for key, value in (("release_bound", False), ("release_bound", 1), ("observation_phase", "baseline"),
                           ("observation_phase", "verification"), ("error", "unknown"),
                           ("profile", {}), ("profile", {"producer_process_bound": False}),
                           ("profile", {"producer_process_bound": 1}), ("profile", None), ("profile", [])):
            changed = copy.deepcopy(wave); changed[key] = value
            with self.subTest(key=key, value=value):
                self.assertIs(self.classify(document, layout, changed), False)
        for changed in (None, [], {}, "wave"):
            with self.subTest(wave=changed):
                self.assertIs(self.classify(document, layout, changed), False)

    def test_sampler_exit_proof_requires_exact_two_roles_and_typed_zero_exits(self):
        document, layout, wave = canonical_failure()
        self.assertIs(self.classify(document, layout, wave), True)
        changed = copy.deepcopy(wave); del changed["sampler_exits"]
        self.assertIs(self.classify(document, layout, changed), False)
        for exits in (None, [], {}, "0", {"node1": 0}, {"client1": 0},
                      {"node1": 0, "client2": 0}, {"node1": 0, "client1": 0, "node2": 0}):
            changed = copy.deepcopy(wave); changed["sampler_exits"] = exits
            with self.subTest(exits=exits):
                self.assertIs(self.classify(document, layout, changed), False)
        for role in ("node1", "client1"):
            for code in (1, -1, 137, True, False, 0.0, "0", None, [], {}):
                changed = copy.deepcopy(wave); changed["sampler_exits"][role] = code
                with self.subTest(role=role, code=code):
                    self.assertIs(self.classify(document, layout, changed), False)

    def test_collection_errors_must_be_absent_or_an_empty_list_not_silenced_unknown_faults(self):
        document, layout, wave = canonical_failure()
        self.assertNotIn("collection_errors", wave)
        self.assertIs(self.classify(document, layout, wave), True)
        changed = copy.deepcopy(wave); changed["collection_errors"] = []
        self.assertIs(self.classify(document, layout, changed), True)
        for errors in (None, False, 0, "", {}, (), ["SSH transport failed"], ["producer abort failed"],
                       ["producer reap timed out"], ["trace capture failed"], [None], [False], [{}]):
            changed = copy.deepcopy(wave); changed["collection_errors"] = errors
            with self.subTest(errors=errors):
                self.assertIs(self.classify(document, layout, changed), False)

    def test_started_producer_receipt_requires_positive_exact_integer_pid_and_starttime(self):
        document, layout, wave = canonical_failure()
        changed = copy.deepcopy(wave); del changed["producer_process_receipt"]
        self.assertIs(self.classify(document, layout, changed), False)
        for receipt in (None, [], {}, "receipt", {"started": True}, {"pid": 900, "starttime_ticks": 9000}):
            changed = copy.deepcopy(wave); changed["producer_process_receipt"] = receipt
            with self.subTest(receipt=receipt):
                self.assertIs(self.classify(document, layout, changed), False)
        for value in (False, 1, "true", None):
            changed = copy.deepcopy(wave); changed["producer_process_receipt"]["started"] = value
            with self.subTest(started=value):
                self.assertIs(self.classify(document, layout, changed), False)
        for field in ("pid", "starttime_ticks"):
            changed = copy.deepcopy(wave); del changed["producer_process_receipt"][field]
            self.assertIs(self.classify(document, layout, changed), False)
            for value in (0, -1, True, False, 900.0, "900", None, [], {}, 10**18):
                changed = copy.deepcopy(wave); changed["producer_process_receipt"][field] = value
                with self.subTest(field=field, value=value):
                    self.assertIs(self.classify(document, layout, changed), False)
        changed = copy.deepcopy(wave); changed["producer_process_receipt"].update(pid=1, starttime_ticks=1)
        self.assertIs(self.classify(document, layout, changed), True)

    def test_nonempty_history_requires_exact_address_population_and_identity_in_every_sample(self):
        document, layout, wave = canonical_failure()
        self.assertEqual(len(wave["native_activity"]), 3)
        self.assertIs(self.classify(document, layout, wave), True)
        missing = copy.deepcopy(wave); del missing["native_activity"]
        self.assertIs(self.classify(document, layout, missing), False)
        for history in (None, [], {}, "history", [None], [{}], [{"storage_processes": None}],
                        [{"storage_processes": []}], [{"storage_processes": {}}]):
            changed = copy.deepcopy(wave); changed["native_activity"] = history
            with self.subTest(history=history):
                self.assertIs(self.classify(document, layout, changed), False)
        for index in range(3):
            for mutation in ("missing-address", "extra-address", "foreign-address", "swapped-ids",
                             "missing-id", "changed-id", "untyped-id", "malformed-process"):
                changed = copy.deepcopy(wave)
                processes = changed["native_activity"][index]["storage_processes"]
                first, second = "10.93.0.2:4510", "10.93.0.2:4511"
                if mutation == "missing-address":
                    del processes[second]
                elif mutation == "extra-address":
                    processes["10.93.0.2:4512"] = copy.deepcopy(processes[second])
                elif mutation == "foreign-address":
                    processes["10.93.0.2:4512"] = processes.pop(second)
                elif mutation == "swapped-ids":
                    processes[first]["storage_id"], processes[second]["storage_id"] = (
                        processes[second]["storage_id"], processes[first]["storage_id"])
                elif mutation == "missing-id":
                    del processes[second]["storage_id"]
                elif mutation == "changed-id":
                    processes[second]["storage_id"] = "replacement-storage"
                elif mutation == "untyped-id":
                    processes[second]["storage_id"] = True
                else:
                    processes[second] = None
                with self.subTest(sample=index, mutation=mutation):
                    self.assertIs(self.classify(document, layout, changed), False)

    def test_recruited_storage_identity_is_fatal_even_when_failed_qos_matches_replacement(self):
        document, layout, wave = canonical_failure()
        for port in ("4510", "4511"):
            changed = copy.deepcopy(document)
            changed["cluster"]["processes"][port]["roles"][0]["id"] = "replacement-storage"
            for reason_key in ("performance_limited_by", "batch_performance_limited_by"):
                changed["cluster"]["qos"][reason_key]["reason_server_id"] = "replacement-storage"
            with self.subTest(recruited_port=port):
                self.assertIs(self.classify(changed, layout, wave), False)
        changed = copy.deepcopy(document)
        first = changed["cluster"]["processes"]["4510"]["roles"][0]
        second = changed["cluster"]["processes"]["4511"]["roles"][0]
        first["id"], second["id"] = second["id"], first["id"]
        self.assertIs(self.classify(changed, layout, wave), False)

    def test_warning_whitelist_is_exact_not_substring_or_general_warning_tolerance(self):
        document, layout, wave = canonical_failure()
        canonical = document["cluster"]["messages"][0]
        for messages in ([], ["batch_priority_transaction_start_probe_timeout"],
                         [dict(canonical, name="transaction_start_probe_timeout")],
                         [dict(canonical, description=canonical["description"]+" More failures.")],
                         [canonical, {"name": "unknown", "description": "warning"}],
                         [dict(canonical, extra=True)]):
            changed = copy.deepcopy(document); changed["cluster"]["messages"] = messages
            with self.subTest(messages=messages):
                self.assertIs(self.classify(changed, layout, wave), False)
        changed = copy.deepcopy(document); changed["client"]["messages"] = [canonical]
        self.assertIs(self.classify(changed, layout, wave), False)
        changed = copy.deepcopy(document); changed["cluster"]["processes"]["4510"]["messages"] = [canonical]
        self.assertIs(self.classify(changed, layout, wave), False)

    def test_queue_reason_requires_both_canonical_names_types_descriptions_and_actual_storage_ids(self):
        document, layout, wave = canonical_failure()
        for reason_key in ("performance_limited_by", "batch_performance_limited_by"):
            for key, value in (("name", "storage_server_durability_lag"), ("description", "storage queue"),
                               ("reason_id", True), ("reason_id", 1.0), ("reason_id", 2),
                               ("reason_server_id", "role-4504-log"), ("reason_server_id", "unknown")):
                changed = copy.deepcopy(document); changed["cluster"]["qos"][reason_key][key] = value
                with self.subTest(reason=reason_key, key=key, value=value):
                    self.assertIs(self.classify(changed, layout, wave), False)
            for value in ({}, None, [], {"name": "storage_server_write_queue_size"}):
                changed = copy.deepcopy(document); changed["cluster"]["qos"][reason_key] = value
                with self.subTest(reason=reason_key, value=value):
                    self.assertIs(self.classify(changed, layout, wave), False)

    def test_original_cluster_client_process_memory_latency_and_identity_guards_stay_strict(self):
        document, layout, wave = canonical_failure()
        changes = (
            lambda value: value["client"]["database_status"].update(available=False),
            lambda value: value["client"]["database_status"].update(healthy=True),
            lambda value: value["client"]["database_status"].update(available=1),
            lambda value: value["client"]["cluster_file"].update(path="/foreign/fdb.cluster"),
            lambda value: value["client"]["cluster_file"].update(up_to_date=False),
            lambda value: value["client"]["coordinators"].update(quorum_reachable=False),
            lambda value: value["client"]["coordinators"]["coordinators"][0].update(reachable=False),
            lambda value: value["client"]["coordinators"]["coordinators"][0].update(address="10.0.0.1:4500"),
            lambda value: value["cluster"].update(database_available=False),
            lambda value: value["cluster"].update(full_replication=False),
            lambda value: value["cluster"].update(degraded_processes=1),
            lambda value: value["cluster"].update(incompatible_connections=["foreign"]),
            lambda value: value["cluster"]["recovery_state"].update(name="recruiting"),
            lambda value: value["cluster"]["data"]["state"].update(healthy=False),
            lambda value: value["cluster"]["data"]["state"].update(name="unhealthy"),
            lambda value: value["cluster"]["data"].update(team_trackers=[]),
            lambda value: value["cluster"]["data"]["team_trackers"][0].update(unhealthy_servers=1),
            lambda value: value["cluster"]["configuration"].update(storage_engine="memory"),
            lambda value: value["cluster"]["latency_probe"].update(read_seconds=5),
            lambda value: value["cluster"]["latency_probe"].update(extra_probe=0),
            lambda value: value["cluster"]["qos"].update(worst_queue_bytes_storage_server=True),
            lambda value: value["cluster"]["processes"]["4510"].update(excluded=True),
            lambda value: value["cluster"]["processes"]["4510"].update(version="7.3.76"),
            lambda value: value["cluster"]["processes"]["4510"]["memory"].update(rss_bytes=True),
            lambda value: value["cluster"]["processes"]["4510"]["memory"].update(limit_bytes=2*p.GIB),
            lambda value: value["cluster"]["processes"]["4510"]["locality"].update(zoneid="other-host"),
            lambda value: value["cluster"]["processes"]["4510"].update(address="10.0.0.1:4510"),
            lambda value: value["cluster"]["processes"]["4511"]["roles"][0].update(id="role-4510-storage"),
            lambda value: value["cluster"]["processes"].pop("4511"),
        )
        for index, change in enumerate(changes):
            changed = copy.deepcopy(document); change(changed)
            with self.subTest(change=index):
                self.assertIs(self.classify(changed, layout, wave), False)

    def test_unknown_or_malformed_raw_documents_fail_closed(self):
        document, layout, wave = canonical_failure()
        for value in (None, [], {}, "status", {"client": {}}, {"cluster": {}}):
            with self.subTest(document=value):
                self.assertIs(self.classify(value, layout, wave), False)
        for value in (None, [], {}, "layout"):
            with self.subTest(layout=value):
                self.assertIs(self.classify(document, value, wave), False)


if __name__ == "__main__":
    unittest.main()
