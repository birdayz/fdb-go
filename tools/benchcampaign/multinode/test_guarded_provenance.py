#!/usr/bin/env python3
"""Revised RFC-283 conditional provenance, opaque metadata and IP attribution.

Use real guarded initialization, Journal and Campaign methods. Only provider
metadata, process identities and clocks are synthetic; no authorization is stubbed.
"""
import contextlib
import copy
import json
import os
from pathlib import Path
import sys
import unittest
from unittest import mock

HERE = Path(os.path.abspath(__file__)).parent
sys.path.insert(0, str(HERE))

import campaign_safety as cs  # noqa: E402
import creation_ledger as cl  # noqa: E402
import mn_common as mc  # noqa: E402
import provision  # noqa: E402
from test_guarded_initialize import Initialization  # noqa: E402

MISSING = object()


@contextlib.contextmanager
def provider_created(fixture, literal):
    """Vary only provider metadata, independently of the fixed local clocks."""
    original = fixture.cloud.materialize
    def materialize(kind, body):
        payload = original(kind, body)
        item = payload[cs.resource_key(kind)]
        rid = item['id']
        if literal is MISSING:
            item.pop('created')
            fixture.cloud.rows[kind][rid].pop('created')
        else:
            item['created'] = literal
            fixture.cloud.rows[kind][rid]['created'] = literal
        if kind == 'server':
            ip = fixture.cloud.rows[kind][rid]['public_net']['ipv4']['id']
            if literal is MISSING:
                fixture.cloud.rows['primary-ip'][ip].pop('created')
            else:
                fixture.cloud.rows['primary-ip'][ip]['created'] = literal
        return payload
    with mock.patch.object(fixture.cloud, 'materialize', side_effect=materialize):
        yield


def armed(fixture):
    journal = fixture.initialize()
    campaign = fixture.campaign(journal)
    fixture.actor = 112
    campaign.arm_guard()
    fixture.actor = 111
    return journal, campaign


def closing(fixture, campaign):
    fixture.actor = 112
    fixture.processes.pop(111, None)
    campaign.close_admission()


def response_lost_server(fixture, literal='2099-12-31T23:59:59.123456+00:00', before_last=None):
    journal, campaign = armed(fixture)
    plan, ids = fixture.plan(), {}
    def body(entry):
        value = copy.deepcopy(entry['body'])
        if entry['kind'] == 'server':
            value['ssh_keys'] = [ids[entry['body']['ssh_keys'][0]]]
            value['firewalls'] = [dict(firewall=ids[entry['body']['firewalls'][0]['firewall']])]
        return value
    for entry in plan[:6]:
        ids[entry['name']] = campaign.create(entry['kind'], body(entry))
    if not campaign.poll_actions():
        raise AssertionError('first six positive-control creates did not complete')
    if before_last:
        before_last(fixture)
    fixture.cloud.lose_ack = True
    try:
        with provider_created(fixture, literal):
            campaign.create('server', body(plan[-1]))
    except mc.GuardError as error:
        if 'failed or ambiguous' not in str(error):
            raise
    else:
        raise AssertionError('whole server response loss did not interrupt the real create')
    finally:
        fixture.cloud.lose_ack = False
    if len(fixture.cloud.requests) != 7 or 104 not in fixture.cloud.rows['server']:
        raise AssertionError('response-loss fixture did not commit its seventh one-shot request')
    action = next(action for action in fixture.cloud.actions.values()
                  if action['command'] == 'create_server' and dict(type='server', id=104) in action['resources'])
    action['status'] = 'running'
    closing(fixture, campaign)
    campaign.recover()
    return journal, campaign, action


