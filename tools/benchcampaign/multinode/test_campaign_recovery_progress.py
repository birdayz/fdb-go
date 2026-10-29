"""Offline recovery ambiguity, durable identity and continuing cleanup controls."""
import copy
import json
import unittest
from unittest.mock import patch

import campaign_cloud as cc
import campaign_safety as cs
import creation_ledger as cl
import mn_common as mc
from test_campaign_safety import Fixture, Provider, NS, T0, utc


class RecoveryProvider(Provider):
    """Only provider reads fail; real Campaign and Journal methods are untouched."""
    def __init__(self, fixture, profile):
        super().__init__(fixture, profile)
        self.activity = []
        self.describe_error = self.action_error = self.resource_error = None
        self.list_result = self.describe_result = None

    def allowance(self, seconds):
        return float(seconds() if callable(seconds) else seconds)

    def list(self, kind, seconds=20):
        seconds = self.allowance(seconds)
        self.activity.append(('list', kind))
        rows = super().list(kind, seconds)
        return self.list_result(kind, rows) if self.list_result else rows

    def describe(self, kind, rid, seconds=20):
        seconds = self.allowance(seconds)
        self.profile.revalidate(self.fixture.header['context'])
        self.activity.append(('describe', kind, rid))
        if self.describe_error:
            self.describe_error(kind, rid)
        item = super().describe(kind, rid, seconds)
        return self.describe_result(kind, rid, item) if self.describe_result else item

    def action(self, rid, seconds=20):
        seconds = self.allowance(seconds)
        self.profile.revalidate(self.fixture.header['context'])
        self.activity.append(('action', rid))
        if self.action_error:
            self.action_error(rid)
        return super().action(rid, seconds)

    def resource_actions(self, kind, rid, seconds=20, on_page=None):
        seconds = self.allowance(seconds)
        self.profile.revalidate(self.fixture.header['context'])
        self.activity.append(('resource-actions', kind, rid))
        if self.resource_error:
            self.resource_error(kind, rid)
        return super().resource_actions(kind, rid, seconds, on_page=on_page)

    def request(self, method, path, body=None, seconds=20):
        seconds = self.allowance(seconds)
        self.activity.append(('request', method, path))
        return super().request(method, path, body, seconds)


def backend(f):
    f.cloud = RecoveryProvider(f, f.cloud.profile)
    f.campaign = campaign(f, f.journal)


def campaign(f, journal):
    return cs.Campaign(journal, f.cloud, clock=lambda: f.wall, monotonic=lambda: f.mono)


def reopened(f):
    journal = cl.Journal(f.journal.path)
    f.stack.callback(journal.close)
    return journal


def lose_create(test, f, kind):
    if kind == 'server':
        f.dependencies()
    f.cloud.lose_ack = True
    try:
        with test.assertRaisesRegex(mc.GuardError, 'failed or ambiguous'):
            f.create(kind)
    finally:
        f.cloud.lose_ack = False
    return len(f.journal.read()['intents']) - 1


def server_record(state, rid=101):
    return next(row for row in state['owned'] if row['kind'] == 'server' and row['id'] == rid)


