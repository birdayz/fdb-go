#!/usr/bin/env python3
"""Offline queuewait guards. No benchmark inputs, builds or real pprof required.

Import has no effects. The owner runs run_tests() only in the reserved nice19
fast-lock lane; this file deliberately does not launch an unlocked test command.
"""
from __future__ import annotations

import copy
import gzip
import hashlib
import importlib.util
import io
import json
from pathlib import Path
import subprocess
import sys
import tarfile
import tempfile
import types
import unittest
from unittest import mock

sys.dont_write_bytecode = True
HERE = Path(__file__).resolve().parent
SPEC = importlib.util.spec_from_file_location('queuewait_fixture_subject', HERE / 'rev6-cpu-queuewait.py')
q = importlib.util.module_from_spec(SPEC)
sys.modules[SPEC.name] = q
SPEC.loader.exec_module(q)
PIN = 'a' * 64
SOURCE = '''package client
func (q *timeoutQueue) schedule() {
 q.mu.Lock()
 other.mu.Lock()
 cancel()
 q.mu.Unlock()
}
func (q *timeoutQueue) retire() {
 q.mu.Lock()
 q.mu.Unlock()
}
func (q *timeoutQueue) remove() {
 q.mu.Lock()
 q.mu.Unlock()
}
func (q *timeoutQueue) newNode() {
 q.mu.Lock()
 q.mu.Unlock()
}
func (q *timeoutQueue) newNodeForShard() {
 runtime.AddCleanup(tx, func() { n.queue.remove(n) })
}
func (q *timeoutQueue) fire() {
 q.mu.Lock()
 q.mu.Unlock()
}
func (q *timeoutQueue) collect() {
 q.mu.Lock()
 tx.readErrMu.Lock()
 tx.readErrMu.Unlock()
 q.mu.Unlock()
}
func (q *timeoutQueue) requestLocked() {
 q.timer.request()
}
func (root *timeoutQueueRoot) fire() {
 root.mu.Lock()
 root.mu.Unlock()
 q.mu.Lock()
 q.mu.Unlock()
}
func (root *timeoutQueueRoot) deliverReady() {
 q.mu.Lock()
 tx.readErrMu.Lock()
 q.mu.Unlock()
}
'''


def source(text=SOURCE, role='production'):
    return q.SourceBinding(text, PIN, role)


def frame(binding, method, statement, *, file=None, function=None, start=None):
    first, last = binding.methods[method]
    numbers = [number for number in range(first, last + 1) if binding.text.splitlines()[number - 1].strip() == statement]
    if len(numbers) != 1:
        raise AssertionError('fixture statement not unique in source method')
    return q.Frame(function or q.PACKAGE + '(*' + method[0] + ').' + method[1], file or q.QUEUE_FILE, numbers[0], first if start is None else start)


def raw(samples, *, columns=True, mappings=True):
    """samples = [(contentions, delay_ns, [innermost...outermost Frame])]."""
    lines = ['PeriodType: contentions count', 'Period: 1', 'Samples:', 'contentions/count delay/nanoseconds[dflt]']
    locations, identifier = [], 1
    for count, delay, frames in samples:
        ids = []
        for item in frames:
            ids.append(identifier)
            tail = f'{item.function} {item.file}:{item.line}' + (':0' if columns else '') + (f' s={item.start}' if item.start else '')
            locations.append(f'{identifier:6d}: 0x0 ' + ('M=1 ' if mappings else '') + tail)
            identifier += 1
        lines.append(f'{count:10d} {delay:10d}: ' + ' '.join(map(str, ids)))
    lines += ['Locations'] + locations + ['Mappings']
    if mappings:
        lines.append('1: 0x0/0x100/0x0 program build [FN][FL][LN][IN]')
    return '\n'.join(lines) + '\n'


def normal_frame():
    return q.Frame('runtime._LostContendedRuntimeLock', 'runtime/proc.go', 1)


