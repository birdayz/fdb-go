#!/usr/bin/env python3
"""Validated ACK/list action observations remain authoritative after newer replies.

Real Cloud HTTP parsing, Campaign, Journal and closed cleanup are exercised.
Only provider wire replies, processes/clocks and a stop-receipt filesystem fault
are synthetic; no parser, authorization or proof method is replaced.
"""
import contextlib
import copy
import json
import os
from pathlib import Path
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


def observed(f):
    journal = cl.Journal(f.journal.path, observe=True)
    try:
        return journal.peek()
    finally:
        journal.close()


class ObservationWire(MemoryProvider):
    """Apply synthetic provider operations only after stdlib emits their bytes."""
    def __init__(self, f):
        super().__init__()
        self.f, self.backend = f, f.cloud
        self.ack_path, self.ack_status, self.ack_location = None, None, 'primary'
        self.list_status, self.list_location, self.action_rows = None, 'primary', None
        self.observed_ids, self.ack_payloads, self.action_pages = [], [], []
        self.before_action = self.before_get = self.after_body = None
        self.paged_actions, self.page2_fault = False, None
        f.campaign.cloud = cc.Cloud(self.backend.profile, f.header['context'],
            connection_factory=self.connection, deadline=contextlib.nullcontext)

    def response_for(self, request, seconds):
        method, path = request['method'], request['path'].removeprefix('/v1')
        if method != 'GET':
            body = json.loads(request['body']) if request['body'] else None
            reply = self.backend.request(method, path, body, seconds)
            payload = copy.deepcopy(reply['payload'])
            if self.ack_status is not None and path == self.ack_path:
                if self.ack_location == 'next':
                    sibling = self.backend.new_action('server', payload['server']['id'], 'start_server')
                    payload['next_actions'].append(sibling)
                    action = payload['next_actions'][-1]
                elif self.ack_location == 'actions':
                    action = payload['actions'][0]
                else:
                    action = payload['action']
                action['status'] = self.ack_status
                self.observed_ids.append(action['id'])
                self.ack_payloads.append(copy.deepcopy(payload))
            return self.json(payload, reply['status']) if payload is not None else {'status': reply['status'], 'raw': b''}
        if self.before_get is not None:
            self.before_get(path)
        route = path.partition('?')[0].strip('/').split('/')
        if route[0] == 'actions':
            rid = int(route[1])
            if self.before_action is not None:
                self.before_action(rid)
            return self.json({'action': self.backend.action(rid, seconds)})
        kind = next(kind for kind, plural in cc.PLURALS.items() if plural == route[0])
        if len(route) == 1:
            rows = self.backend.list(kind, seconds)
            return self.json({route[0]: rows, 'meta': {'pagination': {'next_page': None, 'total_entries': len(rows)}}})
        rid = int(route[1])
        if len(route) == 2:
            item = self.backend.describe(kind, rid, seconds)
            return self.json({kind.replace('-', '_'): item}, 200 if item is not None else 404)
        if route[2:] != ['actions']:
            raise AssertionError('unexpected synthetic provider route ' + path)
        rows = (self.backend.resource_actions(kind, rid, seconds) if self.action_rows is None
                else copy.deepcopy(self.action_rows))
        if self.paged_actions:
            rows.sort(key=lambda action: (action['command'] == 'create_server') != (self.list_location == 'primary'))
        total = len(rows)
        page = int(dict(field.split('=') for field in path.partition('?')[2].split('&'))['page'])
        if self.paged_actions and page == 2 and self.page2_fault == 'provider':
            return self.json({'error': {'message': 'synthetic second action page unavailable'}}, 500)
        if self.paged_actions:
            rows = rows[page - 1:page]
        selected = [action for action in rows if (action['command'] == 'create_server')
                    == (self.list_location == 'primary')]
        if self.list_status is not None and selected:
            selected[0]['status'] = self.list_status
            self.observed_ids.append(selected[0]['id'])
        if self.list_status is not None or self.paged_actions:
            self.action_pages.append(copy.deepcopy(rows))
        return self.json({'actions': rows, 'meta': {'pagination': {
            'next_page': page + 1 if self.paged_actions and page < total else None, 'total_entries': total}}})

    def connection(self, host, seconds):
        self.replies.append({})
        connection = super().connection(host, seconds)
        socket = self.sockets[-1]
        makefile = socket.makefile
        def response(mode):
            socket.reply = self.response_for(self.requests[-1], seconds)
            return makefile(mode)
        socket.makefile = response
        getresponse = connection.getresponse
        def incoming():
            result = getresponse()
            read = result.read
            def body(size):
                raw = read(size)
                if self.after_body is not None:
                    self.after_body(self.requests[-1])
                return raw
            result.read = body
            return result
        connection.getresponse = incoming
        return connection


