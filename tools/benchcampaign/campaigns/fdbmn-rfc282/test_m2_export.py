import copy
import csv
import io
import unittest
from unittest.mock import Mock, patch

from helpers import legacy_m2
import test_m2_throughput as fixture_source
import m2_export


class Tests(unittest.TestCase):
    def collect(self, counts):
        with patch.object(legacy_m2, 'PHASES', counts):
            inputs = fixture_source.fixture()
        expected = {phase: [c['key'] for c in inputs.json[f'results-raw/{phase}/plan.json']['cases']] for phase in counts}
        calls = []
        def paired(ratios, *, resamples, seed):
            self.assertEqual((resamples, seed), (20000, 492817))
            self.assertEqual(ratios, [2, 2, 2, 2, 2, 2])
            calls.append(ratios)
            return dict(gm=2, nominal_95_interval=[1.5, 2.5], blocks=6, resamples=resamples, seed=seed)
        rows, method = m2_export.collect(inputs, paired, expected, 'analysis/measured-492817')
        self.assertEqual(len(calls), 2 * sum(clean for _, clean in counts.values()))
        return rows, method

    def test_full_sixteen_clean_not_historical_twelve(self):
        rows, method = self.collect({'M2rl': (12, 12), 'M2skip': (4, 4)})
        self.assertEqual(method['eligible_cases'], 16)
        self.assertEqual(method['process_rows'], 192)
        self.assertEqual(method['source_repeat_samples'], 576)
        self.assertEqual(sum(r['record_type'] == 'omitted' for r in rows), 0)

    def test_new_collector_preserves_process_absolute_and_paired_numerical_values(self):
        with patch.object(legacy_m2, 'PHASES', {'M2rl': (12, 12), 'M2skip': (4, 4)}):
            inputs = fixture_source.fixture()
        expected = {phase: [c['key'] for c in inputs.json[f'results-raw/{phase}/plan.json']['cases']]
                    for phase in ('M2rl', 'M2skip')}
        for job in inputs.json['results-raw/M2rl/plan.json']['jobs']:
            if job['case'] != 'case-0':
                continue
            block, arm = job['block'], job['arm']
            user, system, wall = ((block + 1, (block + 1) / 2, 4_000_000_000)
                                 if arm == 'final-ordinary' else (2 * (block + 2), block + 2, 8_000_000_000))
            folder = 'results-raw/M2rl/' + job['id']
            inputs.json[folder + '/result.json'].update(process_user_cpu_seconds=user,
                process_system_cpu_seconds=system, process_wall_ns=wall)
            inputs.strings[folder + '/process-time.txt'] = (
                f'User time (seconds): {user:.2f}\nSystem time (seconds): {system:.2f}\n'
                'Percent of CPU this job got: 50%\n'
                'Elapsed (wall clock) time (h:mm:ss or m:ss): 0:03.99\nExit status: 0\n')
        calls = []
        def paired(ratios, *, resamples, seed):
            self.assertEqual((resamples, seed), (20000, 492817))
            self.assertEqual(ratios, [2, 1.5, 4 / 3, 1.25, 1.2, 7 / 6] if len(calls) == 1 else [2] * 6)
            calls.append(ratios[:])
            return dict(gm=3 if len(calls) == 2 else 2, nominal_95_interval=[1.5, 3.5],
                        blocks=6, resamples=resamples, seed=seed)
        rows, method = m2_export.collect(inputs, paired, expected, 'analysis/measured-492817')
        self.assertEqual(len(calls), 32)
        selected = [r for r in rows if r['phase'] == 'M2rl' and r['case'] == 'case-0']
        process = next(r for r in selected if r['record_type'] == 'process' and r['arm'] == 'final-ordinary' and r['block'] == 0)
        self.assertEqual(process['throughput_ops_per_second'], 1000)
        self.assertEqual(len(process['repeat_throughput_ops_per_second']), 3)
        for actual, wanted in zip(process['repeat_throughput_ops_per_second'], [1250, 1000, 833.3333333333334]):
            self.assertAlmostEqual(actual, wanted)
        self.assertEqual(process['repeat_ns_per_op'], [800000, 1000000, 1200000])
        self.assertEqual(process['repeat_N'], [1, 10000, 2])
        self.assertEqual(process['repeat_rows_per_op'], [997, 997, 997])
        self.assertEqual(process['lifecycle_cpu_util_cores'], 0.375)
        self.assertEqual(process['process_user_cpu_seconds'], 1)
        self.assertEqual(process['process_system_cpu_seconds'], 0.5)
        self.assertEqual(process['process_wall_ns'], 4_000_000_000)
        self.assertEqual(process['gnu_time_elapsed_seconds'], 3.99)
        self.assertNotAlmostEqual(process['lifecycle_cpu_util_cores'], 1.5 / 3.99)
        absolute = {r['arm']: r for r in selected if r['record_type'] == 'absolute'}
        self.assertAlmostEqual(absolute['final-ordinary']['throughput_ops_per_second'], 291.66666666666663)
        self.assertAlmostEqual(absolute['final-remote']['throughput_ops_per_second'], 583.3333333333333)
        self.assertNotAlmostEqual(absolute['final-ordinary']['throughput_ops_per_second'], 1e9 / 3_500_000)
        self.assertEqual(absolute['final-ordinary']['lifecycle_cpu_util_cores'], 1.3125)
        self.assertEqual(absolute['final-remote']['lifecycle_cpu_util_cores'], 1.6875)
        self.assertEqual(absolute['final-ordinary']['process_lifecycle_cpu_util_cores'], [0.375, 0.75, 1.125, 1.5, 1.875, 2.25])
        self.assertEqual(absolute['final-remote']['process_lifecycle_cpu_util_cores'], [0.75, 1.125, 1.5, 1.875, 2.25, 2.625])
        paired_rows = {r['metric']: r for r in selected if r['record_type'] == 'paired'}
        throughput, cpu = paired_rows['throughput_ops_per_second'], paired_rows['lifecycle_cpu_util_cores']
        self.assertEqual(throughput['block_ratios'], [2] * 6)
        self.assertEqual(len(throughput['block_values_denominator']), 6)
        self.assertEqual(len(throughput['block_values_numerator']), 6)
        for actual, wanted in zip(throughput['block_values_numerator'], [2000, 1000, 666.6666666666666, 500, 200, 100]):
            self.assertAlmostEqual(actual, wanted)
        for actual, wanted in zip(throughput['block_values_denominator'], [1000, 500, 333.3333333333333, 250, 100, 50]):
            self.assertAlmostEqual(actual, wanted)
        self.assertEqual(cpu['block_values_numerator'], [0.75, 1.125, 1.5, 1.875, 2.25, 2.625])
        self.assertEqual(cpu['block_values_denominator'], [0.375, 0.75, 1.125, 1.5, 1.875, 2.25])
        self.assertEqual(cpu['block_ratios'], [2, 1.5, 4 / 3, 1.25, 1.2, 7 / 6])
        self.assertEqual((throughput['ratio'], cpu['ratio']), (2, 3))
        self.assertEqual((cpu['ci95_low'], cpu['ci95_high']), (1.5, 3.5))
        self.assertEqual((cpu['numerator'], cpu['denominator']), ('final-remote', 'final-ordinary'))
        self.assertFalse(method['measured_window_cpu_available'])
        self.assertTrue(all(r['cpu_util_cores'] == '' for r in rows))

    def test_arbitrary_omissions_no_partial_process_salvage(self):
        rows, method = self.collect({'M2rl': (12, 2), 'M2skip': (4, 1)})
        self.assertEqual((method['eligible_cases'], method['omitted_cases'], method['process_rows']), (3, 13, 36))
        self.assertTrue(all(r['source_processes'] == 1 for r in rows if r['record_type'] == 'process'))

    def test_zero_clean_emits_only_omission_rows_not_estimators(self):
        rows, method = self.collect({'M2rl': (12, 0), 'M2skip': (4, 0)})
        self.assertEqual((method['eligible_cases'], method['process_rows']), (0, 0))
        self.assertEqual(len(rows), 16)
        self.assertEqual({r['record_type'] for r in rows}, {'omitted'})

    def test_new_collector_refuses_each_raw_mismatch_before_estimation(self):
        reasons = dict(missing_job='incomplete planned M2 process matrix',
                       duplicate_job='duplicate/invalid M2 run identity',
                       missing_repeat='incomplete M2 three-repeat process', duplicate_repeat='duplicate M2 sample',
                       stored_median='M2 raw CSV/stored process median differs',
                       process_identity='M2 successful process receipt identity differs',
                       user_cpu='GNU time/process CPU receipt differs or is zero',
                       system_cpu='GNU time/process CPU receipt differs or is zero')
        for mutation, reason in reasons.items():
            with self.subTest(mutation=mutation):
                with patch.object(legacy_m2, 'PHASES', {'M2rl': (12, 1), 'M2skip': (4, 0)}):
                    inputs = fixture_source.fixture()
                expected = {phase: [case['key'] for case in inputs.json[f'results-raw/{phase}/plan.json']['cases']]
                            for phase in ('M2rl', 'M2skip')}
                estimate = Mock(return_value=dict(gm=2, nominal_95_interval=[2, 2], blocks=6, resamples=20000, seed=492817))
                healthy, method = m2_export.collect(inputs, estimate, expected, 'analysis/measured-492817')
                self.assertEqual((method['eligible_cases'], method['process_rows'], estimate.call_count), (1, 12, 2))
                first = next(row for row in healthy if row['record_type'] == 'process')
                self.assertEqual((first['throughput_ops_per_second'], first['lifecycle_cpu_util_cores']), (1000, 0.5))
                estimate.reset_mock()
                jobs = inputs.json['results-raw/M2rl/plan.json']['jobs']
                first_id = jobs[0]['id']
                if mutation == 'missing_job': jobs.pop()
                if mutation == 'duplicate_job': jobs.append(copy.deepcopy(jobs[0]))
                if mutation in ('missing_repeat', 'duplicate_repeat'):
                    path = 'results-raw/M2rl/metrics.csv'
                    values = list(csv.DictReader(io.StringIO(inputs.strings[path])))
                    if mutation == 'missing_repeat': values.pop(0)
                    else: values.append(copy.deepcopy(values[0]))
                    inputs.strings[path] = fixture_source.csv_text(values)
                if mutation == 'stored_median':
                    inputs.json['analysis/measured-492817/M2rl/report.json']['comparisons'][0]['block_medians_denominator'][0] += 1
                receipt = inputs.json['results-raw/M2rl/' + first_id + '/result.json']
                if mutation == 'process_identity': receipt['run_id'] = 'foreign-process'
                if mutation == 'user_cpu': receipt['process_user_cpu_seconds'] += 1
                if mutation == 'system_cpu': receipt['process_system_cpu_seconds'] += 1
                with self.assertRaisesRegex(ValueError, reason):
                    m2_export.collect(inputs, estimate, expected, 'analysis/measured-492817')
                estimate.assert_not_called()

    def test_foreign_expected_population_refused(self):
        inputs = fixture_source.fixture()
        with self.assertRaises(ValueError):
            m2_export.collect(inputs, lambda *a, **k: self.fail('must not estimate'),
                              {'M2rl': ['foreign'], 'M2skip': ['foreign']}, 'analysis/measured-492817')


if __name__ == '__main__':
    unittest.main()
