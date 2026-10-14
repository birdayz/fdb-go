"""Stdlib unit tests only; never start FDB, benchmarks, SSH or builds.

Inputs are testdata/ beside this file. Set FDB_BENCH_DIAGNOSTICS_DIR to a real
results/smoke-parser-all directory to also validate a complete real diagnostic
population (69 logs); unset, that one test is skipped.
"""
import copy
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest import mock

import bench_runner as r
import bench_analyze as a

D = Path(__file__).parent/'testdata'
GOMAXPROCS = json.loads((D/'protocol.json').read_text())['runtime']['GOMAXPROCS']


def parse(text, job):
    return r.parse_output(text, job, gomaxprocs=GOMAXPROCS)


def diagnostic_jobs():
    expected = {}
    for phase in ('B', 'C1', 'D1', 'C2', 'D2'):
        arms = ['legacy', 'before', 'final'] if phase in ('B', 'D1') else ['pr2-ordinary', 'pr2-remote', 'final-ordinary', 'final-remote'] if phase == 'C1' else ['final-ordinary', 'final-remote']
        for i, case in enumerate(r.cases_for(phase)):
            for arm in arms:
                expected[f'{phase}-{i:02d}-{arm}.log'] = dict(case=case, arm=arm, name=r.benchmark_name(case, arm))
    return expected


def validate_actual_diagnostics(folder):
    folder = Path(folder)
    expected = diagnostic_jobs()
    actual = {p.name for p in folder.glob('*.log')}
    if actual != set(expected) or len(actual) != 69:
        raise ValueError('actual diagnostic population must be exactly69 declared case/arm logs')
    population = 0
    for name, job in expected.items():
        command = json.loads((folder/(name[:-4]+'.command.json')).read_text())
        if '-test.v' not in command or '-test.count=3' not in command:
            raise ValueError('actual diagnostic did not enable parent audit logging/count3')
        rows, _ = parse((folder/name).read_text(), job)
        population += len(rows)
    complete = json.loads((folder/'complete.json').read_text())
    if complete != dict(diagnostic_only=True, processes=69, rows=207) or population != 207:
        raise ValueError('actual diagnostic completion population mismatch')
    return dict(diagnostic_only=True, processes=69, result_rows=population,
                parser_sha256=r.sha(r.__file__), raw_logs_sha256={name: r.sha(folder/name) for name in sorted(expected)})


