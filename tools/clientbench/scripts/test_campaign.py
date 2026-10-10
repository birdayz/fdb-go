#!/usr/bin/env python3
import argparse
import copy
import csv
import json
import math
import os
from pathlib import Path
import random
import subprocess
import sys
import tempfile
import time
import unittest

import analyze
import r3_plan
import run


class CampaignTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        for name in ['go', 'cgo', 'cluster', 'native', 'server', 'harness']:
            (self.root / name).write_text(name)
        self.args = argparse.Namespace(go_binary=self.root / 'go', cgo_binary=self.root / 'cgo',
                                      cluster_file=self.root / 'cluster', native_library=self.root / 'native',
                                      server_binary=self.root / 'server', source_revision='a' * 40,
                                      harness_id=r3_plan.digest(self.root / 'harness'), campaign_id='c' * 29, pin=[self.root / 'harness'],
                                      housekeeping_cpus='0-1', server_cpus='2-7', client_cpus='8-15')
        self.plan = r3_plan.make_plan(self.args)

    def test_r3_recipe_and_counterbalance(self):
        self.assertEqual(self.plan['workloads'], [dict(name='batchset50', iterations=3000, warmup=200, workers=1)])
        self.assertEqual(self.plan['campaign_wall_cap_seconds'], 180)
        self.assertEqual(self.plan['environment']['GOMAXPROCS'], '8')
        schedule = run.make_schedule(self.plan, self.root / 'out')
        self.assertEqual(len(schedule), 12)
        self.assertEqual(schedule, run.make_schedule(self.plan, self.root / 'out'))
        orders = [tuple(r['arm'] for r in schedule if r['block'] == b) for b in range(6)]
        self.assertEqual(orders.count(('go', 'cgo')), 3)
        self.assertEqual(orders.count(('cgo', 'go')), 3)
        prefixes = [r['command'][r['command'].index('--prefix') + 1] for r in schedule]
        self.assertEqual(len(set(prefixes)), 12)
        self.assertEqual({len(p.encode()) for p in prefixes}, {68})
        self.assertEqual(self.plan['expected_sha256'][str(self.root / 'go')], r3_plan.digest(self.root / 'go'))

    def test_bad_schedule_and_cpu_sets(self):
        bad_plans = []
        p = copy.deepcopy(self.plan)
        p['blocks'] = 3
        bad_plans.append(p)
        p = copy.deepcopy(self.plan)
        p['campaign_wall_cap_seconds'] = 181
        bad_plans.append(p)
        p = copy.deepcopy(self.plan)
        p['arms'][1]['name'] = 'native'
        bad_plans.append(p)
        p = copy.deepcopy(self.plan)
        p['arms'][0]['name'] = '../go'
        bad_plans.append(p)
        p = copy.deepcopy(self.plan)
        p['arms'][0]['command'][-1] = p['arms'][1]['command'][-1]
        bad_plans.append(p)
        p = copy.deepcopy(self.plan)
        p['arms'][0]['command'][-1] += 'x'
        bad_plans.append(p)
        for p in bad_plans:
            with self.subTest(plan=p), self.assertRaises(ValueError):
                run.make_schedule(p, self.root / 'out')
        for field, value in [('client_cpus', '0,8-15'), ('server_cpus', '7-2'), ('harness_id', 'mutable')]:
            args = copy.copy(self.args)
            setattr(args, field, value)
            with self.subTest(field=field), self.assertRaises(ValueError):
                r3_plan.make_plan(args)

    def test_required_pins_and_identical_arm_options(self):
        for kind in ['missing_pins', 'missing_native', 'missing_binary', 'wrong_harness', 'affinity', 'timeout', 'retries']:
            with self.subTest(kind=kind):
                plan = copy.deepcopy(self.plan)
                if kind == 'missing_pins':
                    del plan['expected_sha256']
                elif kind == 'missing_native':
                    del plan['native_library_sha256']
                elif kind == 'missing_binary':
                    del plan['expected_sha256'][plan['arms'][0]['binary']]
                elif kind == 'wrong_harness':
                    plan['harness_id'] = '0' * 64
                else:
                    flag, value = {'affinity': ('--cpu-list', '8-9'), 'timeout': ('--transaction-timeout-ms', '1'),
                                   'retries': ('--retry-limit', '0')}[kind]
                    command = plan['arms'][0]['command']
                    command[command.index(flag) + 1] = value
                with self.assertRaises(ValueError):
                    run.make_schedule(plan, self.root / 'out')
        args = copy.copy(self.args)
        args.harness_id = '0' * 64
        with self.assertRaises(ValueError):
            r3_plan.make_plan(args)
        plan = copy.deepcopy(self.plan)
        plan['arms'][0]['source_revision'] = 'd' * 40
        command = plan['arms'][0]['command']
        command[command.index('--source-revision') + 1] = 'd' * 40
        self.assertEqual(len(run.make_schedule(plan, self.root / 'out')), 12)

    def test_telemetry_captures_descendants_and_partial_process_data(self):
        code = 'import os,time;print(os.getpid(),flush=True);time.sleep(30)'
        process = subprocess.Popen([sys.executable, '-c', code], stdout=subprocess.PIPE, text=True)
        try:
            pid = int(process.stdout.readline())
            records = run.telemetry(os.getpid())['processes']
            child = next((p for p in records if p['pid'] == pid), None)
            self.assertIsNotNone(child, 'the benchmark descendant is not the GNU time PID')
            self.assertIn('stat', child)
            self.assertIn('status', child)
        finally:
            process.terminate()
            process.wait(timeout=5)
            process.stdout.close()
        proc = self.root / '12345'
        proc.mkdir()
        (proc / 'stat').write_text('world-readable stat')
        (proc / 'status').write_text('world-readable status')
        (proc / 'io').mkdir()
        record = run.process_record(proc, 'fdbserver')
        self.assertEqual(record['stat'], 'world-readable stat')
        self.assertEqual(record['status'], 'world-readable status')
        self.assertIn('io', record['errors'])

    def test_schedule_only_refuses_overwrite(self):
        plan_path = self.root / 'plan.json'
        plan_path.write_text(json.dumps(self.plan))
        command = [sys.executable, '-O', '-B', str(Path(run.__file__)), '--plan', str(plan_path),
                   '--output', str(self.root / 'schedule'), '--schedule-only']
        result = subprocess.run(command, capture_output=True, text=True, timeout=10)
        self.assertEqual(result.returncode, 0, result.stderr)
        result = subprocess.run(command, capture_output=True, text=True, timeout=10)
        self.assertNotEqual(result.returncode, 0)
        self.plan['blocks'] = 3
        plan_path.write_text(json.dumps(self.plan))
        command[-2] = str(self.root / 'invalid')
        result = subprocess.run(command, capture_output=True, text=True, timeout=10)
        self.assertNotEqual(result.returncode, 0, 'optimized Python disabled the validation')

    def test_changed_binary_is_rejected_before_execution(self):
        out = self.root / 'run'
        out.mkdir()
        (self.root / 'go').write_text('changed since registration')
        with self.assertRaisesRegex(ValueError, 'preregistered'):
            run.execute(self.plan, run.make_schedule(self.plan, out), self.root / 'plan.json', out, time.monotonic() + 180)
        self.assertFalse((out / 'complete.json').exists())

    def test_quantile_and_block_bootstrap(self):
        self.assertEqual(analyze.quantile(list(range(1, 2001)), .99), 1980)
        self.assertEqual(analyze.quantile(list(range(1, 2001)), .999), 1998)
        pairs = [(2, 1), (4, 2), (6, 3), (4, 4), (5, 5), (6, 6)]
        result = analyze.bootstrap_log_ratio(pairs, random.Random(7), 200)
        self.assertAlmostEqual(result['ratio'], math.sqrt(2))
        self.assertLess(result['ci95_low'], result['ratio'])
        self.assertGreater(result['ci95_high'], result['ratio'])
        for values, p in [([], .99), ([1], 0), ([math.nan], .5)]:
            with self.subTest(values=values, p=p), self.assertRaises(ValueError):
                analyze.quantile(values, p)
        for pairs in [[(1, 2)], [(0, 1), (1, 2)], [(math.inf, 1), (1, 2)]]:
            with self.subTest(pairs=pairs), self.assertRaises(ValueError):
                analyze.bootstrap_log_ratio(pairs, random.Random(7), 200)

    def fixture(self, name='fixture'):
        out = self.root / name
        out.mkdir()
        plan = copy.deepcopy(self.plan)
        plan['workloads'][0].update(iterations=5, warmup=2)
        schedule = run.make_schedule(plan, out)
        pins = plan['expected_sha256']
        for filename, value in [('plan.json', plan), ('schedule.json', schedule), ('binaries.json', pins),
                                ('complete.json', dict(scheduled=12, completed=12, binaries=pins))]:
            (out / filename).write_text(json.dumps(value))
        for entry in schedule:
            folder = out / entry['run_id']
            folder.mkdir()
            native = entry['arm'] == 'cgo'
            base = 10 * (entry['block'] + 1)
            latency_base = base * (2 if not native and entry['block'] < 3 else 1)
            cfg = dict(workload='batchset50', iterations=5, warmup=2, workers=1, block=entry['block'],
                       run_id=entry['run_id'], profile='none', allow_writes=True, transaction_timeout_ms=30000,
                       retry_limit=10, source_revision=plan['source_commit'], harness_id=plan['harness_id'],
                       prefix=entry['command'][-1])
            binary = str(self.root / ('cgo' if native else 'go'))
            backend = dict(backend=entry['arm'], native_library=native, executable=binary,
                           executable_sha256=pins[binary], api_version=730,
                           loaded_fdb_libraries_sha256={str(self.root / 'native'): plan['native_library_sha256']} if native else {})
            manifest = dict(config=cfg, backend=backend, go_version='go1.26.9', gomaxprocs=8)
            before = dict(process_user_cpu_us=0, process_system_cpu_us=0, go_total_alloc_bytes=0,
                          go_mallocs=0, go_gc_cycles=0, go_gc_pause_ns=0, process_high_water_rss_kb=1)
            after = dict(before, process_user_cpu_us=base * (40 if native else 30),
                         process_system_cpu_us=base * (20 if native else 15), go_total_alloc_bytes=1000,
                         go_mallocs=20, go_gc_cycles=1, go_gc_pause_ns=100, process_high_water_rss_kb=100)
            counters = dict(observation='unobserved') if native else dict(observation='observed', values=dict(
                grv_cache_hits=0, read_versions_completed=5, grv_successful_batch_replies=5))
            summary = dict(config=cfg, backend=backend, success=True, persisted_validation='ok',
                           requested=5, completed=5, errors=0, warmup_completed=2, warmup_errors=0,
                           measured_wall_ns=latency_base * 20000, stats_before=before, stats_after=after,
                           client_counters_delta=counters)
            for filename, value in [('manifest.json', manifest), ('summary.json', summary), ('result.json', dict(returncode=0))]:
                (folder / filename).write_text(json.dumps(value))
            for filename, count, phase in [('samples.csv', 5, 'measure'), ('warmup.csv', 2, 'warmup')]:
                with (folder / filename).open('w', newline='') as f:
                    writer = csv.writer(f)
                    writer.writerow(['phase', 'index', 'worker', 'start_offset_ns', 'latency_ns', 'attempts', 'error', 'check_status'])
                    for i in range(count):
                        writer.writerow([phase, i, 0, i * latency_base * 5000, (i+1) * latency_base * 1000, 1, '', 'ok'])
        return out, schedule

    def analyze(self, folder):
        return subprocess.run([sys.executable, '-O', '-B', str(Path(analyze.__file__)), str(folder), '--resamples', '200', '--seed', '7'],
                              capture_output=True, text=True, timeout=30)

    def test_absolute_and_paired_tables_from_raw(self):
        out, _ = self.fixture()
        result = self.analyze(out)
        self.assertEqual(result.returncode, 0, result.stderr)
        report = json.loads((out / 'analysis.json').read_text())
        ratios = {r['metric']: r for r in report['comparisons']}
        self.assertAlmostEqual(ratios['mean_ns']['ratio'], math.sqrt(2))
        self.assertAlmostEqual(ratios['process_cpu_ns_per_op']['ratio'], .75)
        self.assertEqual(ratios['process_cpu_ns_per_op']['pairs_below_one'], 6)
        self.assertEqual(ratios['process_cpu_ns_per_op']['numerator_go_gc_cycles'], 6)
        self.assertEqual(ratios['process_cpu_ns_per_op']['denominator_go_gc_cycles'], 6)
        self.assertIn('not complete-lifecycle', report['window_cpu_scope'])
        with (out / 'paired.csv').open() as f:
            self.assertEqual(next(csv.DictReader(f))['numerator_go_gc_cycles'], '6')
        self.assertIn('causal-read-risky', (out / 'TABLES.md').read_text())
        with (out / 'absolute.csv').open() as f:
            absolute = {r['arm']: r for r in csv.DictReader(f)}
        self.assertEqual(float(absolute['go']['mean_ns']), 135000)
        self.assertEqual(float(absolute['cgo']['mean_ns']), 105000)
        self.assertNotAlmostEqual(135000 / 105000, ratios['mean_ns']['ratio'])
        self.assertEqual(len(report['per_run']), 12)
        self.assertIn('getrusage', (out / 'TABLES.md').read_text())
        self.assertIn('native C++ heap', (out / 'TABLES.md').read_text())

    def test_aborted_campaign_never_reports_success(self):
        out, _ = self.fixture()
        (out / 'ABORTED.json').write_text(json.dumps(dict(error='received signal after completion write')))
        result = self.analyze(out)
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse((out / 'analysis.json').exists())

    def test_archived_campaign_can_move_without_rewriting_schedule(self):
        out, _ = self.fixture()
        moved = out.rename(self.root / 'archive')
        result = self.analyze(moved)
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_schedule_must_match_registered_commands_and_ids(self):
        for field in ['prefix', 'run_id', 'transaction_timeout_ms', 'affinity', 'duplicate_output']:
            with self.subTest(field=field):
                out, schedule = self.fixture(field)
                entry = schedule[0]
                folder = out / entry['run_id']
                command = entry['command']
                if field == 'prefix':
                    value = 'different/' + command[-1]
                    command[-1] = value
                elif field == 'run_id':
                    value = entry['run_id'] + '-different'
                    entry['run_id'] = value
                    folder = folder.rename(out / value)
                    command[command.index('--run-id') + 1] = value
                    command[command.index('--output') + 1] = str(folder)
                elif field == 'transaction_timeout_ms':
                    value = 1
                    command[command.index('--transaction-timeout-ms') + 1] = str(value)
                elif field == 'affinity':
                    command[command.index('--cpu-list') + 1] = '0'
                else:
                    command.extend(['--output', str(folder)])
                if field in ['prefix', 'run_id', 'transaction_timeout_ms']:
                    for filename in ['manifest.json', 'summary.json']:
                        path = folder / filename
                        data = json.loads(path.read_text())
                        data['config'][field] = value
                        path.write_text(json.dumps(data))
                (out / 'schedule.json').write_text(json.dumps(schedule))
                result = self.analyze(out)
                self.assertNotEqual(result.returncode, 0, field)
                self.assertIn('schedule differs from counterbalanced plan', result.stderr)
                self.assertFalse((out / 'analysis.json').exists())

    def test_corrupt_campaigns_never_report_success(self):
        def mutate_json(path, fn):
            value = json.loads(path.read_text())
            fn(value)
            path.write_text(json.dumps(value))
        cases = [
            ('missing_run', 'complete.json', lambda x: x.update(completed=11)),
            ('empty_schedule', 'schedule.json', lambda x: x.clear()),
            ('bad_exit', 'result.json', lambda x: x.update(returncode=1)),
            ('wrong_source', 'manifest.json', lambda x: x['config'].update(source_revision='wrong')),
            ('profiling', 'manifest.json', lambda x: x['config'].update(profile='cpu')),
            ('wrong_binary', 'manifest.json', lambda x: x['backend'].update(executable_sha256='0' * 64)),
            ('synthetic_native_counters', 'summary.json', lambda x: x['client_counters_delta'].update(observation='observed')),
            ('missing_stats', 'summary.json', lambda x: x.pop('stats_after')),
            ('negative_cpu', 'summary.json', lambda x: x['stats_after'].update(process_user_cpu_us=-1)),
        ]
        for name, filename, mutate in cases:
            with self.subTest(name=name):
                out, schedule = self.fixture(name)
                native = next(r for r in schedule if r['arm'] == 'cgo')
                folder = out if filename in ['complete.json', 'schedule.json'] else out / native['run_id']
                mutate_json(folder / filename, mutate)
                result = self.analyze(out)
                self.assertNotEqual(result.returncode, 0, name)
                self.assertFalse((out / 'analysis.json').exists(), name)
        for name, filename, column, value in [('duplicate_index', 'samples.csv', 1, '1'),
                                               ('bad_check', 'samples.csv', 7, 'error'),
                                               ('bad_warmup', 'warmup.csv', 6, 'failed')]:
            with self.subTest(name=name):
                out, schedule = self.fixture(name)
                path = out / schedule[0]['run_id'] / filename
                with path.open() as f:
                    rows = list(csv.reader(f))
                rows[1][column] = value
                with path.open('w', newline='') as f:
                    csv.writer(f).writerows(rows)
                self.assertNotEqual(self.analyze(out).returncode, 0)
                self.assertFalse((out / 'analysis.json').exists())

    def test_cpu_accounting_and_owned_process_group_cleanup(self):
        usage = run.cpu_usage([10, 0, 10, 70, 0, 0, 0, 10])
        self.assertEqual(usage['busy_fraction'], .2)
        self.assertEqual(usage['steal_fraction'], .1)
        with self.assertRaises(ValueError):
            run.cpu_usage([0] * 8)
        code = 'import subprocess,sys; p=subprocess.Popen([sys.executable,"-c","import time; time.sleep(30)"]); print(p.pid,flush=True)'
        process = subprocess.Popen([sys.executable, '-c', code], start_new_session=True, stdout=subprocess.PIPE, text=True)
        try:
            child = int(process.stdout.readline())
            process.wait(timeout=5)
            run.stop_process_group(process)
            deadline = time.monotonic() + 5
            while time.monotonic() < deadline:
                status = Path(f'/proc/{child}/stat')
                if not status.exists() or status.read_text().split(') ', 1)[1].startswith('Z'):
                    break
                time.sleep(.01)
            else:
                self.fail('grandchild survived cleanup after its wrapper exited')
        finally:
            run.stop_process_group(process)
            process.stdout.close()


if __name__ == '__main__':
    unittest.main(verbosity=2)
