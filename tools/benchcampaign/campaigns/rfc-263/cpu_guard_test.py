#!/usr/bin/env python3
"""Fixture-only guards. No builds, benchmark processes, cluster or Docker access."""
from copy import deepcopy
import csv
import datetime as dt
import hashlib
import json
import math
import os
from pathlib import Path
import subprocess
import sys
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import Mock, patch

sys.dont_write_bytecode = True
import cpu_collect as c
import cpu_analyze as a
import lifecycle_helper as lifecycle

IDS = c.identities('1' * 40, '2' * 64)


def snapshot(user, system, mallocs=5, allocated=50, gc=1):
    return dict(process_user_cpu_us=user, process_system_cpu_us=system, go_mallocs=mallocs,
                go_total_alloc_bytes=allocated, go_gc_cycles=gc, go_gc_pause_ns=0, process_high_water_rss_kb=100)


def fixture(row, out, frozen, ratio=1):
    manifest = dict(config=c.expected_config(row, out, frozen), go_version='go1.26.9', gomaxprocs=4,
                    environment=dict(c.ENV), backend=dict(executable_sha256=row['binary_sha256'], backend='go', native_library=False))
    n = row['iterations']
    # Endpoint literal controls: window=(6N+4N)/N=10; lifecycle=(12N+8N)/N=20.
    # Identity counters are nonzero to catch accidental absolute-E/N or wrong subtraction.
    summary = dict(config=deepcopy(manifest['config']), backend=deepcopy(manifest['backend']), requested=n, completed=n,
        errors=0, retries=0, warmup_requested=2000, warmup_completed=2000, warmup_errors=0, warmup_retries=0,
        measured_wall_ns=100000, success=True, failure='', profile=row.get('profile', 'none'), persisted_validation='ok',
        stats_after_identity=snapshot(100, 100, 0, 0, 0), stats_before=snapshot(200, 200, 10, 100, 1),
        stats_after=snapshot(200 + int(6 * n * ratio), 200 + int(4 * n * ratio), 10 + 2 * n, 100 + 20 * n, 2),
        lifecycle=dict(complete=True, stats_after_measurement_drain=snapshot(200 + int(7 * n * ratio), 200 + int(5 * n * ratio), 10 + 2 * n, 100 + 20 * n, 3),
                       stats_after_shutdown=snapshot(100 + int(12 * n * ratio), 100 + int(8 * n * ratio), 10 + 2 * n, 100 + 20 * n, 4)))
    return manifest, summary


def csv_fixture(path, rows, fields=None):
    fields = fields or ['index', 'worker', 'phase', 'error', 'check_status', 'latency_ns', 'attempts']
    with path.open('w', newline='') as stream:
        writer = csv.DictWriter(stream, fields)
        writer.writeheader()
        writer.writerows(rows)


def csv_rows(n=8, workers=8, phase='measure'):
    return [dict(index=i, worker=i % workers, phase=phase, error='', check_status='ok', latency_ns=100, attempts=1) for i in range(n)]