class RunnerTests(unittest.TestCase):
    def plan(self, phase='B'):
        protocol = json.loads((D/'protocol.json').read_text())
        orders = json.loads((D/'orders.json').read_text())
        paths = dict(protocol='/p/protocol.json', orders='/p/orders.json', runner='/p/bench_runner.py',
                     analyzer='/p/bench_analyze.py', source_receipt='/p/source.json')
        pins = {p: 'a'*64 for p in [*(str(r.ROOT/'bin'/b) for b in r.BINARIES), r.CLUSTER, r.ENGINE, r.HELPER, *paths.values(), '/p/network.json']}
        return r.make_plan(protocol, orders, phase, pins, paths, '/p/network.json' if phase == 'C3' else None)

    def output(self, job, zero=False):
        c = job['case']
        metrics = f"100 ns/op {0 if zero else 4} B/op {0 if zero else 2} allocs/op"
        if c['audit'] in ('fetch', 'planskip', 'sql-fetch'):
            metrics += f" {c['rows']} rows/op 20 prefix-B"
        if c['audit'] in ('sql-read', 'sql-write'):
            metrics += ' 20 prefix-B'
        lines = [f"{job['name']}-8 {1000 if c['write'] else 100} {metrics}" for _ in range(3)]
        audits = []
        if c['audit'] == 'classic-write':
            audits = [dict(case=job['name'], first_id=0, count=1000, present=True, n=1000, prefix_bytes=20) for _ in range(3)]
        elif c['audit'] in ('sql-read', 'sql-write'):
            audits = [dict(case=job['name'], rows=c['rows'], first_id=1, last_id=1000, query='SELECT a', checksum=42, prefix_bytes=20) for _ in range(3)]
        elif c['audit'] != 'none':
            fixture = dict(case=job['name'].split('/')[0], phase='fixture', rows=1000, prefix_bytes=20,
                           **{k: v for k, v in c['fields'].items() if k in ('payload', 'payload_bytes', 'chunks')})
            fixture['split'] = c['fields'].get('payload') == 'split'
            audits.append(fixture)
            remote = job['arm'].endswith('-remote')
            for _ in range(3):
                for phase in ('before', 'after'):
                    audits.append(dict(case=job['name'], phase=phase, method='USE_REMOTE_FETCH' if remote else 'SCAN_AND_FETCH',
                                       rows=c['rows'], remote_openings=int(remote), remote_entries=c['rows']+4 if remote else 0,
                                       index_scans=0 if remote else 1, record_loads=0 if remote else c['rows'],
                                       checksum=42, prefix_bytes=20, query='SELECT a', **c['fields']))
        return '\n'.join(lines+['log FDB_BENCH_AUDIT '+json.dumps(x) for x in audits]+['PASS'])

    def test_frozen_populations_and_affinity(self):
        for phase, count in [('B', 144), ('C1', 192), ('D1', 54), ('C2', 48), ('D2', 24), ('C3', 24)]:
            plan = self.plan(phase)
            self.assertEqual(len(plan['jobs']), count)
            self.assertEqual(len({j['id'] for j in plan['jobs']}), count)
            for j in plan['jobs']:
                self.assertEqual(j['command'][:3], ['taskset', '--cpu-list', '8-15'])
                self.assertIn('-test.count=3', j['command'])
                self.assertIn('-test.run=^$', j['command'])
                self.assertIn('-test.v', j['command'])
                self.assertEqual(j['name'], r.benchmark_name(j['case'], j['arm']))
                self.assertTrue(all(part.startswith('^') and part.endswith('$') for part in next(x for x in j['command'] if x.startswith('-test.bench=')).split('=', 1)[1].split('/')))

    def test_orders_empty_duplicate_unbalanced_and_williams(self):
        p = self.plan('C1')['phase']
        for orders in ([], [['final-ordinary']]*8, [p['arms']]*8):
            with self.assertRaises(ValueError):
                r.validate_orders(p, orders)
        valid = self.plan('C1')['orders']
        broken = copy.deepcopy(valid)
        broken[0] = list(reversed(broken[0]))
        with self.assertRaises(ValueError):
            r.validate_orders(p, broken)
        r.validate_orders(p, valid)

    def test_all_case_parsers_and_zero_allocations(self):
        for phase in r.PHASES:
            for job in self.plan(phase)['jobs']:
                rows, audits = parse(self.output(job, zero=True), job)
                self.assertEqual(len(rows), 3)
                self.assertEqual(rows[0]['metrics']['B/op'], 0)

    def test_exact_name_nonempty_population_and_malformed_metrics(self):
        j = self.plan()['jobs'][0]
        text = self.output(j)
        bad = [text.replace('100 ns/op', 'nan ns/op'), text.replace('100 ns/op', 'inf ns/op'),
               text.replace('100 ns/op', '0 ns/op'), text.replace('4 B/op', '-1 B/op'),
               text.replace('2 allocs/op', 'oops allocs/op'), text.replace('1000 100', '0 100'),
               text.replace(j['name']+'-8', 'BenchmarkWrong-8'), text.replace('-8 1000', '-16 1000'),
               text.replace('100 ns/op', '100 ms/op'), text.replace(' 2 allocs/op', ''),
               text.replace('PASS', 'FAIL'), '', text+'\n'+text, text.splitlines()[0]+'\nPASS',
               text.replace('100 ns/op', '100 ns/op 100 ns/op')]
        for b in bad:
            with self.subTest(b=b[:60]), self.assertRaises(ValueError):
                parse(b, j)

    def test_audit_guards_and_skip_shape(self):
        plan = self.plan('C2')
        j = next(j for j in plan['jobs'] if j['case']['audit'] == 'planskip' and j['arm'] == 'final-remote')
        self.assertEqual(j['case']['rows'], 10)
        text = self.output(j)
        no_fixture = '\n'.join(line for line in text.splitlines() if '"phase": "fixture"' not in line)
        with self.assertRaisesRegex(ValueError, 'fixture receipt'):
            parse(no_fixture, j)
        for b in [text.replace('"phase": "after"', '"phase": "before"'),
                  text.replace('"remote_openings": 1', '"remote_openings": 0'),
                  text.replace('"remote_entries": 14', '"remote_entries": 13'),
                  text.replace('"checksum": 42', '"checksum": null'),
                  text.replace('10 rows/op', '7 rows/op'), text.replace('20 prefix-B', '21 prefix-B'),
                  text.replace('"skip": 3', '"skip": 0'), text.replace('"rows": 1000', '"rows": 0'),
                  text.replace('FDB_BENCH_AUDIT {', 'FDB_BENCH_AUDIT INVALID {')]:
            with self.assertRaises(ValueError):
                parse(b, j)

    def test_classic_reads_do_not_demand_fake_audits(self):
        j = next(j for j in self.plan()['jobs'] if j['case']['key'] == 'LoadRecord')
        self.assertEqual(parse(self.output(j), j)[1], [])

    def test_pins_and_overwrites(self):
        with tempfile.TemporaryDirectory() as folder:
            p = Path(folder)/'input'
            p.write_text('frozen')
            plan = dict(expected_sha256={str(p): r.sha(p)})
            r.verify_pins(plan)
            p.write_text('mutated')
            with self.assertRaises(r.EnvironmentFailure):
                r.verify_pins(plan)
            receipt = Path(folder)/'receipt'
            r.dump(receipt, {'ok': True}, exclusive=True)
            with self.assertRaises(FileExistsError):
                r.dump(receipt, {}, exclusive=True)
            with self.assertRaises(FileExistsError):
                a.analyze('/nonexistent', folder)

    def test_missing_pins_and_c3_receipt(self):
        p = self.plan()
        protocol = json.loads((D/'protocol.json').read_text())
        orders = json.loads((D/'orders.json').read_text())
        pins = dict(p['expected_sha256'])
        del pins[r.CLUSTER]
        with self.assertRaises(ValueError):
            r.make_plan(protocol, orders, 'B', pins, p['paths'])
        with self.assertRaises(ValueError):
            r.make_plan(protocol, orders, 'C3', p['expected_sha256'], p['paths'])

    def test_malformed_phase_cases_rejected(self):
        plan = self.plan()
        for phase, field, value in [('B', 'workloads', []), ('C1', 'cells', {}), ('C2', 'cells', ['unknown']), ('D2', 'rows', [])]:
            protocol = json.loads((D/'protocol.json').read_text())
            next(p for p in protocol['phases'] if p['id'] == phase)[field] = value
            with self.assertRaises(ValueError):
                r.make_plan(protocol, json.loads((D/'orders.json').read_text()), phase, plan['expected_sha256'], plan['paths'])

    def test_source_receipt_and_hash_identity(self):
        plan = self.plan()
        receipt = dict(harness_commit=plan['harness_commit'], production_revisions=plan['production_revisions'],
                       source_artifacts_sha256={'/p/source.json': 'a'*64})
        r.validate_source_receipt(receipt, plan)
        for bad in [dict(receipt, harness_commit='stale'), dict(receipt, production_revisions={}),
                    dict(receipt, source_artifacts_sha256={}), dict(receipt, source_artifacts_sha256={'/not-pinned': 'b'*64})]:
            with self.assertRaises(r.EnvironmentFailure):
                r.validate_source_receipt(bad, plan)

    def test_telemetry_steal_missing_and_server_restart(self):
        def sample(n, steal=0, pid=44):
            return dict(monotonic_ns=n*1000000000, processes=[dict(pid=pid, comm='fdbserver')],
                        **{'/proc/stat': '\n'.join('cpu'+str(i)+' '+f'{n*10} 0 0 {n*90} 0 0 0 {steal}' for i in range(16))})
        plan = self.plan()
        r.telemetry_guard([sample(1), sample(2)], plan)
        for samples in [[sample(1)], [sample(1), sample(2, steal=2)],
                        [sample(1), sample(4)], [sample(1), sample(2, pid=45)]]:
            with self.assertRaises(r.EnvironmentFailure):
                r.telemetry_guard(samples, plan)

    def test_sql_requires_physical_prefix_and_adverse_audits_retained(self):
        j = self.plan('D1')['jobs'][0]
        with self.assertRaises(ValueError):
            parse(self.output(j).replace(' 20 prefix-B', ''), j)
        with tempfile.TemporaryDirectory() as folder:
            path = Path(folder)
            (path/'stdout.txt').write_text('FDB_BENCH_AUDIT {malformed}\n')
            r.retain_raw_audits(path)
            self.assertEqual(json.loads((path/'audits.raw.json').read_text()), [{'malformed_raw': '{malformed}'}])

    @unittest.skipUnless(os.environ.get('FDB_BENCH_DIAGNOSTICS_DIR'), 'FDB_BENCH_DIAGNOSTICS_DIR unset')
    def test_actual_runtime_diagnostic_parser_population(self):
        receipt = validate_actual_diagnostics(os.environ['FDB_BENCH_DIAGNOSTICS_DIR'])
        self.assertEqual(receipt['processes'], 69)
        self.assertEqual(receipt['result_rows'], 207)

    def test_diagnostic_population_validator(self):
        jobs = diagnostic_jobs()
        with tempfile.TemporaryDirectory() as folder:
            path = Path(folder)
            for name, job in jobs.items():
                (path/name).write_text(self.output(job))
                (path/(name[:-4]+'.command.json')).write_text(json.dumps(['-test.count=3', '-test.v']))
            r.dump(path/'complete.json', dict(diagnostic_only=True, processes=69, rows=207))
            receipt = validate_actual_diagnostics(path)
            self.assertEqual((receipt['processes'], receipt['result_rows']), (69, 207))
            (path/'B-00-final.log').unlink()
            with self.assertRaises(ValueError):
                validate_actual_diagnostics(path)

    def test_real_log_excerpts_parse_for_every_audit_kind(self):
        # Unmodified 1x diagnostic logs from a real campaign, one per audit kind/path.
        jobs = diagnostic_jobs()
        folder = D/'real-logs'
        logs = sorted(folder.glob('*.log'))
        self.assertEqual({jobs[p.name]['case']['audit'] for p in logs},
                         {'classic-write', 'none', 'fetch', 'sql-read', 'sql-write', 'planskip', 'sql-fetch'})
        for log in logs:
            command = json.loads((folder/(log.name[:-4]+'.command.json')).read_text())
            self.assertIn('-test.v', command)
            self.assertIn('-test.count=3', command)
            rows, audits = parse(log.read_text(), jobs[log.name])
            self.assertEqual(len(rows), 3)
            if jobs[log.name]['case']['audit'] in ('fetch', 'planskip', 'sql-fetch'):
                bad = '\n'.join(x for x in log.read_text().splitlines() if '"phase":"fixture"' not in x)
                with self.assertRaises(r.CorrectnessFailure):
                    parse(bad, jobs[log.name])

    def test_cluster_clear_guard(self):
        plan = self.plan()
        with mock.patch.dict(os.environ, FDB_BENCH_SERVER_ADDR='192.0.2.1:4500'):
            with mock.patch.object(Path, 'read_text', return_value='owned:key@8.8.8.8:4500'):
                with self.assertRaisesRegex(r.EnvironmentFailure, 'private owned'):
                    r.cluster_guard(plan)
            # A matching coordinator passes the address gate and reaches the engine gate.
            with mock.patch.object(Path, 'read_text', return_value='owned:key@192.0.2.1:4500'):
                with self.assertRaisesRegex(r.EnvironmentFailure, 'memory-engine'):
                    r.cluster_guard(plan)
        with mock.patch.dict(os.environ, {}, clear=True), mock.patch.object(Path, 'read_text', return_value='owned:key@192.0.2.1:4500'):
            with self.assertRaisesRegex(r.EnvironmentFailure, 'FDB_BENCH_SERVER_ADDR'):
                r.cluster_guard(plan)

    def test_runtime_cpu_sets_come_from_protocol(self):
        plan = self.plan()
        self.assertEqual([(s['name'], s['cpus']) for s in plan['cpu_sets']],
                         [('housekeeping', [0, 1]), ('server', list(range(2, 8))), ('client', list(range(8, 16)))])
        self.assertEqual(plan['env']['GOMAXPROCS'], '8')
        self.assertEqual(r.parse_cpus('0,2-3,7'), [0, 2, 3, 7])
        for bad in ('', '3-1', 'a-b', '1,1', '1-'):
            with self.assertRaises(ValueError):
                r.parse_cpus(bad)
        orders = json.loads((D/'orders.json').read_text())
        for field, value in [('client_cpus', '8-11'), ('server_cpus', '7-9'), ('GOMAXPROCS', 4), ('GOGC', 50)]:
            protocol = json.loads((D/'protocol.json').read_text())
            protocol['runtime'][field] = value
            with self.subTest(field=field), self.assertRaisesRegex(ValueError, 'CPU/GC'):
                r.make_plan(protocol, orders, 'B', plan['expected_sha256'], plan['paths'])
        protocol = json.loads((D/'protocol.json').read_text())
        protocol['runtime'].update(client_cpus='8-11', GOMAXPROCS=4)
        small = r.make_plan(protocol, orders, 'B', plan['expected_sha256'], plan['paths'])
        self.assertEqual(small['jobs'][0]['command'][:3], ['taskset', '--cpu-list', '8-11'])
        job = small['jobs'][0]
        text = self.output(job).replace(job['name']+'-8 ', job['name']+'-4 ')
        self.assertEqual(len(r.parse_output(text, job, gomaxprocs=small['env']['GOMAXPROCS'])[0]), 3)
        with self.assertRaises(r.CorrectnessFailure):
            r.parse_output(text, job, gomaxprocs=8)

    def test_harness_commit_from_protocol_or_flag(self):
        plan = self.plan()
        protocol = json.loads((D/'protocol.json').read_text())
        orders = json.loads((D/'orders.json').read_text())
        self.assertEqual(plan['harness_commit'], protocol['harness_revision'])
        self.assertEqual(r.make_plan(protocol, orders, 'B', plan['expected_sha256'], plan['paths'],
                                     harness_commit='f'*40)['harness_commit'], protocol['harness_revision'])
        del protocol['harness_revision']
        with self.assertRaisesRegex(ValueError, 'harness commit'):
            r.make_plan(protocol, orders, 'B', plan['expected_sha256'], plan['paths'])
        self.assertEqual(r.make_plan(protocol, orders, 'B', plan['expected_sha256'], plan['paths'],
                                     harness_commit='f'*40)['harness_commit'], 'f'*40)


