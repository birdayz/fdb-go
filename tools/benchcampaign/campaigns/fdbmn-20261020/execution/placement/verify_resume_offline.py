#!/usr/bin/env python3
"""Fake-clock, archived-data, no-subprocess P0 settle/continuation regressions."""
import copy
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import types
import unittest
from unittest.mock import patch

sys.dont_write_bytecode = True
HERE = Path(__file__).absolute().parent
sys.path.insert(0, str(HERE))
import p0
import resume_p0 as resume
C = HERE.parent.parent
A = C.parent.parent / 'hcloud-multinode-vpc-20261014'
sys.path.insert(0, str(C / 'tooling/tools/benchcampaign/multinode'))
import placement_receipt as pr


class Clock:
    def __init__(self):
        self.t = 0

    def now(self):
        return self.t

    def sleep(self, seconds):
        self.t += seconds


def old(name):
    return p0.load(A / 'provenance/p0' / name)


class Settle(p0.P0):
    def __init__(self, statuses):
        self.a = types.SimpleNamespace(settle_timeout_seconds=1200)
        self.cg = types.SimpleNamespace(status_verdict=lambda status, *_: (status['errors'], status['movement'], {}),
                                        role_identity=lambda status: status['role'])
        self.layout, self.statuses, self.calls = {}, statuses, 0

    def cli_exec(self, *_):
        value = self.statuses[min(self.calls, len(self.statuses) - 1)]
        self.calls += 1
        return json.dumps(value)


def sample(errors=(), movement=(), role='stable'):
    return dict(errors=list(errors), movement=list(movement), role=role)


