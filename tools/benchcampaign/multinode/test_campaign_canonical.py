#!/usr/bin/env python3
"""Independent firewall-rule and SSH-public-key canonical ownership controls.

Real plans, journals, ACK acceptance, nonce recovery and deletion authorization
are used. Only synthetic provider responses/rows and process/clock boundaries
come from the existing fixture; no production proof method is replaced.
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


RULES = [
    dict(direction='out', protocol='udp', port='53', source_ips=[],
         destination_ips=['198.51.100.9/32'], description='canonical outbound DNS'),
    dict(direction='in', protocol='tcp', port='22',
         source_ips=['203.0.113.8/32', '203.0.113.7/32'], destination_ips=[],
         description='canonical controller SSH'),
]
PUBLIC_KEY = 'ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAINdamAGCsQq31Uv+08lkBzoO4XLz2qYjJa8CGmj3B1Ea canonical-fixture'
OTHER_PUBLIC_KEY = 'ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAID1AF8PoQ4lakrcKp00bfrycmCzPLsSWjMDNVfEq9GYM canonical-fixture'
# Independent literal records: do not derive these oracles from a production
# extractor, expected-value helper, fixture body, or provider response.
EXPECTED = {
    'firewall': {'rules': [
        {'direction': 'in', 'protocol': 'tcp', 'port': '22',
         'source_ips': ['203.0.113.7/32', '203.0.113.8/32'], 'destination_ips': [],
         'description': 'canonical controller SSH'},
        {'direction': 'out', 'protocol': 'udp', 'port': '53', 'source_ips': [],
         'destination_ips': ['198.51.100.9/32'], 'description': 'canonical outbound DNS'},
    ]},
    'ssh-key': {'public_key': 'ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAINdamAGCsQq31Uv+08lkBzoO4XLz2qYjJa8CGmj3B1Ea canonical-fixture'},
}
FIELDS = {'firewall': 'rules', 'ssh-key': 'public_key'}
KEYS = {'firewall': 'firewall', 'ssh-key': 'ssh_key'}
IDS = {'firewall': 301, 'ssh-key': 401}
PATHS = {'firewall': '/firewalls', 'ssh-key': '/ssh_keys'}


@contextlib.contextmanager
def canonical_fixture():
    with Fixture(arm=False) as f:
        for kind, body in f.bodies:
            if kind == 'firewall': body['rules'] = copy.deepcopy(RULES)
            if kind == 'ssh-key': body['public_key'] = PUBLIC_KEY
        header = copy.deepcopy(f.header)
        for entry in header['plan']:
            if entry['kind'] == 'firewall': entry['descriptor']['rules'] = copy.deepcopy(RULES)
            if entry['kind'] == 'ssh-key': entry['descriptor']['public_key'] = PUBLIC_KEY
        # Seal a NEW journal; the original unarmed journal/header is untouched.
        journal = cl.Journal.initialize(f.root / 'canonical-journal' / 'head.json', header,
            dict(mode='OPEN', intents=[], owned=[], associations=[], events=[], guard=None, active_lease=None))
        f.stack.callback(journal.close)
        f.journal, f.header = journal, journal.header
        f.initial_checkpoint = journal.checkpoint
        f.campaign = cs.Campaign(journal, f.cloud, clock=lambda: f.wall, monotonic=lambda: f.mono)
        f.actor = 112
        f.challenge = f.campaign.arm_guard()['challenge']
        f.actor = 111
        f.heartbeat()
        yield f


class CampaignCanonicalTest(unittest.TestCase):
    def setUp(self):
        self.stack = contextlib.ExitStack()
        self.addCleanup(self.stack.close)
        self.stack.enter_context(patch.dict(os.environ, {}, clear=True))
        self.stack.enter_context(patch.object(cc.http.client.HTTPSConnection, 'connect',
            side_effect=AssertionError('canonical controls must never open a real network connection')))
        self.stack.enter_context(patch.object(cs.os, 'pidfd_open', create=True,
            side_effect=AssertionError('canonical controls must never open a real pidfd')))
        self.stack.enter_context(patch.object(cs.signal, 'pidfd_send_signal', create=True,
            side_effect=AssertionError('canonical controls must never signal a real process')))

    def state(self, f):
        with contextlib.closing(cl.Journal(f.journal.path)) as journal:
            return journal.read()

    def changed(self, kind, row, drift):
        result = copy.deepcopy(row)
        field = FIELDS[kind]
        if drift == 'missing': result.pop(field)
        elif kind == 'firewall': result['rules'][0]['port'] = '5353'
        else: result['public_key'] = OTHER_PUBLIC_KEY
        self.assertEqual({k: v for k, v in result.items() if k != field},
                         {k: v for k, v in row.items() if k != field})
        if drift == 'missing': self.assertNotIn(field, result)
        else: self.assertNotEqual(result[field], row[field])
        return result

    def assert_other_bindings(self, f, kind, item, intent):
        self.assertEqual(item['id'], IDS[kind])
        self.assertIs(type(item['id']), int)
        self.assertEqual(item['name'], intent['name'])
        self.assertEqual(item['name'], f.body(kind)['name'])
        self.assertEqual(item['labels'], intent['descriptor']['labels'])
        self.assertEqual(item['labels']['intent'], intent['nonce'])
        self.assertRegex(intent['nonce'], '^[0-9a-f]{32}$')
        matching = [row['id'] for row in f.cloud.rows[kind].values()
                    if (row.get('labels') or {}).get('intent') == intent['nonce']]
        self.assertEqual(matching, [IDS[kind]])
        self.assertNotIn(item['id'], f.header['baseline'][kind])
        self.assertNotIn(item['id'], intent['pre_ids'][kind])
        self.assertEqual(item['created'], f.cloud.rows[kind][IDS[kind]]['created'])
        self.assertEqual(intent['producer'], f.header['producer'])
        self.assertEqual(intent['send_lease']['actor'], f.header['producer'])
        self.assertGreater(intent['send_lease']['deadline_ns'], f.campaign.clock())
        self.assertGreater(intent['send_lease']['monotonic_deadline_ns'], f.mono)
        self.assertEqual(intent['request_sha256'], cc.digest(cc.wire_json(f.cloud.requests[0][2])))

    def assert_literal_record(self, f, kind, state):
        self.assertEqual(len(state['owned']), 1)
        record = state['owned'][0]
        self.assertEqual((record['kind'], record['id']), (kind, IDS[kind]))
        self.assertEqual(record['canonical'], EXPECTED[kind])
        if kind == 'firewall': self.assertEqual(len(record['canonical']['rules']), 2)
        return record

    def assert_retired(self, f, kind):
        state = self.state(f)
        events = [event for event in state['events'] if event.get('event') == 'observed-retirement'
                  and event.get('kind') == kind and event.get('id') == IDS[kind]]
        self.assertEqual(len(events), 1)
        self.assertEqual(events[0]['source'], 'owned-identity-discontinuity')
        return state

    def test_changed_or_missing_ack_field_is_unknown_at_the_specific_acceptance_gate(self):
        for kind in ('firewall', 'ssh-key'):
            for drift in ('changed', 'missing'):
                with self.subTest(kind=kind, drift=drift), canonical_fixture() as f:
                    request, original, supplied = f.cloud.request, [], []
                    def response(method, path, body=None, seconds=20):
                        reply = request(method, path, body, seconds)
                        self.assertEqual((method, path), ('POST', PATHS[kind]))
                        original.append(copy.deepcopy(reply['payload'][KEYS[kind]]))
                        reply['payload'][KEYS[kind]] = self.changed(kind, original[-1], drift)
                        reply['body_sha256'] = cc.digest(cc.wire_json(reply['payload']))
                        supplied.append(copy.deepcopy(reply))
                        return reply
                    with patch.object(f.cloud, 'request', side_effect=response):
                        with self.assertRaisesRegex(mc.GuardError, '^create failed or ambiguous; exact-ledger cleanup required$') as caught:
                            f.create(kind)
                    # The wrapper suppresses display of the original exception;
                    # its context still identifies the actual acceptance conjunct.
                    underlying = caught.exception.__context__
                    if drift == 'missing' and kind == 'firewall':
                        self.assertIs(type(underlying), KeyError)
                        self.assertEqual(underlying.args, ('rules',))
                    else:
                        self.assertIs(type(underlying), mc.GuardError)
                        self.assertEqual(str(underlying), 'invalid SSH key identity' if drift == 'missing'
                                         else 'creation canonical binding differs')
                    state = self.state(f)
                    self.assertEqual(state['mode'], 'OPEN')
                    self.assertEqual(state['owned'], [])
                    self.assertIsNone(state['active_lease'])
                    self.assertEqual(len(state['intents']), 1)
                    intent = state['intents'][0]
                    self.assertEqual(intent['state'], 'UNKNOWN')
                    self.assertIsNone(intent['resource_id'])
                    self.assertIs(intent['acknowledgement_validated'], True)
                    self.assertIs(intent['required_action_identified'], True)
                    self.assertEqual(intent['actions'], [1001] if kind == 'firewall' else [])
                    self.assertEqual(intent['ack'], supplied[0])
                    self.assertEqual(intent['ack']['body_sha256'], cc.digest(cc.wire_json(intent['ack']['payload'])))
                    self.assert_other_bindings(f, kind, supplied[0]['payload'][KEYS[kind]], intent)
                    self.assertEqual(f.cloud.rows[kind][IDS[kind]], original[0])
                    unknown = [event for event in state['events'] if event.get('event') == 'unknown-create']
                    self.assertEqual(len(unknown), 1)
                    self.assertEqual(unknown[0]['error_type'], type(underlying).__name__)
                    self.assertEqual([(m, p) for m, p, _, _ in f.cloud.requests], [('POST', PATHS[kind])])
                    self.assertEqual(f.deletes(), [])
                    self.assertTrue((f.guard_directory / 'stop.json').exists())

    def test_healthy_ack_stores_independent_literal_canonical_identity(self):
        for kind in ('firewall', 'ssh-key'):
            with self.subTest(kind=kind), canonical_fixture() as f:
                self.assertEqual(f.create(kind), IDS[kind])
                state = self.state(f)
                self.assert_literal_record(f, kind, state)
                intent = state['intents'][0]
                self.assertEqual(intent['state'], 'ACKNOWLEDGED')
                self.assert_other_bindings(f, kind, intent['ack']['payload'][KEYS[kind]], intent)
                self.assertEqual(intent['actions'], [1001] if kind == 'firewall' else [])
                self.assertTrue(f.campaign.poll_actions())
                self.assertEqual(self.state(f)['intents'][0]['state'], 'DONE')
                self.assert_literal_record(f, kind, self.state(f))
                self.assertEqual([(m, p) for m, p, _, _ in f.cloud.requests], [('POST', PATHS[kind])])

    def test_nonce_recovery_distinguishes_changed_identity_from_missing_canonical_field(self):
        for kind in ('firewall', 'ssh-key'):
            for drift in ('changed', 'missing'):
                with self.subTest(kind=kind, drift=drift), canonical_fixture() as f:
                    f.cloud.lose_ack = True
                    with self.assertRaisesRegex(mc.GuardError, 'create failed or ambiguous'):
                        f.create(kind)
                    original = copy.deepcopy(f.cloud.rows[kind][IDS[kind]])
                    intent = self.state(f)['intents'][0]
                    self.assert_other_bindings(f, kind, original, intent)
                    f.cloud.rows[kind][IDS[kind]] = self.changed(kind, original, drift)
                    before = copy.deepcopy(f.cloud.requests)
                    f.closing()
                    f.campaign.recover()
                    state = self.state(f)
                    self.assertEqual(state['owned'], [])
                    self.assertEqual(state['intents'][0]['state'], 'UNKNOWN')
                    self.assertIsNone(state['intents'][0]['resource_id'])
                    self.assertIsNone(state['intents'][0]['ack'])
                    census = [e for e in state['events'] if e.get('event') == 'recovery-census']
                    self.assertEqual(len(census), 1)
                    self.assertEqual(census[0]['candidates'], [])
                    refused = [e for e in state['events'] if e.get('event') == 'recovery-candidate-refused']
                    unavailable = [e for e in state['events'] if e.get('event') == 'recovery-candidate-unavailable']
                    if drift == 'changed':
                        self.assertEqual(census[0]['inconclusive_candidates'], [])
                        self.assertEqual([(e['kind'], e['id']) for e in refused], [(kind, IDS[kind])])
                        self.assertEqual(unavailable, [])
                    else:
                        self.assertEqual(census[0]['inconclusive_candidates'], [IDS[kind]])
                        self.assertEqual(refused, [])
                        self.assertEqual([(e['kind'], e['id'], e['error_type']) for e in unavailable],
                                         [(kind, IDS[kind], 'KeyError' if kind == 'firewall' else 'GuardError')])
                    self.assertEqual(f.cloud.requests, before)
                    result = f.campaign.cleanup_pass()
                    self.assertFalse(result['complete'])
                    self.assertEqual(self.state(f)['owned'], [])
                    self.assertEqual(f.cloud.requests, before)
                    self.assertEqual(f.deletes(), [])

    def test_healthy_nonce_recovery_stores_literal_identity_and_permits_exact_delete(self):
        for kind in ('firewall', 'ssh-key'):
            with self.subTest(kind=kind), canonical_fixture() as f:
                f.cloud.lose_ack = True
                with self.assertRaisesRegex(mc.GuardError, 'create failed or ambiguous'):
                    f.create(kind)
                before = copy.deepcopy(f.cloud.requests)
                f.closing()
                f.campaign.recover()
                state = self.state(f)
                self.assert_literal_record(f, kind, state)
                intent = state['intents'][0]
                self.assertEqual(intent['resource_id'], IDS[kind])
                self.assertIs(intent['recovered'], True)
                self.assertIsNone(intent['ack'])
                self.assertIsNone(intent['original_ack'])
                self.assertEqual(intent['recovery']['candidate'], f.cloud.rows[kind][IDS[kind]])
                self.assertEqual(intent['state'], 'UNKNOWN' if kind == 'firewall' else 'ACKNOWLEDGED')
                self.assertEqual(f.cloud.requests, before)
                f.campaign.mutate(kind, IDS[kind], 'delete')
                self.assertEqual(f.deletes(), [('DELETE', PATHS[kind] + '/' + str(IDS[kind]))])
                self.assertEqual([(m, p) for m, p, _, _ in f.cloud.requests],
                                 [('POST', PATHS[kind]), ('DELETE', PATHS[kind] + '/' + str(IDS[kind]))])
                self.assertNotIn(IDS[kind], f.cloud.rows[kind])
                self.assert_literal_record(f, kind, self.state(f))

    def test_delete_field_drift_retires_before_dispatch_and_restoration_cannot_restore_authority(self):
        for kind in ('firewall', 'ssh-key'):
            for drift in ('changed', 'missing'):
                with self.subTest(kind=kind, drift=drift), canonical_fixture() as f:
                    self.assertEqual(f.create(kind), IDS[kind])
                    self.assertTrue(f.campaign.poll_actions())
                    owned = copy.deepcopy(self.assert_literal_record(f, kind, self.state(f)))
                    original = copy.deepcopy(f.cloud.rows[kind][IDS[kind]])
                    f.closing()
                    f.cloud.rows[kind][IDS[kind]] = self.changed(kind, original, drift)
                    before = copy.deepcopy(f.cloud.requests)
                    if drift == 'missing' and kind == 'firewall':
                        with self.assertRaises(KeyError) as caught:
                            f.campaign.mutate(kind, IDS[kind], 'delete')
                        self.assertEqual(caught.exception.args, ('rules',))
                    else:
                        expected = 'invalid SSH key identity' if drift == 'missing' else 'owned resource identity changed or ID reused'
                        with self.assertRaisesRegex(mc.GuardError, '^' + expected + '$'):
                            f.campaign.mutate(kind, IDS[kind], 'delete')
                    retired = self.assert_retired(f, kind)
                    self.assertEqual(retired['owned'], [owned])
                    self.assertEqual(len(retired['intents']), 1)
                    self.assertEqual(f.cloud.requests, before)
                    f.cloud.rows[kind][IDS[kind]] = copy.deepcopy(original)
                    self.assertEqual(f.cloud.describe(kind, IDS[kind]), original)
                    with self.assertRaisesRegex(mc.GuardError, '^observed-retired ID cannot inherit ownership$'):
                        f.campaign.mutate(kind, IDS[kind], 'delete')
                    self.assertEqual(f.cloud.requests, before)
                    self.assertEqual(f.deletes(), [])
                    self.assert_literal_record(f, kind, self.assert_retired(f, kind))

    def test_healthy_delete_dispatches_only_literal_owned_id_and_restores_baseline(self):
        for kind in ('firewall', 'ssh-key'):
            with self.subTest(kind=kind), canonical_fixture() as f:
                self.assertEqual(f.create(kind), IDS[kind])
                self.assertTrue(f.campaign.poll_actions())
                self.assert_literal_record(f, kind, self.state(f))
                f.closing()
                f.campaign.mutate(kind, IDS[kind], 'delete')
                self.assertEqual(f.deletes(), [('DELETE', '/firewalls/301' if kind == 'firewall' else '/ssh_keys/401')])
                self.assertTrue(f.campaign.poll_actions())
                self.assertTrue(f.campaign.completion()['complete'])
                self.assert_literal_record(f, kind, self.state(f))
                for resource_kind, ids in f.header['baseline'].items():
                    self.assertEqual(sorted(f.cloud.rows[resource_kind]), ids)


if __name__ == '__main__':
    unittest.main()
