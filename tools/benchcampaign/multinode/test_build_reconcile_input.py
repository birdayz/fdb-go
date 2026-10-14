#!/usr/bin/env python3
"""End-to-end tests for build_reconcile_input.py: a fake R2 (or R1) phase run, the real
node_sampler loop on fake hosts, the builder, then reconcile_telemetry.py (R4).
Synthetic fixtures only: no FDB, ssh, cloud, chronyc or systemctl."""
import contextlib
import datetime as dt
import io
import json
import os
from pathlib import Path
import shutil
import sys
import unittest

HERE = Path(os.path.abspath(__file__)).parent
sys.path.insert(0, str(HERE))

import build_reconcile_input as bri  # noqa: E402
import cluster_gate as cg  # noqa: E402
import mn_common as mc  # noqa: E402
import node_sampler as ns  # noqa: E402
import reconcile_telemetry as rt  # noqa: E402
import test_bench_runner_mn as tr2  # noqa: E402
import test_client_campaign_mn as tr1  # noqa: E402
from test_node_sampler import M0_NS, T0_NS, FakeHost, chrony_text, node_units  # noqa: E402

generic = cg.generic
SMALL, SPLIT = 'small-rows10-forward', 'split-rows10-forward'


def utc_clock(start_s, step_s):
    """run.py-format UTC stamps on the fake hosts' timeline: T0 + start, + step per call."""
    t = [start_s]

    def utc():
        ns_ = T0_NS + round(t[0] * 1e9)
        t[0] += step_s
        return dt.datetime.fromtimestamp(ns_ // 10**9, dt.timezone.utc).replace(microsecond=(ns_ % 10**9) // 1000).isoformat()
    return utc


class Telemetry:
    """node_sampler outputs for every layout node and the client's chrony receipts."""

    def __init__(self, tmp, layout, phase):
        self.tmp, self.layout, self.phase = tmp, layout, phase
        self.hosts = {}
        for i, node in enumerate(('node1', 'node2', 'node3')):
            ports = [p['port'] for p in layout['processes'] if p['node'] == node]
            self.hosts[node] = FakeHost(node, units=node_units(ports, base_pid=1000 * (i + 1)), stop_at=60)

    def materialize(self):
        for node, host in self.hosts.items():
            ns.run_sampler(host, self.tmp / 'telemetry' / node, node, self.phase, {0, 1}, 1.0, 3600, ns.UNIT_GLOB, [],
                           None, ['node_sampler.py'], host.stop)
        (self.tmp / 'client').mkdir()
        for kind, at in (('before', -5.0), ('after', 65.0)):
            h = FakeHost('client', units={})
            h.mono = M0_NS + int(at * 1e9)
            h.chrony = [chrony_text()]
            mc.dump_json(self.tmp / 'client' / f'{kind}.json',
                         ns.build_receipt(h, kind, 'client', self.phase, {}, False, ns.UNIT_GLOB, [], None, []))

    def argv(self):
        return [*sum([['--client-receipt', str(self.tmp / 'client' / f'{k}.json')] for k in ('before', 'after')], []),
                *sum([['--node', f'{n}={self.tmp / "telemetry" / n}'] for n in self.hosts], [])]


def quiet(fn, *args):
    with contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(io.StringIO()):
        return fn(*args)


class TestingBPhaseTest(tr2.Phase):
    """R2 fake run of M2rl (cases small and split, 12 processes each, at T0+10 s .. T0+34 s)."""

    def setUp(self):
        super().setUp()
        self.telemetry = Telemetry(self.tmp, self.lay, 'M2rl')

    def build(self, **over):
        t = self.tmp
        values = dict(manifest=t / 'plans' / 'manifest.json', sha=generic.sha(t / 'plans' / 'manifest.json'),
                      results=t / 'results' / 'M2rl', layout=t / 'layout.json', output=t / 'input.json')
        values.update(over)
        argv = ['--manifest', str(values['manifest']), '--manifest-sha256', values['sha'], '--phase', 'M2rl',
                '--protocol', str(values.get('protocol', t / 'protocol.json')), '--results', str(values['results']), '--layout', str(values['layout']), '--output', str(values['output']),
                *self.telemetry.argv()]
        return quiet(bri.main, argv)

    def reconcile(self):
        self.assertEqual(self.build(), 0)
        rc = quiet(rt.main, ['--input', str(self.tmp / 'input.json'), '--output', str(self.tmp / 'report.json')])
        self.assertNotEqual(rc, 2, 'R4 refused the builder output')
        report = json.loads((self.tmp / 'report.json').read_text())
        return rc, {c['case']: c for c in report['cases']}

    def run_all(self):
        self.assertEqual(self.run_phase(utc=utc_clock(10.0, 0.5)), 0)
        self.telemetry.materialize()

    def test_clean_phase_end_to_end(self):
        self.run_all()
        rc, cases = self.reconcile()
        self.assertEqual({k: c['verdict'] for k, c in cases.items()}, {SMALL: 'clean', SPLIT: 'clean'},
                         json.dumps({k: c['unverifiable_reasons'][:3] + c['contaminated_reasons'][:3] for k, c in cases.items()}))
        self.assertEqual(rc, 0)
        self.assertEqual(len(cases[SMALL]['runs']), 12)
        doc = json.loads((self.tmp / 'input.json').read_text())
        self.assertEqual(doc['expected_cases'], [SMALL, SPLIT])
        self.assertTrue(doc['placement_required'])
        self.assertEqual(doc['builder']['missing'], [])
        self.assertEqual(doc['expected_dd_state'], 'frozen')
        self.assertTrue(all(Path(p).is_absolute() for c in doc['cases'] for p in c['placement_compares']))
        for c in doc['cases']:
            self.assertEqual((c['runner_verdict']['omitted'], c['runner_verdict']['adverse']), (False, []))
            self.assertEqual(c['runner_verdict']['source_sha256'], generic.sha(c['runner_verdict']['source']))
            self.assertEqual((len(c['run_results']), c['runs']), (12, []))

    def test_missing_receipt_makes_the_case_unverifiable(self):
        self.run_all()
        (self.tmp / 'results' / 'M2rl' / 'cases' / SPLIT / 'status-after.json').unlink()
        rc, cases = self.reconcile()
        self.assertEqual(rc, 3)
        self.assertEqual((cases[SMALL]['verdict'], cases[SPLIT]['verdict']), ('clean', 'unverifiable'))
        self.assertTrue(any('status json after' in r for r in cases[SPLIT]['unverifiable_reasons']))
        doc = json.loads((self.tmp / 'input.json').read_text())
        self.assertEqual(len(doc['builder']['missing']), 1)

    def test_lost_case_directory_is_unverifiable(self):
        self.run_all()
        shutil.rmtree(self.tmp / 'results' / 'M2rl' / 'cases' / SPLIT)
        rc, cases = self.reconcile()
        self.assertEqual((cases[SMALL]['verdict'], cases[SPLIT]['verdict']), ('clean', 'unverifiable'))

    def test_lost_job_directory_is_a_missing_window_not_a_dropped_run(self):
        self.run_all()
        job = next(j['id'] for j in self.plan['jobs'] if j['case'] == SPLIT)
        shutil.rmtree(self.tmp / 'results' / 'M2rl' / job)
        rc, cases = self.reconcile()
        self.assertEqual((cases[SMALL]['verdict'], cases[SPLIT]['verdict']), ('clean', 'unverifiable'))
        doc = json.loads((self.tmp / 'input.json').read_text())
        split = next(c for c in doc['cases'] if c['case'] == SPLIT)
        self.assertEqual((len(split['run_results']), split['runs']), (11, [dict(run_id=job, start_utc=None, end_utc=None)]))
        self.assertTrue(any(job in m for m in doc['builder']['missing']))

    def test_runner_omission_on_qos_lag_alone_is_never_clean(self):
        # The post-case gate of the first case sees only a durability-lag finding; roles and generation unchanged.
        lag = tr1.status(self.lay, dd_disabled=True,
                         mutate=lambda s: s['cluster']['qos']['worst_durability_lag_storage_server'].update(seconds=2.5))
        ok = tr1.status(self.lay, dd_disabled=True)
        self.statuses = [ok] * 25 + [lag, ok]  # 1 case-before + 12 x (pre-arm, post-run) + case-after
        self.assertEqual(self.run_phase(utc=utc_clock(10.0, 0.5)), 2)
        self.assertIn('durability lag', json.dumps(self.out('omitted.json')))
        self.telemetry.materialize()
        rc, cases = self.reconcile()
        doc = json.loads((self.tmp / 'input.json').read_text())
        verdict = next(c for c in doc['cases'] if c['case'] == SMALL)['runner_verdict']
        self.assertTrue(verdict['omitted'])
        self.assertTrue(any('durability lag' in a for a in verdict['adverse']))
        self.assertNotEqual(cases[SMALL]['verdict'], 'clean')
        self.assertEqual(cases[SPLIT]['verdict'], 'clean')

    def test_missing_runner_record_gives_null_verdict(self):
        self.run_all()
        (self.tmp / 'results' / 'M2rl' / 'omitted.json').unlink()
        rc, cases = self.reconcile()
        doc = json.loads((self.tmp / 'input.json').read_text())
        self.assertTrue(all(c['runner_verdict']['omitted'] is None for c in doc['cases']))
        self.assertTrue(any('runner verdict' in m for m in doc['builder']['missing']))
        self.assertEqual({c['verdict'] for c in cases.values()}, {'unverifiable'})

    def test_steal_on_a_node_contaminates_only_the_overlapping_case(self):
        host = self.telemetry.hosts['node1']
        for second in (12, 13):
            host.cpu_events[(second, 10)] = (40, 50, 10)  # 10% steal on an FDB storage CPU
        self.run_all()
        rc, cases = self.reconcile()
        self.assertEqual(rc, 3)
        self.assertEqual((cases[SMALL]['verdict'], cases[SPLIT]['verdict']), ('contaminated', 'clean'))
        self.assertTrue(any('steal' in r for r in cases[SMALL]['contaminated_reasons']))

    def test_unequal_placement_compare_contaminates(self):
        self.fresh[('benchfetch/split', 'after')] = tr2.receipt('benchfetch/split', record_servers=['10.88.0.12:4503', '10.88.0.11:4505'])
        self.assertEqual(self.run_phase(utc=utc_clock(10.0, 0.5)), 2)
        self.telemetry.materialize()
        rc, cases = self.reconcile()
        self.assertEqual(cases[SPLIT]['verdict'], 'contaminated')
        self.assertTrue(any('placement changed' in r for r in cases[SPLIT]['contaminated_reasons']))

    def test_sealed_plan_that_drops_a_case_is_refused(self):
        self.run_all()
        t = self.tmp
        forged = t / 'plans-forged'
        shutil.copytree(t / 'plans', forged)
        plan = json.loads((forged / 'M2rl.plan.json').read_text())
        plan['cases'] = [c for c in plan['cases'] if c['key'] != SPLIT]
        plan['jobs'] = [j for j in plan['jobs'] if j['case'] != SPLIT]
        (forged / 'M2rl.plan.json').write_text(json.dumps(plan))
        manifest = json.loads((forged / 'manifest.json').read_text())
        manifest['phases'][0]['plan_sha256'] = generic.sha(forged / 'M2rl.plan.json')
        (forged / 'manifest.json').write_text(json.dumps(manifest))
        err = io.StringIO()
        with contextlib.redirect_stderr(err), contextlib.redirect_stdout(io.StringIO()):
            rc = bri.main(['--manifest', str(forged / 'manifest.json'), '--manifest-sha256', generic.sha(forged / 'manifest.json'),
                           '--phase', 'M2rl', '--protocol', str(t / 'protocol.json'), '--results', str(t / 'results' / 'M2rl'),
                           '--layout', str(t / 'layout.json'), '--output', str(t / 'input.json'), *self.telemetry.argv()])
        self.assertEqual(rc, 2)
        self.assertIn('protocol cases minus', err.getvalue())

    def test_refusals(self):
        self.run_all()
        t = self.tmp
        cases = [
            ('manifest not acknowledged', dict(sha='0' * 64)),
            ('results of no runner', dict(results=t / 'plans')),
            ('pinned file that is no layout', dict(layout=t / 'protocol.json')),
            ('layout not pinned', dict(layout=t / 'layout-copy.json')),
            ('protocol not the sealed one', dict(protocol=t / 'protocol-copy.json')),
        ]
        (t / 'layout-copy.json').write_text((t / 'layout.json').read_text() + ' ')
        (t / 'protocol-copy.json').write_text((t / 'protocol.json').read_text() + ' ')
        for name, over in cases:
            with self.subTest(name):
                self.assertEqual(self.build(**over), 2)
                self.assertFalse((t / 'input.json').exists())
        with self.subTest('node directory missing'):
            shutil.move(t / 'telemetry' / 'node3', t / 'node3-elsewhere')
            self.assertEqual(self.build(), 2)
            shutil.move(t / 'node3-elsewhere', t / 'telemetry' / 'node3')
        with self.subTest('client receipt missing'):
            (t / 'client' / 'after.json').rename(t / 'client' / 'after.moved')
            self.assertEqual(self.build(), 2)
            (t / 'client' / 'after.moved').rename(t / 'client' / 'after.json')
        with self.subTest('receipt changed after its runner recorded it'):
            path = t / 'results' / 'M2rl' / 'cases' / SMALL / 'check-roles-after.json'
            path.write_text(path.read_text().replace('"passed": true', '"passed": false'))
            self.assertEqual(self.build(), 2)
        with self.subTest('output inside a Git work tree'):
            self.assertEqual(self.build(output=HERE / 'reconcile-input-never-written.json'), 2)
            self.assertFalse((HERE / 'reconcile-input-never-written.json').exists())


class ClientPhaseTest(tr1.CampaignFixture):
    """R1 fake campaign of M1w1/get100 (12 runs from T0+10 s), then builder and R4."""

    def campaign(self, fail_after=None):
        """Run R1 with a run.py stand-in writing run.py's result.json windows; optionally abort after N runs."""
        utc = utc_clock(10.0, 1.0)

        def execute(plan, schedule, plan_path, out, deadline):
            for i, entry in enumerate(schedule):
                if fail_after is not None and i == fail_after:
                    raise TimeoutError('campaign wall cap reached before next run')
                run = out / entry['run_id']
                run.mkdir()
                helper.snapshot(run / 'before')
                started = utc()
                helper.telemetry(4242)
                helper.telemetry(4242)
                helper.snapshot(run / 'after')
                generic.dump(run / 'result.json', dict(run_id=entry['run_id'], started_utc=started, ended_utc=utc(),
                                                       returncode=0))
                helper.validate_raw_output(run, entry['workload'])
            generic.dump(out / 'complete.json', dict(scheduled=len(schedule), completed=len(schedule)))
            return 0
        helper = self.helper(execute=execute)
        if fail_after is None:
            self.assertEqual(self.run_adapter(self.args(), helper=helper)[0], 0)
        else:
            with self.assertRaises(TimeoutError):
                self.run_adapter(self.args(), helper=helper)
        self.telemetry = Telemetry(self.tmp, self.lay, 'M1w1')
        self.telemetry.materialize()
        self.folder = self.tmp / 'results' / 'M1w1' / 'get100'

    def build(self, output='input.json'):
        t = self.tmp
        argv = ['--manifest', str(t / 'plans' / 'manifest.json'), '--manifest-sha256', generic.sha(t / 'plans' / 'manifest.json'),
                '--phase', 'M1w1', '--protocol', str(t / 'protocol.json'), '--results', str(t / 'results' / 'M1w1'),
                '--layout', str(t / 'layout.json'), '--output', str(t / output), *self.telemetry.argv()]
        return quiet(bri.main, argv)

    def reconcile(self, name):
        self.assertEqual(self.build(f'{name}.input.json'), 0)
        rc = quiet(rt.main, ['--input', str(self.tmp / f'{name}.input.json'), '--output', str(self.tmp / f'{name}.json')])
        self.assertNotEqual(rc, 2, 'R4 refused the builder output')
        return rc, json.loads((self.tmp / f'{name}.json').read_text())['cases'][0]

    def run_ids(self):
        return [r['run_id'] for r in json.loads((self.tmp / 'plans' / 'M1w1' / 'get100' / 'schedule.json').read_text())]

    def test_clean_client_phase_end_to_end(self):
        self.campaign()
        rc, case = self.reconcile('clean')
        self.assertEqual((rc, case['verdict']), (0, 'clean'))
        doc = json.loads((self.tmp / 'clean.input.json').read_text())
        self.assertEqual((doc['expected_cases'], doc['placement_required'], doc['expected_dd_state']), (['get100'], False, 'on'))
        self.assertEqual((len(doc['cases'][0]['run_results']), doc['cases'][0]['runs']), (12, []))
        self.assertEqual(doc['cases'][0]['runner_verdict']['omitted'], False)

    def test_lost_result_or_run_directory_is_a_missing_window(self):
        self.campaign()
        ids = self.run_ids()
        (self.folder / ids[3] / 'result.json').unlink()
        shutil.rmtree(self.folder / ids[7])
        rc, case = self.reconcile('lost')
        self.assertEqual((rc, case['verdict']), (3, 'unverifiable'))
        doc = json.loads((self.tmp / 'lost.input.json').read_text())
        self.assertEqual(sorted(r['run_id'] for r in doc['cases'][0]['runs']), sorted([ids[3], ids[7]]))
        self.assertEqual(len(doc['cases'][0]['run_results']), 10)

    def test_altered_runtime_schedule_is_refused(self):
        self.campaign()
        runtime = self.folder / 'schedule.json'
        shortened = json.loads(runtime.read_text())[:-1]  # the dropped run would otherwise vanish from R4
        runtime.write_text(json.dumps(shortened))
        self.assertEqual(self.build(), 2)
        self.assertFalse((self.tmp / 'input.json').exists())

    def test_adverse_workload_is_never_clean(self):
        self.campaign(fail_after=5)
        rc, case = self.reconcile('adverse')
        doc = json.loads((self.tmp / 'adverse.input.json').read_text())
        verdict = doc['cases'][0]['runner_verdict']
        self.assertTrue(verdict['omitted'])
        self.assertTrue(any('ADAPTER_ADVERSE' in a for a in verdict['adverse']))
        self.assertEqual(verdict['source'], str((self.tmp / 'results' / 'M1w1' / 'client-phase-state.json').resolve()))
        self.assertEqual(len(doc['cases'][0]['runs']), 7)  # the never-run tail is accounted, not dropped
        self.assertNotEqual(case['verdict'], 'clean')
        self.assertIn('runner omitted/adverse', json.dumps(case['contaminated_reasons']))


if __name__ == '__main__':
    unittest.main()
