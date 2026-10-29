#!/usr/bin/env python3
"""Campaign-owned read budgets through real Cloud pagination and private journals.

Only provider wire replies, process identity and controller clocks are synthetic.
No Campaign/Journal authorization or orchestration method is replaced.
"""
import contextlib
import copy
import os
from pathlib import Path
import signal
import sys
import unittest
from unittest.mock import patch

HERE = Path(os.path.abspath(__file__)).parent
sys.path.insert(0, str(HERE))

import campaign_cloud as cc  # noqa: E402
import creation_ledger as cl  # noqa: E402
import mn_common as mc  # noqa: E402
from test_campaign_cloud import MemoryProvider  # noqa: E402
from test_campaign_safety import Fixture, NS  # noqa: E402


def durable_state(f):
    # A new authenticated observer must see a publication, not just the caller's
    # in-memory state that is still held under the operation lock.
    observer = cl.Journal(f.journal.path, observe=True)
    try:
        return observer.peek()
    finally:
        observer.close()


class CampaignWire(MemoryProvider):
    """Route actual stdlib HTTP bytes to the declared synthetic provider rows."""

    def __init__(self, fixture):
        super().__init__()
        self.fixture, self.backend = fixture, fixture.cloud
        self.reads, self.after_body = [], None
        self.action_rows = None
        self.exact_absent = set()
        fixture.campaign.cloud = cc.Cloud(self.backend.profile, fixture.header['context'],
            connection_factory=self.connection, deadline=contextlib.nullcontext)

    def response_for(self, request, seconds):
        method, path = request['method'], request['path'].removeprefix('/v1')
        if method != 'GET':
            import json
            body = json.loads(request['body']) if request['body'] else None
            reply = self.backend.request(method, path, body, seconds)
            return self.json(reply['payload'], reply['status'])
        route, _, query = path.partition('?')
        parts = route.strip('/').split('/')
        if parts[0] == 'actions':
            return self.json({'action': self.backend.action(int(parts[1]), seconds)})
        kind = next(kind for kind, plural in cc.PLURALS.items() if plural == parts[0])
        if len(parts) == 1:
            rows = self.backend.list(kind, seconds)
            return self.json({parts[0]: rows, 'meta': {'pagination': {
                'next_page': None, 'total_entries': len(rows)}}})
        rid = int(parts[1])
        if len(parts) == 2:
            item = None if (kind, rid) in self.exact_absent else self.backend.describe(kind, rid, seconds)
            return self.json({kind.replace('-', '_'): item}, 200 if item is not None else 404)
        if parts[2:] != ['actions']:
            raise AssertionError('unexpected synthetic GET ' + path)
        rows = (self.backend.resource_actions(kind, rid, seconds) if self.action_rows is None
                else copy.deepcopy(self.action_rows))
        page = int(dict(field.split('=') for field in query.split('&'))['page'])
        # One action per real wire page exposes an eagerly evaluated/reused
        # allowance independently from the Campaign resource_actions call site.
        if not 1 <= page <= max(1, len(rows)):
            raise AssertionError('unexpected action page ' + path)
        return self.json({'actions': rows[page - 1:page], 'meta': {'pagination': {
            'next_page': page + 1 if page < len(rows) else None, 'total_entries': len(rows)}}})

    def connection(self, host, seconds):
        self.replies.append({})
        connection = super().connection(host, seconds)
        sock = self.sockets[-1]
        caller = sys._getframe(1)
        while caller is not None:
            if (Path(caller.f_code.co_filename).name == 'campaign_safety.py'
                    and caller.f_code.co_name in {'create', 'poll_actions', 'recover', 'completion', 'cleanup_pass'}):
                site = caller.f_code.co_name
                break
            caller = caller.f_back
        else:
            raise AssertionError('wire request did not originate in a Campaign caller')
        snapshot = durable_state(self.fixture)
        entry = dict(site=site, allowance=float(seconds), mode=snapshot['mode'],
                     events=copy.deepcopy(snapshot['events']), path=None)
        makefile = sock.makefile
        def incoming(mode):
            request = self.requests[-1]
            entry['path'] = request['path'].removeprefix('/v1')
            if request['method'] == 'GET':
                self.reads.append(entry)
            sock.reply = self.response_for(request, seconds)
            return makefile(mode)
        sock.makefile = incoming
        getresponse = connection.getresponse
        def response():
            result = getresponse()
            read = result.read
            def body(size):
                raw = read(size)
                if self.after_body is not None:
                    self.after_body(entry)
                return raw
            result.read = body
            return result
        connection.getresponse = response
        return connection