class RecoveryCandidateTests(unittest.TestCase):
    def test_unavailable_exact_candidate_cannot_turn_two_matching_networks_into_unique_ownership(self):
        for fault, failure, error_type in (('guard-error', mc.GuardError, 'GuardError'),
                ('os-error', OSError, 'OSError'), ('exact-none', None, 'GuardError'),
                ('missing-canonical', None, 'KeyError'), ('malformed-canonical', None, 'TypeError')):
            with self.subTest(fault=fault), Fixture() as f:
                backend(f)
                sequence = lose_create(self, f, 'network')
                second = copy.deepcopy(f.cloud.rows['network'][201])
                second['id'] = 202
                f.cloud.rows['network'][202] = second
                f.closing()
                failed_reads = []
                def unavailable(kind, rid):
                    if (kind, rid) == ('network', 202):
                        failed_reads.append(rid)
                        if failure:
                            raise failure('synthetic second candidate exact GET unavailable')
                def incomplete(kind, rid, item):
                    if (kind, rid) != ('network', 202):
                        return item
                    if fault == 'exact-none':
                        return None  # Cloud.describe's exact-404 result.
                    if fault == 'missing-canonical':
                        item.pop('ip_range')
                    if fault == 'malformed-canonical':
                        return list(item.items())
                    return item
                f.cloud.describe_error = unavailable
                f.cloud.describe_result = incomplete
                f.campaign.recover()
                self.assertEqual(failed_reads, [202])
                first = reopened(f).read()
                self.assertEqual(first['intents'][sequence]['state'], 'UNKNOWN')
                self.assertIsNone(first['intents'][sequence]['resource_id'])
                self.assertIsNone(first['intents'][sequence]['ack'])
                self.assertEqual(first['owned'], [])
                census = [e for e in first['events'] if e.get('event') == 'recovery-census'][-1]
                self.assertEqual(census['candidates'], [201])
                self.assertEqual(census['inconclusive_candidates'], [202])
                self.assertIn(dict(event='recovery-candidate-unavailable', kind='network', id=202,
                    error_type=error_type), first['events'])
                self.assertEqual(f.deletes(), [])
                self.assertEqual(len(f.cloud.requests), 1)
                incomplete_cleanup = f.campaign.cleanup_pass()
                self.assertFalse(incomplete_cleanup['complete'])
                self.assertIn(sequence, incomplete_cleanup['pending_intents'])
                self.assertEqual(reopened(f).read()['owned'], [])
                self.assertEqual(f.deletes(), [])
                self.assertEqual(len(f.cloud.requests), 1)
                # A healthy read of both rows confirms ambiguity, not a license
                # to keep the survivor from the earlier incomplete observation.
                f.cloud.describe_error = None
                f.cloud.describe_result = None
                f.campaign.recover()
                restored = reopened(f).read()
                self.assertEqual(restored['intents'][sequence]['state'], 'UNKNOWN')
                self.assertIsNone(restored['intents'][sequence]['resource_id'])
                self.assertEqual(restored['owned'], [])
                census = [e for e in restored['events'] if e.get('event') == 'recovery-census'][-1]
                self.assertEqual(census['candidates'], [201, 202])
                self.assertEqual(census['inconclusive_candidates'], [])
                result = f.campaign.cleanup_pass()
                self.assertFalse(result['complete'])
                self.assertEqual(result['pending_intents'], [sequence])
                self.assertEqual(f.deletes(), [])
                self.assertEqual(len(f.cloud.requests), 1)
                self.assertEqual(set(f.cloud.rows['network']), {9001, 201, 202})

    def test_one_fully_described_network_is_recovered_and_cleaned_without_another_post(self):
        with Fixture() as f:
            backend(f)
            sequence = lose_create(self, f, 'network')
            original = copy.deepcopy(f.cloud.rows['network'][201])
            f.closing()
            f.campaign.recover()
            recovered = reopened(f).read()
            self.assertEqual([(row['kind'], row['id']) for row in recovered['owned']], [('network', 201)])
            intent = recovered['intents'][sequence]
            self.assertEqual(intent['state'], 'ACKNOWLEDGED')
            self.assertEqual(intent['recovery']['candidate'], original)
            self.assertEqual(intent['recovery']['origin'], 'conditional-nonce-recovery')
            self.assertIsNone(intent['ack'])
            self.assertIsNone(intent['original_ack'])
            self.assertTrue(f.campaign.poll_actions())
            result = f.campaign.cleanup_pass()
            self.assertTrue(result['complete'])
            self.assertTrue(result['baseline_equal'])
            self.assertEqual([(method, path) for method, path, _, _ in f.cloud.requests],
                [('POST', '/networks'), ('DELETE', '/networks/201')])


