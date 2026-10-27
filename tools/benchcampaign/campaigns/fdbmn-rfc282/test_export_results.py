"""Synthetic population controls; no private archive, estimator, SSH or cloud."""
import copy
import itertools
import hashlib
import json
import unittest
from unittest.mock import Mock, patch
from decimal import Decimal
from types import SimpleNamespace
import test_server_cpu

import export_results as export


class Inputs:
    def __init__(self, values):
        self.values = values

    def load(self, path):
        return copy.deepcopy(self.values[path])

    def text(self, path):
        return self.values.get(path, 'synthetic CSV validated by the existing measured_process tests')

    def digest(self, path):
        value = self.values[path]
        data = value if isinstance(value, str) else json.dumps(value, sort_keys=True)
        return hashlib.sha256(data.encode()).hexdigest()

    def repin_analysis(self):
        for phase in export.protocol_cases(protocol())[1]:
            suffix = 'summary.json' if phase.startswith('M1') else 'report.json'
            self.values[f'analysis/current/{phase}/{suffix}']['r4_reconciliation_sha256'] = self.digest(f'reconcile/{phase}/report.json')


def protocol():
    phases = []
    for workers, (iterations, warmup) in export.QUOTAS.items():
        phases.append(dict(id=f'M1w{workers}', kind='client', workers=workers, iterations=iterations,
            warmup=warmup, blocks=6, arms=['before-Go', 'final-Go', 'CGo-7.3.77'], workloads=list(export.WORKLOADS)))
    for phase, cells in [('M2rl', ['limit1', 'rows10', 'rows1000']), ('M2skip', ['rows10', 'rows1000'])]:
        phases.append(dict(id=phase, kind='testingb', blocks=6, repeats_per_arm_process=3, benchtime='3s',
            arms=['final-ordinary', 'final-remote'], dirs=['forward', 'reverse'], cells=cells, payloads=['small', 'split']))
    return dict(phases=phases, sample_policy=dict(analysis_seed=492817, bootstrap_resamples=20000))


def population_fixture():
    p = protocol()
    specs, expected = export.protocol_cases(p)
    values = {}
    for phase, cases in expected.items():
        recs = [dict(case=c, verdict='clean', contaminated_reasons=[], unverifiable_reasons=[], runs=[]) for c in cases]
        values[f'reconcile/{phase}/report.json'] = dict(phase=phase, reconciliation_complete=True, cases=recs)
        if phase.startswith('M1'):
            analyses = {}
            for case in cases:
                runs, jobs = [], []
                for block, order in enumerate(itertools.permutations(export.ARMS)):
                    for pos, arm in enumerate(order):
                        run_id = f'b{block}-p{pos}-{arm}'
                        r = dict(run_id=run_id, arm=arm, block=block, position=pos, workload=case, workers=specs[phase]['workers'])
                        r.update({metric: 10 for metric in export.METRICS})
                        runs.append(r)
                        jobs.append(dict(run_id=run_id, block=block, position=pos, arm=arm, workload=dict(
                            name=case, workers=specs[phase]['workers'], iterations=specs[phase]['iterations'], warmup=specs[phase]['warmup'])))
                        values[f'results-raw/{phase}/{case}/{run_id}/summary.json'] = dict(
                            requested=specs[phase]['iterations'], warmup_completed=specs[phase]['warmup'])
                comps = [dict(metric=m, numerator=n, denominator=d, ratio=1, ci95_low=1, ci95_high=1, blocks=6)
                         for n, d in export.legacy_m1.CONTRASTS for m in export.METRICS]
                analyses[case] = dict(status='clean', per_run=runs, comparisons=comps)
                next(r for r in recs if r['case'] == case)['runs'] = [dict(run_id=r['run_id']) for r in runs]
                values[f'results-raw/{phase}/{case}/schedule.json'] = jobs
            values[f'analysis/current/{phase}/summary.json'] = dict(workloads=analyses)
        else:
            values[f'analysis/current/{phase}/report.json'] = dict(cases={c: dict(status='ok') for c in cases})
    inputs = Inputs(values)
    inputs.repin_analysis()
    return inputs, specs, expected


