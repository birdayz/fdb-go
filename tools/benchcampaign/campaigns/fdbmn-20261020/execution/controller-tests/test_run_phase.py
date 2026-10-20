#!/usr/bin/env python3
"""Full controller with an in-memory SSH/systemd model; real subprocesses forbidden."""
import contextlib
import hashlib
import io
import json
from pathlib import Path
import shlex
import subprocess
import sys
import tempfile
import time
import types
import unittest
from unittest.mock import patch

sys.dont_write_bytecode = True
HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE.parent))
import run_phase as rp


def encoded(value):
    return (json.dumps(value, indent=2) + '\n').encode()


class Clock:
    def __init__(self):
        self.now = 0

    def clock(self):
        return self.now

    def sleep(self, seconds):
        self.now += seconds


class FakeTransport:
    def __init__(self, *, lost_runner=False, unknown_runner=False, lost_sampler=None,
                 bad_node_hash=None, bad_receipt=None, changed_r4_pin=False, runner_rc=0,
                 r4_rc=0, testingb=False, empty_r4=False, client_cases=None, sampler_delay_seconds=0):
        self.lost_runner, self.unknown_runner = lost_runner, unknown_runner
        self.lost_sampler, self.bad_node_hash = lost_sampler, bad_node_hash
        self.bad_receipt, self.changed_r4_pin = bad_receipt, changed_r4_pin
        self.runner_rc, self.r4_rc, self.empty_r4 = runner_rc, r4_rc, empty_r4
        self.events, self.commands, self.units, self.files, self.dirs = [], [], {}, {}, set()
        self.client_hashes = 0
        self.sampler_delay_seconds, self.wall_advance = sampler_delay_seconds, lambda _: None
        self.phase = 'M2skip' if testingb else 'M1w1'
        self.expected = ['skip3-rows10-forward'] if testingb else (client_cases or ['grv'])
        self.protocol_path, self.orders_path = rp.R + '/protocol.json', rp.R + '/orders.json'
        self.manifest_path = rp.R + '/plans/' + ('testingb' if testingb else 'client') + '/manifest.json'
        self.template_path = rp.R + '/provenance/p0/placement-command.json'
        proto = {'phases': [{'id': self.phase, 'kind': 'testingb' if testingb else 'client', 'wall_cap_seconds': 30}]}
        self.files['client', self.protocol_path] = encoded(proto)
        self.files['client', self.orders_path] = encoded({'fixture': 'orders'})
        self.files['client', self.template_path] = encoded(['python3', '/never-execute-placement.py'])
        self.pins = {p: 'a' * 64 for p in (rp.T + '/node_sampler.py', rp.T + '/mn_common.py', rp.CF, rp.LAYOUT,
                                           rp.T + '/build_reconcile_input.py', rp.T + '/reconcile_telemetry.py')}
        for p in (self.protocol_path, self.orders_path, self.template_path):
            self.pins[p] = rp.sha(self.files['client', p])
        manifest = {'pinned_files': self.pins}
        if testingb:
            plan = encoded({'cases': [{'key': self.expected[0]}]})
            self.files['client', str(Path(self.manifest_path).parent / (self.phase + '.plan.json'))] = plan
            manifest['phases'] = [{'phase': self.phase, 'cases': 1, 'jobs': 4, 'plan_sha256': rp.sha(plan)}]
        else:
            manifest['campaigns'] = [{'phase': self.phase, 'workload': name, 'run': ['never-execute-R1']}
                                     for name in self.expected]
        self.files['client', self.manifest_path] = encoded(manifest)

    def args(self, diagnostic=False):
        return types.SimpleNamespace(phase=self.phase, label='fresh-' + self.phase,
            manifest=self.manifest_path, manifest_sha256=rp.sha(self.files['client', self.manifest_path]),
            protocol=self.protocol_path, orders=self.orders_path, results=rp.R + '/results/' + self.phase,
            placement_template=self.template_path, diagnostic=diagnostic)

    def receipt(self, role, kind):
        value = {'format': rp.RECEIPT_FORMAT, 'host': role, 'phase': self.phase, 'kind': kind,
                 'sampler_sha256': self.pins[rp.T + '/node_sampler.py'], 'boot_id': 'fake-boot',
                 'realtime_ns': 1, 'monotonic_ns': 1, 'units': {'names': ['fdb-bench-4500.service']},
                 'samples': 5, 'samples_sha256': 'b' * 64}
        if self.bad_receipt and self.bad_receipt[:2] == (role, kind):
            field = self.bad_receipt[2]
            value[field] = 'wrong-' + field
        return value

    def state(self, role, unit):
        item = self.units.get((role, unit))
        if item is None:
            return {'LoadState': 'not-found'}
        if item['runner'] and not item['stopped'] and not self.unknown_runner:
            item['done'] = True
        if item['stopped']:
            active, sub, pid = 'inactive', 'dead', '0'
        elif item['runner'] and item['done']:
            active, sub, pid = ('active', 'exited', '0') if self.runner_rc == 0 else ('failed', 'failed', '0')
        else:
            active, sub, pid = 'active', 'running', '123'
        return {'LoadState': 'loaded', 'ActiveState': active, 'SubState': sub, 'MainPID': pid,
                'ExecMainStatus': str(self.runner_rc if item['runner'] else 0), 'ExecMainCode': '1',
                'Result': 'exit-code' if item['runner'] and self.runner_rc else 'success',
                'ControlGroup': '/system.slice/' + unit}

    def __call__(self, role, command, **kwargs):
        words = shlex.split(command)
        self.commands.append((role, words))

        def reply(rc=0, stdout='', stderr=''):
            if kwargs.get('check') and rc:
                raise subprocess.CalledProcessError(rc, words, output=stdout, stderr=stderr)
            return subprocess.CompletedProcess(words, rc, stdout=stdout, stderr=stderr)

        if rp.HASH_PROGRAM in words:
            expected = json.loads(words[-1])
            if role == 'client':
                self.client_hashes += 1
            self.events.append(('hash', role, self.client_hashes))
            actual, errors = dict(expected), []
            if role == self.bad_node_hash or (role == 'client' and self.changed_r4_pin and self.client_hashes > 1):
                name = rp.T + ('/reconcile_telemetry.py' if role == 'client' else '/node_sampler.py')
                actual[name] = 'c' * 64
                errors.append(name + ': hash mismatch')
            return reply(2 if errors else 0, json.dumps({'actual': actual, 'errors': errors}))
        if rp.CGROUP_PROGRAM in words:
            unit = words[-1].rsplit('/', 1)[-1]
            item = self.units.get((role, unit))
            populated = bool(item and not item['stopped'] and not item['done'])
            return reply(stdout=json.dumps({'exists': bool(item), 'populated': populated, 'pids': [123] if populated else []}))
        if words[:2] == ['systemctl', 'show']:
            unit = words[-1]
            self.events.append(('show', role, unit))
            if role == 'client' and 'runner' in unit and (role, unit) in self.units and self.unknown_runner:
                return reply(255, stderr='fake transport lost')
            return reply(stdout=''.join(k + '=' + v + '\n' for k, v in self.state(role, unit).items()))
        if words[0] == 'systemd-run':
            unit = next(w.split('=', 1)[1] for w in words if w.startswith('--unit='))
            runner = 'runner' in unit
            self.units[role, unit] = {'runner': runner, 'stopped': False, 'done': False}
            self.events.append(('launch', role, unit))
            if not runner:
                self.wall_advance(self.sampler_delay_seconds)
                telemetry = words[words.index('--output-dir') + 1]
                self.files[role, telemetry + '/receipt-start.json'] = encoded(self.receipt(role, 'start'))
            if (runner and self.lost_runner) or (not runner and self.lost_sampler in (role, (role, 'timeout'))):
                if self.lost_runner == 'timeout' or self.lost_sampler == (role, 'timeout'):
                    raise subprocess.TimeoutExpired(words, kwargs.get('timeout'))
                return reply(255, stderr='fake lost acknowledgement after remote launch')
            return reply()
        if words[:2] == ['systemctl', 'stop']:
            unit = words[-1]
            self.events.append(('stop', role, unit))
            if role == 'client' and 'runner' in unit and self.unknown_runner:
                return reply(255, stderr='fake remote state unknown')
            item = self.units.get((role, unit))
            if item is None:
                return reply(5, stderr='unit not loaded')
            item['stopped'] = True
            if not item['runner']:
                telemetry = next(path.removesuffix('/receipt-start.json') for r, path in self.files
                                 if r == role and path.endswith('/receipt-start.json'))
                self.files[role, telemetry + '/receipt-stop.json'] = encoded(self.receipt(role, 'stop'))
            return reply()
        if words[0] == 'cat':
            value = self.files.get((role, words[1]))
            return reply(1, stderr='missing fake file') if value is None else reply(stdout=value.decode())
        if words[:2] == ['test', '-f']:
            return reply(0 if (role, words[2]) in self.files else 1)
        if words[:3] == ['test', '!', '-e']:
            path = words[3]
            if (role, path) in self.dirs:
                return reply(1)
            self.dirs.add((role, path))
            return reply()
        if rp.T + '/node_sampler.py' in words and 'receipt' in words:
            kind, output = words[words.index('--kind') + 1], words[words.index('--output') + 1]
            self.files[role, output] = encoded(self.receipt(role, kind))
            return reply()
        if words[0] == 'journalctl':
            self.events.append(('journal', role))
            return reply()
        if rp.T + '/build_reconcile_input.py' in words:
            self.events.append(('r4-build', role))
            return reply()
        if rp.T + '/reconcile_telemetry.py' in words:
            self.events.append(('r4', role))
            clean = len(self.expected) if self.r4_rc == 0 else len(self.expected) - 1
            report = {'phase': self.phase, 'reconciliation_complete': True,
                      'cases': [] if self.empty_r4 else [{'case': c} for c in self.expected],
                      'phase_summary': {'cases': len(self.expected), 'clean': clean,
                                        'contaminated': len(self.expected) - clean, 'unverifiable': 0}}
            self.files[role, words[words.index('--output') + 1]] = encoded(report)
            return reply(self.r4_rc)
        raise AssertionError('unmodeled fake command: ' + command)