class Checks(unittest.TestCase):
    def test_transient_backlog_missing_movement_resets_healthy_clock(self):
        missing = ['log queue 261181373 not below 100 MB',
                   'moving_data.in_flight_bytes missing or not a number: data movement unverifiable',
                   'moving_data.in_queue_bytes missing or not a number: data movement unverifiable']
        statuses = [sample(missing), sample(movement=['moving_data.in_flight_bytes=5']), sample()]
        statuses += [sample()] * 8 + [sample(['log queue 200000000 not below 100 MB']), sample()]
        clock = Clock()
        with tempfile.TemporaryDirectory(dir=HERE) as tmp, patch.object(subprocess, 'run', side_effect=AssertionError('no subprocess')), \
             patch.object(p0.time, 'monotonic', clock.now), patch.object(p0.time, 'time', clock.now), patch.object(p0.time, 'sleep', clock.sleep):
            runner = Settle(statuses)
            runner.settle(Path(tmp), deadline_epoch=1200)
            result = p0.load(Path(tmp) / 'settled.json')
            self.assertEqual(result['zero_movement_seconds'], 120)
            self.assertEqual(clock.t, 180)
            self.assertEqual(result['deadline_epoch'], 1200)
            self.assertEqual(p0.load(Path(tmp) / 'settle-0000-verdict.json')['errors'], missing)

    def test_real_poll_overhead_does_not_require_nominal_25_samples(self):
        clock = Clock()

        class SlowStatus(Settle):
            def cli_exec(self, *args):
                clock.t += 2
                return super().cli_exec(*args)

        with tempfile.TemporaryDirectory(dir=HERE) as tmp, patch.object(p0.time, 'monotonic', clock.now), \
             patch.object(p0.time, 'time', clock.now), patch.object(p0.time, 'sleep', clock.sleep):
            SlowStatus([sample()]).settle(Path(tmp), deadline_epoch=1200)
            result = p0.load(Path(tmp) / 'settled.json')
            self.assertGreaterEqual(result['zero_movement_seconds'], 120)
            self.assertGreaterEqual(result['polls'], 2)
            self.assertLess(result['polls'], 25)

    def test_original_remaining_budget_never_resets_and_unknown_is_never_zero(self):
        for statuses in ([sample(['moving_data.in_flight_bytes missing or not a number: data movement unverifiable'])],
                         [sample()] * 10 + [sample(['log queue 200000000 not below 100 MB'])] + [sample()]):
            clock = Clock()
            with tempfile.TemporaryDirectory(dir=HERE) as tmp, patch.object(p0.time, 'monotonic', clock.now), \
                 patch.object(p0.time, 'time', clock.now), patch.object(p0.time, 'sleep', clock.sleep):
                with self.assertRaises(p0.Failure):
                    Settle(statuses).settle(Path(tmp), deadline_epoch=150)
                self.assertEqual(clock.t, 150)
                self.assertFalse((Path(tmp) / 'settled.json').exists())

    def test_roles_configuration_dd_recovery_still_abort(self):
        for error in ('check-roles: storage roles changed', 'status storage_engine bad != ssd-2',
                      'data distribution is disabled; M1 needs it on and settled',
                      "recovery_state 'recovering', not fully_recovered"):
            with tempfile.TemporaryDirectory(dir=HERE) as tmp:
                with self.assertRaises(p0.Failure):
                    Settle([sample([error])]).settle(Path(tmp))
        clock = Clock()
        with tempfile.TemporaryDirectory(dir=HERE) as tmp, patch.object(p0.time, 'monotonic', clock.now), \
             patch.object(p0.time, 'time', clock.now), patch.object(p0.time, 'sleep', clock.sleep):
            with self.assertRaises(p0.Failure):
                Settle([sample(), sample(role='changed')]).settle(Path(tmp))

    def test_exact_fixture_and_ballast_data_census_before_clear(self):
        for name in p0.STORES:
            probe = old(f'probe-{name}-a3.json')
            prefix = pr.pack(['benchfetch', name])
            addresses = next(k['addresses'] for k in probe['keys'] if k['kind'] == 'record')
            for key, (kind, id_) in resume.expected_ballast(pr, prefix).items():
                probe['keys'].append(dict(key_hex=key.hex(), kind=kind, id=id_, addresses=addresses))
            fixture = A / f'provenance/p0/fixture-{name}.json'
            result = resume.verify_ballasted_probe(probe, old('status-probe-a3.json'), p0.load(fixture),
                p0.sha(fixture), probe['cluster'], pr, name)
            self.assertEqual(result['ballast_keys'], 2685)
            for defect in ('missing', 'extra', 'diagnostic'):
                bad = copy.deepcopy(probe)
                if defect == 'missing':
                    bad['keys'].pop()
                elif defect == 'extra':
                    bad['keys'].append(dict(bad['keys'][-1], key_hex=(prefix + pr.pack([1, 9999999])).hex()))
                else:
                    bad['dd_mode']['required_off'] = False
                with self.assertRaises(p0.Failure):
                    resume.verify_ballasted_probe(bad, old('status-probe-a3.json'), p0.load(fixture),
                        p0.sha(fixture), probe['cluster'], pr, name)

    def test_resume_boundary_rejects_further_ddoff_clear_or_attempt(self):
        with tempfile.TemporaryDirectory(dir=HERE) as tmp:
            root = Path(tmp)
            for name in ('invocation.json', 'failure.json'):
                p0.dump(root / name, {})
            (root / 'qualification').mkdir()
            (root / 'attempt-1').mkdir()
            runner = resume.Resume.__new__(resume.Resume)
            runner.prior = root
            for name in ('dd-off', 'ballast-clear', 'canonical', 'attempt.json'):
                extra = root / 'attempt-1' / name
                extra.mkdir()
                with self.assertRaises(p0.Failure):
                    runner.validate_boundary({})
                extra.rmdir()
            (root / 'attempt-2').mkdir()
            with self.assertRaises(p0.Failure):
                runner.validate_boundary({})

    def test_original_output_tree_hash_guard(self):
        with tempfile.TemporaryDirectory(dir=HERE) as tmp:
            root = Path(tmp)
            p0.dump(root / 'receipt.json', {'ok': True})
            runner = resume.Resume.__new__(resume.Resume)
            runner.prior, runner.prior_hashes = root, resume.files(root)
            runner.pins = dict(runner.prior_hashes)
            runner.check_pins()
            p0.dump(root / 'extra.json', {'unexpected': True})
            with self.assertRaises(p0.Failure):
                runner.check_pins()

    def test_resume_same_attempt_then_original_conditional_attempts_only(self):
        class Fake(resume.Resume):
            def __init__(self, output, passed):
                self.out, self.prior, self.prior_hashes, self.pins = output, output.parent / 'old', {}, {}
                self.a = types.SimpleNamespace(expect_prior_invocation_sha256='a' * 64,
                    ack_prior_p0_sha256='b' * 64, ack_current_p0_sha256='c' * 64)
                self.settle_deadline, self.original_timeout = 1200, 1200
                self.frozen = False
                self.settle_expected_roles, self.preclear = 'original-role', {'small': 'original-epoch'}
                self.fixtures, self.events, self.passed = {}, [], passed

            def complete_attempt(self, k, mib, a, fixtures, **kwargs):
                self.events.append(('resume-complete', k, mib, kwargs['deadline_epoch']))
                self.assert_no_replay = True
                return self.passed[0], a, fixtures, {}, 'stable'

            def attempt(self, k, mib):
                if self.preclear or hasattr(self, 'settle_expected_roles'):
                    raise AssertionError('attempt1-specific epoch baseline leaked into fresh conditional attempt')
                self.events.append(('conditional-attempt', k, mib))
                return self.passed[k - 1], None, {}, {}, 'stable'

            def finish(self, k, *_):
                self.events.append(('selected', k))
                return 0 if self.passed[k - 1] else 3

        for passed, selected in (((True, True, True), 1), ((False, True, True), 2), ((False, False, False), 3)):
            with tempfile.TemporaryDirectory(dir=HERE) as tmp, patch.object(resume.time, 'time', return_value=0):
                runner = Fake(Path(tmp) / 'continuation', passed)
                self.assertEqual(runner.execute(), 0 if passed[selected - 1] else 3)
                self.assertEqual(runner.events[0], ('resume-complete', 1, 256, 1200))
                self.assertEqual(runner.events[-1], ('selected', selected))
                self.assertEqual(runner.events[1:-1], [('conditional-attempt', k, mib) for k, mib in
                    ((2, 512), (3, 1024)) if k <= selected])
                self.assertTrue(runner.assert_no_replay)
                self.assertFalse((Path(tmp) / 'old').exists())
        with tempfile.TemporaryDirectory(dir=HERE) as tmp, patch.object(resume.time, 'time', return_value=0):
            runner = Fake(Path(tmp) / 'continuation', (False, False, False))
            with patch.object(Fake, 'complete_attempt', side_effect=p0.Failure('integrity defect')):
                with self.assertRaises(p0.Failure):
                    runner.execute()
            self.assertEqual(runner.events, [])
            self.assertFalse((runner.out / 'complete.json').exists())

    def test_frozen_complete_attempt_never_settles_or_toggles_dd(self):
        class Frozen(p0.P0):
            def __init__(self):
                self.events = []

            def settle(self, *_args, **_kwargs):
                raise AssertionError('must not restart settle clock')

            def cli_exec(self, *_args):
                raise AssertionError('must not toggle DD')

            def status(self, *_args):
                self.events.append('status-disabled')
                return {}, []

            def pp_cmd(self, folder, label, command, store, fixture):
                self.events.append(command + '-' + store)
                return None, dict(empty=True, keys_found=0, read_version=7)

            def probe(self, folder, name, store, fixture):
                self.events.append('canonical-' + store)
                return folder / 'canonical.json', old('placement-' + store.split('/')[-1] + '-a3.json'), {}, 'stable'

        with tempfile.TemporaryDirectory(dir=HERE) as tmp:
            folder = Path(tmp)
            for n in p0.STORES:
                (folder / n).mkdir()
            runner = Frozen()
            runner.complete_attempt(1, 256, folder, {n: None for n in p0.STORES}, already_frozen=True,
                deadline_epoch=-1000, before_clear=lambda *_: runner.events.append('read-only-dd-snapshot'))
            expected = ['status-disabled', 'read-only-dd-snapshot']
            for n in p0.STORES:
                expected += ['ballast-clear-benchfetch/' + n, 'ballast-verify-empty-benchfetch/' + n]
            expected += ['canonical-benchfetch/' + n for n in p0.STORES]
            self.assertEqual(runner.events, expected)

    def test_empty_control_snapshot_is_dd_only_with_same_global_epoch(self):
        with tempfile.TemporaryDirectory(dir=HERE) as tmp:
            root = Path(tmp)
            fixture = root / 'qualification/original-small/fixture.json'
            fixture.parent.mkdir(parents=True)
            prefix = pr.pack(['benchfetch', 'control', 'small'])
            p0.dump(fixture, dict(phase='fixture', payload='control-small', prefix_bytes=len(prefix), rows=1000, chunks=1))
            small = old('placement-small-a3.json')
            probe = dict(format=pr.PROBE_FORMAT, store='benchfetch/control-small', store_prefix_hex=prefix.hex(),
                read_version=7, boundaries_read_version=7, boundaries_complete=True, boundaries_hex=[],
                boundary_range_hex=[prefix.hex(), pr.strinc(prefix).hex()],
                scanned_subspaces_hex=[(prefix + pr.pack([1])).hex(), (prefix + pr.pack([2])).hex()],
                keys_complete=True, keys=[], **pr.synthetic_provenance(prefix, fixture_sha256=p0.sha(fixture),
                    cluster=small['cluster'], owner_hex=small['move_keys_owner_hex'], write_token_hex=small['move_keys_write_token_hex']))
            runner = resume.Resume.__new__(resume.Resume)
            runner.prior, runner.pr, runner.identity = root, pr, small['cluster']
            runner.preclear = {'small': dict(owner=small['move_keys_owner_hex'], token=small['move_keys_write_token_hex'])}
            runner.frozen_version_floor, runner.settle_expected_roles = 6, 'stable'
            runner.cg = types.SimpleNamespace(role_identity=lambda _: 'stable')
            runner.status = lambda *_: ({}, [])
            calls = []
            runner.pp_cmd = lambda *a, **kw: (calls.append(a) or (None, probe))
            runner.frozen_snapshot(root, {})
            self.assertEqual(calls[0][2:4], ('probe', 'benchfetch/control-small'))
            self.assertTrue(p0.load(root / 'frozen-dd-snapshot-verified.json')['not_placement'])
            self.assertEqual(runner.frozen_version_floor, 8)
            self.assertEqual(runner.frozen_epoch, (small['move_keys_owner_hex'], small['move_keys_write_token_hex']))
            # Recheck a changed lock before any clear: fatal even though range is empty.
            probe['move_keys_lock']['owner_hex_at_v2'] = 'changed'
            with self.assertRaises(p0.Failure):
                runner.frozen_snapshot(root, {})

    def test_preclear_to_canonical_epoch_and_role_must_match(self):
        receipt = old('placement-small-a3.json')
        runner = resume.Resume.__new__(resume.Resume)
        runner.preclear = {'small': dict(owner=receipt['move_keys_owner_hex'], token=receipt['move_keys_write_token_hex'],
                                        digest=receipt['key_servers_sha256'])}
        runner.settle_expected_roles = 'stable'
        result = (Path('/never-written'), receipt, {}, 'stable')
        with patch.object(p0.P0, 'probe', return_value=result):
            self.assertEqual(runner.probe(None, 'canonical', 'benchfetch/small', None), result)
            runner.preclear['small']['digest'] = 'changed'
            with self.assertRaises(p0.Failure):
                runner.probe(None, 'canonical', 'benchfetch/small', None)


if __name__ == '__main__':
    os.umask(0o077)
    unittest.main(verbosity=2)