def summary():
    keys = {'grv_cache_hits': 0, 'read_versions_completed': 500000, 'grv_successful_batch_replies': 1}
    return dict(completed=500000, stats_before=dict(process_user_cpu_us=100, process_system_cpu_us=200),
                stats_after=dict(process_user_cpu_us=120, process_system_cpu_us=210),
                client_counters_before=dict(observation='observed', values={k: 0 for k in keys}),
                client_counters_after=dict(observation='observed', values=dict(keys)),
                client_counters_delta=dict(observation='observed', values=dict(keys)))


def save_json(path, data):
    path.write_text(json.dumps(data))


def tar_source(path, files):
    with tarfile.open(path, 'w:gz') as archive:
        for name, data in files:
            item = tarfile.TarInfo(name)
            encoded = data.encode()
            item.size = len(encoded)
            archive.addfile(item, io.BytesIO(encoded))


class SourceGuards(unittest.TestCase):
    def test_exact_source_binding_and_fixed_fixture_values(self):
        binding = source()
        self.assertEqual(binding.methods[('timeoutQueue', 'schedule')], (2, 7))
        self.assertEqual(binding.sites[3], q.Site(3, 'shard', 'Lock', ('timeoutQueue', 'schedule')))
        self.assertEqual(binding.sites[6], q.Site(6, 'shard', 'Unlock', ('timeoutQueue', 'schedule')))
        self.assertEqual(len(binding.sites), 18)
        item = frame(binding, ('timeoutQueue', 'schedule'), 'q.mu.Unlock()')
        self.assertEqual(q.sample_classification([item], binding, 'mutex'), ('shard', 'caller', 'caller_schedule'))
        self.assertIsNone(q.sample_classification([item], binding, 'block'))
        self.assertEqual(q.sample_classification([frame(binding, ('timeoutQueue', 'schedule'), 'q.mu.Lock()')], binding, 'block'),
                         ('shard', 'caller', 'caller_schedule'))

    def test_file_method_start_and_line_misbinding_reject(self):
        binding = source()
        item = frame(binding, ('timeoutQueue', 'schedule'), 'q.mu.Unlock()')
        for bad in (q.Frame(item.function, 'pkg/other/timeout_queue.go', item.line, item.start),
                    q.Frame(item.function, item.file, item.line + 20, item.start),
                    q.Frame(item.function, item.file, item.line, item.start + 1),
                    q.Frame(q.PACKAGE + '(*timeoutQueue).retire', item.file, item.line, 0),
                    q.Frame(q.PACKAGE + '(*timeoutQueue).unknown', item.file, item.line, 0)):
            with self.subTest(bad=bad), self.assertRaises(ValueError):
                q.sample_classification([bad], binding, 'mutex')
        self.assertEqual(q.sample_classification([q.Frame(item.function, 'fdb.dev/' + item.file, item.line, item.start)], binding, 'mutex'),
                         ('shard', 'caller', 'caller_schedule'))

    def test_source_census_comments_strings_unknown_and_changed_idiom(self):
        noisy = SOURCE + '// q.mu.Lock()\nvar example = "root.mu.Unlock()"\n/* q.mu.Unlock() */\n'
        self.assertEqual(source(noisy).sites, source().sites)
        for text in (SOURCE + 'func (q *timeoutQueue) unknown() {\n q.mu.Lock()\n}\n',
                     SOURCE.replace(' q.mu.Lock()', ' if true { q.mu.Lock() }', 1), SOURCE.replace('q.mu.', 'q.other.')):
            with self.subTest(text=text), self.assertRaises(ValueError):
                source(text)
        for broken in ('/*', '"', "'", '`'):
            with self.subTest(broken=broken), self.assertRaises(ValueError):
                q.mask_go(broken)

    def test_baseline_absent_queue_and_archive_provenance(self):
        baseline = source(None, 'baseline')
        self.assertIsNone(q.sample_classification([normal_frame()], baseline, 'mutex'))
        with self.assertRaises(ValueError):
            q.sample_classification([frame(source(), ('timeoutQueue', 'schedule'), 'q.mu.Unlock()')], baseline, 'mutex')
        with self.assertRaises(ValueError):
            source(None)
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / 'source.tar.gz'
            files = [('go.mod', 'module fdb.dev\n'), ('pkg/fdbgo/client/transaction.go', 'package client\n')]
            tar_source(path, files)
            binding = q.SourceBinding.archive(path, q.sha(path), 'baseline')
            self.assertIsNone(binding.text)
            with self.assertRaises(ValueError):
                q.SourceBinding.archive(path, 'b' * 64, 'baseline')
            with self.assertRaises(ValueError):
                q.SourceBinding.archive(path, q.sha(path), 'production')
            tar_source(path, files + [(q.QUEUE_FILE, SOURCE)])
            self.assertEqual(q.SourceBinding.archive(path, q.sha(path), 'production').sites, source().sites)
            tar_source(path, [('snapshot/' + name, data) for name, data in files + [(q.QUEUE_FILE, SOURCE)]])
            self.assertEqual(q.SourceBinding.archive(path, q.sha(path), 'production').sites, source().sites)
            for bad in ([('go.mod', 'module other\n'), (q.QUEUE_FILE, SOURCE)],
                        files + [(q.QUEUE_FILE, SOURCE), ('copy/' + q.QUEUE_FILE, SOURCE)],
                        files + [('../escape', '')], files + [('go.mod', 'module fdb.dev\n')]):
                with self.subTest(bad=bad):
                    tar_source(path, bad)
                    with self.assertRaises(ValueError):
                        q.SourceBinding.archive(path, q.sha(path), 'baseline')

    def test_shard_root_caller_drainer_and_cleanup_not_method_type(self):
        binding = source()
        expected = [(('timeoutQueue', 'schedule'), 'q.mu.Unlock()', ('shard', 'caller', 'caller_schedule')),
                    (('timeoutQueue', 'retire'), 'q.mu.Unlock()', ('shard', 'caller', 'caller_retire')),
                    (('timeoutQueue', 'remove'), 'q.mu.Unlock()', ('shard', 'caller', 'caller_cleanup_remove')),
                    (('timeoutQueue', 'newNode'), 'q.mu.Unlock()', ('shard', 'caller', 'caller_newNode')),
                    (('timeoutQueue', 'fire'), 'q.mu.Unlock()', ('shard', 'drainer', 'drainer_fire')),
                    (('timeoutQueue', 'collect'), 'q.mu.Unlock()', ('shard', 'drainer', 'drainer_collect')),
                    (('timeoutQueueRoot', 'fire'), 'root.mu.Unlock()', ('root', 'drainer', 'drainer_root_fire')),
                    (('timeoutQueueRoot', 'fire'), 'q.mu.Unlock()', ('shard', 'drainer', 'drainer_root_fire')),
                    (('timeoutQueueRoot', 'deliverReady'), 'q.mu.Unlock()', ('shard', 'drainer', 'drainer_root_deliverReady'))]
        for method, statement, result in expected:
            with self.subTest(method=method, statement=statement):
                self.assertEqual(q.sample_classification([frame(binding, method, statement)], binding, 'mutex'), result)
                self.assertEqual(q.sample_classification([frame(binding, method, statement.replace('Unlock', 'Lock'))], binding, 'block'), result)
        with self.assertRaises(ValueError):
            q.sample_classification([frame(binding, ('timeoutQueue', 'schedule'), 'q.mu.Unlock()'),
                                     frame(binding, ('timeoutQueueRoot', 'fire'), 'root.mu.Unlock()')], binding, 'mutex')

    def test_generated_timer_wrapper_is_explicit_and_never_a_lock_site(self):
        binding = source()
        wrapper = q.Frame(q.PACKAGE + '(*timeoutQueue).fire-fm', '<autogenerated>', 1)
        self.assertIsNone(q.sample_classification([wrapper], binding, 'mutex'))
        actual = frame(binding, ('timeoutQueue', 'fire'), 'q.mu.Unlock()')
        self.assertEqual(q.sample_classification([actual, wrapper], binding, 'mutex'), ('shard', 'drainer', 'drainer_fire'))
        for bad in (q.Frame(wrapper.function, q.QUEUE_FILE, 1),
                    q.Frame(wrapper.function, '<autogenerated>', 2),
                    q.Frame(q.PACKAGE + '(*timeoutQueue).schedule-fm', '<autogenerated>', 1)):
            with self.subTest(bad=bad), self.assertRaises(ValueError):
                q.sample_classification([bad], binding, 'mutex')

    def test_helpers_inherit_origin_unknown_origin_rejects(self):
        changed = SOURCE.replace(' q.timer.request()', ' q.mu.Lock()\n q.mu.Unlock()')
        binding = source(changed)
        helper = frame(binding, ('timeoutQueue', 'requestLocked'), 'q.mu.Unlock()')
        with self.assertRaises(ValueError):
            q.sample_classification([helper], binding, 'mutex')
        caller = frame(binding, ('timeoutQueue', 'schedule'), 'cancel()')
        drainer = frame(binding, ('timeoutQueue', 'collect'), 'tx.readErrMu.Lock()')
        self.assertEqual(q.sample_classification([helper, caller], binding, 'mutex'), ('shard', 'caller', 'caller_schedule'))
        self.assertEqual(q.sample_classification([helper, drainer], binding, 'mutex'), ('shard', 'drainer', 'drainer_collect'))

    def test_unrelated_locks_readerrmu_and_context_under_queue_are_excluded(self):
        binding = source()
        unrelated = [frame(binding, ('timeoutQueue', 'schedule'), 'other.mu.Lock()'),
                     frame(binding, ('timeoutQueue', 'schedule'), 'cancel()'),
                     frame(binding, ('timeoutQueue', 'collect'), 'tx.readErrMu.Lock()'),
                     frame(binding, ('timeoutQueueRoot', 'deliverReady'), 'tx.readErrMu.Lock()')]
        for item in unrelated:
            for profile in q.PROFILE_KINDS:
                with self.subTest(frame=item, profile=profile):
                    stack = [q.Frame('sync.(*Mutex).Lock', 'sync/mutex.go', 1),
                             q.Frame('context.(*cancelCtx).cancel', 'context/context.go', 1), item]
                    self.assertIsNone(q.sample_classification(stack, binding, profile))
        text = raw([(1, 999999999, [item]) for item in unrelated])
        report = q.summarize(text, binding, 'block', 1000)
        self.assertTrue(report['strict_below_one_percent'])
        self.assertEqual(report['queue_delay_ns'], 0)
        self.assertEqual(report['excluded_non_lock_queue_stack_sample_rows'], 4)


