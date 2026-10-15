#!/usr/bin/env python3
"""Tests for bench_runner_mn.py (R2). Temporary fixtures and fakes only: the
benchmark process, fdbcli, the placement probe and run.py's host effects are
all stand-ins; nothing touches FDB, ssh or a cloud."""
import copy
import json
import os
from pathlib import Path
import shutil
import sys
import types
import unittest
from unittest import mock

HERE = Path(os.path.abspath(__file__)).parent
sys.path.insert(0, str(HERE))

import bench_runner_mn as r2  # noqa: E402
import cluster_gate as cg  # noqa: E402
import placement_receipt as pr  # noqa: E402
import seal_multinode as sm  # noqa: E402
from test_client_campaign_mn import FakeClock, Temp, VPC, layout, protocol, sample, status  # noqa: E402

generic = cg.generic
INDEX = ['10.88.0.11:4504', '10.88.0.12:4502']
RECORD = ['10.88.0.13:4502', '10.88.0.11:4505']
STORES = {'benchfetch/small': ('small', 'unsplit'), 'benchfetch/split': ('split', 'split')}


FIXTURE_SHA = 'f' * 64  # synthetic fixture receipt hash recorded by the probe


def cluster_id():
    """Identity of the generated cluster file the tests write (layout()['cluster_file'] + newline)."""
    return pr.cluster_identity((layout()['cluster_file'] + '\n').encode())


def probe(store, record_servers=RECORD):
    name, record_format = STORES[store]
    prefix = pr.pack(['benchfetch', name])
    keys = []
    for i in range(1000):
        keys.append(dict(kind='index', id=i, key_hex=(prefix + pr.pack([2, 'price_idx', i, i])).hex(), addresses=INDEX))
        for suffix in pr.RECORD_SUFFIXES[record_format]:
            keys.append(dict(kind='record', id=i, key_hex=(prefix + pr.pack([1, i, suffix])).hex(), addresses=record_servers))
    return dict(format=pr.PROBE_FORMAT, store=store, store_prefix_hex=prefix.hex(), read_version=7,
                boundaries_read_version=7, boundary_range_hex=[prefix.hex(), pr.strinc(prefix).hex()],
                boundaries_complete=True, boundaries_hex=[(prefix + pr.pack([2])).hex()], keys=keys, keys_complete=True,
                scanned_subspaces_hex=[(prefix + pr.pack([1])).hex(), (prefix + pr.pack([2])).hex()],
                **pr.synthetic_provenance(prefix, fixture_sha256=FIXTURE_SHA, cluster=cluster_id()))


def receipt(store, record_servers=RECORD):
    name, record_format = STORES[store]
    prefix = pr.pack(['benchfetch', name])
    fixture = dict(phase='fixture', prefix_bytes=len(prefix), rows=1000, chunks=len(pr.RECORD_SUFFIXES[record_format]))
    return pr.build_receipt(status(layout(), dd_disabled=True), probe(store, record_servers), pr.PRESETS['benchfetch'],
                            layout='recordlayer', rows=1000, record_format=record_format, fixture=fixture,
                            expect_prefix=prefix, preset='benchfetch', fixture_sha256=FIXTURE_SHA,
                            expect_cluster=cluster_id())


def bench_output(view, rows_override=None):
    """What a passing frozen benchfetch process prints with -test.v, in testing.B's order:
    the fixture receipt, then per repeat the invocation's before/after receipts and its result line."""
    c = view['case']
    rows = rows_override or c['rows']
    fixture = dict(case=view['name'].split('/')[0], phase='fixture', rows=1000, prefix_bytes=20,
                   **{k: v for k, v in c['fields'].items() if k in ('payload', 'payload_bytes', 'chunks')})
    fixture['split'] = c['fields'].get('payload') == 'split'
    lines = ['log FDB_BENCH_AUDIT ' + json.dumps(fixture)]
    remote = view['arm'].endswith('-remote')
    for _ in range(3):
        for phase in ('before', 'after'):
            lines.append('log FDB_BENCH_AUDIT ' + json.dumps(dict(
                case=view['name'], phase=phase, method='USE_REMOTE_FETCH' if remote else 'SCAN_AND_FETCH',
                rows=c['rows'], remote_openings=int(remote), remote_entries=c['rows'] + 4 if remote else 0,
                index_scans=0 if remote else 1, record_loads=0 if remote else c['rows'],
                checksum=42, prefix_bytes=20, **c['fields'])))
        lines.append(f"{view['name']}-8 100 100 ns/op 4 B/op 2 allocs/op {rows} rows/op 20 prefix-B")
    return '\n'.join(lines + ['PASS']) + '\n'


