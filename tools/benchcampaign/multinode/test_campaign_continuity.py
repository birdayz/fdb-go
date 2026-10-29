"""Permanent identity continuity, required-action evidence and lifetime history.

Real Campaign/Journal and actual cleanup are exercised. Synthetic provider,
process, clock and in-memory wire boundaries come from the existing fixtures;
no authorization, recovery, retirement or completion method is replaced.
"""
import contextlib
import copy
import json
import unittest
from unittest.mock import patch

import campaign_cloud as cc
import campaign_safety as cs
import creation_ledger as cl
import mn_common as mc
from test_campaign_acknowledgements import WireMutations
from test_campaign_safety import Fixture, NS, T0, utc


class CampaignContinuityTest(unittest.TestCase):
    def setUp(self):
        self.stack = contextlib.ExitStack()
        self.addCleanup(self.stack.close)
        self.stack.enter_context(patch.object(cc.http.client.HTTPSConnection, 'connect',
            side_effect=AssertionError('continuity controls must never open a real network connection')))
        self.stack.enter_context(patch.object(cs.os, 'pidfd_open', create=True,
            side_effect=AssertionError('continuity controls must never open a real pidfd')))
        self.stack.enter_context(patch.object(cs.signal, 'pidfd_send_signal', create=True,
            side_effect=AssertionError('continuity controls must never signal a real process')))

    def fresh_state(self, fixture):
        with contextlib.closing(cl.Journal(fixture.journal.path)) as journal:
            return journal.read()

    def assert_retired(self, fixture, kind, rid):
        state = self.fresh_state(fixture)
        events = [event for event in state['events'] if event.get('event') == 'observed-retirement'
                  and event.get('kind') == kind and event.get('id') == rid]
        self.assertEqual(len(events), 1, 'observed identity discontinuity must be permanently durable')
        self.assertIs(type(events[0]['id']), int)
        self.assertTrue(events[0].get('source'))
        return state

    def assert_unknown_required_action(self, fixture, sequence, sibling):
        state = self.fresh_state(fixture)
        intent = state['intents'][sequence]
        self.assertEqual(intent['state'], 'UNKNOWN')
        self.assertEqual(intent['actions'], [sibling])
        self.assertIs(intent.get('required_action_identified'), False,
                      'a parseable sibling is not the missing required operation action')
        self.assertEqual(intent['ack']['payload']['next_actions'][0]['id'], sibling)
        self.assertIsNone(state['active_lease'])
        return intent

    def test_owned_identity_drift_is_permanent_after_original_looking_readback_returns(self):
        for drift in ('name', 'labels', 'created', 'canonical'):
            with self.subTest(drift=drift), Fixture() as f:
                network = f.create()
                ssh = f.create('ssh-key')
                self.assertEqual((network, ssh), (201, 401))
                self.assertTrue(f.campaign.poll_actions())
                f.closing()
                original = copy.deepcopy(f.cloud.rows['network'][network])
                owned = copy.deepcopy(next(row for row in f.journal.read()['owned'] if row['kind'] == 'network'))
                current = f.cloud.rows['network'][network]
                if drift == 'name': current['name'] = 'test-replacement'
                if drift == 'labels': current['labels']['intent'] = 'different-observed-intent'
                if drift == 'created': current['created'] = utc(T0 + 11 * NS)
                if drift == 'canonical': current['ip_range'] = '10.99.0.0/24'
                before = copy.deepcopy(f.cloud.requests)
                with self.assertRaises(mc.GuardError):
                    f.campaign.mutate('network', network, 'delete')
                retired = self.assert_retired(f, 'network', network)
                self.assertEqual(next(row for row in retired['owned'] if row['kind'] == 'network'), owned)
                self.assertEqual(f.cloud.requests, before)
                f.cloud.rows['network'][network] = copy.deepcopy(original)
                self.assertEqual(f.cloud.rows['network'][network], original)
                with self.assertRaisesRegex(mc.GuardError, 'observed-retired ID cannot inherit ownership'):
                    f.campaign.mutate('network', network, 'delete')
                self.assertEqual(f.cloud.requests, before)
                result = f.campaign.cleanup_pass()
                self.assertEqual(f.deletes(), [('DELETE', '/ssh_keys/401')])
                self.assertFalse(result['complete'])
                self.assertEqual(result['owned_survivors'], [dict(kind='network', id=network)])
                self.assertEqual(f.cloud.rows['network'][network], original)
                self.assert_retired(f, 'network', network)

    def test_recorded_primary_ip_identity_drift_survives_restored_original_looking_bytes(self):
        with Fixture() as f:
            server = f.create('server')
            self.assertEqual(server, 101)
            self.assertTrue(f.campaign.poll_actions())
            # Register the association through the real cascade proof, without
            # sending a DELETE or inventing an association record in the test.
            with f.journal.locked() as state:
                associations = f.campaign.cascade(state, f.cloud.describe('server', server),
                                                  f.cloud.list('primary-ip'))
                f.journal.commit(state)
            self.assertEqual([row['id'] for row in associations], [10101])
            saved_association = copy.deepcopy(associations[0])
            f.closing()
            original = copy.deepcopy(f.cloud.rows['primary-ip'][10101])
            f.cloud.rows['primary-ip'][10101]['created'] = utc(T0 + 11 * NS)
            before = copy.deepcopy(f.cloud.requests)
            with self.assertRaises(mc.GuardError):
                f.campaign.mutate('server', server, 'delete')
            retired = self.assert_retired(f, 'primary-ip', 10101)
            self.assertEqual(retired['associations'], [saved_association])
            self.assertFalse(any(event.get('event') == 'observed-retirement' and event.get('kind') == 'server'
                                 and event.get('id') == server for event in retired['events']))
            self.assertEqual(f.cloud.requests, before)
            f.cloud.rows['primary-ip'][10101] = copy.deepcopy(original)
            with self.assertRaisesRegex(mc.GuardError, 'observed-retired IP cannot inherit association'):
                f.campaign.mutate('server', server, 'delete')
            self.assertEqual(f.cloud.requests, before)
            result = f.campaign.cleanup_pass()
            self.assertFalse(result['complete'])
            self.assertEqual(result['owned_survivors'], [dict(kind='server', id=server)])
            self.assertEqual(result['subordinate_survivors'], 1)
            self.assertNotIn(('DELETE', '/servers/101'), f.deletes())
            self.assertEqual(f.deletes(), [('DELETE', '/firewalls/301'), ('DELETE', '/ssh_keys/401')])
            self.assertIn(server, f.cloud.rows['server'])
            self.assertEqual(f.cloud.rows['primary-ip'][10101], original)
            self.assert_retired(f, 'primary-ip', 10101)

    def record_real_ip_association(self, fixture, server):
        with fixture.journal.locked() as state:
            associations = fixture.campaign.cascade(state, fixture.cloud.describe('server', server),
                                                     fixture.cloud.list('primary-ip'))
            fixture.journal.commit(state)
        self.assertEqual([row['id'] for row in associations], [10101])
        return copy.deepcopy(associations[0])

    def assert_retired_ip_blocks_restored_server_but_unrelated_cleanup_executes(self, fixture, server, original_ip):
        before = copy.deepcopy(fixture.cloud.requests)
        with self.assertRaisesRegex(mc.GuardError, 'observed-retired IP cannot inherit association'):
            fixture.campaign.mutate('server', server, 'delete')
        self.assertEqual(fixture.cloud.requests, before)
        result = fixture.campaign.cleanup_pass()
        self.assertFalse(result['complete'])
        self.assertEqual(result['owned_survivors'], [dict(kind='server', id=server)])
        self.assertEqual(result['subordinate_survivors'], 1)
        self.assertEqual(fixture.deletes(), [('DELETE', '/firewalls/301'), ('DELETE', '/ssh_keys/401')])
        self.assertNotIn(('DELETE', '/servers/101'), fixture.deletes())
        self.assertIn(server, fixture.cloud.rows['server'])
        self.assertEqual(fixture.cloud.rows['primary-ip'][10101], original_ip)
        self.assert_retired(fixture, 'primary-ip', 10101)

    def test_server_ip_reference_drift_is_permanent_from_direct_cascade_and_exact_server_read(self):
        for boundary in ('direct-cascade', 'exact-server-read'):
            for drift in ('changed-id', 'missing-ipv4', 'missing-id'):
                with self.subTest(boundary=boundary, drift=drift), Fixture() as f:
                    server = f.create('server')
                    self.assertTrue(f.campaign.poll_actions())
                    association = self.record_real_ip_association(f, server)
                    original_server = copy.deepcopy(f.cloud.rows['server'][server])
                    original_ip = copy.deepcopy(f.cloud.rows['primary-ip'][10101])
                    f.closing()
                    changed = f.cloud.rows['server'][server]
                    if drift == 'changed-id': changed['public_net']['ipv4']['id'] = 20202
                    if drift == 'missing-ipv4': changed['public_net'].pop('ipv4')
                    if drift == 'missing-id': changed['public_net']['ipv4'].pop('id')
                    before = copy.deepcopy(f.cloud.requests)
                    # IP-side identity and association census remain entirely
                    # original; only the supplied server reference is different.
                    self.assertEqual(f.cloud.rows['primary-ip'][10101], original_ip)
                    if boundary == 'direct-cascade':
                        with f.journal.locked() as state:
                            with self.assertRaises(mc.GuardError):
                                f.campaign.cascade(state, copy.deepcopy(changed), f.cloud.list('primary-ip'))
                    else:
                        with self.assertRaises(mc.GuardError):
                            f.campaign.mutate('server', server, 'delete')
                    retired = self.assert_retired(f, 'primary-ip', 10101)
                    self.assertEqual(retired['associations'], [association])
                    self.assertEqual(f.cloud.requests, before)
                    self.assertEqual(f.cloud.rows['primary-ip'][10101], original_ip)
                    f.cloud.rows['server'][server] = copy.deepcopy(original_server)
                    self.assertEqual(f.cloud.rows['server'][server], original_server)
                    self.assert_retired_ip_blocks_restored_server_but_unrelated_cleanup_executes(
                        f, server, original_ip)

    def test_completed_server_census_retires_reference_drift_before_later_kind_failure(self):
        for drift in ('changed-id', 'missing-ipv4', 'missing-id'):
            with self.subTest(drift=drift), Fixture() as f:
                server = f.create('server')
                self.assertTrue(f.campaign.poll_actions())
                association = self.record_real_ip_association(f, server)
                original_server = copy.deepcopy(f.cloud.rows['server'][server])
                original_ip = copy.deepcopy(f.cloud.rows['primary-ip'][10101])
                f.closing()
                before = copy.deepcopy(f.cloud.requests)
                f.cloud.lists.clear()
                f.cloud.describes.clear()
                supplied, failures = [], []
                listed = f.cloud.list
                with contextlib.closing(cl.Journal(f.journal.path)) as reader:
                    def inventory(kind, seconds=20):
                        rows = listed(kind, seconds)
                        if kind == 'server':
                            item = next(row for row in rows if row['id'] == server)
                            if drift == 'changed-id': item['public_net']['ipv4']['id'] = 20202
                            if drift == 'missing-ipv4': item['public_net'].pop('ipv4')
                            if drift == 'missing-id': item['public_net']['ipv4'].pop('id')
                            supplied.append(copy.deepcopy(item))
                        return rows
                    def later_kind(kind, seconds):
                        if kind == 'network':
                            failures.append(reader.peek())
                            raise mc.GuardError('synthetic later census failure after server reference observation')
                    with patch.object(f.cloud, 'list', side_effect=inventory), \
                         patch.object(f.cloud, 'before_list', side_effect=later_kind):
                        with self.assertRaisesRegex(mc.GuardError, 'later census failure after server reference observation'):
                            f.campaign.recover()
                self.assertEqual(len(supplied), 1)
                self.assertNotEqual(supplied[0], original_server)
                self.assertEqual([kind for kind, _ in f.cloud.lists], ['server', 'network'])
                self.assertEqual(f.cloud.describes, [], 'exact readbacks must not supply this census-only proof')
                self.assertEqual(len(failures), 1)
                prior = [event for event in failures[0]['events'] if event.get('event') == 'observed-retirement'
                         and event.get('kind') == 'primary-ip' and event.get('id') == 10101]
                self.assertEqual(len(prior), 1, 'reference retirement must be headed before the later read fails')
                retired = self.assert_retired(f, 'primary-ip', 10101)
                self.assertEqual(retired['associations'], [association])
                self.assertEqual(f.cloud.requests, before)
                self.assertEqual(f.cloud.rows['server'][server], original_server)
                self.assertEqual(f.cloud.rows['primary-ip'][10101], original_ip)
                # Removing the response-only override restores the original
                # reference, not a newly created object or new local provenance.
                self.assert_retired_ip_blocks_restored_server_but_unrelated_cleanup_executes(
                    f, server, original_ip)

    def test_recorded_ip_exact_only_drift_retires_before_generic_census_mismatch(self):
        for drift in ('created', 'assignee', 'auto-delete'):
            with self.subTest(drift=drift), Fixture() as f:
                server = f.create('server')
                self.assertTrue(f.campaign.poll_actions())
                association = self.record_real_ip_association(f, server)
                original_server = copy.deepcopy(f.cloud.rows['server'][server])
                original_ip = copy.deepcopy(f.cloud.rows['primary-ip'][10101])
                f.closing()
                before = copy.deepcopy(f.cloud.requests)
                f.cloud.lists.clear()
                census, exact = [], []
                described, listed = f.cloud.describe, f.cloud.list
                with contextlib.closing(cl.Journal(f.journal.path)) as reader:
                    def inventory(kind, seconds=20):
                        rows = listed(kind, seconds)
                        if kind == 'primary-ip': census.append(copy.deepcopy(rows))
                        return rows
                    def changed_exact(kind, rid, seconds=20):
                        item = described(kind, rid, seconds)
                        if (kind, rid) == ('primary-ip', 10101):
                            headed = reader.peek()
                            self.assertFalse(any(event.get('event') == 'observed-retirement'
                                                 and event.get('kind') == 'primary-ip' and event.get('id') == 10101
                                                 for event in headed['events']),
                                             'unchanged census must not preempt the exact-only branch')
                            item = copy.deepcopy(item)
                            if drift == 'created': item['created'] = utc(T0 + 11 * NS)
                            if drift == 'assignee': item['assignee_id'] = 999
                            if drift == 'auto-delete': item['auto_delete'] = False
                            exact.append(copy.deepcopy(item))
                        return item
                    with patch.object(f.cloud, 'list', side_effect=inventory), \
                         patch.object(f.cloud, 'describe', side_effect=changed_exact):
                        with self.assertRaisesRegex(mc.GuardError, 'recorded primary-IP identity changed'):
                            f.campaign.mutate('server', server, 'delete')
                self.assertEqual(len(census), 1)
                self.assertEqual(next(row for row in census[0] if row['id'] == 10101), original_ip)
                self.assertEqual(len(exact), 1)
                self.assertNotEqual(exact[0], original_ip)
                retired = self.assert_retired(f, 'primary-ip', 10101)
                self.assertEqual(retired['associations'], [association])
                self.assertEqual(f.cloud.requests, before)
                self.assertEqual(f.cloud.rows['server'][server], original_server)
                self.assertEqual(f.cloud.rows['primary-ip'][10101], original_ip)
                self.assert_retired_ip_blocks_restored_server_but_unrelated_cleanup_executes(
                    f, server, original_ip)

    def test_recorded_ip_exact_missing_required_fields_retire_before_original_fields_return(self):
        for missing in ('created', 'type', 'assignee_type', 'assignee_id', 'auto_delete'):
            with self.subTest(missing=missing), Fixture() as f:
                server = f.create('server')
                self.assertTrue(f.campaign.poll_actions())
                association = self.record_real_ip_association(f, server)
                original_server = copy.deepcopy(f.cloud.rows['server'][server])
                original_ip = copy.deepcopy(f.cloud.rows['primary-ip'][10101])
                self.assertIn(missing, original_ip)
                f.closing()
                before = copy.deepcopy(f.cloud.requests)
                f.cloud.lists.clear()
                census, exact = [], []
                described, listed = f.cloud.describe, f.cloud.list
                with contextlib.closing(cl.Journal(f.journal.path)) as reader:
                    def inventory(kind, seconds=20):
                        rows = listed(kind, seconds)
                        if kind == 'primary-ip': census.append(copy.deepcopy(rows))
                        return rows
                    def missing_exact(kind, rid, seconds=20):
                        item = described(kind, rid, seconds)
                        if (kind, rid) == ('primary-ip', 10101):
                            self.assertEqual(item, original_ip)
                            headed = reader.peek()
                            self.assertFalse(any(event.get('event') == 'observed-retirement'
                                                 and event.get('kind') == 'primary-ip' and event.get('id') == 10101
                                                 for event in headed['events']),
                                             'unchanged census must not preempt exact-field-loss retirement')
                            item = copy.deepcopy(item)
                            item.pop(missing)
                            exact.append(copy.deepcopy(item))
                        return item
                    with patch.object(f.cloud, 'list', side_effect=inventory), \
                         patch.object(f.cloud, 'describe', side_effect=missing_exact):
                        with self.assertRaisesRegex(mc.GuardError, 'recorded primary-IP identity changed'):
                            f.campaign.mutate('server', server, 'delete')
                self.assertEqual(len(census), 1)
                self.assertEqual(next(row for row in census[0] if row['id'] == 10101), original_ip)
                self.assertEqual(len(exact), 1)
                self.assertEqual(exact[0], {key: value for key, value in original_ip.items() if key != missing})
                self.assertNotIn(missing, exact[0])
                retired = self.assert_retired(f, 'primary-ip', 10101)
                self.assertEqual(retired['associations'], [association])
                self.assertFalse(any(event.get('event') == 'observed-retirement' and event.get('kind') == 'server'
                                     and event.get('id') == server for event in retired['events']))
                self.assertEqual(f.cloud.requests, before)
                self.assertEqual(f.cloud.rows['server'][server], original_server)
                self.assertEqual(f.cloud.rows['primary-ip'][10101], original_ip)
                # Remove only the exact-response override: all fields return
                # without a new allocation, census change or ownership record.
                self.assertEqual(f.cloud.describe('primary-ip', 10101), original_ip)
                self.assert_retired_ip_blocks_restored_server_but_unrelated_cleanup_executes(
                    f, server, original_ip)

    def test_completion_exact_ip_changed_or_missing_identity_retires_before_later_read_failure(self):
        changes = dict(created=utc(T0 + 11 * NS), type='ipv6', assignee_type='network',
                       assignee_id=999, auto_delete=False)
        for observation in ('changed', 'missing'):
            for field, changed in changes.items():
                with self.subTest(observation=observation, field=field), Fixture() as f:
                    server = f.create('server')
                    self.assertTrue(f.campaign.poll_actions())
                    association = self.record_real_ip_association(f, server)
                    original_server = copy.deepcopy(f.cloud.rows['server'][server])
                    original_ip = copy.deepcopy(f.cloud.rows['primary-ip'][10101])
                    self.assertIn(field, original_ip)
                    self.assertNotEqual(original_ip[field], changed)
                    f.closing()
                    before = copy.deepcopy(f.cloud.requests)
                    census, exact, reads, failures = [], [], [], []
                    described, listed = f.cloud.describe, f.cloud.list
                    with contextlib.closing(cl.Journal(f.journal.path)) as reader:
                        def inventory(kind, seconds=20):
                            rows = listed(kind, seconds)
                            if kind == 'primary-ip': census.append(copy.deepcopy(rows))
                            return rows
                        def completion_exact(kind, rid, seconds=20):
                            reads.append((kind, rid))
                            if exact and (kind, rid) == ('server', server):
                                headed = reader.peek()
                                retired = [event for event in headed['events']
                                    if event.get('event') == 'observed-retirement'
                                    and event.get('kind') == 'primary-ip' and event.get('id') == 10101]
                                self.assertEqual(len(retired), 1,
                                    'completion must publish exact-IP retirement before the next read fails')
                                self.assertEqual(headed['associations'], [association])
                                failures.append(copy.deepcopy(headed))
                                raise mc.GuardError('synthetic later completion exact read unavailable')
                            item = described(kind, rid, seconds)
                            if (kind, rid) == ('primary-ip', 10101):
                                self.assertEqual(item, original_ip)
                                headed = reader.peek()
                                self.assertFalse(any(event.get('event') == 'observed-retirement'
                                    and event.get('kind') == 'primary-ip' and event.get('id') == 10101
                                    for event in headed['events']),
                                    'unchanged completion census must not retire before the exact-only response')
                                item = copy.deepcopy(item)
                                if observation == 'changed': item[field] = changed
                                else: item.pop(field)
                                exact.append(copy.deepcopy(item))
                            return item
                        with patch.object(f.cloud, 'list', side_effect=inventory), \
                             patch.object(f.cloud, 'describe', side_effect=completion_exact):
                            result = f.campaign.completion()
                    self.assertFalse(result['complete'])
                    self.assertEqual(len(census), 1)
                    self.assertEqual(next(row for row in census[0] if row['id'] == 10101), original_ip)
                    expected = copy.deepcopy(original_ip)
                    if observation == 'changed': expected[field] = changed
                    else: expected.pop(field)
                    self.assertEqual(exact, [expected])
                    self.assertEqual(reads[reads.index(('primary-ip', 10101)) + 1], ('server', server))
                    self.assertEqual(len(failures), 1)
                    self.assertEqual(result['unresolved_readbacks'],
                                     [dict(kind='server', id=server, error_type='GuardError')])
                    retired = self.assert_retired(f, 'primary-ip', 10101)
                    self.assertEqual(retired['associations'], [association])
                    self.assertFalse(any(event.get('event') == 'observed-retirement' and event.get('kind') == 'server'
                                         and event.get('id') == server for event in retired['events']))
                    self.assertEqual(f.cloud.requests, before)
                    self.assertEqual(f.cloud.rows['server'][server], original_server)
                    self.assertEqual(f.cloud.rows['primary-ip'][10101], original_ip)
                    # Returning every original exact field cannot restore the
                    # recorded association's authority after completion saw drift.
                    self.assertEqual(f.cloud.describe('primary-ip', 10101), original_ip)
                    self.assert_retired_ip_blocks_restored_server_but_unrelated_cleanup_executes(
                        f, server, original_ip)

    def test_partial_server_ack_with_only_sibling_stays_unknown_across_two_recovery_passes(self):
        for primary in ('missing', 'null'):
            with self.subTest(primary=primary), Fixture() as f:
                f.dependencies()
                self.assertTrue(f.campaign.poll_actions())
                sequence = len(f.journal.read()['intents'])
                siblings, omitted_primary = [], []
                def partial(reply):
                    payload = reply['payload']
                    server = payload['server']['id']
                    original = payload['action']
                    omitted_primary.append(f.cloud.actions.pop(original['id']))
                    sibling = f.cloud.new_action('server', server, 'apply_firewall')
                    sibling['status'] = 'running'
                    f.cloud.actions[sibling['id']]['status'] = 'running'
                    siblings.append(sibling['id'])
                    payload['next_actions'] = [sibling]
                    if primary == 'missing': payload.pop('action')
                    else: payload['action'] = None
                    return reply
                wire = WireMutations(f, partial)
                with patch.object(f.cloud, 'request', side_effect=wire.request):
                    with self.assertRaisesRegex(mc.GuardError, 'failed or ambiguous'):
                        f.create('server')
                self.assertEqual(len(wire.wire.requests), 1)
                self.assertEqual(wire.wire.requests[0]['path'], '/v1/servers')
                self.assertEqual(len(siblings), 1)
                sibling = siblings[0]
                intent = self.assert_unknown_required_action(f, sequence, sibling)
                self.assertIsNone(intent['resource_id'])
                self.assertNotIn(omitted_primary[0]['id'], f.cloud.actions)
                f.closing()
                lookups = []
                resource_actions = f.cloud.resource_actions
                def listed(kind, rid, seconds=20, on_page=None):
                    rows = resource_actions(kind, rid, seconds, on_page=on_page)
                    lookups.append(copy.deepcopy(rows))
                    return rows
                with patch.object(f.cloud, 'resource_actions', side_effect=listed):
                    for _ in range(2):
                        f.campaign.recover()
                        current = self.assert_unknown_required_action(f, sequence, sibling)
                        self.assertEqual(current['resource_id'], 101)
                        self.assertTrue(lookups, 'conditional server recovery must inspect actual creation evidence')
                        self.assertEqual([row['id'] for row in lookups[-1]], [sibling])
                        self.assertEqual(lookups[-1][0]['command'], 'apply_firewall')
                        result = f.campaign.completion()
                        self.assertFalse(result['complete'])
                        self.assertEqual(result['pending_intents'], [sequence])
                f.cloud.actions[sibling]['status'] = 'success'
                self.assertFalse(f.campaign.poll_actions())
                self.assert_unknown_required_action(f, sequence, sibling)
                result = f.campaign.cleanup_pass()
                self.assertTrue(result['baseline_equal'])
                self.assertEqual(result['owned_survivors'], [])
                self.assertFalse(result['complete'])
                self.assertEqual(result['pending_intents'], [sequence])
                self.assertIn(('DELETE', '/servers/101'), f.deletes())
                self.assert_unknown_required_action(f, sequence, sibling)

    def noncreate_target(self, fixture, operation):
        if operation == 'subnet':
            rid = fixture.create()
            target = ('network', rid, operation, copy.deepcopy(fixture.header['subnet']))
        elif operation == 'attach':
            network = fixture.create()
            rid = fixture.create('server')
            target = ('server', rid, operation, dict(network=network, ip=fixture.header['plan'][3]['private_ip']))
        else:
            rid = fixture.create('server')
            target = ('server', rid, operation, {} if operation == 'poweron' else None)
        self.assertTrue(fixture.campaign.poll_actions())
        if operation == 'delete': fixture.closing()
        return target

    def test_partial_noncreate_ack_sibling_cannot_supply_its_missing_primary_operation(self):
        for operation in ('subnet', 'attach', 'poweron', 'delete'):
            with self.subTest(operation=operation), Fixture() as f:
                target = self.noncreate_target(f, operation)
                kind, rid = target[:2]
                sequence = len(f.journal.read()['intents'])
                siblings = []
                def partial(reply):
                    payload = reply['payload']
                    omitted = payload['action']['id']
                    f.cloud.actions.pop(omitted)
                    sibling = f.cloud.new_action(kind, rid, 'unidentified_operation_followup')
                    sibling['status'] = 'running'
                    f.cloud.actions[sibling['id']]['status'] = 'running'
                    siblings.append(sibling['id'])
                    payload['action'] = None
                    payload['next_actions'] = [sibling]
                    return reply
                wire = WireMutations(f, partial)
                with patch.object(f.cloud, 'request', side_effect=wire.request):
                    with self.assertRaisesRegex(mc.GuardError, 'failed or ambiguous'):
                        f.campaign.mutate(*target)
                self.assertEqual(len(wire.wire.requests), 1)
                sibling = siblings[0]
                self.assert_unknown_required_action(f, sequence, sibling)
                if operation != 'delete': f.closing()
                for status in ('running', 'success'):
                    f.cloud.actions[sibling]['status'] = status
                    f.campaign.recover()
                    self.assert_unknown_required_action(f, sequence, sibling)
                    self.assertFalse(f.campaign.poll_actions())
                    result = f.campaign.completion()
                    self.assertFalse(result['complete'])
                    self.assertEqual(result['pending_intents'], [sequence])
                result = f.campaign.cleanup_pass()
                self.assertTrue(result['baseline_equal'])
                self.assertEqual(result['owned_survivors'], [])
                self.assertFalse(result['complete'])
                self.assertEqual(result['pending_intents'], [sequence])
                self.assert_unknown_required_action(f, sequence, sibling)

    def test_guard_cleanup_cannot_erase_latched_h_breach_after_forward_wall_step_is_corrected(self):
        with Fixture() as f:
            network = f.create()
            self.assertTrue(f.campaign.poll_actions())
            policy = copy.deepcopy(f.header['time'])
            hard = policy['h_ns']
            f.closing()
            f.processes.pop(112)
            f.processes[113] = dict(pid=113, parent=110, starttime='1130', argv_sha256=cc.digest(b'113'))
            f.actor = 113
            f.wall = hard + NS
            failures, sleeps = [], []
            def unavailable(kind, seconds):
                if kind == 'server' and not failures:
                    observed = f.journal.peek()
                    failures.append(observed)
                    self.assertEqual(f.wall, hard + NS)
                    self.assertIs(observed.get('hard_lifetime_breached'), True)
                    raise mc.GuardError('synthetic cleanup census unavailable after observed H breach')
            def rollback(seconds):
                sleeps.append(seconds)
                self.assertEqual(sleeps, [1], 'second real cleanup pass must complete without another wait')
                self.assertEqual(len(failures), 1)
                latched = self.fresh_state(f)
                self.assertIs(latched.get('hard_lifetime_breached'), True)
                self.assertEqual(latched['mode'], 'INCIDENT')
                self.assertIn(network, f.cloud.rows['network'])
                f.wall = hard - 120 * NS
                f.mono += NS
                self.assertLess(f.campaign.clock(), hard,
                                'this control requires a corrected effective clock below H')
                f.cloud.before_list = None
            f.cloud.before_list = unavailable
            result = f.campaign.guard_loop(sleep=rollback, poll_seconds=1, cleanup_interval_seconds=1)
            self.assertEqual(len(failures), 1)
            self.assertEqual(sleeps, [1])
            self.assertTrue(result['complete'])
            self.assertTrue(result['baseline_equal'])
            self.assertEqual(result['pending_intents'], [])
            self.assertFalse(result['hard_lifetime_met'])
            self.assertLess(f.campaign.clock(), hard)
            self.assertEqual(f.deletes(), [('DELETE', '/networks/201')])
            final = self.fresh_state(f)
            self.assertEqual(final['mode'], 'COMPLETE')
            self.assertIs(final.get('hard_lifetime_breached'), True)
            self.assertEqual(f.journal.header['time'], policy)
            self.assertEqual(json.loads((f.guard_directory / 'cleanup.json').read_bytes()), result)
            incidents = list(f.guard_directory.glob('incident-*.json'))
            self.assertEqual(len(incidents), 1)
            self.assertFalse(json.loads(incidents[0].read_bytes())['hard_lifetime_met'])


if __name__ == '__main__':
    unittest.main()