class OpaqueCreatedTest(unittest.TestCase):
    def test_far_past_and_future_ack_created_allow_actual_exact_cleanup_under_fixed_local_h(self):
        for literal in ('1900-01-01T00:00:00Z', '2099-12-31T23:59:59.123456+00:00'):
            with self.subTest(literal=literal), Initialization() as f:
                journal, campaign = armed(f)
                time_policy = copy.deepcopy(journal.header['time'])
                with provider_created(f, literal):
                    rid = campaign.create('network', f.plan()[0]['body'])
                record = journal.read()['owned'][0]
                self.assertEqual((record['kind'], record['id'], record['created']), ('network', rid, literal))
                self.assertEqual(journal.read()['intents'][0]['ack']['payload']['network']['created'], literal)
                self.assertTrue(campaign.poll_actions())
                closing(f, campaign)
                campaign.mutate('network', rid, 'delete')
                self.assertTrue(campaign.poll_actions())
                result = campaign.completion()
                self.assertTrue(result['complete'])
                self.assertTrue(result['baseline_equal'])
                self.assertTrue(result['hard_lifetime_met'])
                self.assertEqual([(method, path) for method, path, _, _ in f.cloud.requests],
                                 [('POST', '/networks'), ('DELETE', f'/networks/{rid}')])
                self.assertEqual(journal.header['time'], time_policy)
                self.assertNotIn('provider_clock_bound_ns', time_policy)
                self.assertEqual(journal.read()['mode'], 'COMPLETE')

    def test_missing_or_malformed_created_never_authorizes_ack_or_recovered_cleanup(self):
        for literal in (MISSING, None, '', 'not-a-time', '2023-11-14T22:13:30', True, 0):
            with self.subTest(literal=literal), Initialization() as f:
                journal, campaign = armed(f)
                with provider_created(f, literal), self.assertRaisesRegex(mc.GuardError, 'failed or ambiguous'):
                    campaign.create('network', f.plan()[0]['body'])
                self.assertEqual(len(f.cloud.requests), 1)
                self.assertEqual(journal.read()['owned'], [])
                self.assertEqual(journal.read()['intents'][0]['state'], 'UNKNOWN')
                closing(f, campaign)
                result = campaign.cleanup_pass()
                self.assertFalse(result['complete'])
                self.assertEqual(journal.read()['owned'], [])
                self.assertEqual(journal.read()['intents'][0]['state'], 'UNKNOWN')
                self.assertIn(201, f.cloud.rows['network'])
                self.assertEqual([(method, path) for method, path, _, _ in f.cloud.requests], [('POST', '/networks')])

    def test_changed_literal_created_refuses_even_when_the_instant_is_identical(self):
        with Initialization() as f:
            journal, campaign = armed(f)
            original = '1900-01-01T00:00:00Z'
            with provider_created(f, original):
                rid = campaign.create('network', f.plan()[0]['body'])
            campaign.poll_actions()
            closing(f, campaign)
            f.cloud.rows['network'][rid]['created'] = '1900-01-01T00:00:00+00:00'
            with self.assertRaisesRegex(mc.GuardError, 'identity|created|retired'):
                campaign.mutate('network', rid, 'delete')
            self.assertEqual([(method, path) for method, path, _, _ in f.cloud.requests], [('POST', '/networks')])
            self.assertIn(rid, f.cloud.rows['network'])
            self.assertEqual(journal.read()['owned'][0]['created'], original)

    def test_retirement_is_typed_and_never_protects_an_owned_server_with_same_numeric_id(self):
        with Initialization() as f:
            journal, campaign = armed(f)
            plan = f.plan()
            f.cloud.next_ids['network'] = 101
            network = campaign.create('network', plan[0]['body'])
            firewall = campaign.create('firewall', plan[1]['body'])
            key = campaign.create('ssh-key', plan[2]['body'])
            body = copy.deepcopy(plan[3]['body'])
            body['ssh_keys'], body['firewalls'] = [key], [dict(firewall=firewall)]
            server = campaign.create('server', body)
            self.assertEqual((network, server), (101, 101))
            campaign.poll_actions()
            closing(f, campaign)
            removed = f.cloud.rows['network'].pop(network)
            with self.assertRaisesRegex(mc.GuardError, 'absent'):
                campaign.mutate('network', network, 'delete')
            f.cloud.rows['network'][network] = removed
            with self.assertRaisesRegex(mc.GuardError, 'retired'):
                campaign.mutate('network', network, 'delete')
            campaign.mutate('server', server, 'delete')
            deletes = [(method, path) for method, path, _, _ in f.cloud.requests if method == 'DELETE']
            self.assertEqual(deletes, [('DELETE', '/servers/101')])
            self.assertIn(network, f.cloud.rows['network'])
            self.assertNotIn(server, f.cloud.rows['server'])
            self.assertNotIn(10101, f.cloud.rows['primary-ip'])
            retired = [(event['kind'], event['id']) for event in journal.read()['events']
                       if event.get('event') == 'observed-retirement']
            self.assertIn(('network', 101), retired)
            self.assertNotIn(('server', 101), retired)

    def test_observed_absence_permanently_retires_exact_typed_id_before_identical_reappearance(self):
        with Initialization() as f:
            journal, campaign = armed(f)
            rid = campaign.create('network', f.plan()[0]['body'])
            closing(f, campaign)
            original = f.cloud.rows['network'].pop(rid)
            # Keep the original acknowledgement unresolved so the absence census
            # cannot finish the campaign before its replacement reappears.
            result = campaign.completion()
            self.assertFalse(result['complete'])
            self.assertTrue(result['baseline_equal'])
            self.assertEqual(result['pending_intents'], [0])
            f.cloud.rows['network'][rid] = original
            with self.assertRaisesRegex(mc.GuardError, 'retired'):
                campaign.mutate('network', rid, 'delete')
            self.assertEqual([(method, path) for method, path, _, _ in f.cloud.requests], [('POST', '/networks')])
            self.assertIn(rid, f.cloud.rows['network'])
            self.assertEqual(journal.read()['owned'][0]['id'], rid)


