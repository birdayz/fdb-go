#!/usr/bin/env python3
"""Deterministic journal work bounds with real validation and recovery."""
from pathlib import Path
import json
import os
import sys
import unittest
from unittest.mock import patch

HERE = Path(os.path.abspath(__file__)).parent
sys.path.insert(0, str(HERE))

import creation_ledger as ledger  # noqa: E402
import mn_common as mc  # noqa: E402
import test_creation_ledger as fixtures  # noqa: E402


class JournalCostTests(unittest.TestCase):
    def setUp(self):
        self.fixture = fixtures.RecoveryTests()
        self.addCleanup(self.fixture.doCleanups)
        self.fixture.setUp()
        clear = getattr(ledger._credential_field, 'cache_clear', None)
        if clear is not None:
            clear()

    def test_credential_key_classification_is_reused_bounded_and_never_caches_values(self):
        keys = ('cost_probe', 'cost_probe_token_sha256', 'cost_probe_password_present', 'cost_probe_secret')
        expected = (None, 'digest', 'redaction', 'forbidden')
        with patch.object(ledger.re, 'search', wraps=ledger.re.search) as search:
            self.assertEqual(tuple(map(ledger._credential_field, keys)), expected)
            first_calls = search.call_count
            self.assertEqual(tuple(map(ledger._credential_field, keys)), expected)
            repeated_calls = search.call_count - first_calls
            ledger._json_value({'cost_probe_token_sha256': 'a' * 64,
                                'cost_probe_password_present': True}, credentials=True)
            for key, value in ((keys[1], 'z' * 64), (keys[1], None),
                               (keys[2], 1), (keys[2], 'true'),
                               (keys[3], 'forbidden'), (keys[3], False)):
                with self.subTest(key=key, value=value):
                    with self.assertRaisesRegex(mc.GuardError, 'credential fields are forbidden'):
                        ledger._json_value({'nested': [{key: value}]}, credentials=True)
            ledger._credential_field('cost_eviction_probe')
            for number in range(4096):
                ledger._credential_field('cost_eviction_' + str(number))
            before_revisit = search.call_count
            ledger._credential_field('cost_eviction_probe')
            eviction_calls = search.call_count - before_revisit
        self.assertEqual(repeated_calls, 0, 'repeated pure key classification must not repeat regex work')
        self.assertEqual(eviction_calls, 1, 'classification retention must be bounded to at most 4096 keys')

    def test_load_hashes_exact_validated_generation_bytes_without_reserializing_records(self):
        with self.fixture.journal.locked() as current:
            current['events'].append({'event': 'cost-probe'})
            self.fixture.journal.commit(current)
        expected = self.fixture.journal.read()
        generation_raw = {path.read_bytes()[:-1] for path in self.fixture.root.glob('generation-*.json')}
        self.assertEqual(len(generation_raw), 2)
        validated = set()
        hashed = []
        serialized_generations = []
        read_json = ledger.Journal._read_json
        sha256 = ledger.sha256

        def read(journal, name):
            value, raw, identity = read_json(journal, name)
            if ledger.GENERATION.fullmatch(name):
                validated.add(raw[:-1])
            return value, raw, identity

        def digest(value):
            if type(value) is bytes and value in generation_raw:
                self.assertIn(value, validated, 'raw generation hashing must follow canonical validation')
                hashed.append(value)
            if type(value) is dict and value.get('format') == ledger.GENERATION_FORMAT and 'mac' in value:
                serialized_generations.append(value)
            return sha256(value)

        with patch.object(ledger.Journal, '_read_json', read), patch.object(ledger, 'sha256', digest):
            self.assertEqual(self.fixture.journal.read(), expected)
        self.assertEqual(serialized_generations, [],
                         'load must hash validated raw bytes, not reserialize authenticated generation records')
        self.assertEqual(set(hashed), generation_raw)
        self.assertEqual(len(hashed), len(generation_raw))

    def test_strict_open_and_read_refuse_parseable_noncanonical_anchor_without_changing_evidence(self):
        def metadata(path):
            info = path.lstat()
            return (info.st_dev, info.st_ino, info.st_mode, info.st_uid, info.st_gid,
                    info.st_nlink, info.st_size, info.st_mtime_ns, info.st_ctime_ns)

        def snapshot():
            return (metadata(self.fixture.root),
                    {path.name: (path.read_bytes(), metadata(path))
                     for path in self.fixture.root.iterdir()})

        canonical_evidence = snapshot()
        reader = ledger.Journal(self.fixture.path)
        self.addCleanup(reader.close)
        self.assertEqual(reader.read(), fixtures.state())
        self.assertEqual(snapshot(), canonical_evidence)
        anchor = self.fixture.root / ledger.ANCHOR
        original = anchor.read_bytes()
        identity = metadata(anchor)[:2]
        noncanonical = b' ' + original
        self.assertNotEqual(noncanonical, original)
        self.assertEqual(json.loads(noncanonical), json.loads(original))
        with anchor.open('wb') as stream:
            stream.write(noncanonical)
            stream.flush()
            os.fsync(stream.fileno())
        self.assertEqual(metadata(anchor)[:2], identity)
        before = snapshot()
        with self.assertRaisesRegex(mc.GuardError, '^noncanonical journal JSON$'):
            reopened = ledger.Journal(self.fixture.path)
            self.addCleanup(reopened.close)
        self.assertEqual(snapshot(), before)
        with self.assertRaisesRegex(mc.GuardError, '^noncanonical journal JSON$'):
            reader.read()
        self.assertEqual(snapshot(), before)

    def test_token_fixture_recovers_without_an_open_to_headed_setup_commit(self):
        commits = []
        commit = ledger.Journal.commit

        def delegated(journal, current):
            commits.append(journal.path)
            return commit(journal, current)

        with patch.object(ledger.Journal, 'commit', delegated):
            self.fixture.assert_token_partial_recovers('cost-probe', 17, 1, 'orphan')
        self.assertEqual(commits, [], 'token recovery fixture must initialize its intended headed state directly')


if __name__ == '__main__':
    unittest.main()
