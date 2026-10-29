#!/usr/bin/env python3
"""RFC3339 provider literals through real ownership and association transitions.

Only provider responses/metadata, process identities and fixed clocks are fake.
No timestamp, Campaign, Journal, recovery or cascade proof method is replaced.
"""
import contextlib
import copy
import os
import unittest
from unittest.mock import patch

import campaign_cloud as cc
import campaign_safety as cs
import creation_ledger as cl
import mn_common as mc
from test_campaign_safety import Fixture


MALFORMED = (
    '20261027T12:00:00Z',
    '2026-10-27T120000Z',
    '2026-10-27T12:00:00+0000',
    '20261027T120000+0000',
    '2026-10-27T12:00:00,123Z',
    '2026-10-27 12:00:00Z',
    '2026-10-27T12:00Z',
    '2026-10-27T12:00.5Z',
    '2026-10-27T12:00:00+00',
    '2026-10-27T12:00:00+00:00:00',
    # RFC3339 punctuation alone cannot establish calendar/time parseability.
    '2026-02-30T12:00:00Z',
    '2025-02-29T12:00:00Z',
    '2026-13-01T12:00:00Z',
    '2026-10-00T12:00:00Z',
    '2026-10-27T24:00:00Z',
    '2026-10-27T12:60:00Z',
    '2026-10-27T12:34:61Z',
    # Offset components have independent lexical numeric limits.
    '2026-10-27T12:34:56+00:60',
    '2026-10-27T12:34:56-00:60',
    '2026-10-27T12:34:56+24:00',
)
VALID = (
    '2099-01-01T00:00:00Z',
    '1900-01-01t00:00:00z',
    '2026-10-27T12:34:56.123456789Z',
    '2026-10-27t12:34:56.5+05:30',
    '2026-10-27T12:34:56-04:00',
    '2026-10-27T12:34:56.000000001z',
    '2024-02-29T12:34:56Z',
    '2026-10-27T12:34:56+23:59',
    '2026-10-27T12:34:56-23:59',
)


