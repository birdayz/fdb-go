#!/usr/bin/env python3
"""Offline scaling contract and shared-lease tests; no cloud or subprocesses."""
import builtins
from contextlib import contextmanager
import copy
import hashlib
import json
from pathlib import Path
import tempfile
import unittest
from unittest import mock

import diagnostic as n
import driver as d
import policy as p
import scaling as s
from test_diagnostic import plan as diagnostic_plan
from test_horizontal import evidence
from test_session import canonical_failure


def plan():
    value = diagnostic_plan()
    del value["probe_count"], value["probe_seconds"]
    value.update(schema="otel_native_scaling_v1", stages=[[1, 1], [2, 2], [3, 3]],
                 measured_count=375000, measured_seconds=120, prior_rows_upper=0,
                 batch=64, workers=8, storage_process_count=3,
                 storage_max_outstanding=64, storage_memory_profile="scaling3ss3g",
                 managed_watch={"root": "/private/native-test/watch", "output": "/private/native-test/run"})
    return value


def grant(hosts=1, count=100000, index=1):
    lease = plan(); root = "/srv/otel-horizontal/"+lease["campaign"]
    run = "prof20k"+str(9300+(hosts-1)*10+(3 if count == 375000 else 0)+index-1)
    value = {key: lease[key] for key in ("campaign", "first_epoch", "deadline_epoch", "binary_sha256",
                                       "source_revision", "batch", "workers", "gomaxprocs", "storage_options")}
    value.update(schema="otel_native_scaling_producer_v1", stage_hosts=hosts, producer_index=index,
                 role="client"+str(index), node_id=100+index, run=run, count=count,
                 independent_store=True, diagnostic_only=True,
                 diagnostic_seconds=300 if count == 100000 else 120, token="a"*64,
                 binary=root+"/bin/otel-demo", cluster_file=root+"/arms/scaling-"+str(hosts)+"/fdb.cluster",
                 work=root+"/producers/"+run)
    return value


class ScalingPlanTests(unittest.TestCase):
    def test_fixed_three_stages_exact_authorization_and_immutable_plan(self):
        value = plan(); before = copy.deepcopy(value)
        self.assertIs(s.validate_plan(value, check_assets=False), value)
        self.assertEqual(value, before)
        self.assertEqual(s.SCHEMA, "otel_native_scaling_v1")
        self.assertEqual(s.STAGES, [[1, 1], [2, 2], [3, 3]])
        self.assertEqual(s.AUTHORIZED_ROWS, 2850000)
        self.assertEqual(sum((100000+375000)*producers for _, producers in value["stages"]), 2850000)
        self.assertEqual(value["deadline_epoch"]-value["first_epoch"], 7200)

    def test_stages_are_an_exact_typed_list_not_a_sweep_or_reorder(self):
        for stages in (None, (), [], [[1, 1]], [[3, 3], [2, 2], [1, 1]],
                       [[1, 1], [2, 2], [2, 2]], [[1, 2], [2, 2], [3, 3]],
                       [[1, 1], [2, 2], [3, 3], [3, 3]], ((1, 1), (2, 2), (3, 3)),
                       [(1, 1), (2, 2), (3, 3)], [[True, 1], [2, 2], [3, 3]],
                       [[1, True], [2, 2], [3, 3]], [[1.0, 1], [2, 2], [3, 3]],
                       [[1, 1], [2, 2.0], [3, 3]]):
            value = plan(); value["stages"] = stages
            with self.subTest(stages=stages), self.assertRaises(ValueError):
                s.validate_plan(value, check_assets=False)

    def test_every_scaling_fixed_field_is_required_and_exactly_typed(self):
        fixed = {"count": 100000, "measured_count": 375000, "measured_seconds": 120,
                 "batch": 64, "workers": 8, "gomaxprocs": 4, "storage_process_count": 3,
                 "storage_max_outstanding": 64}
        for key, expected in fixed.items():
            value = plan(); del value[key]
            with self.subTest(missing=key), self.assertRaises(ValueError):
                s.validate_plan(value, check_assets=False)
            for bad in (None, True, False, 0, expected-1, expected+1, float(expected), str(expected), [], {}):
                value = plan(); value[key] = bad
                with self.subTest(key=key, bad=bad), self.assertRaises(ValueError):
                    s.validate_plan(value, check_assets=False)
        for profile in (None, "baseline", "capacity16g", True, 3, [], {}):
            value = plan(); value["storage_memory_profile"] = profile
            with self.subTest(profile=profile), self.assertRaises(ValueError):
                s.validate_plan(value, check_assets=False)
        value = plan(); del value["storage_memory_profile"]
        with self.assertRaises(ValueError):
            s.validate_plan(value, check_assets=False)

    def test_prior_rows_inclusive_ten_million_ceiling_rejects_boolean_and_zero_vacuity(self):
        for prior in (0, 1, 7150000):
            value = plan(); value["prior_rows_upper"] = prior
            self.assertIs(s.validate_plan(value, check_assets=False), value)
        for prior in (None, True, False, -1, 7150001, 0.0, 7150000.0, "0", [], {}):
            value = plan(); value["prior_rows_upper"] = prior
            with self.subTest(prior=prior), self.assertRaises(ValueError):
                s.validate_plan(value, check_assets=False)
        value = plan(); del value["prior_rows_upper"]
        with self.assertRaises(ValueError):
            s.validate_plan(value, check_assets=False)

    def test_common_approval_source_host_storage_and_absolute_lifetime_guards_remain(self):
        mutations = (("schema", "otel_horizontal_v1"), ("approved", 1), ("approved", False),
                     ("campaign", "other"), ("prefix", "../native-test"), ("location", "fsn1"),
                     ("server_type", "ccx33"), ("replication", "double"), ("source_revision", "b"*39),
                     ("first_epoch", True), ("deadline_epoch", 8201), ("controller_cidr", "192.0.2.0/24"),
                     ("hcloud", "hcloud"), ("binary_sha256", "changed"),
                     ("storage_options", {"record_compression": "brotli-4"}))
        for key, bad in mutations:
            value = plan(); value[key] = bad
            with self.subTest(key=key, bad=bad), self.assertRaises(ValueError):
                s.validate_plan(value, check_assets=False)

    def test_explicit_foreground_mode_is_valid_without_managed_configuration(self):
        value = plan(); del value["managed_watch"]; value["foreground_only"] = True
        before = copy.deepcopy(value)
        with mock.patch.object(s.supervisor, "validate_config", side_effect=AssertionError("foreground must not consult managed mode")):
            self.assertIs(s.validate_plan(value, check_assets=False), value)
        self.assertEqual(value, before)
        self.assertEqual(p.scaling_execution_mode(value), "foreground_only")

    def test_execution_mode_has_no_implicit_fallback_or_untyped_foreground_value(self):
        base = plan(); del base["managed_watch"]
        values = [base, dict(plan(), foreground_only=True), dict(plan(), foreground_only=False)]
        values += [dict(base, foreground_only=bad) for bad in (False, 1, 0, 1.0, 0.0, None, "true", [], {})]
        for value in values:
            with self.subTest(value=value), self.assertRaises(ValueError):
                s.validate_plan(value, check_assets=False)

    def test_managed_watch_configuration_is_required_and_uses_the_pure_supervisor_validator(self):
        value = plan(); del value["managed_watch"]
        with self.assertRaises(ValueError):
            s.validate_plan(value, check_assets=False)
        value = plan()
        with mock.patch.object(s.supervisor, "validate_config", side_effect=ValueError("managed watch refused")) as validate:
            with self.assertRaisesRegex(ValueError, "managed watch refused"):
                s.validate_plan(value, check_assets=False)
        validate.assert_called_once_with(value)

    def test_asset_projection_checks_hashes_and_refuses_symlinks_without_plan_mutation(self):
        with tempfile.TemporaryDirectory() as tmp:
            value = plan(); root = Path(tmp)
            for key in p.ASSETS:
                asset = root/key; asset.write_text("sealed-"+key)
                value[key] = str(asset)
                value["input_manifest_sha256" if key == "source_manifest" else key+"_sha256"] = p.sha(asset)
            before = copy.deepcopy(value)
            self.assertIs(s.validate_plan(value), value)
            self.assertEqual(value, before)
            binary = Path(value["binary"]); binary.write_text("changed")
            with self.assertRaisesRegex(ValueError, "changed asset"):
                s.validate_plan(value)
            binary.write_text("sealed-binary")
            target = root/"target"; target.write_bytes(binary.read_bytes()); binary.unlink(); binary.symlink_to(target)
            with self.assertRaisesRegex(ValueError, "changed asset"):
                s.validate_plan(value)


class ScalingGrantTests(unittest.TestCase):
    def test_all_twelve_producer_grants_have_distinct_fixed_names_and_preserve_lease(self):
        names = []
        for hosts in (1, 2, 3):
            for count, seconds in ((100000, 300), (375000, 120)):
                for index in range(1, hosts+1):
                    value = grant(hosts, count, index); before = copy.deepcopy(value)
                    with self.subTest(hosts=hosts, count=count, index=index):
                        self.assertIs(s.producer_grant(value), value)
                        self.assertEqual(value, before)
                        self.assertEqual(value["role"], "client"+str(index))
                        self.assertEqual(value["diagnostic_seconds"], seconds)
                        self.assertEqual((value["first_epoch"], value["deadline_epoch"]), (1000, 8200))
                    names.append(value["run"])
        self.assertEqual(names, ["prof20k9300", "prof20k9303", "prof20k9310", "prof20k9311",
                                 "prof20k9313", "prof20k9314", "prof20k9320", "prof20k9321",
                                 "prof20k9322", "prof20k9323", "prof20k9324", "prof20k9325"])
        self.assertEqual(len(set(names)), 12)
        for hosts in (1, 2, 3):
            for count in (100000, 375000):
                for index in range(1, hosts+1):
                    self.assertEqual(s.run_name(hosts, count, index), grant(hosts, count, index)["run"])

    def test_stage_index_count_role_name_and_duration_cannot_be_reassigned(self):
        mutations = (("schema", "otel_horizontal_producer_v1"), ("stage_hosts", 0), ("stage_hosts", 4),
                     ("stage_hosts", True), ("stage_hosts", 1.0), ("producer_index", 0),
                     ("producer_index", 2), ("producer_index", True), ("producer_index", 1.0),
                     ("role", "node1"), ("role", "client2"), ("run", "prof20k9301"),
                     ("run", "prof20k9303"), ("count", 0), ("count", True), ("count", 375000.0),
                     ("count", 1000000), ("diagnostic_seconds", 120), ("diagnostic_seconds", True),
                     ("diagnostic_seconds", 300.0), ("batch", 32), ("workers", 4), ("gomaxprocs", 8))
        for key, bad in mutations:
            value = grant(); value[key] = bad
            with self.subTest(key=key, bad=bad), self.assertRaises(ValueError):
                s.producer_grant(value)
        for hosts in (1, 2, 3):
            value = grant(hosts, 375000); value["diagnostic_seconds"] = 300
            with self.subTest(hosts=hosts), self.assertRaises(ValueError):
                s.producer_grant(value)
        for key in ("stage_hosts", "producer_index", "role", "run", "count", "diagnostic_seconds"):
            value = grant(); del value[key]
            with self.subTest(missing=key), self.assertRaises(ValueError):
                s.producer_grant(value)

    def test_common_independent_store_binary_namespace_and_clock_guards_remain(self):
        mutations = (("independent_store", False), ("independent_store", 1), ("diagnostic_only", False),
                     ("token", "bad"), ("node_id", True), ("binary_sha256", "bad"),
                     ("source_revision", "bad"), ("deadline_epoch", 8201),
                     ("binary", "/foreign/otel-demo"), ("cluster_file", "/foreign/fdb.cluster"),
                     ("work", "/srv/otel-horizontal/native-test/../foreign"), ("storage_options", {}))
        for key, bad in mutations:
            value = grant(); value[key] = bad
            with self.subTest(key=key, bad=bad), self.assertRaises(ValueError):
                s.producer_grant(value)


