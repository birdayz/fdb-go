"""Literal HTTP acknowledgements through real Cloud and durable Campaign paths."""
import contextlib
import copy
import io
import json
import os
import unittest
from unittest.mock import patch

import campaign_cloud as cc
import creation_ledger as cl
import mn_common as mc
from test_campaign_cloud import DATE, MemoryProvider
from test_campaign_safety import Fixture


class WireMutations:
    """Provider-side application occurs only after stdlib HTTP sent its request."""
    def __init__(self, fixture, transform=lambda reply: reply, raw=None, headers=None, omit_framing=False):
        self.fixture, self.transform, self.raw, self.headers = fixture, transform, raw, headers or {}
        self.omit_framing, self.unframed_responses = omit_framing, []
        self.backend = fixture.cloud.request
        self.wire = MemoryProvider()
        self.replies = []
        self.client = cc.Cloud(fixture.cloud.profile, fixture.header['context'],
                               connection_factory=self.connection, deadline=contextlib.nullcontext)

    def connection(self, host, seconds):
        self.wire.replies.append({'status': 200, 'raw': b'', 'headers': self.headers})
        connection = self.wire.connection(host, seconds)
        socket = self.wire.sockets[-1]
        makefile = socket.makefile
        def response(mode):
            request = self.wire.requests[-1]
            body = json.loads(request['body']) if request['body'] else None
            reply = self.transform(self.backend(request['method'], request['path'].removeprefix('/v1'), body, seconds))
            socket.reply['status'] = reply['status']
            socket.reply['raw'] = (self.raw if self.raw is not None else
                cc.wire_json(reply['payload']) if reply['payload'] is not None else b'')
            if self.omit_framing:
                # MemorySocket otherwise adds Content-Length unconditionally.
                # Serialize this response here to exercise genuine header absence.
                headers = {'Date': DATE, **self.headers}
                head = f'HTTP/1.1 {reply["status"]} Fake\r\n' + ''.join(
                    f'{key}: {value}\r\n' for key, value in headers.items())
                raw = head.encode('ascii') + b'\r\n' + socket.reply['raw']
                self.unframed_responses.append(raw)
                return io.BytesIO(raw)
            return makefile(mode)
        socket.makefile = response
        return connection

    def request(self, *args, **kwargs):
        reply = self.client.request(*args, **kwargs)
        self.replies.append(copy.deepcopy(reply))
        return reply