class AnalysisTests(unittest.TestCase):
    def samples(self):
        plan = RunnerTests().plan()
        plan['cases'] = [plan['cases'][0]]
        plan['bootstrap_resamples'] = 100
        rows = [dict(phase='B', case=plan['cases'][0]['key'], block=b, position=order.index(arm), arm=arm,
                     repeat=i, N=1000, **{'ns/op': (1 if arm == 'final' else 2)*(b+1)+i*.01,
                                         'B/op': 0, 'allocs/op': 0, 'rows/op': None, 'prefix-B': None})
                for b, order in enumerate(plan['orders']) for arm in plan['phase']['arms'] for i in range(3)]
        return plan, rows

    def test_gm_bootstrap_deterministic_and_zero(self):
        result = a.paired_gm([.5]*6)
        self.assertAlmostEqual(result['gm'], .5)
        self.assertEqual(result['nominal_95_interval'], [.5, .5])
        self.assertEqual(result, a.paired_gm([.5]*6))
        for ratios in ([], [0, 1], [-1], [float('nan')]):
            with self.assertRaises(ValueError):
                a.paired_gm(ratios)
        p, rows = self.samples()
        report = a.summarize(p, rows, {})
        self.assertEqual(report['raw_samples'], 54)
        self.assertEqual(next(c for c in report['comparisons'] if c['metric'] == 'B/op')['status'], 'undefined-zero')
        self.assertIn('legacy contains parity differences', report['comparisons'][0]['label'])

    def test_population_duplicates_wrong_cases_order_and_missing(self):
        p, rows = self.samples()
        for bad in ([], rows[:-1], rows+rows[:1], [dict(s, case='unknown') for s in rows],
                    [dict(s, position=99) for s in rows]):
            with self.assertRaises(ValueError):
                a.summarize(p, bad, {})
        report = a.summarize(p, rows[:1], {p['cases'][0]['key']: {'reason': 'historical failed'}})
        self.assertEqual(report['cases'][p['cases'][0]['key']]['status'], 'N/A')
        self.assertEqual(report['comparisons'], [])

    def test_complete_raw_artifact_analysis_and_csv_tamper(self):
        fixture = RunnerTests()
        plan = fixture.plan()
        plan['cases'] = plan['cases'][:1]
        plan['jobs'] = [j for j in plan['jobs'] if j['case']['key'] == plan['cases'][0]['key']]
        plan['expected_sha256'][plan['paths']['analyzer']] = r.sha(a.__file__)
        plan['bootstrap_resamples'] = 100
        with tempfile.TemporaryDirectory() as folder:
            inp = Path(folder)/'input'
            inp.mkdir()
            samples = []
            for job in plan['jobs']:
                run = inp/job['id']
                run.mkdir()
                text = fixture.output(job)
                (run/'stdout.txt').write_text(text)
                (run/'stderr.txt').write_text('')
                rows, audits = parse(text, job)
                r.dump(run/'result.json', {'status': 'ok'})
                r.dump(run/'audits.json', audits)
                for row in rows:
                    samples.append(dict(phase='B', case=job['case']['key'], arm=job['arm'], block=job['block'],
                                        position=job['position'], repeat=row['repeat'], N=row['N'], **row['metrics']))
            r.dump(inp/'plan.json', plan)
            r.dump(inp/'controller.json', {'plan_sha256': r.sha(inp/'plan.json')})
            for name in ('pins.before.json', 'pins.after.json'):
                r.dump(inp/name, plan['expected_sha256'])
            r.dump(inp/'omitted.json', {})
            r.dump(inp/'complete.json', {'samples': len(samples), 'completed': len(plan['jobs']), 'pins_unchanged': True})
            r.write_metrics(inp/'metrics.csv', samples)
            report = a.analyze(inp, Path(folder)/'report')
            self.assertEqual(report['raw_samples'], 54)
            (inp/'metrics.csv').write_text((inp/'metrics.csv').read_text().replace('100.0', '101.0'))
            with self.assertRaises(ValueError):
                a.analyze(inp, Path(folder)/'bad-report')

    def test_physical_prefix_confound_is_reported_not_hidden(self):
        p, rows = self.samples()
        for s in rows:
            s['prefix-B'] = 10 if s['arm'] == 'final' else 11
        report = a.summarize(p, rows, {})
        self.assertTrue(report['cases'][p['cases'][0]['key']]['physical_prefix_confound'])
        self.assertTrue(report['comparisons'][0]['physical_prefix_confound'])


if __name__ == '__main__':
    unittest.main()