class RecoveryGuardLoopTests(unittest.TestCase):
    def test_malformed_unrelated_census_labels_publish_an_incident_then_healthy_retry_recovers_and_cleans(self):
        for labels in (['unexpected'], 'unexpected', 7, []):
            with self.subTest(labels=labels), Fixture() as f:
                backend(f)
                f.dependencies()
                sequence = lose_create(self, f, 'network')
                initial = f.journal.read()
                self.assertEqual(initial['intents'][sequence]['state'], 'UNKNOWN')
                self.assertIsNone(initial['intents'][sequence]['resource_id'])
                self.assertEqual({(row['kind'], row['id']) for row in initial['owned']},
                    {('firewall', 301), ('ssh-key', 401)})
                known_action = next(i['actions'][0] for i in initial['intents'] if i['kind'] == 'firewall')
                baseline_id = f.header['baseline']['network'][0]
                malformed_reads = []
                def malformed(kind, rows):
                    if kind == 'network':
                        row = next(row for row in rows if row['id'] == baseline_id)
                        row['labels'] = copy.deepcopy(labels)
                        malformed_reads.append(baseline_id)
                    return rows
                f.cloud.list_result = malformed
                with self.assertRaisesRegex(mc.GuardError, 'malformed recovery census labels'):
                    f.campaign.recover()
                self.assertTrue(malformed_reads)
                self.assertEqual(f.deletes(), [])
                self.assertEqual(len(f.cloud.requests), 3)
                self.assertIsNone(reopened(f).read()['intents'][sequence]['resource_id'])
                f.processes[113] = dict(pid=113, parent=110, starttime='1130', argv_sha256=cc.digest(b'113'))
                f.processes.pop(111)
                f.processes.pop(112)
                f.actor = 113
                journal = reopened(f)
                guard = campaign(f, journal)
                incidents_before = set(f.guard_directory.glob('incident-*.json'))
                incidents, sleeps = {}, []
                f.cloud.activity.clear()
                def sleep(seconds):
                    sleeps.append(seconds)
                    self.assertEqual(sleeps, [1], 'malformed metadata must cause one bounded retry, not exit or spin')
                    current = reopened(f).read()
                    intent = current['intents'][sequence]
                    self.assertEqual(current['mode'], 'CLOSING')
                    self.assertEqual(intent['state'], 'UNKNOWN')
                    self.assertIsNone(intent['resource_id'])
                    self.assertIsNone(intent['ack'])
                    self.assertEqual({(row['kind'], row['id']) for row in current['owned']},
                        {('firewall', 301), ('ssh-key', 401)})
                    self.assertEqual(f.deletes(), [])
                    self.assertEqual(len(f.cloud.requests), 3)
                    self.assertNotIn(('action', known_action), f.cloud.activity,
                        'the malformed recovery census must precede action polling and resource mutation')
                    published = set(f.guard_directory.glob('incident-*.json')) - incidents_before
                    self.assertEqual(len(published), 1)
                    for path in published:
                        self.assertEqual(path.stat().st_mode & 0o777, 0o600)
                        raw = cc.private_bytes(path)
                        evidence = json.loads(raw)
                        self.assertIs(evidence['complete'], False)
                        self.assertIs(evidence['hard_lifetime_met'], False)
                        self.assertEqual(evidence['error_type'], 'GuardError')
                        incidents[path] = raw
                    heartbeat = json.loads(cc.private_bytes(f.guard_directory / 'heartbeat.json'))
                    self.assertEqual(heartbeat['identity'], f.processes[113])
                    # Restore provider metadata only at the real loop's sleep
                    # boundary; the same guard must retry without creator replay.
                    f.cloud.list_result = None
                    f.wall += round(seconds * NS)
                    f.mono += round(seconds * NS)
                with patch.object(cs.os, 'pidfd_open', create=True,
                        side_effect=AssertionError('already-gone producer must not be signalled')), \
                     patch.object(cs.signal, 'pidfd_send_signal', create=True,
                        side_effect=AssertionError('offline guard must not signal a real process')), \
                     patch.object(cs.os, 'kill', side_effect=AssertionError('bare PID signalling forbidden')):
                    result = guard.guard_loop(sleep=sleep, poll_seconds=1, cleanup_interval_seconds=1)
                self.assertEqual(sleeps, [1])
                self.assertTrue(result['complete'])
                self.assertTrue(result['baseline_equal'])
                self.assertTrue(result['hard_lifetime_met'])
                self.assertEqual(result['pending_intents'], [])
                self.assertIn(('action', known_action), f.cloud.activity)
                final = reopened(f).read()
                self.assertEqual(final['mode'], 'COMPLETE')
                self.assertEqual(final['guard']['identity'], f.processes[113])
                self.assertEqual(final['intents'][sequence]['resource_id'], 201)
                self.assertEqual(final['intents'][sequence]['state'], 'DONE')
                self.assertIsNone(final['intents'][sequence]['ack'])
                self.assertEqual(final['intents'][sequence]['recovery']['candidate']['id'], 201)
                self.assertEqual({(row['kind'], row['id']) for row in final['owned']},
                    {('firewall', 301), ('ssh-key', 401), ('network', 201)})
                self.assertTrue(all(i['state'] == 'DONE' and not i.get('pending_actions') for i in final['intents']))
                self.assertEqual([(method, path) for method, path, _, _ in f.cloud.requests],
                    [('POST', '/firewalls'), ('POST', '/ssh_keys'), ('POST', '/networks'),
                     ('DELETE', '/firewalls/301'), ('DELETE', '/ssh_keys/401'), ('DELETE', '/networks/201')])
                self.assertEqual(json.loads(cc.private_bytes(f.guard_directory / 'cleanup.json')), result)
                self.assertEqual(set(f.guard_directory.glob('incident-*.json')) - incidents_before, set(incidents))
                for path, raw in incidents.items():
                    self.assertEqual(cc.private_bytes(path), raw)