class CampaignReadBudgetTest(unittest.TestCase):
    def setUp(self):
        self.stack = contextlib.ExitStack()
        self.addCleanup(self.stack.close)
        self.stack.enter_context(patch.dict(os.environ, {}, clear=True))
        self.stack.enter_context(patch.object(os, 'pidfd_open', side_effect=AssertionError('real pidfd forbidden')))
        self.stack.enter_context(patch.object(signal, 'pidfd_send_signal', side_effect=AssertionError('real signal forbidden')))
        self.stack.enter_context(patch.object(cc.http.client.HTTPSConnection, 'connect',
            side_effect=AssertionError('real network forbidden')))

    def advance(self, f, seconds):
        f.wall += seconds * NS
        f.mono += seconds * NS

    def assert_incident(self, entry):
        self.assertEqual(entry['mode'], 'INCIDENT')
        events = [event for event in entry['events'] if event['event'] == 'cleanup-reserve-exhausted']
        self.assertEqual(len(events), 1)
        self.assertIs(events[0]['hard_lifetime_met'], False)
        self.assertIs(events[0]['provisioning_reopened'], False)

    def test_create_precall_census_shortens_and_stops_before_the_next_get_at_cutoff(self):
        for cutoff in (False, True):
            with self.subTest(cutoff=cutoff), Fixture() as f:
                wire = CampaignWire(f)
                f.wall = f.header['time']['c_ns'] - 7 * NS
                f.heartbeat()
                def received(entry):
                    if len(wire.reads) == 1:
                        self.advance(f, 5 if cutoff else 1)
                wire.after_body = received
                if cutoff:
                    with self.assertRaisesRegex(mc.GuardError, 'budget exhausted'):
                        f.create()
                    self.assertEqual([(r['path'], r['allowance']) for r in wire.reads],
                        [('/servers?page=1&per_page=50', 5)])
                    self.assertEqual(f.journal.read()['intents'], [])
                    self.assertEqual((wire.posts, f.cloud.requests), (0, []))
                else:
                    rid = f.create()
                    self.assertEqual(len(wire.reads), len(mc.PROTECT_KINDS))
                    self.assertEqual([r['allowance'] for r in wire.reads], [5] + [4] * 9)
                    self.assertTrue(all(r['site'] == 'create' for r in wire.reads))
                    self.assertEqual((wire.posts, f.cloud.requests[-1][3]), (1, 4))
                    self.assertEqual(f.journal.read()['owned'][0]['id'], rid)
                self.assertEqual(len(wire.connections), len(wire.requests))
                self.assertTrue(all(sock.closed for sock in wire.sockets))

    def test_poll_actions_itself_cannot_issue_a_get_after_setup_cutoff(self):
        for cutoff in (False, True):
            with self.subTest(cutoff=cutoff), Fixture() as f:
                f.create('server')  # The firewall and server each have a real saved action ID.
                ids = [rid for intent in f.journal.read()['intents'] for rid in intent['actions']]
                self.assertEqual(len(ids), 2)
                for action in f.cloud.actions.values():
                    action['status'] = 'running'
                wire = CampaignWire(f)
                f.wall = f.header['time']['setup_ns'] - 7 * NS
                def received(entry):
                    if len(wire.reads) == 1:
                        self.advance(f, 5 if cutoff else 1)
                wire.after_body = received
                if cutoff:
                    # Call poll_actions directly: no later wait_actions/orchestrator
                    # budget check can make an accidentally admitted GET pass.
                    with self.assertRaisesRegex(mc.GuardError, 'budget exhausted'):
                        f.campaign.poll_actions()
                else:
                    self.assertFalse(f.campaign.poll_actions())
                expected = ids[:1] if cutoff else ids
                self.assertEqual([r['path'] for r in wire.reads], [f'/actions/{rid}' for rid in expected])
                self.assertEqual([r['allowance'] for r in wire.reads], [5] if cutoff else [5, 4])
                self.assertEqual(len(wire.connections), len(expected))
                self.assertTrue(all(r['site'] == 'poll_actions' and r['mode'] == 'OPEN' for r in wire.reads))
                self.assertEqual(wire.posts, 0)

    def action_observation_case(self, f, same_intent):
        materialize = f.cloud.materialize
        def resource(kind, body):
            payload = materialize(kind, body)
            if kind == 'firewall' and same_intent:
                payload['actions'].append(f.cloud.new_action('firewall', payload['firewall']['id'], 'apply_firewall'))
            return payload
        with patch.object(f.cloud, 'materialize', side_effect=resource):
            self.assertEqual(f.create('server'), 101)
        state = f.journal.read()
        firewall = next(intent for intent in state['intents'] if intent['kind'] == 'firewall')
        server = next(intent for intent in state['intents'] if intent['kind'] == 'server')
        self.assertEqual(firewall['state'], 'ACKNOWLEDGED')
        self.assertEqual(server['state'], 'ACKNOWLEDGED')
        self.assertEqual(len(firewall['actions']), 2 if same_intent else 1)
        self.assertEqual(len(server['actions']), 1)
        self.assertEqual(len(f.cloud.requests), 3)
        return firewall['sequence'], firewall['actions']

    def first_action_reply(self, wire, rid, observation):
        response_for = wire.response_for
        def response(request, seconds):
            if observation == 'unavailable' and request['path'] == f'/v1/actions/{rid}':
                return wire.json({'error': {'message': 'synthetic first action unavailable'}}, 500)
            return response_for(request, seconds)
        return patch.object(wire, 'response_for', side_effect=response)

    def assert_action_observation(self, state, sequence, ids, observation):
        intent = state['intents'][sequence]
        expected_pending = ids[1:] if observation == 'error' else ids
        self.assertEqual(intent['pending_actions'], expected_pending,
                         'unread known siblings must stay pending until observed terminal')
        self.assertEqual(intent['state'], 'ACKNOWLEDGED' if expected_pending else 'FAILED')
        self.assertIs(intent.get('operation_failed') is True, observation == 'error')
        diagnostics = [event for event in state['events'] if event.get('event') == 'action-read-unavailable'
                       and event.get('intent') == sequence]
        if observation == 'unavailable':
            self.assertEqual(len(diagnostics), 1)
            self.assertEqual(diagnostics[0]['action_id'], ids[0])
            self.assertEqual(diagnostics[0]['error_type'], 'GuardError')
        else:
            self.assertEqual(diagnostics, [])
        return diagnostics

    def refresh_observation_closed(self, f, wire, sequence, observation, diagnostics):
        wire.after_body = None
        for action in f.cloud.actions.values():
            action['status'] = 'success'
        f.closing()
        self.assertIs(f.campaign.poll_actions(), observation != 'error')
        final = durable_state(f)
        intent = final['intents'][sequence]
        self.assertEqual(intent['pending_actions'], [])
        self.assertEqual(intent['state'], 'FAILED' if observation == 'error' else 'DONE')
        self.assertIs(intent.get('operation_failed') is True, observation == 'error')
        retained = [event for event in final['events'] if event.get('event') == 'action-read-unavailable'
                    and event.get('intent') == sequence]
        self.assertEqual(retained, diagnostics, 'later success must retain earlier unavailable-read evidence')

    def test_unchanged_done_multi_action_repoll_has_only_the_final_batch_publication(self):
        with Fixture() as f:
            self.action_observation_case(f, same_intent=True)
            wire = CampaignWire(f)
            self.assertTrue(f.campaign.poll_actions())
            baseline = f.journal.read()
            self.assertTrue(all(intent['state'] == 'DONE' and intent['pending_actions'] == []
                                for intent in baseline['intents']))
            known = [rid for intent in baseline['intents'] for rid in intent['actions']]
            self.assertEqual(known, [1001, 1002, 1003], 'two firewall actions plus one server action must be real')
            self.assertTrue(any(len(intent['actions']) == 2 for intent in baseline['intents']),
                            'a single-action intent cannot expose transient unread-sibling demotion')
            expected_paths = [f'/actions/{rid}' for rid in known]
            self.assertEqual([entry['path'] for entry in wire.reads], expected_paths,
                             'the initial DONE evidence must come from actual Cloud action GETs')
            for rid in known:
                self.assertEqual(f.cloud.actions[rid]['status'], 'success')
            checkpoint = copy.deepcopy(f.journal.checkpoint)
            mutations = copy.deepcopy(f.cloud.requests)
            wire.reads.clear()
            before_requests, before_connections = len(wire.requests), len(wire.connections)
            observed = []
            def received(entry):
                observer = cl.Journal(f.journal.path, observe=True)
                try:
                    state = observer.peek()
                    at_read = copy.deepcopy(observer.checkpoint)
                finally:
                    observer.close()
                observed.append((entry['path'], at_read))
                self.assertEqual(state, baseline,
                                 'unchanged terminal siblings must not become pending during refresh')
                self.assertEqual(at_read, checkpoint,
                                 'unchanged DONE action replies must not cause intermediate journal publications')
            wire.after_body = received
            self.assertTrue(f.campaign.poll_actions())
            after = f.journal.read()
            self.assertEqual(after, baseline)
            self.assertEqual([path for path, _ in observed], expected_paths)
            self.assertEqual([entry['path'] for entry in wire.reads], expected_paths)
            self.assertEqual([entry['allowance'] for entry in wire.reads], [20] * 3)
            dispatched = wire.requests[before_requests:]
            self.assertEqual([request['path'] for request in dispatched], ['/v1' + path for path in expected_paths])
            self.assertTrue(all(request['method'] == 'GET' for request in dispatched))
            self.assertEqual(len(wire.connections) - before_connections, 3)
            self.assertEqual(f.journal.checkpoint['generation'] - checkpoint['generation'], 1,
                             'retain exactly the final batch commit: neither duplicate publications nor zero commits')
            self.assertNotEqual(f.journal.checkpoint['sha256'], checkpoint['sha256'])
            self.assertEqual(f.cloud.requests, mutations)
            self.assertEqual(wire.posts, 0)
            self.assertTrue(all(sock.closed for sock in wire.sockets))
            self.assertFalse((f.guard_directory / 'stop.json').exists())

    def test_each_observed_action_is_durable_before_a_later_setup_cutoff(self):
        for same_intent in (False, True):
            for observation in ('error', 'running', 'unavailable'):
                with self.subTest(same_intent=same_intent, observation=observation), Fixture() as f:
                    sequence, ids = self.action_observation_case(f, same_intent)
                    f.cloud.actions[ids[0]]['status'] = observation if observation != 'unavailable' else 'success'
                    wire = CampaignWire(f)
                    mutations = copy.deepcopy(f.cloud.requests)
                    f.wall = f.header['time']['setup_ns'] - 7 * NS
                    first = []
                    def received(entry):
                        self.assertEqual(entry['path'], f'/actions/{ids[0]}')
                        self.assertEqual((entry['site'], entry['mode'], entry['allowance']), ('poll_actions', 'OPEN', 5))
                        first.append(copy.deepcopy(entry))
                        self.advance(f, 5)
                    wire.after_body = received
                    with self.first_action_reply(wire, ids[0], observation):
                        with self.assertRaisesRegex(mc.GuardError, '^provisioning budget exhausted$'):
                            f.campaign.poll_actions()
                    self.assertEqual(len(first), 1)
                    self.assertEqual(f.wall, f.header['time']['setup_ns'] - 2 * NS)
                    self.assertEqual([entry['path'] for entry in wire.reads], [f'/actions/{ids[0]}'])
                    self.assertEqual((len(wire.connections), len(wire.requests)), (1, 1),
                                     'no later sibling/server GET may start at cutoff')
                    retained = durable_state(f)
                    self.assertEqual(retained['mode'], 'OPEN')
                    diagnostics = self.assert_action_observation(retained, sequence, ids, observation)
                    self.assertEqual(wire.posts, 0)
                    self.assertEqual(f.cloud.requests, mutations)
                    self.refresh_observation_closed(f, wire, sequence, observation, diagnostics)
                    self.assertEqual(f.cloud.requests, mutations)
                    self.assertTrue(all(request['method'] == 'GET' for request in wire.requests))
                    self.assertTrue(all(sock.closed for sock in wire.sockets))

    def test_observed_error_or_unavailable_is_durable_before_stop_receipt_publication_failure(self):
        for same_intent in (False, True):
            for observation in ('error', 'unavailable'):
                with self.subTest(same_intent=same_intent, observation=observation), Fixture() as f:
                    sequence, ids = self.action_observation_case(f, same_intent)
                    f.cloud.actions[ids[0]]['status'] = observation if observation == 'error' else 'success'
                    wire = CampaignWire(f)
                    mutations = copy.deepcopy(f.cloud.requests)
                    stop = f.guard_directory / 'stop.json'
                    self.assertFalse(stop.exists())
                    f.wall = f.header['time']['setup_ns'] - 7 * NS
                    blocked, replace = [], os.replace
                    def publication(source, destination, *args, **kwargs):
                        if Path(destination) == stop:
                            observed = durable_state(f)
                            self.assert_action_observation(observed, sequence, ids, observation)
                            self.assertEqual([entry['path'] for entry in wire.reads], [f'/actions/{ids[0]}'])
                            blocked.append(copy.deepcopy(observed))
                            raise OSError('synthetic stop receipt publication failure')
                        return replace(source, destination, *args, **kwargs)
                    with self.first_action_reply(wire, ids[0], observation), \
                            patch.object(os, 'replace', side_effect=publication):
                        with self.assertRaisesRegex(OSError, '^synthetic stop receipt publication failure$'):
                            f.campaign.poll_actions()
                    self.assertEqual(len(blocked), 1, 'the real stop publication must reach the filesystem fault')
                    retained = durable_state(f)
                    self.assertEqual(retained, blocked[0])
                    diagnostics = self.assert_action_observation(retained, sequence, ids, observation)
                    self.assertEqual((len(wire.connections), len(wire.requests)), (1, 1))
                    self.assertFalse(stop.exists(), 'failure occurred before receipt replacement')
                    self.assertEqual(wire.posts, 0)
                    self.assertEqual(f.cloud.requests, mutations)
                    self.refresh_observation_closed(f, wire, sequence, observation, diagnostics)
                    self.assertEqual(f.cloud.requests, mutations)
                    self.assertTrue(all(request['method'] == 'GET' for request in wire.requests))
                    self.assertTrue(all(sock.closed for sock in wire.sockets))

    def terminal_action_observation_case(self, f, terminal):
        sequence, ids = self.action_observation_case(f, same_intent=True)
        self.assertEqual((sequence, ids), (0, [1001, 1002]))
        wire = CampaignWire(f)
        stop = f.guard_directory / 'stop.json'
        self.assertFalse(stop.exists())
        setup_faults = []
        if terminal == 'DONE':
            self.assertTrue(f.campaign.poll_actions())
            expected_reads = ['/actions/1001', '/actions/1002', '/actions/1003']
        else:
            self.assertEqual(terminal, 'FAILED')
            # Fail only the actual stop publication after the LAST known
            # firewall action reports error. This creates real FAILED evidence
            # with no pending siblings and no stop receipt, without deleting a
            # policy artifact or inventing authenticated journal state.
            f.cloud.actions[1002]['status'] = 'error'
            replace = os.replace
            def publication(source, destination, *args, **kwargs):
                if Path(destination) == stop:
                    state = durable_state(f)
                    intent = state['intents'][sequence]
                    self.assertEqual((intent['state'], intent['pending_actions']), ('FAILED', []))
                    self.assertIs(intent.get('operation_failed'), True)
                    self.assertEqual([entry['path'] for entry in wire.reads], ['/actions/1001', '/actions/1002'])
                    setup_faults.append(copy.deepcopy(intent))
                    raise OSError('synthetic terminal setup stop publication failure')
                return replace(source, destination, *args, **kwargs)
            with patch.object(os, 'replace', side_effect=publication):
                with self.assertRaisesRegex(OSError, '^synthetic terminal setup stop publication failure$'):
                    f.campaign.poll_actions()
            self.assertEqual(len(setup_faults), 1)
            expected_reads = ['/actions/1001', '/actions/1002']
        state = durable_state(f)
        intent = state['intents'][sequence]
        self.assertEqual(state['mode'], 'OPEN')
        self.assertEqual(intent['actions'], [1001, 1002])
        self.assertEqual((intent['state'], intent['pending_actions']), (terminal, []))
        self.assertIs(intent.get('operation_failed') is True, terminal == 'FAILED')
        self.assertEqual([entry['path'] for entry in wire.reads], expected_reads)
        self.assertEqual([request['path'] for request in wire.requests], ['/v1' + path for path in expected_reads])
        self.assertEqual(len(wire.connections), len(expected_reads))
        self.assertTrue(all(request['method'] == 'GET' for request in wire.requests))
        self.assertFalse(stop.exists(), 'the next fault must target a new actual stop publication')
        self.assertEqual(wire.posts, 0)
        return wire, sequence, ids

    def assert_terminal_action_refresh(self, state, sequence, terminal, observation):
        intent = state['intents'][sequence]
        self.assertEqual(intent['actions'], [1001, 1002])
        self.assertEqual(intent['state'], 'ACKNOWLEDGED')
        self.assertEqual(intent['pending_actions'], [1002] if observation == 'error' else [1001, 1002],
                         'terminal refresh must durably propagate the exact unread sibling, not just the current ID')
        self.assertIs(intent.get('operation_failed') is True, terminal == 'FAILED' or observation == 'error')
        diagnostics = [event for event in state['events'] if event.get('event') == 'action-read-unavailable'
                       and event.get('intent') == sequence]
        if observation == 'unavailable':
            self.assertEqual(len(diagnostics), 1)
            self.assertEqual((diagnostics[0]['action_id'], diagnostics[0]['error_type']), (1001, 'GuardError'))
        else:
            self.assertEqual(diagnostics, [])
        return diagnostics

    def refresh_terminal_observation_closed(self, f, wire, sequence, terminal, observation, diagnostics):
        wire.after_body = None
        for action in f.cloud.actions.values():
            action['status'] = 'success'
        at_cutover = (f.wall, f.mono)
        f.closing()
        self.assertEqual((f.wall, f.mono), at_cutover, 'closed refresh must not reset or extend the provisioning clock')
        start = len(wire.requests)
        failed = terminal == 'FAILED' or observation == 'error'
        self.assertIs(f.campaign.poll_actions(), not failed)
        self.assertEqual([request['path'] for request in wire.requests[start:]],
                         ['/v1/actions/1001', '/v1/actions/1002', '/v1/actions/1003'])
        final = durable_state(f)
        intent = final['intents'][sequence]
        self.assertEqual((intent['state'], intent['pending_actions']), ('FAILED' if failed else 'DONE', []))
        self.assertIs(intent.get('operation_failed') is True, failed)
        self.assertEqual([event for event in final['events'] if event.get('event') == 'action-read-unavailable'
                          and event.get('intent') == sequence], diagnostics)
        self.assertTrue(all(request['method'] == 'GET' for request in wire.requests))
        self.assertTrue(all(socket.closed for socket in wire.sockets))
        self.assertEqual(wire.posts, 0)

    def test_terminal_multi_action_refresh_persists_unread_siblings_before_setup_cutoff(self):
        for terminal in ('DONE', 'FAILED'):
            for observation in ('running', 'error', 'unavailable'):
                with self.subTest(terminal=terminal, observation=observation), Fixture() as f:
                    wire, sequence, ids = self.terminal_action_observation_case(f, terminal)
                    mutations = copy.deepcopy(f.cloud.requests)
                    f.cloud.actions[1001]['status'] = observation if observation != 'unavailable' else 'success'
                    wire.reads.clear()
                    start_requests, start_connections = len(wire.requests), len(wire.connections)
                    f.wall = f.header['time']['setup_ns'] - 7 * NS
                    first = []
                    def received(entry):
                        self.assertEqual((entry['path'], entry['site'], entry['mode'], entry['allowance']),
                                         ('/actions/1001', 'poll_actions', 'OPEN', 5))
                        first.append(copy.deepcopy(entry))
                        self.advance(f, 5)
                    wire.after_body = received
                    with self.first_action_reply(wire, ids[0], observation):
                        with self.assertRaisesRegex(mc.GuardError, '^provisioning budget exhausted$'):
                            f.campaign.poll_actions()
                    self.assertEqual(len(first), 1)
                    self.assertEqual(f.wall, f.header['time']['setup_ns'] - 2 * NS)
                    self.assertEqual([request['path'] for request in wire.requests[start_requests:]], ['/v1/actions/1001'])
                    self.assertEqual(len(wire.connections) - start_connections, 1,
                                     'interrupt before the unread sibling GET, not after observing it')
                    retained = durable_state(f)
                    self.assertEqual(retained['mode'], 'OPEN')
                    diagnostics = self.assert_terminal_action_refresh(retained, sequence, terminal, observation)
                    self.assertEqual(f.cloud.requests, mutations)
                    self.refresh_terminal_observation_closed(f, wire, sequence, terminal, observation, diagnostics)
                    self.assertEqual(f.cloud.requests, mutations, 'terminal refresh must not dispatch provider mutations')

    def test_terminal_multi_action_refresh_persists_unread_siblings_before_new_stop_failure(self):
        for terminal in ('DONE', 'FAILED'):
            for observation in ('error', 'unavailable'):
                with self.subTest(terminal=terminal, observation=observation), Fixture() as f:
                    wire, sequence, ids = self.terminal_action_observation_case(f, terminal)
                    mutations = copy.deepcopy(f.cloud.requests)
                    f.cloud.actions[1001]['status'] = 'error' if observation == 'error' else 'success'
                    wire.reads.clear()
                    start_requests, start_connections = len(wire.requests), len(wire.connections)
                    stop = f.guard_directory / 'stop.json'
                    self.assertFalse(stop.exists())
                    f.wall = f.header['time']['setup_ns'] - 7 * NS
                    faults, replace = [], os.replace
                    def publication(source, destination, *args, **kwargs):
                        if Path(destination) == stop:
                            state = durable_state(f)
                            self.assert_terminal_action_refresh(state, sequence, terminal, observation)
                            self.assertEqual([request['path'] for request in wire.requests[start_requests:]],
                                             ['/v1/actions/1001'])
                            faults.append(copy.deepcopy(state))
                            raise OSError('synthetic terminal refresh stop publication failure')
                        return replace(source, destination, *args, **kwargs)
                    with self.first_action_reply(wire, ids[0], observation), \
                            patch.object(os, 'replace', side_effect=publication):
                        with self.assertRaisesRegex(OSError, '^synthetic terminal refresh stop publication failure$'):
                            f.campaign.poll_actions()
                    self.assertEqual(len(faults), 1, 'reach the NEW refresh fault independently of the FAILED setup fault')
                    retained = durable_state(f)
                    self.assertEqual(retained, faults[0])
                    self.assertEqual(retained['mode'], 'OPEN')
                    diagnostics = self.assert_terminal_action_refresh(retained, sequence, terminal, observation)
                    self.assertEqual(len(wire.requests) - start_requests, 1)
                    self.assertEqual(len(wire.connections) - start_connections, 1)
                    self.assertFalse(stop.exists(), 'the real replacement fault must occur before publication')
                    self.assertEqual(f.cloud.requests, mutations)
                    self.refresh_terminal_observation_closed(f, wire, sequence, terminal, observation, diagnostics)
                    self.assertEqual(f.cloud.requests, mutations, 'terminal refresh must not dispatch provider mutations')

    def test_recovery_exact_and_each_resource_action_page_recompute_phase_budget(self):
        for branch in ('unknown-id', 'known-id'):
            for cross_h in (False, True):
                with self.subTest(branch=branch, cross_h=cross_h), Fixture() as f:
                    f.dependencies()
                    f.cloud.lose_ack = True
                    with self.assertRaisesRegex(mc.GuardError, 'failed or ambiguous'):
                        f.create('server')
                    self.assertIsNone(f.journal.read()['intents'][-1]['resource_id'])
                    f.closing()
                    wire = CampaignWire(f)
                    if branch == 'known-id':
                        # Real first recovery accepts exact ownership but cannot
                        # identify an asynchronous create from an empty census.
                        wire.action_rows = []
                        f.campaign.recover()
                        unresolved = f.journal.read()['intents'][-1]
                        self.assertEqual((unresolved['resource_id'], unresolved['state']), (101, 'UNKNOWN'))
                        self.assertEqual(unresolved['actions'], [])
                        self.assertIsNone(unresolved['ack'])
                    wire.action_rows = None
                    f.cloud.new_action('server', 101, 'start_server')
                    wire.reads.clear()
                    before = len(wire.requests)
                    f.wall = f.header['time']['h_ns'] - 8 * NS
                    def received(entry):
                        if entry['path'].startswith('/certificates?'):
                            self.advance(f, 1)
                        elif entry['path'] == '/servers/101':
                            self.advance(f, 1)
                        elif entry['path'] == '/servers/101/actions?page=1&per_page=50':
                            self.advance(f, 4 if cross_h else 1)
                    wire.after_body = received
                    f.campaign.recover()
                    lookup = [r for r in wire.reads if r['path'].startswith('/servers/101')]
                    self.assertEqual([r['path'] for r in lookup], ['/servers/101',
                        '/servers/101/actions?page=1&per_page=50', '/servers/101/actions?page=2&per_page=50'])
                    self.assertEqual([r['allowance'] for r in lookup], [5, 4, 20 if cross_h else 3])
                    self.assertTrue(all(r['site'] == 'recover' for r in wire.reads))
                    self.assertEqual(len(wire.requests) - before, len(mc.PROTECT_KINDS) + 3)
                    self.assertEqual([r['allowance'] for r in wire.reads[:10]], [6] * 10)
                    if cross_h:
                        self.assert_incident(lookup[-1])
                    else:
                        self.assertTrue(all(r['mode'] == 'CLOSING' for r in lookup))
                    recovered = f.journal.read()['intents'][-1]
                    self.assertEqual(recovered['state'], 'ACKNOWLEDGED')
                    self.assertEqual(recovered['actions'], [1002, 1003])
                    self.assertIs(recovered['required_action_identified'], True)
                    self.assertIsNone(recovered['ack'])
                    self.assertEqual(wire.posts, 0)

    def test_open_unknown_candidate_cutoff_after_final_census_escapes_before_exact_get(self):
        for kind in ('network', 'server'):
            for cutoff in (False, True):
                with self.subTest(kind=kind, cutoff=cutoff), Fixture() as f:
                    if kind == 'server':
                        f.dependencies()
                    f.cloud.lose_ack = True
                    with self.assertRaisesRegex(mc.GuardError, 'failed or ambiguous'):
                        f.create(kind)
                    f.cloud.lose_ack = False
                    initial = f.journal.read()
                    sequence = len(initial['intents']) - 1
                    intent = initial['intents'][sequence]
                    self.assertEqual((initial['mode'], intent['state']), ('OPEN', 'UNKNOWN'))
                    self.assertIsNone(intent['resource_id'])
                    self.assertIsNone(intent['ack'])
                    self.assertFalse(any(record['kind'] == kind for record in initial['owned']))
                    rid = 201 if kind == 'network' else 101
                    candidate = copy.deepcopy(f.cloud.rows[kind][rid])
                    self.assertEqual(candidate['labels']['intent'], intent['nonce'])
                    head, checkpoint = f.journal.path.read_bytes(), copy.deepcopy(f.journal.checkpoint)
                    mutations = copy.deepcopy(f.cloud.requests)
                    describes = copy.deepcopy(f.cloud.describes)
                    wire = CampaignWire(f)
                    f.wall = f.header['time']['setup_ns'] - 7 * NS
                    final_pages = []
                    def received(entry):
                        if entry['path'] == '/certificates?page=1&per_page=50':
                            self.assertEqual((entry['site'], entry['mode'], entry['allowance']),
                                             ('recover', 'OPEN', 5))
                            self.assertEqual(len(wire.reads), len(mc.PROTECT_KINDS))
                            final_pages.append(copy.deepcopy(entry))
                            self.advance(f, 5 if cutoff else 1)
                    wire.after_body = received
                    if cutoff:
                        with self.assertRaisesRegex(mc.GuardError, '^provisioning budget exhausted$'):
                            f.campaign.recover()
                    else:
                        f.campaign.recover()
                    self.assertEqual(len(final_pages), 1, 'the final census response must consume the allowance')
                    census_paths = [f'/{cc.PLURALS[k]}?page=1&per_page=50' for k in mc.PROTECT_KINDS]
                    self.assertEqual([entry['path'] for entry in wire.reads[:10]], census_paths)
                    self.assertEqual([entry['allowance'] for entry in wire.reads[:10]], [5] * 10)
                    self.assertTrue(all(entry['site'] == 'recover' and entry['mode'] == 'OPEN' for entry in wire.reads))
                    candidate_path = f'/{cc.PLURALS[kind]}/{rid}'
                    retained = durable_state(f)
                    current = retained['intents'][sequence]
                    self.assertEqual(retained['mode'], 'OPEN')
                    self.assertIsNone(current['ack'])
                    self.assertFalse(any(event.get('event') in {'recovery-candidate-unavailable',
                        'recovery-candidate-refused', 'recovery-action-read-unavailable'}
                        or event.get('inconclusive_candidates') for event in retained['events']),
                        'controller cutoff must not become provider-unavailable/inconclusive evidence')
                    if cutoff:
                        self.assertEqual(f.wall, f.header['time']['setup_ns'] - 2 * NS)
                        self.assertEqual([entry['path'] for entry in wire.reads], census_paths)
                        self.assertEqual(len(wire.connections), len(mc.PROTECT_KINDS))
                        self.assertEqual(len(wire.requests), len(mc.PROTECT_KINDS))
                        self.assertFalse(any(request['path'] == '/v1' + candidate_path for request in wire.requests))
                        self.assertEqual(f.cloud.describes, describes, 'no provider candidate exact lookup may start')
                        self.assertEqual(retained, initial)
                        self.assertEqual(f.journal.path.read_bytes(), head)
                        self.assertEqual(f.journal.checkpoint, checkpoint)
                    else:
                        expected = census_paths + [candidate_path]
                        if kind == 'server':
                            expected.append('/servers/101/actions?page=1&per_page=50')
                        self.assertEqual([entry['path'] for entry in wire.reads], expected)
                        self.assertEqual([entry['allowance'] for entry in wire.reads[10:]], [4] * (len(expected) - 10))
                        self.assertEqual(len(wire.connections), len(expected))
                        self.assertEqual(len(wire.requests), len(expected))
                        self.assertEqual((current['resource_id'], current['state']), (rid, 'ACKNOWLEDGED'))
                        self.assertIs(current['required_action_identified'], True)
                        self.assertEqual(current['recovery']['candidate'], candidate)
                        censuses = [event for event in retained['events'] if event.get('event') == 'recovery-census'
                                    and event.get('intent') == sequence]
                        self.assertEqual(len(censuses), 1)
                        self.assertEqual(censuses[0]['candidates'], [rid])
                        self.assertEqual(censuses[0]['inconclusive_candidates'], [])
                    self.assertTrue(all(request['method'] == 'GET' for request in wire.requests))
                    self.assertTrue(all(sock.closed for sock in wire.sockets))
                    self.assertEqual(wire.posts, 0)
                    self.assertEqual(f.cloud.requests, mutations, 'recovery must never replay or start a provider mutation')

    def test_open_recovery_action_page_policy_error_escapes_for_unknown_and_known_server(self):
        for branch in ('unknown-id', 'known-id'):
            with self.subTest(branch=branch), Fixture() as f:
                f.dependencies()
                f.cloud.lose_ack = True
                with self.assertRaisesRegex(mc.GuardError, 'failed or ambiguous'):
                    f.create('server')
                initial = f.journal.read()
                self.assertEqual(initial['mode'], 'OPEN')
                self.assertIsNone(initial['intents'][-1]['resource_id'])
                wire = CampaignWire(f)
                if branch == 'known-id':
                    # Recover the real identity with a successful empty action
                    # census, without closing provisioning admission.
                    wire.action_rows = []
                    f.campaign.recover()
                    known = f.journal.read()
                    self.assertEqual(known['mode'], 'OPEN')
                    self.assertEqual((known['intents'][-1]['resource_id'], known['intents'][-1]['state']),
                                     (101, 'UNKNOWN'))
                    self.assertEqual(known['intents'][-1]['actions'], [])
                    self.assertIsNone(known['intents'][-1]['ack'])
                wire.action_rows = None
                f.cloud.new_action('server', 101, 'start_server')
                self.assertEqual(len(f.cloud.resource_actions('server', 101)), 2)
                admitted = f.journal.read()
                self.assertEqual(admitted['mode'], 'OPEN')
                self.assertEqual(admitted['intents'][-1]['resource_id'],
                                 101 if branch == 'known-id' else None)
                wire.reads.clear()
                before_requests, before_connections = len(wire.requests), len(wire.connections)
                requests_before = copy.deepcopy(f.cloud.requests)
                f.wall = f.header['time']['setup_ns'] - 7 * NS
                first_pages = []
                def received(entry):
                    if entry['path'] == '/servers/101/actions?page=1&per_page=50':
                        self.assertEqual(entry['site'], 'recover')
                        self.assertEqual(entry['mode'], 'OPEN')
                        self.assertEqual(entry['allowance'], 5)
                        first_pages.append(copy.deepcopy(entry))
                        self.advance(f, 5)
                wire.after_body = received
                with self.assertRaisesRegex(mc.GuardError, '^provisioning budget exhausted$'):
                    f.campaign.recover()
                self.assertEqual(len(first_pages), 1)
                self.assertEqual(f.wall, f.header['time']['setup_ns'] - 2 * NS)
                lookup = [r for r in wire.reads if r['path'].startswith('/servers/101')]
                self.assertEqual([r['path'] for r in lookup], ['/servers/101',
                    '/servers/101/actions?page=1&per_page=50'])
                self.assertEqual([r['allowance'] for r in lookup], [5, 5])
                self.assertEqual([r['allowance'] for r in wire.reads[:10]], [5] * 10)
                self.assertTrue(all(r['site'] == 'recover' and r['mode'] == 'OPEN' for r in wire.reads))
                dispatched = wire.requests[before_requests:]
                self.assertEqual(len(dispatched), len(mc.PROTECT_KINDS) + 2)
                self.assertTrue(all(r['method'] == 'GET' for r in dispatched))
                self.assertFalse(any('/actions?page=2' in r['path'] for r in dispatched))
                self.assertEqual(len(wire.connections) - before_connections, len(dispatched))
                self.assertTrue(all(sock.closed for sock in wire.sockets))
                self.assertEqual(wire.posts, 0)
                self.assertEqual(f.cloud.requests, requests_before)
                retained = durable_state(f)
                self.assertEqual(retained['mode'], 'OPEN')
                self.assertFalse(any(e.get('event') == 'recovery-action-read-unavailable' for e in retained['events']))
                self.assertFalse(any(e.get('event') == 'cleanup-reserve-exhausted' for e in retained['events']))

    def test_completion_census_shortens_then_publishes_incident_before_later_get(self):
        with Fixture() as f:
            f.create()
            self.assertTrue(f.campaign.poll_actions())
            f.closing()
            wire = CampaignWire(f)
            f.wall = f.header['time']['h_ns'] - 7 * NS
            def received(entry):
                if len(wire.reads) == 1:
                    self.advance(f, 1)
                elif len(wire.reads) == 2:
                    self.advance(f, 4)
            wire.after_body = received
            result = f.campaign.completion()
            self.assertFalse(result['complete'])  # Owned network still exists.
            self.assertEqual(len(wire.reads), len(mc.PROTECT_KINDS) + 1)
            self.assertEqual(wire.reads[-1]['path'], '/networks/201')
            self.assertEqual([r['allowance'] for r in wire.reads], [5, 4] + [20] * 9)
            self.assertTrue(all(r['site'] == 'completion' for r in wire.reads))
            self.assertEqual([r['mode'] for r in wire.reads[:2]], ['CLOSING', 'CLOSING'])
            for entry in wire.reads[2:]:
                self.assert_incident(entry)
            self.assertEqual(durable_state(f)['mode'], 'INCIDENT')
            self.assertEqual(wire.posts, 0)

    def test_cleanup_pass_preliminary_exact_read_has_its_own_phase_budget(self):
        for exhausted in (False, True):
            with self.subTest(exhausted=exhausted), Fixture() as f:
                rid = f.create()
                self.assertTrue(f.campaign.poll_actions())
                f.closing()
                wire = CampaignWire(f)
                wire.exact_absent.add(('network', rid))
                def received(entry):
                    if entry['site'] == 'recover' and entry['path'].startswith('/certificates?'):
                        f.wall = f.header['time']['h_ns'] - (2 if exhausted else 7) * NS
                    elif entry['site'] == 'cleanup_pass':
                        self.advance(f, 5)
                wire.after_body = received
                result = f.campaign.cleanup_pass()
                self.assertFalse(result['complete'])
                preliminary = [r for r in wire.reads if r['site'] == 'cleanup_pass']
                self.assertEqual(len(preliminary), 1)
                self.assertEqual((preliminary[0]['path'], preliminary[0]['allowance']),
                    (f'/networks/{rid}', 20 if exhausted else 5))
                if exhausted:
                    self.assert_incident(preliminary[0])
                else:
                    self.assertEqual(preliminary[0]['mode'], 'CLOSING')
                completion = [r for r in wire.reads if r['site'] == 'completion']
                self.assertEqual(len(completion), len(mc.PROTECT_KINDS) + 1)
                self.assertEqual(completion[-1]['path'], f'/networks/{rid}')
                self.assertEqual([r['allowance'] for r in completion], [20] * 11)
                for entry in completion:
                    self.assert_incident(entry)
                state = durable_state(f)
                self.assertTrue(any(e.get('event') == 'observed-retirement' and e.get('kind') == 'network'
                    and e.get('id') == rid and e.get('source') == 'cleanup-exact-readback' for e in state['events']))
                self.assertFalse(any(request['method'] == 'DELETE' for request in wire.requests))
                self.assertEqual(wire.posts, 0)


if __name__ == '__main__':
    unittest.main()
