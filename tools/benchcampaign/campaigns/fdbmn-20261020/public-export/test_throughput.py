#!/usr/bin/env python3
"""Tiny throughput-export controls; no retained inputs or estimators are used."""
import copy
import csv
import io
import random
import unittest

import export_throughput as exporter


SAMPLE_FIELDS = ("phase", "index", "worker", "start_offset_ns", "latency_ns",
                 "attempts", "error", "check_status")


def sample_csv(rows):
    stream = io.StringIO(newline="")
    writer = csv.DictWriter(stream, fieldnames=SAMPLE_FIELDS, lineterminator="\n")
    writer.writeheader()
    writer.writerows(rows)
    return stream.getvalue()


def process_fixture():
    raw = {
        "success": True, "completed": 4, "requested": 4, "errors": 0,
        "warmup_errors": 0, "persisted_validation": "ok", "retries": 1,
        "measured_wall_ns": 1_000_000_000, "warmup_wall_ns": 7_000_000_000,
        "process_wall_ns": 90_000_000_000,
        "config": {"workload": "get100", "workers": 2,
                   "run_id": "tiny-b000-final", "block": 0},
        "stats_before": {"process_user_cpu_us": 100, "process_system_cpu_us": 200},
        "stats_after": {"process_user_cpu_us": 200_100, "process_system_cpu_us": 50_200},
        "stats_after_identity": {"process_user_cpu_us": 1, "process_system_cpu_us": 2},
        "lifecycle": {"stats_after_shutdown": {
            "process_user_cpu_us": 999_999_999, "process_system_cpu_us": 999_999_999}},
    }
    retained = {
        "workload": "get100", "workers": 2, "run_id": "tiny-b000-final",
        "block": 0, "position": 1, "arm": "final", "successes": 4,
        "observations": 4, "errors": 0, "retries": 1,
        "mean_ns": 200_000_000, "measured_wall_ns": 1_000_000_000,
        "process_cpu_ns": 250_000_000, "closed_loop_ops_per_second": 4.0,
        "process_cpu_over_wall": 0.25,
    }
    samples = [
        dict(phase="measure", index=i, worker=i % 2, start_offset_ns=start,
             latency_ns=200_000_000, attempts=2 if i == 2 else 1,
             error="", check_status="ok")
        for i, start in enumerate((0, 100_000_000, 500_000_000, 600_000_000))
    ]
    return raw, retained, samples


