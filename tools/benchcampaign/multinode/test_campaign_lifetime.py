#!/usr/bin/env python3
"""Out-of-band H-breach evidence survives lock contention and a fresh guard.

Real Campaign/Journal, authenticated observation, flock, marker I/O and cleanup
are exercised. Only synthetic provider, process and clock boundaries are used.
"""
import contextlib
import copy
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
from test_campaign_safety import Fixture, NS  # noqa: E402


class CampaignLifetimeTest(unittest.TestCase):
    def setUp(self):
        self.stack = contextlib.ExitStack()
        self.addCleanup(self.stack.close)
        self.stack.enter_context(patch.dict(os.environ, {}, clear=True))
        self.stack.enter_context(patch.object(cc.http.client.HTTPSConnection, 'connect',
            side_effect=AssertionError('lifetime controls must never use a real network')))
        self.stack.enter_context(patch.object(cs.os, 'pidfd_open', create=True,
            side_effect=AssertionError('lifetime controls must never open a real pidfd')))
        self.stack.enter_context(patch.object(cs.signal, 'pidfd_send_signal', create=True,
            side_effect=AssertionError('lifetime controls must never signal a real process')))
        self.stack.enter_context(patch.object(cs.os, 'kill',
            side_effect=AssertionError('lifetime controls must never use a bare PID signal')))

    def marker(self, f):
        return f.guard_directory / 'hard-lifetime-breached.json'

    def prepare(self, f):
        created = f.create_all()
        self.assertEqual(created, [('network', 201), ('firewall', 301), ('ssh-key', 401),
                                   ('server', 101), ('server', 102), ('server', 103), ('server', 104)])
        self.assertTrue(f.campaign.poll_actions())
        self.assertEqual(len(f.cloud.requests), 7)
        f.closing()
        self.assertNotIn(111, f.processes)
        # Establish actual strict-release proof before contention. No SENDING
        # generation is being hidden by the lock in this marker-only control.
        f.campaign.heartbeat()
        state = f.journal.read()
        self.assertEqual(state['mode'], 'CLOSING')
        self.assertIsNone(state['active_lease'])
        self.assert_unlatched(state)
        self.assertFalse(self.marker(f).exists())
        self.assertEqual(f.deletes(), [])
        return state

    def assert_unlatched(self, state):
        self.assertNotIn('hard_lifetime_breached', state,
                         'the marker, not an existing headed flag, must be the sole H-breach evidence')
        self.assertFalse(any(event.get('event') in {'hard-lifetime-breached',
            'hard-deadline-unresolved', 'cleanup-reserve-exhausted'} for event in state['events']))

    def fresh_state(self, f):
        with contextlib.closing(cl.Journal(f.journal.path)) as journal:
            return journal.read()

    @contextlib.contextmanager
    def replacement(self, f):
        # The old guard and producer are gone; the replacement has a different
        # process identity and a new Journal/Campaign, not cached latch state.
        old_guard = copy.deepcopy(f.journal.read()['guard'])
        self.assertEqual(old_guard['identity']['pid'], 112)
        f.processes.pop(112)
        self.assertNotIn(111, f.processes)
        f.processes[113] = dict(pid=113, parent=110, starttime='1130', argv_sha256=cc.digest(b'113'))
        f.actor = 113
        with contextlib.closing(cl.Journal(f.journal.path)) as journal:
            campaign = cs.Campaign(journal, f.cloud, clock=lambda: f.wall, monotonic=lambda: f.mono)
            self.assertIsNot(campaign, f.campaign)
            self.assertIsNot(journal, f.journal)
            self.assertEqual(journal.binding, f.journal.binding)
            self.assertLess(campaign.clock(), f.header['time']['h_ns'])
            yield campaign, journal, old_guard

    def assert_cleaned(self, f, result, hard_met):
        self.assertTrue(result['complete'])
        self.assertTrue(result['baseline_equal'])
        self.assertEqual(result['owned_survivors'], [])
        self.assertEqual(result['subordinate_survivors'], 0)
        self.assertEqual(result['pending_intents'], [])
        self.assertIs(result['hard_lifetime_met'], hard_met)
        self.assertEqual(f.deletes(), [('DELETE', '/servers/101'), ('DELETE', '/servers/102'),
            ('DELETE', '/servers/103'), ('DELETE', '/servers/104'), ('DELETE', '/firewalls/301'),
            ('DELETE', '/ssh_keys/401'), ('DELETE', '/networks/201')])
        self.assertEqual(len(f.cloud.requests), 14)
        for kind, baseline in f.header['baseline'].items():
            self.assertEqual(sorted(f.cloud.rows[kind]), baseline,
                             'cleanup must restore every typed baseline, including implicit primary IPs')
        self.assertEqual(self.fresh_state(f)['mode'], 'COMPLETE')

    def test_busy_heartbeat_marker_alone_survives_wall_correction_and_fresh_guard_cleanup(self):
        with Fixture() as f:
            original = self.prepare(f)
            hard = f.header['time']['h_ns']
            head = f.journal.path.read_bytes()
            checkpoint = copy.deepcopy(f.journal.checkpoint)
            requests = copy.deepcopy(f.cloud.requests)
            before = json.loads(cc.private_bytes(f.guard_directory / 'heartbeat.json'))
            with contextlib.closing(cl.Journal(f.journal.path)) as external:
                self.assertIsNot(external, f.journal)
                with external.locked() as held:
                    self.assertEqual(held, original)
                    self.assertIsNone(f.journal.read_if_available(), 'independent flock must actually exclude the guard')
                    f.wall = hard + NS
                    f.mono += NS
                    fixed_origin = f.header['time']['t0_ns'] + f.mono - f.header['time']['monotonic_t0_ns']
                    self.assertLess(fixed_origin, hard)
                    observed = f.campaign.heartbeat()
                    self.assertEqual(observed, original)
                    self.assert_unlatched(observed)
                    self.assert_unlatched(f.journal.peek())
                    self.assertEqual(f.journal.path.read_bytes(), head,
                                     'busy heartbeat must not obtain its latch from any headed-state publication')
                    self.assertEqual(f.journal.checkpoint, checkpoint)
                    self.assertIsNone(f.journal.read_if_available(), 'the other Journal still owns the actual lock')
                    marker = self.marker(f)
                    info = marker.lstat()
                    self.assertTrue(stat.S_ISREG(info.st_mode))
                    self.assertEqual(stat.S_IMODE(info.st_mode), 0o600)
                    self.assertEqual((info.st_uid, info.st_nlink), (os.geteuid(), 1))
                    evidence = json.loads(cc.private_bytes(marker))
                    self.assertEqual(evidence, dict(binding=f.journal.binding, hard_lifetime_breached=True,
                        observed_unix_ns=hard + NS, hard_lifetime_met=False))
                    self.assertIs(evidence['hard_lifetime_breached'], True)
                    self.assertIs(evidence['hard_lifetime_met'], False)
                    raw_marker = marker.read_bytes()
                    heartbeat = json.loads(cc.private_bytes(f.guard_directory / 'heartbeat.json'))
                    self.assertEqual(heartbeat['identity'], f.processes[112])
                    self.assertEqual(heartbeat['monotonic_ns'], f.mono)
                    self.assertGreater(heartbeat['monotonic_ns'], before['monotonic_ns'])
                    self.assertEqual(f.cloud.requests, requests)
            # A strict reopened reader still has no breach flag/event: only the
            # separately bound marker can teach the replacement about H.
            self.assert_unlatched(self.fresh_state(f))
            self.assertEqual(f.journal.path.read_bytes(), head)
            f.wall = hard - 120 * NS
            self.assertLess(f.campaign.clock(), hard)
            with self.replacement(f) as (campaign, journal, old_guard):
                self.assert_unlatched(journal.read())
                replacement = campaign.arm_guard()
                self.assertEqual(replacement['identity'], f.processes[113])
                self.assertNotEqual(replacement['challenge'], old_guard['challenge'])
                latched = journal.read()
                self.assertIs(latched.get('hard_lifetime_breached'), True)
                latch_events = [event for event in latched['events'] if event.get('event') == 'hard-lifetime-breached']
                self.assertEqual(len(latch_events), 1)
                self.assertLess(latch_events[0]['unix_ns'], hard,
                                'the fresh guard must learn the prior breach after wall correction')
                self.assertFalse(latch_events[0]['hard_lifetime_met'])
                self.assertEqual(f.cloud.requests, requests, 'rearming must not dispatch provider mutations')
                result = campaign.cleanup_pass()
                self.assert_cleaned(f, result, hard_met=False)
                self.assertIs(self.fresh_state(f).get('hard_lifetime_breached'), True)
                self.assertLess(campaign.clock(), hard)
                self.assertEqual(self.marker(f).read_bytes(), raw_marker,
                                 'restart/cleanup must preserve the original bound observation')

    def test_before_h_without_marker_fresh_guard_cleanup_reports_lifetime_success(self):
        with Fixture() as f:
            self.prepare(f)
            f.wall = f.header['time']['h_ns'] - 120 * NS
            with self.replacement(f) as (campaign, journal, old_guard):
                replacement = campaign.arm_guard()
                self.assertEqual(replacement['identity']['pid'], 113)
                self.assertNotEqual(replacement['challenge'], old_guard['challenge'])
                self.assert_unlatched(journal.read())
                self.assertFalse(self.marker(f).exists())
                result = campaign.cleanup_pass()
                self.assert_cleaned(f, result, hard_met=True)
                self.assert_unlatched(self.fresh_state(f))
                self.assertFalse(self.marker(f).exists())

    def test_invalid_marker_refuses_heartbeat_and_replacement_without_dispatch_or_success(self):
        for fault in ('malformed-json', 'wrong-binding', 'false-flag'):
            with self.subTest(fault=fault), Fixture() as f:
                f.create()
                self.assertTrue(f.campaign.poll_actions())
                f.closing()
                f.campaign.heartbeat()
                self.assert_unlatched(f.journal.read())
                self.assertFalse(self.marker(f).exists())
                evidence = dict(binding=copy.deepcopy(f.journal.binding), hard_lifetime_breached=True,
                    observed_unix_ns=f.header['time']['h_ns'] + NS, hard_lifetime_met=False)
                if fault == 'wrong-binding':
                    evidence['binding']['uuid'] = '0' * 32
                    self.assertNotEqual(evidence['binding'], f.journal.binding)
                elif fault == 'false-flag':
                    evidence['hard_lifetime_breached'] = False
                raw = b'{"binding":' if fault == 'malformed-json' else cc.wire_json(evidence)
                marker = self.marker(f)
                marker.write_bytes(raw)
                marker.chmod(0o600)
                self.assertEqual(stat.S_IMODE(marker.stat().st_mode), 0o600)
                requests = copy.deepcopy(f.cloud.requests)
                headed = f.journal.path.read_bytes()
                expected = json.JSONDecodeError if fault == 'malformed-json' else mc.GuardError
                for phase in ('heartbeat', 'replacement'):
                    with self.subTest(phase=phase):
                        if phase == 'heartbeat':
                            with self.assertRaises(expected) as caught:
                                f.campaign.heartbeat()
                        else:
                            with self.replacement(f) as (campaign, journal, old_guard):
                                with self.assertRaises(expected) as caught:
                                    campaign.arm_guard()
                                self.assertEqual(journal.read()['guard'], old_guard)
                        if fault != 'malformed-json':
                            self.assertIn('hard-lifetime breach evidence binding differs', str(caught.exception))
                        self.assertEqual(f.cloud.requests, requests)
                        self.assertEqual(f.deletes(), [])
                        self.assertEqual(f.journal.path.read_bytes(), headed)
                        self.assertEqual(marker.read_bytes(), raw, 'invalid marker must not be silently repaired or ignored')
                        state = self.fresh_state(f)
                        self.assert_unlatched(state)
                        self.assertEqual(state['mode'], 'CLOSING')
                        self.assertFalse(any(event.get('event') == 'completion-check' for event in state['events']))
                        self.assertFalse((f.guard_directory / 'cleanup.json').exists())
                        self.assertIn(201, f.cloud.rows['network'])


if __name__ == '__main__':
    unittest.main()
