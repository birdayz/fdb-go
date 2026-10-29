#!/usr/bin/env python3
"""Fail-closed dependency, cascade and recovery admission controls.

Campaign, Journal, protection binding and cleanup are real. Only the synthetic
provider's census/exact replies and process/clock boundaries are substituted.
"""
import contextlib
import copy
import os
from pathlib import Path
import sys
import unittest
from unittest.mock import patch

HERE = Path(os.path.abspath(__file__)).parent
sys.path.insert(0, str(HERE))

import campaign_cloud as cc  # noqa: E402
import campaign_safety as cs  # noqa: E402
import creation_ledger as cl  # noqa: E402
import mn_common as mc  # noqa: E402
from test_campaign_safety import Fixture, NS, utc  # noqa: E402


@contextlib.contextmanager
def provider_reads(f, census=None, exact=None):
    """Observe only provider boundaries, retaining actual allowance evaluation."""
    events = []
    listing, describing, requesting = f.cloud.list, f.cloud.describe, f.cloud.request
    def listed(kind, seconds=20):
        rows = listing(kind, seconds)
        if census is not None:
            census(kind, rows)
        events.append(('list', kind, copy.deepcopy(rows)))
        return rows
    def described(kind, rid, seconds=20):
        item = describing(kind, rid, seconds)
        if exact is not None:
            exact(kind, rid, item)
        events.append(('describe', kind, rid, copy.deepcopy(item)))
        return item
    def requested(method, path, body=None, seconds=20):
        events.append(('request', method, path))
        return requesting(method, path, body, seconds)
    with patch.object(f.cloud, 'list', side_effect=listed), \
            patch.object(f.cloud, 'describe', side_effect=described), \
            patch.object(f.cloud, 'request', side_effect=requested):
        yield events