class MeasuredProcessTests(unittest.TestCase):
    def test_known_rate_cpu_and_worker_time_values_not_lifecycle_or_process_wall(self):
        raw, retained, samples = process_fixture()
        result = exporter.measured_process(raw, retained, sample_csv(samples))
        expected = {
            "record_type": "process", "arm": "final", "block": 0,
            "position": 1, "run_id": "tiny-b000-final", "status": "ok",
            "runs": 1, "workers": 2, "measured_ops": 4,
            "measured_wall_ns": 1_000_000_000, "window_cpu_ns": 250_000_000,
            "throughput_ops_per_second": 4.0, "cpu_util_cores": 0.25,
            "mean_latency_ns": 200_000_000, "measured_latency_sum_ns": 800_000_000,
            "retries": 1, "in_operation_concurrency": 0.8,
            "workers_over_mean_ops_per_second": 10.0,
            "closed_loop_throughput_ratio": 0.4,
        }
        self.assertEqual(result, expected)
        self.assertIsInstance(result["measured_latency_sum_ns"], int)

    def test_multicore_cpu_is_not_divided_by_workers(self):
        raw, retained, samples = process_fixture()
        raw["stats_after"] = {"process_user_cpu_us": 2_000_100,
                              "process_system_cpu_us": 500_200}
        retained.update(process_cpu_ns=2_500_000_000, process_cpu_over_wall=2.5)
        result = exporter.measured_process(raw, retained, sample_csv(samples))
        self.assertEqual(result["window_cpu_ns"], 2_500_000_000)
        self.assertEqual(result["cpu_util_cores"], 2.5)
        self.assertEqual(result["throughput_ops_per_second"], 4.0)

    def test_shuffled_csv_and_touching_worker_intervals_are_valid(self):
        raw, retained, samples = process_fixture()
        samples[2]["start_offset_ns"] = 200_000_000
        samples[3]["start_offset_ns"] = 300_000_000
        result = exporter.measured_process(raw, retained, sample_csv(list(reversed(samples))))
        self.assertEqual(result["measured_latency_sum_ns"], 800_000_000)
        self.assertEqual(result["throughput_ops_per_second"], 4.0)

    def test_failed_or_incomplete_counts_are_refused(self):
        mutations = (
            ("raw", "success", False), ("raw", "errors", 1),
            ("raw", "warmup_errors", 1), ("raw", "persisted_validation", "error"),
            ("raw", "requested", 5), ("raw", "completed", 3),
            ("retained", "successes", 3), ("retained", "observations", 5),
            ("retained", "errors", 1),
        )
        for target, key, value in mutations:
            with self.subTest(target=target, key=key):
                raw, retained, samples = process_fixture()
                (raw if target == "raw" else retained)[key] = value
                with self.assertRaises(ValueError):
                    exporter.measured_process(raw, retained, sample_csv(samples))
        for count in (0, -1):
            with self.subTest(count=count):
                raw, retained, samples = process_fixture()
                raw.update(completed=count, requested=count)
                retained.update(successes=count, observations=count)
                with self.assertRaises(ValueError):
                    exporter.measured_process(raw, retained, sample_csv(samples))

    def test_nonpositive_or_nonfinite_wall_is_refused(self):
        for value in (0, -1, float("nan"), float("inf")):
            with self.subTest(wall=value):
                raw, retained, samples = process_fixture()
                raw["measured_wall_ns"] = value
                with self.assertRaises(ValueError):
                    exporter.measured_process(raw, retained, sample_csv(samples))

    def test_raw_summary_identity_must_match_retained_process(self):
        for key, value in (("workload", "set100"), ("workers", 3),
                           ("run_id", "different-run"), ("block", 1)):
            with self.subTest(key=key):
                raw, retained, samples = process_fixture()
                raw["config"][key] = value
                with self.assertRaises(ValueError):
                    exporter.measured_process(raw, retained, sample_csv(samples))

    def test_cpu_counters_are_finite_nondecreasing_and_nonzero(self):
        mutations = (
            ("stats_before", "process_user_cpu_us", -1),
            ("stats_before", "process_system_cpu_us", float("nan")),
            ("stats_after", "process_user_cpu_us", float("inf")),
            ("stats_after", "process_user_cpu_us", 99),
            ("stats_after", "process_system_cpu_us", 199),
        )
        for snapshot, key, value in mutations:
            with self.subTest(snapshot=snapshot, key=key, value=value):
                raw, retained, samples = process_fixture()
                raw[snapshot][key] = value
                with self.assertRaises(ValueError):
                    exporter.measured_process(raw, retained, sample_csv(samples))
        raw, retained, samples = process_fixture()
        raw["stats_after"] = copy.deepcopy(raw["stats_before"])
        with self.assertRaises(ValueError):
            exporter.measured_process(raw, retained, sample_csv(samples))

    def test_retained_rate_wall_and_cpu_must_match_raw_counters(self):
        for key, value in (("measured_wall_ns", 1_000_000_001),
                           ("process_cpu_ns", 250_000_001),
                           ("closed_loop_ops_per_second", 5.0),
                           ("process_cpu_over_wall", 0.5)):
            with self.subTest(key=key):
                raw, retained, samples = process_fixture()
                retained[key] = value
                with self.assertRaises(ValueError):
                    exporter.measured_process(raw, retained, sample_csv(samples))

    def test_sample_population_phase_worker_and_checks_are_validated(self):
        mutations = (
            (0, "index", 1), (0, "index", "not-an-index"),
            (0, "phase", "warmup"), (0, "worker", 1), (0, "worker", 2),
            (0, "error", "failed"), (0, "check_status", "error"),
            (0, "attempts", 0), (0, "latency_ns", "1.5"),
        )
        for index, key, value in mutations:
            with self.subTest(key=key, value=value):
                raw, retained, samples = process_fixture()
                samples[index][key] = value
                with self.assertRaises(ValueError):
                    exporter.measured_process(raw, retained, sample_csv(samples))
        raw, retained, samples = process_fixture()
        with self.assertRaises(ValueError):
            exporter.measured_process(raw, retained, sample_csv(samples[:-1]))

    def test_sample_windows_are_positive_contained_and_nonoverlapping(self):
        mutations = (
            (0, "latency_ns", 0), (0, "latency_ns", -1),
            (0, "start_offset_ns", -1), (0, "start_offset_ns", 900_000_000),
            (2, "start_offset_ns", 100_000_000),
        )
        for index, key, value in mutations:
            with self.subTest(index=index, key=key, value=value):
                raw, retained, samples = process_fixture()
                samples[index][key] = value
                with self.assertRaises(ValueError):
                    exporter.measured_process(raw, retained, sample_csv(samples))

    def test_raw_latency_mean_and_retry_totals_must_match_retained(self):
        for target in ("mean", "raw-retries", "retained-retries", "sample-retries"):
            with self.subTest(target=target):
                raw, retained, samples = process_fixture()
                if target == "mean":
                    retained["mean_ns"] = 200_000_001
                elif target == "raw-retries":
                    raw["retries"] = 0
                elif target == "retained-retries":
                    retained["retries"] = 0
                else:
                    samples[2]["attempts"] = 1
                with self.assertRaises(ValueError):
                    exporter.measured_process(raw, retained, sample_csv(samples))


