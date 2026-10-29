"""Exact-only completion absence must permanently retire a recorded primary IP."""
import contextlib
import copy
import os
import unittest
from unittest.mock import patch

import campaign_cloud as cc
import campaign_safety as cs
import creation_ledger as cl
import mn_common as mc
from test_campaign_safety import Fixture


KINDS = ('server', 'network', 'firewall', 'ssh-key', 'volume', 'primary-ip',
         'floating-ip', 'load-balancer', 'placement-group', 'certificate')
COUNTS = {'server': 3, 'network': 2, 'firewall': 2, 'ssh-key': 2, 'volume': 1,
          'primary-ip': 3, 'floating-ip': 1, 'load-balancer': 1,
          'placement-group': 1, 'certificate': 1}
OWNED = {('network', 201), ('firewall', 301), ('ssh-key', 401), ('server', 101), ('server', 102)}
ABSENCE_SOURCE = 'completion-exact'


def retirements(state):
    return [event for event in state['events'] if event.get('event') == 'observed-retirement']


class CompletionExactAbsenceTests(unittest.TestCase):
    def setUp(self):
        safety = contextlib.ExitStack()
        self.addCleanup(safety.close)
        safety.enter_context(patch.dict(os.environ, {}, clear=True))
        for owner, name in ((cc.http.client.HTTPSConnection, 'connect'),
                            (cs.os, 'pidfd_open'), (cs.signal, 'pidfd_send_signal')):
            safety.enter_context(patch.object(owner, name, create=True,
                side_effect=AssertionError('unexpected real network/process boundary: ' + name)))

    def prepare(self, f):
        self.assertEqual(f.create(), 201)
        self.assertEqual(f.create('server', 0), 101)
        self.assertEqual(f.create('server', 1), 102)
        with f.journal.locked() as state:
            for rid in (101, 102):
                reads = lambda: f.campaign._phase_budget(state)
                server = f.cloud.describe('server', rid, reads)
                ips = f.cloud.list('primary-ip', reads)
                actual = f.campaign.cascade(state, server, ips, reads)
                self.assertEqual([row['id'] for row in actual], [rid + 10000])
            f.journal.commit(state)
        self.assertTrue(f.campaign.poll_actions())
        f.closing()
        state = f.journal.read()
        self.assertEqual(state['mode'], 'CLOSING')
        self.assertEqual({(row['kind'], row['id']) for row in state['owned']}, OWNED)
        self.assertEqual({(row['server_id'], row['id']) for row in state['associations']},
                         {(101, 10101), (102, 10102)})
        self.assertEqual(len(state['associations']), 2)
        self.assertEqual(retirements(state), [])
        self.assertTrue(all(intent['state'] == 'DONE' and not intent.get('pending_actions')
                            for intent in state['intents']))
        self.assertEqual(len(f.cloud.requests), 5)
        self.assertEqual(f.deletes(), [])
        self.assertEqual(tuple(mc.PROTECT_KINDS), KINDS)
        self.assertEqual(set(f.cloud.rows), set(KINDS))
        snapshot = {kind: copy.deepcopy(list(f.cloud.rows[kind].values())) for kind in KINDS}
        self.assertEqual({kind: len(rows) for kind, rows in snapshot.items()}, COUNTS)
        self.assertEqual(sum(len(rows) for rows in snapshot.values()), 17)
        for rows in snapshot.values():
            self.assertTrue(all(type(row['id']) is int and row['id'] > 0 for row in rows))
        for kind, rid in OWNED | {('primary-ip', 10101), ('primary-ip', 10102)}:
            self.assertIn(rid, f.cloud.rows[kind], 'every owned resource and both attributed IPs must remain live')
        return state, snapshot

    def fresh_during_completion(self, f):
        # This reader never authorizes a mutation. Its newly opened authenticated
        # head distinguishes a durable publication from the caller's local state.
        with contextlib.closing(cl.Journal(f.journal.path, observe=True)) as reader:
            state = reader.peek()
            checkpoint = copy.deepcopy(reader.checkpoint)
            self.assertEqual(reader.binding, f.journal.binding)
            self.assertIsNone(reader.read_if_available(), 'completion must still hold its real operation lock')
            return state, checkpoint

    def observe_completion(self, f, absent):
        before, snapshot = self.prepare(f)
        checkpoint = copy.deepcopy(f.journal.checkpoint)
        head = f.journal.path.read_bytes()
        associations = copy.deepcopy(before['associations'])
        store = copy.deepcopy(f.cloud.rows)
        requests = copy.deepcopy(f.cloud.requests)
        hashes = {kind: cl.sha256(rows) for kind, rows in snapshot.items()}
        listed, described = f.cloud.list, f.cloud.describe
        census, exact, first_proofs, later_proofs = [], [], [], []
        describe_start = len(f.cloud.describes)

        def inventory(kind, seconds=20):
            rows = listed(kind, seconds)
            self.assertEqual(rows, snapshot[kind], 'completion census rows must remain positive and unchanged')
            self.assertEqual(cl.sha256(rows), hashes[kind])
            census.append((kind, copy.deepcopy(rows), cl.sha256(rows)))
            return rows

        def completion_exact(kind, rid, seconds=20):
            exact.append((kind, rid))
            if (kind, rid) == ('primary-ip', 10102):
                self.assertEqual(exact[-2:], [('primary-ip', 10101), ('primary-ip', 10102)])
                self.assertNotIn(('primary-ip', 10102),
                    [(k, identity) for k, identity, _ in f.cloud.describes[describe_start:]],
                    'durable retirement must be checked before the second exact provider read starts')
                durable, seen = self.fresh_during_completion(f)
                self.assertEqual(durable['associations'], associations)
                self.assertTrue(all(i['state'] == 'DONE' and not i.get('pending_actions') for i in durable['intents']))
                if absent:
                    self.assertEqual(retirements(durable), [dict(event='observed-retirement', kind='primary-ip',
                        id=10101, source=ABSENCE_SOURCE, unix_ns=f.wall)])
                    self.assertGreater(seen['generation'], checkpoint['generation'])
                else:
                    self.assertEqual(retirements(durable), [])
                    self.assertEqual(seen, checkpoint)
                    self.assertEqual(f.journal.path.read_bytes(), head)
                self.assertEqual(f.cloud.rows, store)
                self.assertEqual(f.cloud.requests, requests)
                later_proofs.append((copy.deepcopy(durable), seen))
            item = described(kind, rid, seconds)
            self.assertEqual(item, store[kind][rid], 'all exact metadata must be healthy before the one None override')
            if (kind, rid) == ('primary-ip', 10101):
                self.assertEqual({entry[0] for entry in census}, set(KINDS))
                self.assertEqual(len(census), 10)
                durable, seen = self.fresh_during_completion(f)
                self.assertEqual(retirements(durable), [], 'no earlier census or other observation may retire this IP')
                self.assertEqual(durable['associations'], associations)
                self.assertEqual(seen, checkpoint)
                self.assertEqual(f.journal.path.read_bytes(), head)
                first_proofs.append(copy.deepcopy(durable))
                if absent:
                    return None
            return item

        with patch.object(f.cloud, 'list', side_effect=inventory), \
             patch.object(f.cloud, 'describe', side_effect=completion_exact):
            result = f.campaign.completion()
        self.assertEqual(len(first_proofs), 1)
        self.assertEqual(len(later_proofs), 1)
        self.assertEqual({kind for kind, _, _ in census}, set(KINDS))
        self.assertEqual(len(census), 10)
        self.assertEqual({kind: len(rows) for kind, rows, _ in census}, COUNTS)
        self.assertEqual({kind: digest for kind, _, digest in census}, hashes)
        self.assertEqual(set(exact), OWNED | {('primary-ip', 10101), ('primary-ip', 10102)})
        self.assertEqual(len(exact), 7)
        self.assertFalse(result['complete'])
        self.assertFalse(result['baseline_equal'])
        self.assertEqual({(row['kind'], row['id']) for row in result['owned_survivors']}, OWNED)
        self.assertEqual(result['subordinate_survivors'], 2)
        self.assertEqual(result['pending_intents'], [])
        self.assertEqual(result.get('unresolved_readbacks', []), [])
        self.assertEqual(f.cloud.rows, store)
        self.assertEqual(f.cloud.requests, requests)
        self.assertEqual(f.deletes(), [])
        with contextlib.closing(cl.Journal(f.journal.path)) as reader:
            durable = reader.read()
            self.assertEqual(durable['associations'], associations)
            if absent:
                self.assertEqual(retirements(durable), [dict(event='observed-retirement', kind='primary-ip',
                    id=10101, source=ABSENCE_SOURCE, unix_ns=f.wall)])
            else:
                self.assertEqual(retirements(durable), [])
        return store, associations

    def test_exact_only_none_retires_durably_before_next_read_and_restoration_never_restores_delete_authority(self):
        with Fixture() as f:
            store, associations = self.observe_completion(f, absent=True)
            self.assertEqual(f.cloud.describe('primary-ip', 10101), store['primary-ip'][10101])
            self.assertEqual(f.cloud.describe('server', 101), store['server'][101])
            with contextlib.closing(cl.Journal(f.journal.path)) as journal:
                current = cs.Campaign(journal, f.cloud, clock=lambda: f.wall, monotonic=lambda: f.mono)
                before = copy.deepcopy(journal.checkpoint)
                requests = copy.deepcopy(f.cloud.requests)
                with self.assertRaisesRegex(mc.GuardError, 'observed-retired IP cannot inherit association'):
                    current.mutate('server', 101, 'delete')
                self.assertEqual(journal.checkpoint, before)
                self.assertEqual(f.cloud.requests, requests, 'retired-IP refusal must happen before DELETE dispatch')
                self.assertEqual(f.cloud.rows['server'][101], store['server'][101])
                self.assertEqual(f.cloud.rows['primary-ip'][10101], store['primary-ip'][10101])
                result = current.cleanup_pass()
                self.assertFalse(result['complete'])
                self.assertIn(('DELETE', '/servers/102'), f.deletes())
                self.assertIn(('DELETE', '/networks/201'), f.deletes())
                self.assertNotIn(('DELETE', '/servers/101'), f.deletes())
                self.assertIn(dict(kind='server', id=101), result['owned_survivors'])
                self.assertEqual(result['subordinate_survivors'], 1)
                self.assertEqual(result['pending_intents'], [])
                self.assertEqual(result.get('unresolved_readbacks', []), [])
                durable = journal.read()
                target = [e for e in retirements(durable) if e['kind'] == 'primary-ip' and e['id'] == 10101]
                self.assertEqual(target, [dict(event='observed-retirement', kind='primary-ip', id=10101,
                    source=ABSENCE_SOURCE, unix_ns=f.wall)])
                self.assertFalse(any(e['kind'] == 'server' and e['id'] == 101 for e in retirements(durable)))
                self.assertEqual(durable['associations'], associations)
            self.assertEqual(f.cloud.rows['server'][101], store['server'][101])
            self.assertEqual(f.cloud.rows['primary-ip'][10101], store['primary-ip'][10101])
            self.assertEqual(f.cloud.rows['server'].get(102), None)
            self.assertEqual(f.cloud.rows['primary-ip'].get(10102), None)
            self.assertEqual(f.cloud.rows['network'].get(201), None)

    def test_unchanged_positive_exact_reads_do_not_retire_and_allow_real_server_delete_and_complete_cleanup(self):
        with Fixture() as f:
            self.observe_completion(f, absent=False)
            with contextlib.closing(cl.Journal(f.journal.path)) as journal:
                current = cs.Campaign(journal, f.cloud, clock=lambda: f.wall, monotonic=lambda: f.mono)
                self.assertEqual(retirements(journal.read()), [])
                current.mutate('server', 101, 'delete')
                self.assertEqual(f.deletes(), [('DELETE', '/servers/101')])
                self.assertNotIn(101, f.cloud.rows['server'])
                self.assertNotIn(10101, f.cloud.rows['primary-ip'])
                self.assertTrue(current.poll_actions())
                result = current.cleanup_pass()
                self.assertTrue(result['complete'])
                self.assertTrue(result['baseline_equal'])
                self.assertTrue(result['hard_lifetime_met'])
                self.assertEqual(result['owned_survivors'], [])
                self.assertEqual(result['subordinate_survivors'], 0)
                self.assertEqual(result['pending_intents'], [])
                self.assertEqual(current.terminal_result(), result)
                self.assertEqual(journal.read()['mode'], 'COMPLETE')
            self.assertEqual(set(f.deletes()), {('DELETE', '/servers/101'), ('DELETE', '/servers/102'),
                ('DELETE', '/firewalls/301'), ('DELETE', '/ssh_keys/401'), ('DELETE', '/networks/201')})
            self.assertEqual(len(f.deletes()), 5)
            self.assertEqual(len(f.cloud.requests), 10)


if __name__ == '__main__':
    unittest.main()