class FakeProcess:
    pid = 4242

    def __init__(self, returncode=0):
        self.returncode = returncode

    def wait(self, timeout=None):
        return self.returncode


class Phase(Temp):
    def setUp(self):
        super().setUp()
        t = self.tmp
        self.protocol_doc = protocol()
        self.lay = layout()
        (t / 'protocol.json').write_text(json.dumps(self.protocol_doc))
        (t / 'orders.json').write_text(json.dumps(sm.make_orders(self.protocol_doc)))
        (t / 'layout.json').write_text(json.dumps(self.lay))
        (t / 'fdb.cluster').write_text(self.lay['cluster_file'] + '\n')
        (t / 'fetch-final').write_bytes(b'\x7fELF stand-in benchfetch binary')
        (t / 'fetch-final').chmod(0o755)
        (t / 'scripts').mkdir()
        (t / 'scripts' / 'run.py').write_text('# stand-in for the frozen run.py\n')
        (t / 'probe.sh').write_text('#!/bin/sh\n# stand-in placement probe\n')
        (t / 'final-src.tar.gz').write_text('source')
        (t / 'source-receipt.json').write_text(json.dumps(dict(
            harness_commit='d' * 40, production_revisions=self.protocol_doc['production_revisions'],
            source_artifacts_sha256={str((t / 'final-src.tar.gz').resolve()): generic.sha(t / 'final-src.tar.gz')})))
        self.receipts = {}
        for store in STORES:
            path = t / f'placement-{store.replace("/", "-")}.json'
            path.write_text(json.dumps(receipt(store)))
            self.receipts[store] = path
        self.pins = [t / 'layout.json', t / 'source-receipt.json', t / 'final-src.tar.gz', t / 'scripts' / 'run.py',
                     t / 'probe.sh', *sorted(cg.code_files(r2.cc, pr, sm, r2))]
        self.seal()
        self.fresh = {}  # (store, when) -> receipt override for the fake probe
        self.statuses = None

    def seal(self, pins=None, out='plans'):
        t = self.tmp
        argv = ['testingb-plans', '--protocol', str(t / 'protocol.json'), '--orders', str(t / 'orders.json'),
                '--cluster-file', str(t / 'fdb.cluster'), '--binary', f'fetch={t / "fetch-final"}',
                *sum([['--placement-receipt', f'{s}={p}'] for s, p in self.receipts.items()], []),
                *sum([['--pin', str(p)] for p in (self.pins if pins is None else pins)], []), '--output-dir', str(t / out)]
        with mock.patch('sys.stdout'):
            sm.main(argv)
        self.plan = json.loads((t / out / 'M2rl.plan.json').read_text())

    def args(self, plans='plans', **over):
        t = self.tmp
        values = dict(manifest=str(t / plans / 'manifest.json'), manifest_sha256=generic.sha(t / plans / 'manifest.json'),
                      phase='M2rl', protocol=str(t / 'protocol.json'), orders=str(t / 'orders.json'),
                      layout=str(t / 'layout.json'), vpc_cidr=VPC, scripts=str(t / 'scripts'),
                      source_receipt=str(t / 'source-receipt.json'),
                      placement_command=json.dumps([str(t / 'probe.sh'), '--store', '{store}', '--when', '{when}',
                                                    '--output', '{output}']),
                      output=str(t / 'results' / 'M2rl'), lifetime_deadline='2099-01-01T00:00:00Z')
        values.update(over)
        return types.SimpleNamespace(**values)

    def command(self, argv, folder, label, timeout):
        self.calls.append(label)
        if argv[0] == 'fdbcli':
            if self.statuses:
                return json.dumps(self.statuses.pop(0) if len(self.statuses) > 1 else self.statuses[0])
            return json.dumps(status(self.lay, dd_disabled=True))
        store, when, output = argv[argv.index('--store') + 1], argv[argv.index('--when') + 1], argv[argv.index('--output') + 1]
        value = self.fresh.get((store, when))
        Path(output).write_text(json.dumps(value if value is not None else json.loads(self.receipts[store].read_text())))
        return ''

    def helper(self):
        clock = iter(range(1, 10 ** 6))

        def snapshot(dest):
            dest.mkdir()
            (dest / 'proc_vmstat').write_text('oom_kill 0\n')
        return types.SimpleNamespace(
            quiescence=lambda policy: dict(passed=True, groups=[g['name'] for g in policy['cpu_sets']]),
            snapshot=snapshot, telemetry=lambda pid: sample(next(clock)), descendants=lambda pid: {pid},
            stop_process_group=lambda p: None, check_builds=lambda: None)

    def spawn(self, command, stdout, stderr, env):
        # Jobs of one case and arm share an argv; the job is the folder GNU time writes into.
        job = self.jobs[Path(command[3]).parent.name]
        self.assertEqual(tuple(command[5:]), tuple(job['command']))
        self.spawned.append(job['id'])
        view = dict(job, case=r2.audit_case(self.cases[job['case']]))
        rc, time_head = self.exit_for(job)
        if rc == 0:
            stdout.write(self.output_for(view))
        Path(command[3]).write_text(time_head + 'User time (seconds): 1.50\nSystem time (seconds): 0.25\n')
        return FakeProcess(rc)

    def exit_for(self, job):
        """(exit status of /usr/bin/time, its -o file's first line) for this job."""
        return 0, ''

    def output_for(self, view):
        return bench_output(view)

    def run_phase(self, args=None, processes=((1, 'systemd'),), utc=cg.utc_now):
        self.calls, self.spawned = [], []
        self.jobs = {j['id']: j for j in self.plan['jobs']}
        self.cases = {c['key']: c for c in self.plan['cases']}
        clock = FakeClock()
        with mock.patch.object(cg, 'process_table', return_value=list(processes)), \
                mock.patch('os.sched_getaffinity', return_value={0, 1}), mock.patch('sys.stdout'):
            return r2.run(args or self.args(), command=self.command, helper=self.helper(), spawn=self.spawn,
                          sleep=clock.sleep, clock=clock, utc=utc)

    def out(self, name):
        return json.loads((self.tmp / 'results' / 'M2rl' / name).read_text())

    def metrics_cases(self):
        lines = (self.tmp / 'results' / 'M2rl' / 'metrics.csv').read_text().splitlines()[1:]
        return [line.split(',')[1] for line in lines]


