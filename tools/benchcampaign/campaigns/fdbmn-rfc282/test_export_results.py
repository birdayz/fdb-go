"""Synthetic population controls; no private archive, estimator, SSH or cloud."""
import copy
import csv
import io
import importlib.util
import itertools
import hashlib
import json
from pathlib import Path
import unittest
from unittest.mock import Mock, patch
from decimal import Decimal
from types import SimpleNamespace
import test_server_cpu

import export_results as export


WORKLOADS = ('grv', 'get100', 'get1000', 'range100', 'set100', 'batchset50', 'batchset50x1000')
QUOTAS = {1: (3000, 200), 8: (12000, 800), 32: (24000, 1600), 64: (32000, 1600)}


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
    for workers, (iterations, warmup) in QUOTAS.items():
        phases.append(dict(id=f'M1w{workers}', kind='client', workers=workers, iterations=iterations,
            warmup=warmup, blocks=6, arms=['before-Go', 'final-Go', 'CGo-7.3.77'], workloads=list(WORKLOADS)))
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


def seal_export_fixture(inputs, expected, clean=44):
    p = protocol()
    p.update(production_revisions=dict(before_allocation_work_after_read_parity='1' * 40, final='2' * 40),
             measurement_extensions=dict(throughput_preregistered=True, server_cpu_source='exclusive-cgroup-v2-usage-usec',
                 server_cpu_metric='observed-cgroup-accounting-delta', server_cpu_actual_window_available=False,
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
        current.setdefault('comparisons', [])
        for case in current['cases'].values():
            if case['status'] == 'ok':
                case.setdefault('absolutes', {arm: {m: dict(median_of_process_repeat_medians=10)
                    for m in ('ns/op', 'B/op', 'allocs/op', 'rows/op', 'prefix-B')} for arm in export.m2_export.ARMS})
    phases = {phase: inputs.load(f'analysis/current/{phase}/' +
              ('summary.json' if phase.startswith('M1') else 'report.json')) for phase in expected}
    inputs.values['analysis/current/summary.json'] = dict(
        status='complete' if clean else 'complete-no-eligible-cases', eligible_cases=clean, phases=phases,
        provenance=dict(protocol_sha256=inputs.digest('deploy/protocol.json'),
            source_receipt_sha256=inputs.digest('provenance/source-receipt.json'),
            runtime_receipts={role: dict(identities={path: '8' * 64 for path in
                ('/usr/sbin/fdbserver', '/usr/bin/fdbcli', '/usr/lib/libfdb_c.so')}) for role in ('client', 'node1', 'node2', 'node3')}))
    inputs.verify_all = Mock()


def assert_accounting_only(test, row):
    test.assertIs(row['actual_window_cpu_available'], False)
    for record in [row, *row.get('nodes', {}).values()]:
        unsupported = {key for key in record if any(token in key for token in
            ('lower', 'upper', 'bound', 'per_op', 'cores', 'rate', 'ratio', 'ci95', 'interval_95'))}
        unsupported.update(set(record) & {'cpu_util_cores', 'throughput_ops_per_second', 'gm',
                                         'nominal_95_interval', 'bracket_cpu_seconds', 'bracket_wall_seconds',
                                         'bracket_user_cpu_seconds', 'bracket_system_cpu_seconds', 'outer_intervals'})
        test.assertEqual(unsupported, set(), 'unsupported server CPU estimates')


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

    def test_m1_retained_metrics_and_paired_values_are_not_recomputed_or_zeroed(self):
        inputs, specs, expected = population_fixture()
        metrics = ('mean_ns', 'p50_ns', 'p99_ns', 'shutdown_process_cpu_ns_per_op',
                   'go_alloc_bytes_per_op', 'go_mallocs_per_op')
        factors = {'before': 1, 'final': 2, 'cgo': 4}
        retained_comparisons = [dict(metric=metric, numerator=n, denominator=d,
            ratio=10 + index + contrast / 10, ci95_low=9 + index + contrast / 10,
            ci95_high=11 + index + contrast / 10, blocks=6)
            for contrast, (n, d) in enumerate((('final', 'before'), ('final', 'cgo'), ('before', 'cgo')))
            for index, metric in enumerate(metrics)]
        for phase in ('M1w1', 'M1w8', 'M1w32', 'M1w64'):
            for retained in inputs.values[f'analysis/current/{phase}/summary.json']['workloads'].values():
                for row in retained['per_run']:
                    row.update({metric: 1000 * (index + 1) + 100 * factors[row['arm']] + row['block']
                                for index, metric in enumerate(metrics)})
                retained['comparisons'] = copy.deepcopy(retained_comparisons)
        inputs.repin_analysis()
        _, reports, analyses = export.population(inputs, 'analysis/current', expected)
        def measured(raw, retained, samples):
            return dict(arm=retained['arm'], block=retained['block'], position=retained['position'],
                        run_id=retained['run_id'], throughput_ops_per_second=100 * (retained['block'] + 1) * factors[retained['arm']],
                        cpu_util_cores=factors[retained['arm']] / 2)
        calls = []
        def bootstrap(pairs, rng, resamples):
            expected_pairs = (
                [(200, 100), (400, 200), (600, 300), (800, 400), (1000, 500), (1200, 600)],
                [(200, 400), (400, 800), (600, 1200), (800, 1600), (1000, 2000), (1200, 2400)],
                [(100, 400), (200, 800), (300, 1200), (400, 1600), (500, 2000), (600, 2400)])
            contrast = len(calls) % 3
            self.assertEqual(pairs, expected_pairs[contrast])
            self.assertEqual(resamples, 20000)
            calls.append(pairs)
            ratio = (2, 0.5, 0.25)[contrast]
            return dict(blocks=6, ratio=ratio, ci95_low=ratio / 2, ci95_high=ratio * 2)
        with patch.object(export.legacy_m1, 'measured_process', side_effect=measured):
            rows, comparisons = export.collect_m1(inputs, specs, expected, analyses, reports, bootstrap)
        self.assertEqual(len(calls), 84)
        self.assertEqual(len(rows), 504)
        for row in rows:
            self.assertEqual([row[m] for m in metrics],
                             [1000 * (index + 1) + 100 * factors[row['arm']] + row['block'] for index in range(6)])
            self.assertEqual(row['throughput_ops_per_second'], 100 * (row['block'] + 1) * factors[row['arm']])
            self.assertEqual(row['cpu_util_cores'], factors[row['arm']] / 2)
        selected = [r for r in comparisons if r['phase'] == 'M1w32' and r['case'] == 'get1000']
        self.assertEqual([{k: r[k] for k in ('metric', 'numerator', 'denominator', 'ratio', 'ci95_low', 'ci95_high', 'blocks')}
                          for r in selected if r['metric'] != 'throughput_ops_per_second'], retained_comparisons)
        self.assertEqual([(r['numerator'], r['denominator'], r['ratio'], r['ci95_low'], r['ci95_high'])
                          for r in selected if r['metric'] == 'throughput_ops_per_second'],
                         [('final', 'before', 2, 1, 4), ('final', 'cgo', 0.5, 0.25, 1), ('before', 'cgo', 0.25, 0.125, 0.5)])

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
        for phase in ('M2rl', 'M2skip'):
            arms = ['final-ordinary', 'final-remote']
            orders = [arms[:], arms[::-1]] * 3
            jobs = [dict(id=f'c{index}-b{block}-p{position}', case=case, block=block, position=position, arm=arm)
                    for index, case in enumerate(expected[phase]) for block, order in enumerate(orders)
                    for position, arm in enumerate(order)]
            inputs.values[f'results-raw/{phase}/plan.json'] = dict(phase=phase, orders=orders, jobs=jobs,
                cases=[dict(key=case, placement_label='cross-server', placement_fraction=1.0) for case in expected[phase]])
            current = inputs.values[f'analysis/current/{phase}/report.json']
            current.update(phase=phase, orders=orders, comparisons=[])
            stream = io.StringIO(newline='')
            csv.DictWriter(stream, fieldnames=['phase', 'case', 'block', 'position', 'arm', 'repeat', 'N',
                'ns/op', 'B/op', 'allocs/op', 'rows/op', 'prefix-B']).writeheader()
            inputs.values[f'results-raw/{phase}/metrics.csv'] = stream.getvalue()
        seal_export_fixture(inputs, expected, clean=0)
        estimator = Mock(side_effect=AssertionError('zero-clean campaign must not estimate'))
        result = export.export(inputs, 'analysis/current', estimator, estimator, None)
        estimator.assert_not_called()
        self.assertFalse(result['performance_results_available'])
        self.assertEqual(result['format'], 'fdbmn-rfc282-public-results/v2')
        self.assertNotIn('server_process_cpu', result)
        self.assertEqual((result['coverage']['planned'], result['coverage']['clean'], result['coverage']['unverifiable']),
                         (44, 0, 44))
        excluded = [(phase, case, detail) for phase, cases in result['coverage']['phases'].items()
                    for case, detail in cases.items()]
        self.assertEqual(len(excluded), 44)
        public_reason = 'required evidence missing [sha256:' + hashlib.sha256(b'missing').hexdigest() + ']'
        self.assertTrue(all(detail == dict(verdict='unverifiable', reasons=[public_reason]) for _, _, detail in excluded))
        self.assertEqual({(phase, case) for phase, case, _ in excluded},
                         {(phase, case) for phase, cases in expected.items() for case in cases})
        self.assertEqual(len(result['M2']), 16)
        self.assertEqual({row['record_type'] for row in result['M2']}, {'omitted'})
        for key in ('M1', 'M1_comparisons', 'M2_absolutes', 'M2_comparisons', 'server_cgroup_accounting'):
            self.assertEqual(result[key], [], key)
        self.assertEqual(inputs.verify_all.call_count, 2)

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

    def test_frozen_geometry_has_literal_workloads_and_all_four_quotas(self):
        specs, expected = export.protocol_cases(protocol())
        self.assertEqual({phase: (spec['iterations'], spec['warmup']) for phase, spec in specs.items()
                          if phase.startswith('M1')},
                         {'M1w1': (3000, 200), 'M1w8': (12000, 800),
                          'M1w32': (24000, 1600), 'M1w64': (32000, 1600)})
        for phase in ('M1w1', 'M1w8', 'M1w32', 'M1w64'):
            self.assertEqual(expected[phase],
                             ['grv', 'get100', 'get1000', 'range100', 'set100', 'batchset50', 'batchset50x1000'])
        self.assertEqual(expected['M2rl'], [
            'small-limit1-forward', 'small-limit1-reverse', 'small-rows10-forward', 'small-rows10-reverse',
            'small-rows1000-forward', 'small-rows1000-reverse', 'split-limit1-forward', 'split-limit1-reverse',
            'split-rows10-forward', 'split-rows10-reverse', 'split-rows1000-forward', 'split-rows1000-reverse'])
        self.assertEqual(expected['M2skip'],
                         ['skip3-rows10-forward', 'skip3-rows10-reverse', 'skip3-rows1000-forward', 'skip3-rows1000-reverse'])

    def test_matrix_changes_are_not_generic_population_relaxations(self):
        for index in range(4):
            for field in ('iterations', 'warmup'):
                with self.subTest(phase=index, field=field):
                    p = protocol()
                    p['phases'][index][field] -= 1
                    with self.assertRaisesRegex(ValueError, 'changed frozen M1 geometry'):
                        export.protocol_cases(p)
        for mutation in ('w32', 'workload', 'replacement', 'phase'):
            with self.subTest(mutation=mutation):
                p = protocol()
                if mutation == 'w32': p['phases'][2]['workers'] = 31
                if mutation == 'workload': p['phases'][0]['workloads'].pop()
                if mutation == 'replacement': p['phases'][0]['workloads'][1] = 'get200'
                if mutation == 'phase': p['phases'].pop()
                with self.assertRaises(ValueError): export.protocol_cases(p)

    def m1_cpu_fixture(self):
        layout, units, telemetry, _, lifecycle = test_server_cpu.fixture()
        report = dict(cases=[dict(case='grv', verdict='clean', runs=[])],
                      nodes={n: dict(units=sorted(units[n])) for n in units},
                      clock=dict(client=dict(bound_s='0')), inputs_sha256={})
        values = {}
        for node, (start, stop, samples) in telemetry.items():
            start['chrony'] = stop['chrony'] = dict(rc=0, raw='synthetic synced chrony')
            root = f'telemetry-cpu/M1w1/{node}'
            values[root + '/samples.jsonl'] = ''.join(json.dumps(s) + '\n' for s in samples)
            stop['samples_sha256'] = hashlib.sha256(values[root + '/samples.jsonl'].encode()).hexdigest()
            for kind, receipt in [('start', start), ('stop', stop)]:
                values[root + f'/receipt-{kind}.json'] = receipt
                values[f'telemetry/M1w1/{node}/receipt-{kind}.json'] = copy.deepcopy(receipt)
        inputs = Inputs(values)
        for path in list(values):
            if path.startswith('telemetry/'):
                report['inputs_sha256']['/srv/fdb-bench/' + path] = inputs.digest(path)
        rows = []
        for block in range(6):
            for arm, ops in [('before', 100), ('final', 200), ('cgo', 400)]:
                run_id = f'b{block}-{arm}'
                report['cases'][0]['runs'].append(dict(copy.deepcopy(lifecycle), run_id=run_id))
                rows.append(dict(phase='M1w1', arm=arm, block=block, run_id=run_id, measured_ops=ops))
                root = f'results-raw/M1w1/grv/{run_id}'
                values[root + '/result.json'] = dict(run_id=run_id,
                    started_utc=lifecycle['start_utc'], ended_utc=lifecycle['end_utc'])
                values[root + '/summary.json'] = dict(measured_fences_format='fdb-bench-measured-fences/v1',
                    measured_started_unix_ns=1_200_000_000, measured_ended_unix_ns=1_400_000_000,
                    measured_started_utc='1970-01-01T00:00:01.2Z', measured_ended_utc='1970-01-01T00:00:01.4Z',
                    measured_wall_ns=200_000_000, warmup_wall_ns=100_000_000)
        parser = SimpleNamespace(parse_chrony_tracking=lambda text: ({}, []), chrony_error_bound=lambda fields: Decimal('0'))
        return inputs, report, layout, rows, parser

    def m1_clock_fixture(self, start_dispersion, stop_dispersion, r4_bound, widened_start_ns, widened_end_ns,
                         client_bound='0.004', client_dispersion='0.002'):
        inputs, report, layout, processes, _ = self.m1_cpu_fixture()
        def chrony(offset, delay, dispersion):
            return dict(rc=0, raw=(f'System time: {offset} seconds slow of NTP time\n'
                f'Root delay: {delay} seconds\nRoot dispersion: {dispersion} seconds\nLeap status: Normal\n'))
        report['clock']['client'] = dict(bound_s=client_bound, receipts={}, reasons=[])
        for kind, stamp in [('before', 1_000_000_000), ('after', 2_000_000_000)]:
            path = f'telemetry/M1w1/client/receipt-{kind}.json'
            inputs.values[path] = dict(format='fdb-bench-node-receipt/v1', host='client', phase='M1w1', kind=kind,
                realtime_ns=stamp, chrony=chrony('0.001', '0.002', client_dispersion))
            report['clock']['client']['receipts'][path] = client_bound
            report['inputs_sha256']['/srv/fdb-bench/' + path] = inputs.digest(path)
        # R4's base audit receipts are distinct from the CPU sidecar receipts.
        base_bound, base_clock = (('0.020', chrony('0.001', '0.002', '0.018')) if r4_bound == '1.025' else
                                 ('0.001', chrony('0.0002', '0.0004', '0.0006')))
        report['clock']['nodes'] = {node: dict(bound_s=base_bound, u_s=r4_bound, reasons=[])
                                    for node in ('node1', 'node2', 'node3')}
        for run in report['cases'][0]['runs']:
            for evidence in run['nodes'].values():
                evidence.update(u_s=r4_bound, widened_start_ns=widened_start_ns, widened_end_ns=widened_end_ns)
            inputs.values[f'results-raw/M1w1/grv/{run["run_id"]}/summary.json'].update(
                measured_ended_unix_ns=1_600_000_000, measured_ended_utc='1970-01-01T00:00:01.6Z',
                measured_wall_ns=400_000_000)
        for node in ('node1', 'node2', 'node3'):
            for kind, dispersion in [('start', start_dispersion), ('stop', stop_dispersion)]:
                inputs.values[f'telemetry-cpu/M1w1/{node}/receipt-{kind}.json']['chrony'] = chrony('0.001', '0.002', dispersion)
                base = f'telemetry/M1w1/{node}/receipt-{kind}.json'
                inputs.values[base].update(format='fdb-bench-node-receipt/v1', chrony=copy.deepcopy(base_clock))
                report['inputs_sha256']['/srv/fdb-bench/' + base] = inputs.digest(base)
        multinode = Path(export.__file__).resolve().parents[2] / 'multinode'
        common_spec = importlib.util.spec_from_file_location('rfc282_clock_common', multinode / 'mn_common.py')
        common = importlib.util.module_from_spec(common_spec)
        common_spec.loader.exec_module(common)
        sampler_spec = importlib.util.spec_from_file_location('rfc282_clock_sampler', multinode / 'node_sampler.py')
        parser = importlib.util.module_from_spec(sampler_spec)
        # Use the production parser without changing other tests' import bindings.
        with patch.dict('sys.modules', {'mn_common': common}):
            sampler_spec.loader.exec_module(parser)
        for path, receipt in inputs.values.items():
            if path.startswith('telemetry/'):
                fields, errors = parser.parse_chrony_tracking(receipt['chrony']['raw'])
                self.assertEqual(errors, [])
                self.assertEqual(parser.chrony_error_bound(fields), Decimal(client_bound if '/client/' in path else base_bound))
                self.assertEqual(report['inputs_sha256']['/srv/fdb-bench/' + path], inputs.digest(path))
        self.assertEqual(Decimal(r4_bound), Decimal('1.001') + Decimal(client_bound) + Decimal(base_bound))
        return inputs, report, layout, processes, parser

    def test_cpu_rows_composes_receipt_and_r4_clocks_before_selecting_brackets(self):
        cases = [
            ('start-dominates', '0.006', '0.001', '1.006', 94_000_000, 2_906_000_000,
             Decimal('0.008'), Decimal('0.003'), 14_000_001, [18, 62], 1_180_000_000, 1_620_000_000, 36, 7_480_000),
            ('stop-dominates', '0.001', '0.006', '1.006', 94_000_000, 2_906_000_000,
             Decimal('0.003'), Decimal('0.008'), 14_000_001, [18, 62], 1_180_000_000, 1_620_000_000, 36, 7_480_000),
            ('r4-dominates', '0.006', '0.001', '1.025', 75_000_000, 2_925_000_000,
             Decimal('0.008'), Decimal('0.003'), 26_000_001, [17, 63], 1_170_000_000, 1_630_000_000, 34, 7_820_000),
        ]
        for label, start_disp, stop_disp, r4, widened_start, widened_end, start_bound, stop_bound, allowance, sequence, start, end, inner, usage in cases:
            with self.subTest(clock=label):
                inputs, report, layout, processes, parser = self.m1_clock_fixture(
                    start_disp, stop_disp, r4, widened_start, widened_end)
                for kind, expected in [('start', start_bound), ('stop', stop_bound)]:
                    fields, errors = parser.parse_chrony_tracking(
                        inputs.values[f'telemetry-cpu/M1w1/node1/receipt-{kind}.json']['chrony']['raw'])
                    self.assertEqual(errors, [])
                    self.assertEqual(parser.chrony_error_bound(fields), expected)
                rows = export.cpu_rows(inputs, 'M1w1', report, layout, processes, parser)
                self.assertEqual(len(rows), 18)
                for row in rows:
                    self.assertEqual(row['status'], 'observed')
                    self.assertEqual(row['observed_accounted_cpu_usec'], usage)
                    self.assertEqual((row['requested_window_started_unix_ns'], row['requested_window_ended_unix_ns']),
                                     (1_200_000_000, 1_600_000_000))
                    assert_accounting_only(self, row)
                    for data in row['nodes'].values():
                        self.assertEqual(data['temporal_selection_allowance_ns'], allowance)
                        self.assertEqual(data['sample_sequence'], sequence)
                        self.assertEqual((data['sample_bracket_started_unix_ns'], data['sample_bracket_ended_unix_ns']),
                                         (start, end))
                        self.assertEqual(data['sample_bracket_wall_ns'], end - start)
                        self.assertEqual(data['inner_intervals'], inner)

    def test_composed_clock_allowance_can_make_whole_case_accounting_unavailable(self):
        for start_disp, stop_disp in [('0.023', '0.001'), ('0.001', '0.023')]:
            with self.subTest(start_dispersion=start_disp, stop_dispersion=stop_disp):
                inputs, report, layout, processes, parser = self.m1_clock_fixture(
                    start_disp, stop_disp, '1.006', 94_000_000, 2_906_000_000)
                positive = export.cpu_rows(inputs, 'M1w1', report, layout, processes, parser)
                self.assertEqual(len(positive), 18)
                for row in positive:
                    self.assertEqual(row['status'], 'observed')
                    for data in row['nodes'].values():
                        self.assertEqual(data['temporal_selection_allowance_ns'], 31_000_001)
                        self.assertEqual(data['sample_sequence'], [16, 64])
                        self.assertEqual((data['sample_bracket_started_unix_ns'], data['sample_bracket_ended_unix_ns'],
                                          data['sample_bracket_wall_ns']), (1_160_000_000, 1_640_000_000, 480_000_000))
                inputs, report, layout, processes, parser = self.m1_clock_fixture(
                    start_disp, stop_disp, '1.016', 84_000_000, 2_916_000_000,
                    client_bound='0.014', client_dispersion='0.012')
                result = export.cpu_rows(inputs, 'M1w1', report, layout, processes, parser)
                self.assertEqual(len(result), 1)
                self.assertEqual((result[0]['phase'], result[0]['case'], result[0]['status']),
                                 ('M1w1', 'grv', 'CPU-unavailable'))
                self.assertIn('accounting temporal resolution unqualified', result[0]['reason'])
                assert_accounting_only(self, result[0])
                for field in ('arm', 'nodes', 'observed_accounted_cpu_usec', 'observed_accounted_cpu_seconds'):
                    self.assertNotIn(field, result[0])

    def test_m1_accounting_rows_join_measured_fences_without_operation_normalization(self):
        inputs, report, layout, processes, parser = self.m1_cpu_fixture()
        rows = export.cpu_rows(inputs, 'M1w1', report, layout, processes, parser)
        self.assertEqual(len(rows), 18)
        self.assertEqual({(r['block'], r['arm']) for r in rows},
                         {(block, arm) for block in range(6) for arm in ('before', 'final', 'cgo')})
        for row in rows:
            self.assertEqual(row['status'], 'observed')
            self.assertIs(type(row['observed_accounted_cpu_usec']), int)
            self.assertEqual(row['observed_accounted_cpu_usec'], 3_740_000)
            self.assertAlmostEqual(row['observed_accounted_cpu_seconds'], 3.74)
            self.assertEqual(row['window_kind'], 'measured-client-UTC-fences')
            self.assertEqual((row['requested_window_started_unix_ns'], row['requested_window_ended_unix_ns']),
                             (1_200_000_000, 1_400_000_000))
            assert_accounting_only(self, row)
            for node, count, usage in [('node1', 7, 1_540_000), ('node2', 5, 1_100_000), ('node3', 5, 1_100_000)]:
                data = row['nodes'][node]
                self.assertIs(type(data['observed_accounted_cpu_usec']), int)
                self.assertEqual(data['observed_accounted_cpu_usec'], usage)
                self.assertAlmostEqual(data['observed_accounted_cpu_seconds'], usage / 1e6)
                self.assertEqual((data['sample_bracket_started_unix_ns'], data['sample_bracket_ended_unix_ns'],
                                  data['sample_bracket_wall_ns']), (1_190_000_000, 1_410_000_000, 220_000_000))
                self.assertEqual(data['sample_sequence'], [19, 41])
                self.assertEqual((data['inner_intervals'], data['observed_intervals']), (18, 22))
                self.assertEqual(data['temporal_selection_allowance_ns'], 2_000_001)
                self.assertEqual(data['phase_correlation_range_ns'], 0)
                self.assertEqual(data['processes'], count)
                self.assertAlmostEqual(data['observed_accounted_user_cpu_seconds'], usage * .8 / 1e6)
                self.assertAlmostEqual(data['observed_accounted_system_cpu_seconds'], usage * .2 / 1e6)
        for index, process in enumerate(processes):
            if index % 3 == 0:
                process.pop('measured_ops')
            else:
                process['measured_ops'] = 0 if index % 3 == 1 else None
        self.assertEqual(export.cpu_rows(inputs, 'M1w1', report, layout, processes, parser), rows)

    def test_m1_bad_last_arm_fence_or_temporal_resolution_omits_entire_case(self):
        for mutation, reason in [('missing', 'missing reviewed measured fences'),
                                 ('mismatch', 'UTC/Unix measured fences differ'),
                                 ('short', 'accounting temporal resolution unqualified')]:
            with self.subTest(mutation=mutation):
                inputs, report, layout, processes, parser = self.m1_cpu_fixture()
                positive = export.cpu_rows(inputs, 'M1w1', report, layout, processes, parser)
                self.assertEqual(len(positive), 18)
                self.assertTrue(all(r['status'] == 'observed' for r in positive))
                raw = inputs.values['results-raw/M1w1/grv/b5-cgo/summary.json']
                if mutation == 'missing':
                    raw.pop('measured_fences_format')
                elif mutation == 'mismatch':
                    raw['measured_ended_utc'] = '1970-01-01T00:00:01.400000001Z'
                else:
                    raw.update(measured_ended_unix_ns=1_250_000_000,
                               measured_ended_utc='1970-01-01T00:00:01.25Z', measured_wall_ns=50_000_000)
                result = export.cpu_rows(inputs, 'M1w1', report, layout, processes, parser)
                self.assertEqual(len(result), 1)
                self.assertEqual((result[0]['phase'], result[0]['case'], result[0]['status']),
                                 ('M1w1', 'grv', 'CPU-unavailable'))
                self.assertIn(reason, result[0]['reason'])
                assert_accounting_only(self, result[0])
                for field in ('arm', 'observed_accounted_cpu_usec', 'observed_accounted_cpu_seconds', 'nodes'):
                    self.assertNotIn(field, result[0])

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
        # Positive control: both rows had observed accounting before the raw mutation.
        positive = export.cpu_rows(inputs, 'M2rl', report, layout, rows, parser)
        self.assertEqual(len(positive), 2)
        for row in positive:
            self.assertEqual(row['status'], 'observed')
            self.assertEqual(row['window_kind'], 'process-lifecycle-UTC')
            self.assertEqual((row['requested_window_started_unix_ns'], row['requested_window_ended_unix_ns']),
                             (1_100_000_000, 1_900_000_000))
            self.assertEqual(row['observed_accounted_cpu_usec'], 13_940_000)
            self.assertAlmostEqual(row['observed_accounted_cpu_seconds'], 13.94)
            self.assertEqual(row['nodes']['node1']['temporal_selection_allowance_ns'], 3_000_000)
            assert_accounting_only(self, row)
        values['results-raw/M2rl/final-remote/result.json']['ended_utc'] = '1970-01-01T00:00:01.8Z'
        result = export.cpu_rows(inputs, 'M2rl', report, layout, rows, parser)
        self.assertEqual(len(result), 1)
        self.assertEqual(result[0]['status'], 'CPU-unavailable')
        self.assertIn('R4/raw lifecycle window differs', result[0]['reason'])
        self.assertNotIn('observed_accounted_cpu_seconds', result[0])
        assert_accounting_only(self, result[0])

    def test_export_wiring_retains_full_population_absolutes_and_source_identities(self):
        inputs, _, expected = population_fixture()
        seal_export_fixture(inputs, expected)
        with patch.object(export, 'collect_m1', return_value=([], [])), \
             patch.object(export.m2_export, 'collect', return_value=([], {})), \
             patch.object(export, 'cpu_rows', return_value=[]):
            result = export.export(inputs, 'analysis/current', None, None, None)
        self.assertEqual(result['coverage']['clean'], 44)
        self.assertTrue(result['performance_results_available'])
        self.assertEqual(result['format'], 'fdbmn-rfc282-public-results/v2')
        self.assertEqual(result['server_cgroup_accounting'], [])
        self.assertNotIn('server_process_cpu', result)
        self.assertEqual(result['method']['server_cpu']['metric'], 'observed-cgroup-accounting-delta')
        self.assertIs(result['method']['server_cpu']['actual_window_cpu_available'], False)
        example = json.loads((Path(__file__).resolve().parent / 'accounting-example.json').read_text())
        self.assertEqual(result['method']['server_cpu'], example['method']['server_cpu'])
        self.assertEqual(len(result['M2_absolutes']), 32)
        self.assertEqual(result['M2_absolutes'][0]['metrics']['ns/op'], 10)
        self.assertEqual(result['method']['compiler_sha256'], '4' * 64)
        self.assertEqual(set(result['method']['runtime_binary_sha256']), {'client', 'node1', 'node2', 'node3'})
        self.assertEqual(inputs.verify_all.call_count, 2)
        # Individually pinned, stale global summary must not bind other phase bodies.
        inputs.values['analysis/current/summary.json']['phases']['M1w32'] = {}
        with self.assertRaisesRegex(ValueError, 'provenance differs'):
            export.export(inputs, 'analysis/current', None, None, None)

    def test_export_refuses_legacy_or_conflicting_accounting_policy_before_collectors(self):
        for field, value in [('server_cpu_metric', None), ('server_cpu_metric', 'actual-execution-window-cpu'),
                             ('server_cpu_actual_window_available', None), ('server_cpu_actual_window_available', True),
                             ('server_cpu_actual_window_available', 'false'), ('server_cpu_actual_window_available', 0),
                             ('server_cpu_source', 'host-proc-stat'), ('process_cpu_interval_ns', 20_000_000),
                             ('process_cpu_min_inner_intervals', 1), ('process_cpu_max_time_envelope_ratio', 2),
                             ('process_cpu_clock_reserve_ns', 0), ('process_cpu_phase_correlation_range_ns', 2_000_000),
                             ('throughput_preregistered', False)]:
            with self.subTest(field=field, value=value):
                inputs, _, expected = population_fixture()
                seal_export_fixture(inputs, expected)
                policy = inputs.values['deploy/protocol.json']['measurement_extensions']
                if value is None:
                    policy.pop(field)
                else:
                    policy[field] = value
                inputs.values['analysis/current/summary.json']['provenance']['protocol_sha256'] = inputs.digest('deploy/protocol.json')
                with patch.object(export, 'collect_m1') as collector:
                    with self.assertRaisesRegex(ValueError, 'unsealed measurement extension policy'):
                        export.export(inputs, 'analysis/current', None, None, None)
                collector.assert_not_called()

    def test_cpu_unavailability_does_not_publish_private_exception_paths(self):
        reason = export.cpu_failure(FileNotFoundError('/home/private/campaign/secret-resource-id'))
        self.assertNotIn('/home', reason)
        self.assertNotIn('secret-resource-id', reason)
        self.assertEqual(export.cpu_failure(export.server_cpu.CPUUnqualified('accounting temporal resolution unqualified')),
                         'accounting temporal resolution unqualified')

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
