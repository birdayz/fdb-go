#!/usr/bin/env python3
"""Tiny offline controls: synthetic files and fake analyzers, never campaign data."""
import copy
import importlib.util
import json
from pathlib import Path
from types import SimpleNamespace
import tempfile
import unittest

spec = importlib.util.spec_from_file_location('campaign_analysis_adapter', Path(__file__).with_name('analyze_campaign.py'))
a = importlib.util.module_from_spec(spec)
spec.loader.exec_module(a)


def write(path, value):
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(value))


def reconciliation(phase, verdict='clean'):
    return dict(phase=phase, reconciliation_complete=True,
        cases=[dict(case='grv', verdict=verdict, runs=[], contaminated_reasons=['overload'] if verdict == 'contaminated' else [], unverifiable_reasons=[])],
        phase_summary=dict(cases=1, clean=int(verdict == 'clean'), contaminated=int(verdict == 'contaminated'),
                           unverifiable=int(verdict == 'unverifiable'), analysis_eligible_cases=['grv'] if verdict == 'clean' else []))


def client_rows():
    return [dict(block=b, arm=arm, workload='grv', errors=0, go_alloc_bytes_per_op=100 + b, go_mallocs_per_op=10 + b)
            for b in range(6) for arm in ('before', 'final', 'cgo')]


class FakeClientMath:
    def __init__(self):
        self.calls = []

    def bootstrap_log_ratio(self, pairs, rng, resamples):
        assert len(pairs) == 6 and resamples == 20000
        self.calls.append(('bootstrap', pairs))
        return dict(ratio=1, ci95_low=1, ci95_high=1, blocks=6)

    def dependence_sensitivity(self, pairs, rng, resamples):
        assert len(pairs) == 6 and resamples == 20000
        self.calls.append(('sensitivity', pairs))
        return dict(moving_block_length=3)