class RunPhaseTest(Phase):
    def test_complete_phase_with_case_placement_checks(self):
        self.assertEqual(self.run_phase(), 0)
        self.assertEqual(len(self.spawned), 24)
        complete = self.out('complete.json')
        self.assertEqual(complete['samples'], 72)
        self.assertEqual(set(complete['placement_labels'].values()), {'cross-server'})
        # The case key, not a CPU metric name, labels every sample (the single-node key-shadowing fix).
        self.assertEqual(sorted(set(self.metrics_cases())), ['small-rows10-forward', 'split-rows10-forward'])
        for key in ('small-rows10-forward', 'split-rows10-forward'):
            case = self.tmp / 'results' / 'M2rl' / 'cases' / key
            self.assertTrue(json.loads((case / 'compare-sealed-before.json').read_text())['equal'])
            self.assertTrue(json.loads((case / 'compare-before-after.json').read_text())['equal'])
            self.assertTrue((case / 'status-case-before-check-roles.json').is_file())
            self.assertTrue((case / 'status-case-after-check-roles.json').is_file())
            index = json.loads((case / 'case-receipts.json').read_text())
            self.assertEqual(sorted(index), ['check-roles-after.json', 'check-roles-before.json',
                                             'status-after.json', 'status-before.json'])
            for name, digest in index.items():
                self.assertEqual(generic.sha(case / name), digest)
            self.assertTrue(json.loads((case / 'check-roles-after.json').read_text())['passed'])
            self.assertEqual(json.loads((case / 'status-before.json').read_text())['cluster']['generation'], 3)
        # Placement before the first and after the last process of each case, never per arm; no user-key clear.
        self.assertEqual([c for c in self.calls if c.startswith('placement')], ['placement-before', 'placement-after'] * 2)
        self.assertFalse(any('clear' in c for c in self.calls))
        job = self.plan['jobs'][0]
        result = self.out(f'{job["id"]}/result.json')
        self.assertEqual((result['status'], result['process_user_cpu_seconds'], result['placement_label']),
                         ('ok', 1.5, 'cross-server'))
        self.assertEqual(result['run_id'], job['id'])
        self.assertLessEqual(result['started_utc'], result['ended_utc'])
        self.assertTrue(result['started_utc'].endswith('+00:00'))
        self.assertEqual(self.out(f'{job["id"]}/quiescence.json')['groups'], ['housekeeping', 'server', 'client'])

    def test_case_omitted_before_any_process_when_placement_moved_since_sealing(self):
        moved = receipt('benchfetch/small', record_servers=['10.88.0.13:4503', '10.88.0.11:4505'])
        self.fresh[('benchfetch/small', 'before')] = moved
        self.assertEqual(self.run_phase(), 2)
        omitted = self.out('omitted.json')
        self.assertEqual(list(omitted), ['small-rows10-forward'])
        self.assertIn('placement changed', omitted['small-rows10-forward']['reason'])
        self.assertTrue(all(not j.startswith('M2rl-c00') for j in self.spawned))
        self.assertEqual(len(self.spawned), 12)
        self.assertEqual(self.out('INCOMPLETE.json')['omitted'], omitted)

    def test_case_contaminated_when_placement_changes_during_the_case(self):
        moved = receipt('benchfetch/split', record_servers=['10.88.0.12:4503', '10.88.0.11:4505'])
        self.fresh[('benchfetch/split', 'after')] = moved
        self.assertEqual(self.run_phase(), 2)
        omitted = self.out('omitted.json')
        self.assertIn('contaminated after the last process', omitted['split-rows10-forward']['reason'])
        case = self.tmp / 'results' / 'M2rl' / 'cases' / 'split-rows10-forward'
        self.assertFalse(json.loads((case / 'compare-before-after.json').read_text())['equal'])
        self.assertTrue((case / 'status-after.json').is_file())  # taken although placement failed
        # Raw data of the contaminated case is retained, never deleted.
        self.assertEqual(self.metrics_cases().count('split-rows10-forward'), 36)

    def test_fresh_receipts_need_integrity_and_the_right_store(self):
        broken = receipt('benchfetch/small')
        broken.update(integrity_ok=False, error_count=1, errors=['1 canonical fixture key(s) missing'])
        for name, value in [('no integrity', broken), ('other store', receipt('benchfetch/split'))]:
            with self.subTest(name):
                self.setUp()
                self.fresh[('benchfetch/small', 'before')] = value
                self.assertEqual(self.run_phase(), 2)
                self.assertEqual(list(self.out('omitted.json')), ['small-rows10-forward'])

    def test_fresh_receipt_must_pass_sealed_errors_even_when_it_compares_equal(self):
        stripped = receipt('benchfetch/small')
        del stripped['provenance']  # placement, boundaries and identity fields unchanged: compare says equal
        self.assertTrue(pr.compare(receipt('benchfetch/small'), stripped)['equal'])
        self.fresh[('benchfetch/small', 'before')] = stripped
        self.assertEqual(self.run_phase(), 2)
        omitted = self.out('omitted.json')
        self.assertEqual(list(omitted), ['small-rows10-forward'])
        self.assertIn('not sealable', omitted['small-rows10-forward']['reason'])
        other = receipt('benchfetch/small')
        other['provenance']['cluster']['connection_string_id'] = 'fdbbenchvpc:other'
        self.assertTrue(pr.sealed_errors(other, cluster_id()))

    def test_server_gate_omits_cases(self):
        lay = self.lay
        cases = [
            ('DD enabled (placement not frozen)', [status(lay)], {'small-rows10-forward', 'split-rows10-forward'}),
            # Only the case the recovery happened in; the next case runs wholly after it, on a new baseline.
            ('recovery during the first case', [status(lay, dd_disabled=True)] * 6 + [status(lay, dd_disabled=True, generation=8)],
             {'small-rows10-forward'}),
        ]
        for name, statuses, want in cases:
            with self.subTest(name):
                self.setUp()
                self.statuses = list(statuses)
                self.assertEqual(self.run_phase(), 2)
                omitted = self.out('omitted.json')
                self.assertEqual(set(omitted), want)
                at = {o['at'] for o in omitted.values()}
                errors = [self.out(f'{j}/result.json')['error'] for j in at if (self.tmp / 'results' / 'M2rl' / j).is_dir()]
                self.assertRegex(json.dumps(omitted) + json.dumps(errors), 'data distribution is enabled|generation')

    def test_fdbserver_on_the_client_aborts_the_arm(self):
        self.assertEqual(self.run_phase(processes=[(1, 'systemd'), (99, 'fdbserver')]), 2)
        first = self.out(f'{self.plan["jobs"][0]["id"]}/result.json')
        self.assertEqual(first['status'], 'environment-aborted')
        self.assertIn('fdbserver', first['error'])

    def test_signal_killed_process_is_an_environmental_case_abort(self):
        # GNU time exits 128+9 for a SIGKILLed child and says so only in its -o file; stderr stays empty.
        first = self.plan['jobs'][0]['id']
        self.exit_for = lambda job: (137, 'Command terminated by signal 9\n') if job['id'] == first else (0, '')
        self.assertEqual(self.run_phase(), 2)  # not CorrectnessFailure: the phase continued
        result = self.out(f'{first}/result.json')
        self.assertEqual(result['status'], 'environment-aborted')
        self.assertIn('exit 137', result['error'])
        self.assertEqual(list(self.out('omitted.json')), ['small-rows10-forward'])
        self.assertEqual(len(self.spawned), 13)  # the rest of the small case skipped, split ran whole

    def test_environmental_exit_rule(self):
        cases = [((137, '', 'Command terminated by signal 9\n'), True), ((143, '', ''), True), ((-9, '', ''), True),
                 ((1, 'rpc: timed out', ''), True), ((1, '', 'Command terminated by signal 6\n'), True),
                 ((1, 'FAIL: wrong rows', 'Command exited with non-zero status 1\n'), False), ((2, '', ''), False)]
        for (rc, stderr, time_text), want in cases:
            with self.subTest(rc=rc, stderr=stderr):
                self.assertEqual(r2.environmental_exit(rc, stderr, time_text), want)

    def test_final_tree_correctness_failure_stops_the_phase(self):
        self.output_for = lambda view: bench_output(view, rows_override=7)
        with self.assertRaises(cg.CorrectnessFailure):
            self.run_phase()
        self.assertIn('final correctness-failed', json.dumps(self.out('ABORTED.json')))
        self.assertEqual(len(self.spawned), 1)

    def test_lifetime_gate(self):
        import time
        deadline = str(int(time.time()) + 6300 + 600 + 299)
        with self.assertRaisesRegex(cg.EnvironmentFailure, 'lifetime'):
            self.run_phase(self.args(lifetime_deadline=deadline))
        self.assertEqual(self.out('UNEXECUTED.json')['required_seconds'], 6300 + 900)
        self.assertEqual(self.spawned, [])