def paired_processes():
    rates = {"before": (10, 20, 30, 40, 50, 60),
             "final": (21, 42, 63, 84, 105, 126),
             "cgo": (7, 14, 21, 28, 35, 42)}
    return list(reversed([
        dict(block=block, arm=arm, throughput_ops_per_second=values[block])
        for block in range(6) for arm, values in rates.items()
    ]))


class PairedComparisonTests(unittest.TestCase):
    def test_seeded_callback_pairs_parameters_and_whole_block_draw_alignment(self):
        calls = []
        estimates = (
            dict(ratio=1.25, ci95_low=1.0, ci95_high=1.5, blocks=6),
            dict(ratio=2.5, ci95_low=2.0, ci95_high=3.0, blocks=6),
            dict(ratio=0.5, ci95_low=0.4, ci95_high=0.6, blocks=6),
        )

        def bootstrap(pairs, rng, resamples):
            state = rng.getstate()
            draws = [rng.randrange(6) for _ in range(12)]
            calls.append((pairs, state, resamples, draws))
            return estimates[len(calls) - 1]

        result = exporter.paired_comparisons(paired_processes(), bootstrap)
        self.assertEqual(len(calls), 3)
        expected_pairs = (
            [(21, 10), (42, 20), (63, 30), (84, 40), (105, 50), (126, 60)],
            [(21, 7), (42, 14), (63, 21), (84, 28), (105, 35), (126, 42)],
            [(10, 7), (20, 14), (30, 21), (40, 28), (50, 35), (60, 42)],
        )
        seeded = random.Random(492817)
        state = seeded.getstate()
        expected_draws = [seeded.randrange(6) for _ in range(12)]
        for call, pairs in zip(calls, expected_pairs):
            self.assertEqual(call, (pairs, state, 20000, expected_draws))
        for row, contrast, estimate in zip(result, (("final", "before"), ("final", "cgo"), ("before", "cgo")), estimates):
            self.assertEqual(row, dict(record_type="paired", status="ok",
                metric="throughput_ops_per_second", numerator=contrast[0],
                denominator=contrast[1], runs=18, seed=492817, resamples=20000,
                ratio_gt_1_means="numerator has more measured operations/s", **estimate))

    def test_missing_duplicate_and_unexpected_blocks_or_arms_are_refused(self):
        def forbidden_bootstrap(*args):
            self.fail("incomplete population reached the numerical callback")

        populations = []
        original = paired_processes()
        populations.append(original[:-1])
        duplicate = copy.deepcopy(original)
        duplicate[-1] = copy.deepcopy(duplicate[0])
        populations.append(duplicate)
        unknown_arm = copy.deepcopy(original)
        unknown_arm[0]["arm"] = "unknown"
        populations.append(unknown_arm)
        unknown_block = copy.deepcopy(original)
        unknown_block[0]["block"] = 6
        populations.append(unknown_block)
        populations.append(original + [copy.deepcopy(original[0])])
        for index, population in enumerate(populations):
            with self.subTest(population=index):
                with self.assertRaises(ValueError):
                    exporter.paired_comparisons(population, forbidden_bootstrap)

    def test_callback_cannot_reduce_the_six_block_population(self):
        def reduced(pairs, rng, resamples):
            return dict(ratio=1.0, ci95_low=0.9, ci95_high=1.1, blocks=5)

        with self.assertRaises(ValueError):
            exporter.paired_comparisons(paired_processes(), reduced)


