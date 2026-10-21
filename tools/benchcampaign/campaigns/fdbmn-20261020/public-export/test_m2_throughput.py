#!/usr/bin/env python3
"""Tiny synthetic M2 populations; the bootstrap callback is always a fake."""
import copy
import csv
import io
import unittest

import m2_throughput as m2


class Inputs:
    def __init__(self):
        self.json = {}
        self.strings = {}
        self.reads = []

    def load(self, path):
        self.reads.append(path)
        return copy.deepcopy(self.json[path])

    def text(self, path):
        self.reads.append(path)
        return self.strings[path]


def csv_text(rows):
    stream = io.StringIO(newline="")
    writer = csv.DictWriter(stream, fieldnames=["phase", "case", "block", "position", "arm", "repeat", "N",
                                                "ns/op", "B/op", "allocs/op", "rows/op", "prefix-B"], lineterminator="\n")
    writer.writeheader()
    writer.writerows(rows)
    return stream.getvalue()


def fixture():
    inputs = Inputs()
    latency_by_block = [1e6, 2e6, 3e6, 4e6, 10e6, 20e6]
    orders = [list(m2.ARMS), list(reversed(m2.ARMS))] * 3
    for phase, (planned, clean_count) in m2.PHASES.items():
        cases, jobs, reconciled, samples, analyses, comparisons = [], [], [], [], {}, []
        for index in range(planned):
            name = "case-" + str(index)
            clean = index < clean_count
            spec = dict(key=name, placement_label="cross-server", placement_fraction=1.0)
            cases.append(spec)
            run_ids = []
            for block in range(6):
                for position, arm in enumerate(orders[block]):
                    run_id = f"{phase}-c{index:02d}-b{block:02d}-p{position}-{arm}"
                    job = dict(id=run_id, case=name, block=block, position=position, arm=arm)
                    jobs.append(job)
                    if not clean:
                        continue
                    run_ids.append(dict(run_id=run_id))
                    multiplier = 0.5 if arm == "final-remote" else 1.0
                    latency = latency_by_block[block] * multiplier
                    for repeat, factor in enumerate((0.8, 1.0, 1.2)):
                        samples.append(dict(phase=phase, case=name, block=block, position=position, arm=arm,
                                            repeat=repeat, N=(1, 10000, 2)[repeat], **{
                                                "ns/op": latency * factor, "B/op": 32, "allocs/op": 2,
                                                "rows/op": 997, "prefix-B": 22}))
                    folder = f"results-raw/{phase}/{run_id}"
                    cpu = 2.0 if arm == "final-remote" else 1.0
                    inputs.json[folder + "/result.json"] = dict(
                        **job, run_id=run_id, status="ok", returncode=0,
                        process_user_cpu_seconds=cpu, process_system_cpu_seconds=cpu,
                        process_wall_ns=4_000_000_000,
                        cpu_scope="GNU time user/system CPU includes fixtures/calibration/audits/shutdown; NOT CPU/op")
                    inputs.strings[folder + "/process-time.txt"] = (
                        f"User time (seconds): {cpu:.2f}\nSystem time (seconds): {cpu:.2f}\n"
                        "Percent of CPU this job got: 50%\n"
                        "Elapsed (wall clock) time (h:mm:ss or m:ss): 0:03.99\nExit status: 0\n")
            reconciled.append(dict(case=name, verdict="clean" if clean else "contaminated", runs=run_ids,
                                   contaminated_reasons=[] if clean else ["synthetic omitted whole case"],
                                   unverifiable_reasons=[]))
            if clean:
                analyses[name] = dict(status="ok", placement_label="cross-server", placement_fraction=1.0,
                                      absolutes={arm: {"rows/op": {"median_of_process_repeat_medians": 997}}
                                                 for arm in m2.ARMS})
                comparisons.append(dict(case=name, metric="ns/op", numerator="final-remote", denominator="final-ordinary",
                                        status="ok", blocks=6, raw_samples_per_arm=18,
                                        block_medians_numerator=[value * 0.5 for value in latency_by_block],
                                        block_medians_denominator=latency_by_block[:]))
            else:
                analyses[name] = dict(status="N/A")
                # Retained partial samples must never become eligible processes.
                first = next(job for job in jobs if job["case"] == name)
                for repeat in range(3):
                    samples.append(dict(phase=phase, case=name, block=first["block"], position=first["position"],
                                        arm=first["arm"], repeat=repeat, N=1, **{
                                            "ns/op": 1e6, "B/op": 32, "allocs/op": 2, "rows/op": 997, "prefix-B": 22}))
        inputs.json[f"results-raw/{phase}/plan.json"] = dict(phase=phase, cases=cases, jobs=jobs, orders=orders)
        inputs.json[f"analysis/measured-492817/{phase}/report.json"] = dict(
            phase=phase, cases=analyses, comparisons=comparisons, orders=orders)
        inputs.json[f"reconcile/{phase}/report.json"] = dict(
            phase=phase, reconciliation_complete=True, cases=reconciled)
        inputs.strings[f"results-raw/{phase}/metrics.csv"] = csv_text(samples)
    return inputs