def sample(queue=0, lag=0):
    return {"healthy": True, "queue_bytes": queue, "lag_seconds": lag}


def wave_result(hosts, count):
    return {"count_per_producer": count, "producers": hosts, "rows": count*hosts,
            "healthy": True, "ingest_valid": True, "census_pass": True, "oracle_pass": True,
            "measurement_eligible": True, "ack_rows_per_second": 1500*hosts,
            "drained_rows_per_second": 1000*hosts}


def ingest_diagnostics(count):
    return {"committed_rows": count, "committed_batches": 1,
            "phase_latencies": {phase: {"count": 1, "min_nanos": 1, "max_nanos": 1, "total_nanos": 1}
                                for phase in ("open", "save", "commit", "run")}}


def stage_result(hosts):
    return {"schema": "otel_native_scaling_stage_v1", "prerequisite": wave_result(hosts, 100000),
            "measured": wave_result(hosts, 375000)}


def records():
    return [{"hosts": hosts, "producers": hosts, "status": "PASS", **stage_result(hosts)}
            for hosts in (1, 2, 3)]


class ScalingMetricsTests(unittest.TestCase):
    def metrics(self, *, rows=375000, start=1000000000, ack=11000000000, drained=16000000000,
                load=None, drain=None):
        return s.wave_metrics(rows, start, ack, drained,
                              [sample(100, 1), sample(200, 4)] if load is None else load,
                              [sample(500, 5), sample(9, .3), sample(5, .2), sample(0, .1)] if drain is None else drain)

    def test_ack_and_drain_rates_use_distinct_common_windows_and_all_samples(self):
        value = self.metrics()
        self.assertEqual(value["rows"], 375000)
        self.assertEqual(value["controller_started_ns"], 1000000000)
        self.assertEqual(value["controller_ack_end_ns"], 11000000000)
        self.assertEqual(value["controller_drained_end_ns"], 16000000000)
        self.assertEqual(value["ack_rows_per_second"], 37500)
        self.assertEqual(value["drained_rows_per_second"], 25000)
        self.assertEqual(value["drain_seconds"], 5)
        self.assertEqual(value["queue"], {"start": 100, "end": 0, "max": 500})
        self.assertEqual(value["lag"], {"start": 1, "end": .1, "max": 5})
        self.assertTrue(value["timing_boundary"])
        value = self.metrics(drained=11000000000)
        self.assertEqual(value["drain_seconds"], 0)
        self.assertEqual(value["ack_rows_per_second"], value["drained_rows_per_second"])

    def test_empty_rows_boolean_nan_and_nonpositive_or_reversed_clocks_fail_closed(self):
        for rows in (0, -1, True, False, 375000.0, "375000", None):
            with self.subTest(rows=rows), self.assertRaises(ValueError):
                self.metrics(rows=rows)
        for key in ("start", "ack", "drained"):
            for bad in (0, -1, True, False, 1.0, float("nan"), float("inf"), "1", None):
                with self.subTest(key=key, bad=bad), self.assertRaises(ValueError):
                    self.metrics(**{key: bad})
        for clocks in ((1, 1, 2), (2, 1, 3), (1, 3, 2)):
            with self.subTest(clocks=clocks), self.assertRaises(ValueError):
                self.metrics(start=clocks[0], ack=clocks[1], drained=clocks[2])

    def test_load_and_drain_health_populations_are_nonempty_and_exactly_typed(self):
        for load in ([], {}, "samples", [None], [{}]):
            with self.subTest(load=load), self.assertRaises(ValueError):
                self.metrics(load=load)
        with self.assertRaises(ValueError):
            s.wave_metrics(1, 1, 2, 3, None, [sample(), sample(), sample()])
        with self.assertRaises(ValueError):
            s.wave_metrics(1, 1, 2, 3, [sample()], None)
        for drain in ([], {}, "samples", [sample()], [sample(), sample()], [None, None, None]):
            with self.subTest(drain=drain), self.assertRaises(ValueError):
                self.metrics(drain=drain)
        for phase in ("load", "drain"):
            for key, bad in (("healthy", 1), ("healthy", None),
                             ("queue_bytes", True), ("queue_bytes", 0.0), ("queue_bytes", -1),
                             ("queue_bytes", None), ("lag_seconds", True), ("lag_seconds", -1),
                             ("lag_seconds", float("nan")), ("lag_seconds", float("inf")),
                             ("lag_seconds", None)):
                values = [sample(), sample(), sample()]
                values[0][key] = bad
                with self.subTest(phase=phase, key=key, bad=bad), self.assertRaises(ValueError):
                    self.metrics(**{phase: values})
            for key in ("healthy", "queue_bytes", "lag_seconds"):
                values = [sample(), sample(), sample()]; del values[0][key]
                with self.subTest(phase=phase, missing=key), self.assertRaises(ValueError):
                    self.metrics(**{phase: values})
        transient_load = [sample(100, 1), {**sample(200, 4), "healthy": False}]
        transient_drain = [{**sample(500, 12), "healthy": False}, sample(9, .3), sample(5, .2), sample(0, .1)]
        metrics = self.metrics(load=transient_load, drain=transient_drain)
        self.assertEqual(metrics["transient_unhealthy_samples"], 2)
        self.assertEqual(metrics["golden_signals"]["saturation"]["transient_unhealthy_samples"], 2)
        for index in (1, 2, 3):
            bad_drain = [sample(500, 5), sample(9, .3), sample(5, .2), sample(0, .1)]
            bad_drain[index]["healthy"] = False
            with self.subTest(final_drain_index=index), self.assertRaises(ValueError):
                self.metrics(drain=bad_drain)

    def test_every_one_of_last_three_drain_samples_must_be_below_both_strict_thresholds(self):
        for index in (0, 1, 2):
            for key, bad in (("queue_bytes", 16*1024**2), ("lag_seconds", 10)):
                values = [sample(), sample(), sample()]; values[index][key] = bad
                with self.subTest(index=index, key=key), self.assertRaises(ValueError):
                    self.metrics(drain=values)
        values = [sample(16*1024**2, 10), sample(), sample(), sample()]
        self.assertEqual(self.metrics(drain=values)["queue"]["max"], 16*1024**2)


class ScalingSummaryTests(unittest.TestCase):
    def test_primary_efficiency_is_drained_rate_not_ack_and_threshold_is_inclusive(self):
        values = records(); before = copy.deepcopy(values)
        result = s.summarize_scaling(values)
        self.assertEqual(result["verdict"], "NEAR_LINEAR")
        self.assertIs(result["near_linear"], True)
        self.assertEqual(result["efficiency_threshold"], .90)
        self.assertEqual(result["primary_metric"], "drained_rows_per_second")
        self.assertEqual([(x["hosts"], x["scale_factor"], x["efficiency"], x["ack_scale_factor"])
                          for x in result["stages"]], [(1, 1, 1, 1), (2, 2, 1, 2), (3, 3, 1, 3)])
        self.assertEqual(result["reasons"], [])
        self.assertIn("single-replica", result["boundary"])
        self.assertIn("no shared-corpus", result["boundary"])
        self.assertIn("statistical repeatability or sustained-120-second 20k certification", result["boundary"])
        self.assertEqual(values, before)
        for hosts in (2, 3):
            values[hosts-1]["measured"]["drained_rows_per_second"] = 900*hosts
            values[hosts-1]["measured"]["ack_rows_per_second"] = 900*hosts
        self.assertEqual(s.summarize_scaling(values)["verdict"], "NEAR_LINEAR")
        values[2]["measured"]["drained_rows_per_second"] = 2699
        values[2]["measured"]["ack_rows_per_second"] = 1000000
        result = s.summarize_scaling(values)
        self.assertEqual(result["verdict"], "BELOW_NEAR_LINEAR")
        self.assertIs(result["near_linear"], False)
        self.assertAlmostEqual(result["stages"][2]["efficiency"], 2699/3000)

    def test_no_linearity_inference_without_all_three_passing_stages_and_valid_one_x_baseline(self):
        populations = ([], records()[:1], records()[:2], records()+[records()[2]],
                       [records()[0], records()[0], records()[2]])
        for values in populations:
            with self.subTest(hosts=[x["hosts"] for x in values]):
                result = s.summarize_scaling(values)
                self.assertEqual(result["verdict"], "INCONCLUSIVE")
                self.assertIs(result["near_linear"], False)
                self.assertTrue(result["reasons"])
        for index in (0, 1, 2):
            for status in ("ARM_FAILURE", "INCONCLUSIVE", "SESSION_FATAL", None, True):
                values = records(); values[index]["status"] = status
                with self.subTest(index=index, status=status):
                    result = s.summarize_scaling(values)
                    self.assertEqual(result["verdict"], "INCONCLUSIVE")
                    self.assertIs(result["near_linear"], False)

    def test_boolean_zero_wrong_rows_and_untyped_rates_never_create_a_baseline_or_comparison(self):
        mutations = [("rows", bad) for bad in (0, True, 375000.0, 375001)]
        mutations += [("count_per_producer", bad) for bad in (0, True, 375000.0, 100000)]
        mutations += [(key, bad) for key in ("healthy", "ingest_valid", "census_pass", "oracle_pass", "measurement_eligible")
                      for bad in (False, 1, None)]
        mutations += [(key, bad) for key in ("ack_rows_per_second", "drained_rows_per_second")
                      for bad in (0, -1, True, None, float("nan"), float("inf"), "1000")]
        for index in (0, 1, 2):
            for key, bad in mutations:
                values = records(); values[index]["measured"][key] = bad
                with self.subTest(index=index, key=key, bad=bad):
                    result = s.summarize_scaling(values)
                    self.assertEqual(result["verdict"], "INCONCLUSIVE")
                    self.assertIs(result["near_linear"], False)
                    self.assertTrue(result["reasons"])
            for key in ("hosts", "producers"):
                for bad in (0, True, float(index+1), "1", None):
                    values = records(); values[index][key] = bad
                    with self.subTest(index=index, key=key, bad=bad):
                        self.assertEqual(s.summarize_scaling(values)["verdict"], "INCONCLUSIVE")


