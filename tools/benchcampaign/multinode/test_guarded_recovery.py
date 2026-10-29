#!/usr/bin/env python3
"""Actual guard-led journal crash repair followed by exact-ID cloud cleanup.

Only provider/process/clock boundaries and selected filesystem faults are fake.
The interrupted calls, durable orphans, authenticated repair, rearming, cascade
checks, action reads, deletions and completion all use production methods.
"""
import contextlib
import copy
import fcntl
import hashlib
import hmac
import io
import json
import os
from pathlib import Path
import signal
import stat
import sys
import unittest
from unittest import mock

HERE = Path(os.path.abspath(__file__)).parent
sys.path.insert(0, str(HERE))

import campaign_cloud as cc  # noqa: E402
import campaign_safety as cs  # noqa: E402
import creation_ledger as cl  # noqa: E402
import mn_common as mc  # noqa: E402
import teardown_guard as tg  # noqa: E402
from test_campaign_safety import Fixture, INVENTORY_KINDS  # noqa: E402


class GuardedRecoveryTest(unittest.TestCase):
    def setUp(self):
        # recover_closed_journal constructs a real Profile with explicit fixture
        # context/config. Never allow a controller HCLOUD_* override into it.
        environment = mock.patch.dict(os.environ, {}, clear=True)
        environment.start()
        self.addCleanup(environment.stop)
        for target, name in ((os, 'pidfd_open'), (signal, 'pidfd_send_signal'),
                             (cc.http.client, 'HTTPSConnection')):
            boundary = mock.patch.object(target, name, side_effect=AssertionError(
                'unexpected real process/network boundary: ' + name))
            boundary.start()
            self.addCleanup(boundary.stop)
        self.f = Fixture()
        self.addCleanup(self.f.close)
        self.original_header = copy.deepcopy(self.f.header)
        self.original_binding = copy.deepcopy(self.f.journal.binding)
        self.path = self.f.journal.path
        self.evidence = self.f.root / 'recovery-evidence'

    def register_recovery_guard(self, producer_gone=True, guard_gone=True):
        f = self.f
        if producer_gone:
            f.processes.pop(111, None)
        if guard_gone:
            f.processes.pop(112, None)
        f.processes[113] = dict(pid=113, parent=110, starttime='1130', argv_sha256=cc.digest(b'113'))
        f.actor = 113

    def crash_snapshot(self):
        f = self.f
        self.assertTrue(f.journal._poisoned)
        headed_raw = self.path.read_bytes()
        headed = json.loads(headed_raw)
        orphan_path = self.path.parent / f'generation-{headed["generation"] + 1:020d}.json'
        self.assertTrue(orphan_path.is_file())
        orphan_raw = orphan_path.read_bytes()
        orphan = json.loads(orphan_raw)
        self.assertEqual(orphan['previous_sha256'], cl.sha256(headed))
        self.assertEqual(orphan['generation'], headed['generation'] + 1)
        self.assertEqual(orphan['uuid'], headed['uuid'])
        self.assertEqual(orphan['header_sha256'], headed['header_sha256'])
        self.assertEqual(orphan_raw, cl.canonical_json(orphan) + b'\n')
        self.assertEqual(stat.S_IMODE(orphan_path.stat().st_mode), 0o600)
        adverse = {self.path.name: headed_raw, cl.ANCHOR: (self.path.parent / cl.ANCHOR).read_bytes(),
                   orphan_path.name: orphan_raw}
        for temporary in self.path.parent.glob('.head-*'):
            adverse[temporary.name] = temporary.read_bytes()
        f.journal.close()
        return dict(headed=headed, orphan=orphan, orphan_path=orphan_path, adverse=adverse)

    def crash_final_ack(self):
        f = self.f
        for kind, body in f.bodies[:6]:
            f.campaign.create(kind, copy.deepcopy(body))
        self.assertEqual(len(f.journal.read()['owned']), 6)
        self.assertTrue(f.campaign.poll_actions())
        self.assertTrue(all(intent['state'] == 'DONE' for intent in f.journal.read()['intents']))
        self.assertEqual(len(f.cloud.requests), 6)
        self.assertEqual(f.cloud.next_ids['server'], 104)
        real_fsync = os.fsync
        after_dispatch = False
        acknowledgement_syncs = []
        def dispatched(method, path, body, seconds):
            nonlocal after_dispatch
            self.assertEqual((method, path, body['name']), ('POST', '/servers', 'test-node3'))
            after_dispatch = True
        def sync(fd):
            if after_dispatch:
                acknowledgement_syncs.append(stat.S_IFMT(os.fstat(fd).st_mode))
                if len(acknowledgement_syncs) == 2:
                    self.assertTrue(stat.S_ISDIR(os.fstat(fd).st_mode))
                    raise OSError('synthetic final ACK generation-directory fsync failure')
            return real_fsync(fd)
        f.cloud.before_request = dispatched
        with mock.patch.object(cl.os, 'fsync', side_effect=sync):
            with self.assertRaisesRegex(OSError, 'final ACK generation-directory fsync'):
                f.campaign.create('server', f.body('server', 3))
        f.cloud.before_request = None
        self.assertEqual(acknowledgement_syncs, [stat.S_IFREG, stat.S_IFDIR])
        self.assertEqual(len(f.cloud.requests), 7)
        self.assertEqual(f.cloud.requests[-1][0:2], ('POST', '/servers'))
        self.assertIn(104, f.cloud.rows['server'])
        self.assertIn(10104, f.cloud.rows['primary-ip'])
        crash = self.crash_snapshot()
        self.assertEqual(crash['headed']['state']['intents'][-1]['state'], 'SENDING')
        self.assertEqual(len(crash['headed']['state']['owned']), 6)
        candidate = crash['orphan']['state']
        self.assertEqual(len(candidate['owned']), 7)
        intent = candidate['intents'][-1]
        self.assertEqual((intent['state'], intent['resource_id']), ('ACKNOWLEDGED', 104))
        self.assertEqual(intent['ack']['status'], 201)
        self.assertEqual(intent['ack']['payload']['server']['id'], 104)
        self.assertEqual(intent['actions'], [intent['ack']['payload']['action']['id']])
        return crash

    def crash_sending_before_http(self):
        f = self.f
        real_fsync = os.fsync
        synced = []
        def sync(fd):
            synced.append(stat.S_IFMT(os.fstat(fd).st_mode))
            # PREPARED fully publishes with four syncs. SENDING's generation
            # file is next fsynced, then its directory fails before publication.
            if len(synced) == 6:
                self.assertTrue(stat.S_ISDIR(os.fstat(fd).st_mode))
                raise OSError('synthetic SENDING generation-directory fsync failure')
            return real_fsync(fd)
        with mock.patch.object(cl.os, 'fsync', side_effect=sync):
            with self.assertRaisesRegex(OSError, 'SENDING generation-directory fsync'):
                f.campaign.create('network', f.body('network'))
        self.assertEqual(synced, [stat.S_IFREG, stat.S_IFDIR, stat.S_IFREG, stat.S_IFDIR,
                                  stat.S_IFREG, stat.S_IFDIR])
        self.assertEqual(f.cloud.requests, [])
        crash = self.crash_snapshot()
        self.assertEqual(crash['headed']['state']['intents'][0]['state'], 'PREPARED')
        self.assertEqual(crash['orphan']['state']['intents'][0]['state'], 'SENDING')
        self.assertIsNone(crash['orphan']['state']['intents'][0]['ack'])
        self.assertIsNone(crash['orphan']['state']['intents'][0]['resource_id'])
        return crash

    def repair(self):
        # Keep this explicit even though setUp already clears the environment;
        # no follow-up edit may accidentally reintroduce ambient credential use.
        with mock.patch.dict(os.environ, {}, clear=True):
            journal = cs.recover_closed_journal(self.path, self.evidence)
        self.addCleanup(journal.close)
        self.f.journal = journal
        self.f.campaign = cs.Campaign(journal, self.f.cloud, clock=lambda: self.f.wall,
                                      monotonic=lambda: self.f.mono)
        self.assertEqual(journal.header, self.original_header)
        self.assertEqual(journal.header['time'], self.original_header['time'])
        self.assertEqual(journal.binding, self.original_binding)
        self.assertEqual(journal.read()['mode'], 'CLOSING')
        self.assertIsNone(journal.read()['active_lease'])
        return journal

    def assert_archive(self, crash, journal):
        events = [event for event in journal.read()['events'] if event['event'] == 'journal-recovery-closed']
        self.assertEqual(len(events), 1)
        receipt = events[0]['evidence']
        archive = Path(receipt['archive'])
        self.assertEqual(archive.parent, self.evidence)
        self.assertEqual(receipt['candidate_source'], 'orphan')
        self.assertEqual(receipt['headed_checkpoint'], dict(generation=crash['headed']['generation'],
                                                           sha256=cl.sha256(crash['headed'])))
        self.assertEqual(receipt['orphan_checkpoint'], dict(generation=crash['orphan']['generation'],
                                                           sha256=cl.sha256(crash['orphan'])))
        self.assertEqual({record['source'] for record in receipt['files']}, set(crash['adverse']))
        for record in receipt['files']:
            original = crash['adverse'][record['source']]
            archived = archive / record['archive_name']
            self.assertEqual(archived.read_bytes(), original)
            self.assertEqual(record['sha256'], hashlib.sha256(original).hexdigest())
            self.assertEqual(record['size'], len(original))
            self.assertEqual(stat.S_IMODE(archived.stat().st_mode), 0o600)
        manifest_raw = (archive / 'manifest.json').read_bytes()
        self.assertEqual(receipt['manifest_sha256'], hashlib.sha256(manifest_raw).hexdigest())
        manifest = json.loads(manifest_raw)
        signature = manifest.pop('mac')
        key = (self.path.parent / cl.KEY).read_bytes()
        self.assertEqual(signature, hmac.new(key, cl.canonical_json(manifest), hashlib.sha256).hexdigest())
        self.assertEqual(manifest['authorization']['recovery_guard']['pid'], 113)
        self.assertTrue(manifest['authorization']['producer_quiesced'])
        self.assertTrue(manifest['authorization']['old_guard_gone'])
        self.assertEqual(manifest['authorization']['immutable_time'], self.original_header['time'])
        self.assertEqual(stat.S_IMODE(archive.stat().st_mode), 0o700)
        self.assertEqual(stat.S_IMODE(self.evidence.stat().st_mode), 0o700)
        self.assertFalse((self.path.parent / cl.RECOVERY).exists())

    def assert_no_recovery_mutation(self, requests):
        self.assertEqual(self.f.cloud.requests, requests)
        self.assertEqual(self.f.deletes(), [])

    def test_orphan_seventh_ack_repairs_then_really_deletes_all_owned_resources(self):
        f = self.f
        crash = self.crash_final_ack()
        before = copy.deepcopy(f.cloud.requests)
        self.register_recovery_guard()
        journal = self.repair()
        self.assert_no_recovery_mutation(before)
        state = journal.read()
        self.assertEqual(len(state['owned']), 7)
        self.assertEqual({r['id'] for r in state['owned'] if r['kind'] == 'server'}, {101, 102, 103, 104})
        interrupted = state['intents'][-1]
        original = crash['orphan']['state']['intents'][-1]
        self.assertEqual(interrupted['state'], 'UNKNOWN')
        self.assertEqual(interrupted['resource_id'], 104)
        self.assertEqual(interrupted['ack'], original['ack'])
        self.assertEqual(interrupted['actions'], original['actions'])
        self.assert_archive(crash, journal)

        armed = f.campaign.arm_guard()
        self.assertEqual(armed['identity']['pid'], 113)
        result = f.campaign.cleanup_pass()
        self.assertTrue(result['complete'])
        self.assertTrue(result['baseline_equal'])
        self.assertTrue(result['hard_lifetime_met'])
        self.assertEqual(result['pending_intents'], [])
        self.assertEqual(result['owned_survivors'], [])
        self.assertEqual(result['subordinate_survivors'], 0)
        self.assertEqual(f.deletes(), [('DELETE', f'/servers/{rid}') for rid in (101, 102, 103, 104)] +
                         [('DELETE', '/firewalls/301'), ('DELETE', '/ssh_keys/401'), ('DELETE', '/networks/201')])
        self.assertEqual(len(f.cloud.requests), 14)  # seven original POSTs, seven exact DELETEs, no replay
        for rid in (10101, 10102, 10103, 10104):
            self.assertNotIn(rid, f.cloud.rows['primary-ip'])
        for kind in INVENTORY_KINDS:
            self.assertEqual(sorted(f.cloud.rows[kind]), f.header['baseline'][kind])
        terminal = journal.read()
        self.assertEqual(terminal['mode'], 'COMPLETE')
        self.assertEqual(terminal['intents'][6]['ack'], original['ack'])
        self.assertEqual(terminal['intents'][6]['actions'], original['actions'])
        self.assertEqual(journal.header, self.original_header)

    def test_empty_censuses_preserve_unknown_and_permanently_retire_previously_owned_ids(self):
        f = self.f
        crash = self.crash_final_ack()
        before = copy.deepcopy(f.cloud.requests)
        self.register_recovery_guard()
        journal = self.repair()
        self.assert_archive(crash, journal)
        interrupted = journal.read()['intents'][-1]
        visible = copy.deepcopy(f.cloud.rows)
        for kind in INVENTORY_KINDS:
            f.cloud.rows[kind] = {rid: row for rid, row in visible[kind].items()
                                  if rid in f.header['baseline'][kind]}
        for _ in range(2):
            result = f.campaign.completion()
            self.assertFalse(result['complete'])
            self.assertTrue(result['baseline_equal'])
            self.assertEqual(result['owned_survivors'], [])
            self.assertEqual(result['subordinate_survivors'], 0)
            self.assertEqual(result['pending_intents'], [interrupted['sequence']])
            self.assertEqual(journal.read()['intents'][-1]['state'], 'UNKNOWN')
        retired = [e for e in journal.read()['events'] if e['event'] == 'observed-retirement']
        self.assertEqual({(e['kind'], e['id']) for e in retired},
                         {('network', 201), ('firewall', 301), ('ssh-key', 401),
                          *(('server', rid) for rid in (101, 102, 103, 104)),
                          *(('primary-ip', rid) for rid in (10101, 10102, 10103, 10104))})
        self.assertEqual(len(retired), 11)
        f.cloud.rows = visible
        self.assert_no_recovery_mutation(before)
        f.campaign.arm_guard()
        result = f.campaign.cleanup_pass()
        self.assertFalse(result['complete'])
        self.assertEqual(f.cloud.requests, before)
        self.assertEqual(f.deletes(), [])
        self.assertEqual(len(journal.read()['owned']), 7)
        self.assertEqual(len([e for e in journal.read()['events'] if e['event'] == 'observed-retirement']), 11)
        self.assertEqual(sorted(f.cloud.rows['server']), sorted(visible['server']))

    def test_orphan_sending_before_http_repairs_not_dispatched_and_really_completes(self):
        f = self.f
        crash = self.crash_sending_before_http()
        self.register_recovery_guard()
        journal = self.repair()
        self.assert_archive(crash, journal)
        intent = journal.read()['intents'][0]
        self.assertEqual(intent['state'], 'NOT_DISPATCHED')
        self.assertIsNone(intent['ack'])
        self.assertIsNone(intent['resource_id'])
        self.assertEqual(journal.read()['owned'], [])
        self.assertEqual(f.cloud.requests, [])
        f.campaign.arm_guard()
        result = f.campaign.cleanup_pass()
        self.assertTrue(result['complete'])
        self.assertTrue(result['baseline_equal'])
        self.assertTrue(result['hard_lifetime_met'])
        self.assertEqual(result['pending_intents'], [])
        self.assertEqual(f.cloud.requests, [])
        self.assertEqual(f.deletes(), [])
        self.assertEqual(journal.read()['mode'], 'COMPLETE')
        self.assertEqual(journal.read()['intents'][0]['state'], 'NOT_DISPATCHED')
        for kind in INVENTORY_KINDS:
            self.assertEqual(sorted(f.cloud.rows[kind]), f.header['baseline'][kind])

    def assert_known_target_mutation_repairs_before_http(self, operation):
        f = self.f
        f.create_all()
        self.assertTrue(f.campaign.poll_actions())
        self.assertEqual(len(f.cloud.requests), 7)
        kind, rid = ('network', 201) if operation in {'delete', 'subnet'} else ('server', 101)
        body = None
        if operation == 'delete':
            f.closing()
        elif operation == 'subnet':
            body = copy.deepcopy(f.header['subnet'])
        elif operation == 'attach':
            address = next(p['private_ip'] for p in f.header['plan'] if p['name'] == 'test-node0')
            body = dict(network=201, ip=address)
        else:
            self.assertEqual(operation, 'poweron')
            body = {}
        state = f.journal.read()
        self.assertEqual(len(state['intents']), 7)
        self.assertTrue(all(intent['state'] == 'DONE' for intent in state['intents']))
        owned = copy.deepcopy(state['owned'])
        self.assertEqual(len(owned), 7)
        self.assertEqual(len([r for r in owned if (r['kind'], r['id']) == (kind, rid)]), 1)
        sequence = len(state['intents'])
        checkpoint = copy.deepcopy(f.journal.checkpoint)
        requests = copy.deepcopy(f.cloud.requests)
        rows = copy.deepcopy(f.cloud.rows)
        actions = copy.deepcopy(f.cloud.actions)
        next_action = f.cloud.next_action
        real_fsync = os.fsync
        synced = []
        def sync(fd):
            synced.append(stat.S_IFMT(os.fstat(fd).st_mode))
            # Unlike create, mutate has no PREPARED publication. Its first
            # SENDING generation retains the already-owned target ID.
            if len(synced) == 2:
                self.assertTrue(stat.S_ISDIR(os.fstat(fd).st_mode))
                raise OSError('synthetic mutation SENDING generation-directory fsync failure')
            return real_fsync(fd)
        with mock.patch.object(cl.os, 'fsync', side_effect=sync):
            with self.assertRaisesRegex(OSError, 'mutation SENDING generation-directory fsync'):
                f.campaign.mutate(kind, rid, operation, body)
        self.assertEqual(synced, [stat.S_IFREG, stat.S_IFDIR])
        self.assert_no_recovery_mutation(requests)
        self.assertEqual(f.cloud.rows, rows)
        self.assertEqual(f.cloud.actions, actions)
        self.assertEqual(f.cloud.next_action, next_action)
        crash = self.crash_snapshot()
        self.assertEqual(crash['headed']['generation'], checkpoint['generation'])
        self.assertEqual(cl.sha256(crash['headed']), checkpoint['sha256'])
        self.assertEqual(crash['headed']['state'], state)
        self.assertEqual(len(crash['headed']['state']['intents']), sequence)
        candidate = crash['orphan']['state']
        self.assertEqual(len(candidate['intents']), sequence + 1)
        self.assertEqual(candidate['owned'], owned)
        interrupted = candidate['intents'][sequence]
        self.assertEqual((interrupted['sequence'], interrupted['operation'], interrupted['kind'],
                          interrupted['resource_id'], interrupted['state']),
                         (sequence, operation, kind, rid, 'SENDING'))
        self.assertIsNone(interrupted['ack'])
        self.assertEqual(interrupted['actions'], [])
        self.assertEqual(interrupted['send_lease']['intent'], sequence)
        self.assertEqual(interrupted['send_generation'], crash['orphan']['generation'])

        self.register_recovery_guard()
        journal = self.repair()  # Uses production recover_closed_journal/close_state.
        self.assert_archive(crash, journal)
        repaired = journal.read()
        expected = copy.deepcopy(interrupted)
        expected['state'] = 'NOT_DISPATCHED'
        self.assertEqual(repaired['intents'][sequence], expected)
        self.assertEqual(repaired['intents'][:sequence], state['intents'])
        self.assertEqual(repaired['owned'], owned)
        self.assert_no_recovery_mutation(requests)
        self.assertEqual(f.cloud.rows, rows)
        self.assertEqual(f.cloud.actions, actions)
        self.assertEqual(f.cloud.next_action, next_action)

        self.assertEqual(f.campaign.arm_guard()['identity']['pid'], 113)
        result = f.campaign.cleanup_pass()
        self.assertTrue(result['complete'])
        self.assertTrue(result['baseline_equal'])
        self.assertTrue(result['hard_lifetime_met'])
        self.assertEqual(result['pending_intents'], [])
        self.assertEqual(result['owned_survivors'], [])
        self.assertEqual(result['subordinate_survivors'], 0)
        deletes = [('DELETE', f'/servers/{server}') for server in (101, 102, 103, 104)] + [
            ('DELETE', '/firewalls/301'), ('DELETE', '/ssh_keys/401'), ('DELETE', '/networks/201')]
        self.assertEqual(f.deletes(), deletes)
        # Only fresh, separately authorized cleanup DELETEs follow the original
        # creates; no interrupted provisioning call or DELETE is replayed.
        self.assertEqual(f.cloud.requests[:len(requests)], requests)
        self.assertEqual([(method, path) for method, path, _, _ in f.cloud.requests[len(requests):]], deletes)
        self.assertEqual(len(f.cloud.requests), 14)
        terminal = journal.read()
        self.assertEqual(terminal['mode'], 'COMPLETE')
        self.assertEqual(terminal['intents'][sequence], expected)
        self.assertEqual(journal.header, self.original_header)
        for inventory_kind in INVENTORY_KINDS:
            self.assertEqual(sorted(f.cloud.rows[inventory_kind]), f.header['baseline'][inventory_kind])

    def test_orphan_known_target_delete_before_http_repairs_and_guard_really_cleans(self):
        self.assert_known_target_mutation_repairs_before_http('delete')

    def test_orphan_known_target_subnet_before_http_repairs_and_guard_really_cleans(self):
        self.assert_known_target_mutation_repairs_before_http('subnet')

    def test_orphan_known_target_attach_before_http_repairs_and_guard_really_cleans(self):
        self.assert_known_target_mutation_repairs_before_http('attach')

    def test_orphan_known_target_poweron_before_http_repairs_and_guard_really_cleans(self):
        self.assert_known_target_mutation_repairs_before_http('poweron')

    def public_recovery_args(self, **changes):
        values = dict(label='campaign=synthetic', prefix='test',
                      deadline=str(self.original_header['time']['g_ns'] // cs.NS), wait='60')
        values.update(changes)
        return tg.parse_args(['--label', values['label'], '--prefix', values['prefix'],
            '--deadline', values['deadline'], '--request-wait-seconds=' + values['wait'],
            '--creation-ledger', str(self.path), '--execute', '--recover-journal',
            '--recovery-evidence-dir', str(self.evidence)])

    @contextlib.contextmanager
    def public_recovery_boundaries(self):
        # Construct real profiles and campaigns; replace only the provider and
        # the clocks used by campaigns constructed inside the public caller.
        real_campaign = cs.Campaign
        def clocked(journal, cloud):
            return real_campaign(journal, cloud, clock=lambda: self.f.wall, monotonic=lambda: self.f.mono)
        def cloud(profile, binding):
            self.assertEqual(profile.binding, self.original_header['context'])
            self.assertEqual(binding, self.original_header['context'])
            return self.f.cloud
        with mock.patch.object(cc, 'Profile', wraps=cc.Profile) as profiles, \
                mock.patch.object(cc, 'Cloud', side_effect=cloud) as clouds, \
                mock.patch.object(cs, 'Campaign', side_effect=clocked) as campaigns:
            yield profiles, clouds, campaigns

    def private_recovery_snapshot(self, full_metadata=False):
        result = {}
        paths = sorted(self.f.root.rglob('*'))
        if full_metadata:
            paths = [self.f.root, *paths]
        for path in paths:
            info = path.lstat()
            key = str(path.relative_to(self.f.root))
            result[key] = (info.st_dev, info.st_ino, info.st_mode, path.read_bytes() if path.is_file() else None)
            if full_metadata:
                result[key] += (info.st_uid, info.st_gid, info.st_nlink, info.st_size,
                                info.st_mtime_ns, info.st_ctime_ns)
        return result

    def test_public_recovery_binding_and_wait_refusals_precede_all_repair_effects(self):
        f = self.f
        crash = self.crash_final_ack()
        # A live producer makes premature quiescence observable: the actual
        # stop publication would alter the tree, and real pidfd calls are fatal.
        self.register_recovery_guard(producer_gone=False)
        with self.assertRaisesRegex(mc.GuardError, '^journal head differs from retained chain/orphan generation$'):
            cl.Journal(self.path, observe=True)
        deadline = self.original_header['time']['g_ns'] // cs.NS
        cases = [
            ('campaign', dict(label='campaign=other'), 'guard campaign binding differs'),
            ('prefix', dict(prefix='other'), 'guard campaign binding differs'),
            ('earlier-deadline', dict(deadline=str(deadline - 1)), 'guard cutover deadline changed'),
            ('later-deadline', dict(deadline=str(deadline + 1)), 'guard cutover deadline changed'),
        ]
        cases += [('wait-' + value, dict(wait=value), 'immediate confirmation wait must be bounded to60s')
                  for value in ('0', '-1', '60.01', 'inf', 'nan')]
        for label, changes, reason in cases:
            with self.subTest(binding=label):
                args = self.public_recovery_args(**changes)
                before = self.private_recovery_snapshot()
                processes = copy.deepcopy(f.processes)
                requests = copy.deepcopy(f.cloud.requests)
                reads = copy.deepcopy((f.cloud.lists, f.cloud.describes, f.cloud.action_reads))
                rows = copy.deepcopy(f.cloud.rows)
                actions = copy.deepcopy(f.cloud.actions)
                output = io.StringIO()
                with self.public_recovery_boundaries() as (profiles, clouds, campaigns):
                    with self.assertRaisesRegex(mc.GuardError, '^' + reason + '$'):
                        tg.guarded_run(args, sleep=lambda seconds: self.fail('refused recovery waited'), out=output)
                    profiles.assert_not_called()
                    clouds.assert_not_called()
                    campaigns.assert_not_called()
                self.assertEqual(output.getvalue(), '')
                self.assertEqual(self.private_recovery_snapshot(), before)
                self.assertEqual(f.processes, processes)
                self.assertTrue(cs.live(self.original_header['producer']))
                self.assert_no_recovery_mutation(requests)
                self.assertEqual((f.cloud.lists, f.cloud.describes, f.cloud.action_reads), reads)
                self.assertEqual(f.cloud.rows, rows)
                self.assertEqual(f.cloud.actions, actions)
                self.assertFalse((f.guard_directory / 'stop.json').exists())
                self.assertFalse((self.path.parent / cl.RECOVERY).exists())
                self.assertFalse(self.evidence.exists())
                for name, raw in crash['adverse'].items():
                    self.assertEqual((self.path.parent / name).read_bytes(), raw)

    def test_public_recovery_complete_head_refuses_before_authorization_and_quiescence(self):
        f = self.f
        f.closing()
        completed = f.campaign.cleanup_pass()
        self.assertTrue(completed['complete'])
        self.assertTrue(completed['hard_lifetime_met'])
        terminal = f.journal.read()
        self.assertEqual(terminal['mode'], 'COMPLETE')
        self.assertEqual(f.campaign.terminal_result(), completed)
        self.register_recovery_guard()
        checkpoint = copy.deepcopy(f.journal.checkpoint)
        tree = self.private_recovery_snapshot(full_metadata=True)
        processes = copy.deepcopy(f.processes)
        actor = f.actor
        provider = copy.deepcopy((f.cloud.requests, f.cloud.lists, f.cloud.describes, f.cloud.action_reads,
            f.cloud.rows, f.cloud.actions, f.cloud.next_ids, f.cloud.next_action, f.cloud.pending))
        output = io.StringIO()
        with self.public_recovery_boundaries() as (profiles, clouds, campaigns):
            with self.assertRaisesRegex(mc.GuardError, '^completed journal cannot recover creation admission$'):
                tg.guarded_run(self.public_recovery_args(),
                    sleep=lambda seconds: self.fail('completed journal repair waited'), out=output)
            profiles.assert_not_called()
            clouds.assert_not_called()
            campaigns.assert_not_called()
        self.assertEqual(output.getvalue(), '')
        self.assertEqual(self.private_recovery_snapshot(full_metadata=True), tree)
        self.assertEqual(f.processes, processes)
        self.assertEqual(f.actor, actor)
        self.assertEqual((f.cloud.requests, f.cloud.lists, f.cloud.describes, f.cloud.action_reads,
            f.cloud.rows, f.cloud.actions, f.cloud.next_ids, f.cloud.next_action, f.cloud.pending), provider)
        self.assertEqual(f.journal.read(), terminal)
        self.assertEqual(f.journal.checkpoint, checkpoint)
        self.assertFalse((self.path.parent / cl.RECOVERY).exists())
        self.assertFalse(self.evidence.exists())

    def test_public_recovery_nonterminal_head_really_repairs_and_completes(self):
        f = self.f
        headed = f.journal.read()
        self.assertEqual(headed['mode'], 'OPEN')
        self.assertEqual(headed['intents'], [])
        checkpoint = copy.deepcopy(f.journal.checkpoint)
        head_raw = self.path.read_bytes()
        anchor_raw = (self.path.parent / cl.ANCHOR).read_bytes()
        self.assertFalse(self.evidence.exists())
        self.register_recovery_guard()
        output = io.StringIO()
        with self.public_recovery_boundaries() as (profiles, clouds, campaigns):
            ok, receipt = tg.guarded_run(self.public_recovery_args(),
                sleep=lambda seconds: self.fail('nonterminal repair unexpectedly waited'), out=output)
            self.assertEqual(profiles.call_count, 2)
            self.assertEqual(clouds.call_count, 2)
            self.assertEqual(campaigns.call_count, 2)
        self.assertTrue(ok)
        self.assertTrue(receipt['result']['complete'])
        self.assertTrue(receipt['result']['hard_lifetime_met'])
        self.assertTrue(receipt['result']['baseline_equal'])
        self.assertEqual(json.loads(output.getvalue()), receipt['result'])
        terminal = f.journal.read()
        self.assertEqual(terminal['mode'], 'COMPLETE')
        self.assertEqual(f.journal.header, self.original_header)
        self.assertEqual(f.journal.binding, self.original_binding)
        repairs = [event['evidence'] for event in terminal['events'] if event['event'] == 'journal-recovery-closed']
        self.assertEqual(len(repairs), 1)
        repair = repairs[0]
        self.assertEqual(repair['candidate_source'], 'headed')
        self.assertEqual(repair['headed_checkpoint'], checkpoint)
        self.assertIsNone(repair['orphan_checkpoint'])
        self.assertTrue(repair['owner_proof']['producer_quiesced'])
        self.assertTrue(repair['owner_proof']['old_guard_gone'])
        self.assertEqual(repair['owner_proof']['recovery_guard']['pid'], 113)
        archive = Path(repair['archive'])
        self.assertEqual(archive.parent, self.evidence)
        self.assertEqual({item['source'] for item in repair['files']}, {self.path.name, cl.ANCHOR})
        for item in repair['files']:
            raw = head_raw if item['source'] == self.path.name else anchor_raw
            self.assertEqual((archive / item['archive_name']).read_bytes(), raw)
            self.assertEqual(item['sha256'], hashlib.sha256(raw).hexdigest())
        self.assertFalse((self.path.parent / cl.RECOVERY).exists())
        self.assertEqual(f.cloud.requests, [])
        self.assertEqual(f.deletes(), [])
        for kind in INVENTORY_KINDS:
            self.assertEqual(sorted(f.cloud.rows[kind]), f.header['baseline'][kind])

    def test_public_recovery_matching_bindings_really_repair_and_complete_cleanup(self):
        f = self.f
        crash = self.crash_final_ack()
        self.register_recovery_guard()
        requests = copy.deepcopy(f.cloud.requests)
        output = io.StringIO()
        with self.public_recovery_boundaries() as (profiles, clouds, campaigns):
            ok, receipt = tg.guarded_run(self.public_recovery_args(),
                sleep=lambda seconds: self.fail('matching public recovery unexpectedly waited'), out=output)
            self.assertEqual(profiles.call_count, 2)
            self.assertEqual(clouds.call_count, 2)
            self.assertEqual(campaigns.call_count, 2)
        self.assertTrue(ok)
        self.assertTrue(receipt['result']['complete'])
        self.assertTrue(receipt['result']['hard_lifetime_met'])
        self.assertTrue(receipt['result']['baseline_equal'])
        self.assertEqual(receipt['result']['pending_intents'], [])
        self.assertEqual(receipt['campaign'], 'synthetic')
        self.assertEqual(receipt['prefix'], 'test')
        self.assertEqual(receipt['journal'], self.original_binding)
        self.assertEqual(json.loads(output.getvalue()), receipt['result'])
        self.assertEqual(f.cloud.requests[:7], requests)
        deletes = [('DELETE', f'/servers/{rid}') for rid in (101, 102, 103, 104)] + [
            ('DELETE', '/firewalls/301'), ('DELETE', '/ssh_keys/401'), ('DELETE', '/networks/201')]
        self.assertEqual([(method, path) for method, path, _, _ in f.cloud.requests[7:]], deletes)
        self.assertEqual(len(f.cloud.requests), 14)
        journal = cl.Journal(self.path)
        self.addCleanup(journal.close)
        f.journal = journal
        self.assertEqual(journal.header, self.original_header)
        self.assertEqual(journal.read()['mode'], 'COMPLETE')
        self.assert_archive(crash, journal)
        self.assertTrue((f.guard_directory / 'stop.json').is_file())
        self.assertEqual(json.loads((f.guard_directory / 'cleanup.json').read_bytes()), receipt['result'])
        for kind in INVENTORY_KINDS:
            self.assertEqual(sorted(f.cloud.rows[kind]), f.header['baseline'][kind])

    def test_bad_orphan_mac_refuses_repair_without_mutation_or_evidence_loss(self):
        crash = self.crash_final_ack()
        self.register_recovery_guard()
        altered = copy.deepcopy(crash['orphan'])
        altered['mac'] = '0' * 64
        raw = cl.canonical_json(altered) + b'\n'
        crash['orphan_path'].write_bytes(raw)
        requests = copy.deepcopy(self.f.cloud.requests)
        with self.assertRaisesRegex(mc.GuardError, 'MAC|mac|authentication'):
            self.repair()
        self.assertEqual(crash['orphan_path'].read_bytes(), raw)
        self.assertEqual(self.path.read_bytes(), crash['adverse'][self.path.name])
        self.assert_no_recovery_mutation(requests)
        self.assertFalse(self.evidence.exists())

    def assert_repair_authorization_refuses_without_effects(self, crash, reason):
        f = self.f
        self.assertTrue(cs.live(self.original_header['producer']))
        self.assertFalse((f.guard_directory / 'stop.json').exists())
        # Include guard files as well as all headed/orphan/evidence paths. A
        # premature stop publication must fail even if quiescence later refuses.
        tree = self.private_recovery_snapshot()
        processes = copy.deepcopy(f.processes)
        actor = f.actor
        requests = copy.deepcopy(f.cloud.requests)
        provider = copy.deepcopy((f.cloud.lists, f.cloud.describes, f.cloud.action_reads,
            f.cloud.rows, f.cloud.actions, f.cloud.next_ids, f.cloud.next_action, f.cloud.pending))
        with mock.patch.object(cs.os, 'pidfd_open',
                side_effect=AssertionError('unauthorized recovery reached producer pidfd')) as acquire, \
                mock.patch.object(cs.signal, 'pidfd_send_signal',
                side_effect=AssertionError('unauthorized recovery signaled producer')) as send:
            with self.assertRaisesRegex(mc.GuardError, '^' + reason + '$'):
                self.repair()
            acquire.assert_not_called()
            send.assert_not_called()
        self.assertEqual(self.private_recovery_snapshot(), tree)
        self.assertEqual(f.processes, processes)
        self.assertEqual(f.actor, actor)
        self.assertTrue(cs.live(self.original_header['producer']))
        self.assert_no_recovery_mutation(requests)
        self.assertEqual((f.cloud.lists, f.cloud.describes, f.cloud.action_reads,
            f.cloud.rows, f.cloud.actions, f.cloud.next_ids, f.cloud.next_action, f.cloud.pending), provider)
        self.assertFalse((f.guard_directory / 'stop.json').exists())
        self.assertFalse((self.path.parent / cl.RECOVERY).exists())
        self.assertFalse(self.evidence.exists())
        for name, raw in crash['adverse'].items():
            self.assertEqual((self.path.parent / name).read_bytes(), raw)

    def test_live_old_guard_refuses_repair_before_quiescence_or_mutation(self):
        crash = self.crash_final_ack()
        self.register_recovery_guard(producer_gone=False, guard_gone=False)
        self.assert_repair_authorization_refuses_without_effects(crash, 'old guard is still live during journal repair')

    def test_unsupervised_recovery_actor_refuses_without_mutation(self):
        crash = self.crash_final_ack()
        self.register_recovery_guard(producer_gone=False)
        self.f.processes[113]['parent'] = 1
        self.assert_repair_authorization_refuses_without_effects(crash,
            'journal repair requires the pinned supervised recovery guard')

    def test_changed_source_pin_refuses_without_mutation(self):
        crash = self.crash_final_ack()
        self.register_recovery_guard(producer_gone=False)
        source = next(iter(self.original_header['sources']))
        Path(source).write_text('# changed source after crash\n')
        # The shared assertion snapshots after this intentional source change.
        self.assert_repair_authorization_refuses_without_effects(crash, 'pinned guard source changed')

    def test_live_busy_producer_never_repairs_until_owned_process_is_quiescent(self):
        crash = self.crash_final_ack()
        self.register_recovery_guard(producer_gone=False)
        requests = copy.deepcopy(self.f.cloud.requests)
        fake_pidfd = os.open(self.f.config, os.O_RDONLY)
        with mock.patch.object(cs.os, 'pidfd_open', return_value=fake_pidfd) as acquire, \
                mock.patch.object(cs.signal, 'pidfd_send_signal') as send, \
                mock.patch.object(cs.select, 'select', return_value=([], [], [])):
            with self.assertRaisesRegex(mc.GuardError, 'producer did not quiesce'):
                self.repair()
        acquire.assert_called_once_with(111)
        self.assertEqual(send.call_args_list, [mock.call(fake_pidfd, signal.SIGTERM),
                                               mock.call(fake_pidfd, signal.SIGKILL)])
        self.assertEqual(self.path.read_bytes(), crash['adverse'][self.path.name])
        self.assert_no_recovery_mutation(requests)
        self.assertFalse(self.evidence.exists())

    def test_busy_journal_lock_refuses_before_authorization_or_mutation(self):
        crash = self.crash_final_ack()
        self.register_recovery_guard()
        requests = copy.deepcopy(self.f.cloud.requests)
        lock = os.open(self.path.parent / cl.LOCK, os.O_RDWR)
        try:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
            with self.assertRaisesRegex(mc.GuardError, 'quiescent unlocked actors'):
                self.repair()
        finally:
            fcntl.flock(lock, fcntl.LOCK_UN)
            os.close(lock)
        self.assertEqual(self.path.read_bytes(), crash['adverse'][self.path.name])
        self.assert_no_recovery_mutation(requests)
        self.assertFalse(self.evidence.exists())

    def test_repair_sync_failure_retains_one_shot_marker_and_all_adverse_bytes(self):
        crash = self.crash_final_ack()
        self.register_recovery_guard()
        requests = copy.deepcopy(self.f.cloud.requests)
        real_fsync = os.fsync
        calls = []
        def sync(fd):
            if (self.path.parent / cl.RECOVERY).exists():
                # Authorization also fsyncs its stop request. Fault only the
                # repair marker after it exists, never that earlier operation.
                calls.append(stat.S_IFMT(os.fstat(fd).st_mode))
                if len(calls) == 2:
                    self.assertTrue(stat.S_ISDIR(os.fstat(fd).st_mode))
                    raise OSError('synthetic recovery marker-directory fsync failure')
            return real_fsync(fd)
        with mock.patch.object(cl.os, 'fsync', side_effect=sync):
            with self.assertRaisesRegex(OSError, 'recovery marker-directory fsync'):
                self.repair()
        self.assertEqual(calls, [stat.S_IFREG, stat.S_IFDIR])
        marker = self.path.parent / cl.RECOVERY
        self.assertTrue(marker.exists())
        retained_marker = marker.read_bytes()
        for name, original in crash['adverse'].items():
            self.assertEqual((self.path.parent / name).read_bytes(), original)
        self.assert_no_recovery_mutation(requests)
        with self.assertRaisesRegex(mc.GuardError, 'prior journal recovery requires owner incident'):
            self.repair()
        self.assertEqual(marker.read_bytes(), retained_marker)
        self.assert_no_recovery_mutation(requests)
        for name, original in crash['adverse'].items():
            self.assertEqual((self.path.parent / name).read_bytes(), original)


if __name__ == '__main__':
    unittest.main()