class Controls(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(prefix='analysis-adapter-mock-', dir=Path(__file__).parent)
        self.root = Path(self.tmp.name)
        self.adapter = a.Adapter.__new__(a.Adapter)
        self.adapter.c = self.root
        self.adapter.out = self.root / 'analysis/mock'
        self.adapter.out.mkdir(parents=True)
        self.adapter.protocol = dict(production_revisions=dict(before_allocation_work_after_read_parity='old', final='new'),
                                     frozen_harness_revision='d94', phases=[])
        self.adapter.protocol_path = self.root / 'protocol.json'
        write(self.adapter.protocol_path, self.adapter.protocol)
        self.adapter.ca = FakeClientMath()
        self.adapter.scripts = self.root / 'fake-scripts'
        self.adapter.binary_hashes = {'old/client-go': 'b' * 64, 'master/client-go': 'f' * 64, 'master/client-cgo': 'c' * 64}
        self.adapter.provenance = {'runtime_receipts': {'client': {'identities': {'/usr/lib/libfdb_c.so': 'e' * 64}}}}
        self.adapter.historical = lambda *args: {'status': 'descriptive-only-fake'}

    def tearDown(self):
        self.tmp.cleanup()

    def test_r4_requires_complete_nonempty_exact_case_population(self):
        valid = reconciliation('M1w1')
        self.assertEqual(a.r4_cases(valid, 'M1w1', ['grv'])['grv']['verdict'], 'clean')
        bads = [dict(valid, reconciliation_complete=False), dict(valid, cases=[]), dict(valid, cases=valid['cases'] * 2)]
        for bad in bads:
            with self.assertRaises(a.Refused):
                a.r4_cases(bad, 'M1w1', ['grv'])
        with self.assertRaises(a.Refused):
            a.r4_cases(valid, 'M1w1', [])

    def test_missing_block_and_duplicate_r4_run_refuse(self):
        with self.assertRaises(a.Refused):
            a.paired_rows(client_rows()[:-1], 'grv')
        with self.assertRaises(a.Refused):
            a.exact_runs({'case': 'grv', 'runs': [{'run_id': 'one'}, {'run_id': 'one'}]}, ['one', 'two'])

    def test_allocations_use_existing_functions_on_six_pairs(self):
        ca = FakeClientMath()
        comparisons = a.allocation_comparisons(ca, client_rows(), 'grv')
        self.assertEqual(len(comparisons), 6)
        self.assertEqual(len(ca.calls), 12)
        self.assertEqual({(c['numerator'], c['denominator']) for c in comparisons}, set(a.CONTRASTS))
        rows = client_rows()
        rows[0]['go_alloc_bytes_per_op'] = 0
        self.assertTrue(any(c['status'] == 'undefined-zero' for c in a.allocation_comparisons(ca, rows, 'grv')))

    def test_budget_absent_phase_never_uses_diagnostic_capacity(self):
        self.adapter.protocol['phases'] = [dict(id='M1w32', kind='client', workloads=['grv'])]
        (self.root / 'results-raw/diag-m1-compat/grv').mkdir(parents=True)
        result = self.adapter.phase('M1w32', 'budget: omitted under 11:15 rule')
        self.assertEqual(result['status'], 'N/A')
        self.assertIn('budget', result['reason'])

    def test_overloaded_m1_whole_case_does_not_call_analyzer(self):
        self.adapter.protocol['phases'] = [dict(id='M1w1', kind='client', workloads=['grv'])]
        (self.root / 'results-raw/M1w1').mkdir(parents=True)
        self.adapter.controller = lambda *args: ({}, {})
        self.adapter.mn = self.root / 'fake-mn'
        self.adapter.mn.mkdir()
        for name in ('reconcile_telemetry.py', 'node_sampler.py'):
            (self.adapter.mn / name).write_text('fake')
        report = reconciliation('M1w1', 'contaminated')
        report['tool_sha256'] = {name: a.sha(self.adapter.mn / name) for name in ('reconcile_telemetry.py', 'node_sampler.py')}
        write(self.root / 'reconcile/M1w1/report.json', report)
        write(self.root / 'reconcile/M1w1/input.json', {'expected_cases': ['grv']})
        self.adapter.client_case = lambda *args: self.fail('overloaded case reached analyzer')
        result = self.adapter.phase('M1w1')
        self.assertEqual(result['workloads']['grv']['status'], 'N/A')

    def test_controller_accepts_r2_protocol_pin_without_top_level_hash(self):
        folder = self.root / 'phase-controller/M2rl'
        pins = {'/srv/fdb-bench/protocol.json': a.sha(self.adapter.protocol_path)}
        write(folder / 'manifest.json', {'pinned_files': pins})
        write(folder / 'inputs.json', dict(phase='M2rl', diagnostic=False, expected_cases=['grv'],
            protocol='/srv/fdb-bench/protocol.json', manifest_sha256=a.sha(folder / 'manifest.json')))
        write(folder / 'complete.json', dict(phase='M2rl', label='M2rl', diagnostic=False, runner_quiescent=True,
            errors=[], reconcile_rc=0))
        write(folder / 'client-pins-before-r4.json', dict(actual=pins, errors=[]))
        self.adapter.tool_paths = []
        manifest, _ = self.adapter.controller('M2rl', ['grv'])
        self.assertEqual(manifest['pinned_files'], pins)

    def test_m1_two_cli_denominators_and_all_contrasts_on_copies(self):
        raw = self.root / 'results-raw/M1w1/grv'
        dest = self.adapter.out / 'M1w1/grv'
        dest.mkdir(parents=True)
        arms = [dict(name=name, source_revision='old' if name == 'before' else 'new', binary='/bin/' + name)
                for name in ('before', 'final', 'cgo')]
        pins = {arm['binary']: self.adapter.binary_hashes[{'before': 'old/client-go', 'final': 'master/client-go', 'cgo': 'master/client-cgo'}[arm['name']]] for arm in arms}
        plan = dict(blocks=6, arms=arms, workloads=[dict(name='grv', workers=1)], expected_sha256=pins, native_library_sha256='e' * 64)
        schedule = [dict(block=b, arm=arm, workload={'name': 'grv'}, run_id=f'b{b}-{arm}') for b in range(6) for arm in ('before', 'final', 'cgo')]
        for name, value in [('plan.json', plan), ('schedule.json', schedule), ('complete.json', {'scheduled': 18, 'completed': 18})]:
            write(raw / name, value)
        manifest = {'campaigns': [dict(phase='M1w1', workload='grv', plan_sha256=a.sha(raw / 'plan.json'))]}
        manifest_path = self.root / 'phase-controller/M1w1/manifest.json'
        write(manifest_path, manifest)
        write(raw / 'adapter.json', dict(manifest_sha256=a.sha(manifest_path), protocol_sha256=a.sha(self.adapter.protocol_path)))
        calls = []

        def fake_cli(command, **kwargs):
            calls.append(command)
            target = Path(command[3])
            denominator = command[command.index('--denominator') + 1]
            self.assertEqual(command[-4:], ['--seed', '492817', '--resamples', '20000'])
            comparisons = [dict(workload='grv', numerator=n, denominator=denominator, metric=m, blocks=6, ratio=1, ci95_low=1, ci95_high=1)
                           for n in ('before', 'final', 'cgo') if n != denominator for m in a.CLIENT_METRICS]
            write(target / 'analysis.json', dict(valid_zero_error_campaign=True, issues=[], bootstrap_seed=492817,
                bootstrap_resamples=20000, per_run=client_rows(), comparisons=comparisons, unavailable_comparisons=[]))
            return SimpleNamespace(returncode=0, stdout='fake analyzer only', stderr='')

        self.adapter.run = fake_cli
        rec = dict(case='grv', runs=[{'run_id': r['run_id']} for r in schedule])
        result = self.adapter.client_case('M1w1', {'workers': 1}, 'grv', raw, rec, dest, manifest)
        self.assertEqual(len(calls), 2)
        self.assertEqual(len(result['comparisons']), 36)
        self.assertTrue((dest / 'paired-all.csv').exists())
        self.assertFalse((raw / 'analysis.json').exists())

    def m2_fixture(self, verdict='clean'):
        raw = self.root / 'results-raw/M2rl'
        dest = self.adapter.out / 'M2rl'
        dest.mkdir()
        key = 'small-limit1-forward'
        metrics = {'ns/op': 1, 'B/op': 2, 'allocs/op': 3, 'rows/op': 1, 'prefix-B': 19}
        parsed = [dict(repeat=i, N=1, metrics=metrics, measured_prefix_bytes=19) for i in range(3)]
        audits = [{'case': 'fake'}]
        jobs, samples = [], []
        for block in range(6):
            for position, arm in enumerate(('final-ordinary', 'final-remote')):
                job = dict(id=f'b{block}-{arm}', case=key, block=block, position=position, arm=arm)
                jobs.append(job)
                folder = raw / job['id']
                for name, value in [('result.json', {'status': 'ok'}), ('parsed.json', parsed), ('audits.json', audits)]:
                    write(folder / name, value)
                (folder / 'stdout.txt').write_text('FAKE')
                (folder / 'stderr.txt').write_text('')
                samples += [dict(phase='M2rl', case=key, block=block, position=position, arm=arm, repeat=i, N=1, **metrics) for i in range(3)]
        write(raw / 'omitted.json', {})
        (raw / 'metrics.csv').write_text('fake load_samples input')
        write(self.root / 'reconcile/M2rl/report.json', {'fake': True})
        calls = []

        def parse(text, view, **kwargs):
            calls.append(view)
            self.assertEqual(view['case']['audit'], 'fake-audit-case')
            self.assertEqual(kwargs, {'repeats': 3, 'gomaxprocs': '8'})
            return copy.deepcopy(parsed), audits

        def summarize(plan, rows, omitted):
            self.assertEqual((plan['analysis_seed'], plan['bootstrap_resamples'], plan['phase']['blocks']), (492817, 20000, 6))
            if verdict == 'clean':
                self.assertTrue(all(r['audit_prefix_bytes'] == 19 for r in rows))
            else:
                self.assertIn(key, omitted)
            return dict(cases={key: {'status': 'ok' if verdict == 'clean' else 'N/A'}}, comparisons=[])

        self.adapter.ba = SimpleNamespace(load_samples=lambda path: samples, summarize=summarize)
        self.adapter.br = SimpleNamespace(METRICS=tuple(metrics), parse_output=parse)
        self.adapter.mr = SimpleNamespace(audit_case=lambda case: {'audit': 'fake-audit-case'})
        case = dict(key=key, placement_label='cross-server', placement_fraction=1)
        plan = dict(cases=[case], jobs=jobs, orders=[['final-ordinary', 'final-remote']] * 6, env={'GOMAXPROCS': '8'})
        spec = dict(blocks=6, repeats_per_arm_process=3, arms=['final-ordinary', 'final-remote'])
        recs = {key: dict(case=key, verdict=verdict, runs=[{'run_id': j['id']} for j in jobs], contaminated_reasons=['overload'], unverifiable_reasons=[])}
        write(raw / 'plan.json', plan)
        write(raw / 'complete.json', dict(scheduled=12, completed=12, samples=36, pins_unchanged=True,
                                          plan_sha256=a.sha(raw / 'plan.json')))
        return raw, dest, plan, spec, recs, calls

    def test_m2_validates_all_raw_repeats_with_existing_apis(self):
        raw, dest, plan, spec, recs, calls = self.m2_fixture()
        report = self.adapter.testingb('M2rl', spec, raw, recs, dest, plan)
        self.assertEqual(len(calls), 12)
        self.assertEqual(report['cases']['small-limit1-forward']['placement_fraction'], 1)

    def test_m2_csv_tamper_refuses_instead_of_using_partial_population(self):
        raw, dest, plan, spec, recs, calls = self.m2_fixture()
        self.adapter.ba.load_samples(raw / 'metrics.csv')[0]['N'] = 2
        with self.assertRaises(a.Refused):
            self.adapter.testingb('M2rl', spec, raw, recs, dest, plan)
        self.assertFalse((dest / 'report.json').exists())

    def test_historical_comparison_is_only_cgo_side_by_side(self):
        self.adapter.prior = self.root / 'prior'
        self.adapter.protocol['phases'] = [dict(id='M1w1', workers=1)]
        write(self.adapter.prior / 'provenance/source-receipt.json', {'production_revisions': {'final': 'old'}})
        write(self.adapter.prior / 'reconcile/M1w1/report.json', reconciliation('M1w1'))
        old = dict(status='clean', comparisons={'mean_ns': {'ratio': 1.1}},
                   per_run=[{'workload': 'grv', 'workers': 1}])
        write(self.adapter.prior / 'analysis/M1w1/summary.json', {'workloads': {'grv': old}})
        current = [dict(metric='mean_ns', numerator=n, denominator=d, ratio=2) for n, d in a.CONTRASTS]
        result = a.Adapter.historical(self.adapter, 'M1w1', 'grv', current)
        self.assertEqual(result['status'], 'descriptive-only')
        self.assertEqual(len(result['records']), 2)
        self.assertTrue(all(r['historical']['ratio'] == 1.1 for r in result['records']))
        self.assertTrue(all(r['current_denominator'] == 'cgo' for r in result['records']))

    def test_m2_overload_omits_whole_case_before_raw_parsing(self):
        raw, dest, plan, spec, recs, calls = self.m2_fixture('contaminated')
        report = self.adapter.testingb('M2rl', spec, raw, recs, dest, plan)
        self.assertEqual(calls, [])
        self.assertEqual(report['cases']['small-limit1-forward']['status'], 'N/A')


if __name__ == '__main__':
    unittest.main(verbosity=2)