class ActionObservationsTest(unittest.TestCase):
    def setUp(self):
        self.stack = contextlib.ExitStack()
        self.addCleanup(self.stack.close)
        self.stack.enter_context(patch.dict(os.environ, {}, clear=True))
        self.stack.enter_context(patch.object(cc.http.client.HTTPSConnection, 'connect',
            side_effect=AssertionError('real network forbidden')))
        import campaign_safety as cs
        self.stack.enter_context(patch.object(cs.os, 'pidfd_open', create=True,
            side_effect=AssertionError('real pidfd forbidden')))
        self.stack.enter_context(patch.object(cs.signal, 'pidfd_send_signal', create=True,
            side_effect=AssertionError('real signal forbidden')))
        self.stack.enter_context(patch.object(cs.os, 'kill', side_effect=AssertionError('bare PID signalling forbidden')))

    def assert_failed_proof(self, f, wire, sequence, recovery=False):
        state = observed(f)
        intent = state['intents'][sequence]
        self.assertIs(intent.get('operation_failed'), True)
        self.assertTrue(wire.observed_ids)
        self.assertTrue(set(wire.observed_ids) <= set(intent['actions']))
        if not recovery:
            self.assertIs(intent['required_action_identified'], True)
        self.assertIsNotNone(intent['resource_id'])
        self.assertTrue(any(row['kind'] == intent['kind'] and row['id'] == intent['resource_id']
                            for row in state['owned']), 'validated resource provenance must survive the action error')
        if recovery:
            self.assertIsNone(intent['ack'])
            self.assertIsNone(intent['original_ack'])
            self.assertEqual(intent['recovery']['origin'], 'conditional-nonce-recovery')
            self.assertEqual(intent['recovery']['candidate']['id'], intent['resource_id'])
        else:
            self.assertIs(intent['acknowledgement_validated'], True)
            self.assertEqual(intent['ack']['payload'], wire.ack_payloads[-1])
        return copy.deepcopy(intent)

    @contextlib.contextmanager
    def stop_publication(self, f, wire, sequence, fail, recovery=False, observer=None):
        caught, replace = [], os.replace
        def publication(source, destination, *args, **kwargs):
            if observer is not None and Path(destination) == f.guard_directory / 'stop.json':
                observer()
            if fail and Path(destination) == f.guard_directory / 'stop.json':
                caught.append(self.assert_failed_proof(f, wire, sequence, recovery))
                raise OSError('synthetic stop publication failed after durable action observation')
            return replace(source, destination, *args, **kwargs)
        with patch.object(os, 'replace', side_effect=publication):
            yield caught
        if fail:
            self.assertTrue(caught, 'the actual stop publication must reach the filesystem fault')

    def invoke(self, call, fail):
        if fail:
            with self.assertRaisesRegex(OSError, '^synthetic stop publication failed after durable action observation$'):
                call()
        else:
            call()

    def successful_gets_cannot_erase_error_and_closed_cleanup_continues(self, f, wire, sequence, recovery=False,
                                                                      open_provisioning=True):
        before = self.assert_failed_proof(f, wire, sequence, recovery)
        wire.ack_status = wire.list_status = None
        wire.after_body = None
        wire.page2_fault = None
        checks = []
        def before_read(rid):
            self.assert_failed_proof(f, wire, sequence, recovery)
            checks.append(rid)
        wire.before_action = before_read
        wire.before_get = lambda path: self.assert_failed_proof(f, wire, sequence, recovery)
        if recovery and before['required_action_identified'] is not True:
            # Stop-publication failure may interrupt action correlation after
            # its separate identity/error commit. Resolve it with real reads.
            f.campaign.recover()
            self.assertIs(observed(f)['intents'][sequence]['required_action_identified'], True)
        self.assertTrue(all(f.cloud.actions[rid]['status'] == 'success' for rid in before['actions']),
                        'later action GET success must be independent of the older error-bearing reply')
        self.assertFalse(f.campaign.poll_actions())
        refreshed = self.assert_failed_proof(f, wire, sequence, recovery)
        self.assertEqual(refreshed['state'], 'FAILED')
        self.assertEqual(refreshed['pending_actions'], [])
        self.assertEqual(refreshed['ack'], before['ack'])
        self.assertTrue(set(refreshed['actions']) <= set(checks), 'all additive known actions must actually be refreshed through Cloud')
        mutations = copy.deepcopy(f.cloud.requests)
        if open_provisioning:
            self.assertEqual(observed(f)['mode'], 'OPEN')
            next_kind = 'ssh-key' if before['kind'] == 'network' else 'network'
            with self.assertRaisesRegex(mc.GuardError, 'cleanup requested'):
                f.campaign.create(next_kind, f.body(next_kind))
            self.assertEqual(f.cloud.requests, mutations, 'observed failure must refuse all later OPEN provisioning sends')
            f.closing()
        else:
            self.assertIn(observed(f)['mode'], {'CLOSING', 'INCIDENT'})
            self.assertEqual(f.cloud.requests, mutations)
        result = f.campaign.cleanup_pass()
        self.assertTrue(result['complete'])
        self.assertTrue(result['baseline_equal'])
        self.assertTrue(result['hard_lifetime_met'])
        final = observed(f)['intents'][sequence]
        self.assertEqual(final['state'], 'FAILED')
        self.assertIs(final['operation_failed'], True)
        self.assertEqual(final['ack'], before['ack'])
        for kind, ids in f.header['baseline'].items():
            self.assertEqual(sorted(f.cloud.rows[kind]), ids)
        self.assertTrue(f.deletes(), 'safe closed cleanup must still dispatch independently authorized DELETEs')
        self.assertTrue(all(socket.closed for socket in wire.sockets))

    def test_server_create_primary_and_next_action_ack_errors_are_durable_before_stop_and_later_get(self):
        for location in ('primary', 'next'):
            for fail in (False, True):
                with self.subTest(location=location, stop_failure=fail), Fixture() as f:
                    f.dependencies()
                    self.assertTrue(f.campaign.poll_actions())
                    sequence = len(f.journal.read()['intents'])
                    wire = ObservationWire(f)
                    wire.ack_path, wire.ack_status, wire.ack_location = '/servers', 'error', location
                    with self.stop_publication(f, wire, sequence, fail):
                        self.invoke(lambda: f.create('server'), fail)
                    self.assertEqual(wire.posts, 1)
                    self.assertEqual(wire.requests[0]['path'], '/v1/servers?page=1&per_page=50')
                    self.successful_gets_cannot_erase_error_and_closed_cleanup_continues(f, wire, sequence)

    def test_firewall_actions_array_ack_error_and_running_are_durable_before_stop_or_get(self):
        for status, fail in (('error', False), ('error', True), ('running', False)):
            with self.subTest(status=status, stop_failure=fail), Fixture() as f:
                self.assertEqual(f.create('ssh-key'), 401)
                self.assertTrue(f.campaign.poll_actions())
                sequence = len(f.journal.read()['intents'])
                wire = ObservationWire(f)
                wire.ack_path, wire.ack_status, wire.ack_location = '/firewalls', status, 'actions'
                stop = f.guard_directory / 'stop.json'
                self.assertFalse(stop.exists())
                stops, reads = [], []

                def durable_ack(leased=False):
                    # A fresh authenticated disk observation is safe even while
                    # create owns the writer lock; no in-memory state is reused.
                    state = observed(f)
                    intent = state['intents'][sequence]
                    self.assertEqual(state['mode'], 'OPEN')
                    self.assertEqual((intent['kind'], intent['name'], intent['state']),
                                     ('firewall', 'test-firewall', 'ACKNOWLEDGED'))
                    self.assertIs(type(intent['resource_id']), int)
                    self.assertEqual(intent['resource_id'], 301)
                    self.assertEqual(intent['actions'], [1001])
                    self.assertEqual(intent.get('pending_actions', []), [1001] if status == 'running' else [])
                    if status == 'error':
                        self.assertIs(intent.get('operation_failed'), True)
                    else:
                        self.assertIsNot(intent.get('operation_failed'), True)
                    self.assertIs(intent['acknowledgement_validated'], True)
                    self.assertIs(intent['required_action_identified'], True)
                    self.assertEqual(intent['ack']['status'], 201)
                    payload = intent['ack']['payload']
                    self.assertEqual(payload, wire.ack_payloads[-1])
                    self.assertNotIn('action', payload)
                    self.assertNotIn('next_actions', payload)
                    self.assertEqual(payload['actions'], [dict(id=1001, command='create_firewall', status=status,
                        resources=[dict(type='firewall', id=301)])])
                    self.assertIs(type(payload['actions'][0]['id']), int)
                    self.assertIs(type(payload['actions'][0]['resources'][0]['id']), int)
                    item = payload['firewall']
                    self.assertIs(type(item['id']), int)
                    self.assertEqual((item['id'], item['name'], item['rules']), (301, 'test-firewall', []))
                    labels = dict(campaign='synthetic', deadline='1700054000', role='firewall', intent=intent['nonce'])
                    self.assertEqual(item['labels'], labels)
                    self.assertEqual(intent['descriptor']['rules'], [])
                    self.assertEqual(intent['descriptor']['labels'], labels)
                    self.assertNotIn(301, f.header['baseline']['firewall'])
                    self.assertNotIn(301, intent['pre_ids']['firewall'])
                    self.assertEqual([row for row in state['owned'] if row['kind'] == 'firewall'], [dict(
                        kind='firewall', id=301, name='test-firewall', labels=labels, created=item['created'],
                        canonical={'rules': []}, intent=sequence, implicit_ips=[])])
                    self.assertEqual(state['intents'][0]['state'], 'DONE')
                    self.assertTrue(any(row['kind'] == 'ssh-key' and row['id'] == 401 for row in state['owned']))
                    if leased:
                        self.assertEqual(state['active_lease']['intent'], sequence)
                    else:
                        self.assertIsNone(state['active_lease'])
                    self.assertEqual(f.cloud.action_reads, [], 'the ACK must be durable before the first action GET')
                    self.assertEqual(f.cloud.actions[1001], dict(id=1001, command='create_firewall', status='success',
                        resources=[dict(type='firewall', id=301)]), 'only the response ACK may carry the injected status')
                    return intent

                def before_stop():
                    self.assertFalse(stop.exists())
                    stops.append(copy.deepcopy(durable_ack(leased=True)))

                with self.stop_publication(f, wire, sequence, fail, observer=before_stop):
                    self.invoke(lambda: f.create('firewall'), fail)
                self.assertEqual(len(stops), 1 if status == 'error' else 0)
                self.assertEqual(stop.exists(), status == 'error' and not fail)
                self.assertEqual(wire.posts, 1)
                post = next(request for request in wire.requests if request['method'] == 'POST')
                self.assertEqual(post['path'], '/v1/firewalls')
                outgoing = json.loads(post['body'])
                self.assertEqual((outgoing['name'], outgoing['rules']), ('test-firewall', []))
                before = copy.deepcopy(durable_ack())
                # The completed call also admits a strict read with the writer
                # lock released; both boundaries must see the same committed ACK.
                self.assertEqual(f.journal.read()['intents'][sequence], before)

                def before_get(path):
                    self.assertEqual(path, '/actions/1001')
                    reads.append(copy.deepcopy(durable_ack()))
                wire.before_get = before_get
                self.assertEqual(f.campaign.poll_actions(), status == 'running')
                wire.before_get = None
                self.assertEqual(len(reads), 1)
                self.assertEqual(f.cloud.action_reads, [1001])
                terminal = f.journal.read()['intents'][sequence]
                self.assertEqual(terminal['state'], 'FAILED' if status == 'error' else 'DONE')
                self.assertEqual(terminal['pending_actions'], [])
                self.assertEqual(terminal['ack'], before['ack'])
                self.assertEqual(terminal['actions'], [1001])
                self.assertIs(terminal.get('operation_failed'), True if status == 'error' else None)
                if status == 'error':
                    mutations = copy.deepcopy(f.cloud.requests)
                    sent = copy.deepcopy(wire.requests)
                    self.assertEqual(f.journal.read()['mode'], 'OPEN')
                    with self.assertRaisesRegex(mc.GuardError, 'cleanup requested'):
                        f.create('network')
                    self.assertEqual(f.cloud.requests, mutations)
                    self.assertEqual(wire.requests, sent, 'later OPEN provisioning must not send another POST')
                else:
                    self.assertFalse(stop.exists())
                    self.assertEqual(list(f.guard_directory.glob('incident-*.json')), [])
                f.closing()
                result = f.campaign.cleanup_pass()
                self.assertTrue(result['complete'])
                self.assertTrue(result['baseline_equal'])
                self.assertTrue(result['hard_lifetime_met'])
                self.assertIn(('DELETE', '/ssh_keys/401'), f.deletes(),
                              'unrelated healthy owned cleanup must remain independently authorized')
                self.assertIn(('DELETE', '/firewalls/301'), f.deletes())
                final = f.journal.read()['intents'][sequence]
                self.assertEqual(final['state'], terminal['state'])
                self.assertEqual(final['ack'], before['ack'])
                self.assertIs(final.get('operation_failed'), True if status == 'error' else None)
                for kind, ids in f.header['baseline'].items():
                    self.assertEqual(sorted(f.cloud.rows[kind]), ids)
                self.assertEqual(wire.posts, 1)
                self.assertTrue(all(socket.closed for socket in wire.sockets))

    def test_mutation_ack_errors_are_durable_before_stop_and_later_success(self):
        for operation in ('subnet', 'poweron'):
            for fail in (False, True):
                with self.subTest(operation=operation, stop_failure=fail), Fixture() as f:
                    rid = f.create('network' if operation == 'subnet' else 'server')
                    self.assertTrue(f.campaign.poll_actions())
                    sequence = len(f.journal.read()['intents'])
                    kind = 'network' if operation == 'subnet' else 'server'
                    body = copy.deepcopy(f.header['subnet']) if operation == 'subnet' else {}
                    path = f'/networks/{rid}/actions/add_subnet' if operation == 'subnet' else f'/servers/{rid}/actions/poweron'
                    wire = ObservationWire(f)
                    wire.ack_path, wire.ack_status = path, 'error'
                    with self.stop_publication(f, wire, sequence, fail):
                        self.invoke(lambda: f.campaign.mutate(kind, rid, operation, body), fail)
                    self.assertEqual(wire.posts, 1)
                    self.successful_gets_cannot_erase_error_and_closed_cleanup_continues(f, wire, sequence)

    def recovery_setup(self, f, branch):
        f.dependencies()
        self.assertTrue(f.campaign.poll_actions())
        sequence = len(f.journal.read()['intents'])
        wire = ObservationWire(f)
        f.cloud.lose_ack = True
        with self.assertRaisesRegex(mc.GuardError, 'failed or ambiguous'):
            f.create('server')
        f.cloud.lose_ack = False
        if branch == 'known-id':
            wire.action_rows = []
            f.campaign.recover()
            prior = observed(f)['intents'][sequence]
            self.assertEqual((prior['resource_id'], prior['state']), (101, 'UNKNOWN'))
            self.assertEqual(prior['actions'], [])
        wire.action_rows = None
        f.cloud.new_action('server', 101, 'start_server')
        return wire, sequence

    def test_recovery_primary_and_additive_sibling_list_errors_survive_later_get_success(self):
        for branch in ('unknown-id', 'known-id'):
            for location in ('primary', 'sibling'):
                for fail in (False, True):
                    with self.subTest(branch=branch, location=location, stop_failure=fail), Fixture() as f:
                        wire, sequence = self.recovery_setup(f, branch)
                        wire.list_status, wire.list_location = 'error', location
                        with self.stop_publication(f, wire, sequence, fail, recovery=True):
                            self.invoke(f.campaign.recover, fail)
                        self.assertEqual(len(wire.action_pages), 1)
                        self.assertEqual(len(wire.action_pages[0]), 2)
                        self.assertEqual(set(observed(f)['intents'][sequence]['actions']),
                                         {action['id'] for action in wire.action_pages[0]})
                        self.successful_gets_cannot_erase_error_and_closed_cleanup_continues(
                            f, wire, sequence, recovery=True)

    def test_first_recovery_page_error_is_durable_before_later_page_failure_or_cutoff(self):
        for location in ('primary', 'sibling'):
            for failure in ('provider', 'cutoff'):
                with self.subTest(location=location, failure=failure), Fixture() as f:
                    wire, sequence = self.recovery_setup(f, 'unknown-id')
                    wire.list_status, wire.list_location = 'error', location
                    wire.paged_actions = True
                    wire.page2_fault = failure
                    if failure == 'cutoff':
                        f.wall = f.header['time']['setup_ns'] - 7 * NS
                    first_pages, second_reads, stops = [], [], []
                    def received(request):
                        if request['path'] == '/v1/servers/101/actions?page=1&per_page=50':
                            first_pages.append(copy.deepcopy(request))
                            if failure == 'cutoff':
                                f.wall += 5 * NS
                                f.mono += 5 * NS
                    def before_get(path):
                        if path == '/servers/101/actions?page=2&per_page=50':
                            self.assert_failed_proof(f, wire, sequence, recovery=True)
                            second_reads.append(path)
                    replace = os.replace
                    def publication(source, destination, *args, **kwargs):
                        if Path(destination) == f.guard_directory / 'stop.json':
                            stops.append(self.assert_failed_proof(f, wire, sequence, recovery=True))
                        return replace(source, destination, *args, **kwargs)
                    wire.after_body, wire.before_get = received, before_get
                    start = len(wire.requests)
                    with patch.object(os, 'replace', side_effect=publication):
                        if failure == 'cutoff':
                            with self.assertRaisesRegex(mc.GuardError, '^provisioning budget exhausted$'):
                                f.campaign.recover()
                        else:
                            f.campaign.recover()
                    self.assertEqual(len(first_pages), 1)
                    self.assertTrue(stops, 'first-page action error must be committed before stop and next-page work')
                    self.assert_failed_proof(f, wire, sequence, recovery=True)
                    dispatched = wire.requests[start:]
                    page2 = [request for request in dispatched if '/actions?page=2' in request['path']]
                    if failure == 'cutoff':
                        self.assertEqual(page2, [])
                        self.assertEqual(second_reads, [])
                        self.assertFalse(any(event.get('event') == 'recovery-action-read-unavailable'
                                             for event in observed(f)['events']))
                    else:
                        self.assertEqual(len(page2), 1)
                        self.assertEqual(len(second_reads), 1)
                        self.assertTrue(any(event.get('event') == 'recovery-action-read-unavailable'
                                            for event in observed(f)['events']))
                    if failure == 'cutoff':
                        # The provisioning cutoff is never extended or rolled
                        # back. Only real closed-admission cleanup may continue.
                        at_cutoff = f.wall
                        f.closing()
                        self.assertEqual(f.wall, at_cutoff)
                        self.assertEqual(observed(f)['mode'], 'CLOSING')
                    self.successful_gets_cannot_erase_error_and_closed_cleanup_continues(
                        f, wire, sequence, recovery=True, open_provisioning=failure != 'cutoff')

    def test_healthy_two_page_recovery_adds_both_action_ids_without_error_or_open_stop(self):
        for branch in ('unknown-id', 'known-id'):
            with self.subTest(branch=branch), Fixture() as f:
                wire, sequence = self.recovery_setup(f, branch)
                initial = observed(f)
                self.assertEqual(initial['mode'], 'OPEN')
                self.assertEqual(initial['intents'][sequence]['actions'], [])
                self.assertIsNot(initial['intents'][sequence].get('operation_failed'), True)
                # Preserve the lost-response setup's existing policy artifact.
                # Count publications and retain its inode/mode as well as bytes:
                # a fixed clock could otherwise hide an identical rewrite.
                stop = f.guard_directory / 'stop.json'
                self.assertTrue(stop.exists())
                stop_bytes, stop_info = cc.private_bytes(stop), stop.lstat()
                wire.paged_actions = True
                wire.action_pages.clear()
                start = len(wire.requests)
                first_page_checks, stops = [], []
                def before_get(path):
                    if path == '/servers/101/actions?page=2&per_page=50':
                        prior = observed(f)['intents'][sequence]
                        self.assertEqual(prior['actions'], [1002], 'first-page new ID must be durable before the next GET')
                        self.assertEqual(prior['resource_id'], 101)
                        self.assertIsNone(prior['ack'])
                        self.assertIsNone(prior['original_ack'])
                        self.assertEqual(prior['recovery']['origin'], 'conditional-nonce-recovery')
                        self.assertIs(prior['required_action_identified'], False)
                        self.assertIsNot(prior.get('operation_failed'), True)
                        self.assertEqual(prior.get('pending_actions', []), [])
                        first_page_checks.append(copy.deepcopy(prior))
                replace = os.replace
                def publication(source, destination, *args, **kwargs):
                    if Path(destination) == stop:
                        stops.append(Path(destination))
                    return replace(source, destination, *args, **kwargs)
                wire.before_get = before_get
                with patch.object(os, 'replace', side_effect=publication):
                    f.campaign.recover()
                    self.assertEqual(len(first_page_checks), 1)
                    pages = [request for request in wire.requests[start:] if '/actions?page=' in request['path']]
                    self.assertEqual([request['path'] for request in pages], [
                        '/v1/servers/101/actions?page=1&per_page=50', '/v1/servers/101/actions?page=2&per_page=50'])
                    self.assertTrue(all(request['method'] == 'GET' for request in pages))
                    self.assertEqual([len(page) for page in wire.action_pages], [1, 1])
                    self.assertEqual([action['id'] for page in wire.action_pages for action in page], [1002, 1003])
                    self.assertTrue(all(action['status'] == 'success' for page in wire.action_pages for action in page))
                    intent = observed(f)['intents'][sequence]
                    self.assertEqual((intent['resource_id'], intent['state']), (101, 'ACKNOWLEDGED'))
                    self.assertEqual(intent['actions'], [1002, 1003])
                    self.assertIs(intent['required_action_identified'], True)
                    self.assertIsNone(intent['ack'])
                    self.assertIsNone(intent['original_ack'])
                    self.assertIsNot(intent.get('operation_failed'), True)
                    self.assertEqual(intent.get('pending_actions', []), [])
                    self.assertEqual(intent['recovery']['candidate']['id'], 101)
                    self.assertEqual(wire.action_pages[0][0]['command'], 'create_server')
                    if branch == 'unknown-id':
                        self.assertEqual(intent['recovery']['corroborating_create_action']['id'], 1002)
                    before_poll = len(wire.requests)
                    self.assertTrue(f.campaign.poll_actions())
                    self.assertEqual([request['path'] for request in wire.requests[before_poll:]],
                        ['/v1/actions/1001', '/v1/actions/1002', '/v1/actions/1003'])
                    self.assertEqual(stops, [], 'healthy OPEN ACK/list/GET observations must not issue another stop')
                    self.assertEqual(cc.private_bytes(stop), stop_bytes)
                    current_stop = stop.lstat()
                    self.assertEqual((current_stop.st_dev, current_stop.st_ino, current_stop.st_mode, current_stop.st_uid),
                                     (stop_info.st_dev, stop_info.st_ino, stop_info.st_mode, stop_info.st_uid))
                wire.before_get = None
                current = observed(f)
                self.assertTrue(all(intent['state'] == 'DONE' and intent['pending_actions'] == []
                                    and intent.get('operation_failed') is not True for intent in current['intents']))
                self.assertEqual(list(f.guard_directory.glob('incident-*.json')), [])
                f.closing()
                result = f.campaign.cleanup_pass()
                self.assertTrue(result['complete'])
                self.assertTrue(result['baseline_equal'])
                self.assertTrue(result['hard_lifetime_met'])
                self.assertTrue(all(intent.get('operation_failed') is not True for intent in observed(f)['intents']))
                self.assertEqual(list(f.guard_directory.glob('incident-*.json')), [])
                self.assertTrue(all(socket.closed for socket in wire.sockets))

    def test_valid_running_observations_remain_pending_without_becoming_sticky_failures(self):
        for source in ('create', 'mutate', 'recovery'):
            with self.subTest(source=source), Fixture() as f:
                if source == 'recovery':
                    wire, sequence = self.recovery_setup(f, 'unknown-id')
                    wire.list_status = 'running'
                    f.campaign.recover()
                elif source == 'create':
                    f.dependencies()
                    self.assertTrue(f.campaign.poll_actions())
                    sequence = len(f.journal.read()['intents'])
                    wire = ObservationWire(f)
                    wire.ack_path, wire.ack_status = '/servers', 'running'
                    f.create('server')
                else:
                    network = f.create()
                    self.assertTrue(f.campaign.poll_actions())
                    sequence = len(f.journal.read()['intents'])
                    wire = ObservationWire(f)
                    wire.ack_path, wire.ack_status = f'/networks/{network}/actions/add_subnet', 'running'
                    f.campaign.mutate('network', network, 'subnet', copy.deepcopy(f.header['subnet']))
                state = observed(f)['intents'][sequence]
                self.assertTrue(wire.observed_ids)
                self.assertTrue(set(wire.observed_ids) <= set(state['pending_actions']))
                self.assertIsNot(state.get('operation_failed'), True)
                self.assertEqual(state['state'], 'ACKNOWLEDGED')
                wire.ack_status = wire.list_status = None
                self.assertTrue(f.campaign.poll_actions())
                terminal = observed(f)['intents'][sequence]
                self.assertEqual((terminal['state'], terminal['pending_actions']), ('DONE', []))
                self.assertIsNot(terminal.get('operation_failed'), True)
                f.closing()
                self.assertTrue(f.campaign.cleanup_pass()['complete'])
                self.assertTrue(all(socket.closed for socket in wire.sockets))


if __name__ == '__main__':
    unittest.main()