class AcknowledgementTests(unittest.TestCase):
    def setUp(self):
        self.environment = patch.dict(os.environ, {}, clear=True)
        self.environment.start()
        self.addCleanup(self.environment.stop)
        self.network = patch.object(cc.http.client.HTTPSConnection, 'connect',
                                    side_effect=AssertionError('real network forbidden'))
        self.network.start()
        self.addCleanup(self.network.stop)

    def target(self, f, operation, kind):
        if operation == 'create':
            if kind == 'server':
                f.dependencies()
                self.assertTrue(f.campaign.poll_actions())
            return lambda: f.campaign.create(kind, f.body(kind))
        if operation == 'subnet':
            rid = f.create()
            body = copy.deepcopy(f.header['subnet'])
        elif operation == 'attach':
            network = f.create()
            rid = f.create('server')
            body = dict(network=network, ip=f.header['plan'][3]['private_ip'])
        else:
            rid = f.create(kind)
            body = {} if operation == 'poweron' else None
        self.assertTrue(f.campaign.poll_actions())
        if operation == 'delete':
            f.closing()
        return lambda: f.campaign.mutate(kind, rid, operation, body)

    def assert_one_wire(self, wire):
        self.assertEqual(len(wire.wire.requests), 1)
        self.assertEqual(len(wire.wire.connections), 1)
        self.assertTrue(wire.wire.sockets[0].closed)
        self.assertEqual(len(wire.replies), 1)

    def test_all_supported_endpoints_accept_real_wire_flags_and_actionless_firewall(self):
        endpoints = [('create', kind) for kind in ('network', 'firewall', 'ssh-key', 'server')]
        endpoints += [('subnet', 'network'), ('attach', 'server'), ('poweron', 'server')]
        endpoints += [('delete', kind) for kind in ('server', 'network', 'firewall', 'ssh-key')]
        for operation, kind in endpoints:
            with self.subTest(operation=operation, kind=kind), Fixture() as f:
                call = self.target(f, operation, kind)
                def actionless(reply):
                    if operation == 'create' and kind == 'firewall':
                        reply['payload'].pop('actions', None)
                    return reply
                wire = WireMutations(f, actionless)
                with patch.object(f.cloud, 'request', side_effect=wire.request):
                    call()
                self.assert_one_wire(wire)
                reply = wire.replies[0]
                self.assertIs(reply['json_valid'], True)
                self.assertIs(reply['body_empty'], operation == 'delete' and kind != 'server')
                intent = f.journal.read()['intents'][-1]
                self.assertEqual(intent['state'], 'ACKNOWLEDGED')
                self.assertEqual(intent['ack'], reply)
                if operation == 'create' and kind == 'firewall':
                    self.assertEqual(intent['actions'], [])
                self.assertTrue(f.campaign.poll_actions())
                self.assertEqual(f.journal.read()['intents'][-1]['state'], 'DONE')

    @contextlib.contextmanager
    def initial_identity_fixture(self, case):
        with Fixture() as f:
            # A nonbaseline project resource independently pins pre-call exclusion.
            f.cloud.rows['firewall'][777] = dict(id=777, name='unrelated-firewall', rules=[],
                labels={'campaign': 'unrelated'}, created='2020-01-01T00:00:00Z')
            assigned = 9002 if case == 'baseline' else 777 if case == 'pre-call' else 1
            f.cloud.next_ids['firewall'] = assigned
            if case == 'baseline':
                # Keep sealed baseline protection, but remove the overlapping
                # pre-call operand so either exclusion gate is independently needed.
                f.cloud.rows['firewall'].pop(9002)
            yield f, assigned

    def initial_identity_state(self, f):
        with contextlib.closing(cl.Journal(f.journal.path)) as journal:
            return journal.read()

    def assert_initial_identity_ack_evidence(self, f, wire, assigned, case):
        self.assert_one_wire(wire)
        self.assertEqual(len(f.cloud.requests), 1)
        self.assertEqual((wire.wire.requests[0]['method'], wire.wire.requests[0]['path']), ('POST', '/v1/firewalls'))
        self.assertEqual((f.cloud.requests[0][0], f.cloud.requests[0][1]), ('POST', '/firewalls'))
        self.assertEqual([kind for kind, _ in f.cloud.lists], list(mc.PROTECT_KINDS))
        state = self.initial_identity_state(f)
        self.assertEqual(len(state['intents']), 1)
        intent = state['intents'][0]
        reply = wire.replies[0]
        self.assertEqual(reply['status'], 201)
        self.assertIs(reply['json_valid'], True)
        self.assertIs(reply['body_empty'], False)
        self.assertEqual(reply['body_sha256'], cc.digest(cc.wire_json(reply['payload'])))
        self.assertEqual(intent['ack'], reply)
        self.assertIs(intent['acknowledgement_validated'], True)
        self.assertIs(intent['required_action_identified'], True)
        self.assertEqual(intent['actions'], [1001])
        action = reply['payload']['actions'][0]
        self.assertEqual(len(reply['payload']['actions']), 1)
        self.assertEqual((action['id'], action['command'], action['status']), (1001, 'create_firewall', 'success'))
        self.assertEqual(action['resources'], [{'type': 'firewall', 'id': assigned}])
        self.assertIs(type(action['resources'][0]['id']), int)
        item = reply['payload']['firewall']
        self.assertEqual(item['rules'], [])
        self.assertEqual(item['created'], '2023-11-14T22:13:30Z')
        if not case.startswith('name-'):
            self.assertEqual(item.get('name'), intent['name'])
        if not case.startswith(('labels-', 'nonce-')):
            self.assertEqual(item.get('labels'), intent['descriptor']['labels'])
        self.assertEqual(intent['name'], 'test-firewall')
        self.assertRegex(intent['nonce'], '^[0-9a-f]{32}$')
        self.assertEqual(intent['descriptor']['labels']['intent'], intent['nonce'])
        sent = json.loads(wire.wire.requests[0]['body'])
        self.assertEqual(sent, f.cloud.requests[0][2])
        self.assertEqual(sent, intent['descriptor'])
        self.assertEqual(sent['name'], 'test-firewall')
        self.assertEqual(sent['rules'], [])
        self.assertEqual(sent['labels']['intent'], intent['nonce'])
        self.assertEqual(intent['request_sha256'], cc.digest(wire.wire.requests[0]['body']))
        self.assertEqual(f.header['baseline']['firewall'], [9002])
        self.assertEqual(intent['pre_ids']['firewall'], [777] if case == 'baseline' else [777, 9002])
        self.assertEqual(intent['send_lease']['actor'], f.header['producer'])
        self.assertGreater(intent['send_lease']['deadline_ns'], f.campaign.clock())
        self.assertGreater(intent['send_lease']['monotonic_deadline_ns'], f.mono)
        self.assertIsNone(state['active_lease'])
        return state, intent, item

    def test_initial_ack_identity_and_existing_id_exclusions_have_independent_acceptance_refusals(self):
        invalid_ids = {'id-true': True, 'id-false': False, 'id-string': '1', 'id-float': 1.0,
                       'id-null': None, 'id-zero': 0, 'id-negative': -1}
        cases = (*invalid_ids, 'id-missing', 'baseline', 'pre-call', 'name-changed', 'name-missing',
                 'labels-changed', 'labels-missing', 'nonce-changed', 'nonce-missing')
        for case in cases:
            with self.subTest(case=case), self.initial_identity_fixture(case) as (f, assigned):
                originals = []
                def adverse(reply):
                    item = reply['payload']['firewall']
                    original = copy.deepcopy(item)
                    originals.append(original)
                    if case in invalid_ids: item['id'] = invalid_ids[case]
                    elif case == 'id-missing': item.pop('id')
                    elif case == 'name-changed': item['name'] = 'test-another-firewall'
                    elif case == 'name-missing': item.pop('name')
                    elif case == 'labels-changed': item['labels']['campaign'] = 'different-campaign'
                    elif case == 'labels-missing': item.pop('labels')
                    elif case == 'nonce-changed':
                        issued = item['labels']['intent']
                        item['labels']['intent'] = ('0' if issued[0] != '0' else '1') + issued[1:]
                    elif case == 'nonce-missing': item['labels'].pop('intent')
                    field = 'id' if case.startswith('id-') or case in {'baseline', 'pre-call'} else (
                        'name' if case.startswith('name-') else 'labels')
                    self.assertEqual({k: v for k, v in item.items() if k != field},
                                     {k: v for k, v in original.items() if k != field})
                    return reply
                wire = WireMutations(f, adverse)
                id_gate = case.startswith('id-') or case in {'baseline', 'pre-call'}
                expected = 'creation response conflicts with existing typed ID' if id_gate else 'creation canonical binding differs'
                with patch.object(f.cloud, 'request', side_effect=wire.request):
                    with self.assertRaisesRegex(mc.GuardError, '^create failed or ambiguous; exact-ledger cleanup required$') as caught:
                        f.create('firewall')
                    underlying = caught.exception.__context__
                    self.assertIs(type(underlying), mc.GuardError)
                    self.assertEqual(str(underlying), expected)
                    saved = self.initial_identity_state(f)
                    checkpoint = copy.deepcopy(f.journal.checkpoint)
                    with self.assertRaisesRegex(mc.GuardError, '^cleanup requested$'):
                        f.create('firewall')
                    self.assertEqual(self.initial_identity_state(f), saved)
                    self.assertEqual(f.journal.checkpoint, checkpoint)
                state, intent, item = self.assert_initial_identity_ack_evidence(f, wire, assigned, case)
                self.assertEqual(len(originals), 1)
                self.assertEqual(state['mode'], 'OPEN')
                self.assertEqual(state['owned'], [])
                self.assertEqual(state['associations'], [])
                self.assertEqual(intent['state'], 'UNKNOWN')
                self.assertIsNone(intent['resource_id'])
                if case == 'baseline':
                    self.assertIn(item['id'], f.header['baseline']['firewall'])
                    self.assertNotIn(item['id'], intent['pre_ids']['firewall'])
                elif case == 'pre-call':
                    self.assertNotIn(item['id'], f.header['baseline']['firewall'])
                    self.assertIn(item['id'], intent['pre_ids']['firewall'])
                elif not id_gate:
                    self.assertEqual(item['id'], 1)
                    self.assertNotIn(item['id'], f.header['baseline']['firewall'])
                    self.assertNotIn(item['id'], intent['pre_ids']['firewall'])
                if case in invalid_ids:
                    self.assertIs(type(item['id']), type(invalid_ids[case]))
                    self.assertEqual(item['id'], invalid_ids[case])
                elif case == 'id-missing': self.assertNotIn('id', item)
                if case in {'labels-changed', 'labels-missing', 'nonce-changed', 'nonce-missing'}:
                    self.assertEqual(item['name'], 'test-firewall')
                if case == 'labels-changed':
                    self.assertEqual(item['labels']['intent'], intent['nonce'])
                    self.assertEqual(item['labels'], dict(intent['descriptor']['labels'], campaign='different-campaign'))
                if case in {'nonce-changed', 'nonce-missing'}:
                    self.assertEqual({k: v for k, v in item['labels'].items() if k != 'intent'},
                                     {k: v for k, v in intent['descriptor']['labels'].items() if k != 'intent'})
                    if case == 'nonce-changed':
                        self.assertRegex(item['labels']['intent'], '^[0-9a-f]{32}$')
                        self.assertNotEqual(item['labels']['intent'], intent['nonce'])
                    else: self.assertNotIn('intent', item['labels'])
                failures = [e for e in state['events'] if e.get('event') == 'unknown-create']
                self.assertEqual([(e['intent'], e['error_type']) for e in failures], [(0, 'GuardError')])
                self.assertTrue((f.guard_directory / 'stop.json').exists())
                self.assertEqual(f.deletes(), [])

    def test_initial_ack_identity_healthy_integer_one_preserves_all_bindings_and_action_evidence(self):
        with self.initial_identity_fixture('healthy') as (f, assigned):
            wire = WireMutations(f)
            with patch.object(f.cloud, 'request', side_effect=wire.request):
                self.assertEqual(f.create('firewall'), 1)
            state, intent, item = self.assert_initial_identity_ack_evidence(f, wire, assigned, 'healthy')
            self.assertIs(type(item['id']), int)
            self.assertEqual(item['id'], 1)
            self.assertNotIn(item['id'], f.header['baseline']['firewall'])
            self.assertNotIn(item['id'], intent['pre_ids']['firewall'])
            self.assertEqual(item['name'], intent['name'])
            self.assertEqual(item['labels'], intent['descriptor']['labels'])
            self.assertEqual(intent['state'], 'ACKNOWLEDGED')
            self.assertEqual(intent['resource_id'], 1)
            self.assertEqual(len(state['owned']), 1)
            self.assertEqual(state['owned'][0]['id'], 1)
            self.assertEqual(state['owned'][0]['canonical'], {'rules': []})
            self.assertTrue(f.campaign.poll_actions())
            terminal = self.initial_identity_state(f)['intents'][0]
            self.assertEqual(terminal['state'], 'DONE')
            self.assertEqual(terminal['ack'], wire.replies[0])
            self.assertEqual(terminal['actions'], [1001])
            self.assertFalse((f.guard_directory / 'stop.json').exists())
            self.assertEqual(f.deletes(), [])
            self.assert_one_wire(wire)

    def assert_initial_network_ack_evidence(self, f, wire):
        self.assert_one_wire(wire)
        self.assertEqual(wire.wire.posts, 1)
        self.assertEqual(len(f.cloud.requests), 1)
        self.assertEqual((wire.wire.requests[0]['method'], wire.wire.requests[0]['path']), ('POST', '/v1/networks'))
        self.assertEqual((f.cloud.requests[0][0], f.cloud.requests[0][1]), ('POST', '/networks'))
        self.assertEqual([kind for kind, _ in f.cloud.lists], list(mc.PROTECT_KINDS))
        state = self.initial_identity_state(f)
        self.assertEqual(len(state['intents']), 1)
        intent = state['intents'][0]
        reply = wire.replies[0]
        self.assertEqual(reply['status'], 201)
        self.assertIs(reply['json_valid'], True)
        self.assertIs(reply['body_empty'], False)
        self.assertEqual(set(reply['payload']), {'network'})
        self.assertEqual(reply['body_sha256'], cc.digest(cc.wire_json(reply['payload'])))
        self.assertEqual(intent['ack'], reply)
        self.assertIs(intent['acknowledgement_validated'], True)
        self.assertIs(intent['required_action_identified'], True)
        self.assertEqual(intent['actions'], [])  # Pinned network create has no asynchronous action.
        item = reply['payload']['network']
        self.assertEqual(item['id'], 201)
        self.assertIs(type(item['id']), int)
        self.assertEqual(item['name'], 'test-network')
        self.assertEqual(item['name'], intent['name'])
        self.assertEqual(item['labels'], intent['descriptor']['labels'])
        self.assertEqual(item['labels']['intent'], intent['nonce'])
        self.assertRegex(intent['nonce'], '^[0-9a-f]{32}$')
        self.assertEqual(item['created'], '2023-11-14T22:13:30Z')
        self.assertEqual(f.header['baseline']['network'], [9001])
        self.assertEqual(intent['pre_ids']['network'], [9001])
        self.assertNotIn(201, f.header['baseline']['network'])
        self.assertNotIn(201, intent['pre_ids']['network'])
        sent = json.loads(wire.wire.requests[0]['body'])
        self.assertEqual(sent, {'name': 'test-network', 'ip_range': '10.42.0.0/24',
                                'labels': intent['descriptor']['labels']})
        self.assertEqual(sent, f.cloud.requests[0][2])
        self.assertEqual(sent, intent['descriptor'])
        self.assertEqual(intent['request_sha256'], cc.digest(wire.wire.requests[0]['body']))
        self.assertEqual(intent['send_lease']['actor'], f.header['producer'])
        self.assertGreater(intent['send_lease']['deadline_ns'], f.campaign.clock())
        self.assertGreater(intent['send_lease']['monotonic_deadline_ns'], f.mono)
        self.assertIsNone(state['active_lease'])
        return state, intent, item

    def test_initial_network_ack_changed_or_missing_cidr_fails_its_exact_acceptance_gate(self):
        for drift in ('changed', 'missing'):
            with self.subTest(ip_range=drift), Fixture() as f:
                originals = []
                def adverse(reply):
                    item = reply['payload']['network']
                    original = copy.deepcopy(item)
                    originals.append(original)
                    self.assertEqual(original['ip_range'], '10.42.0.0/24')
                    if drift == 'changed': item['ip_range'] = '10.99.0.0/24'
                    else: item.pop('ip_range')
                    self.assertEqual({key: value for key, value in item.items() if key != 'ip_range'},
                                     {key: value for key, value in original.items() if key != 'ip_range'})
                    return reply
                wire = WireMutations(f, adverse)
                with patch.object(f.cloud, 'request', side_effect=wire.request):
                    with self.assertRaisesRegex(mc.GuardError, '^create failed or ambiguous; exact-ledger cleanup required$') as caught:
                        f.create('network')
                    underlying = caught.exception.__context__
                    if drift == 'changed':
                        self.assertIs(type(underlying), mc.GuardError)
                        self.assertEqual(str(underlying), 'creation canonical binding differs')
                    else:
                        self.assertIs(type(underlying), KeyError)
                        self.assertEqual(underlying.args, ('ip_range',))
                    saved = self.initial_identity_state(f)
                    checkpoint = copy.deepcopy(f.journal.checkpoint)
                    with self.assertRaisesRegex(mc.GuardError, '^cleanup requested$'):
                        f.create('network')
                    self.assertEqual(self.initial_identity_state(f), saved)
                    self.assertEqual(f.journal.checkpoint, checkpoint)
                state, intent, item = self.assert_initial_network_ack_evidence(f, wire)
                self.assertEqual(len(originals), 1)
                self.assertEqual(f.cloud.rows['network'][201], originals[0], 'only the response may be altered')
                self.assertEqual(f.cloud.rows['network'][201]['ip_range'], '10.42.0.0/24')
                if drift == 'changed': self.assertEqual(item['ip_range'], '10.99.0.0/24')
                else: self.assertNotIn('ip_range', item)
                self.assertEqual(state['mode'], 'OPEN')
                self.assertEqual(state['owned'], [])
                self.assertEqual(state['associations'], [])
                self.assertEqual(intent['state'], 'UNKNOWN')
                self.assertIsNone(intent['resource_id'])
                failures = [event for event in state['events'] if event.get('event') == 'unknown-create']
                self.assertEqual([(event['intent'], event['error_type']) for event in failures],
                                 [(0, 'GuardError' if drift == 'changed' else 'KeyError')])
                self.assertTrue((f.guard_directory / 'stop.json').exists())
                self.assertEqual(f.deletes(), [])

    def test_initial_network_ack_matching_literal_cidr_becomes_owned_and_done(self):
        with Fixture() as f:
            wire = WireMutations(f)
            with patch.object(f.cloud, 'request', side_effect=wire.request):
                self.assertEqual(f.create('network'), 201)
            state, intent, item = self.assert_initial_network_ack_evidence(f, wire)
            self.assertEqual(item['ip_range'], '10.42.0.0/24')
            self.assertEqual(intent['state'], 'ACKNOWLEDGED')
            self.assertEqual(intent['resource_id'], 201)
            self.assertIs(type(intent['resource_id']), int)
            self.assertEqual(len(state['owned']), 1)
            self.assertEqual((state['owned'][0]['kind'], state['owned'][0]['id']), ('network', 201))
            self.assertIs(type(state['owned'][0]['id']), int)
            self.assertEqual(state['owned'][0]['canonical'], {'ip_range': '10.42.0.0/24'})
            self.assertTrue(f.campaign.poll_actions())
            terminal_state = self.initial_identity_state(f)
            terminal = terminal_state['intents'][0]
            self.assertEqual(terminal['state'], 'DONE')
            self.assertEqual(terminal['ack'], wire.replies[0])
            self.assertEqual(terminal['actions'], [])
            self.assertEqual(terminal_state['owned'], state['owned'])
            self.assertFalse((f.guard_directory / 'stop.json').exists())
            self.assertEqual(f.deletes(), [])
            self.assert_one_wire(wire)

    def test_empty_null_and_malformed_real_create_responses_remain_unknown(self):
        for raw, valid, empty in ((b'', True, True), (b'null', True, False),
                                  (b'not JSON', False, False), (b'{"network":', False, False)):
            with self.subTest(raw=raw), Fixture() as f:
                wire = WireMutations(f, raw=raw)
                with patch.object(f.cloud, 'request', side_effect=wire.request):
                    with self.assertRaisesRegex(mc.GuardError, 'failed or ambiguous'):
                        f.create()
                self.assert_one_wire(wire)
                self.assertIs(wire.replies[0]['json_valid'], valid)
                self.assertIs(wire.replies[0]['body_empty'], empty)
                state = f.journal.read()
                self.assertEqual(state['intents'][-1]['state'], 'UNKNOWN')
                self.assertIsNone(state['active_lease'])
                self.assertTrue((f.guard_directory / 'stop.json').exists())
                f.closing()
                self.assertEqual(f.campaign.completion()['pending_intents'], [0])

    def test_server_create_rejects_missing_or_malformed_next_actions_from_real_wire(self):
        for invalid in ('missing', None, {}, False, 'actions', [{}], 'duplicate'):
            with self.subTest(next_actions=invalid), Fixture() as f:
                call = self.target(f, 'create', 'server')
                def malformed(reply):
                    payload = reply['payload']
                    if invalid == 'missing':
                        payload.pop('next_actions')
                    else:
                        payload['next_actions'] = ([copy.deepcopy(payload['action'])]
                            if invalid == 'duplicate' else invalid)
                    return reply
                wire = WireMutations(f, malformed)
                with patch.object(f.cloud, 'request', side_effect=wire.request):
                    with self.assertRaisesRegex(mc.GuardError, 'failed or ambiguous'):
                        call()
                self.assert_one_wire(wire)
                intent = f.journal.read()['intents'][-1]
                self.assertEqual(intent['state'], 'UNKNOWN')
                self.assertEqual(intent['actions'], [wire.replies[0]['payload']['action']['id']])
                self.assertTrue((f.guard_directory / 'stop.json').exists())

    def test_synchronous_deletes_reject_wrong_status_nonempty_null_and_malformed_wire(self):
        for kind in ('network', 'firewall', 'ssh-key'):
            for status, raw in ((200, b''), (201, b''), (204, b'{}'), (204, b'null'), (204, b'bad JSON')):
                with self.subTest(kind=kind, status=status, raw=raw), Fixture() as f:
                    call = self.target(f, 'delete', kind)
                    wire = WireMutations(f, lambda reply: dict(reply, status=status), raw=raw)
                    with patch.object(f.cloud, 'request', side_effect=wire.request):
                        with self.assertRaisesRegex(mc.GuardError, 'failed or ambiguous'):
                            call()
                    self.assert_one_wire(wire)
                    if status == 204 and raw:
                        # stdlib suppresses 204 content; its nonzero framing,
                        # not unframed trailing bytes, disproves a bodyless ACK.
                        self.assertIs(wire.replies[0]['body_empty'], False)
                    state = f.journal.read()
                    self.assertEqual(state['mode'], 'INCIDENT')
                    self.assertEqual(state['intents'][-1]['state'], 'UNKNOWN')
                    self.assertIsNone(state['active_lease'])
                    result = f.campaign.completion()
                    self.assertFalse(result['complete'])
                    self.assertEqual(result['pending_intents'], [state['intents'][-1]['sequence']])

    def test_204_contradictory_framing_is_not_a_bodyless_acknowledgement(self):
        for headers in ({'Content-Length': '5'}, {'Content-Length': '-1'}, {'Content-Length': 'invalid'},
                        {'Content-Length': '0', 'Transfer-Encoding': 'identity'}):
            with self.subTest(headers=headers), Fixture() as f:
                call = self.target(f, 'delete', 'network')
                wire = WireMutations(f, raw=b'', headers=headers)
                with patch.object(f.cloud, 'request', side_effect=wire.request):
                    with self.assertRaisesRegex(mc.GuardError, 'failed or ambiguous'):
                        call()
                self.assert_one_wire(wire)
                self.assertIs(wire.replies[0]['body_empty'], False)
                state = f.journal.read()
                self.assertEqual(state['mode'], 'INCIDENT')
                self.assertEqual(state['intents'][-1]['state'], 'UNKNOWN')
                self.assertEqual(f.campaign.completion()['pending_intents'], [state['intents'][-1]['sequence']])
        with Fixture() as f:
            call = self.target(f, 'delete', 'network')
            wire = WireMutations(f, raw=b'', headers={'Content-Length': '000'})
            with patch.object(f.cloud, 'request', side_effect=wire.request):
                call()
            self.assert_one_wire(wire)
            self.assertIs(wire.replies[0]['body_empty'], True)
            self.assertTrue(f.campaign.poll_actions())

    def test_bodyless_204_without_content_length_or_transfer_encoding_is_durably_acknowledged(self):
        with Fixture() as f:
            call = self.target(f, 'delete', 'network')
            wire = WireMutations(f, raw=b'', omit_framing=True)
            with patch.object(f.cloud, 'request', side_effect=wire.request):
                call()
            self.assert_one_wire(wire)
            self.assertEqual(len(wire.unframed_responses), 1)
            head, body = wire.unframed_responses[0].split(b'\r\n\r\n', 1)
            self.assertEqual(head.split(b'\r\n')[0], b'HTTP/1.1 204 Fake')
            fields = {line.split(b':', 1)[0].lower() for line in head.split(b'\r\n')[1:]}
            self.assertNotIn(b'content-length', fields)
            self.assertNotIn(b'transfer-encoding', fields)
            self.assertEqual(body, b'')
            reply = wire.replies[0]
            self.assertEqual(reply['status'], 204)
            self.assertIs(reply['json_valid'], True)
            self.assertIs(reply['body_empty'], True)
            self.assertIsNone(reply['payload'])
            intent = f.journal.read()['intents'][-1]
            self.assertEqual(intent['state'], 'ACKNOWLEDGED')
            self.assertEqual(intent['ack'], reply)
            self.assertEqual(intent['actions'], [])
            self.assertIsNone(f.journal.read()['active_lease'])
            self.assertTrue(f.campaign.poll_actions())
            completed = f.journal.read()['intents'][-1]
            self.assertEqual(completed['state'], 'DONE')
            self.assertEqual(completed['ack'], reply)
            self.assertEqual(wire.wire.requests[0]['method'], 'DELETE')
            self.assertEqual(wire.wire.requests[0]['path'], '/v1/networks/201')
            self.assertNotIn(201, f.cloud.rows['network'])
            self.assert_one_wire(wire)

    def test_malformed_resource_ack_retains_all_known_siblings_through_recovery(self):
        for malformed in ('image', 'missing-resource'):
            with self.subTest(malformed=malformed), Fixture() as f:
                call = self.target(f, 'create', 'server')
                sibling = []
                def adverse(reply):
                    server = reply['payload']['server']['id']
                    action = f.cloud.new_action('server', server, 'apply_firewall')
                    f.cloud.actions[action['id']]['status'] = 'running'
                    sibling.append(action['id'])
                    reply['payload']['next_actions'] = [action]
                    if malformed == 'image':
                        reply['payload']['server']['image'] = None
                    else:
                        reply['payload'].pop('server')
                    return reply
                wire = WireMutations(f, adverse)
                with patch.object(f.cloud, 'request', side_effect=wire.request):
                    with self.assertRaisesRegex(mc.GuardError, 'failed or ambiguous'):
                        call()
                self.assert_one_wire(wire)
                before = f.journal.read()['intents'][-1]
                expected = sorted([wire.replies[0]['payload']['action']['id'], *sibling])
                self.assertEqual(before['state'], 'UNKNOWN')
                self.assertEqual(before['actions'], expected)
                self.assertIsNone(before['resource_id'])
                f.closing()
                f.campaign.recover()
                recovered = f.journal.read()['intents'][before['sequence']]
                self.assertEqual(recovered['actions'], expected)
                self.assertEqual(recovered['state'], 'ACKNOWLEDGED')
                self.assertEqual(recovered['original_ack'], before['ack'])
                self.assertFalse(f.campaign.poll_actions())
                result = f.campaign.cleanup_pass()
                self.assertTrue(result['baseline_equal'])
                self.assertFalse(result['complete'])
                self.assertEqual(result['pending_intents'], [before['sequence']])
                self.assertEqual(f.journal.read()['intents'][before['sequence']]['pending_actions'], sibling)
                reads = f.cloud.action_reads.count(sibling[0])
                f.cloud.actions[sibling[0]]['status'] = 'success'
                f.campaign.poll_actions()
                self.assertGreater(f.cloud.action_reads.count(sibling[0]), reads)
                self.assertTrue(f.campaign.completion()['complete'])


if __name__ == '__main__':
    unittest.main()