class ActionProgressTests(unittest.TestCase):
    def test_unavailable_known_action_including_done_does_not_block_any_of_seven_safe_deletes(self):
        for previously_done in (False, True):
            for failure in (mc.GuardError, OSError):
                with self.subTest(previously_done=previously_done, failure=failure.__name__), Fixture() as f:
                    backend(f)
                    ids = f.create_all()
                    if previously_done:
                        self.assertTrue(f.campaign.poll_actions())
                        self.assertTrue(all(i['state'] == 'DONE' for i in f.journal.read()['intents']))
                    initial = f.journal.read()
                    sequence = next(i['sequence'] for i in initial['intents']
                        if i['kind'] == 'server' and i['resource_id'] == 101)
                    action = initial['intents'][sequence]['actions'][0]
                    f.closing()
                    failed = []
                    def unavailable(rid):
                        if rid == action:
                            failed.append(rid)
                            raise failure('synthetic known action endpoint unavailable')
                    f.cloud.action_error = unavailable
                    result = f.campaign.cleanup_pass()
                    expected = [('DELETE', f'/servers/{rid}') for rid in (101, 102, 103, 104)]
                    expected += [('DELETE', '/firewalls/301'), ('DELETE', '/ssh_keys/401'), ('DELETE', '/networks/201')]
                    self.assertEqual(f.deletes(), expected)
                    self.assertEqual(len(ids), 7)
                    self.assertEqual(len(f.cloud.requests), 14)
                    self.assertTrue(failed)
                    self.assertFalse(result['complete'])
                    self.assertTrue(result['baseline_equal'])
                    self.assertEqual(result['owned_survivors'], [])
                    self.assertEqual(result['subordinate_survivors'], 0)
                    self.assertIn(sequence, result['pending_intents'])
                    current = reopened(f).read()
                    self.assertEqual(current['intents'][sequence]['state'], 'ACKNOWLEDGED')
                    self.assertIn(action, current['intents'][sequence]['actions'])
                    self.assertIn(action, current['intents'][sequence]['pending_actions'])
                    self.assertIs(current['intents'][sequence]['required_action_identified'], True)
                    self.assertTrue(any(e.get('event') == 'action-read-unavailable' and e.get('intent') == sequence
                        and e.get('action_id') == action and e.get('error_type') == failure.__name__
                        for e in current['events']), 'failed action read must retain durable diagnostic evidence')
                    again = f.campaign.cleanup_pass()
                    self.assertFalse(again['complete'])
                    self.assertIn(sequence, again['pending_intents'])
                    self.assertEqual(f.deletes(), expected, 'absent resources must not be dispatched again')
                    f.cloud.action_error = None
                    self.assertEqual(f.cloud.actions[action]['status'], 'success')
                    final = f.campaign.cleanup_pass()
                    self.assertTrue(final['complete'])
                    self.assertTrue(final['hard_lifetime_met'])
                    self.assertEqual(f.deletes(), expected)
                    self.assertEqual(len(f.cloud.requests), 14)
                    self.assertEqual(reopened(f).read()['intents'][sequence]['state'], 'DONE')

    def test_failed_actions_are_refreshed_and_demoted_for_running_or_unavailable_reads_until_terminal_again(self):
        for observation, failure in (('running', None), ('guard-error', mc.GuardError), ('os-error', OSError)):
            with self.subTest(observation=observation), Fixture() as f:
                backend(f)
                rid = f.create()
                f.campaign.mutate('network', rid, 'subnet', copy.deepcopy(f.header['subnet']))
                initial = f.journal.read()
                sequence = next(i['sequence'] for i in initial['intents'] if i['operation'] == 'subnet')
                action = initial['intents'][sequence]['actions'][0]
                f.cloud.actions[action]['status'] = 'error'
                self.assertFalse(f.campaign.poll_actions())
                failed = reopened(f).read()['intents'][sequence]
                self.assertEqual(failed['state'], 'FAILED')
                self.assertIs(failed['operation_failed'], True)
                self.assertEqual(failed['pending_actions'], [])
                self.assertEqual(failed['actions'], [action])
                f.closing()
                if observation == 'running':
                    f.cloud.actions[action]['status'] = 'running'
                unavailable_reads = []
                def unavailable(candidate):
                    if candidate == action and failure:
                        unavailable_reads.append(candidate)
                        raise failure('synthetic formerly failed action endpoint unavailable')
                f.cloud.action_error = unavailable
                f.cloud.activity.clear()
                result = f.campaign.cleanup_pass()
                self.assertGreaterEqual(f.cloud.activity.count(('action', action)), 2,
                    'both cleanup polls must refresh the known action even after its previous FAILED state')
                self.assertFalse(result['complete'])
                self.assertTrue(result['baseline_equal'])
                self.assertEqual(result['owned_survivors'], [])
                self.assertEqual(result['subordinate_survivors'], 0)
                self.assertEqual(result['pending_intents'], [sequence])
                state = reopened(f).read()
                pending = state['intents'][sequence]
                self.assertEqual(state['mode'], 'CLOSING')
                self.assertEqual(pending['state'], 'ACKNOWLEDGED')
                self.assertIs(pending['operation_failed'], True)
                self.assertIs(pending['required_action_identified'], True)
                self.assertEqual(pending['pending_actions'], [action])
                self.assertEqual(pending['actions'], [action])
                if failure:
                    self.assertTrue(unavailable_reads)
                    self.assertTrue(any(e.get('event') == 'action-read-unavailable' and e.get('intent') == sequence
                        and e.get('action_id') == action and e.get('error_type') == failure.__name__ for e in state['events']))
                self.assertEqual(f.deletes(), [('DELETE', '/networks/201')])
                self.assertEqual(len(f.cloud.requests), 3)
                self.assertIsNone(f.campaign.terminal_result())
                again = f.campaign.cleanup_pass()
                self.assertFalse(again['complete'])
                self.assertEqual(again['pending_intents'], [sequence])
                self.assertEqual(f.deletes(), [('DELETE', '/networks/201')])
                # A later successful terminal observation resolves uncertainty,
                # but must never erase the earlier operation failure.
                f.cloud.action_error = None
                f.cloud.actions[action]['status'] = 'success'
                self.assertFalse(f.campaign.poll_actions())
                terminal = reopened(f).read()['intents'][sequence]
                self.assertEqual(terminal['state'], 'FAILED')
                self.assertIs(terminal['operation_failed'], True)
                self.assertEqual(terminal['pending_actions'], [])
                self.assertEqual(terminal['actions'], [action])
                complete = f.campaign.cleanup_pass()
                self.assertTrue(complete['complete'])
                self.assertTrue(complete['baseline_equal'])
                self.assertTrue(complete['hard_lifetime_met'])
                self.assertEqual(complete['pending_intents'], [])
                self.assertEqual(f.campaign.terminal_result(), complete)
                final = reopened(f).read()
                self.assertEqual(final['mode'], 'COMPLETE')
                self.assertEqual(final['intents'][sequence]['state'], 'FAILED')
                self.assertIs(final['intents'][sequence]['operation_failed'], True)
                self.assertEqual(f.deletes(), [('DELETE', '/networks/201')])
                self.assertEqual(len(f.cloud.requests), 3)

    def test_success_then_running_demotes_done_and_pending_evidence_blocks_completion_and_terminal_restart(self):
        with Fixture() as f:
            backend(f)
            rid = f.create()
            f.campaign.mutate('network', rid, 'subnet', copy.deepcopy(f.header['subnet']))
            self.assertTrue(f.campaign.poll_actions())
            state = f.journal.read()
            sequence = next(i['sequence'] for i in state['intents'] if i['operation'] == 'subnet')
            action = state['intents'][sequence]['actions'][0]
            self.assertEqual(state['intents'][sequence]['state'], 'DONE')
            f.closing()
            f.campaign.mutate('network', rid, 'delete')
            self.assertTrue(f.campaign.poll_actions())
            self.assertEqual(f.cloud.actions[action]['status'], 'success')
            f.cloud.actions[action]['status'] = 'running'
            self.assertFalse(f.campaign.poll_actions(), 'a fresh running read must demote a previously DONE intent')
            running = reopened(f).read()['intents'][sequence]
            self.assertEqual(running['state'], 'ACKNOWLEDGED')
            self.assertEqual(running['pending_actions'], [action])
            result = f.campaign.completion()
            self.assertFalse(result['complete'])
            self.assertTrue(result['baseline_equal'])
            self.assertEqual(result['owned_survivors'], [])
            self.assertIn(sequence, result['pending_intents'])
            # Authenticate an inconsistent state label without changing the real
            # observed pending ID; completion must independently honor the ID.
            with f.journal.locked() as state:
                state['intents'][sequence]['state'] = 'DONE'
                f.journal.commit(state)
            result = f.campaign.completion()
            self.assertFalse(result['complete'])
            self.assertIn(sequence, result['pending_intents'])
            self.assertEqual(f.journal.read()['mode'], 'CLOSING')
            f.cloud.actions[action]['status'] = 'success'
            self.assertTrue(f.campaign.poll_actions())
            complete = f.campaign.completion()
            self.assertTrue(complete['complete'])
            self.assertEqual(f.campaign.terminal_result(), complete)
            valid = reopened(f).read()
            check = copy.deepcopy([e for e in valid['events'] if e.get('event') == 'completion-check'][-1])
            # Keep the genuinely produced COMPLETE receipt, and independently
            # corrupt only its supposedly terminal intent's pending evidence.
            with f.journal.locked() as state:
                self.assertEqual(state['intents'][sequence]['state'], 'DONE')
                state['intents'][sequence]['pending_actions'] = [action]
                f.journal.commit(state)
            journal = reopened(f)
            current = campaign(f, journal)
            checkpoint = copy.deepcopy(journal.checkpoint)
            f.cloud.activity.clear()
            with self.assertRaisesRegex(mc.GuardError, 'invalid authenticated completion evidence'):
                current.terminal_result()
            self.assertEqual(f.cloud.activity, [])
            self.assertEqual(journal.checkpoint, checkpoint)
            self.assertEqual([e for e in journal.read()['events'] if e.get('event') == 'completion-check'][-1], check)
            with journal.locked() as state:
                state['intents'][sequence]['pending_actions'] = []
                journal.commit(state)
            self.assertEqual(current.terminal_result(), complete)
            self.assertEqual(f.deletes(), [('DELETE', '/networks/201')])
            self.assertEqual(len(f.cloud.requests), 3)


