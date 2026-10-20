#!/usr/bin/env python3
"""Remote-only regression checks for the external measurement protocol scripts."""
import collections
import csv
import io
import os
import importlib.util
import json
import math
from pathlib import Path
import random
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch
from contextlib import redirect_stdout


def module(name, filename):
    spec = importlib.util.spec_from_file_location(name, Path(__file__).with_name(filename))
    result = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(result)
    return result


run = module("perf_run", "astra-perf-run.py")
analyze = module("perf_analyze", "astra-perf-analyze.py")


class ProtocolTests(unittest.TestCase):
    def test_nearest_rank(self):
        self.assertEqual(analyze.quantile(list(range(1, 101)), .99), 99)
        self.assertEqual(analyze.quantile([1, 2, 3, 4], .5), 2)
        for values, p in (([], .5), ([math.nan], .5), ([math.inf], .5), ([1], 0), ([1], 1.1), ([1], math.nan)):
            with self.assertRaises(ValueError):
                analyze.quantile(values, p)

    def test_constant_paired_ratio(self):
        result = analyze.bootstrap_log_ratio([(20, 10)] * 24, random.Random(1), 1000)
        for key in ("ratio", "ci95_low", "ci95_high"):
            self.assertAlmostEqual(result[key], 2)
        self.assertEqual(result["blocks"], 24)

    def test_varying_paired_ratio_deterministic_finite(self):
        pairs = [(10 + i, 20 + i) for i in range(24)]
        a = analyze.bootstrap_log_ratio(pairs, random.Random(7), 1000)
        b = analyze.bootstrap_log_ratio(pairs, random.Random(7), 1000)
        self.assertEqual(a, b)
        self.assertTrue(0 < a["ci95_low"] <= a["ratio"] <= a["ci95_high"] < 1)

    def test_invalid_bootstrap_not_success(self):
        for pairs in ([], [(1, 2)], [(0, 1), (1, 1)], [(-1, 1), (1, 1)], [(math.nan, 1), (1, 1)], [(math.inf, 1), (1, 1)]):
            with self.assertRaises(ValueError):
                analyze.bootstrap_log_ratio(pairs, random.Random(1), 100)
        with self.assertRaises(ValueError):
            analyze.bootstrap_log_ratio([(1, 1), (2, 2)], random.Random(1), 0)

    def test_schedule_exact_counterbalance(self):
        plan = json.loads(Path(__file__).with_name("astra-perf-sequential-plan.json").read_text())
        with tempfile.TemporaryDirectory() as tmp:
            out = Path(tmp) / "campaign"
            source = Path(tmp) / "plan.json"
            source.write_text(json.dumps(plan))
            with patch.object(sys, "argv", ["run", "--plan", str(source), "--output", str(out), "--schedule-only"]):
                self.assertEqual(run.main(), 0)
            schedule = json.loads((out / "schedule.json").read_text())
            self.assertEqual(len(schedule), 24 * 2 * 7)
            self.assertEqual(len({entry["run_id"] for entry in schedule}), len(schedule))
            for workload in plan["workloads"]:
                orders = collections.Counter()
                for block in range(24):
                    arm = [entry["arm"] for entry in schedule if entry["block"] == block and entry["workload"]["name"] == workload["name"]]
                    orders[tuple(arm)] += 1
                self.assertEqual(orders, {("go", "cgo"): 12, ("cgo", "go"): 12})

    def test_per_cpu_gate_catches_guest_average_hidden_load(self):
        plan = json.loads(Path(__file__).with_name("astra-perf-sequential-plan.json").read_text())
        before = {"cpu" + str(cpu): [0] * 8 for cpu in range(16)}
        after = {"cpu" + str(cpu): [0, 0, 0, 100, 0, 0, 0, 0] for cpu in range(16)}
        after["cpu8"] = [100, 0, 0, 0, 0, 0, 0, 0]
        before["cpu"] = [0] * 8
        after["cpu"] = [sum(after["cpu" + str(cpu)][i] for cpu in range(16)) for i in range(8)]
        with patch.object(run, "cpu_counters", side_effect=[before, after]), patch.object(run.time, "sleep"):
            result = run.quiescence(plan)
        self.assertLess(result["cpus"]["cpu"]["busy_fraction"], plan["max_busy_fraction"])
        self.assertFalse(result["groups"]["client"]["passed"])
        self.assertFalse(result["passed"])

    def test_child_group_cleanup_waits(self):
        process = subprocess.Popen([sys.executable, "-c", "import time; time.sleep(60)"], start_new_session=True)
        run.stop_process_group(process)
        self.assertIsNotNone(process.returncode)
        with self.assertRaises(ProcessLookupError):
            os.killpg(process.pid, 0)

    def test_runner_telemetry_error_cleans_group_and_pins_abort(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            plan = {"seed": 1, "blocks": 2, "source_commit": "fixture", "cooldown_seconds": 0,
                    "timeout_seconds": 10, "telemetry_seconds": 1,
                    "arms": [{"name": arm, "binary": sys.executable,
                              "command": ["{binary}", "-c", "import time; time.sleep(60)"]} for arm in ("go", "cgo")],
                    "workloads": [{"name": "fixture", "iterations": 1, "warmup": 0, "workers": 1}]}
            source = root / "plan.json"
            source.write_text(json.dumps(plan))
            out = root / "campaign"
            pids = []
            def broken_telemetry(pid):
                pids.append(pid)
                raise RuntimeError("collector failed")
            with patch.object(sys, "argv", ["run", "--plan", str(source), "--output", str(out)]), patch.object(run, "check_builds"), patch.object(run, "quiescence", return_value={"passed": True}), patch.object(run, "snapshot", side_effect=lambda p: p.mkdir()), patch.object(run, "telemetry", side_effect=broken_telemetry):
                with self.assertRaisesRegex(RuntimeError, "collector failed"):
                    run.main()
            self.assertEqual(len(pids), 1)
            with self.assertRaises(ProcessLookupError):
                os.killpg(pids[0], 0)
            aborted = json.loads((out / "ABORTED.json").read_text())
            self.assertTrue(aborted["binaries_unchanged"])
            self.assertEqual(aborted["completed"], 0)
            self.assertFalse((out / "complete.json").exists())

    def test_moving_block_sensitivity_constant_ratio(self):
        result = analyze.dependence_sensitivity([(2, 1)] * 24, random.Random(9), 100)
        self.assertIsNone(result["paired_log_ratio_lag1"])
        self.assertEqual(result["moving_block_length"], 3)
        self.assertAlmostEqual(result["moving_block_ci95_low"], 2)
        self.assertAlmostEqual(result["moving_block_ci95_high"], 2)

    def make_campaign(self, root):
        plan = {"blocks": 2, "arms": [{"name": "go"}, {"name": "cgo"}],
                "workloads": [{"name": "read", "iterations": 4, "warmup": 2, "workers": 1}]}
        schedule = []
        for block in range(2):
            for position, arm in enumerate(["go", "cgo"] if block == 0 else ["cgo", "go"]):
                run_id = f"b{block}-{arm}"
                schedule.append({"run_id": run_id, "block": block, "position": position,
                                 "arm": arm, "workload": plan["workloads"][0]})
                folder = root / run_id
                folder.mkdir()
                (folder / "result.json").write_text(json.dumps({"returncode": 0}))
                (folder / "summary.json").write_text(json.dumps({"success": True, "persisted_validation": "ok",
                    "requested": 4, "completed": 4, "errors": 0, "warmup_completed": 2,
                    "warmup_errors": 0, "measured_wall_ns": 1000}))
                for filename, phase, count in (("samples.csv", "measure", 4), ("warmup.csv", "warmup", 2)):
                    with open(folder / filename, "w", newline="") as stream:
                        writer = csv.DictWriter(stream, fieldnames=["phase", "index", "worker", "latency_ns", "attempts", "error"])
                        writer.writeheader()
                        for index in range(count):
                            writer.writerow(dict(phase=phase, index=index, worker=0,
                                                 latency_ns=20 if arm == "go" else 10, attempts=1, error=""))
        (root / "plan.json").write_text(json.dumps(plan))
        (root / "schedule.json").write_text(json.dumps(schedule))
        (root / "complete.json").write_text(json.dumps({"scheduled": 4, "completed": 4}))
        return schedule

    def analyze_campaign(self, root):
        with patch.object(sys, "argv", ["analyze", str(root), "--resamples", "100"]), redirect_stdout(io.StringIO()):
            analyze.main()

    def test_analysis_accepts_complete_finite_campaign(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            schedule = self.make_campaign(root)
            for entry in schedule:
                run.validate_raw_output(root / entry["run_id"], entry["workload"])
            self.analyze_campaign(root)
            report = json.loads((root / "analysis.json").read_text())
            self.assertTrue(report["valid_zero_error_campaign"])
            for row in report["comparisons"]:
                self.assertAlmostEqual(row["ratio"], 2)

    def test_analysis_rejects_schema_phase_count_duplicates(self):
        for fault in ("schema", "phase", "count", "duplicate_run", "missing_arm", "duplicate_index", "warmup_phase", "summary_false", "persisted_invalid"):
            with self.subTest(fault=fault), tempfile.TemporaryDirectory() as tmp:
                root = Path(tmp)
                schedule = self.make_campaign(root)
                target = root / schedule[0]["run_id"] / "samples.csv"
                if fault == "schema":
                    target.write_text(target.read_text().replace("latency_ns", "wrong"))
                elif fault == "phase":
                    target.write_text(target.read_text().replace("measure", "wrong"))
                elif fault == "count":
                    target.write_text("\n".join(target.read_text().splitlines()[:-1]) + "\n")
                elif fault == "duplicate_run":
                    schedule[-1]["run_id"] = schedule[0]["run_id"]
                    (root / "schedule.json").write_text(json.dumps(schedule))
                elif fault == "missing_arm":
                    schedule[-1]["arm"] = "cgo"
                    (root / "schedule.json").write_text(json.dumps(schedule))
                elif fault == "duplicate_index":
                    target.write_text(target.read_text().replace("measure,1,", "measure,0,"))
                elif fault == "warmup_phase":
                    target = target.with_name("warmup.csv")
                    target.write_text(target.read_text().replace("warmup", "wrong"))
                else:
                    target = target.with_name("summary.json")
                    data = json.loads(target.read_text())
                    data["success" if fault == "summary_false" else "persisted_validation"] = False
                    target.write_text(json.dumps(data))
                with self.assertRaises(ValueError):
                    self.analyze_campaign(root)

    def test_zero_steal_is_only_counter_observation(self):
        result = run.cpu_usage([1, 0, 2, 97, 0, 0, 0, 0])
        self.assertEqual(result["steal_fraction"], 0)
        self.assertEqual(result["busy_fraction"], .03)


if __name__ == "__main__":
    unittest.main(verbosity=2)
