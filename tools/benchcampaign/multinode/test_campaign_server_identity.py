#!/usr/bin/env python3
"""Independent provider server-identity bindings at acceptance, recovery and DELETE.

The unchanged canonical fixture supplies a real Journal/guard and real nonempty
firewall/public-key dependencies. Only provider reply/row fields are varied;
no Campaign, Journal, canonical extractor or orchestration method is replaced.
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
from test_campaign_canonical import canonical_fixture


# Independent literal oracle, not derived from a request, provider response,
# fixture descriptor, canonical extractor or expected-canonical helper.
EXPECTED_SERVER = {'server_type': 'ccx43', 'location': 'fsn1',
                   'image': 161547269, 'firewalls': [301]}
FIELDS = ('server_type', 'location', 'image', 'firewalls')


class CampaignServerIdentityTest(unittest.TestCase):
    def setUp(self):
        self.stack = contextlib.ExitStack()
        self.addCleanup(self.stack.close)
        self.stack.enter_context(patch.dict(os.environ, {}, clear=True))
        self.stack.enter_context(patch.object(cc.http.client.HTTPSConnection, 'connect',
            side_effect=AssertionError('server identity controls must never open a real network connection')))
        self.stack.enter_context(patch.object(cs.os, 'pidfd_open', create=True,
            side_effect=AssertionError('server identity controls must never open a real pidfd')))
        self.stack.enter_context(patch.object(cs.signal, 'pidfd_send_signal', create=True,
            side_effect=AssertionError('server identity controls must never signal a real process')))

    def state(self, f):
        with contextlib.closing(cl.Journal(f.journal.path)) as journal:
            return journal.read()

    def dependencies(self, f):
        f.dependencies()
        self.assertTrue(f.campaign.poll_actions())
        state = self.state(f)
        self.assertEqual([(r['kind'], r['id']) for r in state['owned']], [('firewall', 301), ('ssh-key', 401)])
        self.assertEqual(len(f.cloud.rows['firewall'][301]['rules']), 2)
        self.assertEqual(f.cloud.rows['ssh-key'][401]['public_key'],
            'ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAINdamAGCsQq31Uv+08lkBzoO4XLz2qYjJa8CGmj3B1Ea canonical-fixture')
        return copy.deepcopy(state['owned'])

    def changed(self, original, field, drift):
        result = copy.deepcopy(original)
        if field == 'firewalls':
            if drift == 'missing': result['public_net'].pop('firewalls')
            elif drift == 'equal-float': result['public_net']['firewalls'] = [{'id': 301.0}]
            else: result['public_net']['firewalls'] = [{'id': 302}]
            self.assertEqual({k: v for k, v in result.items() if k != 'public_net'},
                             {k: v for k, v in original.items() if k != 'public_net'})
            self.assertEqual({k: v for k, v in result['public_net'].items() if k != 'firewalls'},
                             {k: v for k, v in original['public_net'].items() if k != 'firewalls'})
            if drift == 'missing': self.assertNotIn('firewalls', result['public_net'])
            elif drift == 'equal-float':
                self.assertIs(type(original['public_net']['firewalls'][0]['id']), int)
                self.assertIs(type(result['public_net']['firewalls'][0]['id']), float)
                self.assertEqual(result['public_net']['firewalls'][0]['id'], 301)
            else:
                self.assertEqual(result['public_net']['firewalls'], [{'id': 302}])
                self.assertIs(type(result['public_net']['firewalls'][0]['id']), int)
        else:
            if drift == 'missing': result.pop(field)
            elif field == 'server_type': result['server_type']['name'] = 'ccx33'
            elif field == 'location': result['location']['name'] = 'nbg1'
            elif drift == 'equal-float': result['image']['id'] = 161547269.0
            else: result['image']['id'] = 161547270
            self.assertEqual({k: v for k, v in result.items() if k != field},
                             {k: v for k, v in original.items() if k != field})
            if drift == 'missing': self.assertNotIn(field, result)
            elif field == 'server_type': self.assertEqual(result['server_type'], {'name': 'ccx33'})
            elif field == 'image':
                if drift == 'equal-float':
                    self.assertIs(type(original['image']['id']), int)
                    self.assertIs(type(result['image']['id']), float)
                    self.assertEqual(result['image']['id'], 161547269)
                else:
                    self.assertEqual(result['image'], {'id': 161547270})
                    self.assertIs(type(result['image']['id']), int)
            else: self.assertEqual(result['location'], {'name': 'nbg1'})
        if drift == 'equal-float':
            self.assertIn(field, {'image', 'firewalls'})
            self.assertEqual(result, original, 'only the numeric type changes; Python value equality must hold')
        else:
            self.assertNotEqual(result, original)
        return result

    def assert_other_bindings(self, f, item, intent, changed_field=None):
        self.assertEqual(item['id'], 101)
        self.assertIs(type(item['id']), int)
        self.assertEqual(item['name'], 'test-node0')
        self.assertEqual(item['name'], intent['name'])
        self.assertEqual(item['labels'], intent['descriptor']['labels'])
        self.assertEqual(item['labels']['intent'], intent['nonce'])
        self.assertRegex(intent['nonce'], '^[0-9a-f]{32}$')
        self.assertEqual(item['created'], f.cloud.rows['server'][101]['created'])
        if changed_field != 'server_type':
            self.assertEqual(item['server_type'], {'name': 'ccx43'})
        self.assertEqual(item['volumes'], [])
        self.assertIsNone(item['backup_window'])
        self.assertEqual(item['public_net']['ipv4'], {'id': 10101, 'ip': '203.0.113.10'})
        self.assertIsNone(item['public_net']['ipv6'])
        self.assertEqual(item['public_net']['floating_ips'], [])
        ip = f.cloud.rows['primary-ip'][10101]
        self.assertEqual((ip['id'], ip['type'], ip['assignee_type'], ip['assignee_id'], ip['auto_delete']),
                         (10101, 'ipv4', 'server', 101, True))
        self.assertEqual(ip['created'], item['created'])
        matching = [row['id'] for row in f.cloud.rows['server'].values()
                    if (row.get('labels') or {}).get('intent') == intent['nonce']]
        self.assertEqual(matching, [101])
        self.assertNotIn(101, f.header['baseline']['server'])
        self.assertNotIn(101, intent['pre_ids']['server'])
        self.assertNotIn(10101, f.header['baseline']['primary-ip'])
        self.assertNotIn(10101, intent['pre_ids']['primary-ip'])
        self.assertEqual(intent['descriptor']['server_type'], 'ccx43')
        self.assertEqual(intent['descriptor']['location'], 'fsn1')
        self.assertEqual(intent['descriptor']['image'], 161547269)
        self.assertIs(type(intent['descriptor']['image']), int)
        self.assertEqual(intent['descriptor']['firewalls'], [{'firewall': 301}])
        self.assertIs(type(intent['descriptor']['firewalls'][0]['firewall']), int)
        self.assertEqual(intent['descriptor']['ssh_keys'], [401])
        self.assertEqual(intent['producer'], f.header['producer'])
        self.assertEqual(intent['send_lease']['actor'], f.header['producer'])
        self.assertGreater(intent['send_lease']['deadline_ns'], f.campaign.clock())
        self.assertGreater(intent['send_lease']['monotonic_deadline_ns'], f.mono)
        server_posts = [body for method, path, body, _ in f.cloud.requests if (method, path) == ('POST', '/servers')]
        self.assertEqual(len(server_posts), 1)
        self.assertEqual(server_posts[0]['image'], 161547269)
        self.assertIs(type(server_posts[0]['image']), int)
        self.assertEqual(server_posts[0]['firewalls'], [{'firewall': 301}])
        self.assertIs(type(server_posts[0]['firewalls'][0]['firewall']), int)
        self.assertEqual(intent['request_sha256'], cc.digest(cc.wire_json(server_posts[0])))

    def assert_literal_server(self, state):
        records = [r for r in state['owned'] if r['kind'] == 'server']
        self.assertEqual(len(records), 1)
        record = records[0]
        self.assertEqual(record['id'], 101)
        self.assertEqual(record['canonical'], EXPECTED_SERVER)
        self.assertIs(type(record['canonical']['image']), int)
        self.assertEqual([type(rid) for rid in record['canonical']['firewalls']], [int])
        self.assertEqual(set(record['canonical']), {'server_type', 'location', 'image', 'firewalls'})
        self.assertEqual(record['implicit_ips'], [10101])
        return record

    def assert_specific_error(self, error, field, drift, acceptance=False):
        if drift == 'missing' and field in {'server_type', 'location'}:
            self.assertIs(type(error), KeyError)
            self.assertEqual(error.args, (field,))
        else:
            self.assertIs(type(error), mc.GuardError)
            if drift in {'missing', 'equal-float'}: expected = 'missing or invalid server canonical identity'
            elif acceptance: expected = 'creation canonical binding differs'
            else: expected = 'owned resource identity changed or ID reused'
            self.assertEqual(str(error), expected)

    def assert_server_retired(self, f):
        state = self.state(f)
        events = [e for e in state['events'] if e.get('event') == 'observed-retirement'
                  and e.get('kind') == 'server' and e.get('id') == 101]
        self.assertEqual(len(events), 1)
        self.assertEqual(events[0]['source'], 'owned-identity-discontinuity')
        return state

    def test_ack_changed_or_missing_server_fields_fail_the_specific_acceptance_binding(self):
        for field in FIELDS:
            for drift in (('changed', 'missing', 'equal-float') if field in {'image', 'firewalls'} else ('changed', 'missing')):
                with self.subTest(field=field, drift=drift), canonical_fixture() as f:
                    dependencies = self.dependencies(f)
                    request, originals, supplied = f.cloud.request, [], []
                    def response(method, path, body=None, seconds=20):
                        self.assertEqual((method, path), ('POST', '/servers'))
                        reply = request(method, path, body, seconds)
                        originals.append(copy.deepcopy(reply['payload']['server']))
                        reply['payload']['server'] = self.changed(originals[-1], field, drift)
                        reply['body_sha256'] = cc.digest(cc.wire_json(reply['payload']))
                        supplied.append(copy.deepcopy(reply))
                        return reply
                    with patch.object(f.cloud, 'request', side_effect=response):
                        with self.assertRaisesRegex(mc.GuardError, '^create failed or ambiguous; exact-ledger cleanup required$') as caught:
                            f.create('server')
                    self.assert_specific_error(caught.exception.__context__, field, drift, acceptance=True)
                    state = self.state(f)
                    self.assertEqual(state['mode'], 'OPEN')
                    self.assertEqual(state['owned'], dependencies)
                    self.assertEqual(state['associations'], [])
                    self.assertIsNone(state['active_lease'])
                    self.assertEqual(len(state['intents']), 3)
                    intent = state['intents'][-1]
                    self.assertEqual(intent['state'], 'UNKNOWN')
                    self.assertIsNone(intent['resource_id'])
                    self.assertIs(intent['acknowledgement_validated'], True)
                    self.assertIs(intent['required_action_identified'], True)
                    self.assertEqual(intent['actions'], [1002])
                    self.assertEqual(intent['ack'], supplied[0])
                    self.assertEqual(intent['ack']['body_sha256'], cc.digest(cc.wire_json(intent['ack']['payload'])))
                    if drift == 'equal-float':
                        returned = intent['ack']['payload']['server']
                        value = returned['image']['id'] if field == 'image' else returned['public_net']['firewalls'][0]['id']
                        self.assertIs(type(value), float)
                        self.assertEqual(value, 161547269 if field == 'image' else 301)
                    self.assertEqual(intent['ack']['payload']['action']['command'], 'create_server')
                    self.assertEqual(intent['ack']['payload']['action']['status'], 'success')
                    self.assertEqual(intent['ack']['payload']['action']['resources'], [{'type': 'server', 'id': 101}])
                    self.assertEqual(intent['ack']['payload']['next_actions'], [])
                    self.assert_other_bindings(f, intent['ack']['payload']['server'], intent, changed_field=field)
                    self.assertEqual(f.cloud.rows['server'][101], originals[0])
                    unknown = [e for e in state['events'] if e.get('event') == 'unknown-create']
                    self.assertEqual([(e['intent'], e['error_type']) for e in unknown],
                                     [(intent['sequence'], type(caught.exception.__context__).__name__)])
                    self.assertEqual([(m, p) for m, p, _, _ in f.cloud.requests],
                                     [('POST', '/firewalls'), ('POST', '/ssh_keys'), ('POST', '/servers')])
                    self.assertEqual(f.deletes(), [])
                    self.assertTrue((f.guard_directory / 'stop.json').exists())

    def test_healthy_ack_stores_the_independent_literal_four_field_server_identity(self):
        with canonical_fixture() as f:
            self.dependencies(f)
            self.assertEqual(f.create('server'), 101)
            state = self.state(f)
            self.assert_literal_server(state)
            intent = state['intents'][-1]
            self.assertEqual(intent['state'], 'ACKNOWLEDGED')
            self.assertEqual(intent['actions'], [1002])
            self.assert_other_bindings(f, intent['ack']['payload']['server'], intent)
            self.assertEqual(state['associations'], [])  # Acceptance is not a cascade proof.
            self.assertTrue(f.campaign.poll_actions())
            self.assertEqual(self.state(f)['intents'][-1]['state'], 'DONE')
            self.assert_literal_server(self.state(f))
            self.assertEqual(f.deletes(), [])

    def test_nonce_recovery_changed_fields_refuse_and_missing_fields_remain_inconclusive(self):
        for field in FIELDS:
            for drift in (('changed', 'missing', 'equal-float') if field in {'image', 'firewalls'} else ('changed', 'missing')):
                with self.subTest(field=field, drift=drift), canonical_fixture() as f:
                    dependencies = self.dependencies(f)
                    f.cloud.lose_ack = True
                    with self.assertRaisesRegex(mc.GuardError, 'create failed or ambiguous'):
                        f.create('server')
                    original = copy.deepcopy(f.cloud.rows['server'][101])
                    self.assert_other_bindings(f, original, self.state(f)['intents'][-1])
                    f.cloud.rows['server'][101] = self.changed(original, field, drift)
                    ip = copy.deepcopy(f.cloud.rows['primary-ip'][10101])
                    before = copy.deepcopy(f.cloud.requests)
                    f.closing()
                    f.campaign.recover()
                    state = self.state(f)
                    self.assertEqual(state['owned'], dependencies)
                    self.assertEqual(state['associations'], [])
                    intent = state['intents'][-1]
                    self.assertEqual(intent['state'], 'UNKNOWN')
                    self.assertIsNone(intent['resource_id'])
                    self.assertIsNone(intent['ack'])
                    self.assertEqual(intent['actions'], [])
                    censuses = [e for e in state['events'] if e.get('event') == 'recovery-census']
                    self.assertEqual(len(censuses), 1)
                    self.assertEqual(censuses[0]['candidates'], [])
                    refused = [e for e in state['events'] if e.get('event') == 'recovery-candidate-refused']
                    unavailable = [e for e in state['events'] if e.get('event') == 'recovery-candidate-unavailable']
                    if drift == 'changed':
                        self.assertEqual(censuses[0]['inconclusive_candidates'], [])
                        self.assertEqual([(e['kind'], e['id']) for e in refused], [('server', 101)])
                        self.assertEqual(unavailable, [])
                    else:
                        self.assertEqual(censuses[0]['inconclusive_candidates'], [101])
                        self.assertEqual(refused, [])
                        self.assertEqual([(e['kind'], e['id'], e['error_type']) for e in unavailable],
                                         [('server', 101, 'KeyError' if field in {'server_type', 'location'} else 'GuardError')])
                    self.assertEqual(f.cloud.requests, before)
                    result = f.campaign.cleanup_pass()
                    self.assertFalse(result['complete'])
                    self.assertFalse(any(r['kind'] == 'server' for r in self.state(f)['owned']))
                    self.assertEqual([(m, p) for m, p, _, _ in f.cloud.requests if m == 'POST'],
                                     [(m, p) for m, p, _, _ in before])
                    self.assertEqual(f.deletes(), [('DELETE', '/firewalls/301'), ('DELETE', '/ssh_keys/401')])
                    self.assertIn(101, f.cloud.rows['server'])
                    self.assertEqual(f.cloud.rows['primary-ip'][10101], ip)

    def test_healthy_nonce_recovery_preserves_literal_identity_and_permits_real_cascade_delete(self):
        with canonical_fixture() as f:
            self.dependencies(f)
            f.cloud.lose_ack = True
            with self.assertRaisesRegex(mc.GuardError, 'create failed or ambiguous'):
                f.create('server')
            before = copy.deepcopy(f.cloud.requests)
            f.closing()
            f.campaign.recover()
            state = self.state(f)
            self.assert_literal_server(state)
            intent = state['intents'][-1]
            self.assertEqual(intent['resource_id'], 101)
            self.assertIs(intent['recovered'], True)
            self.assertIsNone(intent['ack'])
            self.assertIsNone(intent['original_ack'])
            self.assertEqual(intent['state'], 'ACKNOWLEDGED')
            self.assertIs(intent['required_action_identified'], True)
            self.assertEqual(intent['actions'], [1002])
            self.assertEqual(intent['recovery']['corroborating_create_action']['command'], 'create_server')
            self.assertEqual(intent['recovery']['candidate'], f.cloud.rows['server'][101])
            self.assertEqual(f.cloud.requests, before)
            self.assertTrue(f.campaign.poll_actions())
            f.campaign.mutate('server', 101, 'delete')
            self.assertEqual(f.deletes(), [('DELETE', '/servers/101')])
            self.assertNotIn(101, f.cloud.rows['server'])
            self.assertNotIn(10101, f.cloud.rows['primary-ip'])
            self.assertTrue(f.campaign.cleanup_pass()['complete'])
            self.assert_literal_server(self.state(f))
            self.assertEqual(f.deletes(), [('DELETE', '/servers/101'), ('DELETE', '/firewalls/301'),
                                          ('DELETE', '/ssh_keys/401')])

    def test_delete_field_drift_retires_before_cascade_and_restoration_cannot_restore_authority(self):
        for field in FIELDS:
            for drift in (('changed', 'missing', 'equal-float') if field in {'image', 'firewalls'} else ('changed', 'missing')):
                with self.subTest(field=field, drift=drift), canonical_fixture() as f:
                    self.assertEqual(f.create('server'), 101)
                    self.assertTrue(f.campaign.poll_actions())
                    owned = copy.deepcopy(self.assert_literal_server(self.state(f)))
                    original = copy.deepcopy(f.cloud.rows['server'][101])
                    ip = copy.deepcopy(f.cloud.rows['primary-ip'][10101])
                    f.closing()
                    f.cloud.rows['server'][101] = self.changed(original, field, drift)
                    before = copy.deepcopy(f.cloud.requests)
                    lists = copy.deepcopy(f.cloud.lists)
                    intent_count = len(self.state(f)['intents'])
                    with self.assertRaises((mc.GuardError, KeyError)) as caught:
                        f.campaign.mutate('server', 101, 'delete')
                    self.assert_specific_error(caught.exception, field, drift)
                    retired = self.assert_server_retired(f)
                    self.assertEqual(self.assert_literal_server(retired), owned)
                    self.assertEqual(len(retired['intents']), intent_count)
                    self.assertEqual(retired['associations'], [])
                    self.assertEqual(f.cloud.lists, lists, 'authorization must fail before a cascade IP census')
                    self.assertEqual(f.cloud.requests, before)
                    self.assertEqual(f.cloud.rows['primary-ip'][10101], ip)
                    f.cloud.rows['server'][101] = copy.deepcopy(original)
                    self.assertEqual(f.cloud.describe('server', 101), original)
                    with self.assertRaisesRegex(mc.GuardError, '^observed-retired ID cannot inherit ownership$'):
                        f.campaign.mutate('server', 101, 'delete')
                    self.assertEqual(f.cloud.requests, before)
                    self.assertEqual(f.cloud.lists, lists)
                    self.assertEqual(f.deletes(), [])
                    self.assertFalse(f.campaign.cleanup_pass()['complete'])
                    self.assertEqual(f.deletes(), [('DELETE', '/firewalls/301'), ('DELETE', '/ssh_keys/401')])
                    self.assertIn(101, f.cloud.rows['server'])
                    self.assertEqual(f.cloud.rows['primary-ip'][10101], ip)
                    self.assertEqual(self.assert_literal_server(self.assert_server_retired(f)), owned)

    def test_healthy_delete_uses_only_exact_server_id_and_auto_deletes_its_ip(self):
        with canonical_fixture() as f:
            self.assertEqual(f.create('server'), 101)
            self.assertTrue(f.campaign.poll_actions())
            self.assert_literal_server(self.state(f))
            f.closing()
            f.campaign.mutate('server', 101, 'delete')
            self.assertEqual(f.deletes(), [('DELETE', '/servers/101')])
            self.assertNotIn(101, f.cloud.rows['server'])
            self.assertNotIn(10101, f.cloud.rows['primary-ip'])
            self.assertTrue(f.campaign.cleanup_pass()['complete'])
            self.assert_literal_server(self.state(f))
            self.assertEqual(f.deletes(), [('DELETE', '/servers/101'), ('DELETE', '/firewalls/301'),
                                          ('DELETE', '/ssh_keys/401')])
            for kind, ids in f.header['baseline'].items():
                self.assertEqual(sorted(f.cloud.rows[kind]), ids)


if __name__ == '__main__':
    unittest.main()