class RecoveredServerAssociationTest(unittest.TestCase):
    def assert_owned_missing_ack(self, journal):
        state = journal.read()
        intent = state['intents'][6]
        self.assertIsNone(intent['ack'])
        self.assertIn('original_ack', intent)
        self.assertIsNone(intent['original_ack'])
        self.assertIs(intent['recovered'], True)
        self.assertEqual(intent['recovery']['origin'], 'conditional-nonce-recovery')
        self.assertIs(intent['recovery']['original_ack_missing'], True)
        self.assertEqual(intent['recovery']['candidate']['id'], 104)
        records = [record for record in state['owned'] if record['kind'] == 'server' and record['id'] == 104]
        self.assertEqual(len(records), 1)
        return intent, records[0]

    def terminal_action(self, fixture, journal, campaign, action):
        intent, _ = self.assert_owned_missing_ack(journal)
        self.assertEqual(intent['actions'], [action['id']])
        self.assertFalse(campaign.poll_actions())
        self.assertEqual(journal.read()['intents'][6]['state'], 'ACKNOWLEDGED')
        action['status'] = 'success'
        self.assertTrue(campaign.poll_actions())
        self.assertIn(action['id'], fixture.cloud.action_reads)
        self.assertEqual(journal.read()['intents'][6]['state'], 'DONE')

    def test_whole_server_response_loss_records_distinct_proof_then_real_seven_resource_cleanup(self):
        for literal in ('1900-01-01T00:00:00Z', '2099-12-31T23:59:59.123456+00:00'):
            with self.subTest(literal=literal), Initialization() as f:
                journal, campaign, action = response_lost_server(f, literal=literal)
                time_policy = copy.deepcopy(journal.header['time'])
                intent, record = self.assert_owned_missing_ack(journal)
                self.assertEqual((record['kind'], record['id'], record['created']), ('server', 104, literal))
                self.assertEqual(intent['recovery']['corroborating_create_action']['id'], action['id'])
                self.assertEqual(intent['actions'], [action['id']])
                self.assertEqual(intent['state'], 'ACKNOWLEDGED')
                self.assertEqual(len(journal.read()['owned']), 7)
                self.assertFalse(campaign.poll_actions())
                pending = campaign.completion()
                self.assertFalse(pending['complete'])
                self.assertEqual(pending['pending_intents'], [6])

                server = f.cloud.describe('server', 104)
                ips = f.cloud.list('primary-ip')
                exact_ip = copy.deepcopy(f.cloud.rows['primary-ip'][10104])
                describes_before = len(f.cloud.describes)
                with journal.locked() as state:
                    associations = campaign.cascade(state, server, ips)
                    journal.commit(state)
                self.assertEqual(len(associations), 1)
                self.assertIn(('primary-ip', 10104, 20), f.cloud.describes[describes_before:])
                expected_proof = dict(intent=6, server_kind='server', server_id=104,
                    server_nonce=record['labels']['intent'],
                    server_identity_sha256=cl.sha256({key: record[key] for key in
                                                     ('kind', 'id', 'name', 'labels', 'created', 'canonical')}),
                    server_description_sha256=cl.sha256(server), ip_census_sha256=cl.sha256(ips),
                    ip_description_sha256=cl.sha256(exact_ip),
                    baseline_ids_sha256=cl.sha256(journal.header['baseline']['primary-ip']),
                    pre_call_ids_sha256=cl.sha256(intent['pre_ids']['primary-ip']))
                expected = dict(id=10104, server_id=104, type='ipv4', auto_delete=True, created=literal,
                                origin='recovered-server-association', proof=expected_proof)
                self.assertEqual(associations[0], expected)
                persisted = [association for association in journal.read()['associations']
                             if association['server_id'] == 104]
                self.assertEqual(persisted, [expected])
                self.assertIsNone(journal.read()['intents'][6]['ack'])
                self.assertEqual(len(f.cloud.requests), 7)
                self.assertTrue(all(method == 'POST' for method, _, _, _ in f.cloud.requests))

                self.terminal_action(f, journal, campaign, action)
                result = campaign.cleanup_pass()
                self.assertTrue(result['complete'])
                self.assertTrue(result['baseline_equal'])
                self.assertTrue(result['hard_lifetime_met'])
                self.assertEqual(result['pending_intents'], [])
                deletes = [(method, path) for method, path, _, _ in f.cloud.requests if method == 'DELETE']
                self.assertEqual(deletes, [('DELETE', f'/servers/{rid}') for rid in (101, 102, 103, 104)] +
                                 [('DELETE', '/firewalls/301'), ('DELETE', '/ssh_keys/401'),
                                  ('DELETE', '/networks/201')])
                self.assertEqual(len(f.cloud.requests), 14)
                for rid in (10101, 10102, 10103, 10104):
                    self.assertNotIn(rid, f.cloud.rows['primary-ip'])
                for kind, ids in journal.header['baseline'].items():
                    self.assertEqual(sorted(f.cloud.rows[kind]), ids)
                final = journal.read()
                self.assertEqual(final['mode'], 'COMPLETE')
                self.assertIsNone(final['intents'][6]['ack'])
                self.assertIsNone(final['intents'][6]['original_ack'])
                self.assertEqual([association for association in final['associations']
                                  if association['server_id'] == 104], [expected])
                self.assertEqual({association['id']: association['origin'] for association in final['associations']},
                                 {10101: 'saved-create-response', 10102: 'saved-create-response',
                                  10103: 'saved-create-response', 10104: 'recovered-server-association'})
                readbacks = [event for event in final['events']
                             if event.get('event') == 'cascade-readback' and event['server_id'] == 104]
                self.assertGreaterEqual(len(readbacks), 2)
                self.assertIn(expected, readbacks[0]['proofs'])
                self.assertEqual(journal.header['time'], time_policy)

    def test_missing_ack_requires_independently_validated_recovery_origin_and_candidate(self):
        faults = ('origin', 'candidate-id', 'candidate-name', 'candidate-labels',
                  'candidate-created', 'candidate-canonical')
        for fault in faults:
            with self.subTest(fault=fault), Initialization() as f:
                journal, campaign, action = response_lost_server(f)
                self.terminal_action(f, journal, campaign, action)
                with journal.locked() as state:
                    recovery = state['intents'][6]['recovery']
                    if fault == 'origin':
                        recovery['origin'] = 'ordinary-selector'
                    elif fault == 'candidate-id':
                        recovery['candidate']['id'] = 777
                    elif fault == 'candidate-name':
                        recovery['candidate']['name'] = 'unrelated-server'
                    elif fault == 'candidate-labels':
                        nonce = recovery['candidate']['labels']['intent']
                        recovery['candidate']['labels']['intent'] = ('1' if nonce[0] == '0' else '0') + nonce[1:]
                    elif fault == 'candidate-created':
                        recovery['candidate']['created'] = '1900-01-01T00:00:00Z'
                    elif fault == 'candidate-canonical':
                        recovery['candidate']['server_type']['name'] = 'ccx33'
                    journal.commit(state)
                before = copy.deepcopy(f.cloud.requests)
                with self.assertRaisesRegex(mc.GuardError, 'creation provenance'):
                    campaign.mutate('server', 104, 'delete')
                self.assertEqual(f.cloud.requests, before)
                self.assertIn(104, f.cloud.rows['server'])
                self.assertIsNone(journal.read()['intents'][6]['ack'])
                self.assertEqual(len(journal.read()['owned']), 7)

    def test_independent_fresh_ip_and_server_graph_failures_refuse_actual_server_delete(self):
        faults = ('auto-delete-false', 'auto-delete-integer', 'wrong-ip-type', 'wrong-assignee-type', 'wrong-assignee-id',
                  'missing-ip-created', 'malformed-ip-created', 'missing-assigned-ip',
                  'unexpected-ipv6', 'missing-ipv4', 'floating-ip-attached', 'missing-public-field',
                  'foreign-volume', 'enabled-backups',
                  'missing-cascade-field', 'changed-ip-reference')
        for fault in faults:
            with self.subTest(fault=fault), Initialization() as f:
                journal, campaign, action = response_lost_server(f)
                self.terminal_action(f, journal, campaign, action)
                ip = f.cloud.rows['primary-ip'][10104]
                server = f.cloud.rows['server'][104]
                if fault == 'auto-delete-false':
                    ip['auto_delete'] = False
                elif fault == 'auto-delete-integer':
                    ip['auto_delete'] = 1
                elif fault == 'wrong-ip-type':
                    ip['type'] = 'ipv6'
                elif fault == 'wrong-assignee-type':
                    ip['assignee_type'] = 'load_balancer'
                elif fault == 'wrong-assignee-id':
                    ip['assignee_id'] = 777
                elif fault == 'missing-ip-created':
                    ip.pop('created')
                elif fault == 'malformed-ip-created':
                    ip['created'] = 'not-a-time'
                elif fault == 'missing-assigned-ip':
                    f.cloud.rows['primary-ip'].pop(10104)
                elif fault == 'unexpected-ipv6':
                    server['public_net']['ipv6'] = dict(id=88888, ip='2001:db8::/64')
                elif fault == 'missing-ipv4':
                    server['public_net']['ipv4'] = None
                elif fault == 'floating-ip-attached':
                    server['public_net']['floating_ips'] = [88888]
                elif fault == 'missing-public-field':
                    server['public_net'].pop('floating_ips')
                elif fault == 'foreign-volume':
                    server['volumes'] = [777]
                elif fault == 'enabled-backups':
                    server['backup_window'] = '00-04'
                elif fault == 'missing-cascade-field':
                    server.pop('volumes')
                elif fault == 'changed-ip-reference':
                    server['public_net']['ipv4']['id'] = 88888
                before = copy.deepcopy(f.cloud.requests)
                with self.assertRaises(mc.GuardError):
                    campaign.mutate('server', 104, 'delete')
                self.assertEqual(f.cloud.requests, before)
                self.assertIn(104, f.cloud.rows['server'])
                self.assert_owned_missing_ack(journal)
                self.assertFalse(any(method == 'DELETE' for method, _, _, _ in f.cloud.requests))

    def test_exact_ip_readback_must_exist_and_match_complete_census(self):
        for fault in ('missing', 'id', 'type', 'assignee_type', 'assignee_id', 'auto_delete',
                      'auto-delete-integer', 'created'):
            with self.subTest(fault=fault), Initialization() as f:
                journal, campaign, action = response_lost_server(f)
                self.terminal_action(f, journal, campaign, action)
                original = f.cloud.describe
                calls = []
                def describe(kind, rid, seconds=20):
                    value = original(kind, rid, seconds)
                    if kind != 'primary-ip' or rid != 10104:
                        return value
                    calls.append((kind, rid))
                    if fault == 'missing':
                        return None
                    if fault == 'auto-delete-integer':
                        value['auto_delete'] = 1
                        return value
                    value[fault] = {'id': 88888, 'type': 'ipv6', 'assignee_type': 'load_balancer',
                                    'assignee_id': 777, 'auto_delete': False,
                                    'created': '1900-01-01T00:00:00Z'}[fault]
                    return value
                before = copy.deepcopy(f.cloud.requests)
                with mock.patch.object(f.cloud, 'describe', side_effect=describe):
                    with self.assertRaises(mc.GuardError):
                        campaign.mutate('server', 104, 'delete')
                self.assertEqual(calls, [('primary-ip', 10104)])
                self.assertEqual(f.cloud.requests, before)
                self.assertIn(104, f.cloud.rows['server'])
                self.assert_owned_missing_ack(journal)
                if fault == 'missing':
                    retired = [event for event in journal.read()['events']
                               if event.get('event') == 'observed-retirement']
                    self.assertTrue(any(event['kind'] == 'primary-ip' and event['id'] == 10104
                                        and event['source'] == 'cascade-exact-readback' for event in retired))
                    self.assertFalse(any(event['kind'] == 'server' and event['id'] == 104 for event in retired))
                    # The readback fault is gone, but its absence observation is
                    # permanent. Exact reappearance cannot restore authority.
                    with self.assertRaisesRegex(mc.GuardError, 'retired'):
                        campaign.mutate('server', 104, 'delete')
                    self.assertEqual(f.cloud.requests, before)

    def test_complete_primary_ip_census_is_required_not_just_exact_description(self):
        for fault in ('duplicate-assignment', 'foreign-assignment', 'failed-list'):
            with self.subTest(fault=fault), Initialization() as f:
                journal, campaign, action = response_lost_server(f)
                self.terminal_action(f, journal, campaign, action)
                original = f.cloud.list
                def census(kind, seconds=20):
                    rows = original(kind, seconds)
                    if kind != 'primary-ip':
                        return rows
                    if fault == 'failed-list':
                        raise mc.GuardError('synthetic complete primary-IP census failed')
                    extra = copy.deepcopy(next(row for row in rows if row['id'] == 10104))
                    if fault == 'foreign-assignment':
                        extra['id'] = 88888
                    return rows + [extra]
                before = copy.deepcopy(f.cloud.requests)
                with mock.patch.object(f.cloud, 'list', side_effect=census):
                    with self.assertRaises(mc.GuardError):
                        campaign.mutate('server', 104, 'delete')
                self.assertEqual(f.cloud.requests, before)
                self.assertIn(104, f.cloud.rows['server'])
                self.assert_owned_missing_ack(journal)

    def test_changed_recorded_ip_created_cannot_rebase_recovered_association(self):
        with Initialization() as f:
            journal, campaign, action = response_lost_server(f, literal='1900-01-01T00:00:00Z')
            self.terminal_action(f, journal, campaign, action)
            with journal.locked() as state:
                campaign.cascade(state, f.cloud.describe('server', 104), f.cloud.list('primary-ip'))
                journal.commit(state)
            original = copy.deepcopy(next(association for association in journal.read()['associations']
                                          if association['server_id'] == 104))
            f.cloud.rows['primary-ip'][10104]['created'] = '1900-01-01T00:00:00+00:00'
            before = copy.deepcopy(f.cloud.requests)
            with self.assertRaisesRegex(mc.GuardError, 'association|identity|created'):
                campaign.mutate('server', 104, 'delete')
            self.assertEqual(f.cloud.requests, before)
            self.assertEqual([association for association in journal.read()['associations']
                              if association['server_id'] == 104], [original])
            self.assert_owned_missing_ack(journal)

    def test_recovered_association_requires_a_new_implicit_ip_in_the_durable_request(self):
        with Initialization() as f:
            journal, campaign, action = response_lost_server(f)
            self.terminal_action(f, journal, campaign, action)
            with journal.locked() as state:
                state['intents'][6]['descriptor']['public_net']['ipv4'] = 10104
                journal.commit(state)
            before = copy.deepcopy(f.cloud.requests)
            with self.assertRaisesRegex(mc.GuardError, 'implicit IP allocation differs from sealed request'):
                campaign.mutate('server', 104, 'delete')
            self.assertEqual(f.cloud.requests, before)
            self.assert_owned_missing_ack(journal)
            self.assertEqual(journal.header['plan'][6]['descriptor']['public_net'],
                             dict(enable_ipv4=True, enable_ipv6=False))

    def test_pre_call_ip_conflict_retains_server_provenance_but_refuses_its_delete(self):
        def preexisting(f):
            f.cloud.rows['primary-ip'][10104] = dict(id=10104, type='ipv4', assignee_type='server',
                assignee_id=777, auto_delete=True, created='1900-01-01T00:00:00Z')
        with Initialization() as f:
            journal, campaign, action = response_lost_server(f, before_last=preexisting)
            intent, _ = self.assert_owned_missing_ack(journal)
            self.assertIn(10104, intent['pre_ids']['primary-ip'])
            self.assertNotIn(10104, journal.header['baseline']['primary-ip'])
            self.terminal_action(f, journal, campaign, action)
            before = copy.deepcopy(f.cloud.requests)
            with self.assertRaisesRegex(mc.GuardError, 'protected|pre.call|attributable'):
                campaign.mutate('server', 104, 'delete')
            self.assertEqual(f.cloud.requests, before)
            self.assert_owned_missing_ack(journal)

    def test_baseline_ip_conflict_retains_server_provenance_but_refuses_its_delete(self):
        with Initialization() as f:
            row = next(iter(f.cloud.rows['primary-ip'].values()))
            f.cloud.rows['primary-ip'] = {10104: dict(row, id=10104)}
            f.baseline = copy.deepcopy(f.cloud.rows)
            f.private('inventory/before-primary-ip.json', json.dumps(list(f.baseline['primary-ip'].values())) + '\n')
            # Isolate baseline/pre-call protection from the additional CLI policy.
            f.protect.write_text('\n'.join(f'{kind}\t{rid}\t{item["name"]}'
                for kind, rows in f.baseline.items() if kind != 'primary-ip'
                for rid, item in rows.items()) + '\n')
            f.args = provision.parse_args(f.argv)
            journal, campaign, action = response_lost_server(f)
            intent, _ = self.assert_owned_missing_ack(journal)
            self.assertIn(10104, journal.header['baseline']['primary-ip'])
            self.assertIn(10104, intent['pre_ids']['primary-ip'])
            self.terminal_action(f, journal, campaign, action)
            before = copy.deepcopy(f.cloud.requests)
            with self.assertRaisesRegex(mc.GuardError, 'protected|pre.call|attributable'):
                campaign.mutate('server', 104, 'delete')
            self.assertEqual(f.cloud.requests, before)
            self.assert_owned_missing_ack(journal)


if __name__ == '__main__':
    unittest.main()