class Tests(unittest.TestCase):
    def setUp(self):
        self.inputs = fixture()
        self.calls = []

    def paired(self, ratios, *, resamples, seed):
        self.calls.append((ratios[:], resamples, seed))
        self.assertEqual(len(ratios), 6)
        self.assertEqual(resamples, 20000)
        self.assertEqual(seed, 492817)
        # Synthetic arm ratios are exactly two. No estimator is run in tests.
        self.assertTrue(all(value == 2 for value in ratios))
        return dict(gm=2, nominal_95_interval=[1.5, 2.5], blocks=6, resamples=resamples, seed=seed)

    def collect(self):
        return m2.collect(self.inputs, self.paired)

    def test_exact_populations_clustering_and_callback_contract(self):
        rows, metadata = self.collect()
        counts = {kind: sum(row["record_type"] == kind for row in rows)
                  for kind in ("process", "absolute", "paired", "omitted")}
        self.assertEqual(counts, dict(process=144, absolute=24, paired=24, omitted=4))
        self.assertEqual(len(self.calls), 24)
        self.assertEqual(metadata["source_repeat_samples"], 432)
        self.assertEqual(metadata["independent_paired_blocks"], 6)
        self.assertEqual(len(metadata["input_paths"]), 296)
        self.assertEqual(set(metadata["input_paths"]), set(self.inputs.reads))
        omitted = [row for row in rows if row["record_type"] == "omitted"]
        self.assertTrue(all(row["source_repeat_samples"] == 3 and row["ratio"] == "" for row in omitted))

    def test_even_case_median_not_inverse_even_latency_median(self):
        rows, _ = self.collect()
        row = next(row for row in rows if row["record_type"] == "absolute" and row["phase"] == "M2rl"
                   and row["case"] == "case-0" and row["arm"] == "final-ordinary")
        expected = (1e9 / 3e6 + 1e9 / 4e6) / 2
        self.assertAlmostEqual(row["throughput_ops_per_second"], expected)
        self.assertNotAlmostEqual(row["throughput_ops_per_second"], 1e9 / ((3e6 + 4e6) / 2))
        process = next(row for row in rows if row["record_type"] == "process" and row["phase"] == "M2rl"
                       and row["case"] == "case-0" and row["arm"] == "final-ordinary" and row["block"] == 0)
        self.assertEqual(process["throughput_ops_per_second"], 1000)
        self.assertEqual(process["repeat_N"], [1, 10000, 2])
        self.assertEqual(process["repeat_rows_per_op"], [997, 997, 997])

    def test_measured_cpu_unavailable_and_lifecycle_scope_separate(self):
        rows, metadata = self.collect()
        self.assertFalse(metadata["measured_window_cpu_available"])
        self.assertTrue(all(row["cpu_util_cores"] == "" for row in rows))
        process = next(row for row in rows if row["record_type"] == "process" and row["arm"] == "final-ordinary")
        self.assertEqual(process["lifecycle_cpu_util_cores"], 0.5)
        self.assertNotEqual(process["lifecycle_cpu_util_cores"], 2 / 3.99)
        self.assertEqual(process["gnu_time_elapsed_seconds"], 3.99)
        cpu_pairs = [row for row in rows if row["record_type"] == "paired" and row["metric"] == "lifecycle_cpu_util_cores"]
        self.assertEqual(len(cpu_pairs), 12)
        self.assertTrue(all("not measured-window" in row["ratio_gt_1_means"] for row in cpu_pairs))

    def test_missing_or_duplicate_repeat_refused_before_callback(self):
        path = "results-raw/M2rl/metrics.csv"
        original = self.inputs.strings[path]
        for duplicate in (False, True):
            with self.subTest(duplicate=duplicate):
                values = list(csv.DictReader(io.StringIO(original)))
                if duplicate:
                    values.append(values[0])
                else:
                    values.pop(0)
                self.inputs.strings[path] = csv_text(values)
                with self.assertRaises(ValueError):
                    self.collect()
                self.assertFalse(self.calls)

    def test_missing_planned_process_refused_before_callback(self):
        self.inputs.json["results-raw/M2rl/plan.json"]["jobs"].pop()
        with self.assertRaisesRegex(ValueError, "process matrix"):
            self.collect()
        self.assertFalse(self.calls)

    def test_r4_analysis_eligibility_disagreement_refused(self):
        self.inputs.json["reconcile/M2skip/report.json"]["cases"][0]["verdict"] = "contaminated"
        with self.assertRaisesRegex(ValueError, "eligibility differs"):
            self.collect()
        self.assertFalse(self.calls)

    def test_cpu_receipt_mismatch_refused_before_callback(self):
        path = next(path for path in self.inputs.json if path.endswith("/result.json"))
        self.inputs.json[path]["process_user_cpu_seconds"] = 123
        with self.assertRaisesRegex(ValueError, "CPU receipt differs"):
            self.collect()
        self.assertFalse(self.calls)

    def test_nonpositive_latency_and_bad_wall_refused(self):
        path = "results-raw/M2rl/metrics.csv"
        values = list(csv.DictReader(io.StringIO(self.inputs.strings[path])))
        values[0]["ns/op"] = "0"
        self.inputs.strings[path] = csv_text(values)
        with self.assertRaisesRegex(ValueError, "numeric"):
            self.collect()
        self.assertFalse(self.calls)
        self.inputs = fixture()
        path = next(path for path in self.inputs.json if path.endswith("/result.json"))
        self.inputs.json[path]["process_wall_ns"] = 0
        with self.assertRaisesRegex(ValueError, "numeric"):
            self.collect()
        self.assertFalse(self.calls)

    def test_stored_process_median_drift_refused_before_callback(self):
        comparison = self.inputs.json["analysis/measured-492817/M2skip/report.json"]["comparisons"][0]
        comparison["block_medians_numerator"][0] += 1
        with self.assertRaisesRegex(ValueError, "stored process median differs"):
            self.collect()
        self.assertFalse(self.calls)


if __name__ == "__main__":
    unittest.main()