class DecoderGuards(unittest.TestCase):
    def test_columns_old_shape_inline_frames_and_labels(self):
        binding = source()
        item = frame(binding, ('timeoutQueue', 'schedule'), 'q.mu.Unlock()')
        for columns in (True, False):
            for mappings in (True, False):
                with self.subTest(columns=columns, mappings=mappings):
                    text = raw([(3, 17, [item])], columns=columns, mappings=mappings)
                    report = q.summarize(text, binding, 'mutex', 100000)
                    self.assertEqual((report['queue_contentions'], report['queue_delay_ns']), (3, 17))
        text = raw([(2, 27, [item])])
        inline = text.replace('M=1 ' + item.function, 'M=1 sync.(*Mutex).Unlock sync/mutex.go:1:0 s=1\n             ' + item.function)
        inline = inline.replace('Locations', '                label:[value]\nLocations')
        parsed = q.parse_raw(inline)
        self.assertEqual(len(parsed['locations'][1]), 2)
        self.assertEqual(q.summarize(parsed, binding, 'mutex', 100000)['queue_delay_ns'], 27)

    def test_empty_zero_negative_wrong_units_missing_stack_and_raw_syntax_reject(self):
        valid = raw([(1, 5, [normal_frame()])])
        failures = ['', valid.replace('Samples:', 'Sample:'), valid.replace('delay/nanoseconds', 'delay/microseconds'),
                    valid.replace('contentions/count', 'contentions/bytes'), valid.replace('         1          5:', '         0          5:'),
                    valid.replace('         1          5:', '        -1          5:'), valid.replace('         1          5:', '         1         -5:'),
                    valid.replace('         1          5:', '         0          0:'), valid.replace('         1          5:', '         1          0:'),
                    valid.replace(': 1\nLocations', ': 99\nLocations'), valid.replace(': 1\nLocations', ': \nLocations'),
                    valid.replace('runtime._LostContendedRuntimeLock runtime/proc.go:1:0', '??'), valid.replace('Locations', 'nonsense\nLocations'),
                    valid.replace('Mappings', 'Mappings\nMappings')]
        for bad in failures:
            with self.subTest(bad=bad), self.assertRaises(ValueError):
                q.parse_raw(bad)
        duplicate = valid.replace('Mappings', valid.split('Locations\n')[1].split('Mappings')[0] + 'Mappings')
        with self.assertRaises(ValueError):
            q.parse_raw(duplicate)

    def test_zero_queue_baseline_requires_nonempty_profile(self):
        report = q.summarize(raw([(7, 9000, [normal_frame()])]), source(None, 'baseline'), 'mutex', 1000)
        self.assertEqual(report['profile_contentions'], 7)
        self.assertEqual(report['profile_delay_ns'], 9000)
        self.assertEqual(report['queue_delay_ns'], 0)
        self.assertTrue(report['strict_below_one_percent'])
        with self.assertRaises(ValueError):
            q.summarize(raw([(0, 0, [normal_frame()])]), source(None, 'baseline'), 'mutex', 1000)

    def test_combined_shard_root_strict_boundary_and_per_profile_not_added(self):
        binding = source()
        shard = frame(binding, ('timeoutQueue', 'schedule'), 'q.mu.Unlock()')
        root = frame(binding, ('timeoutQueueRoot', 'fire'), 'root.mu.Unlock()')
        for first, second, passes in ((49, 50, True), (50, 50, False), (50, 51, False)):
            report = q.summarize(raw([(2, first, [shard]), (3, second, [root])]), binding, 'mutex', 10000)
            self.assertEqual(report['strict_below_one_percent'], passes)
            self.assertEqual(report['queue_contentions'], 5)
            self.assertEqual(report['by_lock_and_origin']['shard']['caller']['delay_ns'], first)
            self.assertEqual(report['by_lock_and_origin']['root']['drainer']['delay_ns'], second)
        mutex = q.summarize(raw([(1, 75, [shard])]), binding, 'mutex', 10000)
        block_shard = frame(binding, ('timeoutQueue', 'schedule'), 'q.mu.Lock()')
        block = q.summarize(raw([(1, 75, [block_shard])]), binding, 'block', 10000)
        self.assertTrue(mutex['strict_below_one_percent'] and block['strict_below_one_percent'])
        self.assertGreater(mutex['queue_delay_ns'] + block['queue_delay_ns'], 100)
        for cpu in (0, -1, True, 100.0):
            with self.subTest(cpu=cpu), self.assertRaises(ValueError):
                q.summarize(raw([(1, 1, [shard])]), binding, 'mutex', cpu)

    def test_production_only_gate_still_requires_full_baseline_population(self):
        rows = [dict(workload=w, binary=role, profile=profile, queue_delay_ns=99 if role == 'production' else 1000,
                     measured_window_client_cpu_ns=10000, strict_below_one_percent=role == 'production')
                for w in q.WORKLOADS for role in ('baseline', 'production') for profile in q.PROFILE_KINDS]
        decision = q.diagnostic_decision(rows)
        self.assertEqual(decision['verdict'], 'PASS')
        self.assertEqual(decision['gated_production_profiles'], 8)
        self.assertEqual(decision['baseline_comparison_profiles'], 8)
        self.assertFalse(decision['baseline_is_gate'])
        production = next(row for row in rows if row['binary'] == 'production')
        production.update(queue_delay_ns=100, strict_below_one_percent=False)
        self.assertEqual(q.diagnostic_decision(rows)['verdict'], 'FAIL')
        with self.assertRaises(ValueError):
            q.diagnostic_decision(rows[:-1])
        with self.assertRaises(ValueError):
            q.diagnostic_decision(rows[:-1] + [rows[0]])
        production['strict_below_one_percent'] = True
        with self.assertRaises(ValueError):
            q.diagnostic_decision(rows)

    def test_positive_fixture_nonempty_gzip_protobuf_and_exact_expectations(self):
        for profile in q.PROFILE_KINDS:
            with self.subTest(profile=profile):
                encoded, expected = q.positive_fixture(source(), profile)
                self.assertEqual(len(expected), 3)
                self.assertEqual(sum(v['delay_ns'] for v in expected.values()), 36)
                self.assertEqual(sum(v['contentions'] for v in expected.values()), 6)
                self.assertEqual(sum(v['sample_rows'] for v in expected.values()), 3)
                self.assertIn(b'contentions', gzip.decompress(encoded))
                self.assertIn(b'delay', gzip.decompress(encoded))
                self.assertIn(q.QUEUE_FILE.encode(), gzip.decompress(encoded))
                self.assertEqual(encoded, q.positive_fixture(source(), profile)[0])
        for value in (-1, True, 1.0):
            with self.subTest(value=value), self.assertRaises(ValueError):
                q.varint(value)
        self.assertEqual(q.varint(300), b'\xac\x02')


