#!/usr/bin/env python3
"""Remaining closed-successor refusal arms through real authenticated recovery.

Legacy fixtures are composed through a module alias, not inherited or imported
as TestCase globals, so this target discovers only the new boundary controls.
"""
import copy
import hashlib
import hmac
import json
import os
from pathlib import Path
import stat
import sys
import unittest

HERE = Path(os.path.abspath(__file__)).parent
sys.path.insert(0, str(HERE))

import creation_ledger as ledger  # noqa: E402
import mn_common as mc  # noqa: E402
import test_creation_ledger as fixtures  # noqa: E402


class NormalizerBoundaryTests(unittest.TestCase):
    def setUp(self):
        self.fixture = fixtures.RecoveryTests()
        self.addCleanup(self.fixture.doCleanups)
        self.fixture.setUp()

    def prepare(self, label, headed='PREPARED', candidate='SENDING', operation='create'):
        return self.fixture.prepare(self.fixture.independent_journal(label),
            headed_state=headed, orphan_state=candidate, operation=operation)

    def assert_authenticated(self, case):
        key = (case['path'].parent / ledger.KEY).read_bytes()
        for raw in (case['head_raw'], case['raw']):
            record = json.loads(raw)
            body = {name: value for name, value in record.items() if name != 'mac'}
            expected = hmac.new(key, fixtures.encoded(body)[:-1], hashlib.sha256).hexdigest()
            self.assertEqual(record['mac'], expected)
            self.assertEqual(raw, fixtures.encoded(record))
        self.assertEqual(json.loads(case['head_raw'])['state'], case['headed'])
        self.assertEqual(json.loads(case['raw'])['state'], case['candidate'])
        self.assertEqual(case['next_path'].read_bytes(), case['raw'])

    def snapshot(self, case, evidence):
        directory = case['path'].parent
        files = {}
        for path in directory.iterdir():
            info = path.lstat()
            self.assertTrue(stat.S_ISREG(info.st_mode))
            files[path.name] = (path.read_bytes(), info.st_dev, info.st_ino, info.st_mode,
                                info.st_uid, info.st_gid, info.st_nlink, info.st_size, info.st_mtime_ns)
        info = directory.stat()
        return dict(files=files, directory=(info.st_dev, info.st_ino, info.st_mode, info.st_uid,
                    info.st_gid, info.st_nlink, info.st_mtime_ns),
                    head=case['path'].read_bytes(), checkpoint=case['journal'].checkpoint,
                    parent_names=sorted(path.name for path in directory.parent.iterdir()),
                    evidence_exists=evidence.exists(), marker_exists=(directory / ledger.RECOVERY).exists())

    def recover(self, case, evidence, audit, change=None):
        def authorize(header, binding, checkpoint, headed):
            audit['calls'].append('authorize')
            self.assertEqual(header, fixtures.header())
            self.assertEqual(binding, case['binding'])
            self.assertEqual(checkpoint, case['checkpoint'])
            self.assertEqual(headed, case['headed'])
            return dict(producer_quiesced=True, old_guard_gone=True, recovery_guard={'pid': 777})

        def close_state(candidate, receipt):
            audit['calls'].append('close_state')
            self.assertEqual(candidate, case['candidate'])
            self.assertEqual(receipt['headed_state'], case['headed'])
            self.assertEqual(receipt['headed_checkpoint'], case['checkpoint'])
            self.assertEqual(receipt['candidate_source'], 'orphan')
            self.assertEqual(receipt['orphan_checkpoint'], dict(generation=case['record']['generation'],
                sha256=hashlib.sha256(case['raw'][:-1]).hexdigest()))
            self.assertEqual(Path(receipt['archive']).parent, evidence)
            self.assertFalse(evidence.exists(), 'the close callback must precede archive publication')
            self.assertFalse((case['path'].parent / ledger.RECOVERY).exists())
            audit['candidate'] = copy.deepcopy(candidate)
            successor = self.fixture.normalize(candidate, receipt)
            if change is not None:
                change(successor)
            audit['successor'] = copy.deepcopy(successor)
            return successor

        journal = ledger.Journal.recover_closed(case['path'], evidence_directory=evidence,
                                                authorize=authorize, close_state=close_state)
        self.addCleanup(journal.close)
        return journal

    def refuse(self, case, label, diagnostic, change=None):
        self.assert_authenticated(case)
        evidence = self.fixture.root.parent / ('normalizer-evidence-' + label)
        self.assertFalse(evidence.exists())
        self.assertFalse((case['path'].parent / ledger.RECOVERY).exists())
        before = self.snapshot(case, evidence)
        audit = {'calls': []}
        with self.assertRaises(mc.GuardError) as caught:
            self.recover(case, evidence, audit, change)
        self.assertEqual(str(caught.exception), diagnostic)
        self.assertEqual(audit['calls'], ['authorize', 'close_state'])
        self.assertEqual(audit['candidate'], case['candidate'])
        self.assertEqual(self.snapshot(case, evidence), before,
                         'a rejected normalizer must not publish an archive, marker, generation or head')
        self.assertFalse(evidence.exists())
        self.assertFalse((case['path'].parent / ledger.RECOVERY).exists())
        return audit

    def healthy(self, case, label, expected_state):
        self.assert_authenticated(case)
        evidence = self.fixture.root.parent / ('healthy-normalizer-evidence-' + label)
        self.assertFalse(evidence.exists())
        audit = {'calls': []}
        journal = self.recover(case, evidence, audit)
        self.assertEqual(audit['calls'], ['authorize', 'close_state'])
        current = journal.read()
        self.assertEqual(current['mode'], 'CLOSING')
        self.assertIsNone(current['active_lease'])
        self.assertEqual(current['intents'][0]['state'], expected_state)
        expected = copy.deepcopy(audit['successor'])
        self.assertEqual(current['events'][-1]['event'], 'journal-recovery-closed')
        expected['events'].append(current['events'][-1])
        self.assertEqual(current, expected)
        self.assertTrue(journal.is_ancestor(case['checkpoint']))
        self.assertEqual(journal.checkpoint['generation'], case['checkpoint']['generation'] + 2)
        self.assertEqual(case['next_path'].read_bytes(), case['raw'])
        self.assertFalse((case['path'].parent / ledger.RECOVERY).exists())
        self.fixture.assert_archived_partial(current, case['path'].name, case['head_raw'])
        self.fixture.assert_archived_partial(current, case['next_path'].name, case['raw'])
        return current

    def test_normalizer_cannot_drop_or_add_intents_and_same_context_can_recover(self):
        for change in ('drop', 'add'):
            with self.subTest(change=change):
                case = self.prepare('normalizer-count-' + change, headed='SENDING', candidate='ACKNOWLEDGED')
                self.assertEqual(len(case['candidate']['intents']), 1)
                def altered(successor):
                    if change == 'drop':
                        successor['intents'].pop()
                    else:
                        extra = copy.deepcopy(successor['intents'][0])
                        extra.update(sequence=1, name='synthetic-extra')
                        successor['intents'].append(extra)
                audit = self.refuse(case, 'count-' + change, 'journal recovery changed intent population', altered)
                self.assertEqual(len(audit['successor']['intents']), 0 if change == 'drop' else 2)
                self.healthy(case, 'count-' + change, 'UNKNOWN')

    def test_normalizer_cannot_rewrite_terminal_state_and_same_context_can_recover(self):
        for terminal in ('ACKNOWLEDGED', 'DONE', 'FAILED', 'NOT_DISPATCHED'):
            with self.subTest(terminal=terminal):
                if terminal == 'NOT_DISPATCHED':
                    case = self.prepare('normalizer-terminal-' + terminal)
                else:
                    case = self.fixture.retained_evidence_case('normalizer-terminal-' + terminal)
                headed = copy.deepcopy(case['headed'])
                headed.update(mode='CLOSING', active_lease=None)
                headed['intents'][0]['state'] = terminal
                self.fixture.publish_fixture_head(case, headed)
                self.assertEqual(case['headed']['intents'][0]['state'], terminal)
                self.assertEqual(case['candidate']['intents'][0]['state'], terminal)
                def altered(successor):
                    successor['intents'][0]['state'] = 'UNKNOWN'
                audit = self.refuse(case, 'terminal-' + terminal, 'journal recovery changed terminal evidence', altered)
                self.assertEqual(audit['successor']['intents'][0]['state'], 'UNKNOWN')
                self.healthy(case, 'terminal-' + terminal, terminal)

    def test_normalizer_must_preserve_provably_pre_send_distinction(self):
        for headed in ('PREPARED', None):
            for operation in ('create', 'delete'):
                label = str(headed) + '-' + operation
                with self.subTest(headed=headed, operation=operation):
                    case = self.prepare('normalizer-pre-send-' + label, headed=headed, operation=operation)
                    self.assertEqual(case['candidate']['intents'][0]['state'], 'SENDING')
                    self.assertIsNone(case['candidate']['intents'][0]['ack'])
                    self.assertEqual(case['candidate']['intents'][0]['resource_id'], None if operation == 'create' else 201)
                    def altered(successor):
                        successor['intents'][0]['state'] = 'UNKNOWN'
                    audit = self.refuse(case, 'pre-send-' + label, 'journal recovery lost pre-dispatch distinction', altered)
                    self.assertEqual(audit['successor']['intents'][0]['state'], 'UNKNOWN')
                    current = self.healthy(case, 'pre-send-' + label, 'NOT_DISPATCHED')
                    self.assertEqual(current['intents'][0]['resource_id'], None if operation == 'create' else 201)

    def test_contradictory_pre_send_ack_reaches_callback_but_cannot_publish(self):
        for headed in ('PREPARED', None):
            for candidate in ('PREPARED', 'SENDING'):
                for operation in ('create', 'delete'):
                    label = str(headed) + '-' + candidate + '-' + operation
                    with self.subTest(headed=headed, candidate=candidate, operation=operation):
                        case = self.prepare('normalizer-ack-' + label, headed=headed, candidate=candidate, operation=operation)
                        contradictory = copy.deepcopy(case['candidate'])
                        contradictory['intents'][0]['ack'] = {'status': 503, 'body_sha256': 'd' * 64}
                        self.fixture.rewrite_orphan(case, contradictory)
                        audit = self.refuse(case, 'ack-' + label, 'contradictory pre-dispatch recovery evidence')
                        self.assertEqual(audit['successor']['intents'][0]['state'], 'NOT_DISPATCHED')
                        self.assertEqual(audit['successor']['intents'][0]['ack'], contradictory['intents'][0]['ack'])
                        # Only the contradictory receipt differs in the healthy authenticated twin.
                        healthy = copy.deepcopy(contradictory)
                        healthy['intents'][0]['ack'] = None
                        self.fixture.rewrite_orphan(case, healthy)
                        self.healthy(case, 'ack-' + label, 'NOT_DISPATCHED')

    def test_contradictory_pre_send_create_id_reaches_callback_but_cannot_publish(self):
        for headed in ('PREPARED', None):
            for candidate in ('PREPARED', 'SENDING'):
                label = str(headed) + '-' + candidate
                with self.subTest(headed=headed, candidate=candidate):
                    case = self.prepare('normalizer-create-id-' + label, headed=headed, candidate=candidate)
                    contradictory = copy.deepcopy(case['candidate'])
                    self.assertIsNone(contradictory['intents'][0]['ack'])
                    contradictory['intents'][0]['resource_id'] = 201
                    self.fixture.rewrite_orphan(case, contradictory)
                    audit = self.refuse(case, 'create-id-' + label, 'contradictory pre-dispatch recovery evidence')
                    self.assertEqual(audit['successor']['intents'][0]['state'], 'NOT_DISPATCHED')
                    self.assertEqual(audit['successor']['intents'][0]['resource_id'], 201)
                    # Only the contradictory create target differs in the healthy authenticated twin.
                    healthy = copy.deepcopy(contradictory)
                    healthy['intents'][0]['resource_id'] = None
                    self.fixture.rewrite_orphan(case, healthy)
                    self.healthy(case, 'create-id-' + label, 'NOT_DISPATCHED')


if __name__ == '__main__':
    unittest.main()