class Schedule(unittest.TestCase):
    def test_literal_census_seed_rotation_orders(self):
        rows = c.schedule(IDS)
        self.assertEqual(rows, c.schedule(IDS))
        self.assertEqual(len(rows), 256)
        self.assertEqual(sum(r['iterations'] for r in rows), 66560000)
        self.assertEqual(sum(r['warmup'] for r in rows), 512000)
        self.assertEqual(len({r['pair'] for r in rows}), 128)
        self.assertEqual(len({r['cell'] for r in rows}), 8)
        self.assertEqual(len([r for r in rows if r['contrast'] == 'AB']), 192)
        self.assertEqual(len([r for r in rows if r['contrast'] == 'baselineAA']), 32)
        self.assertEqual(len([r for r in rows if r['contrast'] == 'productionAA']), 32)
        for epoch in (1, 2):
            for cell in a.CELL_NAMES:
                rr = [r for r in rows if r['epoch'] == epoch and r['cell'] == cell and r['member'] == 0]
                self.assertEqual(len(rr), 8)
                self.assertEqual(sum(r['contrast'] == 'AB' and r['order'] == 0 for r in rr), 3)
                self.assertEqual(sum(r['contrast'] == 'AB' and r['order'] == 1 for r in rr), 3)
        self.assertEqual(rows[0]['id'], 'e1-r000')
        self.assertEqual(rows[-1]['id'], 'e2-r127')

    def test_reject_every_schedule_dimension(self):
        rows = c.schedule(IDS)
        for bad in ([], rows[:-1], rows[:-1] + [rows[0]]):
            with self.assertRaises(ValueError): c.validate_schedule(bad, IDS)
        changes = dict(id='bad', prefix='x', epoch=3, visit=99, position=99, member=9, workload='unknown', workers=7,
                       iterations=1, warmup=0, pair='bad', contrast='unknown', order=3, alias='X', binary='baseline',
                       source='wrong', binary_sha256='wrong', cell='wrong', block=100, profile='trace')
        target = next(i for i, r in enumerate(rows) if r['binary'] == 'production')
        for key, value in changes.items():
            bad = deepcopy(rows)
            bad[target][key] = value
            with self.subTest(key=key), self.assertRaises((ValueError, KeyError)): c.validate_schedule(bad, IDS)
        bad = deepcopy(rows)
        bad[0]['prefix'] = bad[1]['prefix']
        with self.assertRaises(ValueError): c.validate_schedule(bad, IDS)
        bad = deepcopy(rows)
        # Preserves pair identities but breaks whole-visit rotation.
        bad[16:20] = bad[18:20] + bad[16:18]
        with self.assertRaises(ValueError): c.validate_schedule(bad, IDS)
        bad_ids = deepcopy(IDS)
        bad_ids['baseline']['source'] = 'wrong'
        with self.assertRaises(ValueError): c.validate_schedule(rows, bad_ids)
        for tree, binary in (('short', '2' * 64), ('1' * 40, 'short')):
            with self.assertRaises(ValueError): c.identities(tree, binary)

    def test_batch_namespace_only_changes_prefixes_and_keeps_all_272_disjoint(self):
        timing, diagnostics = c.schedule(IDS), c.diagnostic_schedule(IDS)
        namespace = '0' * 32
        fresh = c.schedule(IDS, namespace)
        fresh_diag = c.diagnostic_schedule(IDS, namespace)
        self.assertEqual(fresh, c.schedule(IDS, namespace))
        self.assertEqual(fresh_diag, c.diagnostic_schedule(IDS, namespace))
        for old, new in zip(timing + diagnostics, fresh + fresh_diag):
            self.assertEqual({k: v for k, v in old.items() if k != 'prefix'},
                             {k: v for k, v in new.items() if k != 'prefix'})
            self.assertEqual(len(new['prefix'].encode()), 68)
            self.assertEqual(len(new['prefix'].removeprefix('clientbench/rfc263/').removesuffix('/')), 48)
        # Independent printf/sha256sum control for namespace + NUL + original prefix.
        self.assertEqual(fresh[0]['prefix'], 'clientbench/rfc263/a2c4eac21087dab2d39052456b6b9beb5cdb709a6a347d38/')
        c.disjoint_prefix_population(fresh, fresh_diag, timing, diagnostics)
        c.disjoint_prefix_population(c.schedule(IDS, '1' * 32), c.diagnostic_schedule(IDS, '1' * 32), fresh, fresh_diag)
        for bad in ('short', 'A' * 32, None, 0):
            with self.subTest(namespace=bad), self.assertRaises(ValueError): c.schedule(IDS, bad)
        with self.assertRaises(ValueError): c.disjoint_prefix_population([], fresh_diag, timing, diagnostics)
        with self.assertRaises(ValueError): c.disjoint_prefix_population(fresh, fresh_diag, [], diagnostics)
        prior = deepcopy(timing)
        prior[0]['prefix'] = fresh[0]['prefix']
        with self.assertRaises(ValueError): c.disjoint_prefix_population(fresh, fresh_diag, prior, diagnostics)
        prior_diag = deepcopy(diagnostics)
        prior_diag[0]['prefix'] = fresh[0]['prefix']
        with self.assertRaises(ValueError): c.disjoint_prefix_population(fresh, fresh_diag, timing, prior_diag)

    def test_prior_prefix_schedules_require_external_seal_and_both_schedule_pins(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            timing, diagnostics = c.schedule(IDS), c.diagnostic_schedule(IDS)
            c.save(root / 'schedule.json', timing)
            c.save(root / 'schedule-diagnostics.json', diagnostics)
            pins = {str((root / name).resolve()): c.sha(root / name) for name in ('schedule.json', 'schedule-diagnostics.json')}
            c.save(root / 'frozen.json', {'hashes': pins})
            seal = c.sha(root / 'frozen.json')
            self.assertEqual(c.prior_prefix_schedules(root, seal), [timing, diagnostics])
            copies = root / 'copies'
            copies.mkdir()
            for name in ('frozen.json', 'schedule.json', 'schedule-diagnostics.json'):
                (copies / ('prior-' + name)).write_bytes((root / name).read_bytes())
            self.assertEqual(c.prior_prefix_schedules(copies, seal, root), [timing, diagnostics])
            for name in ('frozen.json', 'schedule.json', 'schedule-diagnostics.json'):
                copy = copies / ('prior-' + name)
                original = copy.read_bytes()
                c.save(copy, {})
                with self.subTest(copy=name), self.assertRaises(ValueError): c.prior_prefix_schedules(copies, seal, root)
                copy.write_bytes(original)
            with self.assertRaises(ValueError): c.prior_prefix_schedules(root, '0' * 64)
            for name, rows in (('schedule.json', timing), ('schedule-diagnostics.json', diagnostics)):
                c.save(root / name, [])
                with self.subTest(path=name), self.assertRaises(ValueError): c.prior_prefix_schedules(root, seal)
                c.save(root / name, rows)
            c.save(root / 'frozen.json', {'hashes': {}})
            with self.assertRaises(ValueError): c.prior_prefix_schedules(root, c.sha(root / 'frozen.json'))

    def test_queuewait_analyzer_uses_namespace_for_both_schedules(self):
        queuewait = c.load_module(Path(__file__).with_name('rev6-cpu-queuewait.py'), 'cpu_prefix_queuewait_fixture')
        class BeforeProfiles(Exception):
            pass
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            pprof = root / 'pprof'
            pprof.write_bytes(b'not an executable; profiles must not be reached')
            collector = Path(c.__file__).resolve()
            analyzer = Path(queuewait.__file__).resolve()
            frozen = dict(seed=c.SEED, identities=IDS, prefix_namespace='0' * 32, helper_hashes={},
                          pprof_sha256=c.sha(pprof), hashes={str(p): c.sha(p) for p in (collector, analyzer, pprof)})
            c.save(root / 'frozen.json', frozen)
            c.save(root / 'schedule.json', c.schedule(IDS, frozen['prefix_namespace']))
            c.save(root / 'schedule-diagnostics.json', c.diagnostic_schedule(IDS, frozen['prefix_namespace']))
            args = SimpleNamespace(evidence=root, frozen_sha256=c.sha(root / 'frozen.json'), collector_helper=collector,
                                   pprof_binary=pprof, pprof_sha256=c.sha(pprof), seal1='1' * 64, seal2='2' * 64, seal='3' * 64)
            with patch.object(queuewait, 'load_module', return_value=c), patch.object(c, 'verify_inputs'), \
                 patch.object(c, 'helper_chain'), patch.object(queuewait, 'checked_population', return_value=[]) as populations, \
                 patch.object(queuewait, 'require_posttiming', side_effect=BeforeProfiles):
                with self.assertRaises(BeforeProfiles): queuewait.analyze(args)
            self.assertEqual(populations.call_count, 3)

    def test_separate_diagnostics_literal_population_and_profiles(self):
        rows = c.diagnostic_schedule(IDS)
        self.assertEqual(rows, c.diagnostic_schedule(IDS))
        self.assertEqual(len(rows), 16)
        self.assertEqual(len({r['id'] for r in rows}), 16)
        self.assertEqual(len({r['prefix'] for r in rows}), 16)
        self.assertEqual(sum(r['iterations'] for r in rows), 8000000)
        self.assertEqual(sum(r['warmup'] for r in rows), 32000)
        self.assertEqual({(r['workload'], r['binary'], r['profile']) for r in rows},
                         {(w, role, profile) for w in ('grv', 'get100', 'set100', 'range100') for role in IDS for profile in ('mutex', 'block')})
        for row in rows:
            self.assertEqual(row['workers'], 32)
            self.assertEqual(row['source'], IDS[row['binary']]['source'])
            manifest, summary = fixture(row, Path('/fixture'), {'cluster': 'fixture.cluster'})
            c.check_manifest(manifest, row, Path('/fixture'), {'cluster': 'fixture.cluster'})
            c.check_summary(summary, row, manifest)
            self.assertEqual(manifest['config']['profile'], row['profile'])
        self.assertEqual(c.run_name(SimpleNamespace(population='diagnostics', epoch=0)), 'diagnostics')
        timing = c.schedule(IDS)
        c.validate_prefix_population(timing, rows)
        collision = deepcopy(rows)
        collision[0]['prefix'] = timing[0]['prefix']
        with self.assertRaises(ValueError): c.validate_prefix_population(timing, collision)
        with self.assertRaises(ValueError): c.validate_prefix_population([], rows)
        with self.assertRaises(ValueError): c.validate_prefix_population(timing, [])


class RawGuards(unittest.TestCase):
    def test_csv_both_workers_and_phases_and_errors(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / 'x.csv'
            for workers in (8, 32):
                for phase in ('measure', 'warmup'):
                    good = csv_rows(workers, workers, phase)
                    csv_fixture(path, good)
                    c.csv_integrity(path, workers, workers, phase)
            good = csv_rows()
            for n, workers, phase in ((0, 8, 'measure'), (-1, 8, 'measure'), (True, 8, 'measure'),
                                      (8, 0, 'measure'), (8, 9, 'measure'), (8, 8, 'invalid')):
                with self.assertRaises(ValueError): c.csv_integrity(path, n, workers, phase)
            malformed = [[], good[:-1], good + [good[0]], [good[0]] * 8]
            for key, value in (('index', -1), ('index', 8), ('worker', 7), ('phase', 'warmup'), ('error', 'failed'),
                               ('check_status', 'bad'), ('latency_ns', 0), ('attempts', 0), ('index', 'nan')):
                bad = deepcopy(good)
                bad[0][key] = value
                malformed.append(bad)
            for bad in malformed:
                csv_fixture(path, bad)
                with self.assertRaises(ValueError): c.csv_integrity(path, 8, 8, 'measure')
            path.write_text('index,index\n0,0\n')
            with self.assertRaises(ValueError): c.csv_integrity(path, 8, 8, 'measure')
            csv_fixture(path, good)
            with path.open('a') as stream: stream.write('0\n')
            with self.assertRaises(ValueError): c.csv_integrity(path, 8, 8, 'measure')
            csv_fixture(path, good)
            with path.open('a') as stream: stream.write('0,0,measure,,ok,100,1,extra\n')
            with self.assertRaises(ValueError): c.csv_integrity(path, 8, 8, 'measure')

    def test_manifest_each_configuration_and_identity_field(self):
        frozen = {'cluster': 'fixture.cluster'}
        row, out = c.schedule(IDS)[0], Path('/not-a-run')
        manifest, summary = fixture(row, out, frozen)
        c.check_manifest(manifest, row, out, frozen)
        c.check_summary(summary, row, manifest)
        for key in manifest['config']:
            bad = deepcopy(manifest)
            bad['config'][key] = 'wrong'
            with self.subTest(key=key), self.assertRaises(ValueError): c.check_manifest(bad, row, out, frozen)
        for key, value in (('go_version', 'wrong'), ('gomaxprocs', 1), ('environment', {})):
            bad = deepcopy(manifest)
            bad[key] = value
            with self.assertRaises(ValueError): c.check_manifest(bad, row, out, frozen)
        for key, value in (('executable_sha256', 'wrong'), ('backend', 'cgo'), ('native_library', True)):
            bad = deepcopy(manifest)
            bad['backend'][key] = value
            with self.assertRaises(ValueError): c.check_manifest(bad, row, out, frozen)

    def test_summary_population_health_and_warmup(self):
        row = c.schedule(IDS)[0]
        manifest, summary = fixture(row, Path('/not-a-run'), {'cluster': 'fixture.cluster'})
        changes = dict(config={}, backend={}, requested=0, completed=0, errors=1, warmup_requested=0, warmup_completed=0,
                       warmup_errors=1, success=False, failure='failed', profile='trace', persisted_validation='bad',
                       measured_wall_ns=0, retries=-1, warmup_retries=True)
        for key, value in changes.items():
            bad = deepcopy(summary)
            bad[key] = value
            with self.subTest(key=key), self.assertRaises(ValueError): c.check_summary(bad, row, manifest)

    def test_exact_dual_endpoints_and_zero_allocations(self):
        row = c.schedule(IDS)[0]
        _, summary = fixture(row, Path('/x'), {'cluster': 'fixture.cluster'})
        result = c.endpoint(summary)
        self.assertEqual(result, dict(window_cpu_us_per_tx=10, lifecycle_cpu_us_per_tx=20,
            allocations_per_tx=2, allocated_bytes_per_tx=20, gc_cycles=1))
        summary['stats_after']['go_mallocs'] = summary['stats_before']['go_mallocs']
        summary['stats_after']['go_total_alloc_bytes'] = summary['stats_before']['go_total_alloc_bytes']
        self.assertEqual(c.endpoint(summary)['allocations_per_tx'], 0)
        self.assertEqual(c.endpoint(summary)['allocated_bytes_per_tx'], 0)
        for n in (0, -1, True):
            bad = deepcopy(summary)
            bad['completed'] = n
            with self.assertRaises(ValueError): c.endpoint(bad)

    def test_both_field_paths_and_monotonic_each_snapshot_counter(self):
        _, good = fixture(c.schedule(IDS)[0], Path('/x'), {'cluster': 'fixture.cluster'})
        for path in ('stats_before', 'stats_after', 'stats_after_identity'):
            bad = deepcopy(good)
            del bad[path]
            with self.assertRaises(KeyError): c.endpoint(bad)
        for path in ('stats_after_shutdown', 'stats_after_measurement_drain', 'complete'):
            bad = deepcopy(good)
            del bad['lifecycle'][path]
            with self.assertRaises(KeyError): c.endpoint(bad)
        for name in ('stats_after_identity', 'stats_before', 'stats_after', 'stats_after_measurement_drain', 'stats_after_shutdown'):
            for key in ('process_user_cpu_us', 'process_system_cpu_us', 'go_mallocs', 'go_total_alloc_bytes', 'go_gc_cycles'):
                for value in (-1, True, math.nan):
                    bad = deepcopy(good)
                    target = bad[name] if name in bad else bad['lifecycle'][name]
                    target[key] = value
                    with self.subTest(name=name, key=key, value=value), self.assertRaises(ValueError): c.endpoint(bad)
        bad = deepcopy(good)
        bad['stats_before']['process_user_cpu_us'] = bad['stats_after']['process_user_cpu_us'] + 1
        with self.assertRaises(ValueError): c.endpoint(bad)
        bad = deepcopy(good)
        for key in ('process_user_cpu_us', 'process_system_cpu_us'):
            bad['stats_after'][key] = bad['stats_before'][key]
        with self.assertRaises(ValueError): c.endpoint(bad)
        bad = deepcopy(good)
        bad['lifecycle']['complete'] = False
        with self.assertRaises(ValueError): c.endpoint(bad)


class LifecycleGuards(unittest.TestCase):
    def good(self):
        _, summary = fixture(c.schedule(IDS)[0], Path('/fixture'), {'cluster': 'fixture.cluster'})
        # e1-r000's exact cleanup/GC counters; resource counters remain synthetic.
        def counts(cleanups, gc):
            return dict(finalizers_queued=0, finalizers_executed=0,
                        cleanups_queued_estimate=cleanups, cleanups_executed_estimate=cleanups, gc_cycles=gc)
        summary['stats_before']['go_gc_cycles'] = 216
        summary['stats_after']['go_gc_cycles'] = 217
        life = summary['lifecycle']
        life.update(measurement_barrier_complete=True, database_closed=True, network_stopped=False,
            measurement_drain=dict(before=counts(21977, 217), after=counts(22002, 219), rounds=2, wall_ns=1, complete=True),
            shutdown_drain=dict(before=counts(22002, 219), after=counts(22003, 221), rounds=2, wall_ns=1, complete=True))
        life['stats_after_measurement_drain']['go_gc_cycles'] = 219
        life['stats_after_shutdown']['go_gc_cycles'] = 221
        return summary

    def test_e1_r000_new_cleanup_reaches_complete_fixpoint(self):
        summary = self.good()
        self.assertIs(lifecycle.validate_lifecycle(summary), summary['lifecycle'])
        for name, queued in (('measurement_drain', 25), ('shutdown_drain', 1)):
            drain = summary['lifecycle'][name]
            self.assertEqual(drain['after']['cleanups_queued_estimate'] - drain['before']['cleanups_queued_estimate'], queued)
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            (root / 'inputs').mkdir()
            helper = root / 'inputs' / 'lifecycle_helper.py'
            helper.write_bytes(Path(lifecycle.__file__).read_bytes())
            c.validate_lifecycle(summary, {'lifecycle_helper_sha256': c.sha(helper)}, root)

    def test_pending_exit_finalizers_and_cleanups_rejected(self):
        for name in ('measurement_drain', 'shutdown_drain'):
            for kind in ('finalizers', 'cleanups'):
                bad = self.good()
                after = bad['lifecycle'][name]['after']
                if kind == 'finalizers':
                    after['finalizers_queued'] = 1
                else:
                    after['cleanups_executed_estimate'] -= 1
                with self.subTest(drain=name, kind=kind), self.assertRaises(ValueError):
                    lifecycle.validate_lifecycle(bad)

    def test_incomplete_or_out_of_bounds_drain_rejected(self):
        for name in ('measurement_drain', 'shutdown_drain'):
            for key, value in (('complete', False), ('rounds', 1), ('rounds', 17), ('wall_ns', 0)):
                bad = self.good()
                bad['lifecycle'][name][key] = value
                if key == 'rounds' and value > 16:
                    # Keep GC/snapshot checks valid so only the round limit rejects.
                    life = bad['lifecycle']
                    life[name]['after']['gc_cycles'] += 15
                    life['stats_after_shutdown']['go_gc_cycles'] += 15
                    if name == 'measurement_drain':
                        life['stats_after_measurement_drain']['go_gc_cycles'] += 15
                        for side in ('before', 'after'):
                            life['shutdown_drain'][side]['gc_cycles'] += 15
                with self.subTest(drain=name, key=key, value=value), self.assertRaises(ValueError):
                    lifecycle.validate_lifecycle(bad)

    def test_invalid_decreasing_and_insufficient_gc_counters_rejected(self):
        for name in ('measurement_drain', 'shutdown_drain'):
            for side in ('before', 'after'):
                for key in self.good()['lifecycle'][name][side]:
                    for value in (-1, True, .5):
                        bad = self.good()
                        bad['lifecycle'][name][side][key] = value
                        with self.subTest(drain=name, side=side, key=key, value=value), self.assertRaises(ValueError):
                            lifecycle.validate_lifecycle(bad)
            bad = self.good()
            drain = bad['lifecycle'][name]
            drain['before']['cleanups_queued_estimate'] = drain['after']['cleanups_queued_estimate'] + 1
            with self.assertRaises(ValueError): lifecycle.validate_lifecycle(bad)
            bad = self.good()
            drain = bad['lifecycle'][name]
            drain['after']['gc_cycles'] = drain['before']['gc_cycles'] + drain['rounds'] - 1
            with self.assertRaises(ValueError): lifecycle.validate_lifecycle(bad)

    def test_barrier_shutdown_and_resource_checks_preserved(self):
        for key in ('complete', 'measurement_barrier_complete', 'database_closed', 'network_stopped'):
            bad = self.good()
            bad['lifecycle'][key] = not bad['lifecycle'][key]
            with self.subTest(key=key), self.assertRaises(ValueError): lifecycle.validate_lifecycle(bad)
        for path in ('stats_after_identity', 'stats_before', 'stats_after', 'stats_after_measurement_drain', 'stats_after_shutdown'):
            for key in snapshot(0, 0):
                for value in (-1, True):
                    bad = self.good()
                    target = bad[path] if path in bad else bad['lifecycle'][path]
                    target[key] = value
                    with self.subTest(path=path, key=key, value=value), self.assertRaises(ValueError):
                        lifecycle.validate_lifecycle(bad)
            bad = self.good()
            target = bad[path] if path in bad else bad['lifecycle'][path]
            del target['go_gc_pause_ns']
            with self.assertRaises(ValueError): lifecycle.validate_lifecycle(bad)
        bad = self.good()
        bad['stats_before']['process_high_water_rss_kb'] += 1
        with self.assertRaises(ValueError): lifecycle.validate_lifecycle(bad)
        bad = self.good()
        for value in (bad['stats_after_identity'], bad['stats_before'], bad['stats_after'],
                      bad['lifecycle']['stats_after_measurement_drain'], bad['lifecycle']['stats_after_shutdown']):
            value['process_user_cpu_us'] = value['process_system_cpu_us'] = 100
        with self.assertRaises(ValueError): lifecycle.validate_lifecycle(bad)
        for path in ('stats_after_measurement_drain', 'stats_after_shutdown'):
            bad = self.good()
            bad['lifecycle'][path]['go_gc_cycles'] -= 1
            with self.subTest(path=path), self.assertRaises(ValueError): lifecycle.validate_lifecycle(bad)


class Statistics(unittest.TestCase):
    def test_literal_welch_and_student_reference(self):
        result = a.interval([[-.1, 0, .1, -.1, 0, .1], [-.2, 0, .2, -.2, 0, .2]])
        self.assertAlmostEqual(result['mean_log'], 0)
        self.assertAlmostEqual(result['se_log'], math.sqrt(1 / 600))
        self.assertAlmostEqual(result['df'], 125 / 17)
        # Independent t-table bracket: df=125/17 lies between7and8, and
        # t(.975,8)=2.3060041352 < t(.975,125/17) < t(.975,7)=2.3646242510.
        self.assertGreater(result['critical'], 2.306004135204166)
        self.assertLess(result['critical'], 2.364624251010299)
        equal = a.interval([[-.1, 0, .1, -.1, 0, .1]] * 2)
        self.assertAlmostEqual(equal['se_log'], math.sqrt(1 / 1500))
        self.assertAlmostEqual(equal['df'], 10)
        self.assertAlmostEqual(equal['critical'], 2.2281388519649385, places=10)
        self.assertEqual(result['epoch_counts'], [6, 6])
        from scipy import stats
        self.assertAlmostEqual(float(stats.t.ppf(.975, 10)), 2.2281388519649385, places=10)

    def test_constant_unequal_epochs_and_empty_nonfinite(self):
        result = a.interval([[0] * 6, [math.log(1.01)] * 6])
        self.assertAlmostEqual(result['point_ratio'], math.sqrt(1.01))
        self.assertEqual(result['lower_ratio'], result['upper_ratio'])
        self.assertIsNone(result['df'])
        for values in ([], [[]], [[], []], [[0] * 5, [0] * 6], [[0] * 7, [0] * 6], [[math.nan] * 6, [0] * 6], [[math.inf] * 6, [0] * 6]):
            with self.assertRaises(ValueError): a.interval(values)
        for metric in a.ENDPOINTS:
            for contrast, roles in (('AB', ('baseline', 'production')), ('baselineAA', ('baseline', 'baseline')), ('productionAA', ('production', 'production'))):
                rows = [dict(alias=alias, binary=role, contrast=contrast, **{metric: value}) for alias, role, value in zip(('A', 'B'), roles, (100, 110))]
                self.assertAlmostEqual(a.pair_log(rows, contrast, metric), math.log(1.1))
                self.assertEqual(a.pair_log(rows, contrast, metric), a.pair_log(rows[::-1], contrast, metric))
                for bad in ([], rows[:1], [rows[0], rows[0]]):
                    with self.assertRaises(ValueError): a.pair_log(bad, contrast, metric)
                for value in (0, -1, math.nan, math.inf, True):
                    bad = deepcopy(rows)
                    bad[0][metric] = value
                    with self.assertRaises(ValueError): a.pair_log(bad, contrast, metric)
                bad = deepcopy(rows)
                bad[0]['binary'] = 'wrong'
                with self.assertRaises(ValueError): a.pair_log(bad, contrast, metric)
                bad = deepcopy(rows)
                bad[0]['contrast'] = 'wrong'
                with self.assertRaises(ValueError): a.pair_log(bad, contrast, metric)
                with self.assertRaises(ValueError): a.pair_log(rows, 'wrong', metric)
                with self.assertRaises(ValueError): a.pair_log(rows, contrast, 'wrong')

    def test_exact_both_endpoint_decision_boundaries_fail_precedence(self):
        passing = {metric: {cell: dict(point_ratio=1, lower_ratio=.99, upper_ratio=1.02) for cell in a.CELL_NAMES} for metric in a.ENDPOINTS}
        self.assertEqual(a.decision(passing)['verdict'], 'PASS')
        for metric in a.ENDPOINTS:
            cell = sorted(a.CELL_NAMES)[0]
            bad = deepcopy(passing)
            bad[metric][cell]['upper_ratio'] = 1.02000001
            self.assertEqual(a.decision(bad)['verdict'], 'unresolved')
            bad[metric][cell].update(point_ratio=1.02, upper_ratio=1.03)
            self.assertEqual(a.decision(bad)['verdict'], 'FAIL')
            better = deepcopy(passing)
            better[metric][cell].update(point_ratio=.98, lower_ratio=.97, upper_ratio=.999999)
            self.assertTrue(a.decision(better)['improvement'][metric][cell])
            better[metric][cell]['upper_ratio'] = 1.0
            self.assertFalse(a.decision(better)['improvement'][metric][cell])
        for bad in ({}, {a.ENDPOINTS[0]: passing[a.ENDPOINTS[0]]}, {metric: {} for metric in a.ENDPOINTS}):
            with self.assertRaises(ValueError): a.decision(bad)
        for value in (math.nan, math.inf, 0):
            bad = deepcopy(passing)
            bad[a.ENDPOINTS[0]][sorted(a.CELL_NAMES)[0]]['upper_ratio'] = value
            with self.assertRaises(ValueError): a.decision(bad)
        bad = deepcopy(passing)
        bad[a.ENDPOINTS[0]][sorted(a.CELL_NAMES)[0]]['lower_ratio'] = 1.1
        with self.assertRaises(ValueError): a.decision(bad)


class OperationalGuards(unittest.TestCase):
    def test_prepare_rejects_dirty_untracked_and_wrong_sealed_tree(self):
        args = SimpleNamespace(source_root=Path('/fixture-source'), evidence=Path('/fixture-evidence'),
                               production_tree='1' * 40, production_sha256='2' * 64)
        for status in (' M pkg/input.go\n', 'A  tools/input.py\n', '?? unexpected-input\n'):
            with patch.object(c, 'durable', return_value=True), patch.object(c.subprocess, 'run'), \
                 patch.object(c.subprocess, 'check_output', side_effect=['tree', status]):
                with self.assertRaisesRegex(ValueError, 'clean tracked/staged'): c.prepare(args)
        with patch.object(c, 'durable', return_value=True), patch.object(c.subprocess, 'run'), \
             patch.object(c.subprocess, 'check_output', side_effect=['tree', '', '3' * 40]):
            with self.assertRaisesRegex(ValueError, 'HEAD tree'): c.prepare(args)
        with patch.object(c, 'durable', return_value=True), patch.object(c.subprocess, 'check_output', return_value='commit'):
            with self.assertRaisesRegex(ValueError, 'object itself must be a tree'): c.prepare(args)
        with patch.object(c, 'durable', return_value=False), self.assertRaises(ValueError): c.prepare(args)

    def test_deadline_timeout_paths_status(self):
        now = dt.datetime(2026, 1, 1, tzinfo=dt.timezone.utc)
        self.assertEqual(c.deadline_seconds('2026-01-01T00:30:00Z', now), 1800)
        for value in ('2026-01-01T00:30:01Z', '2026-01-01T00:00:00Z', '2026-01-01T00:10:00', 'bad'):
            with self.assertRaises(ValueError): c.deadline_seconds(value, now)
        self.assertEqual(c.child_timeout(1000, 0), 120)
        self.assertEqual(c.child_timeout(125, 10), 110)
        with self.assertRaises(ValueError): c.child_timeout(5, 0)
        self.assertFalse(c.durable(Path('/tmp/x'), Path('/repo')))
        self.assertFalse(c.durable(Path('/repo/x'), Path('/repo')))
        self.assertTrue(c.durable(Path('/evidence/x'), Path('/repo')))
        rows = c.schedule(IDS)[:128]
        status = dict(complete=True, input_hashes_verified_after=True, completed=[r['id'] for r in rows])
        c.check_status(status, rows)
        for bad in (dict(status, complete=False), dict(status, input_hashes_verified_after=False), dict(status, completed=[]), dict(status, completed=status['completed'][::-1])):
            with self.assertRaises(ValueError): c.check_status(bad, rows)
        with self.assertRaises(ValueError): c.check_status(status, [])

    def test_helper_chain_positive_and_each_hash_identity_error(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            (root / 'rev5-primary.py').write_text('raise RuntimeError("must not import primary")\n')
            primary = c.sha(root / 'rev5-primary.py')
            (root / 'rev6-production-tail.py').write_text(f'PRIMARY={primary!r}\nH={c.HARNESS!r}\ndef gate(evidence): return 0\n')
            tail = c.sha(root / 'rev6-production-tail.py')
            (root / 'rev6-confirm.py').write_text(f'HELPER_SHA={tail!r}\nHARNESS={c.HARNESS!r}\n')
            pins = {p.name: c.sha(p) for p in root.iterdir()}
            self.assertEqual(c.helper_chain(root, pins).gate(root), 0)
            with self.assertRaises(ValueError): c.helper_chain(root, {})
            for name in pins:
                bad = dict(pins, **{name: '0' * 64})
                with self.assertRaises(ValueError): c.helper_chain(root, bad)
            for content in (f'HELPER_SHA={"0" * 64!r}\nHARNESS={c.HARNESS!r}\n', f'HELPER_SHA={tail!r}\nHARNESS="wrong"\n'):
                (root / 'rev6-confirm.py').write_text(content)
                pins['rev6-confirm.py'] = c.sha(root / 'rev6-confirm.py')
                with self.assertRaises(ValueError): c.helper_chain(root, pins)
            (root / 'lifecycle_helper.py').write_text('def validate_lifecycle(summary):\n if not summary["ok"]: raise ValueError("bad lifecycle")\n')
            inputs = root / 'inputs'
            inputs.mkdir()
            (inputs / 'lifecycle_helper.py').write_bytes((root / 'lifecycle_helper.py').read_bytes())
            frozen = dict(lifecycle_helper_sha256=c.sha(inputs / 'lifecycle_helper.py'))
            c.validate_lifecycle({'ok': True}, frozen, root)
            with self.assertRaises(ValueError): c.validate_lifecycle({'ok': False}, frozen, root)
            with self.assertRaises(ValueError): c.validate_lifecycle({'ok': True}, dict(lifecycle_helper_sha256='0' * 64), root)

    def test_source_snapshot_symlinks_vacuity_and_input_drift(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            for i in range(1001): (root / str(i)).write_text(str(i))
            (root / 'link').symlink_to('0')
            paths = b'\0'.join(p.name.encode() for p in root.iterdir()) + b'\0'
            with patch.object(c.subprocess, 'check_output', return_value=paths):
                tree = c.source_tree(root)
                self.assertEqual(len(tree['files']), 1002)
                self.assertTrue(tree['files']['link']['symlink'])
                self.assertEqual(tree['files']['link']['sha256'], hashlib.sha256(b'0').hexdigest())
            with patch.object(c.subprocess, 'check_output', return_value=b'0\0'):
                with self.assertRaises(ValueError): c.source_tree(root)
            e = root / 'e'
            e.mkdir()
            c.save(e / 'schedule.json', c.schedule(IDS, '0' * 32))
            c.save(e / 'schedule-diagnostics.json', c.diagnostic_schedule(IDS, '0' * 32))
            (e / 'inputs').mkdir()
            prior = root / 'prior'
            prior.mkdir()
            c.save(prior / 'schedule.json', c.schedule(IDS))
            c.save(prior / 'schedule-diagnostics.json', c.diagnostic_schedule(IDS))
            c.save(prior / 'frozen.json', {'hashes': {str((prior / name).resolve()): c.sha(prior / name)
                                                   for name in ('schedule.json', 'schedule-diagnostics.json')}})
            for name in ('frozen.json', 'schedule.json', 'schedule-diagnostics.json'):
                (e / 'inputs' / ('prior-' + name)).write_bytes((prior / name).read_bytes())
            frozen = dict(source_root=str(root), source_tree=tree, hashes={str(root / '0'): c.sha(root / '0')}, identities=IDS,
                          prefix_namespace='0' * 32, prior_evidence=str(prior), prior_frozen_sha256=c.sha(prior / 'frozen.json'))
            with patch.object(c, 'source_tree', return_value=tree):
                c.verify_inputs(e, frozen)
                for namespace in ('', None, 'short', '1' * 32):
                    with self.subTest(namespace=namespace), self.assertRaises(ValueError):
                        c.verify_inputs(e, dict(frozen, prefix_namespace=namespace))
                missing_namespace = dict(frozen)
                del missing_namespace['prefix_namespace']
                with self.assertRaises(ValueError): c.verify_inputs(e, missing_namespace)
                for name in ('schedule.json', 'schedule-diagnostics.json'):
                    path = e / name
                    original = path.read_bytes()
                    changed = c.read_json(path)
                    changed[0]['prefix'] = 'clientbench/rfc263/' + 'f' * 48 + '/'
                    c.save(path, changed)
                    with self.subTest(schedule=name), self.assertRaises(ValueError): c.verify_inputs(e, frozen)
                    path.write_bytes(original)
                for name in ('prior-frozen.json', 'prior-schedule.json', 'prior-schedule-diagnostics.json'):
                    path = e / 'inputs' / name
                    original = path.read_bytes()
                    c.save(path, {})
                    with self.subTest(prior_copy=name), self.assertRaises(ValueError): c.verify_inputs(e, frozen)
                    path.write_bytes(original)
                with self.assertRaises(ValueError): c.verify_inputs(e, dict(frozen, prior_frozen_sha256='0' * 64))
                with self.assertRaises(ValueError): c.verify_inputs(e, dict(frozen, hashes={}))
                (root / '0').write_text('changed')
                with self.assertRaises(ValueError): c.verify_inputs(e, frozen)
                frozen['hashes'] = {str(root / '0'): c.sha(root / '0')}
                c.save(e / 'schedule.json', [])
                with self.assertRaises(ValueError): c.verify_inputs(e, frozen)
            with patch.object(c, 'source_tree', return_value={}):
                with self.assertRaises(ValueError): c.verify_inputs(e, frozen)

    def test_supervision_success_timeout_kill_and_interrupt_no_real_process(self):
        cases = (([0], 0), ([subprocess.TimeoutExpired('x', 1), 0], 124),
                 ([subprocess.TimeoutExpired('x', 1), subprocess.TimeoutExpired('x', 2), 0], 124),
                 ([KeyboardInterrupt(), 0], 124), ([-9, 0], -9))
        for waits, expected in cases:
            with tempfile.TemporaryDirectory() as tmp:
                e = Path(tmp)
                args = SimpleNamespace(deadline_utc='unused', evidence=e, epoch=1, authorization='fixture')
                rows = c.schedule(IDS)[:128]
                c.save(e / 'schedule-epoch1.json', rows)
                remaining = list(waits)
                def wait(timeout):
                    value = remaining.pop(0) if remaining else 0
                    if isinstance(value, BaseException): raise value
                    if expected == 0 and not (e / 'epoch1').exists():
                        (e / 'epoch1').mkdir()
                        c.save(e / 'epoch1' / 'status.json', dict(complete=True, input_hashes_verified_after=True,
                            completed=[r['id'] for r in rows]))
                    return value
                child = Mock(pid=123)
                child.wait.side_effect = wait
                def kill_group(pid, sig):
                    if sig == 0: raise ProcessLookupError()
                    with (e / 'lock').open('a') as contender:
                        with self.assertRaises(BlockingIOError): c.fcntl.flock(contender, c.fcntl.LOCK_EX | c.fcntl.LOCK_NB)
                original_failure = c.terminal_failure
                def fail_while_locked(arguments, reason):
                    with (e / 'lock').open('a') as contender:
                        with self.assertRaises(BlockingIOError): c.fcntl.flock(contender, c.fcntl.LOCK_EX | c.fcntl.LOCK_NB)
                    return original_failure(arguments, reason)
                with patch.object(c, 'deadline_seconds', return_value=1800), \
                     patch.object(c, 'frozen_inputs', return_value={'lock': str(e / 'lock')}), \
                     patch.object(c.subprocess, 'Popen', return_value=child), patch.object(c.signal, 'signal'), \
                     patch.object(c, 'enable_subreaper'), patch.object(c.time, 'sleep'), \
                     patch.object(c.os, 'waitpid', side_effect=ChildProcessError), \
                     patch.object(c, 'terminal_failure', side_effect=fail_while_locked), \
                     patch.object(c.os, 'killpg', side_effect=kill_group) as kill, patch('builtins.print'), self.assertRaises(SystemExit) as ex:
                    c.supervise(args)
                self.assertEqual(ex.exception.code, expected)
                self.assertEqual([call.args[1] for call in kill.call_args_list if call.args[1] != 0], [c.signal.SIGTERM, c.signal.SIGKILL])
                if expected != 0:
                    status = c.read_json(e / 'epoch1' / 'status.json')
                    self.assertFalse(status['complete'])
                    self.assertTrue(status['supervisor_failure'])
                    self.assertIn('supervisor-stop.json', c.read_json(e / 'epoch1' / 'artifact-sha256.json'))
        with patch.object(c, 'deadline_seconds', return_value=100), self.assertRaises(ValueError): c.supervise(args)
        with patch.object(c, 'deadline_seconds', return_value=1800), patch.object(c.Path, 'exists', return_value=True), self.assertRaises(ValueError): c.supervise(args)

    def test_prompt_leader_exit_stubborn_descendant_always_killed_while_lock_held(self):
        # TERM promptly reaps the leader, but a descendant remains until KILL.
        # The old conditional-KILL implementation fails this stateful fixture.
        child = Mock(pid=123)
        child.wait.return_value = 0
        descendant_alive = [True]
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / 'lane.lock'
            with path.open('a') as lane:
                c.fcntl.flock(lane, c.fcntl.LOCK_EX | c.fcntl.LOCK_NB)
                def signal_group(pid, signum):
                    self.assertEqual(pid, 123)
                    with path.open('a') as contender:
                        with self.assertRaises(BlockingIOError): c.fcntl.flock(contender, c.fcntl.LOCK_EX | c.fcntl.LOCK_NB)
                    if signum == c.signal.SIGKILL: descendant_alive[0] = False
                    if signum == 0 and not descendant_alive[0]: raise ProcessLookupError()
                with patch.object(c.os, 'killpg', side_effect=signal_group) as kill, \
                     patch.object(c.os, 'waitpid', side_effect=[(456, 0), (0, 0)]) as reaped, patch.object(c.time, 'sleep'):
                    c.stop_process_group(child)
                    self.assertEqual(reaped.call_count, 2)
                self.assertEqual([call.args[1] for call in kill.call_args_list if call.args[1] != 0], [c.signal.SIGTERM, c.signal.SIGKILL])
        self.assertFalse(descendant_alive[0])
        with patch.object(c.os, 'killpg', side_effect=ProcessLookupError), \
             patch.object(c.os, 'waitpid', side_effect=ChildProcessError), patch.object(c.time, 'sleep'):
            c.stop_process_group(child)
        with self.assertRaises(KeyboardInterrupt): c.stop_requested(c.signal.SIGTERM, None)
        with patch.object(c.os, 'killpg'), patch.object(c.os, 'waitpid', return_value=(0, 0)), \
             patch.object(c.time, 'monotonic', side_effect=[0, 0, 0, 3]), patch.object(c.time, 'sleep'), \
             self.assertRaisesRegex(ValueError, 'did not disappear'):
            c.stop_process_group(child)
        with patch.object(c.os, 'killpg', side_effect=[PermissionError('TERM fixture'), None]) as kill, self.assertRaises(PermissionError):
            c.stop_process_group(child)
        self.assertEqual([call.args[1] for call in kill.call_args_list], [c.signal.SIGTERM, c.signal.SIGKILL])

    def test_parent_failure_seals_partial_and_preserves_interrupted_status(self):
        with tempfile.TemporaryDirectory() as tmp:
            e = Path(tmp)
            directory = e / 'epoch1'
            directory.mkdir()
            (directory / 'status.json').write_text('{interrupted')
            (directory / 'partial.csv').write_text('retained partial output')
            args = SimpleNamespace(evidence=e, epoch=1, authorization='fixture', deadline_utc='fixture')
            with patch('builtins.print'):
                c.terminal_failure(args, 'worker SIGKILL fixture')
                c.seal_population(directory)
            self.assertFalse(c.read_json(directory / 'status.json')['complete'])
            self.assertEqual((directory / 'interrupted-status.json').read_text(), '{interrupted')
            index = c.read_json(directory / 'artifact-sha256.json')
            self.assertEqual(index['partial.csv'], c.sha(directory / 'partial.csv'))
            self.assertIn('status.json', index)
            self.assertIn('supervisor-stop.json', index)
            c.save(directory / 'status.json', dict(complete=True, input_hashes_verified_after=True, completed=['partial']))
            c.terminal_failure(args, 'TERM after worker prewrote complete')
            self.assertFalse(c.read_json(directory / 'status.json')['complete'])
            self.assertFalse(c.read_json(directory / 'status.json')['input_hashes_verified_after'])

    def test_inherited_lane_descriptor_identity(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / 'lock'
            wrong = Path(tmp) / 'wrong'
            wrong.touch()
            with path.open('a') as lock, patch.dict(os.environ, {'RFC263_CPU_LOCK_FD': str(lock.fileno()), 'RFC263_CPU_SUPERVISOR_PID': '123'}), \
                 patch.object(c.os, 'getppid', return_value=123):
                c.require_supervised_worker({'lock': str(path)})
                with c.collection_lock({'lock': str(path)}) as inherited:
                    self.assertEqual(os.fstat(inherited.fileno()).st_ino, os.fstat(lock.fileno()).st_ino)
                with self.assertRaises(ValueError): c.collection_lock({'lock': str(wrong)})
                with patch.dict(os.environ, {'RFC263_CPU_SUPERVISOR_PID': 'wrong'}), self.assertRaises(ValueError):
                    c.require_supervised_worker({'lock': str(path)})
            with patch.dict(os.environ, {}, clear=True), self.assertRaises(ValueError): c.require_supervised_worker({'lock': str(path)})

    def test_subreaper_failure_is_explicit(self):
        libc = Mock()
        libc.prctl.return_value = -1
        with patch.object(c.ctypes, 'CDLL', return_value=libc), self.assertRaisesRegex(ValueError, 'subreaper'): c.enable_subreaper()
        libc.prctl.return_value = 0
        with patch.object(c.ctypes, 'CDLL', return_value=libc): c.enable_subreaper()


class ForensicAndQuarantine(unittest.TestCase):
    def args(self, root):
        return SimpleNamespace(evidence=root, epoch=1, authorization='fixture owner approval', deadline_utc='fixture')

    def test_forensic_success_has_own_utc_bounds_and_one_seal(self):
        with tempfile.TemporaryDirectory() as tmp:
            e = Path(tmp)
            directory = e / 'epoch1'
            directory.mkdir()
            c.save(directory / 'status.json', dict(complete=True, input_hashes_verified_after=True, completed=['fixture']))
            with patch('builtins.print') as output:
                self.assertTrue(c.forensic_seal(self.args(e), c.utc()))
            log = c.read_json(directory / 'forensic-status.json')
            terminal = c.read_json(e / 'epoch1-forensic-terminal.json')
            self.assertEqual(log['state'], 'complete')
            start, end = (dt.datetime.fromisoformat(log[k]) for k in ('started_utc', 'deadline_utc'))
            self.assertEqual((end - start).total_seconds(), 300)
            self.assertEqual((end - dt.datetime.fromisoformat(log['hash_deadline_utc'])).total_seconds(), 5)
            self.assertGreaterEqual(dt.datetime.fromisoformat(terminal['finished_utc']), dt.datetime.fromisoformat(log['finished_utc']))
            seals = [call.args[0] for call in output.call_args_list if '_SEAL_SHA256=' in str(call.args[0])]
            self.assertEqual(seals, ['EPOCH1_SEAL_SHA256=' + c.sha(directory / 'artifact-sha256.json')])

    def test_forensic_timeout_invalidates_and_never_prints_seal(self):
        with tempfile.TemporaryDirectory() as tmp:
            e = Path(tmp)
            directory = e / 'epoch1'
            directory.mkdir()
            c.save(directory / 'status.json', dict(complete=True, input_hashes_verified_after=True, completed=['fixture']))
            with patch.object(c, 'seal_population', side_effect=c.ForensicTimeout('fixture deadline')), patch('builtins.print') as output:
                self.assertFalse(c.forensic_seal(self.args(e), c.utc()))
            self.assertFalse(c.read_json(directory / 'status.json')['complete'])
            self.assertEqual(c.read_json(directory / 'forensic-status.json')['state'], 'incomplete')
            self.assertFalse(any('_SEAL_SHA256=' in str(call.args[0]) for call in output.call_args_list))
            path = e / 'hash-fixture'
            path.write_bytes(b'abc')
            for times in ([10], [0, 2], [0, 0, 2]):
                with patch.object(c.time, 'monotonic', side_effect=times), self.assertRaises(c.ForensicTimeout): c.sha(path, 1)

    def test_quarantine_does_not_release_when_group_merely_vanishes(self):
        class HoldReached(BaseException): pass
        with tempfile.TemporaryDirectory() as tmp:
            e = Path(tmp)
            args = self.args(e)
            release = e / 'owner-release.json'
            with (e / 'lane.lock').open('a') as lane:
                c.fcntl.flock(lane, c.fcntl.LOCK_EX | c.fcntl.LOCK_NB)
                def hold(seconds):
                    with (e / 'lane.lock').open('a') as contender:
                        with self.assertRaises(BlockingIOError): c.fcntl.flock(contender, c.fcntl.LOCK_EX | c.fcntl.LOCK_NB)
                    raise HoldReached()
                with patch.object(c, 'group_members', return_value=[dict(pid=456, state='D', ppid=123, pgid=123)]), \
                     patch.object(c, 'group_gone', return_value=True) as gone, patch.object(c, 'seal_population') as seal, patch.object(c, 'sha') as hashes, \
                     patch.object(c.time, 'sleep', side_effect=hold), patch('builtins.print'), self.assertRaises(HoldReached):
                    c.quarantine(args, Mock(pid=123), 'group not proved gone', release, 'fixture-token')
                seal.assert_not_called()
                hashes.assert_not_called()
                gone.assert_not_called()  # No group probe until the owner actually acknowledges.
            receipt = c.read_json(e / 'epoch1' / 'quarantine-unsealed.json')
            self.assertTrue(receipt['unsealed'])
            self.assertEqual(receipt['members'][0]['state'], 'D')
            self.assertFalse(c.read_json(e / 'epoch1' / 'status.json')['complete'])
            self.assertFalse((e / 'epoch1' / 'artifact-sha256.json').exists())

    def test_quarantine_release_requires_exact_owner_identity_and_group_gone(self):
        with tempfile.TemporaryDirectory() as tmp:
            e = Path(tmp)
            args = self.args(e)
            path = e / 'release.json'
            ack = dict(action='release_quarantine', scope='rfc263-cpu-quarantine', token='token', supervisor_pid=os.getpid(),
                       pgid=123, population='epoch1', evidence=str(e.resolve()), owner_ack='explicit fixture owner decision')
            with patch.object(c, 'group_gone', return_value=True) as gone:
                self.assertFalse(c.release_ack(path, args, 'token', 123))
                c.save(path, ack)
                self.assertTrue(c.release_ack(path, args, 'token', 123))
                for key in ack:
                    c.save(path, dict(ack, **{key: ''}))
                    self.assertFalse(c.release_ack(path, args, 'token', 123))
                c.save(path, ack)
                with patch.object(c, 'group_gone', return_value=False): self.assertFalse(c.release_ack(path, args, 'token', 123))
                with patch.object(c, 'group_gone', side_effect=PermissionError): self.assertFalse(c.release_ack(path, args, 'token', 123))
                with patch.object(c.os, 'getuid', return_value=os.getuid() + 1): self.assertFalse(c.release_ack(path, args, 'token', 123))
                link = e / 'link'
                link.symlink_to(path)
                self.assertFalse(c.release_ack(link, args, 'token', 123))
                path.write_text('x' * 4097)
                self.assertFalse(c.release_ack(path, args, 'token', 123))
                path.write_text('{bad')
                self.assertFalse(c.release_ack(path, args, 'token', 123))
                path.write_text('[]')
                self.assertFalse(c.release_ack(path, args, 'token', 123))
                path.write_text('[' * 1200 + ']' * 1200)
                self.assertFalse(c.release_ack(path, args, 'token', 123))

    def test_supervisor_cleanup_failure_holds_lock_and_never_seals(self):
        class HoldReached(BaseException): pass
        with tempfile.TemporaryDirectory() as tmp:
            e = Path(tmp)
            args = self.args(e)
            rows = c.schedule(IDS)[:128]
            c.save(e / 'schedule-epoch1.json', rows)
            child = Mock(pid=123)
            child.wait.return_value = -9
            def hold(*arguments):
                with (e / 'lane.lock').open('a') as contender:
                    with self.assertRaises(BlockingIOError): c.fcntl.flock(contender, c.fcntl.LOCK_EX | c.fcntl.LOCK_NB)
                raise HoldReached()
            with patch.object(c, 'deadline_seconds', return_value=1800), patch.object(c, 'frozen_inputs', return_value={'lock': str(e / 'lane.lock')}), \
                 patch.object(c, 'enable_subreaper'), patch.object(c.subprocess, 'Popen', return_value=child), patch.object(c.signal, 'signal'), \
                 patch.object(c, 'stop_process_group', side_effect=ValueError('group remains')), patch.object(c, 'quarantine', side_effect=hold), \
                 patch.object(c, 'forensic_seal') as forensic, patch.object(c, 'seal_population') as seal, self.assertRaises(HoldReached):
                c.supervise(args)
            forensic.assert_not_called()
            seal.assert_not_called()
            with patch.object(c, 'deadline_seconds', return_value=1800), patch.object(c, 'frozen_inputs', return_value={'lock': str(e / 'lane.lock')}), \
                 patch.object(c, 'enable_subreaper'), patch.object(c.subprocess, 'Popen', return_value=child), patch.object(c.signal, 'signal'), \
                 patch.object(c, 'stop_process_group', side_effect=ValueError('group remains')), patch.object(c, 'quarantine', return_value=None), \
                 patch.object(c, 'forensic_seal') as forensic, self.assertRaises(SystemExit) as stopped:
                c.supervise(args)
            self.assertEqual(stopped.exception.code, 125)
            forensic.assert_not_called()


class CollectionFixtures(unittest.TestCase):
    def run_collection(self, failure=None, epoch=1):
        with tempfile.TemporaryDirectory() as tmp:
            e = Path(tmp)
            rows = c.schedule(IDS, '0' * 32)
            c.save(e / 'schedule.json', rows)
            c.save(e / f'schedule-epoch{epoch}.json', [r for r in rows if r['epoch'] == epoch])
            frozen = dict(identities=IDS, cluster='fixture.cluster', lock=str(e / 'lock'), helper_hashes={}, prefix_namespace='0' * 32)
            args = SimpleNamespace(evidence=e, epoch=epoch, authorization='fixture owner approval', deadline_utc='fixture', deadline=2000)
            clock = [1.0]
            def sleep(seconds): clock[0] += seconds
            def run(cmd, **kwargs):
                if failure == 'child': raise subprocess.TimeoutExpired(cmd, 120)
                if failure == 'term': c.stop_requested(c.signal.SIGTERM, None)
                out = Path(cmd[cmd.index('--output') + 1])
                row = next(r for r in rows if r['id'] == out.name)
                out.mkdir()
                manifest, summary = fixture(row, out, frozen)
                c.save(out / 'manifest.json', manifest)
                c.save(out / 'summary.json', summary)
                (out / 'samples.csv').write_text('fixture')
                (out / 'warmup.csv').write_text('fixture')
                return SimpleNamespace(returncode=1 if failure == 'returncode' else 0)
            helper = SimpleNamespace(gate=lambda e: 99 if failure == 'load' else 0)
            with patch.object(c.os, 'getpriority', return_value=0 if failure == 'nice' else 19), \
                 patch.object(c.os, 'sched_getaffinity', return_value=c.CPUS), \
                 patch.dict(os.environ, {}, clear=True), patch.object(c, 'frozen_inputs', return_value=frozen), \
                 patch.object(c, 'helper_chain', return_value=helper), patch.object(c.time, 'sleep', side_effect=sleep), \
                 patch.object(c.time, 'monotonic', side_effect=lambda: clock[0]), \
                 patch.object(c.time, 'monotonic_ns', side_effect=lambda: int(clock[0] * 1e9)), \
                 patch.object(c.subprocess, 'run', side_effect=run) as child, \
                 patch.object(c, 'csv_integrity') as csv_check, patch.object(c, 'validate_lifecycle') as lifecycle, \
                 patch.object(c, 'verify_inputs', side_effect=ValueError('drift') if failure == 'drift' else None), \
                 patch('builtins.print'):
                if failure or epoch == 2:
                    with self.assertRaises((ValueError, subprocess.TimeoutExpired, FileNotFoundError, KeyboardInterrupt)): c.collect(args)
                    self.assertFalse(c.read_json(e / f'epoch{epoch}' / 'status.json')['complete'])
                else:
                    # Each fixture process consumes15seconds: proves no historical10second exclusion.
                    original_run = run
                    def long_run(cmd, **kwargs):
                        clock[0] += 11
                        return original_run(cmd, **kwargs)
                    child.side_effect = long_run
                    args.deadline = 4000
                    c.collect(args)
                    self.assertEqual(child.call_count, 128)
                    self.assertEqual(csv_check.call_count, 256)
                    self.assertEqual(lifecycle.call_count, 128)
                    self.assertTrue(c.read_json(e / 'epoch1' / 'status.json')['complete'])
                self.assertFalse((e / f'epoch{epoch}' / 'artifact-sha256.json').exists())  # Supervisor-only seal.
                if failure in ('nice', 'load') or epoch == 2:
                    self.assertEqual(child.call_count, 0)

    def test_complete_epoch_no10second_exclusion(self): self.run_collection()
    def test_child_timeout_no_replacement(self): self.run_collection('child')
    def test_worker_term_finalizes_failure_before_supervisor_seal(self): self.run_collection('term')
    def test_returncode_no_replacement(self): self.run_collection('returncode')
    def test_load_deadline_before_launch(self): self.run_collection('load')
    def test_wrong_priority_no_launch(self): self.run_collection('nice')
    def test_postcollection_source_drift_invalidates_epoch(self): self.run_collection('drift')
    def test_epoch2_requires_complete_epoch1(self): self.run_collection(epoch=2)

    def run_diagnostics(self, bad_profile=False, incomplete_prior=False):
        with tempfile.TemporaryDirectory() as tmp:
            e = Path(tmp)
            rows, diag = c.schedule(IDS, '0' * 32), c.diagnostic_schedule(IDS, '0' * 32)
            c.save(e / 'schedule.json', rows)
            c.save(e / 'schedule-diagnostics.json', diag)
            for prior in (1, 2):
                directory = e / f'epoch{prior}'
                directory.mkdir()
                c.save(directory / 'status.json', dict(complete=not incomplete_prior, input_hashes_verified_after=True,
                    completed=[r['id'] for r in rows if r['epoch'] == prior]))
            frozen = dict(identities=IDS, cluster='fixture.cluster', lock=str(e / 'lock'), helper_hashes={}, prefix_namespace='0' * 32)
            args = SimpleNamespace(evidence=e, epoch=0, population='diagnostics', authorization='fixture diagnostic approval',
                                   deadline_utc='fixture', deadline=1500)
            clock = [1.0]
            def sleep(seconds): clock[0] += seconds
            def run(cmd, **kwargs):
                clock[0] += 3
                out = Path(cmd[cmd.index('--output') + 1])
                row = next(r for r in diag if r['id'] == out.name)
                out.mkdir()
                manifest, summary = fixture(row, out, frozen)
                c.save(out / 'manifest.json', manifest)
                c.save(out / 'summary.json', summary)
                (out / 'samples.csv').write_text('fixture')
                (out / 'warmup.csv').write_text('fixture')
                (out / (row['profile'] + '.pprof')).write_bytes(b'' if bad_profile else b'profile fixture')
                return SimpleNamespace(returncode=0)
            with patch.object(c.os, 'getpriority', return_value=19), patch.object(c.os, 'sched_getaffinity', return_value=c.CPUS), \
                 patch.dict(os.environ, {}, clear=True), patch.object(c, 'frozen_inputs', return_value=frozen), \
                 patch.object(c, 'helper_chain', return_value=SimpleNamespace(gate=lambda e: 0)), \
                 patch.object(c.time, 'sleep', side_effect=sleep), patch.object(c.time, 'monotonic', side_effect=lambda: clock[0]), \
                 patch.object(c.time, 'monotonic_ns', side_effect=lambda: int(clock[0] * 1e9)), \
                 patch.object(c.subprocess, 'run', side_effect=run) as child, patch.object(c, 'csv_integrity') as raw, \
                 patch.object(c, 'validate_lifecycle'), patch.object(c, 'verify_inputs'), patch('builtins.print'):
                if bad_profile or incomplete_prior:
                    with self.assertRaises(ValueError): c.collect(args)
                    self.assertFalse(c.read_json(e / 'diagnostics' / 'status.json')['complete'])
                    self.assertEqual(child.call_count, 0 if incomplete_prior else 1)
                else:
                    c.collect(args)
                    self.assertTrue(c.read_json(e / 'diagnostics' / 'status.json')['complete'])
                    self.assertEqual(child.call_count, 16)
                    self.assertEqual(raw.call_count, 32)
            self.assertFalse((e / 'diagnostics' / 'artifact-sha256.json').exists())  # Supervisor-only seal.

    def test_complete_separate_diagnostics(self): self.run_diagnostics()
    def test_empty_profile_aborts_without_replacement(self): self.run_diagnostics(bad_profile=True)
    def test_diagnostics_require_both_complete_timing_epochs(self): self.run_diagnostics(incomplete_prior=True)


class AnalyzerFixtures(unittest.TestCase):
    def build_evidence(self, e):
        rows = c.schedule(IDS, '0' * 32)
        frozen = dict(seed=c.SEED, source_root=str(e.parent / 'source'), identities=IDS, cluster='fixture.cluster', helper_hashes={}, lock=str(e.parent / 'lock'), prefix_namespace='0' * 32)
        c.save(e / 'frozen.json', frozen)
        c.save(e / 'schedule.json', rows)
        seals = []
        for epoch in (1, 2):
            directory = e / f'epoch{epoch}'
            (directory / 'runs').mkdir(parents=True)
            rr = [r for r in rows if r['epoch'] == epoch]
            c.save(directory / 'status.json', dict(epoch=epoch, complete=True, input_hashes_verified_after=True, completed=[r['id'] for r in rr]))
            for row in rr:
                out = directory / 'runs' / row['id']
                out.mkdir()
                manifest, summary = fixture(row, out, frozen, .99 if row['binary'] == 'production' else 1)
                c.save(out / 'manifest.json', manifest)
                c.save(out / 'summary.json', summary)
                for name in ('samples.csv', 'warmup.csv'): (out / name).write_text('fixture')
                c.save(directory / 'runs' / (row['id'] + '.command.json'), dict(schedule=row, returncode=0, env=c.ENV, timeout_seconds=120, duration_ns=15000000000))
            c.save(directory / 'artifact-sha256.json', {str(p.relative_to(directory)): c.sha(p) for p in directory.rglob('*') if p.is_file()})
            seals.append(c.sha(directory / 'artifact-sha256.json'))
        return rows, frozen, seals

    def test_full_sealed_dual_endpoint_pipeline_and_aa_context(self):
        # Synthetic files only; raw CSV parser separately tested with real fixture rows.
        with tempfile.TemporaryDirectory() as tmp:
            e = Path(tmp) / 'evidence'
            e.mkdir()
            rows, frozen, seals = self.build_evidence(e)
            args = SimpleNamespace(evidence=e, output=Path(tmp) / 'analysis', frozen_sha256=c.sha(e / 'frozen.json'), seal1=seals[0], seal2=seals[1])
            with patch.object(c.os, 'getpriority', return_value=19), patch.object(c.os, 'sched_getaffinity', return_value=c.CPUS), \
                 patch.object(c, 'verify_inputs'), patch.object(c, 'helper_chain'), patch.object(c, 'csv_integrity') as csv_check, \
                 patch.object(c, 'validate_lifecycle') as life, patch.object(c, 'durable', return_value=True), patch('builtins.print'):
                a.analyze(args)
            result = c.read_json(args.output / 'results.json')
            self.assertEqual(result['processes'], 256)
            self.assertEqual(result['pairs'], 128)
            self.assertEqual(result['decision']['verdict'], 'PASS')
            self.assertEqual(csv_check.call_count, 512)
            self.assertEqual(life.call_count, 256)
            for metric in a.ENDPOINTS:
                self.assertEqual(set(result['primary_and_coprimary'][metric]), a.CELL_NAMES)
                for cell in a.CELL_NAMES:
                    self.assertAlmostEqual(result['primary_and_coprimary'][metric][cell]['point_ratio'], .99)
                    self.assertIsNone(result['aa_diagnostics'][metric][cell]['baselineAA']['interval'])
                    self.assertEqual(result['aa_diagnostics'][metric][cell]['baselineAA']['pairs'], 2)
            # Missing/incomplete epoch2 cannot trigger endpoint extraction from epoch1.
            status_path = e / 'epoch2' / 'status.json'
            status = c.read_json(status_path)
            status['complete'] = False
            c.save(status_path, status)
            directory = e / 'epoch2'
            index = c.read_json(directory / 'artifact-sha256.json')
            index['status.json'] = c.sha(status_path)
            c.save(directory / 'artifact-sha256.json', index)
            args.seal2 = c.sha(directory / 'artifact-sha256.json')
            with patch.object(c.os, 'getpriority', return_value=19), patch.object(c.os, 'sched_getaffinity', return_value=c.CPUS), \
                 patch.object(c, 'verify_inputs'), patch.object(c, 'endpoint') as extract:
                with self.assertRaises(ValueError): a.analyze(args)
                extract.assert_not_called()

    def test_seal_hash_census_status_receipt_and_path_errors(self):
        with tempfile.TemporaryDirectory() as tmp:
            e = Path(tmp)
            directory = e / 'epoch1'
            directory.mkdir()
            rows = c.schedule(IDS)[:128]
            def seal(index=None):
                if index is None: index = {str(p.relative_to(directory)): c.sha(p) for p in directory.rglob('*') if p.is_file() and p.name != 'artifact-sha256.json'}
                c.save(directory / 'artifact-sha256.json', index)
                return c.sha(directory / 'artifact-sha256.json')
            with self.assertRaises(ValueError): a.checked_epoch(e, 1, seal({}), rows)
            c.save(directory / 'status.json', dict(epoch=1, complete=True, input_hashes_verified_after=True, completed=[r['id'] for r in rows]))
            goodseal = seal()
            with self.assertRaises(ValueError): a.checked_epoch(e, 1, '0' * 64, rows)
            (directory / 'extra').write_text('new')
            with self.assertRaises(ValueError): a.checked_epoch(e, 1, goodseal, rows)
            goodseal = seal()
            (directory / 'extra').write_text('changed')
            with self.assertRaises(ValueError): a.checked_epoch(e, 1, goodseal, rows)
            with self.assertRaises(ValueError): a.checked_epoch(e, 1, seal({'status.json': c.sha(directory / 'status.json'), '../x': '0' * 64}), rows)
            with self.assertRaises(ValueError): a.checked_epoch(e, 1, seal(), rows)


def run_tests():
    suite = unittest.defaultTestLoader.loadTestsFromModule(sys.modules[__name__])
    if suite.countTestCases() == 0:
        raise RuntimeError('vacuous guard test population')
    result = unittest.TextTestRunner(verbosity=2).run(suite)
    if not result.wasSuccessful() or result.testsRun < 24:
        raise SystemExit(1)
    queue_tests = c.load_module(Path(__file__).with_name('cpu_queuewait_guard_test.py'), 'cpu_queuewait_guard_tests')
    queue_count = queue_tests.run_tests()
    if queue_count <= 0:
        raise RuntimeError('vacuous queuewait guard test population')
    a2_tests = c.load_module(Path(__file__).with_name('cpu_a2_guard_test.py'), 'cpu_a2_guard_tests')
    a2_count = a2_tests.run_tests()
    if a2_count <= 0:
        raise RuntimeError('vacuous A2 fixture population')
    print(f'CPU_GUARDS_OK {result.testsRun}+{queue_count}+{a2_count} nonempty fixture groups; no builds or measurements')


if __name__ == '__main__':
    run_tests()
