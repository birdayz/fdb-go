#!/usr/bin/env python3
"""Recover terminal cleanup evidence after its separate receipt publication fails.

Real guard_loop, public teardown, private journals and completion are used.
Only provider/process/clock boundaries and a targeted receipt filesystem fault
are synthetic; no authorization or Journal/Campaign method is replaced.
"""
import contextlib
import copy
import io
import json
import os
from pathlib import Path
import stat
import sys
import unittest
from unittest.mock import patch

HERE = Path(os.path.abspath(__file__)).parent
sys.path.insert(0, str(HERE))

import campaign_cloud as cc  # noqa: E402
import campaign_safety as cs  # noqa: E402
import creation_ledger as cl  # noqa: E402
import mn_common as mc  # noqa: E402
import teardown_guard as tg  # noqa: E402
from test_campaign_safety import Fixture, NS  # noqa: E402

REAL_CAMPAIGN = cs.Campaign
RESULT_KEYS = ('complete', 'baseline_equal', 'owned_survivors', 'subordinate_survivors',
               'pending_intents', 'hard_lifetime_met')


class ReceiptFault:
    """Fail one actual cleanup receipt write/replace before replacing its old bytes."""
    def __init__(self, test, fixture, operation, prior):
        self.test, self.f, self.operation, self.prior = test, fixture, operation, prior
        self.path = fixture.guard_directory / 'cleanup.json'
        self.attempts = []
        self.stack = contextlib.ExitStack()

    def attempt(self):
        captured = self.test.snapshot(self.f)
        self.test.assertEqual(captured['state']['mode'], 'COMPLETE',
                              'fault must occur after actual authenticated terminal publication')
        self.test.assertEqual(self.test.receipt_bytes(self.f), self.prior)
        if self.attempts:
            self.test.assertEqual(captured, self.attempts[0],
                                  'receipt retry must not rearm, republish journal state or read/mutate the provider')
        self.attempts.append(captured)
        if len(self.attempts) == 1:
            raise OSError('synthetic cleanup receipt ' + self.operation + ' failed before replace')

    def __enter__(self):
        if self.operation == 'rename':
            replace = os.replace
            def failing_replace(source, destination, *args, **kwargs):
                if Path(destination) == self.path:
                    self.attempt()
                return replace(source, destination, *args, **kwargs)
            self.stack.enter_context(patch.object(os, 'replace', side_effect=failing_replace))
        else:
            fdopen, fault = os.fdopen, self
            class Writer:
                def __init__(self, stream):
                    self.stream = stream
                def __enter__(self):
                    self.stream.__enter__()
                    return self
                def __exit__(self, *args):
                    return self.stream.__exit__(*args)
                def write(self, data):
                    fault.attempt()
                    return self.stream.write(data)
                def flush(self):
                    return self.stream.flush()
            def failing_fdopen(fd, mode, *args, **kwargs):
                caller = sys._getframe(1)
                while caller is not None and not (caller.f_code.co_name == 'atomic_private'
                        and Path(caller.f_code.co_filename).name == 'campaign_safety.py'):
                    caller = caller.f_back
                selected = (caller is not None and caller.f_locals.get('path') == self.path and mode == 'wb')
                stream = fdopen(fd, mode, *args, **kwargs)
                return Writer(stream) if selected else stream
            self.stack.enter_context(patch.object(os, 'fdopen', side_effect=failing_fdopen))
        return self

    def __exit__(self, *args):
        return self.stack.__exit__(*args)