class PreflightTest(Phase):
    def refused(self, args, text):
        with self.assertRaisesRegex(Exception, text):
            self.run_phase(args)
        self.assertFalse((self.tmp / 'results' / 'M2rl').exists())

    def test_manifest_and_pins(self):
        self.refused(self.args(manifest_sha256='0' * 64), 'acknowledged')
        self.seal(pins=[p for p in self.pins if p != self.tmp / 'probe.sh'], out='no-probe-pin')
        self.refused(self.args(plans='no-probe-pin'), 'must be pinned')
        self.seal(pins=[p for p in self.pins if 'bench_runner_mn' not in str(p)], out='no-code-pin')
        self.refused(self.args(plans='no-code-pin'), 'must be pinned')

    def test_sealed_receipt_change_is_refused(self):
        changed = json.loads(self.receipts['benchfetch/small'].read_text())
        changed['read_version'] = 8
        self.receipts['benchfetch/small'].write_text(json.dumps(changed))
        # The edited receipt no longer re-validates (its preserved provenance is at
        # read version 7), so the plan rebuild refuses it before the hash check.
        self.refused(self.args(), 'pinned|differs|proves nothing')

    def test_tampered_plan_with_a_consistent_manifest_is_refused(self):
        plans = self.tmp / 'plans'
        plan = json.loads((plans / 'M2rl.plan.json').read_text())
        for name, edit in [('argv', lambda p: p['jobs'][0]['command'].append('-test.short')),
                           ('label', lambda p: p['cases'][0].update(placement_label='cross-server (edited)')),
                           ('order', lambda p: p['jobs'].insert(0, p['jobs'].pop(1)))]:
            with self.subTest(name):
                forged = self.tmp / f'forged-{name}'
                shutil.copytree(plans, forged)
                tampered = copy.deepcopy(plan)
                edit(tampered)
                (forged / 'M2rl.plan.json').write_text(json.dumps(tampered))
                manifest = json.loads((forged / 'manifest.json').read_text())
                manifest['phases'][0]['plan_sha256'] = generic.sha(forged / 'M2rl.plan.json')
                (forged / 'manifest.json').write_text(json.dumps(manifest))
                self.refused(self.args(plans=f'forged-{name}'), 'plan differs|case-major')

    def test_manifest_population_must_be_protocol_minus_recorded_unsealed(self):
        plans = self.tmp / 'plans'
        manifest = json.loads((plans / 'manifest.json').read_text())
        for name, unsealed in [('frozen case passed off as unsealed', dict(phase='M2rl', case='split-rows10-forward')),
                               ('unknown case unsealed', dict(phase='M2rl', case='small-rows100-forward'))]:
            with self.subTest(name):
                forged = json.loads(json.dumps(manifest))
                forged['unsealed'].append(dict(unsealed, reason='forged'))
                (plans / 'manifest.json').write_text(json.dumps(forged))
                self.refused(self.args(manifest_sha256=generic.sha(plans / 'manifest.json')), 'protocol cases minus')

    def test_placement_command_needs_the_sealed_cluster_sha256(self):
        t = self.tmp
        command = t / 'placement_command.py'
        command.write_text('# stand-in for placement_command.py\n')
        self.pins.append(command)
        self.seal(out='plans-pc')
        cluster_sha = generic.sha(t / 'fdb.cluster')
        base = [str(command), '--store', '{store}', '--when', '{when}', '--output', '{output}']
        for name, extra in [('flag missing', []), ('wrong SHA-256', ['--expect-cluster-sha256', '0' * 64]),
                            ('value missing', ['--expect-cluster-sha256'])]:
            with self.subTest(name):
                self.refused(self.args(plans='plans-pc', placement_command=json.dumps(base + extra)),
                             'expect-cluster-sha256')
        good = base + ['--expect-cluster-sha256', cluster_sha]
        self.assertEqual(self.run_phase(self.args(plans='plans-pc', placement_command=json.dumps(good))), 0)

    def test_cluster_file_and_vpc(self):
        with self.assertRaisesRegex(cg.EnvironmentFailure, 'VPC'):
            self.run_phase(self.args(vpc_cidr='10.99.0.0/24'))
        self.assertTrue(self.out('ABORTED.json'))

    def test_sealed_receipt_rechecked_at_each_case(self):
        case = dict(self.plan['cases'][0])
        pinned = json.loads((self.tmp / 'plans' / 'manifest.json').read_text())['pinned_files']
        gate = cg.ServerGate(self.tmp / 'fdb.cluster', self.lay, 'ssd-2', 'disabled', self.command)
        self.calls = []
        unit = r2.Case(dict(case, placement_receipt_sha256='0' * 64), self.tmp / 'case-x', gate, self.command, [], '', pinned,
                       cluster_id())
        with self.assertRaisesRegex(cg.EnvironmentFailure, 'changed since sealing'):
            unit.begin()
        self.assertEqual(self.calls, [])