class CompletionAbsenceTests(unittest.TestCase):
    def prepare(self, f, target):
        backend(f)
        if target == 'network':
            rid = f.create()
            self.assertTrue(f.campaign.poll_actions())
            f.closing()
            return 'network', rid
        rid = f.create('server')
        saved_ip = copy.deepcopy(f.cloud.rows['primary-ip'][rid + 10000])
        with f.journal.locked() as state:
            server = f.cloud.describe('server', rid)
            ips = f.cloud.list('primary-ip')
            associations = f.campaign.cascade(state, server, ips)
            self.assertEqual([row['id'] for row in associations], [rid + 10000])
            f.journal.commit(state)
        self.assertTrue(f.campaign.poll_actions())
        f.closing()
        for kind, owned in (('server', rid), ('firewall', 301), ('ssh-key', 401)):
            f.campaign.mutate(kind, owned, 'delete')
        self.assertTrue(f.campaign.poll_actions())
        self.assertNotIn(rid + 10000, f.cloud.rows['primary-ip'])
        # An exact endpoint can still expose the old IP after its server delete
        # even while the list endpoint consistently reports baseline inventory.
        f.cloud.rows['primary-ip'][rid + 10000] = saved_ip
        return 'primary-ip', rid + 10000

    def test_census_omission_cannot_complete_with_live_or_unavailable_owned_and_recorded_ip_exact_reads(self):
        for target in ('network', 'primary-ip'):
            for observation, failure in (('positive', None), ('guard-error', mc.GuardError), ('os-error', OSError)):
                with self.subTest(target=target, observation=observation), Fixture() as f:
                    kind, rid = self.prepare(f, target)
                    baseline_deletes = copy.deepcopy(f.deletes())
                    requests = len(f.cloud.requests)
                    state = f.journal.read()
                    self.assertTrue(all(i['state'] == 'DONE' and not i.get('pending_actions') for i in state['intents']))
                    targets = {(row['kind'], row['id']) for row in state['owned']}
                    targets |= {('primary-ip', ip) for row in state['owned'] for ip in row['implicit_ips']}
                    self.assertIn((kind, rid), targets)
                    def omitted(candidate_kind, rows):
                        return [row for row in rows if (candidate_kind, row['id']) != (kind, rid)]
                    def unavailable(candidate_kind, candidate_id):
                        if failure and (candidate_kind, candidate_id) == (kind, rid):
                            raise failure('synthetic completion exact read unavailable')
                    f.cloud.list_result = omitted
                    f.cloud.describe_error = unavailable
                    f.cloud.activity.clear()
                    result = f.campaign.completion()
                    attempted = {(entry[1], entry[2]) for entry in f.cloud.activity if entry[0] == 'describe'}
                    self.assertTrue(targets <= attempted, 'every recorded owned and implicit ID requires a fresh exact absence check')
                    self.assertTrue(result['baseline_equal'], 'census equality must not independently hide the exact-read gate')
                    self.assertEqual(result['pending_intents'], [], 'pending actions must not independently block this case')
                    self.assertFalse(result['complete'])
                    if failure:
                        self.assertIn(dict(kind=kind, id=rid, error_type=failure.__name__), result['unresolved_readbacks'])
                    elif kind == 'network':
                        self.assertIn(dict(kind=kind, id=rid), result['owned_survivors'])
                    else:
                        self.assertEqual(result['subordinate_survivors'], 1)
                    self.assertIsNone(f.campaign.terminal_result())
                    current = reopened(f).read()
                    self.assertEqual(current['mode'], 'CLOSING')
                    self.assertTrue(any(e.get('event') == 'observed-retirement' and e.get('kind') == kind
                        and e.get('id') == rid for e in current['events']))
                    again = f.campaign.cleanup_pass()
                    self.assertFalse(again['complete'])
                    self.assertTrue(again['baseline_equal'])
                    self.assertEqual(f.deletes(), baseline_deletes)
                    self.assertEqual(len(f.cloud.requests), requests)
                    # Restoring healthy original-looking exact/list metadata
                    # must not undo the already observed permanent retirement.
                    f.cloud.describe_error = None
                    f.cloud.list_result = None
                    restored = f.campaign.cleanup_pass()
                    self.assertFalse(restored['complete'])
                    self.assertEqual(f.deletes(), baseline_deletes)
                    current = reopened(f).read()
                    self.assertTrue(any(e.get('event') == 'observed-retirement' and e.get('kind') == kind
                        and e.get('id') == rid for e in current['events']))
                    # Consistent exact absence is the positive counterpart: it
                    # may finish cleanup, but never restore DELETE authority.
                    f.cloud.rows[kind].pop(rid)
                    f.cloud.activity.clear()
                    absent = f.campaign.completion()
                    attempted = {(entry[1], entry[2]) for entry in f.cloud.activity if entry[0] == 'describe'}
                    self.assertTrue(targets <= attempted)
                    self.assertTrue(absent['complete'])
                    self.assertTrue(absent['baseline_equal'])
                    self.assertTrue(absent['hard_lifetime_met'])
                    self.assertEqual(absent['owned_survivors'], [])
                    self.assertEqual(absent['subordinate_survivors'], 0)
                    self.assertEqual(absent['pending_intents'], [])
                    self.assertEqual(f.campaign.terminal_result(), absent)
                    self.assertEqual(f.deletes(), baseline_deletes)
                    self.assertEqual(len(f.cloud.requests), requests)