class PopulationTests(unittest.TestCase):
    def test_full_44_case_population_including_seven_w32(self):
        inputs, specs, expected = population_fixture()
        coverage, reports, analyses = export.population(inputs, 'analysis/current', expected)
        self.assertEqual((coverage['planned'], coverage['clean'], coverage['contaminated'], coverage['unverifiable']), (44, 44, 0, 0))
        def measured(raw, retained, samples):
            return dict(arm=retained['arm'], block=retained['block'], position=retained['position'],
                        run_id=retained['run_id'], throughput_ops_per_second=100, cpu_util_cores=1)
        def bootstrap(pairs, rng, resamples):
            self.assertEqual((len(pairs), resamples), (6, 20000))
            return dict(blocks=6, ratio=1, ci95_low=1, ci95_high=1)
        with patch.object(export.legacy_m1, 'measured_process', side_effect=measured):
            rows, comparisons = export.collect_m1(inputs, specs, expected, analyses, reports, bootstrap)
        self.assertEqual(len(rows), 504)
        self.assertEqual(len([r for r in rows if r['phase'] == 'M1w32']), 126)
        self.assertEqual(len(comparisons), 28 * (3 + 18))

    def test_partial_and_unverifiable_cases_cannot_export_partial_arms(self):
        inputs, specs, expected = population_fixture()
        for phase, verdict in [('M1w32', 'contaminated'), ('M1w1', 'unverifiable'), ('M2rl', 'unverifiable')]:
            rec = inputs.values[f'reconcile/{phase}/report.json']['cases'][0]
            rec.update(verdict=verdict)
            rec[verdict + '_reasons'] = ['synthetic gate failure']
            path = f'analysis/current/{phase}/' + ('summary.json' if phase.startswith('M1') else 'report.json')
            key = 'workloads' if phase.startswith('M1') else 'cases'
            inputs.values[path][key][expected[phase][0]]['status'] = 'N/A'
        inputs.repin_analysis()
        coverage, reports, analyses = export.population(inputs, 'analysis/current', expected)
        self.assertEqual((coverage['clean'], coverage['contaminated'], coverage['unverifiable']), (41, 1, 2))
        self.assertEqual(analyses['M1w32']['workloads']['grv']['status'], 'N/A')
        # Remove all data for omitted cases: exporting must not read it.
        for phase in ('M1w32', 'M1w1'):
            inputs.values.pop(f'results-raw/{phase}/grv/schedule.json')
        def measured(raw, retained, samples):
            return dict(arm=retained['arm'], block=retained['block'], run_id=retained['run_id'], throughput_ops_per_second=1)
        with patch.object(export.legacy_m1, 'measured_process', side_effect=measured):
            rows, _ = export.collect_m1(inputs, specs, expected, analyses, reports,
                lambda pairs, rng, n: dict(blocks=6, ratio=1))
        self.assertEqual(len(rows), 26 * 18)
        self.assertFalse(any(r['case'] == 'grv' and r['phase'] in ('M1w32', 'M1w1') for r in rows))

    def test_zero_clean_is_coverage_not_vacuous_performance_success(self):
        inputs, _, expected = population_fixture()
        for phase, cases in expected.items():
            for r in inputs.values[f'reconcile/{phase}/report.json']['cases']:
                r.update(verdict='unverifiable', unverifiable_reasons=['missing'])
            suffix, key = ('summary.json', 'workloads') if phase.startswith('M1') else ('report.json', 'cases')
            for case in cases:
                inputs.values[f'analysis/current/{phase}/{suffix}'][key][case]['status'] = 'N/A'
        inputs.repin_analysis()
        coverage, _, _ = export.population(inputs, 'analysis/current', expected)
        self.assertEqual((coverage['clean'], coverage['unverifiable']), (0, 44))

    def test_missing_duplicate_foreign_and_disagreed_population_refused(self):
        for mutation in ('missing', 'duplicate', 'foreign', 'eligibility', 'in_progress'):
            with self.subTest(mutation=mutation):
                inputs, _, expected = population_fixture()
                report = inputs.values['reconcile/M1w32/report.json']
                if mutation == 'missing': report['cases'].pop()
                if mutation == 'duplicate': report['cases'].append(copy.deepcopy(report['cases'][0]))
                if mutation == 'foreign': report['cases'][0]['case'] = 'not-a-case'
                if mutation == 'eligibility': report['cases'][0]['verdict'] = 'unverifiable'
                if mutation == 'in_progress': report['reconciliation_complete'] = False
                inputs.repin_analysis()
                with self.assertRaises(ValueError): export.population(inputs, 'analysis/current', expected)

    def test_canonical_analysis_cannot_reference_foreign_hash_pinned_r4(self):
        inputs, _, expected = population_fixture()
        inputs.values['analysis/current/M1w32/summary.json']['r4_reconciliation_sha256'] = '0' * 64
        with self.assertRaisesRegex(ValueError, 'different R4 report'):
            export.population(inputs, 'analysis/current', expected)

    def test_matrix_changes_are_not_generic_population_relaxations(self):
        for mutation in ('w32', 'quota', 'workload', 'phase'):
            p = protocol()
            if mutation == 'w32': p['phases'][2]['workers'] = 31
            if mutation == 'quota': p['phases'][0]['iterations'] = 2999
            if mutation == 'workload': p['phases'][0]['workloads'].pop()
            if mutation == 'phase': p['phases'].pop()
            with self.assertRaises(ValueError): export.protocol_cases(p)

    def test_m2_foreign_raw_windows_omit_whole_case_cpu_not_one_good_arm(self):
        layout, units, telemetry, _, run = test_server_cpu.fixture()
        report = dict(cases=[dict(case='small-limit1-forward', verdict='clean', runs=[])],
                      nodes={n: dict(units=sorted(units[n])) for n in units},
                      clock=dict(client=dict(bound_s='0')), inputs_sha256={})
        values = {}
        for node, (start, stop, samples) in telemetry.items():
            start['phase'] = stop['phase'] = 'M2rl'
            start['chrony'] = stop['chrony'] = dict(rc=0, raw='synthetic synced chrony')
            root = f'telemetry-cpu/M2rl/{node}'
            values[root + '/samples.jsonl'] = ''.join(json.dumps(s) + '\n' for s in samples)
            stop['samples_sha256'] = hashlib.sha256(values[root + '/samples.jsonl'].encode()).hexdigest()
            for kind, receipt in [('start', start), ('stop', stop)]:
                values[root + f'/receipt-{kind}.json'] = receipt
                base = f'telemetry/M2rl/{node}/receipt-{kind}.json'
                values[base] = copy.deepcopy(receipt)
        inputs = Inputs(values)
        for path in list(values):
            if path.startswith('telemetry/'):
                report['inputs_sha256']['/srv/fdb-bench/' + path] = inputs.digest(path)
        rows = []
        for block, arm in [(0, 'final-ordinary'), (0, 'final-remote')]:
            run_id = arm
            report['cases'][0]['runs'].append(dict(run, run_id=run_id))
            rows.append(dict(phase='M2rl', arm=arm, block=block, source_run_id=run_id))
            values[f'results-raw/M2rl/{run_id}/result.json'] = dict(run_id=run_id,
                started_utc=run['start_utc'], ended_utc=run['end_utc'])
        parser = SimpleNamespace(parse_chrony_tracking=lambda text: ({}, []), chrony_error_bound=lambda fields: Decimal('0'))
        # Positive control: both rows were estimable before the exact raw mutation.
        self.assertEqual(len(export.cpu_rows(inputs, 'M2rl', report, layout, rows, parser)), 2)
        values['results-raw/M2rl/final-remote/result.json']['ended_utc'] = '1970-01-01T00:00:01.8Z'
        result = export.cpu_rows(inputs, 'M2rl', report, layout, rows, parser)
        self.assertEqual(len(result), 1)
        self.assertEqual(result[0]['status'], 'CPU-unqualified')
        self.assertIn('R4/raw lifecycle window differs', result[0]['reason'])
        self.assertNotIn('bracket_cpu_seconds', result[0])

    def test_export_wiring_retains_full_population_absolutes_and_source_identities(self):
        inputs, _, expected = population_fixture()
        p = protocol()
        p.update(production_revisions=dict(before_allocation_work_after_read_parity='1' * 40, final='2' * 40),
                 measurement_extensions=dict(throughput_preregistered=True, server_cpu_source='exclusive-cgroup-v2-usage-usec',
                     process_cpu_interval_ns=10_000_000, process_cpu_min_inner_intervals=10, process_cpu_max_time_envelope_ratio=1.2,
                     process_cpu_clock_reserve_ns=2_000_000, process_cpu_phase_correlation_range_ns=1_000_000),
                 runtime={k: 'synthetic' for k in ('go', 'bazel', 'bazelisk', 'foundationdb', 'api', 'GOMAXPROCS',
                     'GOGC', 'GOMEMLIMIT', 'GODEBUG', 'TZ', 'LC_ALL')})
        inputs.values['deploy/protocol.json'] = p
        inputs.values['provenance/source-receipt.json'] = dict(production_revisions=p['production_revisions'],
            harness_commit='3' * 40, compiler_sha256='4' * 64,
            harness_compatibility=dict(upstream_commit='5' * 40, archive_sha256='6' * 64, patch_sha256='7' * 64))
        inputs.values['cluster-ssd/layout.json'] = test_server_cpu.fixture()[0]
        for phase in ('M2rl', 'M2skip'):
            current = inputs.values[f'analysis/current/{phase}/report.json']
            current['comparisons'] = []
            for case in current['cases'].values():
                case['absolutes'] = {arm: {m: dict(median_of_process_repeat_medians=10)
                    for m in ('ns/op', 'B/op', 'allocs/op', 'rows/op', 'prefix-B')} for arm in export.m2_export.ARMS}
        phases = {phase: inputs.load(f'analysis/current/{phase}/' +
                  ('summary.json' if phase.startswith('M1') else 'report.json')) for phase in expected}
        inputs.values['analysis/current/summary.json'] = dict(status='complete', eligible_cases=44, phases=phases,
            provenance=dict(protocol_sha256=inputs.digest('deploy/protocol.json'),
                source_receipt_sha256=inputs.digest('provenance/source-receipt.json'),
                runtime_receipts={role: dict(identities={path: '8' * 64 for path in
                    ('/usr/sbin/fdbserver', '/usr/bin/fdbcli', '/usr/lib/libfdb_c.so')}) for role in ('client', 'node1', 'node2', 'node3')}))
        inputs.verify_all = Mock()
        with patch.object(export, 'collect_m1', return_value=([], [])), \
             patch.object(export.m2_export, 'collect', return_value=([], {})), \
             patch.object(export, 'cpu_rows', return_value=[]):
            result = export.export(inputs, 'analysis/current', None, None, None)
        self.assertEqual(result['coverage']['clean'], 44)
        self.assertTrue(result['performance_results_available'])
        self.assertEqual(len(result['M2_absolutes']), 32)
        self.assertEqual(result['M2_absolutes'][0]['metrics']['ns/op'], 10)
        self.assertEqual(result['method']['compiler_sha256'], '4' * 64)
        self.assertEqual(set(result['method']['runtime_binary_sha256']), {'client', 'node1', 'node2', 'node3'})
        self.assertEqual(inputs.verify_all.call_count, 2)
        # Individually pinned, stale global summary must not bind other phase bodies.
        inputs.values['analysis/current/summary.json']['phases']['M1w32'] = {}
        with self.assertRaisesRegex(ValueError, 'provenance differs'):
            export.export(inputs, 'analysis/current', None, None, None)

    def test_cpu_unqualification_does_not_publish_private_exception_paths(self):
        reason = export.cpu_failure(FileNotFoundError('/home/private/campaign/secret-resource-id'))
        self.assertNotIn('/home', reason)
        self.assertNotIn('secret-resource-id', reason)
        self.assertEqual(export.cpu_failure(export.server_cpu.CPUUnqualified('CPU resolution unqualified')),
                         'CPU resolution unqualified')

    def test_fence_inversion_mismatch_rounding_and_warmup_refused(self):
        raw = dict(measured_fences_format='fdb-bench-measured-fences/v1', measured_started_unix_ns=1_000_000_000,
                   measured_ended_unix_ns=1_200_000_000, measured_started_utc='1970-01-01T00:00:01Z',
                   measured_ended_utc='1970-01-01T00:00:01.2Z', measured_wall_ns=200_000_000,
                   warmup_wall_ns=999_000_000)
        self.assertEqual(export.measured_window(raw), (1_000_000_000, 1_200_000_000))
        for field, value in [('measured_fences_format', ''), ('measured_ended_unix_ns', 900_000_000),
                             ('measured_ended_unix_ns', 1_200_000_000.0), ('measured_wall_ns', 999_000_000),
                             ('measured_ended_utc', '1970-01-01T00:00:01.200000001Z')]:
            changed = dict(raw, **{field: value})
            with self.assertRaises(ValueError): export.measured_window(changed)


if __name__ == '__main__':
    unittest.main()