class FakeController(rp.Controller):
    def pull(self, role, remote_dir, local_dir):
        self.transport.events.append(('pull', role, remote_dir))
        local_dir.mkdir(parents=True, exist_ok=False)
        if '/reconcile/' in remote_dir:
            (local_dir / 'report.json').write_bytes(self.transport.files[role, remote_dir + '/report.json'])
        else:
            (local_dir / 'fake-raw.txt').write_text('No command was executed.\n')
        return b'fake archive, never passed to a real tar'


class Tests(unittest.TestCase):
    def exercise(self, fake, diagnostic=False, state=None, wall_epoch=None):
        with tempfile.TemporaryDirectory(dir=HERE) as temp:
            base = Path(temp)
            (base / 'state.json').write_bytes(encoded(state or {'deadline_epoch': int(time.time()) + 36000}))
            clock = Clock()
            fake.wall_advance = clock.sleep
            controller = FakeController(fake.args(diagnostic), base=base, transport=fake,
                                        clock=clock.clock, sleep=clock.sleep)
            wall = contextlib.nullcontext() if wall_epoch is None else patch.object(
                rp.time, 'time', side_effect=lambda: wall_epoch + clock.now)
            with wall, patch.object(subprocess, 'run', side_effect=AssertionError('NO REAL SSH/COMMANDS')), \
                 contextlib.redirect_stdout(io.StringIO()):
                code = controller.execute()
            inputs = controller.folder / 'inputs.json'
            controller.saved_inputs = json.loads(inputs.read_text()) if inputs.is_file() else None
            summary = json.loads((controller.folder / 'complete.json').read_text())
            self.assertEqual(summary['exit_status'], code)
            return code, summary, controller

    def test_clean_client_and_testingb_and_phase_ids(self):
        for testingb in (False, True):
            with self.subTest(testingb=testingb):
                fake = FakeTransport(testingb=testingb)
                code, summary, _ = self.exercise(fake)
                self.assertEqual(code, 0)
                self.assertEqual(summary['outcome'], 'clean')
                self.assertTrue(summary['runner_quiescent'])
                launches = [words for _, words in fake.commands if words[0] == 'systemd-run']
                self.assertEqual(len(launches), 4)
                runner = launches[-1]
                for flag in ('--property=Type=exec', '--property=RemainAfterExit=yes', '--property=RuntimeMaxSec=330s',
                             '--property=KillMode=control-group', '--property=TimeoutStopSec=30s', '--property=CPUAffinity=0-1'):
                    self.assertIn(flag, runner)
                self.assertNotIn('--wait', runner)
                for argv in launches[:3]:
                    self.assertEqual(argv[argv.index('--phase') + 1], fake.phase)
                    self.assertNotEqual(argv[argv.index('--phase') + 1], fake.args().label)
                before_r4 = next(i for i, event in enumerate(fake.events) if event[0] == 'r4-build')
                self.assertIn(('hash', 'client', 2), fake.events[:before_r4])

    def test_operational_deadline_precedes_hard_deadline_and_default_is_backward_compatible(self):
        now, hard, operational = 100000, 120000, 110000
        for testingb in (False, True):
            for explicit in (False, True):
                with self.subTest(testingb=testingb, explicit=explicit):
                    state = {'deadline_epoch': hard}
                    if explicit:
                        state['runner_lifetime_deadline_epoch'] = operational
                    fake = FakeTransport(testingb=testingb)
                    code, _, controller = self.exercise(fake, state=state, wall_epoch=now)
                    self.assertEqual(code, 0)
                    expected = operational if explicit else hard
                    self.assertEqual(controller.deadline, expected)
                    self.assertEqual(controller.saved_inputs['hard_deadline_epoch'], hard)
                    self.assertEqual(controller.saved_inputs['runner_lifetime_deadline_epoch'], expected)
                    runner = next(words for _, words in fake.commands if words[0] == 'systemd-run' and
                                  any(w.startswith('--unit=fdb-mn-runner-') for w in words))
                    command = shlex.split(runner[-1])
                    actual = command[command.index('--lifetime-deadline') + 1] if testingb else command[-1]
                    self.assertEqual(actual, str(expected))

    def test_sampler_delay_cannot_consume_phase_budget_before_runner_launch(self):
        now = 100000
        fake = FakeTransport(sampler_delay_seconds=20)
        state = {'deadline_epoch': now + 20000, 'runner_lifetime_deadline_epoch': now + 950}
        code, summary, _ = self.exercise(fake, state=state, wall_epoch=now)
        self.assertEqual(code, 2)
        self.assertTrue(any('runner launch after sampler readiness: insufficient lifetime' in error
                            for error in summary['errors']))
        self.assertFalse(any(event[0:2] == ('launch', 'client') for event in fake.events))
        self.assertEqual([i['role'] for i in summary['intents']], list(rp.NODES))
        self.assertTrue(all(unit['stopped'] for unit in fake.units.values()))

    def test_complete_seven_case_population_and_empty_r4_refusal(self):
        cases = ['grv', 'get100', 'get1000', 'range100', 'set100', 'batchset50', 'batchset50x1000']
        code, summary, _ = self.exercise(FakeTransport(client_cases=cases))
        self.assertEqual(code, 0)
        self.assertEqual(summary['phase_summary']['cases'], 7)
        code, summary, _ = self.exercise(FakeTransport(client_cases=cases, empty_r4=True))
        self.assertEqual(code, 2)
        self.assertTrue(any('complete expected population' in error for error in summary['errors']))

    def test_lost_runner_ack_stops_exact_unit_before_r3_or_collection(self):
        for failure in (True, 'timeout'):
            with self.subTest(failure=failure):
                fake = FakeTransport(lost_runner=failure)
                code, summary, _ = self.exercise(fake)
                self.assertEqual(code, 2)
                self.assertTrue(summary['runner_quiescent'])
                runner_stop = next(i for i, e in enumerate(fake.events) if e[0:2] == ('stop', 'client'))
                sampler_stop = next(i for i, e in enumerate(fake.events) if e[0] == 'stop' and e[1] in rp.NODES)
                first_pull = next(i for i, e in enumerate(fake.events) if e[0] == 'pull')
                self.assertLess(runner_stop, sampler_stop)
                self.assertLess(runner_stop, first_pull)
                self.assertEqual(len(summary['intents']), 4)

    def test_unknown_runner_leaves_all_r3_bounded_armed_no_collection_or_r4(self):
        fake = FakeTransport(lost_runner=True, unknown_runner=True)
        code, summary, _ = self.exercise(fake)
        self.assertEqual(code, 2)
        self.assertFalse(summary['runner_quiescent'])
        self.assertEqual(summary['outcome'], 'runner-state-unknown-r3-left-bounded-armed')
        self.assertFalse(any(e[0] in ('pull', 'r4-build', 'r4') for e in fake.events))
        self.assertFalse(any(e[0] == 'stop' and e[1] in rp.NODES for e in fake.events))
        for (role, _), state in fake.units.items():
            if role in rp.NODES:
                self.assertFalse(state['stopped'])

    def test_lost_sampler_ack_still_tracks_and_cleans_all_three_intents(self):
        for failure in ('node2', ('node2', 'timeout')):
            with self.subTest(failure=failure):
                fake = FakeTransport(lost_sampler=failure)
                code, summary, _ = self.exercise(fake)
                self.assertEqual(code, 2)
                self.assertEqual([i['role'] for i in summary['intents']], list(rp.NODES))
                stops = {e[1] for e in fake.events if e[0] == 'stop'}
                self.assertTrue(set(rp.NODES) <= stops)
                self.assertFalse(any(e[0:2] == ('launch', 'client') for e in fake.events))
                self.assertTrue(all(v['stopped'] for v in fake.units.values()))

    def test_node_pin_mismatch_refuses_before_r3(self):
        code, summary, _ = self.exercise(fake := FakeTransport(bad_node_hash='node2'))
        self.assertEqual(code, 2)
        self.assertFalse(any(e[0] == 'launch' for e in fake.events))
        self.assertEqual(summary['intents'], [])

    def test_start_receipt_host_phase_and_hash_must_match_seal(self):
        for field in ('host', 'phase', 'sampler_sha256'):
            with self.subTest(field=field):
                fake = FakeTransport(bad_receipt=('node2', 'start', field))
                code, summary, _ = self.exercise(fake)
                self.assertEqual(code, 2)
                self.assertFalse(any(e[0:2] == ('launch', 'client') for e in fake.events))
                self.assertTrue(summary['errors'])

    def test_stop_receipt_host_phase_and_hash_must_match_seal(self):
        for field in ('host', 'phase', 'sampler_sha256'):
            with self.subTest(field=field):
                code, summary, _ = self.exercise(FakeTransport(bad_receipt=('node2', 'stop', field)))
                self.assertEqual(code, 2)
                self.assertTrue(any('node2-stop-receipt' in e for e in summary['errors']))

    def test_changed_r4_pin_blocks_reconciliation(self):
        fake = FakeTransport(changed_r4_pin=True)
        code, summary, _ = self.exercise(fake)
        self.assertEqual(code, 2)
        self.assertFalse(any(e[0] in ('r4-build', 'r4') for e in fake.events))
        self.assertTrue(any('R4' in e for e in summary['errors']))

    def test_known_runner_nonzero_and_r4_omissions_never_returns_success(self):
        fake = FakeTransport(runner_rc=7, r4_rc=3)
        code, summary, _ = self.exercise(fake)
        self.assertEqual(code, 3)
        self.assertEqual(summary['runner_rc'], 7)
        self.assertEqual(summary['reconcile_rc'], 3)
        self.assertEqual(summary['outcome'], 'known-adverse-or-omitted')
        self.assertEqual(summary['errors'], [])

    def test_r4_omissions_with_runner_zero_and_diagnostic_are_nonzero(self):
        for diagnostic, expected in ((False, 3), (True, 1)):
            code, summary, _ = self.exercise(FakeTransport(r4_rc=3), diagnostic)
            self.assertEqual(code, expected)
            self.assertEqual(summary['phase_summary']['cases'], 1)


if __name__ == '__main__':
    unittest.main(verbosity=2)