class RecoveryValidationTests(unittest.TestCase):
    def test_signed_malformed_digest_is_refused_by_recover_and_cleanup_before_all_provider_activity(self):
        for caller in ('recover', 'cleanup_pass'):
            with self.subTest(caller=caller), Fixture() as f:
                backend(f)
                f.create()
                self.assertTrue(f.campaign.poll_actions())
                f.closing()
                with f.journal.locked() as state:
                    state['intents'][0]['request_sha256'] = 1
                    f.journal.commit(state)
                journal = reopened(f)
                current = campaign(f, journal)
                checkpoint = copy.deepcopy(journal.checkpoint)
                head = journal.path.read_bytes()
                names = sorted(p.name for p in journal.path.parent.iterdir())
                f.cloud.activity.clear()
                with self.assertRaisesRegex(mc.GuardError, 'invalid pinned intent identity'):
                    getattr(current, caller)()
                self.assertEqual(f.cloud.activity, [], 'schema validation must precede every provider method')
                self.assertEqual(journal.checkpoint, checkpoint)
                self.assertEqual(journal.path.read_bytes(), head)
                self.assertEqual(sorted(p.name for p in journal.path.parent.iterdir()), names)
                self.assertEqual(journal.read()['intents'][0]['request_sha256'], 1)
                self.assertEqual(journal.checkpoint, checkpoint)
                self.assertEqual(f.deletes(), [])

    def test_valid_reopened_records_reach_both_recovery_callers_and_real_cleanup(self):
        for caller in ('recover', 'cleanup_pass'):
            with self.subTest(caller=caller), Fixture() as f:
                backend(f)
                f.create()
                self.assertTrue(f.campaign.poll_actions())
                f.closing()
                journal = reopened(f)
                current = campaign(f, journal)
                checkpoint = copy.deepcopy(journal.checkpoint)
                f.cloud.activity.clear()
                result = getattr(current, caller)()
                self.assertTrue(f.cloud.activity)
                self.assertGreater(journal.checkpoint['generation'], checkpoint['generation'])
                if caller == 'recover':
                    result = current.cleanup_pass()
                self.assertTrue(result['complete'])
                self.assertTrue(result['baseline_equal'])
                self.assertEqual(f.deletes(), [('DELETE', '/networks/201')])
                self.assertEqual(journal.read()['mode'], 'COMPLETE')