class CounterGuards(unittest.TestCase):
    def test_measured_cpu_fixed_arithmetic_and_bad_counters(self):
        self.assertEqual(q.measured_cpu_ns(summary()), 30000)
        for key in ('process_user_cpu_us', 'process_system_cpu_us'):
            for bad in (-1, True, 1.0, 99):
                item = summary()
                item['stats_after'][key] = bad
                with self.subTest(key=key, bad=bad), self.assertRaises(ValueError):
                    q.measured_cpu_ns(item)
        item = summary()
        item['stats_after'] = dict(item['stats_before'])
        with self.assertRaises(ValueError):
            q.measured_cpu_ns(item)
        item['stats_after'].pop('process_user_cpu_us')
        with self.assertRaises(KeyError):
            q.measured_cpu_ns(item)

    def test_client_counters_population_values_and_bad_delta(self):
        self.assertEqual(q.client_counters(summary())['read_versions_completed'], 500000)
        for change in ('unobserved', 'missing', 'empty', 'negative', 'decreasing', 'float', 'bool', 'delta', 'cache', 'versions', 'batches'):
            item = summary()
            before, after, delta = [item[k]['values'] for k in ('client_counters_before', 'client_counters_after', 'client_counters_delta')]
            if change == 'unobserved':
                item['client_counters_before']['observation'] = 'unobserved'
            elif change == 'missing':
                for values in (before, after, delta):
                    values.pop('grv_cache_hits')
            elif change == 'empty':
                for values in (before, after, delta):
                    values.clear()
            elif change == 'negative':
                before['read_versions_completed'] = -1
            elif change == 'decreasing':
                before['read_versions_completed'] = 600000
            elif change == 'float':
                delta['read_versions_completed'] = 500000.0
            elif change == 'bool':
                delta['grv_cache_hits'] = False
            elif change == 'delta':
                delta['read_versions_completed'] += 1
            elif change == 'cache':
                after['grv_cache_hits'] = delta['grv_cache_hits'] = 1
            elif change == 'versions':
                after['read_versions_completed'] = delta['read_versions_completed'] = 499999
            elif change == 'batches':
                after['grv_successful_batch_replies'] = delta['grv_successful_batch_replies'] = 0
            with self.subTest(change=change), self.assertRaises(ValueError):
                q.client_counters(item)


