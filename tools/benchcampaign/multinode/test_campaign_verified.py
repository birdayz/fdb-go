"""Caller-level controls for the verified RFC-283 implementation findings."""
import contextlib
import copy
import io
import json
import os
from pathlib import Path
import signal
import stat
import tempfile
import unittest
from unittest.mock import patch

import campaign_cloud as cc
import campaign_safety as cs
import creation_ledger as cl
import mn_common as mc
import provision
import teardown_guard as tg
from test_campaign_cloud import MemoryProvider, config_text
from test_campaign_safety import Fixture, M0, NS, T0
from test_guarded_cli import constructors, fixture as cli_fixture


class StopLoop(BaseException):
    pass


class VerifiedCampaignTests(unittest.TestCase):
    def setUp(self):
        self.stack = contextlib.ExitStack()
        self.addCleanup(self.stack.close)
        self.stack.enter_context(patch.dict(os.environ, {}, clear=True))
        self.stack.enter_context(patch.object(os, 'pidfd_open', side_effect=AssertionError('real pidfd forbidden')))
        self.stack.enter_context(patch.object(signal, 'pidfd_send_signal', side_effect=AssertionError('real signal forbidden')))
        self.stack.enter_context(patch.object(cc.http.client.HTTPSConnection, 'connect',
                                             side_effect=AssertionError('real network forbidden')))

    def operation(self, f, operation):
        if operation == 'subnet':
            rid = f.create()
            target = ('network', rid, operation, copy.deepcopy(f.header['subnet']))
        elif operation == 'attach':
            network = f.create()
            server = f.create('server')
            target = ('server', server, operation, dict(network=network, ip=f.header['plan'][3]['private_ip']))
        else:
            server = f.create('server')
            target = ('server', server, operation, {} if operation == 'poweron' else None)
        self.assertTrue(f.campaign.poll_actions())
        if operation == 'delete':
            f.closing()
        return target

    def test_sending_fsync_delay_cannot_restart_an_expired_physical_or_phase_lease(self):
        for operation in ('create', 'subnet'):
            for boundary in ('physical', 'phase'):
                with self.subTest(operation=operation, boundary=boundary), Fixture() as f:
                    target = None if operation == 'create' else self.operation(f, 'subnet')
                    before = len(f.cloud.requests)
                    sequence = len(f.journal.read()['intents'])
                    if boundary == 'phase':
                        f.wall = f.header['time']['c_ns' if operation == 'create' else 'setup_ns'] - 3 * NS
                        f.heartbeat()
                    real_sync = os.fsync
                    delayed = []
                    def sync(fd):
                        real_sync(fd)
                        if delayed or not stat.S_ISDIR(os.fstat(fd).st_mode):
                            return
                        head = json.loads(f.journal.path.read_bytes())['state']
                        if len(head['intents']) > sequence and head['intents'][sequence]['state'] == 'SENDING':
                            delayed.append(copy.deepcopy(head['active_lease']))
                            elapsed = 21 if boundary == 'physical' else 4
                            f.wall += elapsed * NS
                            f.mono += elapsed * NS
                            f.heartbeat()  # Keep independent liveness prerequisites valid.
                    with patch.object(cl.os, 'fsync', side_effect=sync):
                        with self.assertRaisesRegex(mc.GuardError, 'failed or ambiguous'):
                            f.create() if target is None else f.campaign.mutate(*target)
                    self.assertEqual(len(delayed), 1)
                    self.assertEqual(len(f.cloud.requests), before)
                    state = f.journal.read()
                    self.assertEqual(state['intents'][sequence]['state'], 'UNKNOWN')
                    self.assertIsNone(state['active_lease'])
                    self.assertTrue((f.guard_directory / 'stop.json').exists())
                    self.assertLessEqual(delayed[0]['monotonic_deadline_ns'], f.mono)

    def test_post_sending_pin_drift_is_unknown_without_any_transport_dispatch(self):
        for operation in ('create', 'subnet', 'delete'):
            for changed in ('source', 'input'):
                with self.subTest(operation=operation, changed=changed), Fixture() as f:
                    target = None if operation == 'create' else self.operation(f, operation)
                    before = len(f.cloud.requests)
                    sequence = len(f.journal.read()['intents'])
                    sync, drifted = os.fsync, []
                    def publish(fd):
                        sync(fd)
                        if drifted or not stat.S_ISDIR(os.fstat(fd).st_mode):
                            return
                        head = json.loads(f.journal.path.read_bytes())['state']
                        if len(head['intents']) > sequence and head['intents'][sequence]['state'] == 'SENDING':
                            self.assertEqual(head['active_lease']['intent'], sequence)
                            path = f.source / 'provision.py' if changed == 'source' else f.userdata
                            drifted.append(path)
                            path.write_bytes(path.read_bytes() + b'# changed after durable SENDING\n')
                    with patch.object(cl.os, 'fsync', side_effect=publish):
                        with self.assertRaisesRegex(mc.GuardError, 'failed or ambiguous'):
                            f.create() if target is None else f.campaign.mutate(*target)
                    self.assertEqual(len(drifted), 1)
                    self.assertEqual(len(f.cloud.requests), before)
                    state = f.journal.read()
                    self.assertEqual(state['intents'][sequence]['state'], 'UNKNOWN')
                    self.assertIsNone(state['intents'][sequence]['ack'])
                    self.assertIsNone(state['active_lease'])
                    if operation == 'delete':
                        self.assertEqual(state['mode'], 'INCIDENT')
                    else:
                        self.assertTrue((f.guard_directory / 'stop.json').exists())

    def test_transport_deadline_covers_real_profile_read_before_any_wire_dispatch(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / 'cli.toml'
            path.write_text(config_text())
            path.chmod(0o600)
            profile = cc.Profile(config=path, environ={})
            provider = MemoryProvider(MemoryProvider.json({'server': {'id': 71}}, 201))
            now, entered = [100 * NS], []
            @contextlib.contextmanager
            def timer(seconds):
                entered.append(seconds)
                yield
            client = cc.Cloud(profile, profile.binding, connection_factory=provider.connection, deadline=timer)
            private = cc.private_bytes
            def delayed(candidate):
                raw = private(candidate)
                self.assertEqual(len(entered), 1, 'profile read preceded the whole-call timer')
                now[0] += 21 * NS
                return raw
            budget = cc.CallBudget(20, 120 * NS, lambda: now[0])
            with patch.object(cc, 'private_bytes', side_effect=delayed):
                with self.assertRaisesRegex(mc.GuardError, 'absolute cloud lease expired'):
                    client.request('GET', '/servers?page=1&per_page=100', seconds=budget)
            self.assertEqual(entered, [20])
            self.assertEqual(provider.connections, [])
            self.assertEqual(provider.requests, [])

    def test_transport_rechecks_physical_and_phase_deadlines_after_connect(self):
        for expiry in ('physical-with-wall-rollback', 'phase'):
            with self.subTest(expiry=expiry), tempfile.TemporaryDirectory() as directory:
                path = Path(directory) / 'cli.toml'
                path.write_text(config_text())
                path.chmod(0o600)
                profile = cc.Profile(config=path, environ={})
                provider = MemoryProvider(MemoryProvider.json({'servers': []}))
                mono, phase, connected = [100 * NS], [1000 * NS], []
                def factory(host, seconds):
                    connection = provider.connection(host, seconds)
                    connect = connection.connect
                    def delayed():
                        connect()
                        connected.append(True)
                        if expiry == 'physical-with-wall-rollback':
                            mono[0] += 21 * NS
                            phase[0] -= 100 * NS
                        else:
                            phase[0] += 21 * NS
                    connection.connect = delayed
                    return connection
                budget = cc.CallBudget(20, 120 * NS, lambda: mono[0], 1020 * NS, lambda: phase[0])
                client = cc.Cloud(profile, profile.binding, connection_factory=factory, deadline=contextlib.nullcontext)
                with self.assertRaisesRegex(mc.GuardError, 'absolute cloud lease expired'):
                    client.request('GET', '/servers?page=1&per_page=50', seconds=budget)
                self.assertEqual(connected, [True])
                self.assertEqual(provider.requests, [])
                self.assertTrue(provider.sockets[0].closed)

    def test_noncreate_final_admission_rechecks_after_each_authorization_readback(self):
        for operation in ('subnet', 'attach', 'poweron'):
            for change in ('stop', 'guard-death', 'source', 'input'):
                for read_kind in (('server', 'network') if operation == 'attach' else
                                  (('network',) if operation == 'subnet' else ('server',))):
                    with self.subTest(operation=operation, change=change, read_kind=read_kind), Fixture() as f:
                        target = self.operation(f, operation)
                        before = len(f.cloud.requests)
                        original = f.cloud.describe
                        reached = []
                        def describe(kind, rid, seconds=20):
                            item = original(kind, rid, seconds)
                            if kind == read_kind and not reached:
                                reached.append((kind, rid))
                                if change == 'stop':
                                    f.campaign.request_stop()
                                elif change == 'guard-death':
                                    f.processes.pop(112)
                                elif change == 'source':
                                    next(f.source.iterdir()).write_text('# changed during readback\n')
                                else:
                                    f.userdata.write_text('# changed during readback\n')
                            return item
                        with patch.object(f.cloud, 'describe', side_effect=describe):
                            with self.assertRaises(mc.GuardError):
                                f.campaign.mutate(*target)
                        self.assertEqual(len(reached), 1)
                        self.assertEqual(len(f.cloud.requests), before)
                        self.assertIsNone(f.journal.read()['active_lease'])
                        self.assertEqual(len(f.journal.read()['intents']), before)

    def test_async_mutation_requires_valid_endpoint_ack_and_retains_unknown(self):
        faults = ('malformed-json', 'json-null', 'empty-object', 'missing-action', 'boolean-action-id', 'wrong-status')
        for operation in ('subnet', 'attach', 'poweron', 'delete'):
            for fault in faults:
                with self.subTest(operation=operation, fault=fault), Fixture() as f:
                    target = self.operation(f, operation)
                    before = len(f.cloud.requests)
                    original = f.cloud.request
                    def request(method, path, body=None, seconds=20):
                        reply = original(method, path, body, seconds)
                        if fault == 'malformed-json':
                            reply.update(payload=None, json_valid=False, body_empty=False)
                        elif fault == 'json-null':
                            reply.update(payload=None, json_valid=True, body_empty=False)
                        elif fault == 'empty-object':
                            reply['payload'] = {}
                        elif fault == 'missing-action':
                            reply['payload'] = {'next_actions': []}
                        elif fault == 'boolean-action-id':
                            reply['payload']['action']['id'] = True
                        else:
                            reply['status'] = 204 if operation == 'delete' else 200
                        return reply
                    with patch.object(f.cloud, 'request', side_effect=request):
                        with self.assertRaisesRegex(mc.GuardError, 'failed or ambiguous'):
                            f.campaign.mutate(*target)
                    state = f.journal.read()
                    self.assertEqual(len(f.cloud.requests), before + 1)
                    self.assertEqual(state['intents'][-1]['state'], 'UNKNOWN')
                    expected_actions = ([state['intents'][-1]['ack']['payload']['action']['id']]
                                        if fault == 'wrong-status' else [])
                    self.assertEqual(state['intents'][-1]['actions'], expected_actions)
                    self.assertIsNone(state['active_lease'])
                    if operation == 'delete':
                        self.assertEqual(state['mode'], 'INCIDENT')
                    else:
                        self.assertTrue((f.guard_directory / 'stop.json').exists())
                        f.closing()
                    result = f.campaign.completion()
                    self.assertEqual(result['pending_intents'], [before])
                    self.assertFalse(result['complete'])

    def test_dispatched_noncreate_loss_and_error_ack_keep_phase_specific_failure_state(self):
        for operation in ('subnet', 'attach', 'poweron', 'delete'):
            for outcome in ('lost-after-commit', 'error-response'):
                with self.subTest(operation=operation, outcome=outcome), Fixture() as f:
                    target = self.operation(f, operation)
                    before = len(f.cloud.requests)
                    original = f.cloud.request
                    def request(method, path, body=None, seconds=20):
                        reply = original(method, path, body, seconds)
                        if outcome == 'lost-after-commit':
                            raise mc.GuardError('synthetic response lost after provider commit')
                        reply.update(status=503, payload={'error': {'code': 'unavailable'}},
                                     json_valid=True, body_empty=False)
                        return reply
                    with patch.object(f.cloud, 'request', side_effect=request):
                        with self.assertRaisesRegex(mc.GuardError, 'failed or ambiguous'):
                            f.campaign.mutate(*target)
                    state = f.journal.read()
                    self.assertEqual(len(f.cloud.requests), before + 1)
                    self.assertEqual(state['intents'][-1]['state'], 'UNKNOWN')
                    self.assertEqual(state['intents'][-1]['ack'] is None, outcome == 'lost-after-commit')
                    self.assertIsNone(state['active_lease'])
                    if operation == 'delete':
                        self.assertEqual(state['mode'], 'INCIDENT')
                    else:
                        self.assertTrue((f.guard_directory / 'stop.json').exists())
                        f.closing()
                    self.assertEqual(f.campaign.completion()['pending_intents'], [before])

    def test_delete_retry_reauthorizes_changed_current_resource_and_cascade(self):
        for drift in ('unchanged', 'name', 'nonce', 'created', 'auto-delete', 'foreign-ip'):
            with self.subTest(drift=drift), Fixture() as f:
                server = f.create('server')
                self.assertTrue(f.campaign.poll_actions())
                f.closing()
                attempts = []
                def locked(method, path, body, seconds):
                    attempts.append((method, path))
                    raise mc.GuardError('synthetic provider locked before deletion')
                f.cloud.before_request = locked
                with self.assertRaisesRegex(mc.GuardError, 'failed or ambiguous'):
                    f.campaign.mutate('server', server, 'delete')
                self.assertEqual(attempts, [('DELETE', '/servers/101')])
                self.assertEqual(f.journal.read()['intents'][-1]['state'], 'UNKNOWN')
                self.assertIn(server, f.cloud.rows['server'])
                f.cloud.before_request = None
                item = f.cloud.rows['server'][server]
                if drift == 'name': item['name'] = 'test-other'
                if drift == 'nonce': item['labels']['intent'] = 'other'
                if drift == 'created': item['created'] = '2099-01-01T00:00:00Z'
                if drift == 'auto-delete': f.cloud.rows['primary-ip'][10101]['auto_delete'] = False
                if drift == 'foreign-ip': item['public_net']['ipv6'] = {'id': 777}
                before = list(f.cloud.requests)
                read_before = len(f.cloud.describes)
                events_before = len(f.journal.read()['events'])
                result = f.campaign.cleanup_pass()
                self.assertFalse(result['complete'])  # The original ambiguous action remains unresolved.
                fresh = f.journal.read()
                sends = [(method, path) for method, path, _, _ in f.cloud.requests[len(before):]
                         if method == 'DELETE' and path == '/servers/101']
                self.assertEqual(sum(kind == 'server' and rid == server
                    for kind, rid, _ in f.cloud.describes[read_before:]),
                    2 if drift in {'name', 'nonce', 'created', 'auto-delete'} else 3,
                    'retired identity refuses at the preliminary read; every cleanup also exact-checks absence at completion')
                refusals = [e for e in fresh['events'][events_before:]
                            if e.get('event') == 'delete-refused-or-unknown' and e.get('kind') == 'server'
                            and e.get('id') == server]
                if drift == 'unchanged':
                    self.assertEqual(sends, [('DELETE', '/servers/101')])
                    self.assertNotIn(server, f.cloud.rows['server'])
                    self.assertEqual(refusals, [])
                else:
                    self.assertEqual(sends, [], 'retry reused stale server deletion authority')
                    self.assertIn(server, f.cloud.rows['server'])
                    self.assertEqual(len(refusals), 1)
                    expected = {'name': 'observed-retired ID', 'nonce': 'observed-retired ID',
                        'created': 'observed-retired ID', 'auto-delete': 'observed-retired IP',
                        'foreign-ip': 'unexpected or missing IP family'}[drift]
                    self.assertIn(expected, refusals[0]['refusal'])
                    if drift != 'foreign-ip':
                        kind, rid = ('primary-ip', 10101) if drift == 'auto-delete' else ('server', server)
                        self.assertTrue(any(e.get('event') == 'observed-retirement' and e.get('kind') == kind
                            and e.get('id') == rid and e.get('source') == 'recovery-census-identity'
                            for e in fresh['events'][events_before:]))
                self.assertTrue(any(i['operation'] == 'delete' and i['state'] == 'UNKNOWN'
                                    for i in fresh['intents']))

    def test_sibling_action_failure_does_not_clear_another_known_pending_action(self):
        with Fixture() as f:
            f.dependencies()
            original = f.cloud.request
            def request(method, path, body=None, seconds=20):
                reply = original(method, path, body, seconds)
                if path == '/servers':
                    server = reply['payload']['server']['id']
                    later = f.cloud.new_action('server', server, 'apply_firewall')
                    reply['payload']['next_actions'] = [later]
                return reply
            with patch.object(f.cloud, 'request', side_effect=request):
                server = f.create('server')
            create = f.journal.read()['intents'][-1]
            first, later = create['actions']
            f.cloud.actions[first]['status'] = 'error'
            f.cloud.actions[later]['status'] = 'running'
            self.assertFalse(f.campaign.poll_actions())
            pending = f.journal.read()['intents'][-1]
            self.assertEqual(pending['state'], 'ACKNOWLEDGED')
            self.assertTrue(pending['operation_failed'])
            self.assertEqual(pending['pending_actions'], [later])
            f.closing()
            result = f.campaign.cleanup_pass()
            self.assertTrue(result['baseline_equal'])
            self.assertEqual(result['owned_survivors'], [])
            self.assertEqual(result['pending_intents'], [create['sequence']])
            self.assertFalse(result['complete'])
            reads = f.cloud.action_reads.count(later)
            f.cloud.actions[later]['status'] = 'success'
            f.campaign.poll_actions()
            self.assertGreater(f.cloud.action_reads.count(later), reads)
            self.assertEqual(f.journal.read()['intents'][create['sequence']]['state'], 'FAILED')
            self.assertTrue(f.campaign.completion()['complete'])
            self.assertNotIn(server, f.cloud.rows['server'])

    def test_unfiltered_recovery_action_candidates_reach_both_production_correlation_branches(self):
        cases = ('none', 'wrong-command', 'wrong-kind', 'wrong-id', 'integer-one', 'boolean-id', 'duplicate-binding',
                 'missing-binding', 'multiple-create', 'one-create-among-others')
        for branch in ('unknown-id', 'recovered-id'):
            for case in cases:
                with self.subTest(branch=branch, case=case), Fixture() as f:
                    f.dependencies()
                    server = 1 if case in {'integer-one', 'boolean-id'} else 101
                    if server == 1:
                        f.cloud.next_ids['server'] = server
                        self.assertTrue(f.campaign.poll_actions())
                    f.cloud.lose_ack = True
                    with self.assertRaises(mc.GuardError): f.create('server')
                    f.closing()
                    valid = dict(id=8001, command='create_server', status='success',
                                 resources=[dict(type='server', id=server)])
                    candidates = [copy.deepcopy(valid)]
                    if case == 'none': candidates = []
                    if case == 'wrong-command': candidates[0]['command'] = 'start_server'
                    if case == 'wrong-kind': candidates[0]['resources'][0]['type'] = 'network'
                    if case == 'wrong-id': candidates[0]['resources'][0]['id'] = 999
                    if case == 'boolean-id': candidates[0]['resources'][0]['id'] = True
                    if case in {'integer-one', 'boolean-id'}:
                        binding = candidates[0]['resources'][0]['id']
                        self.assertEqual(binding, 1, 'boolean/integer equality must not mask exact type validation')
                        self.assertIs(type(binding), bool if case == 'boolean-id' else int)
                        self.assertEqual(f.cloud.rows['server'][server]['id'], 1)
                    if case == 'duplicate-binding': candidates[0]['resources'] *= 2
                    if case == 'missing-binding': candidates[0]['resources'] = [{}]
                    if case == 'multiple-create': candidates.append(dict(copy.deepcopy(valid), id=8002))
                    if case == 'one-create-among-others':
                        candidates += [dict(copy.deepcopy(valid), id=8002, command='start_server'),
                                       dict(copy.deepcopy(valid), id=8003, resources=[dict(type='server', id=999)])]
                    calls = []
                    def unfiltered(kind, rid, seconds=20, on_page=None):
                        if callable(seconds): seconds()
                        calls.append((kind, rid, copy.deepcopy(candidates)))
                        if on_page is not None:
                            on_page(copy.deepcopy(candidates))
                        return copy.deepcopy(candidates)
                    if branch == 'recovered-id':
                        with patch.object(f.cloud, 'resource_actions', return_value=[]):
                            f.campaign.recover()
                        self.assertEqual(f.journal.read()['intents'][-1]['resource_id'], server)
                        self.assertEqual(f.journal.read()['intents'][-1]['state'], 'UNKNOWN')
                    with patch.object(f.cloud, 'resource_actions', side_effect=unfiltered):
                        f.campaign.recover()
                    self.assertEqual(calls, [('server', server, candidates)])
                    if case in {'integer-one', 'boolean-id'}:
                        with contextlib.closing(cl.Journal(f.journal.path)) as reopened:
                            intent = reopened.read()['intents'][-1]
                        self.assertEqual(intent['resource_id'], 1)
                        self.assertIs(type(intent['resource_id']), int)
                    else:
                        intent = f.journal.read()['intents'][-1]
                    self.assertIsNone(intent['ack'])
                    if case == 'integer-one':
                        self.assertEqual(intent['state'], 'ACKNOWLEDGED')
                        self.assertIs(intent['required_action_identified'], True)
                        self.assertEqual(intent['actions'], [8001])
                        f.cloud.actions[8001] = copy.deepcopy(valid)
                        self.assertTrue(f.campaign.poll_actions())
                        with contextlib.closing(cl.Journal(f.journal.path)) as reopened:
                            terminal = reopened.read()['intents'][-1]
                        self.assertEqual(terminal['state'], 'DONE')
                        self.assertEqual(terminal['resource_id'], 1)
                        self.assertEqual(terminal['actions'], [8001])
                        self.assertIs(terminal['required_action_identified'], True)
                        self.assertIsNone(terminal['ack'])
                        self.assertEqual(f.campaign.completion()['pending_intents'], [])
                    elif case == 'one-create-among-others':
                        self.assertEqual(intent['state'], 'ACKNOWLEDGED')
                        self.assertIs(intent['required_action_identified'], True)
                        self.assertEqual(intent['actions'], [8001, 8002])
                        f.cloud.actions[8001] = copy.deepcopy(valid)
                        f.cloud.actions[8002] = dict(copy.deepcopy(valid), id=8002, command='start_server')
                        self.assertTrue(f.campaign.poll_actions())
                    else:
                        self.assertEqual(intent['state'], 'UNKNOWN')
                        self.assertIs(intent['required_action_identified'], False)
                        expected_ids = {'wrong-command': [8001], 'multiple-create': [8001, 8002]}.get(case, [])
                        self.assertEqual(intent['actions'], expected_ids)
                        self.assertFalse(f.campaign.completion()['complete'])
                        if case == 'boolean-id':
                            self.assertFalse(f.campaign.poll_actions())
                            with contextlib.closing(cl.Journal(f.journal.path)) as reopened:
                                unresolved = reopened.read()['intents'][-1]
                            self.assertEqual(unresolved['state'], 'UNKNOWN')
                            self.assertEqual(unresolved['resource_id'], 1)
                            self.assertEqual(unresolved['actions'], [])
                            self.assertIs(unresolved['required_action_identified'], False)
                            self.assertIsNone(unresolved['ack'])
                            self.assertEqual(f.campaign.completion()['pending_intents'], [intent['sequence']])

    def test_ordinary_cutover_finishes_clean_prepared_intents_without_orphan_repair(self):
        for interruption in ('stop', 'budget', 'death'):
            with self.subTest(interruption=interruption), Fixture() as f:
                real_sync = os.fsync
                interrupted = []
                def sync(fd):
                    real_sync(fd)
                    if interrupted or not stat.S_ISDIR(os.fstat(fd).st_mode):
                        return
                    state = json.loads(f.journal.path.read_bytes())['state']
                    if state['intents'] and state['intents'][-1]['state'] == 'PREPARED':
                        interrupted.append(True)
                        if interruption == 'stop':
                            f.campaign.request_stop()
                        elif interruption == 'budget':
                            f.wall = f.header['time']['c_ns']
                            f.heartbeat()
                        else:
                            raise StopLoop('synthetic producer exit after durable PREPARED')
                with patch.object(cl.os, 'fsync', side_effect=sync):
                    with self.assertRaises(StopLoop if interruption == 'death' else mc.GuardError):
                        f.create()
                self.assertEqual(interrupted, [True])
                self.assertEqual(f.cloud.requests, [])
                f.actor = 112
                f.processes.pop(111)
                with contextlib.closing(cl.Journal(f.journal.path)) as journal:
                    guard = cs.Campaign(journal, f.cloud, clock=lambda: f.wall, monotonic=lambda: f.mono)
                    self.assertEqual(journal.read()['intents'][0]['state'], 'PREPARED')
                    guard.close_admission()
                    self.assertEqual(journal.read()['intents'][0]['state'], 'NOT_DISPATCHED')
                    self.assertTrue(guard.cleanup_pass()['complete'])
                    self.assertIsNone(journal.read()['active_lease'])
                self.assertEqual(f.cloud.requests, [])

    def test_per_page_and_per_kind_reads_shorten_then_durably_enter_incident(self):
        with Fixture() as f:
            f.closing()
            f.wall = f.header['time']['h_ns'] - 7 * NS
            replies = []
            kinds = list(mc.PROTECT_KINDS)
            first = kinds[0]
            plural = cc.PLURALS[first]
            replies.append(MemoryProvider.json({plural: [{'id': 9000}],
                'meta': {'pagination': {'next_page': 2, 'total_entries': 2}}}))
            replies.append(MemoryProvider.json({plural: [{'id': 9999}],
                'meta': {'pagination': {'next_page': None, 'total_entries': 2}}}))
            for index, kind in enumerate(kinds[1:], 1):
                replies.append(MemoryProvider.json({cc.PLURALS[kind]: [{'id': 9000 + index}],
                    'meta': {'pagination': {'next_page': None, 'total_entries': 1}}}))
            provider = MemoryProvider(*replies)
            budgets, modes = [], []
            def connection(host, seconds):
                budgets.append(seconds)
                modes.append(f.journal.peek()['mode'])
                result = provider.connection(host, seconds)
                get = result.getresponse
                def response():
                    incoming = get()
                    read = incoming.read
                    def body(size):
                        raw = read(size)
                        if len(budgets) == 1:
                            f.wall += 6 * NS
                            f.mono += 6 * NS
                        return raw
                    incoming.read = body
                    return incoming
                result.getresponse = response
                return result
            f.campaign.cloud = cc.Cloud(f.cloud.profile, f.header['context'], connection_factory=connection,
                                       deadline=contextlib.nullcontext)
            f.campaign.recover()
            self.assertEqual(len(provider.requests), 11)
            self.assertEqual(budgets, [5] + [20] * 10)
            self.assertEqual(modes, ['CLOSING'] + ['INCIDENT'] * 10)
            self.assertEqual(f.journal.read()['mode'], 'INCIDENT')
            self.assertTrue(any(e['event'] == 'cleanup-reserve-exhausted' for e in f.journal.read()['events']))
            self.assertTrue(all(row['method'] == 'GET' for row in provider.requests))

    def test_guard_cli_protections_bound_before_creates_cover_future_and_recovered_ids(self):
        for protected, recovered in (('name', False), ('id', False), ('typed-ip', False), ('typed-ip', True)):
            with self.subTest(protected=protected, recovered=recovered), cli_fixture(arm=False) as f:
                options = []
                if protected == 'name': options = ['--protect-name', 'test-client']
                if protected == 'id': options = ['--protect-id', '101']
                if protected == 'typed-ip':
                    path = f.root / 'additional.tsv'
                    path.write_text('primary-ip\t10101\t\n')
                    path.chmod(0o600)
                    options = ['--protect-file', str(path)]
                args = tg.parse_args(['--label', 'campaign=synthetic', '--prefix', 'test', '--execute', *options])
                f.actor = 112
                with constructors(f), self.assertRaises(StopLoop):
                    tg.execute_guarded(args, f.journal, sleep=lambda _: (_ for _ in ()).throw(StopLoop()), out=io.StringIO())
                self.assertEqual(f.journal.read()['owned'], [])
                self.assertIn('protections', f.journal.read())
                f.actor = 111
                f.challenge = f.journal.read()['guard']['challenge']
                f.heartbeat()
                f.dependencies()
                if protected == 'name':
                    before = f.journal.read()
                    requests = copy.deepcopy(f.cloud.requests)
                    with self.assertRaisesRegex(mc.GuardError, 'additional guard protection conflicts with a planned name'):
                        f.create('server')
                    after = f.journal.read()
                    self.assertEqual(f.cloud.requests, requests)
                    self.assertEqual(after['intents'], before['intents'])
                    self.assertEqual(after['owned'], before['owned'])
                    self.assertEqual(after['mode'], 'OPEN')
                    self.assertFalse(any(r['kind'] == 'server' for r in after['owned']))
                    self.assertNotIn(101, f.cloud.rows['server'])
                    continue
                if recovered:
                    f.cloud.lose_ack = True
                    with self.assertRaises(mc.GuardError): f.create('server')
                    f.closing()
                    f.campaign.recover()
                    self.assertEqual(f.journal.read()['intents'][-1]['resource_id'], 101)
                else:
                    f.create('server')
                    f.closing()
                before = len(f.cloud.requests)
                with self.assertRaisesRegex(mc.GuardError, 'additional guard protection'):
                    f.campaign.mutate('server', 101, 'delete')
                self.assertEqual(len(f.cloud.requests), before)
                self.assertIn(101, f.cloud.rows['server'])
                self.assertIn(10101, f.cloud.rows['primary-ip'])

    def test_typed_additional_protection_does_not_protect_another_kind_same_number(self):
        with Fixture() as f:
            f.campaign.bind_protections(cs.protection_policy(by_kind={'primary-ip': [101]}))
            server = f.create('server')
            f.closing()
            f.campaign.mutate('server', server, 'delete')
            self.assertEqual(f.deletes(), [('DELETE', '/servers/101')])
            self.assertEqual(f.journal.read()['protections']['ids'], [])

    def test_heartbeat_defers_real_publications_without_losing_its_authenticated_watch(self):
        with Fixture() as f:
            f.create()
            lease = dict(intent=0, actor=f.header['producer'], deadline_ns=f.wall + 20 * NS,
                         monotonic_deadline_ns=f.mono + 20 * NS)
            with f.journal.locked() as state:
                state['active_lease'] = copy.deepcopy(lease)
                f.journal.commit(state)
            with contextlib.closing(cl.Journal(f.journal.path)) as journal:
                guard = cs.Campaign(journal, f.cloud, clock=lambda: f.wall, monotonic=lambda: f.mono)
                f.actor = 112
                guard.heartbeat()
                self.assertEqual(guard._watched_lease, lease)
                checkpoint = journal.checkpoint
                original = cl.os.stat
                reads, published = [], []
                with f.journal.locked() as state:
                    def publication(name, *args, **kwargs):
                        if name == f.journal.path.name and kwargs.get('dir_fd') == journal._directory:
                            reads.append(name)
                            if len(reads) % 2 == 0:
                                state['active_lease'] = None
                                state['events'].append(dict(event='concurrent-publication', sequence=len(published)))
                                f.journal.commit(state)
                                published.append(f.journal.checkpoint)
                        return original(name, *args, **kwargs)
                    with patch.object(cl.os, 'stat', side_effect=publication):
                        observed = guard.heartbeat()
                    self.assertEqual(len(published), 3)
                    self.assertEqual(observed['mode'], 'OPEN')
                    self.assertEqual(guard._watched_lease, lease)
                    self.assertEqual(journal.checkpoint, checkpoint)
                    self.assertIsNone(journal.read_if_available())
                    heartbeat = json.loads(cc.private_bytes(f.guard_directory / 'heartbeat.json'))
                    self.assertEqual(heartbeat['checkpoint'], checkpoint)
                guard.heartbeat()
                self.assertIsNone(guard._watched_lease)
                self.assertEqual(journal.checkpoint, published[-1])
                self.assertEqual(len(f.cloud.requests), 1)

    def test_heartbeat_survives_three_real_head_unlinks_between_open_and_fstat(self):
        with Fixture() as f:
            f.create()
            with f.journal.locked() as state:
                lease = copy.deepcopy(state['intents'][0]['send_lease'])
                self.assertEqual(lease['actor'], f.header['producer'])
                state['active_lease'] = copy.deepcopy(lease)
                f.journal.commit(state)
            with contextlib.closing(cl.Journal(f.journal.path)) as journal:
                guard = cs.Campaign(journal, f.cloud, clock=lambda: f.wall, monotonic=lambda: f.mono)
                f.actor = 112
                cached = guard.heartbeat()
                self.assertEqual(guard._watched_lease, lease)
                checkpoint = journal.checkpoint
                previous_heartbeat = json.loads(cc.private_bytes(f.guard_directory / 'heartbeat.json'))
                f.wall += NS
                f.mono += NS
                original_open, original_fstat = cl.os.open, cl.os.fstat
                pending, opened, published, unlinked = set(), [], [], []
                with f.journal.locked() as state:
                    def open_head(name, *args, **kwargs):
                        fd = original_open(name, *args, **kwargs)
                        if name == f.journal.path.name and kwargs.get('dir_fd') == journal._directory:
                            info = original_fstat(fd)
                            self.assertEqual(info.st_nlink, 1)
                            opened.append((info.st_dev, info.st_ino))
                            pending.add(fd)
                        return fd

                    def fstat_head(fd):
                        if fd in pending:
                            pending.remove(fd)
                            state['active_lease'] = None
                            state['events'].append(dict(event='open-fstat-publication', sequence=len(published)))
                            f.journal.commit(state)
                            published.append(f.journal.checkpoint)
                            info = original_fstat(fd)
                            self.assertEqual(info.st_nlink, 0, 'real publication must unlink the opened old head')
                            self.assertEqual((info.st_dev, info.st_ino), opened[-1])
                            self.assertNotEqual(info.st_ino, f.journal.path.stat().st_ino)
                            unlinked.append((info.st_dev, info.st_ino))
                            return info
                        return original_fstat(fd)

                    with patch.object(cl.os, 'open', side_effect=open_head), \
                            patch.object(cl.os, 'fstat', side_effect=fstat_head):
                        observed = guard.heartbeat()
                    self.assertEqual(len(published), 3)
                    self.assertEqual(len(opened), 3)
                    self.assertEqual(unlinked, opened)
                    self.assertEqual(pending, set())
                    self.assertEqual([row['generation'] for row in published],
                                     [checkpoint['generation'] + step for step in (1, 2, 3)])
                    self.assertEqual(observed, cached)
                    self.assertEqual(guard._last_observation, cached)
                    self.assertEqual(guard._watched_lease, lease)
                    self.assertEqual(journal.checkpoint, checkpoint)
                    self.assertTrue(journal.is_ancestor(checkpoint))
                    self.assertFalse(journal.is_ancestor(published[-1]))
                    self.assertIsNone(journal.read_if_available())
                    heartbeat = json.loads(cc.private_bytes(f.guard_directory / 'heartbeat.json'))
                    self.assertEqual(heartbeat['binding'], journal.binding)
                    self.assertEqual(heartbeat['identity'], cached['guard']['identity'])
                    self.assertEqual(heartbeat['challenge'], cached['guard']['challenge'])
                    self.assertEqual(heartbeat['checkpoint'], checkpoint)
                    self.assertEqual(heartbeat['monotonic_ns'], f.mono)
                    self.assertGreater(heartbeat['monotonic_ns'], previous_heartbeat['monotonic_ns'])
                confirmed = guard.heartbeat()
                self.assertIsNone(confirmed['active_lease'])
                self.assertIsNone(guard._watched_lease)
                self.assertEqual(journal.checkpoint, published[-1])
                self.assertTrue(journal.is_ancestor(checkpoint))
                self.assertEqual(confirmed, f.journal.read())
                self.assertEqual(len(f.cloud.requests), 1)
                self.assertFalse((f.guard_directory / 'stop.json').exists())

    def test_help_and_retired_retry_flags_describe_only_supported_execution(self):
        output = io.StringIO()
        with contextlib.redirect_stdout(output), self.assertRaises(SystemExit) as exit:
            provision.parse_args(['--help'])
        self.assertEqual(exit.exception.code, 0)
        help_text = output.getvalue()
        self.assertIn('guarded HTTPS', help_text)
        self.assertIn('diagnostic dry-run', help_text)
        self.assertNotIn('execute the hcloud commands', help_text)
        self.assertNotIn('the client first to build on it', help_text)
        for flag in ('--retries', '--retry-seconds'):
            with self.subTest(flag=flag), contextlib.redirect_stderr(io.StringIO()), self.assertRaises(SystemExit) as exit:
                tg.parse_args(['--label', 'campaign=synthetic', '--prefix', 'test', flag, '1'])
            self.assertEqual(exit.exception.code, 2)


if __name__ == '__main__':
    unittest.main()