class CampaignTerminalTest(unittest.TestCase):
    def setUp(self):
        self.stack = contextlib.ExitStack()
        self.addCleanup(self.stack.close)
        self.stack.enter_context(patch.dict(os.environ, {}, clear=True))
        self.stack.enter_context(patch.object(cc.http.client.HTTPSConnection, 'connect',
            side_effect=AssertionError('terminal controls must never use a real network')))
        self.stack.enter_context(patch.object(cs.os, 'pidfd_open', create=True,
            side_effect=AssertionError('terminal controls must never open a real pidfd')))
        self.stack.enter_context(patch.object(cs.signal, 'pidfd_send_signal', create=True,
            side_effect=AssertionError('terminal controls must never signal a real process')))
        self.stack.enter_context(patch.object(cs.os, 'kill',
            side_effect=AssertionError('terminal controls must never use a bare PID signal')))

    def receipt_bytes(self, f):
        path = f.guard_directory / 'cleanup.json'
        return path.read_bytes() if path.exists() else None

    def snapshot(self, f):
        with contextlib.closing(cl.Journal(f.journal.path)) as journal:
            state = journal.read()
            checkpoint = copy.deepcopy(journal.checkpoint)
        guard_files = {}
        for name in ('stop.json', 'heartbeat.json'):
            path = f.guard_directory / name
            try:
                info = path.lstat()
            except FileNotFoundError:
                guard_files[name] = dict(exists=False)
            else:
                self.assertTrue(stat.S_ISREG(info.st_mode))
                guard_files[name] = dict(exists=True, data=path.read_bytes(), mode=info.st_mode,
                    uid=info.st_uid, gid=info.st_gid, device=info.st_dev, inode=info.st_ino,
                    nlink=info.st_nlink, size=info.st_size,
                    mtime_ns=info.st_mtime_ns, ctime_ns=info.st_ctime_ns)
        return dict(state=state, checkpoint=checkpoint, head=f.journal.path.read_bytes(),
            requests=copy.deepcopy(f.cloud.requests), lists=copy.deepcopy(f.cloud.lists),
            describes=copy.deepcopy(f.cloud.describes), actions=copy.deepcopy(f.cloud.action_reads),
            guard_files=guard_files)

    def incidents(self, f):
        result = {}
        for path in f.guard_directory.glob('incident-*.json'):
            info = path.lstat()
            self.assertTrue(stat.S_ISREG(info.st_mode))
            self.assertEqual(stat.S_IMODE(info.st_mode), 0o600)
            self.assertEqual((info.st_uid, info.st_nlink), (os.geteuid(), 1))
            result[path.name] = cc.private_bytes(path)
        return result

    def terminal_event(self, state):
        checks = [event for event in state['events'] if event.get('event') == 'completion-check']
        self.assertTrue(checks, 'terminal result must come from a real committed completion-check')
        self.assertEqual(state['mode'], 'COMPLETE')
        return {key: checks[-1][key] for key in RESULT_KEYS}

    def prepare(self, f, hard_met, stale):
        self.assertEqual(f.create(), 201)
        self.assertTrue(f.campaign.poll_actions())
        f.closing()
        if not hard_met:
            f.wall = f.header['time']['h_ns'] + NS
        if stale:
            cs.atomic_private(f.guard_directory / 'cleanup.json',
                dict(complete=False, hard_lifetime_met=False, status='older-pending-result'))
        return self.receipt_bytes(f)

    def new_actor(self, f, pid):
        f.processes.pop(111, None)
        f.processes.pop(112, None)
        f.processes[pid] = dict(pid=pid, parent=110, starttime=str(pid * 10),
                                argv_sha256=cc.digest(str(pid).encode()))
        f.actor = pid

    def assert_receipt(self, f, result, hard_met):
        self.assertTrue(result['complete'])
        self.assertTrue(result['baseline_equal'])
        self.assertEqual(result['owned_survivors'], [])
        self.assertEqual(result['subordinate_survivors'], 0)
        self.assertEqual(result['pending_intents'], [])
        self.assertIs(result['hard_lifetime_met'], hard_met)
        path = f.guard_directory / 'cleanup.json'
        self.assertEqual(json.loads(cc.private_bytes(path)), result)
        self.assertEqual(stat.S_IMODE(path.stat().st_mode), 0o600)
        self.assertEqual(f.deletes(), [('DELETE', '/networks/201')])
        self.assertEqual(len(f.cloud.requests), 2, 'one create and one DELETE only')
        for kind, baseline in f.header['baseline'].items():
            self.assertEqual(sorted(f.cloud.rows[kind]), baseline)

    def test_real_guard_retries_failed_terminal_receipt_without_rearming_or_repeating_cleanup(self):
        for operation in ('write', 'rename'):
            for stale in (False, True):
                for hard_met in (True, False):
                    with self.subTest(operation=operation, stale=stale, hard_met=hard_met), Fixture() as f:
                        prior = self.prepare(f, hard_met, stale)
                        self.new_actor(f, 113)
                        sleeps, retained_incidents = [], []
                        prior_incidents = self.incidents(f)
                        with ReceiptFault(self, f, operation, prior) as fault:
                            def sleep(seconds):
                                sleeps.append(seconds)
                                self.assertLessEqual(len(sleeps), 2,
                                    'guard spun in COMPLETE instead of retrying terminal receipt publication')
                                self.assertEqual(seconds, 1)
                                self.assertEqual(len(fault.attempts), 1, 'real terminal publication fault was not reached')
                                self.assertEqual(self.snapshot(f), fault.attempts[0])
                                self.assertEqual(self.receipt_bytes(f), prior)
                                published = self.incidents(f)
                                newly_published = set(published) - set(prior_incidents)
                                self.assertEqual(len(newly_published), 1,
                                    'terminal receipt failure must publish exactly one new private incident')
                                self.assertEqual(set(published), set(prior_incidents) | newly_published)
                                incident = json.loads(published[next(iter(newly_published))])
                                self.assertIs(incident['complete'], False)
                                self.assertIs(incident['hard_lifetime_met'], False)
                                self.assertEqual(incident['error_type'], 'OSError')
                                self.assertIs(type(incident['unix_ns']), int)
                                retained_incidents.append(published)
                                f.mono += NS
                            result = f.campaign.guard_loop(sleep=sleep, poll_seconds=1, cleanup_interval_seconds=1)
                        self.assertEqual(len(fault.attempts), 2, 'one failed and one successful terminal receipt attempt')
                        self.assertEqual(sleeps, [1])
                        self.assertEqual(len(retained_incidents), 1)
                        self.assertEqual(self.incidents(f), retained_incidents[0],
                            'successful terminal receipt retry must retain the exact failure incident')
                        self.assertEqual(self.snapshot(f), fault.attempts[0])
                        self.assertEqual(result, self.terminal_event(fault.attempts[0]['state']))
                        self.assertEqual(fault.attempts[0]['state']['guard']['identity']['pid'], 113)
                        self.assert_receipt(f, result, hard_met)

    @contextlib.contextmanager
    def public_boundaries(self, f, constructed):
        def provider(profile, binding, **kwargs):
            self.assertEqual(profile.binding, f.header['context'])
            self.assertEqual(binding, f.header['context'])
            return f.cloud
        def clocked(journal, cloud, **kwargs):
            campaign = REAL_CAMPAIGN(journal, cloud, clock=lambda: f.wall,
                                     monotonic=lambda: f.mono, **kwargs)
            constructed.append(campaign)
            return campaign
        # Factories inject provider/clock boundaries only; all constructed
        # Profile, Campaign and Journal implementations remain real.
        with patch.object(cc, 'Cloud', side_effect=provider), patch.object(cs, 'Campaign', side_effect=clocked):
            yield

    def test_public_normal_and_immediate_restart_reconstruct_complete_before_receipt_without_rearm(self):
        for immediate in (False, True):
            for stale in (False, True):
                for hard_met in (True, False):
                    with self.subTest(immediate=immediate, stale=stale, hard_met=hard_met), Fixture() as f:
                        prior = self.prepare(f, hard_met, stale)
                        # Stop at the real COMPLETE-before-receipt crash window:
                        # cleanup_pass commits completion but does not write cleanup.json.
                        committed = f.campaign.cleanup_pass()
                        frozen = self.snapshot(f)
                        self.assertEqual(committed, self.terminal_event(frozen['state']))
                        self.assertIs(committed['hard_lifetime_met'], hard_met)
                        self.assertEqual(self.receipt_bytes(f), prior)
                        self.assertEqual(frozen['state']['guard']['identity']['pid'], 112)
                        self.new_actor(f, 114)
                        # Invert the current wall-clock fact. Restart must report
                        # the authenticated completion fact, not recompute it now.
                        hard = f.header['time']['h_ns']
                        f.wall = hard + 120 * NS if hard_met else hard - 120 * NS
                        fixed_origin = f.header['time']['t0_ns'] + f.mono - f.header['time']['monotonic_t0_ns']
                        self.assertLess(fixed_origin, hard)
                        extra = ['--immediate'] if immediate else []
                        args = tg.parse_args(['--label', 'campaign=synthetic', '--prefix', 'test',
                            '--creation-ledger', str(f.journal.path), '--execute', *extra])
                        constructed, output = [], io.StringIO()
                        def no_wait(seconds):
                            self.fail('durable COMPLETE restart must not wait or reenter guard cleanup')
                        with self.public_boundaries(f, constructed):
                            ok, receipt = tg.run(args, sleep=no_wait, out=output)
                        self.assertEqual(len(constructed), 1)
                        self.assertIsNot(constructed[0], f.campaign)
                        self.assertIsNot(constructed[0].journal, f.journal)
                        self.assertIs(ok, hard_met)
                        self.assertEqual(receipt['result'], committed)
                        self.assertEqual(receipt['journal'], f.journal.binding)
                        self.assertEqual(json.loads(output.getvalue()), committed)
                        self.assertEqual(self.snapshot(f), frozen,
                            'terminal restart must not rearm, reopen, publish journal state or access the provider')
                        self.assert_receipt(f, receipt['result'], hard_met)

    def test_public_restart_refuses_invalid_authenticated_complete_evidence_without_side_effects(self):
        faults = [('missing-event', None)]
        faults += [('missing-field', field) for field in RESULT_KEYS]
        faults += [('malformed-field', field) for field in RESULT_KEYS]
        faults += [(fault, None) for fault in ('timestamp-type', 'timestamp-before-t0',
                    'pending-intent', 'unidentified-intent', 'conflicting-h-flag', 'conflicting-h-time')]
        malformed = dict(complete=False, baseline_equal=False, owned_survivors=[{'kind': 'network', 'id': 201}],
                         subordinate_survivors=True, pending_intents=[0], hard_lifetime_met=1)
        for immediate in (False, True):
            for fault, field in faults:
                with self.subTest(immediate=immediate, fault=fault, field=field), Fixture() as f:
                    prior = self.prepare(f, hard_met=True, stale=True)
                    valid = f.campaign.cleanup_pass()
                    self.assertTrue(valid['complete'])
                    self.assertTrue(valid['hard_lifetime_met'])
                    # Authenticate the inconsistent fixture with the real writer;
                    # no Journal reader/validator or terminal-proof method is replaced.
                    with f.journal.locked() as state:
                        self.assertEqual(state['mode'], 'COMPLETE')
                        good = copy.deepcopy([event for event in state['events']
                                              if event.get('event') == 'completion-check'][-1])
                        if fault == 'missing-event':
                            state['events'] = [event for event in state['events']
                                               if event.get('event') != 'completion-check']
                            message = 'completed campaign lacks authenticated completion evidence'
                        else:
                            # Keep the valid earlier receipt. Only the latest one
                            # is authoritative, so falling back to the old one must fail.
                            latest = copy.deepcopy(good)
                            state['events'].append(latest)
                            message = 'invalid authenticated completion evidence'
                            if fault == 'missing-field':
                                latest.pop(field)
                            elif fault == 'malformed-field':
                                latest[field] = copy.deepcopy(malformed[field])
                            elif fault == 'timestamp-type':
                                latest['unix_ns'] = str(latest['unix_ns'])
                            elif fault == 'timestamp-before-t0':
                                latest['unix_ns'] = f.header['time']['t0_ns'] - 1
                            elif fault == 'pending-intent':
                                state['intents'][0]['state'] = 'ACKNOWLEDGED'
                            elif fault == 'unidentified-intent':
                                state['intents'][0]['required_action_identified'] = False
                            elif fault == 'conflicting-h-flag':
                                state['hard_lifetime_breached'] = True
                                message = 'terminal lifetime evidence conflicts'
                            elif fault == 'conflicting-h-time':
                                latest['unix_ns'] = f.header['time']['h_ns'] + NS
                                message = 'terminal lifetime evidence conflicts'
                        f.journal.commit(state)
                    frozen = self.snapshot(f)  # Includes an independent strict authenticated reopen.
                    self.assertEqual(frozen['state']['mode'], 'COMPLETE')
                    self.assertEqual(self.receipt_bytes(f), prior)
                    self.new_actor(f, 114)
                    extra = ['--immediate'] if immediate else []
                    args = tg.parse_args(['--label', 'campaign=synthetic', '--prefix', 'test',
                        '--creation-ledger', str(f.journal.path), '--execute', *extra])
                    constructed, output = [], io.StringIO()
                    incidents = self.incidents(f)
                    def no_wait(seconds):
                        self.fail('invalid terminal proof must refuse, not wait/rearm/retry cleanup')
                    with self.public_boundaries(f, constructed):
                        with self.assertRaisesRegex(mc.GuardError, '^' + message + '$'):
                            tg.run(args, sleep=no_wait, out=output)
                    self.assertEqual(len(constructed), 1)
                    self.assertEqual(output.getvalue(), '')
                    self.assertEqual(self.receipt_bytes(f), prior,
                                     'invalid authenticated terminal proof must not replace the old failed receipt')
                    self.assertEqual(self.snapshot(f), frozen,
                                     'refusal must not rearm, reopen, publish journal state or access the provider')
                    self.assertEqual(self.incidents(f), incidents)
                    self.assertEqual(f.deletes(), [('DELETE', '/networks/201')])
                    self.assertEqual(len(f.cloud.requests), 2)

    def test_completion_uses_one_post_inventory_observation_for_h_fact_and_terminal_timestamp(self):
        for first_observation in ('before-h', 'after-h'):
            with self.subTest(first_observation=first_observation), Fixture() as f:
                self.prepare(f, hard_met=True, stale=False)
                f.campaign.mutate('network', 201, 'delete')
                self.assertTrue(f.campaign.poll_actions())
                self.assertEqual(f.journal.read()['mode'], 'CLOSING')
                self.assertFalse((f.guard_directory / 'hard-lifetime-breached.json').exists())
                hard = f.header['time']['h_ns']
                f.wall = hard - 10 * NS
                f.cloud.lists.clear()
                f.cloud.describes.clear()
                observations = []
                # The old three-read path sees before/before/after H and commits
                # true lifetime with an after-H timestamp. The reverse case pins
                # latching from the same after-H observation despite later rollback.
                sequence = ([hard - NS, hard - NS, hard + NS] if first_observation == 'before-h'
                            else [hard + NS, hard - NS, hard - NS])
                def clock():
                    caller = sys._getframe(1)
                    frames = []
                    while caller is not None:
                        if Path(caller.f_code.co_filename).name == 'campaign_safety.py':
                            frames.append(caller.f_code.co_name)
                        caller = caller.f_back
                    final_observation = (frames and frames[0] in {'completion', '_lifetime_breached'}
                        and 'completion' in frames and '_phase_budget' not in frames and 'lease_seconds' not in frames)
                    if not final_observation:
                        return f.wall
                    self.assertEqual([kind for kind, _ in f.cloud.lists], list(f.header['baseline']))
                    self.assertTrue(any((kind, rid) == ('network', 201)
                                        for kind, rid, _ in f.cloud.describes),
                                    'completion observation must follow the fresh exact absence checks')
                    value = sequence[min(len(observations), len(sequence) - 1)]
                    observations.append(value)
                    return value
                with patch.object(f.campaign, 'clock', new=clock):
                    result = f.campaign.completion()
                expected_time = sequence[0]
                expected_h = first_observation == 'before-h'
                self.assertEqual(observations, [expected_time],
                                 'completion and breach evidence must share one observation after all readbacks')
                frozen = self.snapshot(f)
                event = [event for event in frozen['state']['events'] if event.get('event') == 'completion-check'][-1]
                self.assertTrue(result['complete'])
                self.assertIs(result['hard_lifetime_met'], expected_h)
                self.assertEqual(event['unix_ns'], expected_time)
                self.assertEqual(self.terminal_event(frozen['state']), result)
                if expected_h:
                    self.assertNotIn('hard_lifetime_breached', frozen['state'])
                else:
                    self.assertIs(frozen['state'].get('hard_lifetime_breached'), True)
                    marker = json.loads(cc.private_bytes(f.guard_directory / 'hard-lifetime-breached.json'))
                    self.assertEqual(marker['observed_unix_ns'], expected_time)
                    breaches = [event for event in frozen['state']['events']
                                if event.get('event') == 'hard-lifetime-breached']
                    self.assertEqual([event['unix_ns'] for event in breaches], [expected_time])
                f.wall = hard + 120 * NS if expected_h else hard - 120 * NS
                self.assertEqual(f.campaign.terminal_result(), result)
                self.assertEqual(f.campaign.publish_cleanup(), result)
                self.assertEqual(self.snapshot(f), frozen)
                self.assert_receipt(f, result, expected_h)


if __name__ == '__main__':
    unittest.main()