class EvidenceGuards(unittest.TestCase):
    def test_raw_command_pins_binary_and_archive_no_build_fallback(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            binary, pprof, profile = [root / name for name in ('binary', 'pprof', 'mutex.pprof')]
            for path in (binary, pprof, profile):
                path.write_bytes(path.name.encode())
            output = root / 'decoded'
            def fake_run(command, **kwargs):
                self.assertEqual(command, [str(pprof), '-raw', '-symbolize=none', str(binary), str(profile)])
                self.assertEqual(kwargs['timeout'], 60)
                self.assertEqual(kwargs['stdin'], subprocess.DEVNULL)
                kwargs['stdout'].write(raw([(1, 1, [normal_frame()])]).encode())
                return types.SimpleNamespace(returncode=0)
            with mock.patch.object(q.subprocess, 'run', side_effect=fake_run) as run:
                text = q.run_raw(pprof, q.sha(pprof), binary, q.sha(binary), profile, source(), output)
            self.assertEqual(run.call_count, 1)
            self.assertEqual(q.parse_raw(text)['samples'][0]['delay_ns'], 1)
            receipt = q.read_json(output / 'command.json')
            self.assertEqual(receipt['source_archive_sha256'], PIN)
            self.assertEqual(receipt['binary_sha256'], q.sha(binary))
            for missing in (profile, binary, pprof):
                original = missing.read_bytes()
                missing.unlink()
                with self.subTest(missing=missing), self.assertRaises((FileNotFoundError, ValueError)):
                    q.run_raw(pprof, hashlib.sha256(b'pprof').hexdigest(), binary, hashlib.sha256(b'binary').hexdigest(), profile, source(), root / 'missing')
                missing.write_bytes(original)
            with self.assertRaises(ValueError):
                q.run_raw(pprof, 'b' * 64, binary, q.sha(binary), profile, source(), root / 'badpin')
            profile.write_bytes(b'')
            with self.assertRaises(ValueError):
                q.run_raw(pprof, q.sha(pprof), binary, q.sha(binary), profile, source(), root / 'empty')

    def test_missing_profile_even_if_artifact_census_is_resealed_rejects(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            runs = root / 'runs'
            run = runs / 'd-r000'
            run.mkdir(parents=True)
            row = dict(id='d-r000', epoch=0, profile='mutex')
            collector = types.SimpleNamespace(ENV={'GOMAXPROCS': '4'}, CHILD_LIMIT=120)
            for name in ('manifest.json', 'summary.json', 'samples.csv', 'warmup.csv', 'mutex.pprof'):
                (run / name).write_text('fixture')
            save_json(root / 'status.json', dict(epoch=0, complete=True, input_hashes_verified_after=True, completed=['d-r000']))
            save_json(runs / 'd-r000.command.json', dict(schedule=row, returncode=0, env=collector.ENV, duration_ns=1, timeout_seconds=60))
            def seal():
                save_json(root / 'artifact-sha256.json', {p.relative_to(root).as_posix(): q.sha(p) for p in root.rglob('*') if p.is_file() and p.name != 'artifact-sha256.json'})
                return q.sha(root / 'artifact-sha256.json')
            q.checked_population(root, root, seal(), [row], collector)
            (run / 'mutex.pprof').unlink()
            with self.assertRaisesRegex(ValueError, 'missing required run artifact'):
                q.checked_population(root, root, seal(), [row], collector)
            (run / 'mutex.pprof').write_text('fixture')
            (run / 'block.pprof').write_text('unexpected')
            with self.assertRaisesRegex(ValueError, 'opposite profile'):
                q.checked_population(root, root, seal(), [row], collector)

    def test_posttiming_complete_population_and_chronology(self):
        timing = [dict(started_utc='2020-01-01T00:00:00+00:00', finished_utc='2020-01-01T00:01:00+00:00') for _ in range(256)]
        diagnostics = [dict(started_utc='2020-01-01T00:01:00+00:00', finished_utc='2020-01-01T00:02:00+00:00') for _ in range(16)]
        q.require_posttiming(timing, diagnostics)
        with self.assertRaises(ValueError):
            q.require_posttiming(timing[:-1], diagnostics)
        with self.assertRaises(ValueError):
            q.require_posttiming(timing, diagnostics[:-1])
        early = copy.deepcopy(diagnostics)
        early[0]['started_utc'] = '2020-01-01T00:00:59+00:00'
        with self.assertRaises(ValueError):
            q.require_posttiming(timing, early)
        inverted = copy.deepcopy(diagnostics)
        inverted[0]['finished_utc'] = inverted[0]['started_utc']
        with self.assertRaises(ValueError):
            q.require_posttiming(timing, inverted)
        with self.assertRaises(ValueError):
            q.timestamp('2020-01-01T00:00:00')


def run_tests():
    suite = unittest.defaultTestLoader.loadTestsFromModule(sys.modules[__name__])
    if suite.countTestCases() == 0:
        raise AssertionError('empty queuewait guard population')
    result = unittest.TextTestRunner(verbosity=2).run(suite)
    if not result.wasSuccessful():
        raise AssertionError('queuewait fixture guards failed')
    return result.testsRun