class CampaignTimestampTest(unittest.TestCase):
    def setUp(self):
        self.stack = contextlib.ExitStack()
        self.addCleanup(self.stack.close)
        self.stack.enter_context(patch.dict(os.environ, {}, clear=True))
        self.stack.enter_context(patch.object(cc.http.client.HTTPSConnection, 'connect',
            side_effect=AssertionError('timestamp controls must never open a real network connection')))
        self.stack.enter_context(patch.object(cs.os, 'pidfd_open', create=True,
            side_effect=AssertionError('timestamp controls must never open a real pidfd')))
        self.stack.enter_context(patch.object(cs.signal, 'pidfd_send_signal', create=True,
            side_effect=AssertionError('timestamp controls must never signal a real process')))

    def fresh_state(self, fixture):
        with contextlib.closing(cl.Journal(fixture.journal.path)) as journal:
            return journal.read()

    @contextlib.contextmanager
    def network_ack_created(self, fixture, literal):
        materialize = fixture.cloud.materialize
        def provider(kind, body):
            self.assertEqual(kind, 'network')
            reply = materialize(kind, body)
            rid = reply['network']['id']
            reply['network']['created'] = literal
            fixture.cloud.rows['network'][rid]['created'] = literal
            return reply
        with patch.object(fixture.cloud, 'materialize', side_effect=provider):
            yield

    def lost_network_ack(self, fixture, literal):
        fixture.cloud.lose_ack = True
        with self.assertRaisesRegex(mc.GuardError, 'failed or ambiguous'):
            fixture.create()
        state = self.fresh_state(fixture)
        self.assertEqual(state['owned'], [])
        self.assertEqual(len(state['intents']), 1)
        self.assertIsNone(state['intents'][0]['resource_id'])
        self.assertIsNone(state['intents'][0]['ack'])
        rid = 201
        candidate = fixture.cloud.rows['network'][rid]
        self.assertEqual(candidate['labels']['intent'], state['intents'][0]['nonce'])
        self.assertNotIn(rid, fixture.header['baseline']['network'])
        self.assertNotIn(rid, state['intents'][0]['pre_ids']['network'])
        candidate['created'] = literal
        fixture.closing()
        return rid

    def assert_unowned_network_cannot_be_deleted(self, fixture, literal):
        result = fixture.campaign.cleanup_pass()
        self.assertFalse(result['complete'])
        state = self.fresh_state(fixture)
        self.assertEqual(state['owned'], [])
        self.assertEqual(state['associations'], [])
        self.assertEqual(state['intents'][0]['state'], 'UNKNOWN')
        self.assertIsNone(state['intents'][0]['resource_id'])
        self.assertEqual(fixture.deletes(), [])
        self.assertEqual(fixture.cloud.rows['network'][201]['created'], literal)
        self.assertEqual(result['pending_intents'], [0])

    def test_malformed_created_in_real_ack_cannot_become_owned_or_deleted(self):
        for literal in MALFORMED:
            with self.subTest(created=literal), Fixture() as f:
                wall, mono = f.wall, f.mono
                with self.network_ack_created(f, literal):
                    with self.assertRaisesRegex(mc.GuardError, 'failed or ambiguous'):
                        f.create()
                state = self.fresh_state(f)
                self.assertEqual([(method, path) for method, path, _, _ in f.cloud.requests], [('POST', '/networks')])
                self.assertEqual(state['owned'], [])
                self.assertEqual(state['associations'], [])
                self.assertEqual(state['intents'][0]['state'], 'UNKNOWN')
                self.assertIsNone(state['intents'][0]['resource_id'])
                self.assertEqual(state['intents'][0]['ack']['payload']['network']['created'], literal)
                f.closing()
                self.assert_unowned_network_cannot_be_deleted(f, literal)
                self.assertEqual((f.wall, f.mono), (wall, mono))

    def test_valid_created_ack_keeps_exact_opaque_literal_and_allows_cleanup(self):
        for literal in VALID:
            with self.subTest(created=literal), Fixture() as f:
                wall, mono = f.wall, f.mono
                with self.network_ack_created(f, literal):
                    self.assertEqual(f.create(), 201)
                state = self.fresh_state(f)
                self.assertEqual(state['intents'][0]['state'], 'ACKNOWLEDGED')
                self.assertEqual(state['intents'][0]['ack']['payload']['network']['created'], literal)
                self.assertEqual(state['owned'][0]['created'], literal)
                self.assertEqual(f.cloud.rows['network'][201]['created'], literal)
                self.assertTrue(f.campaign.poll_actions())
                f.closing()
                result = f.campaign.cleanup_pass()
                self.assertTrue(result['complete'])
                self.assertTrue(result['baseline_equal'])
                self.assertEqual(f.deletes(), [('DELETE', '/networks/201')])
                self.assertEqual(self.fresh_state(f)['owned'][0]['created'], literal)
                self.assertEqual((f.wall, f.mono), (wall, mono))

    def test_malformed_created_unique_recovery_candidate_remains_inconclusive_and_unowned(self):
        for literal in MALFORMED:
            with self.subTest(created=literal), Fixture() as f:
                wall, mono = f.wall, f.mono
                rid = self.lost_network_ack(f, literal)
                f.campaign.recover()
                state = self.fresh_state(f)
                self.assertEqual(state['owned'], [])
                self.assertEqual(state['intents'][0]['state'], 'UNKNOWN')
                self.assertIsNone(state['intents'][0]['resource_id'])
                self.assertIsNone(state['intents'][0]['ack'])
                censuses = [event for event in state['events'] if event.get('event') == 'recovery-census']
                self.assertTrue(censuses)
                self.assertEqual(censuses[-1]['candidates'], [])
                self.assertEqual(censuses[-1]['inconclusive_candidates'], [rid])
                failures = [event for event in state['events'] if event.get('event') == 'recovery-candidate-unavailable']
                self.assertTrue(any(event.get('id') == rid and event.get('error_type') == 'GuardError' for event in failures))
                self.assert_unowned_network_cannot_be_deleted(f, literal)
                self.assertEqual((f.wall, f.mono), (wall, mono))

    def test_valid_unique_recovery_preserves_exact_literal_without_manufacturing_ack(self):
        for literal in VALID:
            with self.subTest(created=literal), Fixture() as f:
                wall, mono = f.wall, f.mono
                self.lost_network_ack(f, literal)
                f.campaign.recover()
                state = self.fresh_state(f)
                intent = state['intents'][0]
                self.assertEqual(intent['state'], 'ACKNOWLEDGED')
                self.assertEqual(intent['resource_id'], 201)
                self.assertTrue(intent['recovered'])
                self.assertIsNone(intent['ack'])
                self.assertIsNone(intent['original_ack'])
                self.assertEqual(intent['recovery']['candidate']['created'], literal)
                self.assertEqual(state['owned'][0]['created'], literal)
                self.assertTrue(f.campaign.poll_actions())
                result = f.campaign.cleanup_pass()
                self.assertTrue(result['complete'])
                self.assertEqual(f.deletes(), [('DELETE', '/networks/201')])
                final = self.fresh_state(f)
                self.assertEqual(final['owned'][0]['created'], literal)
                self.assertIsNone(final['intents'][0]['ack'])
                self.assertEqual((f.wall, f.mono), (wall, mono))

    def test_malformed_created_cannot_grant_ip_association_or_server_cascade_delete(self):
        for literal in MALFORMED:
            with self.subTest(created=literal), Fixture() as f:
                wall, mono = f.wall, f.mono
                server = f.create('server')
                self.assertTrue(f.campaign.poll_actions())
                owned = copy.deepcopy(self.fresh_state(f)['owned'])
                f.cloud.rows['primary-ip'][10101]['created'] = literal
                before = copy.deepcopy(f.cloud.requests)
                with f.journal.locked() as state:
                    with self.assertRaises(mc.GuardError):
                        f.campaign.cascade(state, f.cloud.describe('server', server), f.cloud.list('primary-ip'))
                retained = self.fresh_state(f)
                self.assertEqual(retained['associations'], [])
                self.assertEqual(retained['owned'], owned)  # Valid server provenance is not IP authority.
                self.assertEqual(f.cloud.requests, before)
                f.closing()
                with self.assertRaises(mc.GuardError):
                    f.campaign.mutate('server', server, 'delete')
                self.assertEqual(f.cloud.requests, before)
                result = f.campaign.cleanup_pass()
                self.assertFalse(result['complete'])
                self.assertNotIn(('DELETE', '/servers/101'), f.deletes())
                self.assertEqual(f.deletes(), [('DELETE', '/firewalls/301'), ('DELETE', '/ssh_keys/401')])
                self.assertIn(server, f.cloud.rows['server'])
                self.assertEqual(f.cloud.rows['primary-ip'][10101]['created'], literal)
                self.assertEqual(self.fresh_state(f)['associations'], [])
                self.assertEqual((f.wall, f.mono), (wall, mono))

    def test_valid_ip_association_preserves_exact_literal_and_allows_real_cascade_cleanup(self):
        for literal in VALID:
            with self.subTest(created=literal), Fixture() as f:
                wall, mono = f.wall, f.mono
                server = f.create('server')
                self.assertTrue(f.campaign.poll_actions())
                f.cloud.rows['primary-ip'][10101]['created'] = literal
                before = copy.deepcopy(f.cloud.requests)
                with f.journal.locked() as state:
                    associations = f.campaign.cascade(state, f.cloud.describe('server', server),
                                                       f.cloud.list('primary-ip'))
                    f.journal.commit(state)
                self.assertEqual(len(associations), 1)
                self.assertEqual((associations[0]['id'], associations[0]['server_id']), (10101, server))
                self.assertEqual(associations[0]['created'], literal)
                self.assertEqual(associations[0]['origin'], 'saved-create-response')
                self.assertEqual(self.fresh_state(f)['associations'][0]['created'], literal)
                self.assertEqual(f.cloud.requests, before)
                f.closing()
                result = f.campaign.cleanup_pass()
                self.assertTrue(result['complete'])
                self.assertTrue(result['baseline_equal'])
                self.assertEqual(f.deletes(), [('DELETE', '/servers/101'), ('DELETE', '/firewalls/301'),
                                              ('DELETE', '/ssh_keys/401')])
                self.assertNotIn(server, f.cloud.rows['server'])
                self.assertNotIn(10101, f.cloud.rows['primary-ip'])
                self.assertEqual(self.fresh_state(f)['associations'][0]['created'], literal)
                self.assertEqual((f.wall, f.mono), (wall, mono))


if __name__ == '__main__':
    unittest.main()