class RecoveredIdentityTests(unittest.TestCase):
    def lost_server(self, f):
        backend(f)
        sequence = lose_create(self, f, 'server')
        original = copy.deepcopy(f.cloud.rows['server'][101])
        f.closing()
        return sequence, original

    def fail_resource_actions(self, f, sequence, original, failure):
        observer = reopened(f)
        observed = []
        def unavailable(kind, rid):
            self.assertEqual((kind, rid), ('server', 101))
            durable = observer.peek()
            intent = durable['intents'][sequence]
            self.assertEqual(intent['state'], 'UNKNOWN')
            self.assertEqual(intent['resource_id'], 101)
            self.assertEqual(intent['recovery']['candidate'], original)
            self.assertEqual(intent['recovery']['origin'], 'conditional-nonce-recovery')
            self.assertIsNone(intent['ack'])
            self.assertIsNone(intent['original_ack'])
            self.assertIs(intent['required_action_identified'], False)
            self.assertEqual(server_record(durable)['created'], original['created'])
            self.assertEqual(server_record(durable)['labels'], original['labels'])
            self.assertIsNone(observer.read_if_available(), 'identity publication must occur inside the real recovery lock')
            observed.append(copy.deepcopy(intent['recovery']))
            raise failure('synthetic server resource-actions endpoint unavailable')
        f.cloud.resource_error = unavailable
        return observed

    def test_response_lost_server_identity_is_durable_before_failed_actions_and_cleanup_still_deletes_it(self):
        for failure in (mc.GuardError, OSError):
            with self.subTest(failure=failure.__name__), Fixture() as f:
                sequence, original = self.lost_server(f)
                before = copy.deepcopy(f.journal.checkpoint)
                observed = self.fail_resource_actions(f, sequence, original, failure)
                f.campaign.recover()
                self.assertEqual(len(observed), 1)
                journal = reopened(f)
                state = journal.read()
                self.assertGreater(journal.checkpoint['generation'], before['generation'])
                self.assertEqual(server_record(state)['created'], original['created'])
                self.assertEqual(state['intents'][sequence]['state'], 'UNKNOWN')
                self.assertTrue(any(e.get('event') == 'recovery-action-read-unavailable'
                    and e.get('intent') == sequence and e.get('error_type') == failure.__name__ for e in state['events']))
                proof = copy.deepcopy(state['intents'][sequence]['recovery'])
                current = campaign(f, journal)
                result = current.cleanup_pass()
                self.assertGreaterEqual(len(observed), 2)
                self.assertEqual(f.deletes(), [('DELETE', '/servers/101'), ('DELETE', '/firewalls/301'), ('DELETE', '/ssh_keys/401')])
                self.assertFalse(result['complete'])
                self.assertTrue(result['baseline_equal'])
                self.assertEqual(result['owned_survivors'], [])
                self.assertEqual(result['subordinate_survivors'], 0)
                self.assertIn(sequence, result['pending_intents'])
                final = reopened(f).read()
                intent = final['intents'][sequence]
                self.assertEqual(intent['state'], 'UNKNOWN')
                self.assertEqual(intent['resource_id'], 101)
                self.assertEqual(intent['recovery'], proof)
                self.assertIsNone(intent['ack'])
                self.assertIsNone(intent['original_ack'])
                self.assertIs(intent['required_action_identified'], False)
                self.assertEqual(server_record(final)['created'], original['created'])
                self.assertEqual(len(f.cloud.requests), 6)

    def test_failed_action_lookup_cannot_rebase_recovered_identity_after_drift_and_original_restore(self):
        with Fixture() as f:
            sequence, original = self.lost_server(f)
            self.fail_resource_actions(f, sequence, original, mc.GuardError)
            f.campaign.recover()
            proof = copy.deepcopy(reopened(f).read()['intents'][sequence]['recovery'])
            f.cloud.rows['server'][101]['created'] = utc(T0 + 11 * NS)
            self.assertNotEqual(f.cloud.rows['server'][101]['created'], original['created'])
            f.campaign.recover()
            drifted = reopened(f).read()
            self.assertEqual(server_record(drifted)['created'], original['created'])
            self.assertEqual(drifted['intents'][sequence]['recovery'], proof)
            self.assertTrue(any(e.get('event') == 'observed-retirement' and e.get('kind') == 'server'
                and e.get('id') == 101 for e in drifted['events']))
            f.cloud.rows['server'][101] = copy.deepcopy(original)
            current = campaign(f, reopened(f))
            result = current.cleanup_pass()
            self.assertFalse(result['complete'])
            self.assertNotIn(('DELETE', '/servers/101'), f.deletes())
            self.assertIn(('DELETE', '/firewalls/301'), f.deletes())
            self.assertIn(('DELETE', '/ssh_keys/401'), f.deletes())
            final = reopened(f).read()
            self.assertEqual(server_record(final)['created'], original['created'])
            self.assertEqual(final['intents'][sequence]['recovery'], proof)
            self.assertEqual(final['intents'][sequence]['state'], 'UNKNOWN')
            self.assertIn(sequence, result['pending_intents'])

    def test_healthy_server_action_correlation_resolves_the_recovered_identity_and_completes_cleanup(self):
        with Fixture() as f:
            sequence, original = self.lost_server(f)
            f.campaign.recover()
            recovered = reopened(f).read()
            intent = recovered['intents'][sequence]
            self.assertEqual(intent['state'], 'ACKNOWLEDGED')
            self.assertIs(intent['required_action_identified'], True)
            self.assertEqual(intent['resource_id'], 101)
            self.assertEqual(intent['recovery']['candidate'], original)
            self.assertEqual(intent['actions'], [next(action['id'] for action in f.cloud.actions.values()
                if action['command'] == 'create_server')])
            self.assertIsNone(intent['ack'])
            self.assertIsNone(intent['original_ack'])
            self.assertTrue(f.campaign.poll_actions())
            result = f.campaign.cleanup_pass()
            self.assertTrue(result['complete'])
            self.assertTrue(result['baseline_equal'])
            self.assertTrue(result['hard_lifetime_met'])
            self.assertEqual(f.deletes(), [('DELETE', '/servers/101'), ('DELETE', '/firewalls/301'), ('DELETE', '/ssh_keys/401')])
            self.assertEqual(len(f.cloud.requests), 6)


if __name__ == '__main__':
    unittest.main()