class ScalingBudgetTests(unittest.TestCase):
    def test_six_wave_reservation_carries_unused_allocation_forward_without_reset(self):
        remaining = 16*1024**2; spent = 0
        self.assertEqual(s.wave_budget(remaining, 6), 2796202)
        for waves in range(6, 0, -1):
            quota = s.wave_budget(remaining, waves)
            self.assertEqual(quota, remaining//waves)
            used = quota//2
            spent += used; remaining -= used
            self.assertEqual(remaining+spent, 16*1024**2)
        self.assertEqual(s.wave_budget(remaining, 1), remaining)
        self.assertEqual(s.wave_budget(0, 6), 0)
        self.assertEqual(s.wave_budget(1, 6), 0)

    def test_wave_budget_refuses_boolean_untyped_negative_or_inflated_remaining(self):
        for remaining in (None, True, False, -1, 16*1024**2+1, 1.0, "1"):
            with self.subTest(remaining=remaining), self.assertRaises(ValueError):
                s.wave_budget(remaining, 6)
        for waves in (None, True, False, -1, 0, 7, 6.0, "6"):
            with self.subTest(waves=waves), self.assertRaises(ValueError):
                s.wave_budget(16*1024**2, waves)


class FakeStage:
    def __init__(self, harness, index, lease, out, provider, *, stage_hosts, **kwargs):
        self.harness, self.index, self.plan, self.out = harness, index, lease, Path(out)
        self.provider, self.hosts, self.kwargs = provider, stage_hosts, kwargs
        self.nodes, self.layouts = {}, []
        self.trace_remaining, self.observation_history = n.TRACE_BUDGET, {}
        self.last_status_document, self.last_wave_result = None, None
        self.scopes = [{"node": "client"+str(i), "unit": "fake-"+str(stage_hosts)+"-"+str(i)}
                       for i in range(1, stage_hosts+1)]
        self.entry_budget, self.entry_history = None, None

    def run_existing(self, resources, *, helpers_ready=False):
        h = self.harness
        self.entry_budget, self.entry_history = self.trace_remaining, dict(self.observation_history)
        self.nodes = resources["nodes"]
        h.events.extend([("helpers", self.hosts, helpers_ready), ("quiet", self.hosts)])
        self.layouts.append({"schema": s.SCHEMA, "root": "/owned/scaling-"+str(self.hosts),
                             "storage_hosts": ["node"+str(i) for i in range(1, self.hosts+1)],
                             "storage_process_count": 3, "storage_max_outstanding": 64,
                             "storage_memory_profile": "scaling3ss3g"})
        h.events.extend([("cluster", self.hosts), ("prerequisite", self.hosts)])
        self.trace_remaining -= h.consumption[self.index]
        self.observation_history["stage-"+str(self.hosts)] = {"sequence": self.index, "pswpin": 0, "pswpout": 0}
        if h.outcomes[self.index] is not None:
            self.last_status_document = {"literal_failed_stage": self.hosts}
            self.last_wave_result = {"observation_phase": "load", "release_bound": True,
                                     "profile": {"producer_process_bound": True}, "error": str(h.outcomes[self.index])}
            raise h.outcomes[self.index]
        h.events.append(("measured", self.hosts))
        completed = copy.deepcopy(h.completed[self.index])
        d.write(self.out/"stage-result.json", completed)
        if h.after_run is not None:
            h.after_run(self)
        return completed

    def host_samples(self):
        self.harness.events.append(("fresh-hosts", self.hosts))
        if self.harness.host_error is not None:
            raise self.harness.host_error
        return {role: {"fdb_pids": {"200": "5000"} if role.startswith("node") else {}}
                for role in self.nodes}

    def scope_bindings(self):
        return self.scopes


class Harness:
    def __init__(self, root, *, outcomes=(None, None, None), consumption=(11, 17, 23)):
        self.plan, self.events, self.arms = plan(), [], []
        self.outcomes, self.consumption = outcomes, consumption
        self.completed = [stage_result(hosts) for hosts in (1, 2, 3)]
        self.after_run = None
        self.host_error, self.stop_error, self.cleanup_error, self.prepare_error = None, None, None, None
        self.watch_error = None
        self.stop_result = {"passed": True, "quiescent": True}
        self.cleanup_result = {"passed": True, "protected_baseline_restored": True}
        self.out = Path(root)/"campaign"/"run"
        self.plan["managed_watch"] = {"root": str(self.out.parent/"watch"), "output": str(self.out)}
        assets = Path(root)/"assets"; assets.mkdir()
        for key in p.ASSETS:
            path = assets/key; path.write_text("sealed-"+key)
            self.plan[key] = str(path)
            self.plan["input_manifest_sha256" if key == "source_manifest" else key+"_sha256"] = p.sha(path)
        roles = ("node1", "node2", "node3", "client1", "client2", "client3")
        self.resources = {"prepared": True, "campaign": self.plan["campaign"], "prefix": self.plan["prefix"],
                          "ids": {"server": list(range(101, 107))}, "protected_ids": {"server": [1]},
                          "nodes": {role: {"id": 101+i} for i, role in enumerate(roles)}}
        self.provider = mock.Mock()
        for method in ("run_on_node", "setup_cluster", "create", "delete"):
            getattr(self.provider, method).side_effect = AssertionError("real provider operation forbidden")
        self.provider.prepare.side_effect = self.prepare
        self.provider.stop_owned_arm.side_effect = self.stop
        self.provider.teardown.side_effect = self.teardown
        self.session = s.ScalingSession(self.plan, self.out, self.provider, arm_factory=self.arm_factory)

    def register(self, lease, out):
        self.events.append(("register", lease, out))
        if self.watch_error is not None:
            raise self.watch_error
        return {"registered": True}

    def prepare(self, lease, out):
        self.events.append(("prepare", lease, out))
        if self.prepare_error is not None:
            raise self.prepare_error
        return self.resources

    def arm_factory(self, lease, out, provider, **kwargs):
        arm = FakeStage(self, len(self.arms), lease, out, provider, **kwargs)
        self.arms.append(arm)
        return arm

    def stop(self, lease, out, nodes, layout, *, scopes):
        self.events.append(("stop", len(layout["storage_hosts"])))
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


class ScalingSessionTests(unittest.TestCase):
    def setUp(self):
        for name in ("run", "Popen"):
            guard = mock.patch.object(n.subprocess, name, side_effect=AssertionError("real subprocess forbidden"))
            guard.start(); self.addCleanup(guard.stop)

    @contextmanager
    def running(self, harness):
        with mock.patch.object(p, "verify_source", side_effect=harness.verify_source) as source, \
             mock.patch.object(d, "remaining", return_value=30) as remaining, \
             mock.patch.object(s.supervisor, "register", side_effect=harness.register):
            yield source, remaining

    def test_foreground_mode_never_registers_and_finally_cleans_after_success_or_native_fault(self):
        for error in (None, ValueError("native PID vanished")):
            with self.subTest(error=error), tempfile.TemporaryDirectory() as tmp:
                h = Harness(tmp, outcomes=(error, None, None))
                del h.plan["managed_watch"]; h.plan["foreground_only"] = True
                with self.running(h), \
                     mock.patch.object(s.supervisor, "register", side_effect=AssertionError("foreground registered managed watch")) as register, \
                     mock.patch.object(s.supervisor, "admit", side_effect=AssertionError("foreground requested managed admission")) as admit:
                    if error is None:
                        result = h.session.run()
                        self.assertTrue(result["all_stages_passed"])
                    else:
                        with self.assertRaisesRegex(ValueError, "native PID vanished"):
                            h.session.run()
                    register.assert_not_called(); admit.assert_not_called()
                result = d.read(h.out/"scaling-result.json")
                self.assertEqual(result["execution_mode"], "bounded_foreground")
                self.assertIs(result["foreground_only"], True)
                h.provider.teardown.assert_called_once_with(h.plan, h.out/"provision", h.resources)
                self.assertTrue((h.out/"source-after.json").is_file())
                self.assertFalse(any(event[0] == "register" for event in h.events))

    def assert_finalized(self, h, *, source_calls=5):
        h.provider.prepare.assert_called_once_with(h.plan, h.out/"provision")
        self.assertEqual([event[1:] for event in h.events if event[0] == "register"], [(h.plan, h.out)])
        h.provider.teardown.assert_called_once_with(h.plan, h.out/"provision", h.resources)
        self.assertEqual(sum(event[0] == "source" for event in h.events), source_calls)
        self.assertEqual([event[0] for event in h.events[-2:]], ["cleanup", "source"])
        self.assertEqual(d.read(h.out/"source-after.json")["source_files"], 1)
        self.assertEqual(d.read(h.out/"approved-plan.json"), h.plan)

    def assert_fatal(self, h, *, arms=1, source_calls=3):
        self.assert_finalized(h, source_calls=source_calls)
        self.assertEqual(len(h.arms), arms)
        result = d.read(h.out/"scaling-result.json")
        self.assertEqual(result["status"], "SESSION_FATAL")
        self.assertIs(result["all_stages_passed"], False)
        if result["stages"]:
            self.assertEqual(result["stages"][-1]["status"], "SESSION_FATAL")
        return result

    def test_prepare_once_six_owned_nodes_one_immutable_lease_three_quiet_stages_one_cleanup(self):
        with tempfile.TemporaryDirectory() as tmp:
            h = Harness(tmp); before = copy.deepcopy(h.plan)
            with self.running(h):
                result = h.session.run()
            self.assert_finalized(h)
            self.assertEqual(h.plan, before)
            self.assertEqual(result["schema"], "otel_native_scaling_result_v1")
            self.assertIs(result["all_stages_passed"], True)
            self.assertIs(result["cleanup_passed"], True)
            self.assertEqual(result["failed_stages"], [])
            self.assertEqual([(x["hosts"], x["producers"], x["status"]) for x in result["stages"]],
                             [(1, 1, "PASS"), (2, 2, "PASS"), (3, 3, "PASS")])
            self.assertEqual(result["authorized_rows"], 2850000)
            self.assertEqual(result["cumulative_rows_upper"], 2850000)
            self.assertEqual(d.read(h.out/"scaling-result.json"), result)
            self.assertEqual(h.provider.stop_owned_arm.call_count, 3)
            self.assertEqual([event[0] for event in h.events], ["source", "register", "prepare"]+
                             [name for _ in range(3) for name in ("helpers", "quiet", "cluster", "prerequisite", "measured", "fresh-hosts", "source", "stop")]+
                             ["cleanup", "source"])
            self.assertEqual([event for event in h.events if event[0] == "helpers"],
                             [("helpers", 1, False), ("helpers", 2, True), ("helpers", 3, True)])
            self.assertEqual(len({arm.out for arm in h.arms}), 3)
            for index, arm in enumerate(h.arms):
                self.assertEqual(arm.plan, before)
                self.assertIs(arm.provider, h.provider)
                self.assertIs(arm.nodes, h.resources["nodes"])
                self.assertEqual(arm.hosts, index+1)
                self.assertEqual(arm.out, h.out/"stages"/("pairs"+str(index+1)))
                self.assertEqual(d.read(arm.out/"session-stage.json"), result["stages"][index])
                self.assertEqual(arm.kwargs, {"provision_out": h.out/"provision", "receipt_root": h.out})
                stop = h.provider.stop_owned_arm.call_args_list[index]
                self.assertEqual(stop.args, (h.plan, h.out/"provision", h.resources["nodes"], arm.layouts[-1]))
                self.assertEqual(stop.kwargs, {"scopes": arm.scopes})

    def test_trace_reservation_and_host_history_carry_forward_across_all_stages_by_identity(self):
        for consumption in ((11, 17, 23), (n.TRACE_BUDGET, 0, 0)):
            with self.subTest(consumption=consumption), tempfile.TemporaryDirectory() as tmp:
                h = Harness(tmp, consumption=consumption)
                with self.running(h):
                    result = h.session.run()
                for index, arm in enumerate(h.arms):
                    self.assertEqual(arm.entry_budget, 16*1024**2-sum(consumption[:index]))
                    self.assertEqual(set(arm.entry_history), {"stage-"+str(i) for i in range(1, index+1)})
                    self.assertIs(arm.observation_history, h.session.observation_history)
                self.assertEqual(result["trace_bytes_remaining"], 16*1024**2-sum(consumption))
                self.assertEqual([x["trace_bytes_remaining"] for x in result["stages"]],
                                 [16*1024**2-sum(consumption[:i]) for i in (1, 2, 3)])

    def test_only_classified_stage_failure_can_continue_after_fresh_fatal_guards_and_quiescence(self):
        for index in (0, 1, 2):
            with self.subTest(stage=index+1), tempfile.TemporaryDirectory() as tmp:
                errors = [None, None, None]; errors[index] = ValueError("strict client health")
                h = Harness(tmp, outcomes=errors)
                with self.running(h), mock.patch.object(s, "classify_stage_failure", return_value=True) as classify:
                    result = h.session.run()
                self.assert_finalized(h, source_calls=6)
                self.assertIs(result["all_stages_passed"], False)
                self.assertEqual([x["status"] for x in result["stages"]],
                                 ["ARM_FAILURE" if i == index else "PASS" for i in range(3)])
                self.assertEqual(len(h.arms), 3)
                self.assertEqual(h.provider.stop_owned_arm.call_count, 3)
                classify.assert_called_once_with(errors[index], h.arms[index].last_status_document,
                                                 h.arms[index].layouts[-1], h.arms[index].last_wave_result,
                                                 fatal_guards_passed=True)
                self.assertNotIn(("measured", index+1), h.events)
                failure = result["stages"][index]
                self.assertNotIn("measured", failure)

    def test_unknown_or_wrong_typed_exception_is_fatal_and_never_starts_next_stage(self):
        for error in (ValueError("unknown warning"), OSError("SSH transport failed"),
                      RuntimeError("strict client health"), KeyError("status missing")):
            with self.subTest(error=error), tempfile.TemporaryDirectory() as tmp:
                h = Harness(tmp, outcomes=(error, None, None))
                with self.running(h), mock.patch.object(s, "classify_stage_failure", return_value=False):
                    with self.assertRaises(type(error)):
                        h.session.run()
                self.assert_fatal(h)
                h.provider.stop_owned_arm.assert_not_called()

    def test_native_host_fault_is_always_fatal_even_after_a_classifiable_stage_failure(self):
        for error in (None, ValueError("strict client health")):
            for reason in ("host swap activity", "unchanged native cgroup budget/OOM", "FDB process PID/starttime changed"):
                with self.subTest(error=error, reason=reason), tempfile.TemporaryDirectory() as tmp:
                    h = Harness(tmp, outcomes=(error, None, None)); h.host_error = ValueError(reason)
                    with self.running(h), mock.patch.object(s, "classify_stage_failure", return_value=True) as classify:
                        with self.assertRaisesRegex(ValueError, reason):
                            h.session.run()
                    self.assert_fatal(h, source_calls=2)
                    classify.assert_not_called()
                    h.provider.stop_owned_arm.assert_not_called()

    def test_fresh_source_or_absolute_deadline_failure_prevents_classifier_and_stage_reuse(self):
        for error in (None, ValueError("strict client health")):
            for guard in ("source", "deadline"):
                with self.subTest(error=error, guard=guard), tempfile.TemporaryDirectory() as tmp:
                    h = Harness(tmp, outcomes=(error, None, None))
                    with self.running(h) as (source, remaining), \
                         mock.patch.object(s, "classify_stage_failure", return_value=True) as classify:
                        if guard == "source":
                            def verify(lease):
                                result = h.verify_source(lease)
                                if source.call_count == 2:
                                    raise ValueError("changed source file")
                                return result
                            source.side_effect = verify
                        else:
                            remaining.side_effect = [30, ValueError("controller budget exhausted")]
                        with self.assertRaisesRegex(ValueError, "changed source|budget exhausted"):
                            h.session.run()
                    self.assert_fatal(h)
                    classify.assert_not_called()
                    h.provider.stop_owned_arm.assert_not_called()

    def test_owned_stage_quiescence_is_required_before_any_next_stage(self):
        receipts = (None, {}, {"passed": False, "quiescent": False}, {"passed": True},
                    {"passed": 1, "quiescent": True}, {"passed": True, "quiescent": 1})
        for error in (None, ValueError("strict client health")):
            for receipt in receipts:
                with self.subTest(error=error, receipt=receipt), tempfile.TemporaryDirectory() as tmp:
                    h = Harness(tmp, outcomes=(error, None, None)); h.stop_result = receipt
                    with self.running(h), mock.patch.object(s, "classify_stage_failure", return_value=True):
                        with self.assertRaisesRegex(ValueError, "quiescence"):
                            h.session.run()
                    self.assert_fatal(h, source_calls=4 if error is not None else 3)
                    self.assertEqual(h.provider.stop_owned_arm.call_count, 1)
        with tempfile.TemporaryDirectory() as tmp:
            h = Harness(tmp); h.stop_error = OSError("owned-stage stop failed")
            with self.running(h), self.assertRaisesRegex(OSError, "owned-stage stop failed"):
                h.session.run()
            self.assert_fatal(h)

    def test_source_after_is_checked_even_when_final_cleanup_returns_false_or_raises(self):
        for failure in ("false", "untyped", "exception"):
            with self.subTest(failure=failure), tempfile.TemporaryDirectory() as tmp:
                h = Harness(tmp)
                if failure == "false":
                    h.cleanup_result = {"passed": False, "external_ids_missing": [999]}
                elif failure == "untyped":
                    h.cleanup_result = {"passed": 1}
                else:
                    h.cleanup_error = OSError("cleanup command failed")
                with self.running(h), self.assertRaisesRegex((ValueError, OSError), "cleanup"):
                    h.session.run()
                self.assert_finalized(h)
                result = d.read(h.out/"scaling-result.json")
                self.assertEqual(result["status"], "SESSION_FATAL")
                self.assertIs(result["cleanup_passed"], False)
                self.assertIs(result["all_stages_passed"], False)

    def test_budget_increase_boolean_negative_or_reset_history_is_fatal_and_still_finalizes(self):
        for bad in (n.TRACE_BUDGET+1, -1, True, 1.0, None):
            with self.subTest(remaining=bad), tempfile.TemporaryDirectory() as tmp:
                h = Harness(tmp)
                h.after_run = lambda arm: setattr(arm, "trace_remaining", bad)
                with self.running(h), self.assertRaisesRegex(ValueError, "forensic budget"):
                    h.session.run()
                self.assert_fatal(h)
        for replacement in ("empty", "equal-copy"):
            with self.subTest(history=replacement), tempfile.TemporaryDirectory() as tmp:
                h = Harness(tmp); identity = h.session.observation_history
                def reset(arm):
                    arm.observation_history = {} if replacement == "empty" else dict(arm.observation_history)
                h.after_run = reset
                with self.running(h), self.assertRaisesRegex(ValueError, "observation history reset"):
                    h.session.run()
                self.assert_fatal(h)
                self.assertIs(h.session.observation_history, identity)
                self.assertIsNot(h.arms[0].observation_history, identity)

    def test_only_exact_six_new_distinct_unprotected_owned_node_ids_reach_a_stage(self):
        changes = (lambda r: r.update(prepared=False), lambda r: r.update(prepared=1),
                   lambda r: r.update(campaign="other-test"), lambda r: r.update(prefix="other-test"),
                   lambda r: r["nodes"].pop("client3"),
                   lambda r: r["nodes"].update(node4={"id": 107}),
                   lambda r: r["nodes"]["client3"].update(id=101),
                   lambda r: r["nodes"]["client3"].update(id=True),
                   lambda r: r["protected_ids"].update(server=[101]),
                   lambda r: r["ids"].update(server=[101, 102, 103, 104, 105, 107]))
        for index, change in enumerate(changes):
            with self.subTest(change=index), tempfile.TemporaryDirectory() as tmp:
                h = Harness(tmp); change(h.resources)
                with self.running(h), self.assertRaises(ValueError):
                    h.session.run()
                self.assert_fatal(h, arms=0, source_calls=2)
                h.provider.stop_owned_arm.assert_not_called()

    def test_partial_wrong_population_or_false_gate_result_is_never_promoted_to_pass(self):
        invalid = [None, {}, {"schema": "otel_native_scaling_stage_v1"}]
        for phase in ("prerequisite", "measured"):
            value = stage_result(1); del value[phase]; invalid.append(value)
            for key, bad in (("count_per_producer", True), ("count_per_producer", 100000.0),
                             ("count_per_producer", 99999), ("producers", True), ("producers", 1.0),
                             ("producers", 2), ("rows", 0), ("rows", True), ("rows", 100000.0),
                             ("rows", 99999), ("healthy", False), ("healthy", 1),
                             ("ingest_valid", False), ("census_pass", False), ("oracle_pass", False)):
                value = stage_result(1); value[phase][key] = bad; invalid.append(value)
        for index, value in enumerate(invalid):
            with self.subTest(result=index), tempfile.TemporaryDirectory() as tmp:
                h = Harness(tmp); h.completed[0] = value
                with self.running(h), self.assertRaises(ValueError):
                    h.session.run()
                self.assert_fatal(h)
                h.provider.stop_owned_arm.assert_not_called()

    def test_valid_but_forensically_ineligible_measurement_stays_inconclusive_and_never_proves_scaling(self):
        for eligible in (False, None, 0, 1):
            with self.subTest(eligible=eligible), tempfile.TemporaryDirectory() as tmp:
                h = Harness(tmp); h.completed[1]["measured"]["measurement_eligible"] = eligible
                with self.running(h):
                    result = h.session.run()
                self.assert_finalized(h)
                self.assertEqual([x["status"] for x in result["stages"]], ["PASS", "INCONCLUSIVE", "PASS"])
                self.assertEqual(result["failed_stages"], [2])
                self.assertEqual(result["status"], "COMPLETED_WITH_NONPASSING_STAGES")
                self.assertIs(result["all_stages_passed"], False)
                self.assertEqual(result["scaling"]["verdict"], "INCONCLUSIVE")
                self.assertIs(result["accepted_throughput"], False)
                self.assertEqual(h.provider.stop_owned_arm.call_count, 3)

    def test_managed_watch_registration_failure_is_fatal_before_provision_and_still_checks_source_after(self):
        for error in (ValueError("managed watch acknowledgement missing"), OSError("managed watch unavailable")):
            with self.subTest(error=error), tempfile.TemporaryDirectory() as tmp:
                h = Harness(tmp); h.watch_error = error
                with self.running(h), self.assertRaisesRegex(type(error), "managed watch"):
                    h.session.run()
                h.provider.prepare.assert_not_called(); h.provider.teardown.assert_not_called()
                self.assertEqual(h.arms, [])
                self.assertEqual([event[0] for event in h.events], ["source", "register", "source"])
                self.assertEqual(d.read(h.out/"source-after.json")["source_files"], 1)
                result = d.read(h.out/"scaling-result.json")
                self.assertEqual(result["status"], "SESSION_FATAL")
                self.assertIs(result["all_stages_passed"], False)
                self.assertEqual(result["stages"], [])

    def test_output_mismatch_is_rejected_before_source_registration_or_any_output_creation(self):
        with tempfile.TemporaryDirectory() as tmp:
            h = Harness(tmp); other = Path(tmp)/"other-run"; h.session.out = other
            with self.running(h) as (source, remaining):
                with self.assertRaisesRegex(ValueError, "output must match managed watch output"):
                    h.session.run()
                source.assert_not_called(); remaining.assert_not_called()
            h.provider.prepare.assert_not_called(); h.provider.teardown.assert_not_called()
            self.assertEqual(h.events, [])
            self.assertFalse(h.out.exists()); self.assertFalse(other.exists())

    def test_zero_receipt_budget_prevents_provision_and_still_writes_source_after(self):
        with tempfile.TemporaryDirectory() as tmp:
            h = Harness(tmp)
            with self.running(h), mock.patch.object(s, "RECEIPT_BUDGET", 0):
                with self.assertRaisesRegex(ValueError, "receipt"):
                    h.session.run()
            h.provider.prepare.assert_not_called(); h.provider.teardown.assert_not_called()
            self.assertEqual(h.arms, [])
            self.assertEqual(sum(event[0] == "source" for event in h.events), 2)
            self.assertEqual(d.read(h.out/"source-after.json")["source_files"], 1)
            self.assertEqual(d.read(h.out/"scaling-result.json")["status"], "SESSION_FATAL")

    def test_prepare_failure_still_verifies_source_without_tearing_down_unknown_resources(self):
        with tempfile.TemporaryDirectory() as tmp:
            h = Harness(tmp); h.prepare_error = OSError("prepare failed")
            with self.running(h), self.assertRaisesRegex(OSError, "prepare failed"):
                h.session.run()
            h.provider.prepare.assert_called_once_with(h.plan, h.out/"provision")
            h.provider.teardown.assert_not_called()
            self.assertEqual(h.arms, [])
            self.assertEqual(sum(event[0] == "source" for event in h.events), 2)
            self.assertEqual(d.read(h.out/"source-after.json")["source_files"], 1)
            self.assertEqual(d.read(h.out/"scaling-result.json")["status"], "SESSION_FATAL")


def scaling_failure(hosts=1):
    document, layout, wave = canonical_failure()
    root = "/srv/otel-horizontal/native-test/arms/scaling-"+str(hosts)
    layout.update(schema=s.SCHEMA, root=root, storage_hosts=["node"+str(i) for i in range(1, hosts+1)],
                  storage_process_count=3, storage_max_outstanding=64,
                  storage_memory_profile="scaling3ss3g", remote_cluster_file=root+"/fdb.cluster")
    control = [(copy.deepcopy(spec), copy.deepcopy(document["cluster"]["processes"][str(spec["port"])]))
               for spec in layout["processes"] if spec["port"] < 4510]
    storage_spec = copy.deepcopy(next(spec for spec in layout["processes"] if spec["port"] == 4510))
    storage_process = copy.deepcopy(document["cluster"]["processes"]["4510"])
    specs, processes, activity = [], {}, {}
    for spec, process in control:
        specs.append(spec); processes["node1-"+str(spec["port"])] = process
    for index in range(1, hosts+1):
        role = "node"+str(index)
        for port in (4510, 4511, 4512):
            spec, process = copy.deepcopy(storage_spec), copy.deepcopy(storage_process)
            address, storage_id = "10.93.0."+str(index+1)+":"+str(port), "%016x" % (index*10000+port)
            spec.update(node=role, port=port, address=address, machineid=role, zoneid=role, memory_limit_bytes=3*p.GIB)
            process.update(address=address, locality={"machineid": role, "zoneid": role})
            process["memory"]["limit_bytes"] = 3*p.GIB
            process["roles"][0]["id"] = storage_id
            specs.append(spec); processes[role+"-"+str(port)] = process
            activity[address] = {"node": role, "storage_id": storage_id, "input_bytes": 100,
                                 "durable_bytes": 10, "stored_bytes": 20}
    for spec in specs:
        flags = {"--cluster-file": root+"/fdb.cluster", "--public-address": spec["address"],
                 "--listen-address": spec["address"], "--class": spec["process_class"],
                 "--datadir": root+"/data/"+str(spec["port"]), "--logdir": root+"/logs",
                 "--locality-machineid": spec["node"], "--locality-zoneid": spec["node"],
                 "--memory": "3GiB" if spec["port"] >= 4510 else "1GiB",
                 "--cache-memory": "1GiB" if spec["port"] >= 4510 else "64MiB",
                 "--trace-format": "json", "--maxlogssize": "128MiB", "--loggroup": "native-test"}
        spec["args"] = ["/srv/otel-horizontal/native-test/bin/fdbserver"]+[x for item in flags.items() for x in item]
        spec["unit_name"] = "oh-native-test-scaling"+str(hosts)+"-"+str(spec["port"])+".service"
    layout["processes"], document["cluster"]["processes"] = specs, processes
    document["client"]["cluster_file"]["path"] = layout["remote_cluster_file"]
    first_id = activity["10.93.0.2:4510"]["storage_id"]
    for key in ("performance_limited_by", "batch_performance_limited_by"):
        document["cluster"]["qos"][key]["reason_server_id"] = first_id
    wave.update(schema="otel_native_scaling_wave_v1", count=375000, count_per_producer=375000,
                producers=hosts, sampler_exits={role: 0 for role in s.active_roles(hosts)},
                producer_process_receipts={"client"+str(i): {"started": True, "pid": 900+i, "starttime_ticks": 9000+i}
                                           for i in range(1, hosts+1)},
                native_activity=[{"storage_processes": copy.deepcopy(activity)} for _ in range(3)])
    wave["producer_process_receipt"] = wave["producer_process_receipts"]["client1"]
    return document, layout, wave


class ScalingFailureTests(unittest.TestCase):
    def classify(self, document, layout, wave, *, error=None, guards=True):
        return s.classify_stage_failure(ValueError("strict client health") if error is None else error,
                                        document, layout, wave, fatal_guards_passed=guards)

    def test_only_canonical_failure_with_all_active_native_and_producer_identities_is_reusable(self):
        for hosts in (1, 2, 3):
            document, layout, wave = scaling_failure(hosts); before = copy.deepcopy((document, layout, wave))
            with self.subTest(hosts=hosts):
                with self.assertRaisesRegex(ValueError, "strict client health"):
                    p.strict_status(document, layout)
                self.assertIs(self.classify(document, layout, wave), True)
                self.assertEqual((document, layout, wave), before)
                self.assertIs(document["client"]["database_status"]["healthy"], False)

    def test_error_type_fresh_guards_and_scaling_schema_never_accept_lookalikes(self):
        document, layout, wave = scaling_failure()
        for error in (ValueError("strict client health "), ValueError("unknown"),
                      RuntimeError("strict client health"), OSError("strict client health")):
            with self.subTest(error=error):
                self.assertIs(self.classify(document, layout, wave, error=error), False)
        for guards in (False, None, 1, "true"):
            with self.subTest(guards=guards):
                self.assertIs(self.classify(document, layout, wave, guards=guards), False)
        for value in (None, [], {}, {**layout, "schema": "otel_native_diagnostic_v1"},
                      {**layout, "storage_hosts": []}, {**layout, "storage_hosts": ["node2"]}):
            with self.subTest(layout=value):
                self.assertIs(self.classify(document, value, wave), False)

    def test_sampler_exits_and_every_producer_receipt_require_exact_active_typed_population(self):
        document, layout, wave = scaling_failure(3)
        for role in sorted(s.active_roles(3)):
            changed = copy.deepcopy(wave); del changed["sampler_exits"][role]
            self.assertIs(self.classify(document, layout, changed), False)
            for bad in (True, False, 0.0, "0", None, 1, -1):
                changed = copy.deepcopy(wave); changed["sampler_exits"][role] = bad
                with self.subTest(role=role, exit=bad):
                    self.assertIs(self.classify(document, layout, changed), False)
        for role in ("client1", "client2", "client3"):
            changed = copy.deepcopy(wave); del changed["producer_process_receipts"][role]
            self.assertIs(self.classify(document, layout, changed), False)
            for key, bad in (("started", 1), ("started", False), ("pid", 0), ("pid", True),
                             ("starttime_ticks", 0), ("starttime_ticks", 9000.0)):
                changed = copy.deepcopy(wave); changed["producer_process_receipts"][role][key] = bad
                with self.subTest(role=role, key=key, bad=bad):
                    self.assertIs(self.classify(document, layout, changed), False)
        changed = copy.deepcopy(wave); changed["sampler_exits"]["client4"] = 0
        self.assertIs(self.classify(document, layout, changed), False)
        changed = copy.deepcopy(wave); changed["producer_process_receipts"]["client4"] = {"started": True, "pid": 904, "starttime_ticks": 9004}
        self.assertIs(self.classify(document, layout, changed), False)

    def test_missing_history_recruitment_collection_fault_or_unknown_warning_remains_fatal(self):
        document, layout, wave = scaling_failure(3)
        for history in (None, [], {}, [{}]):
            changed = copy.deepcopy(wave); changed["native_activity"] = history
            with self.subTest(history=history):
                self.assertIs(self.classify(document, layout, changed), False)
        for index in range(3):
            for address in wave["native_activity"][index]["storage_processes"]:
                changed = copy.deepcopy(wave)
                changed["native_activity"][index]["storage_processes"][address]["storage_id"] = "recruited"
                self.assertIs(self.classify(document, layout, changed), False)
        for errors in (None, False, {}, ["SSH transport failed"], ["trace capture failed"]):
            changed = copy.deepcopy(wave); changed["collection_errors"] = errors
            self.assertIs(self.classify(document, layout, changed), False)
        changed = copy.deepcopy(document)
        changed["cluster"]["messages"].append({"name": "unknown", "description": "warning"})
        self.assertIs(self.classify(changed, layout, wave), False)


class ScalingArmTests(unittest.TestCase):
    def setUp(self):
        for name in ("run", "Popen"):
            guard = mock.patch.object(n.subprocess, name, side_effect=AssertionError("real subprocess forbidden"))
            guard.start(); self.addCleanup(guard.stop)

    def test_real_arm_requiesces_and_passes_own_100k_gate_before_measured_wave(self):
        for hosts in (1, 2, 3):
            for helpers_ready in (False, True):
                with self.subTest(hosts=hosts, helpers_ready=helpers_ready), tempfile.TemporaryDirectory() as tmp:
                    h = Harness(tmp); arm = s.ScalingArm(h.plan, h.out, h.provider, stage_hosts=hosts,
                                                       provision_out=Path(tmp)/"shared", receipt_root=Path(tmp))
                    events = []
                    arm.install_helpers = mock.Mock(side_effect=lambda: events.append("install"))
                    arm.verify_helpers = mock.Mock(side_effect=lambda: events.append("verify"))
                    arm.quiet = mock.Mock(side_effect=lambda: events.append("quiet"))
                    arm.host_samples = mock.Mock(return_value={role: {"fdb_pids": {}} for role in s.ROLES})
                    arm.stable = mock.Mock(side_effect=lambda *args, **kwargs: events.append("stable"))
                    layout = {"root": "/owned/scaling-"+str(hosts), "storage_hosts": ["node"+str(i) for i in range(1, hosts+1)]}
                    def setup(*args):
                        self.assertEqual(events[-1], "quiet")
                        self.assertEqual(args, (h.plan, Path(tmp)/"shared", h.resources["nodes"], hosts, "scaling-"+str(hosts)))
                        events.append("cluster")
                        return layout
                    h.provider.setup_cluster.side_effect = setup
                    def wave(bound, count, name):
                        self.assertIs(bound, layout)
                        events.append(name)
                        return wave_result(hosts, count)
                    arm.wave = mock.Mock(side_effect=wave)
                    with mock.patch.object(p, "verify_source", side_effect=h.verify_source), mock.patch.object(d, "remaining", return_value=30):
                        result = arm.run_existing(h.resources, helpers_ready=helpers_ready)
                    self.assertEqual(events, ["verify" if helpers_ready else "install", "quiet", "cluster", "stable", "prerequisite", "measured"])
                    self.assertEqual([call.args[1:] for call in arm.wave.call_args_list], [(100000, "prerequisite"), (375000, "measured")])
                    self.assertEqual(result["status"], "PASS")
                    self.assertEqual(d.read(h.out/"stage-result.json"), result)
                    h.provider.prepare.assert_not_called(); h.provider.teardown.assert_not_called()

    def test_false_or_partial_prerequisite_stops_this_arm_before_any_measured_wave(self):
        bad = [None, {}, wave_result(2, 100000), wave_result(1, 375000)]
        for key, value in (("healthy", False), ("healthy", 1), ("ingest_valid", False),
                           ("census_pass", False), ("oracle_pass", False), ("rows", 0), ("rows", True)):
            candidate = wave_result(1, 100000); candidate[key] = value; bad.append(candidate)
        for candidate in bad:
            with self.subTest(gate=candidate), tempfile.TemporaryDirectory() as tmp:
                h = Harness(tmp); arm = s.ScalingArm(h.plan, h.out, h.provider, stage_hosts=1)
                arm.install_helpers = mock.Mock(); arm.quiet = mock.Mock(); arm.stable = mock.Mock()
                arm.host_samples = mock.Mock(return_value={role: {"fdb_pids": {}} for role in s.ROLES})
                h.provider.setup_cluster.side_effect = None
                h.provider.setup_cluster.return_value = {"root": "/owned/scaling-1"}
                arm.wave = mock.Mock(return_value=candidate)
                with mock.patch.object(p, "verify_source", side_effect=h.verify_source), mock.patch.object(d, "remaining", return_value=30):
                    with self.assertRaises(ValueError):
                        arm.run_existing(h.resources)
                arm.wave.assert_called_once_with(h.provider.setup_cluster.return_value, 100000, "prerequisite")
                self.assertNotIn("measured", d.read(h.out/"stage-result.json"))
                self.assertEqual(sum(event[0] == "source" for event in h.events), 2)

    def test_remote_helpers_copy_and_seal_the_supervisor_dependency_on_all_six_hosts(self):
        for mode in ("install_helpers", "verify_helpers"):
            for changed in (False, True):
                with self.subTest(mode=mode, changed=changed), tempfile.TemporaryDirectory() as tmp:
                    h = Harness(tmp); arm = s.ScalingArm(h.plan, h.out, h.provider, stage_hosts=3)
                    arm.nodes = h.resources["nodes"]
                    arm.remote_harness = "/srv/otel-horizontal/native-test/harness"
                    arm.copy = mock.Mock()
                    def command(node, args):
                        name = Path(args[1]).name
                        digest = "0"*64 if changed and name == "supervisor.py" and node is arm.nodes["client3"] else p.sha(n.HERE/name)
                        return digest+"  "+args[1]+"\n"
                    arm.command = mock.Mock(side_effect=command)
                    with mock.patch.object(n.Diagnostic, mode) as base:
                        if changed:
                            with self.assertRaisesRegex(ValueError, "scaling remote helper"):
                                getattr(arm, mode)()
                        else:
                            getattr(arm, mode)()
                    base.assert_called_once_with()
                    for node in arm.nodes.values():
                        arm.command.assert_any_call(node, ["sha256sum", arm.remote_harness+"/supervisor.py"])
                        if mode == "install_helpers":
                            arm.copy.assert_any_call(node, n.HERE/"supervisor.py", arm.remote_harness+"/supervisor.py")
                    if mode == "verify_helpers":
                        arm.copy.assert_not_called()

    def test_scope_bindings_and_remote_dispatch_use_each_typed_independent_run(self):
        with tempfile.TemporaryDirectory() as tmp:
            arm = s.ScalingArm(plan(), tmp, mock.Mock(), stage_hosts=3)
            arm.remote_harness = "/srv/otel-horizontal/native-test/harness"
            arm.producer_plans = [("client"+str(i), grant(3, count, i)) for count in (100000, 375000) for i in (1, 2, 3)]
            scopes = arm.scope_bindings()
            self.assertEqual(len(scopes), 18)
            self.assertEqual(len({value["unit"] for value in scopes}), 18)
            for value in scopes:
                args = value["args"]; count = int(args[args.index("--count")+1]); mode = args[args.index("--mode")+1]
                self.assertEqual(args[args.index("--timeout")+1], "300s" if count == 100000 and mode == "ingest" else "120s")
                self.assertEqual(value["cgroup"], "/horizontal.slice/horizontal-client.slice/"+value["unit"])
            remote = arm.remote({"id": 106}, "_produce", "/owned/plan.json")
            self.assertEqual(remote, ["python3", "-B", arm.remote_harness+"/scaling.py", "_produce", "--path", "/owned/plan.json", "--node-id", "106"])

    def test_remote_measured_producer_uses_native_app_without_revalidating_legacy_count(self):
        for verify in (False, True):
            with self.subTest(verify=verify), tempfile.TemporaryDirectory() as tmp:
                work = Path(tmp); value = grant(3, 375000, 3); path = work/"plan.json"
                d.write(path, value)
                d.write(work/"release.json", {"token": value["token"], "grant_sha256": p.sha(path)})
                with mock.patch.object(n, "no_symlinks", return_value=work), \
                     mock.patch.object(d, "identity") as identity, mock.patch.object(d, "remaining", return_value=30), \
                     mock.patch.object(n, "run_app") as run_app:
                    s.produce(path, verify)
                identity.assert_called_once_with(value["node_id"])
                self.assertEqual([call.args for call in run_app.call_args_list],
                                 [(value, "audit"), (value, "query")] if verify else [(value, "ingest")])
                self.assertEqual(d.read(path)["count"], 375000)
                if not verify:
                    self.assertEqual(d.read(work/"ready.json"), {"node_id": value["node_id"], "run": value["run"],
                                                               "token": value["token"], "grant_sha256": p.sha(path)})


class FakeProcess:
    def __init__(self, kind, harness):
        self.kind, self.harness, self.polls, self.returncode = kind, harness, 0, None

    def poll(self):
        if self.returncode is not None or self.kind == "sampler":
            return self.returncode
        if self.kind == "producer" and not self.harness.released:
            return None
        self.polls += 1
        if self.polls >= 2:
            self.returncode = 0
        return self.returncode

    def wait(self, timeout=None):
        self.returncode = 0
        return 0

    def terminate(self):
        self.returncode = -15

    def kill(self):
        self.returncode = -9


class WaveHarness:
    def __init__(self, root, hosts=3):
        self.lease = Harness(root)
        self.events, self.sampler_plans, self.producer_plans, self.jobs = [], {}, {}, {}
        self.released, self.clock_ns, self.bad_committed, self.bad_profile = False, 0, {}, False
        self.trace_charges = []
        self.ingest_mutator = None
        self.bad_verification = None
        self.bad_trace_binding = None
        self.arm = s.ScalingArm(self.lease.plan, self.lease.out, self.lease.provider, stage_hosts=hosts,
                               clock=self.clock, sleep=lambda seconds: None)
        self.arm.out.mkdir(parents=True)
        self.arm.nodes = self.lease.resources["nodes"]
        self.arm.remote_root = "/srv/otel-horizontal/native-test"
        self.arm.remote_harness = self.arm.remote_root+"/harness"
        _, self.layout, wave = scaling_failure(hosts)
        cluster = Path(root)/"fdb.cluster"; cluster.write_text("fake:cluster@10.93.0.2:4500\n")
        self.layout["cluster_file"] = str(cluster)
        self.observation = {**sample(100, 1), "storage_processes": wave["native_activity"][0]["storage_processes"],
                            "storage": {"node"+str(i): {"input_bytes": 100} for i in range(1, hosts+1)}}
        self.arm.stable = mock.Mock(return_value=self.observation)
        self.arm.monitor = mock.Mock(side_effect=lambda layout: copy.deepcopy(self.observation))
        self.arm.monitor_load = mock.Mock(side_effect=lambda layout: copy.deepcopy(self.observation))
        self.arm.command = mock.Mock(side_effect=self.command)
        self.arm.copy = mock.Mock(side_effect=self.copy)
        self.arm.spawn = mock.Mock(side_effect=self.spawn)
        self.arm.wait_file = mock.Mock(side_effect=self.wait_file)
        self.arm.read_remote = mock.Mock(side_effect=self.read_remote)
        self.arm.collect = mock.Mock()
        self.arm.drain = mock.Mock(side_effect=self.drain)

    def role(self, node):
        return next(role for role, value in self.arm.nodes.items() if value is node)

    def clock(self):
        self.clock_ns += 1000000000
        self.events.append(("clock", self.clock_ns))
        return self.clock_ns

    def copy(self, node, local, remote):
        value = d.read(local); role = self.role(node)
        plans = self.sampler_plans if value["schema"] == "otel_native_scaling_sampler_v1" else self.producer_plans
        plans[role] = (value, Path(local))

    def spawn(self, node, args, directory, name):
        role = self.role(node)
        kind = "sampler" if "_sample" in args else "producer" if "_produce" in args else "verification"
        process = FakeProcess(kind, self)
        self.jobs[(role, kind)] = process
        self.events.append(("spawn", role, kind))
        return process, mock.Mock(), mock.Mock()

    def identity(self, node):
        return {"sealed_node_id": node["id"]}

    def wait_file(self, node, path, cap=60, job=None):
        role = self.role(node)
        if "/diagnostics/" in path:
            grant, local = self.sampler_plans[role]
            digest = hashlib.sha256((json.dumps(self.identity(node), sort_keys=True)+"\n").encode()).hexdigest()
            return {"node_id": node["id"], "token": grant["token"], "baseline_frames": 2,
                    "grant_sha256": p.sha(local), "identity_sha256": digest}
        grant, local = self.producer_plans[role]
        if path.endswith("producer-started.json"):
            return {"started": True, "node_id": node["id"], "run": grant["run"], "token": grant["token"],
                    "grant_sha256": p.sha(local), "pid": node["id"]+1000, "starttime_ticks": node["id"]+10000}
        return {"node_id": node["id"], "run": grant["run"], "token": grant["token"], "grant_sha256": p.sha(local)}

    def read_remote(self, node, path):
        if path.endswith("identity.json"):
            return self.identity(node)
        role = self.role(node); grant, _ = self.producer_plans[role]
        count, mode = grant["count"], Path(path).stem
        value = evidence(mode, count)
        value["config"].update(run=grant["run"], batch=64, workers=8)
        if role == "client3" and mode == self.bad_verification:
            value["failures"] = 1
        if mode == "ingest":
            value["stats"]["diagnostics"] = ingest_diagnostics(count)
            value["stats"]["diagnostics"]["committed_rows"] = self.bad_committed.get(role, count)
            if self.ingest_mutator is not None:
                self.ingest_mutator(role, value["stats"]["diagnostics"])
        elif mode == "audit":
            value["stats"]["structures"] = {key: {"keys": number} for key, number in
                {"records": count, "eq_secondary": count*8, "span_id_v2": count,
                 "span_time_v2": count, "owned_total": count*11+2, "eq_primary": 0}.items()}
        return value

    def command(self, node, args, cap=30):
        role = self.role(node)
        if args[0] == "mkdir":
            return ""
        if args[0] == "sha256sum":
            return p.sha(self.layout["cluster_file"])+"  "+args[1]+"\n"
        if "_frames" in args:
            return json.dumps({"frames": [], "cursor": int(args[args.index("--cursor")+1])})
        if "_phase" in args:
            self.events.append(("phase", role)); return ""
        if "_release" in args:
            self.events.append(("release", role)); self.released = True; return ""
        if "_stop" in args:
            self.events.append(("stop-sampler", role))
            self.jobs[(role, "sampler")].returncode = 0
            return ""
        if "_traces" in args:
            quota = int(args[args.index("--budget")+1]); consumed = quota//2
            self.trace_charges.append((role, quota, consumed))
            grant, local = self.sampler_plans[role]
            packet = {"schema": "otel_native_forensic_evidence_v1", "node_id": node["id"], "role": role,
                      "scaling_hosts": grant["scaling_hosts"], "grant_sha256": p.sha(local),
                      "bytes_read": consumed, "coverage": "CAPTURED", "forensic_only": False, "errors": []}
            if role == "node3" and self.bad_trace_binding is not None:
                packet[self.bad_trace_binding] = "node2" if self.bad_trace_binding == "role" else self.arm.nodes["node2"]["id"]
            return json.dumps(packet)
        raise AssertionError((role, args))

    def drain(self, layout):
        self.events.append(("drain",))
        return [sample(4, .3), sample(2, .2), sample(0, .1)]

    def profile(self, *args, **kwargs):
        return {"verdict": "INCONCLUSIVE" if self.bad_profile else "CAPTURED",
                "producer_process_bound": True, "positive_native_activity": True, "accepted_throughput": False}

    @contextmanager
    def running(self):
        with mock.patch.object(d, "remaining", return_value=30), mock.patch.object(n, "profile", side_effect=self.profile):
            yield


class ScalingWaveTests(unittest.TestCase):
    def setUp(self):
        for name in ("run", "Popen"):
            guard = mock.patch.object(n.subprocess, name, side_effect=AssertionError("real subprocess forbidden"))
            guard.start(); self.addCleanup(guard.stop)

    def test_all_active_producers_release_in_one_window_before_last_ack_and_strict_drain(self):
        for hosts in (1, 2, 3):
            with self.subTest(hosts=hosts), tempfile.TemporaryDirectory() as tmp:
                h = WaveHarness(tmp, hosts)
                with h.running():
                    result = h.arm.wave(h.layout, 375000, "measured")
                self.assertEqual(result["rows"], 375000*hosts)
                self.assertEqual(result["active_roles"], sorted(s.active_roles(hosts)))
                self.assertIs(result["measurement_eligible"], True)
                self.assertIs(result["ingest_valid"], True)
                self.assertEqual(result["profile"]["verdict"], "CAPTURED")
                self.assertGreater(result["ack_rows_per_second"], result["drained_rows_per_second"])
                self.assertEqual(set(result["producer_process_receipts"]), {"client"+str(i) for i in range(1, hosts+1)})
                start = h.events.index(("clock", result["controller_started_ns"]))
                ack = h.events.index(("clock", result["controller_ack_end_ns"]))
                drained = h.events.index(("clock", result["controller_drained_end_ns"]))
                drain = h.events.index(("drain",))
                for role in h.producer_plans:
                    self.assertLess(start, h.events.index(("release", role)))
                    self.assertLess(h.events.index(("release", role)), ack)
                    self.assertLess(drained, h.events.index(("spawn", role, "verification")))
                self.assertLess(ack, drain); self.assertLess(drain, drained)
                self.assertEqual(h.arm.collect.call_count, hosts)
                self.assertEqual(d.read(h.arm.out/"measured"/"result.json"), result)
                for role, (value, _) in h.producer_plans.items():
                    self.assertEqual(value["schema"], "otel_native_scaling_producer_v1")
                    self.assertEqual(value["count"], 375000)
                    self.assertEqual(value["role"], role)

    def test_every_producer_needs_exact_committed_count_before_metrics_or_census_can_pass(self):
        for bad in (None, 0, True, 375000.0, 374999, 375001):
            with self.subTest(committed_rows=bad), tempfile.TemporaryDirectory() as tmp:
                h = WaveHarness(tmp, 3); h.bad_committed["client3"] = bad
                with h.running(), self.assertRaisesRegex(ValueError, "committed ACK"):
                    h.arm.wave(h.layout, 375000, "measured")
                result = d.read(h.arm.out/"measured"/"result.json")
                self.assertIs(result["ingest_valid"], False)
                self.assertIs(result["measurement_eligible"], False)
                self.assertNotIn("ack_rows_per_second", result)
                self.assertNotIn(("drain",), h.events)
                self.assertFalse(any(event[0] == "spawn" and event[2] == "verification" for event in h.events))

    def test_missing_zero_or_boolean_phase_proof_is_rejected_before_metrics_or_verification(self):
        changes = (lambda value: value.pop("phase_latencies"),
                   lambda value: value["phase_latencies"].pop("commit"),
                   lambda value: value["phase_latencies"]["save"].update(count=0),
                   lambda value: value["phase_latencies"]["run"].update(count=True),
                   lambda value: value["phase_latencies"]["open"].update(total_nanos=False))
        for index, change in enumerate(changes):
            with self.subTest(phase_fault=index), tempfile.TemporaryDirectory() as tmp:
                h = WaveHarness(tmp, 3)
                def mutate(role, diagnostics):
                    if role == "client3":
                        change(diagnostics)
                h.ingest_mutator = mutate
                with h.running(), self.assertRaises(ValueError):
                    h.arm.wave(h.layout, 375000, "measured")
                result = d.read(h.arm.out/"measured"/"result.json")
                self.assertIs(result["ingest_valid"], False)
                self.assertIs(result["measurement_eligible"], False)
                self.assertNotIn("ack_rows_per_second", result)
                self.assertNotIn(("drain",), h.events)
                self.assertFalse(any(event[0] == "spawn" and event[2] == "verification" for event in h.events))

    def test_every_audit_and_oracle_must_pass_before_aggregate_rates_are_published(self):
        for mode in ("audit", "query"):
            with self.subTest(failed_verification=mode), tempfile.TemporaryDirectory() as tmp:
                h = WaveHarness(tmp, 3); h.bad_verification = mode
                with h.running(), self.assertRaisesRegex(ValueError, "failed application evidence"):
                    h.arm.wave(h.layout, 375000, "measured")
                result = d.read(h.arm.out/"measured"/"result.json")
                self.assertIs(result["ingest_valid"], True)
                self.assertIs(result["census_pass"], False)
                self.assertIs(result["oracle_pass"], False)
                self.assertIs(result["measurement_eligible"], False)
                self.assertIn(("drain",), h.events)
                self.assertIn(("spawn", "client3", "verification"), h.events)
                self.assertNotIn("ack_rows_per_second", result)
                self.assertNotIn("drained_rows_per_second", result)

    def test_live_profile_is_required_even_when_all_ack_census_and_oracle_receipts_are_valid(self):
        with tempfile.TemporaryDirectory() as tmp:
            h = WaveHarness(tmp); h.bad_profile = True
            with h.running():
                result = h.arm.wave(h.layout, 375000, "measured")
            self.assertTrue(all(result[key] is True for key in ("healthy", "ingest_valid", "census_pass", "oracle_pass")))
            self.assertEqual(result["profile"]["verdict"], "INCONCLUSIVE")
            self.assertIs(result["measurement_eligible"], False)
            self.assertIs(result["accepted_throughput"], False)

    def test_foreign_host_or_role_trace_capture_is_never_retained_or_measurement_eligible(self):
        for field in ("role", "node_id"):
            with self.subTest(foreign_binding=field), tempfile.TemporaryDirectory() as tmp:
                h = WaveHarness(tmp, 3); h.bad_trace_binding = field
                initial = h.arm.trace_remaining
                with h.running(), self.assertRaisesRegex(ValueError, "forensic/collection failure"):
                    h.arm.wave(h.layout, 375000, "measured")
                result = d.read(h.arm.out/"measured"/"result.json")
                self.assertTrue(result["collection_errors"])
                self.assertIs(result["measurement_eligible"], False)
                self.assertEqual(result["profile"]["verdict"], "INCONCLUSIVE")
                self.assertFalse((h.arm.out/"measured"/"node3-native-traces.json").exists())
                charged = sum(quota if role == "node3" else used for role, quota, used in h.trace_charges)
                self.assertEqual(h.arm.trace_remaining, initial-charged)

    def test_two_real_waves_share_reservation_and_split_each_quota_across_active_storage_hosts(self):
        with tempfile.TemporaryDirectory() as tmp:
            h = WaveHarness(tmp, 3); initial = h.arm.trace_remaining
            with h.running():
                first = h.arm.wave(h.layout, 100000, "prerequisite")
                after_first = h.arm.trace_remaining
                second = h.arm.wave(h.layout, 375000, "measured")
            self.assertEqual(first["forensic_allocation"], initial//6)
            self.assertEqual(second["forensic_allocation"], after_first//5)
            self.assertGreater(second["forensic_allocation"], first["forensic_allocation"])
            self.assertEqual(h.arm.remaining_waves, 4)
            self.assertEqual(h.arm.trace_remaining, initial-sum(charge for _, _, charge in h.trace_charges))
            for result in (first, second):
                quotas = result["forensic_node_quotas"]
                self.assertEqual(set(quotas), {"node1", "node2", "node3"})
                self.assertEqual(sum(quotas.values()), result["forensic_allocation"])
                self.assertLessEqual(max(quotas.values())-min(quotas.values()), 1)


class ScalingIngestTests(unittest.TestCase):
    def valid(self):
        value = evidence("ingest", 375000)
        value["stats"]["diagnostics"] = ingest_diagnostics(375000)
        return value

    def test_positive_committed_batches_and_all_four_nonzero_phase_populations_are_immutable(self):
        value = self.valid(); before = copy.deepcopy(value)
        self.assertIs(s.validate_ingest(value, 375000), True)
        self.assertEqual(value, before)
        for phase in ("open", "save", "commit", "run"):
            value["stats"]["diagnostics"]["phase_latencies"][phase] = {
                "count": 2, "min_nanos": 1, "max_nanos": 3, "total_nanos": 4}
        self.assertIs(s.validate_ingest(value, 375000), True)

    def test_missing_diagnostics_empty_batches_and_every_absent_zero_boolean_or_untyped_phase_fail(self):
        for candidate in (None, {}, {"stats": None}, {"stats": {}}, {"stats": {"diagnostics": None}}):
            with self.subTest(evidence=candidate), self.assertRaises(ValueError):
                s.validate_ingest(candidate, 375000)
        for bad in (None, 0, -1, True, False, 1.0, "1"):
            value = self.valid(); value["stats"]["diagnostics"]["committed_batches"] = bad
            with self.subTest(batches=bad), self.assertRaises(ValueError):
                s.validate_ingest(value, 375000)
        value = self.valid(); del value["stats"]["diagnostics"]["committed_batches"]
        with self.assertRaises(ValueError):
            s.validate_ingest(value, 375000)
        for phase in ("open", "save", "commit", "run"):
            value = self.valid(); del value["stats"]["diagnostics"]["phase_latencies"][phase]
            with self.subTest(missing_phase=phase), self.assertRaises(ValueError):
                s.validate_ingest(value, 375000)
            for field in ("count", "min_nanos", "max_nanos", "total_nanos"):
                value = self.valid(); del value["stats"]["diagnostics"]["phase_latencies"][phase][field]
                with self.subTest(phase=phase, missing_field=field), self.assertRaises(ValueError):
                    s.validate_ingest(value, 375000)
                for bad in (None, 0, -1, True, False, 1.0, "1", float("nan"), float("inf")):
                    value = self.valid(); value["stats"]["diagnostics"]["phase_latencies"][phase][field] = bad
                    with self.subTest(phase=phase, field=field, bad=bad), self.assertRaises(ValueError):
                        s.validate_ingest(value, 375000)

    def test_phase_extrema_and_total_must_describe_a_possible_nonempty_population(self):
        invalid = ({"count": 1, "min_nanos": 2, "max_nanos": 1, "total_nanos": 2},
                   {"count": 1, "min_nanos": 1, "max_nanos": 3, "total_nanos": 2},
                   {"count": 2, "min_nanos": 2, "max_nanos": 3, "total_nanos": 3},
                   {"count": 2, "min_nanos": 1, "max_nanos": 2, "total_nanos": 5})
        for phase in ("open", "save", "commit", "run"):
            for counters in invalid:
                value = self.valid(); value["stats"]["diagnostics"]["phase_latencies"][phase] = counters
                with self.subTest(phase=phase, counters=counters), self.assertRaises(ValueError):
                    s.validate_ingest(value, 375000)


class ScalingCLITests(unittest.TestCase):
    def test_readonly_plan_validation_never_imports_provider_or_starts_a_session(self):
        with tempfile.TemporaryDirectory() as tmp:
            h = Harness(tmp); path = Path(tmp)/"plan.json"; d.write(path, h.plan)
            original_import = builtins.__import__
            def guarded_import(name, *args, **kwargs):
                if name == "provision":
                    raise AssertionError("readonly CLI imported provider")
                return original_import(name, *args, **kwargs)
            with mock.patch("builtins.__import__", guarded_import), \
                 mock.patch.object(s, "ScalingSession", side_effect=AssertionError("session started")), \
                 mock.patch.object(p, "verify_source", side_effect=AssertionError("source effect")), \
                 mock.patch.object(s.supervisor, "register", side_effect=AssertionError("watch registration effect")), \
                 mock.patch("builtins.print") as output:
                s.main(["run", "--plan", str(path), "--output", str(h.out)])
            value = json.loads(output.call_args.args[0])
            self.assertEqual(value["nodes"], 6)
            self.assertEqual(value["stages"], [[1, 1], [2, 2], [3, 3]])
            self.assertEqual(value["authorized_rows"], 2850000)
            self.assertEqual(value["cumulative_rows_upper"], 2850000)
            self.assertIs(value["provisioned"], False)
            self.assertFalse(h.out.exists())

    def test_approved_cli_retains_nonpassing_stage_failure_and_passes_explicit_output_directly(self):
        for passed in (True, False):
            with self.subTest(all_stages_passed=passed), tempfile.TemporaryDirectory() as tmp:
                h = Harness(tmp); path = Path(tmp)/"plan.json"; d.write(path, h.plan)
                with mock.patch.dict("sys.modules", {"provision": h.provider}), mock.patch.object(s, "ScalingSession") as session:
                    session.return_value.run.return_value = {"all_stages_passed": passed}
                    args = ["run", "--plan", str(path), "--output", str(h.out), "--execute-approved"]
                    if passed:
                        self.assertIsNone(s.main(args))
                    else:
                        with self.assertRaisesRegex(ValueError, "did not pass"):
                            s.main(args)
                    session.assert_called_once_with(h.plan, str(h.out), h.provider)
                    session.return_value.run.assert_called_once_with()
                self.assertFalse(h.out.exists())

    def test_execute_requires_output_before_provider_import_or_session_construction(self):
        with tempfile.TemporaryDirectory() as tmp:
            h = Harness(tmp); path = Path(tmp)/"plan.json"; d.write(path, h.plan)
            original_import = builtins.__import__
            def guarded_import(name, *args, **kwargs):
                if name == "provision":
                    raise AssertionError("execution without output imported provider")
                return original_import(name, *args, **kwargs)
            with mock.patch("builtins.__import__", guarded_import), mock.patch.object(s, "ScalingSession") as session:
                with self.assertRaisesRegex(ValueError, "output required"):
                    s.main(["run", "--plan", str(path), "--execute-approved"])
                session.assert_not_called()
            self.assertFalse(h.out.exists())

    def test_reset_flag_cannot_restart_the_one_lease_clock_or_authorized_row_budget(self):
        with tempfile.TemporaryDirectory() as tmp:
            h = Harness(tmp); path = Path(tmp)/"plan.json"; d.write(path, h.plan)
            with mock.patch.object(s, "ScalingSession") as session, mock.patch("sys.stderr"):
                with self.assertRaises(SystemExit) as error:
                    s.main(["run", "--plan", str(path), "--output", str(h.out), "--execute-approved", "--reset"])
                self.assertEqual(error.exception.code, 2)
                session.assert_not_called()
            self.assertFalse(h.out.exists())


if __name__ == "__main__":
    unittest.main()