class AuditCaseTest(unittest.TestCase):
    def test_audit_expectations_match_the_single_node_contract(self):
        """Overlapping stems get exactly the single-node audit expectations (key aside)."""
        single = {c['stem']: c for phase in ('C1', 'C2', 'D2') for c in generic.cases_for(phase)}
        p = protocol()
        phases = [dict(p['phases'][1], payloads=['small', 'split'], cells=['limit1', 'rows10', 'rows100', 'rows1000'],
                       dirs=['forward', 'reverse']),
                  dict(id='M2skip', binary_kind='planskip', cells=['rows10', 'rows100', 'rows1000'], dirs=['forward', 'reverse']),
                  dict(id='M2sql', binary_kind='sqlfetch', cells=['rows10', 'rows100', 'rows1000'])]
        matched = 0
        for phase in phases:
            for case in sm.testingb_cells(phase):
                mine = r2.audit_case(case)
                if case['stem'] in single:
                    want = dict(single[case['stem']], key=mine['key'])
                    self.assertEqual(mine, want, case['stem'])
                    matched += 1
        self.assertEqual(matched, 12)  # C1: 6 forward fetch; C2: 2 reverse fetch, 2 forward Skip; D2: 2 SQL
        skip100 = r2.audit_case(next(c for c in sm.testingb_cells(phases[1]) if c['key'] == 'skip3-rows100-reverse'))
        self.assertEqual((skip100['rows'], skip100['fields']['reverse']), (100, True))


if __name__ == '__main__':
    unittest.main()
