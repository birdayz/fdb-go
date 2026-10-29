"""Durable owned-absence observations before subsequent read/refusal failures.

Campaign, Journal, pinning, ownership, retirement and cleanup are real. Only the
provider/process/clock boundaries supplied by the synthetic Fixture are injected.
"""
import contextlib
import copy
import unittest
from unittest.mock import patch

import campaign_cloud as cc
import campaign_safety as cs
import creation_ledger as cl
import mn_common as mc
from test_campaign_safety import Fixture


class CampaignRetirementTest(unittest.TestCase):
    def setUp(self):
        self.stack = contextlib.ExitStack()
        self.addCleanup(self.stack.close)
        self.stack.enter_context(patch.object(cc.http.client.HTTPSConnection, 'connect',
            side_effect=AssertionError('retirement controls must never open a real network connection')))
        self.stack.enter_context(patch.object(cs.os, 'pidfd_open', create=True,
            side_effect=AssertionError('retirement controls must never open a real pidfd')))
        self.stack.enter_context(patch.object(cs.signal, 'pidfd_send_signal', create=True,
            side_effect=AssertionError('retirement controls must never signal a real process')))

    def fresh_state(self, fixture):
        with contextlib.closing(cl.Journal(fixture.journal.path)) as reader:
            return reader.read()

    def assert_retirement(self, state, kind, rid, source):
        rows = [event for event in state['events'] if event.get('event') == 'observed-retirement'
                and event.get('kind') == kind and event.get('id') == rid]
        self.assertEqual(len(rows), 1, 'one durable permanent typed retirement is required')
        self.assertIs(type(rows[0]['id']), int)
        self.assertEqual(rows[0]['source'], source)
        self.assertTrue(any(record['kind'] == kind and record['id'] == rid for record in state['owned']),
                        'absence must retain creation provenance, not discard ownership evidence')
        return rows[0]

    def create_synchronous_pair(self, fixture):
        network = fixture.create()
        ssh = fixture.create('ssh-key')
        self.assertEqual((network, ssh), (201, 401))
        self.assertTrue(fixture.campaign.poll_actions())
        return network, ssh

    def fail_after_completed_network_census(self, fixture, callback):
        observations = []
        network_rows = fixture.cloud.rows['network']
        self.assertNotIn(201, network_rows)
        with contextlib.closing(cl.Journal(fixture.journal.path)) as reader:
            def next_kind(kind, seconds):
                if kind == 'firewall':
                    # This is before the next provider read fails, not a read of
                    # uncommitted caller state after Campaign's context unwinds.
                    observations.append(reader.peek())
                    raise mc.GuardError('synthetic later complete-kind census unavailable')
            with patch.object(fixture.cloud, 'before_list', side_effect=next_kind):
                with self.assertRaisesRegex(mc.GuardError, 'later complete-kind census unavailable'):
                    callback()
        self.assertEqual(len(observations), 1)
        return observations[0]

    def assert_identical_reappearance_refused(self, fixture, original, rid=201):
        fixture.cloud.rows['network'][rid] = copy.deepcopy(original)
        self.assertEqual(fixture.cloud.rows['network'][rid], original)
        before = copy.deepcopy(fixture.cloud.requests)
        with self.assertRaisesRegex(mc.GuardError, 'observed-retired ID cannot inherit ownership'):
            fixture.campaign.mutate('network', rid, 'delete')
        self.assertEqual(fixture.cloud.requests, before)
        self.assertIn(rid, fixture.cloud.rows['network'])

    def assert_cleanup_keeps_retired_network_and_deletes_unrelated(self, fixture, original, expected_deletes):
        before = len(fixture.cloud.requests)
        result = fixture.campaign.cleanup_pass()
        sent = [(method, path) for method, path, _, _ in fixture.cloud.requests[before:] if method == 'DELETE']
        self.assertEqual(sent, expected_deletes, 'unrelated cleanup must really execute, not just refuse earlier')
        self.assertNotIn(('DELETE', '/networks/201'), fixture.deletes())
        self.assertEqual(fixture.cloud.rows['network'][201], original)
        self.assertFalse(result['complete'])
        self.assertFalse(result['baseline_equal'])
        self.assertEqual(result['owned_survivors'], [dict(kind='network', id=201)])
        self.assertEqual(result['pending_intents'], [])
        return self.fresh_state(fixture)

    def test_create_census_retires_completed_kind_before_later_kind_read_failure(self):
        with Fixture() as f:
            network, ssh = self.create_synchronous_pair(f)
            original = f.cloud.rows['network'].pop(network)
            before = copy.deepcopy(f.cloud.requests)
            checkpoint = f.journal.checkpoint
            observed = self.fail_after_completed_network_census(f, lambda: f.create('firewall'))
            self.assert_retirement(observed, 'network', network, 'pre-create-census')
            durable = self.fresh_state(f)
            self.assert_retirement(durable, 'network', network, 'pre-create-census')
            self.assertEqual([(event['kind'], event['id']) for event in durable['events']
                              if event.get('event') == 'observed-retirement'], [('network', network)])
            self.assertGreater(f.journal.checkpoint['generation'], checkpoint['generation'])
            self.assertEqual(f.cloud.requests, before)
            self.assertEqual(len(durable['intents']), 2, 'failed census must not prepare or dispatch a new create')
            self.assertIsNone(durable['active_lease'])
            self.assertIn(ssh, f.cloud.rows['ssh-key'])
            f.closing()
            self.assert_identical_reappearance_refused(f, original)
            final = self.assert_cleanup_keeps_retired_network_and_deletes_unrelated(
                f, original, [('DELETE', '/ssh_keys/401')])
            self.assert_retirement(final, 'network', network, 'pre-create-census')

    def test_recovery_census_retires_completed_kind_before_later_kind_read_failure(self):
        with Fixture() as f:
            network, ssh = self.create_synchronous_pair(f)
            f.closing()
            original = f.cloud.rows['network'].pop(network)
            before = copy.deepcopy(f.cloud.requests)
            observed = self.fail_after_completed_network_census(f, f.campaign.recover)
            self.assert_retirement(observed, 'network', network, 'recovery-census')
            durable = self.fresh_state(f)
            self.assert_retirement(durable, 'network', network, 'recovery-census')
            self.assertEqual([(event['kind'], event['id']) for event in durable['events']
                              if event.get('event') == 'observed-retirement'], [('network', network)])
            self.assertEqual(f.cloud.requests, before)
            self.assertIn(ssh, f.cloud.rows['ssh-key'])
            self.assert_identical_reappearance_refused(f, original)
            final = self.assert_cleanup_keeps_retired_network_and_deletes_unrelated(
                f, original, [('DELETE', '/ssh_keys/401')])
            self.assert_retirement(final, 'network', network, 'recovery-census')

    def test_attach_absent_owned_network_is_durably_retired_before_refusal(self):
        with Fixture() as f:
            network = f.create()
            server = f.create('server')
            self.assertEqual((network, server), (201, 101))
            self.assertTrue(f.campaign.poll_actions())
            original = f.cloud.rows['network'].pop(network)
            before = copy.deepcopy(f.cloud.requests)
            f.cloud.describes.clear()
            plan = next(entry for entry in f.header['plan'] if entry['name'] == 'test-node0')
            with self.assertRaisesRegex(mc.GuardError, 'attach network missing'):
                f.campaign.mutate('server', server, 'attach', dict(network=network, ip=plan['private_ip']))
            self.assertEqual([(kind, rid) for kind, rid, _ in f.cloud.describes],
                             [('server', server), ('network', network)])
            durable = self.fresh_state(f)
            self.assert_retirement(durable, 'network', network, 'attach-network-readback')
            self.assertEqual(f.cloud.requests, before)
            self.assertEqual(len(durable['intents']), 4)
            self.assertIsNone(durable['active_lease'])
            f.closing()
            self.assert_identical_reappearance_refused(f, original)
            final = self.assert_cleanup_keeps_retired_network_and_deletes_unrelated(f, original,
                [('DELETE', '/servers/101'), ('DELETE', '/firewalls/301'), ('DELETE', '/ssh_keys/401')])
            self.assert_retirement(final, 'network', network, 'attach-network-readback')

    def test_known_recovery_exact_404_persists_before_later_intent_lookup_failure(self):
        with Fixture() as f:
            network, ssh = self.create_synchronous_pair(f)
            f.closing()
            # Model an interrupted acknowledged synchronous create: its existing
            # resource/ACK provenance survives, but completion is unresolved.
            with f.journal.locked() as state:
                for intent in state['intents']:
                    self.assertEqual(intent['actions'], [])
                    intent['state'] = 'UNKNOWN'
                f.journal.commit(state)
            original = copy.deepcopy(f.cloud.rows['network'][network])
            before = copy.deepcopy(f.cloud.requests)
            reads, failures = [], []
            describe = f.cloud.describe
            with contextlib.closing(cl.Journal(f.journal.path)) as reader:
                def exact(kind, rid, seconds=20):
                    reads.append((kind, rid))
                    if (kind, rid) == ('ssh-key', ssh):
                        if callable(seconds):
                            seconds()
                        failures.append(reader.peek())
                        raise mc.GuardError('synthetic later exact resource lookup unavailable')
                    result = describe(kind, rid, seconds)
                    if (kind, rid) == ('network', network):
                        self.assertEqual(result, original, 'complete census remains present; only exact read observes404')
                        return None
                    return result
                with patch.object(f.cloud, 'describe', side_effect=exact):
                    with self.assertRaisesRegex(mc.GuardError, 'later exact resource lookup unavailable'):
                        f.campaign.recover()
            self.assertEqual(reads, [('network', network), ('ssh-key', ssh)])
            self.assertEqual(len(failures), 1)
            self.assert_retirement(failures[0], 'network', network, 'known-recovery-readback')
            durable = self.fresh_state(f)
            self.assert_retirement(durable, 'network', network, 'known-recovery-readback')
            self.assertEqual([intent['state'] for intent in durable['intents']], ['UNKNOWN', 'UNKNOWN'])
            self.assertEqual(f.cloud.requests, before)
            self.assert_identical_reappearance_refused(f, original)
            result = f.campaign.cleanup_pass()
            self.assertFalse(result['complete'])
            self.assertEqual([(method, path) for method, path, _, _ in f.cloud.requests[len(before):]],
                             [('DELETE', '/ssh_keys/401')])
            self.assertNotIn(('DELETE', '/networks/201'), f.deletes())
            self.assertEqual(f.cloud.rows['network'][network], original)
            self.assertNotIn(ssh, f.cloud.rows['ssh-key'])
            self.assert_retirement(self.fresh_state(f), 'network', network, 'known-recovery-readback')


if __name__ == '__main__':
    unittest.main()