class CampaignAdmissionTest(unittest.TestCase):
    def setUp(self):
        self.stack = contextlib.ExitStack()
        self.addCleanup(self.stack.close)
        self.stack.enter_context(patch.dict(os.environ, {}, clear=True))
        self.stack.enter_context(patch.object(cc.http.client.HTTPSConnection, 'connect',
            side_effect=AssertionError('admission controls must never use a real network')))
        self.stack.enter_context(patch.object(cs.os, 'pidfd_open', create=True,
            side_effect=AssertionError('admission controls must never open a real pidfd')))
        self.stack.enter_context(patch.object(cs.signal, 'pidfd_send_signal', create=True,
            side_effect=AssertionError('admission controls must never signal a real process')))
        self.stack.enter_context(patch.object(cs.os, 'kill',
            side_effect=AssertionError('admission controls must never use a bare PID signal')))

    def state(self, f):
        with contextlib.closing(cl.Journal(f.journal.path)) as journal:
            return journal.read()

    def retired(self, f, kind, rid):
        rows = [event for event in self.state(f)['events'] if event.get('event') == 'observed-retirement'
                and event.get('kind') == kind and event.get('id') == rid]
        self.assertEqual(len(rows), 1)
        return rows[0]

    def dependencies(self, f):
        f.dependencies()
        self.assertTrue(f.campaign.poll_actions())
        self.assertEqual([(method, path) for method, path, _, _ in f.cloud.requests],
                         [('POST', '/firewalls'), ('POST', '/ssh_keys')])
        return {'firewall': 301, 'ssh-key': 401}

    def assert_no_server_dispatch(self, f, before):
        self.assertEqual(f.cloud.requests, before)
        state = self.state(f)
        self.assertFalse(any(intent['kind'] == 'server' for intent in state['intents']))
        self.assertFalse(any(record['kind'] == 'server' for record in state['owned']))
        self.assertIsNone(state['active_lease'])
        self.assertEqual(sorted(f.cloud.rows['server']), f.header['baseline']['server'])

    def test_retired_dependency_cannot_be_reused_after_absent_or_drifted_census_is_restored(self):
        for kind in ('ssh-key', 'firewall'):
            for observation in ('absent', 'created-drift'):
                with self.subTest(kind=kind, observation=observation), Fixture() as f:
                    rid = self.dependencies(f)[kind]
                    original = copy.deepcopy(f.cloud.rows[kind][rid])
                    if observation == 'absent':
                        f.cloud.rows[kind].pop(rid)
                    else:
                        f.cloud.rows[kind][rid]['created'] = utc(f.wall + NS)
                    f.campaign.recover()  # A real complete census durably observes the break.
                    event = self.retired(f, kind, rid)
                    self.assertTrue(event['source'].startswith('recovery-census'))
                    f.cloud.rows[kind][rid] = copy.deepcopy(original)
                    self.assertEqual(f.cloud.rows[kind][rid], original)
                    before = copy.deepcopy(f.cloud.requests)
                    with provider_reads(f) as events:
                        with self.assertRaisesRegex(mc.GuardError, 'observed-retired ID cannot inherit ownership'):
                            f.create('server')
                    self.assert_no_server_dispatch(f, before)
                    reads = [entry for entry in events if entry[:3] == ('describe', kind, rid)]
                    self.assertEqual(len(reads), 1)
                    self.assertEqual(reads[0][3], original, 'original-looking exact bytes cannot restore authority')
                    self.retired(f, kind, rid)
                    self.assertTrue(any(record['kind'] == kind and record['id'] == rid
                                        for record in self.state(f)['owned']), 'retained provenance is not authority')

    def test_current_precall_census_or_exact_dependency_drift_blocks_server_before_intent(self):
        for kind in ('ssh-key', 'firewall'):
            for boundary in ('precreate-census', 'fresh-exact'):
                with self.subTest(kind=kind, boundary=boundary), Fixture() as f:
                    rid = self.dependencies(f)[kind]
                    original = copy.deepcopy(f.cloud.rows[kind][rid])
                    before = copy.deepcopy(f.cloud.requests)
                    def census(resource_kind, rows):
                        if boundary == 'precreate-census' and resource_kind == kind:
                            next(row for row in rows if row['id'] == rid)['created'] = utc(f.wall + NS)
                    def exact(resource_kind, resource_id, item):
                        if boundary == 'fresh-exact' and (resource_kind, resource_id) == (kind, rid):
                            item['created'] = utc(f.wall + NS)
                    with provider_reads(f, census, exact) as events:
                        with self.assertRaises(mc.GuardError):
                            f.create('server')
                    self.assert_no_server_dispatch(f, before)
                    self.retired(f, kind, rid)
                    self.assertEqual(f.cloud.rows[kind][rid], original, 'only a provider reply was inconsistent')
                    lists = [index for index, event in enumerate(events) if event[0] == 'list']
                    described = [index for index, event in enumerate(events) if event[:3] == ('describe', kind, rid)]
                    self.assertEqual(len(lists), len(mc.PROTECT_KINDS))
                    self.assertEqual(len(described), 1)
                    self.assertGreater(described[0], max(lists), 'dependency authorization must follow the fresh census')
                    supplied = events[described[0]][3]
                    self.assertEqual(supplied['created'], original['created'] if boundary == 'precreate-census'
                                     else utc(f.wall + NS))

    def test_healthy_server_create_exactly_authorizes_both_dependencies_after_census_before_post(self):
        with Fixture() as f:
            self.dependencies(f)
            with provider_reads(f) as events:
                server = f.create('server')
            self.assertEqual(server, 101)
            operations = [entry[0] for entry in events]
            self.assertEqual(operations, ['list'] * len(mc.PROTECT_KINDS) + ['describe', 'describe', 'request'])
            self.assertEqual([entry[1] for entry in events[:10]], list(mc.PROTECT_KINDS))
            self.assertEqual([entry[1:3] for entry in events[10:12]], [('ssh-key', 401), ('firewall', 301)])
            self.assertEqual(events[-1], ('request', 'POST', '/servers'))
            self.assertTrue(f.campaign.poll_actions())
            f.closing()
            result = f.campaign.cleanup_pass()
            self.assertTrue(result['complete'])
            self.assertTrue(result['hard_lifetime_met'])
            self.assertEqual(f.deletes(), [('DELETE', '/servers/101'), ('DELETE', '/firewalls/301'),
                                          ('DELETE', '/ssh_keys/401')])

    def test_exact_primary_ip_name_protection_is_not_hidden_by_incomplete_or_unprotected_census(self):
        fields = ('id', 'type', 'assignee_type', 'assignee_id', 'auto_delete', 'created')
        for census_name in ('omitted', 'unprotected'):
            for exact_name in ('keep-ip', 'ordinary-ip', None):
                with self.subTest(census_name=census_name, exact_name=exact_name), Fixture() as f:
                    server = f.create('server')
                    self.assertTrue(f.campaign.poll_actions())
                    ip = server + 10000
                    if exact_name is None:
                        f.cloud.rows['primary-ip'][ip].pop('name', None)
                    else:
                        f.cloud.rows['primary-ip'][ip]['name'] = exact_name
                    original = copy.deepcopy(f.cloud.rows['primary-ip'][ip])
                    policy = cs.protection_policy(names=['keep-ip'])
                    f.campaign.bind_protections(policy)
                    self.assertEqual(self.state(f)['protections'], policy)
                    f.closing()
                    before = copy.deepcopy(f.cloud.requests)
                    supplied = []
                    def census(kind, rows):
                        if kind == 'primary-ip':
                            row = next(row for row in rows if row['id'] == ip)
                            if census_name == 'omitted':
                                row.pop('name', None)
                            else:
                                row['name'] = 'not-protected-in-census'
                            supplied.append(copy.deepcopy(row))
                    with provider_reads(f, census=census) as events:
                        if exact_name == 'ordinary-ip':
                            f.campaign.mutate('server', server, 'delete')
                        else:
                            message = 'additional guard protection' if exact_name == 'keep-ip' else 'exact primary-IP name'
                            with self.assertRaisesRegex(mc.GuardError, message):
                                f.campaign.mutate('server', server, 'delete')
                    exact = [entry[3] for entry in events if entry[:3] == ('describe', 'primary-ip', ip)]
                    self.assertEqual(exact, [original])
                    self.assertEqual(len(supplied), 1)
                    self.assertEqual({key: supplied[0][key] for key in fields}, {key: original[key] for key in fields})
                    self.assertEqual({key: exact[0][key] for key in fields}, {key: original[key] for key in fields})
                    if exact_name == 'ordinary-ip':
                        self.assertEqual(f.deletes(), [('DELETE', '/servers/101')])
                        self.assertNotIn(ip, f.cloud.rows['primary-ip'])
                        self.assertEqual(self.state(f)['associations'][0]['id'], ip)
                    else:
                        self.assertEqual(f.cloud.requests, before)
                        self.assertEqual(f.deletes(), [])
                        self.assertIn(server, f.cloud.rows['server'])
                        self.assertEqual(f.cloud.rows['primary-ip'][ip], original)
                        self.assertEqual(self.state(f)['associations'], [])

    def test_first_association_server_one_rejects_boolean_assignee_on_either_readback(self):
        for census_boolean, exact_boolean in ((False, False), (True, False), (False, True), (True, True)):
            with self.subTest(census_boolean=census_boolean, exact_boolean=exact_boolean), Fixture() as f:
                f.cloud.next_ids['server'] = 1
                server = f.create('server')
                self.assertEqual(server, 1)
                self.assertTrue(f.campaign.poll_actions())
                ip = 10001
                original = copy.deepcopy(f.cloud.rows['primary-ip'][ip])
                self.assertIs(type(original['assignee_id']), int)
                self.assertEqual(original['assignee_id'], 1)
                self.assertIsInstance(original.get('name'), str, 'shared healthy provider must supply its IP name')
                self.assertEqual(self.state(f)['associations'], [], 'must exercise the very first association')
                f.closing()
                before = copy.deepcopy(f.cloud.requests)
                supplied_census, supplied_exact = [], []
                def census(kind, rows):
                    if kind == 'primary-ip':
                        row = next(row for row in rows if row['id'] == ip)
                        if census_boolean:
                            row['assignee_id'] = True
                        supplied_census.append(copy.deepcopy(row))
                def exact(kind, rid, item):
                    if (kind, rid) == ('primary-ip', ip):
                        if exact_boolean:
                            item['assignee_id'] = True
                        supplied_exact.append(copy.deepcopy(item))
                with provider_reads(f, census, exact):
                    if census_boolean or exact_boolean:
                        with self.assertRaisesRegex(mc.GuardError, 'primary-IP assignee'):
                            f.campaign.mutate('server', 1, 'delete')
                    else:
                        f.campaign.mutate('server', 1, 'delete')
                self.assertEqual(len(supplied_census), 1)
                self.assertIs(type(supplied_census[0]['assignee_id']), bool if census_boolean else int)
                if census_boolean:
                    self.assertEqual(supplied_exact, [], 'malformed census attribution must refuse before the exact read')
                else:
                    self.assertEqual(len(supplied_exact), 1)
                    self.assertIs(type(supplied_exact[0]['assignee_id']), bool if exact_boolean else int)
                if census_boolean or exact_boolean:
                    self.assertEqual(f.cloud.requests, before)
                    self.assertEqual(f.deletes(), [])
                    self.assertEqual(self.state(f)['associations'], [])
                    self.assertIn(1, f.cloud.rows['server'])
                    self.assertEqual(f.cloud.rows['primary-ip'][ip], original)
                else:
                    self.assertEqual(f.deletes(), [('DELETE', '/servers/1')])
                    self.assertNotIn(ip, f.cloud.rows['primary-ip'])
                    association = self.state(f)['associations'][0]
                    self.assertIs(type(association['server_id']), int)
                    self.assertEqual((association['server_id'], association['id']), (1, ip))

    def test_persisted_planned_name_conflict_refuses_before_any_server_post(self):
        for pattern in ('test-node0', 'test-node?', 'unrelated-name'):
            with self.subTest(pattern=pattern), Fixture() as f:
                self.dependencies(f)
                policy = cs.protection_policy(names=[pattern])
                f.campaign.bind_protections(policy)
                self.assertEqual(self.state(f)['protections'], policy)
                before = copy.deepcopy(f.cloud.requests)
                if pattern == 'unrelated-name':
                    self.assertEqual(f.create('server'), 101)
                    self.assertEqual([(method, path) for method, path, _, _ in f.cloud.requests[len(before):]],
                                     [('POST', '/servers')])
                else:
                    with self.assertRaisesRegex(mc.GuardError, 'additional guard protection conflicts with a planned name'):
                        f.create('server')
                    self.assert_no_server_dispatch(f, before)
                    self.assertEqual(self.state(f)['protections'], policy)

    def test_protected_lost_or_adverse_ack_candidate_stays_unowned_with_evidence_while_unrelated_policy_allows_recovery(self):
        for outcome in ('lost-ack', 'adverse-ack'):
            for protection in ('name', 'global-id', 'typed-id', 'unrelated'):
                with self.subTest(outcome=outcome, protection=protection), Fixture() as f:
                    self.dependencies(f)
                    sequence = len(self.state(f)['intents'])
                    adverse = []
                    request = f.cloud.request
                    def response(method, path, body=None, seconds=20):
                        reply = request(method, path, body, seconds)
                        if method == 'POST' and path == '/servers':
                            reply['payload'] = {}
                            reply['body_sha256'] = cc.digest(cc.wire_json({}))
                            adverse.append(copy.deepcopy(reply))
                        return reply
                    if outcome == 'lost-ack':
                        f.cloud.lose_ack = True
                        with self.assertRaisesRegex(mc.GuardError, 'failed or ambiguous'):
                            f.create('server')
                        f.cloud.lose_ack = False
                    else:
                        with patch.object(f.cloud, 'request', side_effect=response):
                            with self.assertRaisesRegex(mc.GuardError, 'failed or ambiguous'):
                                f.create('server')
                    interrupted = self.state(f)['intents'][sequence]
                    original_ack = copy.deepcopy(interrupted['ack'])
                    self.assertEqual(interrupted['state'], 'UNKNOWN')
                    self.assertIsNone(interrupted['resource_id'])
                    self.assertIs(interrupted['required_action_identified'], False)
                    if outcome == 'lost-ack':
                        self.assertIsNone(original_ack)
                    else:
                        self.assertEqual(adverse, [original_ack])
                        self.assertEqual(original_ack['payload'], {})
                    server = 101
                    self.assertIn(server, f.cloud.rows['server'])
                    name = f.cloud.rows['server'][server]['name']
                    policies = {
                        'name': cs.protection_policy(names=[name]),
                        'global-id': cs.protection_policy(ids=[server]),
                        'typed-id': cs.protection_policy(by_kind={'server': [server]}),
                        'unrelated': cs.protection_policy(names=['test-node3'], ids=[9090],
                                                         by_kind={'primary-ip': [server]}),
                    }
                    policy = policies[protection]
                    f.closing()
                    f.campaign.bind_protections(policy)
                    self.assertEqual(self.state(f)['protections'], policy)
                    before = copy.deepcopy(f.cloud.requests)
                    for attempt in range(2):
                        f.campaign.recover()
                        state = self.state(f)
                        intent = state['intents'][sequence]
                        self.assertEqual(intent['ack'], original_ack)
                        self.assertEqual(f.cloud.requests, before, 'read-only recovery must not resend the create')
                        if protection == 'unrelated':
                            self.assertEqual((intent['resource_id'], intent['state']), (server, 'ACKNOWLEDGED'))
                            self.assertEqual(intent['original_ack'], original_ack)
                            self.assertEqual([r['id'] for r in state['owned'] if r['kind'] == 'server'], [server])
                        else:
                            self.assertEqual(intent['state'], 'UNKNOWN')
                            self.assertIsNone(intent['resource_id'])
                            self.assertNotIn('recovery', intent)
                            self.assertFalse(any(record['kind'] == 'server' for record in state['owned']))
                            refused = [event for event in state['events'] if event.get('event') == 'recovery-candidate-refused'
                                       and event.get('kind') == 'server' and event.get('id') == server]
                            self.assertEqual(len(refused), attempt + 1)
                            censuses = [event for event in state['events'] if event.get('event') == 'recovery-census'
                                        and event.get('intent') == sequence]
                            self.assertEqual(len(censuses), attempt + 1)
                            self.assertEqual(censuses[-1]['candidates'], [])
                            self.assertRegex(censuses[-1]['census_sha256'], r'^[0-9a-f]{64}$')
                    result = f.campaign.cleanup_pass()
                    final = self.state(f)
                    self.assertEqual(final['intents'][sequence]['ack'], original_ack)
                    if protection == 'unrelated':
                        self.assertTrue(result['complete'])
                        self.assertTrue(result['baseline_equal'])
                        self.assertTrue(result['hard_lifetime_met'])
                        self.assertEqual(f.deletes(), [('DELETE', '/servers/101'), ('DELETE', '/firewalls/301'),
                                                      ('DELETE', '/ssh_keys/401')])
                    else:
                        self.assertFalse(result['complete'])
                        self.assertFalse(result['baseline_equal'])
                        self.assertEqual(result['pending_intents'], [sequence])
                        self.assertEqual(final['intents'][sequence]['state'], 'UNKNOWN')
                        self.assertIsNone(final['intents'][sequence]['resource_id'])
                        self.assertFalse(any(record['kind'] == 'server' for record in final['owned']))
                        self.assertNotIn(('DELETE', '/servers/101'), f.deletes())
                        self.assertIn(server, f.cloud.rows['server'])


if __name__ == '__main__':
    unittest.main()