class FormattingTests(unittest.TestCase):
    def test_csv_union_columns_quoting_missing_cells_and_line_endings(self):
        rows = [dict(record_type="process", case="get,100", note='a"b\nc', rate=4.0),
                dict(record_type="omitted", case="absent", reason="not run")]
        text = exporter.csv_text(rows)
        self.assertTrue(text.endswith("\n"))
        self.assertNotIn("\r\n", text)
        parsed = csv.DictReader(io.StringIO(text))
        self.assertEqual(parsed.fieldnames,
            ["post_hoc", "preregistered", "record_type", "case", "note", "rate", "reason"])
        self.assertEqual(list(parsed), [
            dict(post_hoc="true", preregistered="false", record_type="process",
                 case="get,100", note='a"b\nc', rate="4.0", reason=""),
            dict(post_hoc="true", preregistered="false", record_type="omitted",
                 case="absent", note="", rate="", reason="not run"),
        ])

    def test_csv_nested_cells_are_compact_sorted_json_without_mutating_input(self):
        rows = [dict(record_type="paired", interval=[0.5, 1.5],
                     diagnostics={"z": 2, "a": {"second": 2, "first": 1}})]
        original = copy.deepcopy(rows)
        parsed = list(csv.DictReader(io.StringIO(exporter.csv_text(rows))))
        self.assertEqual(parsed, [dict(post_hoc="true", preregistered="false",
            record_type="paired", interval="[0.5,1.5]",
            diagnostics='{"a":{"first":1,"second":2},"z":2}')])
        self.assertEqual(rows, original)

    def test_table_arm_contrast_order_units_and_precision(self):
        rows = [dict(record_type="absolute", arm=arm, throughput_ops_per_second=rate,
                     cpu_util_cores=cores, phase="M1w8", case="get100")
                for arm, rate, cores in (("cgo", 3000.4, 1.25),
                                         ("before", 1000.4, 0.125),
                                         ("final", 2000.4, 0.5))]
        rows += [dict(record_type="paired", numerator=n, denominator=d,
                      ratio=ratio, ci95_low=low, ci95_high=high,
                      phase="M1w8", case="get100")
                 for n, d, ratio, low, high in (("before", "cgo", 0.5, 0.4, 0.6),
                                              ("final", "cgo", 1.25, 1.1, 1.4),
                                              ("final", "before", 0.8, 0.7, 0.9))]
        rows.append(dict(record_type="omitted", phase="M1w32", case="get100", status="N/A"))
        self.assertEqual(exporter.render_table(rows),
            "|Case|Ops/s previous / new / CGo|New/previous [95%]|New/CGo [95%]|Previous/CGo [95%]|Busy cores previous / new / CGo|\n"
            "|---|---:|---|---|---|---:|\n"
            "|w8/get100|1,000 / 2,000 / 3,000|0.800 [0.700, 0.900]|1.250 [1.100, 1.400]|0.500 [0.400, 0.600]|0.125 / 0.500 / 1.250|\n")

    def test_table_does_not_invent_rows_for_omitted_cases(self):
        text = exporter.render_table([
            dict(record_type="omitted", phase="M1w32", case="get100", status="N/A")])
        self.assertEqual(len(text.splitlines()), 2)
        self.assertNotIn("w32/get100", text)


if __name__ == "__main__":
    unittest.main()
