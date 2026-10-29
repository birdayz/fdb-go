"""Offline journal durability/integrity controls; no credentials or cloud calls."""
import contextlib
import copy
import hashlib
import hmac
import json
import os
from pathlib import Path
import shutil
import stat
import tempfile
import threading
import unittest
from unittest.mock import patch

import creation_ledger as ledger
import mn_common as mc


def header():
    return dict(campaign='synthetic-campaign', prefix='synthetic-', boot_id='synthetic-boot',
                sources={'tool_sha256': '1' * 64}, baseline={'sha256': '2' * 64},
                time={'t0_ns': 100, 'cutover_ns': 200, 'hard_ns': 300},
                producer={'pid': 100, 'starttime': '123', 'argv_sha256': '3' * 64},
                context={'name': 'synthetic', 'config_sha256': '4' * 64, 'credential_sha256': '5' * 64},
                plan={'servers': 4}, threat_model='benign-project-writers', graph_reserved=True)


def state():
    return dict(mode='OPEN', intents=[], owned=[], associations=[], events=[], guard=None, active_lease=None)


def encoded(value):
    return json.dumps(value, sort_keys=True, separators=(',', ':'), ensure_ascii=False, allow_nan=False).encode() + b'\n'


class JournalCase(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name) / 'private'
        self.path = self.root / 'head.json'
        self.journal = ledger.Journal.initialize(self.path, header(), state())
        self.addCleanup(self.journal.close)

    def reopen(self):
        journal = ledger.Journal(self.path)
        self.addCleanup(journal.close)
        return journal

    def generation(self, number):
        return self.root / f'generation-{number:020d}.json'

    def load(self, path):
        return json.loads(path.read_bytes())

    def sign(self, record):
        record = {key: value for key, value in record.items() if key != 'mac'}
        key = (self.root / 'integrity.key').read_bytes()
        record['mac'] = hmac.new(key, encoded(record)[:-1], hashlib.sha256).hexdigest()
        return record

    def replace(self, path, data):
        temporary = self.root / 'replacement'
        fd = os.open(temporary, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
        with os.fdopen(fd, 'wb') as stream:
            stream.write(data)
        os.replace(temporary, path)

    def advance(self, mode='OPEN'):
        with self.journal.locked() as current:
            current['mode'] = mode
            current['events'].append({'sequence': len(current['events']), 'reason': 'synthetic transition'})
            self.journal.commit(current)


class LedgerTests(JournalCase):
    def test_directory_census_refreshes_after_create_rename_and_retirement(self):
        initial = set(path.name for path in self.root.iterdir())
        self.assertEqual(ledger._names(self.journal._directory), initial)
        temporary = self.root / 'temporary'
        temporary.write_bytes(b'local census probe')
        self.assertEqual(ledger._names(self.journal._directory), initial | {'temporary'})
        temporary.rename(self.root / 'renamed')
        self.assertEqual(ledger._names(self.journal._directory), initial | {'renamed'})
        (self.root / 'renamed').unlink()
        self.assertEqual(ledger._names(self.journal._directory), initial)
        self.assertEqual(self.journal.read(), state())

    def test_multiple_transitions_reopen_and_stable_bindings(self):
        original_binding = self.journal.binding
        previous_head_inode = self.path.stat().st_ino
        self.assertEqual(self.journal.checkpoint['generation'], 0)
        self.assertEqual(self.journal.checkpoint['sha256'], hashlib.sha256(encoded(self.load(self.generation(0)))[:-1]).hexdigest())
        observer = self.reopen()
        for number, mode in enumerate(('OPEN', 'CLOSING', 'INCIDENT', 'COMPLETE'), 1):
            self.advance(mode)
            self.assertNotEqual(self.path.stat().st_ino, previous_head_inode)
            previous_head_inode = self.path.stat().st_ino
            self.assertEqual(self.journal.binding, original_binding)
            self.assertEqual(self.journal.checkpoint['generation'], number)
            self.assertEqual(observer.read()['mode'], mode)
            self.assertEqual(observer.checkpoint, self.journal.checkpoint)
            self.assertEqual(len(observer.read()['events']), number)
        self.assertEqual(self.reopen().read(), self.journal.read())
        self.assertEqual(stat.S_IMODE(self.root.stat().st_mode), 0o700)
        for path in self.root.iterdir():
            self.assertEqual(stat.S_IMODE(path.stat().st_mode), 0o600)
            self.assertEqual(path.stat().st_uid, os.getuid())
        self.assertEqual(len((self.root / 'integrity.key').read_bytes()), 32)
        self.assertNotIn((self.root / 'integrity.key').read_bytes().hex(), self.path.read_text())

    def test_public_values_are_defensive_copies_and_uncommitted_edits_do_not_persist(self):
        self.journal.header['campaign'] = 'changed'
        self.journal.binding['directory']['inode'] = 0
        self.journal.checkpoint['generation'] = 99
        read = self.journal.read()
        read['events'].append({'reason': 'not committed'})
        with self.journal.locked() as current:
            current['events'].append({'reason': 'also not committed'})
        self.assertEqual(self.journal.header, header())
        self.assertNotEqual(self.journal.binding['directory']['inode'], 0)
        self.assertEqual(self.journal.checkpoint['generation'], 0)
        self.assertEqual(self.journal.read(), state())
        with self.assertRaisesRegex(mc.GuardError, 'requires held lock'):
            self.journal.commit(state())
        with self.journal.locked():
            with self.assertRaisesRegex(mc.GuardError, 'nested journal lock'):
                self.journal.read()

    def test_canonical_hashes_and_sensitive_value_policy(self):
        self.assertEqual(ledger.canonical_json({'z': [True, None], 'a': 1}), b'{"a":1,"z":[true,null]}')
        self.assertEqual(ledger.sha256(b'abc'), 'ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad')
        self.assertEqual(ledger.sha256({'a': 1}), hashlib.sha256(b'{"a":1}').hexdigest())
        for value in ({1: 'bad'}, {'x': float('nan')}, {'x': float('inf')}, {'x': {1}}, {'x': b'bytes'}):
            with self.subTest(value=repr(value)), self.assertRaises(mc.GuardError):
                ledger.canonical_json(value)
        for field in ('token', 'HCLOUD_TOKEN', 'Authorization', 'root_password', 'private_key', 'credentials', 'user_data'):
            with self.subTest(field=field):
                candidate = header()
                candidate['context'][field] = 'synthetic-sensitive-value'
                with self.assertRaisesRegex(mc.GuardError, 'credential fields'):
                    ledger.Journal.initialize(self.root.parent / field / 'head.json', candidate, state())
        with self.journal.locked() as current:
            current['events'].append({'root_password_redacted': True, 'user_data_sha256': '6' * 64,
                                      'public_key_sha256': '7' * 64})
            self.journal.commit(current)
        with self.journal.locked() as current:
            current['events'].append({'root_password': 'synthetic-sensitive-value'})
            with self.assertRaisesRegex(mc.GuardError, 'credential fields'):
                self.journal.commit(current)
        self.assertNotIn('synthetic-sensitive-value', self.path.read_text())

    def test_raw_user_data_body_refused_but_hash_descriptors_and_redaction_facts_persist(self):
        with self.journal.locked() as current:
            current['intents'].append({'descriptor': {'user_data_sha256': 'a' * 64},
                                       'wire_body_sha256': 'b' * 64, 'user_data_redacted': True})
            self.journal.commit(current)
        self.assertEqual(self.journal.read()['intents'][0]['descriptor'], {'user_data_sha256': 'a' * 64})
        with self.journal.locked() as current:
            current['events'].append({'response': {'user_data': 'synthetic private user-data bytes'}})
            with self.assertRaisesRegex(mc.GuardError, 'credential fields are forbidden'):
                self.journal.commit(current)
        self.assertNotIn('synthetic private user-data bytes', self.path.read_text())

    def test_header_and_state_required_types_refuse_before_creating_files(self):
        for missing in header():
            candidate = header()
            candidate.pop(missing)
            with self.subTest(missing=missing), self.assertRaisesRegex(mc.GuardError, 'incomplete journal header'):
                ledger.Journal.initialize(self.root.parent / missing / 'head.json', candidate, state())
        for field, value in [('mode', []), ('mode', 'UNKNOWN'), ('intents', {}), ('owned', None),
                             ('associations', 'bad'), ('events', None), ('guard', []), ('active_lease', False)]:
            candidate = state()
            candidate[field] = value
            destination = self.root.parent / ('bad-' + field) / 'head.json'
            with self.subTest(field=field, value=value), self.assertRaises(mc.GuardError):
                ledger.Journal.initialize(destination, header(), candidate)
            self.assertFalse(destination.parent.exists())
        with self.assertRaisesRegex(mc.GuardError, 'not empty'):
            ledger.Journal.initialize(self.path, header(), state())

    def test_mode_owner_symlink_nonregular_and_hardlink_controls(self):
        for name in ('head.json', 'anchor.json', 'integrity.key', 'journal.lock', self.generation(0).name):
            with self.subTest(file=name):
                path = self.root / name
                path.chmod(0o640)
                with self.assertRaisesRegex(mc.GuardError, 'permissions'):
                    ledger.Journal(self.path)
                path.chmod(0o600)
                self.assertEqual(self.reopen().read(), state())
        self.root.chmod(0o750)
        with self.assertRaisesRegex(mc.GuardError, 'permissions'):
            ledger.Journal(self.path)
        self.root.chmod(0o700)
        with patch.object(ledger.os, 'getuid', return_value=os.getuid() + 1):
            with self.assertRaisesRegex(mc.GuardError, 'ownership'):
                ledger.Journal(self.path)
        saved = self.path.read_bytes()
        self.path.unlink()
        self.path.symlink_to(self.generation(0).name)
        with self.assertRaises((mc.GuardError, OSError)):
            ledger.Journal(self.path)
        self.path.unlink()
        os.mkfifo(self.path, 0o600)
        with self.assertRaisesRegex(mc.GuardError, 'file type'):
            ledger.Journal(self.path)
        self.path.unlink()
        self.replace(self.path, saved)
        os.link(self.path, self.root.parent / 'linked-head')
        with self.assertRaisesRegex(mc.GuardError, 'multiple links'):
            ledger.Journal(self.path)

    def test_directory_and_anchor_replacement_refuse(self):
        raw = (self.root / 'anchor.json').read_bytes()
        self.replace(self.root / 'anchor.json', raw)
        with self.assertRaisesRegex(mc.GuardError, 'anchor identity'):
            ledger.Journal(self.path)
        fresh_root = self.root.parent / 'separate'
        fresh = ledger.Journal.initialize(fresh_root / 'head.json', header(), state())
        self.addCleanup(fresh.close)
        moved = self.root.parent / 'moved'
        fresh_root.rename(moved)
        shutil.copytree(moved, fresh_root)
        with self.assertRaisesRegex(mc.GuardError, 'directory identity'):
            fresh.read()
        with self.assertRaisesRegex(mc.GuardError, 'anchor identity'):
            ledger.Journal(fresh_root / 'head.json')

    def test_file_owner_and_directory_symlink_are_independently_refused(self):
        original = ledger.os.stat
        for name in ('head.json', 'anchor.json', 'integrity.key', 'journal.lock', self.generation(0).name):
            with self.subTest(name=name):
                def changed_owner(path, *args, **kwargs):
                    info = original(path, *args, **kwargs)
                    if path == name and kwargs.get('dir_fd') is not None:
                        fields = list(info)
                        fields[4] += 1
                        return os.stat_result(fields)
                    return info
                with patch.object(ledger.os, 'stat', side_effect=changed_owner):
                    with self.assertRaisesRegex(mc.GuardError, 'ownership differs'):
                        ledger.Journal(self.path)
        link = self.root.parent / 'alias'
        link.symlink_to(self.root, target_is_directory=True)
        with self.assertRaisesRegex(mc.GuardError, 'symlink in journal directory'):
            ledger.Journal(link / 'head.json')
        with self.assertRaisesRegex(mc.GuardError, 'symlink in journal directory'):
            ledger.Journal.initialize(link / 'other.json', header(), state())

    def test_key_and_lock_replacement_independently_reach_identity_guard(self):
        for name in ('integrity.key', 'journal.lock'):
            with self.subTest(name=name):
                directory = self.root.parent / name
                path = directory / 'head.json'
                journal = ledger.Journal.initialize(path, header(), state())
                self.addCleanup(journal.close)
                raw = (directory / name).read_bytes()
                replacement = directory / 'new-file'
                replacement.write_bytes(raw)
                replacement.chmod(0o600)
                os.replace(replacement, directory / name)
                with self.assertRaisesRegex(mc.GuardError, 'anchor identity'):
                    ledger.Journal(path)

    def test_lock_replacement_during_held_lock_refuses_commit(self):
        original = self.path.read_bytes()
        with self.journal.locked() as current:
            self.replace(self.root / 'journal.lock', b'')
            with self.assertRaisesRegex(mc.GuardError, 'lock identity/content differs'):
                self.journal.commit(current)
        self.assertEqual(self.path.read_bytes(), original)

    def test_generation_creation_is_exclusive_and_never_overwrites_evidence(self):
        original = self.generation(0).read_bytes()
        with self.journal.locked():
            with self.assertRaises(FileExistsError):
                self.journal._create(self.generation(0).name, b'synthetic overwrite')
        self.assertEqual(self.generation(0).read_bytes(), original)
        self.assertEqual(self.journal.read(), state())

    def test_header_and_anchor_content_are_immutable_even_with_valid_mac(self):
        original = self.load(self.root / 'anchor.json')
        altered = copy.deepcopy(original)
        altered['header']['time']['hard_ns'] = 999
        altered['header_sha256'] = ledger.sha256(altered['header'])
        (self.root / 'anchor.json').write_bytes(encoded(self.sign(altered)))
        with self.assertRaisesRegex(mc.GuardError, 'anchor/key changed'):
            self.journal.read()
        with self.assertRaisesRegex(mc.GuardError, 'generation binding'):
            ledger.Journal(self.path)

    def test_anchor_version_initial_hash_and_key_bytes_have_independent_controls(self):
        for mutation, reason in [('version', 'anchor format'), ('initial', 'initial ancestry'),
                                 ('key_digest', 'key/head binding'), ('uuid', 'invalid journal UUID'),
                                 ('key_bytes', 'MAC differs')]:
            with self.subTest(mutation=mutation):
                directory = self.root.parent / ('anchor-' + mutation)
                path = directory / 'head.json'
                journal = ledger.Journal.initialize(path, header(), state())
                self.addCleanup(journal.close)
                anchor_path = directory / 'anchor.json'
                anchor = json.loads(anchor_path.read_bytes())
                if mutation == 'key_bytes':
                    (directory / 'integrity.key').write_bytes(b'x' * 32)
                else:
                    if mutation == 'version': anchor['format'] = 'fdb-creation-ledger-anchor/v2'
                    if mutation == 'initial': anchor['initial_sha256'] = 'f' * 64
                    if mutation == 'key_digest': anchor['key_sha256'] = 'f' * 64
                    if mutation == 'uuid': anchor['uuid'] = 'not-a-uuid'
                    body = {name: value for name, value in anchor.items() if name != 'mac'}
                    anchor['mac'] = hmac.new((directory / 'integrity.key').read_bytes(), encoded(body)[:-1], hashlib.sha256).hexdigest()
                    anchor_path.write_bytes(encoded(anchor))
                with self.assertRaisesRegex(mc.GuardError, reason):
                    ledger.Journal(path)

    def test_authenticated_ancestor_check_accepts_older_heads_not_foreign_or_future(self):
        initial = self.journal.checkpoint
        self.advance()
        intermediate = self.journal.checkpoint
        self.advance('CLOSING')
        observer = self.reopen()
        for checkpoint in (initial, intermediate, observer.checkpoint):
            self.assertIs(observer.is_ancestor(checkpoint), True)
        for checkpoint in (None, {}, {'generation': True, 'sha256': initial['sha256']},
                           {'generation': -1, 'sha256': initial['sha256']},
                           {'generation': 99, 'sha256': initial['sha256']},
                           {'generation': 0, 'sha256': 'f' * 64},
                           {'generation': 0, 'sha256': []}):
            with self.subTest(checkpoint=checkpoint):
                self.assertIs(observer.is_ancestor(checkpoint), False)

    def test_changed_mac_wrong_version_predecessor_uuid_header_and_gap_refuse(self):
        for mutation, reason in [('mac', 'MAC differs'), ('version', 'generation format'),
                                 ('previous', 'predecessor differs'), ('uuid', 'generation binding'),
                                 ('header', 'generation binding'), ('gap', 'generation gap')]:
            with self.subTest(mutation=mutation):
                directory = self.root.parent / ('chain-' + mutation)
                path = directory / 'head.json'
                journal = ledger.Journal.initialize(path, header(), state())
                self.addCleanup(journal.close)
                with journal.locked() as current:
                    current['events'].append({'reason': 'first'})
                    journal.commit(current)
                generation = directory / 'generation-00000000000000000001.json'
                record = json.loads(generation.read_bytes())
                if mutation == 'gap':
                    generation.rename(directory / 'generation-00000000000000000002.json')
                else:
                    if mutation == 'mac': record['state']['events'].append({'reason': 'tampered'})
                    if mutation == 'version': record['format'] = 'fdb-creation-ledger-generation/v2'
                    if mutation == 'previous': record['previous_sha256'] = 'f' * 64
                    if mutation == 'uuid': record['uuid'] = '00000000-0000-4000-8000-000000000000'
                    if mutation == 'header': record['header_sha256'] = 'f' * 64
                    if mutation != 'mac':
                        body = {key: value for key, value in record.items() if key != 'mac'}
                        record['mac'] = hmac.new((directory / 'integrity.key').read_bytes(), encoded(body)[:-1], hashlib.sha256).hexdigest()
                    generation.write_bytes(encoded(record))
                    path.write_bytes(encoded(record))
                with self.assertRaisesRegex(mc.GuardError, reason):
                    ledger.Journal(path)

    def test_rollback_fork_orphan_generation_and_head_without_generation_refuse(self):
        self.advance()
        old_head = self.path.read_bytes()
        self.advance('CLOSING')
        self.path.write_bytes(old_head)
        with self.assertRaisesRegex(mc.GuardError, 'head differs'):
            ledger.Journal(self.path)
        self.generation(2).unlink()
        with self.assertRaisesRegex(mc.GuardError, 'rollback or fork'):
            self.journal.read()
        self.assertEqual(self.journal.checkpoint['generation'], 2)
        separate = self.root.parent / 'fork'
        fork = ledger.Journal.initialize(separate / 'head.json', header(), state())
        self.addCleanup(fork.close)
        with fork.locked() as current:
            current['mode'] = 'CLOSING'
            fork.commit(current)
        record = json.loads((separate / 'head.json').read_bytes())
        record['state']['mode'] = 'OPEN'
        body = {key: value for key, value in record.items() if key != 'mac'}
        record['mac'] = hmac.new((separate / 'integrity.key').read_bytes(), encoded(body)[:-1], hashlib.sha256).hexdigest()
        (separate / 'head.json').write_bytes(encoded(record))
        (separate / 'generation-00000000000000000001.json').write_bytes(encoded(record))
        with self.assertRaisesRegex(mc.GuardError, 'rollback or fork'):
            fork.read()
        (separate / 'generation-00000000000000000001.json').unlink()
        with self.assertRaisesRegex(mc.GuardError, 'head differs'):
            ledger.Journal(separate / 'head.json')

    def test_commit_orders_file_and_directory_sync_before_and_after_head_rename(self):
        calls = []
        sync, rename = ledger.os.fsync, ledger.os.rename
        def synced(fd):
            calls.append('directory-sync' if stat.S_ISDIR(os.fstat(fd).st_mode) else 'file-sync')
            return sync(fd)
        def renamed(*args, **kwargs):
            calls.append('rename')
            return rename(*args, **kwargs)
        with patch.object(ledger.os, 'fsync', side_effect=synced), patch.object(ledger.os, 'rename', side_effect=renamed):
            self.advance()
        self.assertEqual(calls, ['file-sync', 'directory-sync', 'file-sync', 'rename', 'directory-sync'])

    def test_each_failed_sync_propagates_and_forbids_followup_commit(self):
        for failure in range(1, 5):
            with self.subTest(sync=failure):
                directory = self.root.parent / f'failed-sync-{failure}'
                path = directory / 'head.json'
                journal = ledger.Journal.initialize(path, header(), state())
                self.addCleanup(journal.close)
                old_head = path.read_bytes()
                calls = []
                original = ledger.os.fsync
                def sync(fd):
                    calls.append(fd)
                    if len(calls) == failure:
                        raise OSError('synthetic fsync failure')
                    return original(fd)
                with journal.locked() as current:
                    current['mode'] = 'CLOSING'
                    with patch.object(ledger.os, 'fsync', side_effect=sync):
                        with self.assertRaisesRegex(OSError, 'synthetic fsync failure'):
                            journal.commit(current)
                    with self.assertRaisesRegex(mc.GuardError, 'requires recovery'):
                        journal.commit(current)
                with self.assertRaisesRegex(mc.GuardError, 'requires recovery'):
                    journal.read()
                self.assertEqual(len(calls), failure)
                self.assertEqual(path.read_bytes() == old_head, failure < 4)
                if failure < 4:
                    with self.assertRaisesRegex(mc.GuardError, 'orphan generation|crash residue'):
                        ledger.Journal(path)

    def test_initialize_orders_every_durable_file_and_directory_before_head_publication(self):
        directory = self.root.parent / 'initial-order'
        path = directory / 'head.json'
        calls = []
        original_sync, original_rename = ledger.os.fsync, ledger.os.rename
        def sync(fd):
            target = Path(os.readlink(f'/proc/self/fd/{fd}'))
            if target == directory.parent: label = 'parent-directory'
            elif target == directory: label = 'journal-directory'
            elif target.name.startswith('.head-'): label = 'temporary-head'
            else: label = target.name
            calls.append(label)
            return original_sync(fd)
        def rename(source, target, **kwargs):
            self.assertTrue(source.startswith('.head-'))
            self.assertEqual(target, path.name)
            self.assertFalse(path.exists())
            self.assertEqual((directory / source).read_bytes(), (directory / ledger._generation(0)).read_bytes())
            calls.append('rename')
            return original_rename(source, target, **kwargs)
        with patch.object(ledger.os, 'fsync', side_effect=sync), patch.object(ledger.os, 'rename', side_effect=rename):
            journal = ledger.Journal.initialize(path, header(), state())
        self.addCleanup(journal.close)
        self.assertEqual(calls, ['parent-directory', ledger.KEY, ledger.LOCK, ledger._generation(0), ledger.ANCHOR,
                                 'journal-directory', 'temporary-head', 'rename', 'journal-directory'])
        self.assertEqual(journal.read(), state())
        self.assertEqual(journal.checkpoint['generation'], 0)
        self.assertEqual(path.read_bytes(), (directory / ledger._generation(0)).read_bytes())

    def test_each_initialization_sync_and_rename_failure_preserves_actual_residue(self):
        for failure in (*range(1, 9), 'rename'):
            with self.subTest(failure=failure):
                directory = self.root.parent / ('initial-failure-' + str(failure))
                path = directory / 'head.json'
                original_sync, original_rename = ledger.os.fsync, ledger.os.rename
                synced, renamed = [], []
                def sync(fd):
                    synced.append(Path(os.readlink(f'/proc/self/fd/{fd}')))
                    if len(synced) == failure:
                        raise OSError('synthetic initialization sync failure')
                    return original_sync(fd)
                def rename(source, target, **kwargs):
                    renamed.append((source, target))
                    if failure == 'rename':
                        raise OSError('synthetic initialization rename failure')
                    return original_rename(source, target, **kwargs)
                with patch.object(ledger.os, 'fsync', side_effect=sync), patch.object(ledger.os, 'rename', side_effect=rename):
                    with self.assertRaisesRegex(OSError, 'synthetic initialization .* failure'):
                        ledger.Journal.initialize(path, header(), state())
                self.assertEqual(len(synced), 7 if failure == 'rename' else failure)
                self.assertEqual(len(renamed), 1 if failure in ('rename', 8) else 0)
                before = {entry.name: entry.read_bytes() for entry in directory.iterdir()}
                expected = set()
                if failure != 1: expected.add(ledger.KEY)
                if failure not in (1, 2): expected.add(ledger.LOCK)
                if failure not in (1, 2, 3): expected.update((ledger.ANCHOR, ledger._generation(0)))
                temporary = {name for name in before if name.startswith('.head-')}
                if failure in (7, 'rename'):
                    self.assertEqual(len(temporary), 1)
                    expected |= temporary
                else:
                    self.assertEqual(temporary, set())
                if failure == 8: expected.add(path.name)
                self.assertEqual(set(before), expected)
                self.assertTrue(all(stat.S_IMODE(entry.stat().st_mode) == 0o600 for entry in directory.iterdir()))
                if before:
                    with self.assertRaisesRegex(mc.GuardError, 'not empty'):
                        ledger.Journal.initialize(path, header(), state())
                    self.assertEqual({entry.name: entry.read_bytes() for entry in directory.iterdir()}, before)
                else:
                    # Parent sync failed before any journal evidence existed: a fresh creation is safe.
                    self.assertEqual(failure, 1)
                    fresh = ledger.Journal.initialize(path, header(), state())
                    self.addCleanup(fresh.close)
                    self.assertEqual(fresh.read(), state())
                if before and failure != 8:
                    with self.assertRaises((mc.GuardError, OSError)):
                        ledger.Journal(path)
                if failure == 8:
                    self.assertEqual(before[path.name], before[ledger._generation(0)])

    def test_failed_initial_sync_returns_no_journal_and_never_silently_reinitializes(self):
        directory = self.root.parent / 'failed-initialization'
        with patch.object(ledger.os, 'fsync', side_effect=OSError('initial sync failure')):
            with self.assertRaisesRegex(OSError, 'initial sync failure'):
                ledger.Journal.initialize(directory / 'head.json', header(), state())
        with self.assertRaises((mc.GuardError, OSError)):
            ledger.Journal(directory / 'head.json')
        # Seed evidence: initialization may not erase a failed attempt to resume.
        (directory / 'partial').write_bytes(b'synthetic crash evidence')
        with self.assertRaisesRegex(mc.GuardError, 'not empty'):
            ledger.Journal.initialize(directory / 'head.json', header(), state())

    def test_failed_rename_and_partial_write_preserve_residue_and_fail_closed(self):
        old = self.path.read_bytes()
        with self.journal.locked() as current:
            with patch.object(ledger.os, 'rename', side_effect=OSError('synthetic rename failure')):
                with self.assertRaisesRegex(OSError, 'rename failure'):
                    self.journal.commit(current)
        self.assertEqual(self.path.read_bytes(), old)
        self.assertTrue(any(path.name.startswith('.head-') for path in self.root.iterdir()))
        with self.assertRaisesRegex(mc.GuardError, 'crash residue'):
            ledger.Journal(self.path)
        directory = self.root.parent / 'partial-write'
        journal = ledger.Journal.initialize(directory / 'head.json', header(), state())
        self.addCleanup(journal.close)
        def partial(name, data):
            fd = os.open(directory / name, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
            with os.fdopen(fd, 'wb') as stream:
                stream.write(b'{')
            raise OSError('synthetic write failure')
        with journal.locked() as current:
            with patch.object(journal, '_create', side_effect=partial):
                with self.assertRaisesRegex(OSError, 'write failure'):
                    journal.commit(current)
        with self.assertRaisesRegex(mc.GuardError, 'malformed journal JSON'):
            ledger.Journal(directory / 'head.json')

    def test_nonblocking_peek_observes_committed_lease_during_writer_lock(self):
        observer = self.reopen()
        with self.journal.locked() as current:
            current['active_lease'] = {'deadline_ns': 12345, 'state': 'SENDING'}
            self.journal.commit(current)
            original = ledger.fcntl.flock
            calls = []
            def nonblocking(fd, operation):
                calls.append(operation)
                self.assertEqual(operation, ledger.fcntl.LOCK_EX | ledger.fcntl.LOCK_NB)
                return original(fd, operation)
            with patch.object(ledger.fcntl, 'flock', side_effect=nonblocking):
                observed = observer.peek()
            self.assertEqual(calls, [ledger.fcntl.LOCK_EX | ledger.fcntl.LOCK_NB])
            self.assertEqual(observed['active_lease'], {'deadline_ns': 12345, 'state': 'SENDING'})
            self.assertEqual(observer.checkpoint, self.journal.checkpoint)
            self.assertEqual(observer.binding, self.journal.binding)
            with self.assertRaisesRegex(mc.GuardError, 'requires held lock'):
                observer.commit(observed)
        self.assertEqual(observer.peek(), self.journal.read())

    def test_strict_nonblocking_read_keeps_lock_through_validation_and_refuses_residue(self):
        observer = self.reopen()
        self.assertEqual(observer.read_if_available(), state())
        original = ledger.fcntl.flock
        calls = []
        def nonblocking(fd, operation):
            calls.append(operation)
            self.assertIn(operation, (ledger.fcntl.LOCK_EX | ledger.fcntl.LOCK_NB, ledger.fcntl.LOCK_UN))
            return original(fd, operation)
        with self.journal.locked():
            with patch.object(ledger.fcntl, 'flock', side_effect=nonblocking):
                self.assertIsNone(observer.read_if_available())
            self.assertEqual(calls, [ledger.fcntl.LOCK_EX | ledger.fcntl.LOCK_NB])
        def validate(lock_identity, observational=False):
            fd = os.open(self.root / ledger.LOCK, os.O_RDWR)
            try:
                with self.assertRaises(BlockingIOError):
                    original(fd, ledger.fcntl.LOCK_EX | ledger.fcntl.LOCK_NB)
            finally:
                os.close(fd)
            return original_load(lock_identity, observational)
        original_load = observer._load
        with patch.object(observer, '_load', side_effect=validate):
            self.assertEqual(observer.read_if_available(), state())
        self.generation(1).write_bytes(b'partial unheaded generation')
        self.generation(1).chmod(0o600)
        with self.assertRaisesRegex(mc.GuardError, 'malformed journal JSON'):
            observer.read_if_available()
        with self.assertRaisesRegex(mc.GuardError, 'requires recovery'):
            observer.read_if_available()

    def test_peek_retries_real_publication_at_temporary_open_and_head_identity_check(self):
        for boundary in ('temporary-open', 'head-identity', 'head-fstat', 'temporary-fstat-promoted-replaced'):
            with self.subTest(boundary=boundary):
                with self.journal.locked() as current:
                    current['active_lease'] = {'intent': 0, 'deadline_ns': 12345}
                    self.journal.commit(current)
                observer = self.reopen()
                checkpoint = observer.checkpoint
                staged, publish, finished = threading.Event(), threading.Event(), threading.Event()
                errors, raced, renames = [], [], []
                original_rename, original_stat = ledger.os.rename, ledger.os.stat
                original_open, original_fstat = ledger.os.open, ledger.os.fstat
                original_file = observer._file
                head_stats, head_fds = [], []

                def paused_rename(source, target, **kwargs):
                    renames.append((source, target))
                    staged.set()
                    if not publish.wait(5):
                        raise AssertionError('observer never reached publication boundary')
                    return original_rename(source, target, **kwargs)

                def writer():
                    try:
                        with self.journal.locked() as fresh:
                            fresh['active_lease'] = None
                            self.journal.commit(fresh)
                            if boundary == 'temporary-fstat-promoted-replaced':
                                fresh['events'].append({'event': 'next publication replaces promoted temporary'})
                                self.journal.commit(fresh)
                    except BaseException as error:
                        errors.append(error)
                    finally:
                        finished.set()

                def complete_publication():
                    raced.append(boundary)
                    publish.set()
                    self.assertTrue(finished.wait(5), 'writer did not finish real publication')
                    self.assertEqual(errors, [])

                def opened(name, *args, **kwargs):
                    if boundary == 'temporary-open' and name.startswith('.head-') and not raced:
                        complete_publication()
                    return original_file(name, *args, **kwargs)

                def stated(name, *args, **kwargs):
                    if (boundary == 'head-identity' and name == self.path.name
                            and kwargs.get('dir_fd') == observer._directory):
                        head_stats.append(name)
                        if len(head_stats) == 2:
                            complete_publication()
                    return original_stat(name, *args, **kwargs)

                def open_head(name, *args, **kwargs):
                    fd = original_open(name, *args, **kwargs)
                    mutable = (boundary == 'head-fstat' and name == self.path.name
                               or boundary == 'temporary-fstat-promoted-replaced' and name.startswith('.head-'))
                    if mutable and kwargs.get('dir_fd') == observer._directory:
                        self.assertEqual(original_fstat(fd).st_nlink, 1)
                        head_fds.append(fd)
                    return fd

                def fstat_head(fd):
                    if fd in head_fds and not raced:
                        complete_publication()
                        info = original_fstat(fd)
                        self.assertEqual(info.st_nlink, 0, 'real rename must unlink the opened mutable inode')
                        if boundary == 'temporary-fstat-promoted-replaced':
                            self.assertFalse(any(path.name.startswith('.head-') for path in self.root.iterdir()))
                        self.assertNotEqual(info.st_ino, self.path.stat().st_ino)
                        return info
                    return original_fstat(fd)

                with patch.object(ledger.os, 'rename', side_effect=paused_rename), \
                        patch.object(observer, '_file', side_effect=opened), \
                        patch.object(ledger.os, 'stat', side_effect=stated), \
                        patch.object(ledger.os, 'open', side_effect=open_head), \
                        patch.object(ledger.os, 'fstat', side_effect=fstat_head):
                    thread = threading.Thread(target=writer)
                    thread.start()
                    try:
                        self.assertTrue(staged.wait(5), 'writer did not stage generation and temporary head')
                        observed = observer.peek()
                    finally:
                        publish.set()
                        thread.join(5)
                    self.assertFalse(thread.is_alive(), 'writer leaked after publication control')
                self.assertEqual(errors, [])
                self.assertEqual(raced, [boundary])
                self.assertIsNone(observed['active_lease'])
                advances = 2 if boundary == 'temporary-fstat-promoted-replaced' else 1
                self.assertEqual(len(renames), advances)
                self.assertTrue(all(source.startswith('.head-') and target == self.path.name for source, target in renames))
                self.assertEqual(observer.checkpoint['generation'], checkpoint['generation'] + advances)
                self.assertEqual(observer.checkpoint, self.journal.checkpoint)
                self.assertTrue(observer.is_ancestor(checkpoint))
                self.assertEqual(observer.read_if_available(), observed)

    def test_peek_defers_after_three_real_replacements_without_losing_authenticated_watchdog_view(self):
        with self.journal.locked() as current:
            current['active_lease'] = {'intent': 0, 'deadline_ns': 12345}
            self.journal.commit(current)
        observer = self.reopen()
        last = observer.peek()
        checkpoint, binding, original_header = observer.checkpoint, observer.binding, observer.header
        original_stat = ledger.os.stat
        head_stats, replacements, lock_calls = [], [], []
        original_flock = ledger.fcntl.flock
        with self.journal.locked() as current:
            def stated(name, *args, **kwargs):
                if name == self.path.name and kwargs.get('dir_fd') == observer._directory:
                    head_stats.append(name)
                    if len(head_stats) % 2 == 0:
                        current['active_lease'] = None
                        current['events'].append({'event': 'concurrent-publication', 'sequence': len(replacements)})
                        self.journal.commit(current)
                        replacements.append(self.journal.checkpoint)
                return original_stat(name, *args, **kwargs)
            def nonblocking(fd, operation):
                lock_calls.append(operation)
                self.assertEqual(operation, ledger.fcntl.LOCK_EX | ledger.fcntl.LOCK_NB)
                return original_flock(fd, operation)
            with patch.object(ledger.os, 'stat', side_effect=stated), \
                    patch.object(ledger.fcntl, 'flock', side_effect=nonblocking):
                with self.assertRaisesRegex(ledger.ObservationDeferred, 'observation deferred during publication'):
                    observer.peek()
            self.assertEqual(len(replacements), 3)
            self.assertEqual(lock_calls, [ledger.fcntl.LOCK_EX | ledger.fcntl.LOCK_NB] * 3)
            self.assertEqual(observer.checkpoint, checkpoint)
            self.assertEqual(observer.binding, binding)
            self.assertEqual(observer.header, original_header)
            self.assertTrue(observer.is_ancestor(checkpoint))
            self.assertFalse(observer.is_ancestor(replacements[-1]))
            self.assertEqual(last['active_lease'], {'intent': 0, 'deadline_ns': 12345})
            self.assertIsNone(observer.read_if_available())
        confirmed = observer.read_if_available()
        self.assertIsNone(confirmed['active_lease'])
        self.assertEqual(observer.checkpoint, replacements[-1])
        self.assertTrue(observer.is_ancestor(checkpoint))

    def test_peek_never_defers_acquired_lock_identity_race_or_missing_immutable_file(self):
        observer = self.reopen()
        original_stat = ledger.os.stat
        head_stats = []
        raw = self.path.read_bytes()
        def replaced(name, *args, **kwargs):
            if name == self.path.name and kwargs.get('dir_fd') == observer._directory:
                head_stats.append(name)
                if len(head_stats) == 2:
                    self.replace(self.path, raw)
            return original_stat(name, *args, **kwargs)
        with patch.object(ledger.os, 'stat', side_effect=replaced):
            with self.assertRaisesRegex(mc.GuardError, 'file identity changed during open') as caught:
                observer.peek()
        self.assertNotIsInstance(caught.exception, ledger.ObservationDeferred)
        with self.assertRaisesRegex(mc.GuardError, 'requires recovery'):
            observer.peek()
        other = self.reopen()
        key = self.root / ledger.KEY
        with self.journal.locked():
            key.rename(self.root / 'saved-key')
            try:
                with self.assertRaises(FileNotFoundError):
                    other.peek()
            finally:
                (self.root / 'saved-key').rename(key)
        with self.assertRaisesRegex(mc.GuardError, 'requires recovery'):
            other.peek()

    def test_unlinked_open_fds_remain_fatal_for_strict_reads_immutable_files_and_hardlinks(self):
        cases = [('strict-peek', 'head.json', 'peek', False, False),
                 ('strict-read', 'head.json', 'read', False, False),
                 ('strict-nonblocking', 'head.json', 'read_if_available', False, False),
                 ('busy-anchor', ledger.ANCHOR, 'peek', True, False),
                 ('busy-generation', ledger._generation(0), 'peek', True, False),
                 ('busy-unlinked-permissions', 'head.json', 'peek', True, False),
                 ('busy-hardlink', 'head.json', 'peek', True, True)]
        for label, name, reader, busy, hardlink in cases:
            with self.subTest(case=label):
                path = self.root.parent / label / 'head.json'
                writer = ledger.Journal.initialize(path, header(), state())
                self.addCleanup(writer.close)
                observer = ledger.Journal(path)
                self.addCleanup(observer.close)
                target, raw = path.parent / name, (path.parent / name).read_bytes()
                original_open, original_fstat = ledger.os.open, ledger.os.fstat
                opened, raced = [], []

                def open_target(filename, *args, **kwargs):
                    fd = original_open(filename, *args, **kwargs)
                    if filename == name and kwargs.get('dir_fd') == observer._directory:
                        opened.append(fd)
                    return fd

                def fstat_target(fd):
                    if fd in opened and not raced:
                        raced.append(fd)
                        if hardlink:
                            os.link(target, self.root.parent / ('linked-' + label))
                        else:
                            self.replace(target, raw)
                        if label == 'busy-unlinked-permissions':
                            os.fchmod(fd, 0o640)
                        info = original_fstat(fd)
                        self.assertEqual(info.st_nlink, 2 if hardlink else 0)
                        return info
                    return original_fstat(fd)

                with writer.locked() if busy else contextlib.nullcontext():
                    with patch.object(ledger.os, 'open', side_effect=open_target), \
                            patch.object(ledger.os, 'fstat', side_effect=fstat_target):
                        reason = ('permissions' if label == 'busy-unlinked-permissions' else
                                  'multiple links' if busy else 'identity changed during open')
                        with self.assertRaisesRegex(mc.GuardError, reason) as caught:
                            getattr(observer, reader)()
                    self.assertNotIsInstance(caught.exception, ledger.ObservationDeferred)
                self.assertEqual(len(raced), 1)
                with self.assertRaisesRegex(mc.GuardError, 'requires recovery'):
                    observer.peek()

    def test_peek_revalidates_authenticated_head_instead_of_trusting_cached_state(self):
        observer = self.reopen()
        original = self.path.read_bytes()
        changed = self.load(self.path)
        changed['state']['active_lease'] = {'deadline_ns': 99999}
        with self.journal.locked():
            self.path.write_bytes(encoded(changed))
            with self.assertRaisesRegex(mc.GuardError, 'MAC differs'):
                observer.peek()
            self.path.write_bytes(original)
            with self.assertRaisesRegex(mc.GuardError, 'requires recovery'):
                observer.peek()
            self.assertIs(observer.is_ancestor(self.journal.checkpoint), False)

    def test_peek_only_ignores_one_staged_generation_and_temp_while_lock_is_held(self):
        observer = self.reopen()
        with self.journal.locked():
            self.generation(1).write_bytes(b'partial in-progress generation')
            self.generation(1).chmod(0o600)
            temporary = self.root / ('.head-' + 'a' * 32)
            temporary.write_bytes(b'in-progress head')
            temporary.chmod(0o600)
            self.assertEqual(observer.peek(), state())
            self.generation(2).write_bytes(b'too many in-progress generations')
            self.generation(2).chmod(0o600)
            with self.assertRaisesRegex(mc.GuardError, 'multiple unheaded generations'):
                observer.peek()
            self.generation(2).unlink()
        with self.assertRaisesRegex(mc.GuardError, 'crash residue'):
            observer.peek()
        with self.assertRaisesRegex(mc.GuardError, 'crash residue'):
            ledger.Journal(self.path)


class RecoveryTests(JournalCase):
    def prepare(self, journal=None, headed_state='SENDING', orphan_state='ACKNOWLEDGED', operation='create', partial=None, temporary=None,
                headed_evidence='headed-evidence', initial_name=None):
        journal = self.journal if journal is None else journal
        rid = None if operation == 'create' else 201
        intent = dict(sequence=0, operation=operation, kind='network', name='synthetic-network',
                      state=headed_state, resource_id=rid, ack=None, actions=[], request_sha256='a' * 64)
        def populate_head(current):
            if headed_state is not None:
                current['intents'].append(copy.deepcopy(intent))
            current['events'].append({'event': headed_evidence})
            if headed_state in {'SENDING', 'UNKNOWN'}:
                current['active_lease'] = {'intent': 0, 'deadline_ns': 1234}
        if initial_name is None:
            with journal.locked() as current:
                populate_head(current)
                journal.commit(current)
        else:
            self.assertIs(journal, self.journal)
            initial = state()
            populate_head(initial)
            journal = self.independent_journal(initial_name, initial)
        headed = journal.read()
        checkpoint = journal.checkpoint
        candidate = copy.deepcopy(headed)
        if headed_state is None:
            intent['state'] = orphan_state
            candidate['intents'].append(copy.deepcopy(intent))
        else:
            candidate['intents'][0]['state'] = orphan_state
        if orphan_state == 'ACKNOWLEDGED':
            candidate['intents'][0].update(resource_id=201, ack={'status': 201, 'body_sha256': 'b' * 64}, actions=[501])
            candidate['owned'].append({'kind': 'network', 'id': 201, 'intent': 0, 'evidence_sha256': 'c' * 64})
            candidate['associations'].append({'id': 301, 'server_id': 201, 'proof': 'retained opaque association'})
            candidate['active_lease'] = None
        candidate['events'].append({'event': 'unheaded-evidence'})
        record = dict(format='fdb-creation-ledger-generation/v1', uuid=journal.binding['uuid'],
                      header_sha256=journal.binding['header_sha256'], generation=checkpoint['generation'] + 1,
                      previous_sha256=checkpoint['sha256'], state=candidate)
        key = (journal.path.parent / 'integrity.key').read_bytes()
        record['mac'] = hmac.new(key, encoded(record)[:-1], hashlib.sha256).hexdigest()
        raw = encoded(record)
        next_path = journal.path.parent / f'generation-{record["generation"]:020d}.json'
        next_path.write_bytes(raw if partial is None else raw[:partial])
        next_path.chmod(0o600)
        if temporary is not None:
            temp = journal.path.parent / ('.head-' + 'd' * 32)
            temp.write_bytes(raw if temporary == 'full' else raw[:temporary])
            temp.chmod(0o600)
        return dict(journal=journal, path=journal.path, headed=headed, checkpoint=checkpoint, candidate=candidate,
                    record=record, raw=raw, next_path=next_path, head_raw=journal.path.read_bytes(), binding=journal.binding)

    def rewrite_orphan(self, case, candidate):
        record = dict(format=ledger.GENERATION_FORMAT, uuid=case['binding']['uuid'],
                      header_sha256=case['binding']['header_sha256'], generation=case['checkpoint']['generation'] + 1,
                      previous_sha256=case['checkpoint']['sha256'], state=copy.deepcopy(candidate))
        key = (case['path'].parent / ledger.KEY).read_bytes()
        record['mac'] = hmac.new(key, encoded(record)[:-1], hashlib.sha256).hexdigest()
        raw = encoded(record)
        path = case['path'].parent / ledger._generation(record['generation'])
        path.write_bytes(raw)
        path.chmod(0o600)
        case.update(candidate=copy.deepcopy(candidate), record=record, raw=raw, next_path=path)
        self.assertEqual(ledger._authenticated(json.loads(raw), key)['state'], candidate)

    def retained_evidence_case(self, name):
        case = self.prepare(self.independent_journal(name))
        case['next_path'].unlink()
        case['candidate']['events'].append(dict(event='observed-retirement', kind='primary-ip', id=301,
                                                source='synthetic-complete-census', unix_ns=1234))
        case['candidate']['protections'] = dict(names=['literal-protected'], ids=[701],
            by_kind={kind: [801] if kind == 'server' else [901] if kind == 'primary-ip' else [] for kind in mc.PROTECT_KINDS})
        case['candidate']['intents'][0]['actions'] = [501, 502]
        with case['journal'].locked():
            case['journal'].commit(case['candidate'])
        case.update(headed=case['journal'].read(), checkpoint=case['journal'].checkpoint,
                    head_raw=case['path'].read_bytes())
        self.assertTrue(case['headed']['owned'])
        self.assertTrue(case['headed']['associations'])
        self.assertIsNotNone(case['headed']['intents'][0]['ack'])
        candidate = copy.deepcopy(case['headed'])
        candidate['events'].append({'event': 'later authenticated successor'})
        self.rewrite_orphan(case, candidate)
        return case

    def publish_fixture_head(self, case, headed):
        case['next_path'].unlink()
        with case['journal'].locked():
            case['journal'].commit(headed)
        case.update(headed=case['journal'].read(), checkpoint=case['journal'].checkpoint,
                    head_raw=case['path'].read_bytes())
        candidate = copy.deepcopy(case['headed'])
        candidate['events'].append({'event': 'later fixture generation'})
        self.rewrite_orphan(case, candidate)

    def intent_binding_case(self, name):
        case = self.prepare(self.independent_journal(name), headed_state='UNKNOWN', orphan_state='UNKNOWN')
        headed = copy.deepcopy(case['headed'])
        producer = copy.deepcopy(header()['producer'])
        lease = dict(intent=0, actor=producer, deadline_ns=1234, monotonic_deadline_ns=5678)
        headed['active_lease'] = copy.deepcopy(lease)
        headed['intents'][0].update(nonce='1' * 32, descriptor=dict(name='synthetic-network', ip_range='10.42.0.0/24',
            labels={'intent': '1' * 32}), pre_ids={kind: [] for kind in mc.PROTECT_KINDS},
            observed_start_ns=101, observed_end_ns=102, producer=producer,
            send_lease=lease, send_generation=case['checkpoint']['generation'] + 1,
            acknowledgement_validated=False, required_action_identified=False,
            request_proof={'sha256': 'e' * 64})
        self.publish_fixture_head(case, headed)
        return case

    def assert_orphan_refused_before_callbacks(self, case, reason, label):
        before = {path.name: path.read_bytes() for path in case['path'].parent.iterdir()}
        evidence = self.root.parent / ('refused-retention-' + label)
        authorized, normalized = [], []
        with self.assertRaisesRegex(mc.GuardError, reason):
            self.recover(case, authorize=lambda *args: authorized.append(args),
                         close_state=lambda *args: normalized.append(args), evidence=evidence)
        self.assertEqual(authorized, [])
        self.assertEqual(normalized, [])
        self.assertEqual({path.name: path.read_bytes() for path in case['path'].parent.iterdir()}, before)
        self.assertFalse(evidence.exists())
        self.assertFalse((case['path'].parent / ledger.RECOVERY).exists())

    def normalize(self, candidate, receipt):
        headed = receipt['headed_state']['intents']
        for index, intent in enumerate(candidate['intents']):
            previous = headed[index]['state'] if index < len(headed) else None
            if previous in {'SENDING', 'UNKNOWN'} or intent['state'] == 'UNKNOWN':
                intent['state'] = 'UNKNOWN'
            elif intent['state'] in {'PREPARED', 'SENDING'}:
                intent['state'] = 'NOT_DISPATCHED'
        candidate['mode'], candidate['active_lease'] = 'CLOSING', None
        candidate['events'].append({'event': 'normalized-closed', 'archive': receipt['archive']})
        return candidate

    def recover(self, case, authorize=None, close_state=None, evidence=None):
        def allowed(header_value, binding, checkpoint, headed):
            self.assertEqual(header_value, header())
            self.assertEqual(binding, case['binding'])
            self.assertEqual(checkpoint, case['checkpoint'])
            self.assertEqual(headed, case['headed'])
            return {'producer_quiesced': True, 'old_guard_gone': True, 'recovery_guard': {'pid': 777}}
        recovered = ledger.Journal.recover_closed(case['path'],
            evidence_directory=evidence or self.root.parent / 'recovery-evidence',
            authorize=allowed if authorize is None else authorize,
            close_state=self.normalize if close_state is None else close_state)
        self.addCleanup(recovered.close)
        return recovered

    def independent_journal(self, name, initial_state=None):
        journal = ledger.Journal.initialize(self.root.parent / name / 'head.json', header(),
                                            state() if initial_state is None else initial_state)
        self.addCleanup(journal.close)
        return journal

    def test_valid_orphan_ack_recovery_retains_ids_proofs_and_authenticated_ancestry(self):
        case = self.prepare()
        with self.assertRaisesRegex(mc.GuardError, 'orphan generation'):
            ledger.Journal(self.path)
        recovered = self.recover(case)
        current = recovered.read()
        self.assertEqual(current['mode'], 'CLOSING')
        self.assertIsNone(current['active_lease'])
        self.assertEqual(current['intents'][0]['state'], 'UNKNOWN')
        self.assertEqual(current['intents'][0]['resource_id'], 201)
        self.assertEqual(current['intents'][0]['ack'], {'status': 201, 'body_sha256': 'b' * 64})
        self.assertEqual(current['intents'][0]['actions'], [501])
        self.assertEqual(current['owned'], case['candidate']['owned'])
        self.assertEqual(current['associations'], case['candidate']['associations'])
        self.assertEqual(case['next_path'].read_bytes(), case['raw'])
        self.assertEqual(recovered.binding, case['binding'])
        self.assertEqual(recovered.checkpoint['generation'], 3)
        self.assertTrue(recovered.is_ancestor(case['checkpoint']))
        self.assertTrue(recovered.is_ancestor({'generation': 2, 'sha256': hashlib.sha256(case['raw'][:-1]).hexdigest()}))
        self.assertFalse((self.root / ledger.RECOVERY).exists())
        evidence = current['events'][-1]['evidence']
        archive = Path(evidence['archive'])
        manifest_raw = (archive / 'manifest.json').read_bytes()
        manifest = json.loads(manifest_raw)
        self.assertEqual(hashlib.sha256(manifest_raw).hexdigest(), evidence['manifest_sha256'])
        body = {name: value for name, value in manifest.items() if name != 'mac'}
        self.assertEqual(manifest['mac'], hmac.new((self.root / 'integrity.key').read_bytes(), encoded(body)[:-1], hashlib.sha256).hexdigest())
        self.assertEqual(manifest['authorization']['producer_quiesced'], True)
        self.assertEqual(manifest['headed_checkpoint'], case['checkpoint'])
        sources = {item['source']: (archive / item['archive_name']).read_bytes() for item in manifest['files']}
        self.assertEqual(sources['head.json'], case['head_raw'])
        self.assertEqual(sources[case['next_path'].name], case['raw'])
        self.assertNotIn('integrity.key', sources)
        for item in manifest['files']:
            raw = (archive / item['archive_name']).read_bytes()
            self.assertEqual(hashlib.sha256(raw).hexdigest(), item['sha256'])
            self.assertEqual(len(raw), item['size'])
        self.assertEqual(stat.S_IMODE(archive.stat().st_mode), 0o700)
        self.assertTrue(all(stat.S_IMODE(path.stat().st_mode) == 0o600 for path in archive.iterdir()))

    def test_full_and_partial_temporary_heads_preserve_original_bytes_outside_journal(self):
        for label, temporary in [('full', 'full'), ('partial', 80)]:
            with self.subTest(temporary=label):
                case = self.prepare(self.independent_journal('temporary-' + label), temporary=temporary)
                temp = case['path'].parent / ('.head-' + 'd' * 32)
                raw = temp.read_bytes()
                current = self.recover(case).read()
                self.assertFalse(temp.exists())
                archive = Path(current['events'][-1]['evidence']['archive'])
                manifest = json.loads((archive / 'manifest.json').read_bytes())
                item = next(row for row in manifest['files'] if row['source'] == temp.name)
                self.assertEqual((archive / item['archive_name']).read_bytes(), raw)
                self.assertEqual(current['intents'][0]['state'], 'UNKNOWN')

    def partial_offsets(self, raw):
        mac = raw.index(b',"mac":"') + len(b',"mac":"')
        previous = raw.index(b',"previous_sha256":"') + len(b',"previous_sha256":"')
        state_start = raw.index(b',"state":') + len(b',"state":')
        state_end = raw.rindex(b',"uuid":')
        uuid_start = state_end + len(b',"uuid":"')
        null = raw.index(b'"active_lease":null', state_start) + len(b'"active_lease":')
        array = raw.index(b'"associations":[', state_start) + len(b'"associations":[')
        string = raw.index(b'headed-evidence', state_start) + len(b'headed-evi')
        return dict(mac_first=mac + 1, mac_middle=mac + 32, mac_end=mac + 64,
                    predecessor_key=mac + 64 + len(b'","previous_sha'), predecessor_first=previous + 1,
                    predecessor_middle=previous + 32, predecessor_end=previous + 64,
                    state_open=state_start + 1, state_null_first=null + 1, state_null_last=null + 3,
                    state_array=array, state_string=string, state_complete=state_end,
                    uuid_key=state_end + len(b',"uu'), uuid_first=uuid_start + 1,
                    uuid_middle=uuid_start + 18, uuid_end=uuid_start + 36, uuid_quote=uuid_start + 37)

    def assert_archived_partial(self, current, source, raw):
        archive = Path(current['events'][-1]['evidence']['archive'])
        manifest = json.loads((archive / 'manifest.json').read_bytes())
        row = next(item for item in manifest['files'] if item['source'] == source)
        self.assertEqual((archive / row['archive_name']).read_bytes(), raw)
        self.assertEqual(row['sha256'], hashlib.sha256(raw).hexdigest())
        self.assertEqual(row['size'], len(raw))
        self.assertNotIn('integrity.key', [item['source'] for item in manifest['files']])

    def test_later_canonical_partial_orphans_recover_headed_unknown_and_archive_exact_bytes(self):
        probe = self.prepare(self.independent_journal('partial-offset-probe'))
        for label in self.partial_offsets(probe['raw']):
            with self.subTest(fragment=label):
                case = self.prepare(self.independent_journal('partial-orphan-' + label))
                offset = self.partial_offsets(case['raw'])[label]
                partial = case['raw'][:offset]
                self.assertGreater(offset, case['raw'].index(b',"mac":"'))
                self.assertLess(offset, len(case['raw']) - 1)
                case['next_path'].write_bytes(partial)
                recovered = self.recover(case)
                current = recovered.read()
                self.assertEqual(current['mode'], 'CLOSING')
                self.assertIsNone(current['active_lease'])
                self.assertEqual(current['intents'][0]['state'], 'UNKNOWN')
                self.assertIsNone(current['intents'][0]['resource_id'])
                self.assertIsNone(current['intents'][0]['ack'])
                self.assertEqual(current['intents'][0]['actions'], [])
                self.assertEqual(current['owned'], [])
                self.assertEqual(current['associations'], [])
                self.assertEqual(current['events'][-1]['evidence']['candidate_source'], 'headed')
                self.assertTrue(recovered.is_ancestor(case['checkpoint']))
                self.assert_archived_partial(current, case['next_path'].name, partial)
                self.assertEqual(case['next_path'].read_bytes(), case['path'].read_bytes())
                self.assertFalse((case['path'].parent / ledger.RECOVERY).exists())

    def test_later_canonical_partial_temporary_heads_retain_authenticated_orphan_ack_and_bytes(self):
        probe = self.prepare(self.independent_journal('temporary-offset-probe'))
        for label in self.partial_offsets(probe['raw']):
            with self.subTest(fragment=label):
                case = self.prepare(self.independent_journal('partial-temporary-' + label))
                partial = case['raw'][:self.partial_offsets(case['raw'])[label]]
                temporary = case['path'].parent / ('.head-' + 'd' * 32)
                temporary.write_bytes(partial)
                temporary.chmod(0o600)
                recovered = self.recover(case)
                current = recovered.read()
                self.assertEqual(current['mode'], 'CLOSING')
                self.assertIsNone(current['active_lease'])
                self.assertEqual(current['intents'][0]['state'], 'UNKNOWN')
                self.assertEqual(current['intents'][0]['resource_id'], 201)
                self.assertEqual(current['intents'][0]['ack'], case['candidate']['intents'][0]['ack'])
                self.assertEqual(current['intents'][0]['actions'], [501])
                self.assertEqual(current['owned'], case['candidate']['owned'])
                self.assertEqual(current['associations'], case['candidate']['associations'])
                self.assertEqual(current['events'][-1]['evidence']['candidate_source'], 'orphan')
                self.assertEqual(case['next_path'].read_bytes(), case['raw'])
                self.assertFalse(temporary.exists())
                self.assert_archived_partial(current, temporary.name, partial)
                self.assert_archived_partial(current, case['next_path'].name, case['raw'])
                self.assertTrue(recovered.is_ancestor(case['checkpoint']))

    def test_multibyte_partial_orphans_and_temporary_heads_retain_dispatch_state_and_exact_bytes(self):
        evidence = 'headed-é-€-😀-evidence'
        for placement in ('orphan', 'temporary'):
            for headed in ('SENDING', 'PREPARED'):
                for character in ('é', '€', '😀'):
                    character_bytes = character.encode('utf-8')
                    for interior in range(1, len(character_bytes)):
                        label = f'{placement}-{headed}-{ord(character):x}-{interior}'
                        with self.subTest(fragment=label):
                            case = self.prepare(self.independent_journal('utf8-' + label), headed_state=headed,
                                                orphan_state='SENDING', headed_evidence=evidence)
                            offset = case['raw'].index(character_bytes) + interior
                            partial = case['raw'][:offset]
                            with self.assertRaises(UnicodeDecodeError):
                                partial.decode('utf-8')
                            if placement == 'orphan':
                                source = case['next_path']
                            else:
                                source = case['path'].parent / ('.head-' + 'd' * 32)
                            source.write_bytes(partial)
                            source.chmod(0o600)
                            recovered = self.recover(case)
                            current = recovered.read()
                            self.assertEqual(current['mode'], 'CLOSING')
                            self.assertIsNone(current['active_lease'])
                            self.assertEqual(current['intents'][0]['state'],
                                             'UNKNOWN' if headed == 'SENDING' else 'NOT_DISPATCHED')
                            self.assertIsNone(current['intents'][0]['resource_id'])
                            self.assertIsNone(current['intents'][0]['ack'])
                            self.assertEqual(current['intents'][0]['actions'], [])
                            self.assertEqual(current['owned'], [])
                            self.assertEqual(current['associations'], [])
                            self.assertEqual(current['events'][0], {'event': evidence})
                            self.assertEqual(current['events'][-1]['evidence']['candidate_source'],
                                             'headed' if placement == 'orphan' else 'orphan')
                            self.assertTrue(recovered.is_ancestor(case['checkpoint']))
                            self.assert_archived_partial(current, source.name, partial)
                            self.assert_archived_partial(current, case['path'].name, case['head_raw'])
                            if placement == 'temporary':
                                self.assertFalse(source.exists())
                                self.assertEqual(case['next_path'].read_bytes(), case['raw'])
                                self.assert_archived_partial(current, case['next_path'].name, case['raw'])
                            else:
                                self.assertEqual(case['next_path'].read_bytes(), case['path'].read_bytes())
                            self.assertFalse((case['path'].parent / ledger.RECOVERY).exists())

    def test_invalid_utf8_and_nonstring_incomplete_fragments_refuse_without_archiving(self):
        fragments = {'invalid-byte': b'\xff', 'stray-continuation': b'\x80',
                     'invalid-two-byte-continuation': b'\xc3(',
                     'invalid-three-byte-continuation': b'\xe2\x82(',
                     'invalid-four-byte-continuation': b'\xf0\x9f\x98(',
                     'overlong-two-byte': b'\xc0', 'overlong-three-byte': b'\xe0\x80',
                     'overlong-four-byte': b'\xf0\x80', 'surrogate': b'\xed\xa0',
                     'out-of-range': b'\xf4\x90', 'invalid-leading-byte': b'\xf5',
                     'control-before-incomplete': b'\x01\xc3',
                     'escape-before-incomplete': b'\\\xc3',
                     'unicode-escape-before-incomplete': b'\\u\xc3',
                     'outside-string': None, 'after-state': None}
        for placement in ('orphan', 'temporary'):
            for fault, fragment in fragments.items():
                with self.subTest(placement=placement, fragment=fault):
                    case = self.prepare(self.independent_journal('utf8-invalid-' + placement + '-' + fault))
                    raw = case['raw']
                    if fault == 'outside-string':
                        partial = raw[:raw.index(b',"state":') + len(b',"state":')] + b'{"active_lease":\xc3'
                    elif fault == 'after-state':
                        partial = raw[:raw.rindex(b',"uuid":')] + b'\xc3'
                    else:
                        partial = raw[:raw.index(b'headed-evidence')] + fragment
                    source = case['next_path'] if placement == 'orphan' else case['path'].parent / ('.head-' + 'd' * 32)
                    source.write_bytes(partial)
                    source.chmod(0o600)
                    before = {path.name: path.read_bytes() for path in case['path'].parent.iterdir()}
                    evidence = self.root.parent / ('invalid-utf8-evidence-' + placement + '-' + fault)
                    calls = []
                    self.assertFalse(ledger._partial_generation(partial, case['record']['generation'],
                                     case['binding']['header_sha256'], case['checkpoint']['sha256'], case['binding']['uuid']))
                    reason = 'unrecognized partial' if placement == 'orphan' else 'forked or unbound recovery temporary head'
                    with self.assertRaisesRegex(mc.GuardError, reason):
                        self.recover(case, authorize=lambda *args: calls.append(args), evidence=evidence)
                    self.assertEqual(calls, [])
                    self.assertEqual({path.name: path.read_bytes() for path in case['path'].parent.iterdir()}, before)
                    self.assertFalse(evidence.exists())
                    self.assertFalse((case['path'].parent / ledger.RECOVERY).exists())

    def test_later_partial_sending_after_headed_prepared_remains_not_dispatched(self):
        case = self.prepare(headed_state='PREPARED', orphan_state='SENDING')
        offset = case['raw'].rindex(b',"uuid":"') + len(b',"uuid":"') + 18
        partial = case['raw'][:offset]
        case['next_path'].write_bytes(partial)
        current = self.recover(case).read()
        self.assertEqual(current['mode'], 'CLOSING')
        self.assertIsNone(current['active_lease'])
        self.assertEqual(current['intents'][0]['state'], 'NOT_DISPATCHED')
        self.assertIsNone(current['intents'][0]['resource_id'])
        self.assertIsNone(current['intents'][0]['ack'])
        self.assertEqual(current['owned'], [])
        self.assert_archived_partial(current, case['next_path'].name, partial)

    def test_malformed_later_partial_orphans_refuse_before_authorization_or_archiving(self):
        faults = ('mac-nonhex', 'mac-overlong', 'predecessor-key', 'predecessor-hash',
                  'state-syntax', 'state-escape', 'state-utf8', 'state-type', 'state-mode',
                  'state-lists', 'state-whitespace', 'state-duplicate',
                  'uuid-key', 'uuid-value', 'uuid-close')
        for fault in faults:
            with self.subTest(fragment=fault):
                case = self.prepare(self.independent_journal('malformed-partial-' + fault))
                raw = case['raw']
                mac = raw.index(b',"mac":"') + len(b',"mac":"')
                previous = raw.index(b',"previous_sha256":"') + len(b',"previous_sha256":"')
                state_start = raw.index(b',"state":') + len(b',"state":')
                state_end = raw.rindex(b',"uuid":')
                uuid_start = state_end + len(b',"uuid":"')
                reason = 'unrecognized partial'
                if fault == 'mac-nonhex': partial = raw[:mac] + b'g'
                if fault == 'mac-overlong': partial = raw[:mac + 64] + b'0'
                if fault == 'predecessor-key': partial = raw[:mac + 64] + b'","previous_sha257":'
                if fault == 'predecessor-hash': partial = raw[:previous] + (b'1' if raw[previous:previous + 1] == b'0' else b'0')
                if fault == 'state-syntax': partial = raw[:state_start] + b'{"active_lease":!'
                if fault == 'state-escape': partial = raw[:state_start] + b'{"active_lease":"bad\\q'
                if fault in {'state-syntax', 'state-escape'}: reason = 'invalid journal state bindings'
                if fault == 'state-utf8': partial = raw[:state_start] + b'{"active_lease":"\xff'
                if fault == 'state-type':
                    partial = raw[:state_start] + b'[]' + raw[state_end:uuid_start + 3]
                if fault in {'state-mode', 'state-lists'}:
                    invalid = copy.deepcopy(case['candidate'])
                    if fault == 'state-mode': invalid['mode'] = 'INVALID'
                    else: invalid['owned'] = {}
                    partial = raw[:state_start] + encoded(invalid)[:-1] + raw[state_end:uuid_start + 3]
                    reason = 'invalid journal state mode' if fault == 'state-mode' else 'invalid journal state lists'
                if fault == 'state-whitespace': partial = raw[:state_start + 1] + b' ' + raw[state_start + 1:uuid_start + 3]
                if fault == 'state-duplicate': partial = raw[:state_start + 1] + b'"mode":"OPEN",' + raw[state_start + 1:uuid_start + 3]
                if fault == 'uuid-key': partial = raw[:state_end] + b',"uuix":"'
                if fault == 'uuid-value': partial = raw[:uuid_start] + b'z'
                if fault == 'uuid-close': partial = raw[:uuid_start + 36] + b'"X'
                case['next_path'].write_bytes(partial)
                before = {path.name: path.read_bytes() for path in case['path'].parent.iterdir()}
                evidence = self.root.parent / ('invalid-partial-evidence-' + fault)
                calls = []
                with self.assertRaisesRegex(mc.GuardError, reason):
                    self.recover(case, authorize=lambda *args: calls.append(args), evidence=evidence)
                self.assertEqual(calls, [])
                self.assertEqual({path.name: path.read_bytes() for path in case['path'].parent.iterdir()}, before)
                self.assertFalse(evidence.exists())
                self.assertFalse((case['path'].parent / ledger.RECOVERY).exists())

    def test_invalid_completed_state_prefixes_refuse_before_later_valid_truncations(self):
        base = encoded(state())[:-1].split(b'"owned":')[0]
        faults = {
            'mode': base.replace(b'"mode":"OPEN"', b'"mode":"INVALID"'),
            'mode-type': base.replace(b'"mode":"OPEN"', b'"mode":3'),
            'associations-type': base.replace(b'"associations":[]', b'"associations":{}'),
            'intents-type': base.replace(b'"intents":[]', b'"intents":null'),
            'guard-type': base.replace(b'"guard":null', b'"guard":false'),
            'whitespace': base.replace(b'"events":[]', b'"events": []'),
            'duplicate': base.replace(b'"events":[]', b'"events":[],"events":[]'),
            'order': base.replace(b'"associations":[],"events":[]', b'"events":[],"associations":[]'),
            'nested-duplicate': base.replace(b'"events":[]', b'"events":[{"a":1,"a":2}]'),
            'nested-order': base.replace(b'"events":[]', b'"events":[{"z":1,"a":2}]'),
            'key-escape': base.replace(b'"events":[]', b'"\\u0065vents":[]'),
            'value-escape': base.replace(b'"events":[]', b'"events":["bad\\/escape"]'),
            'number': base.replace(b'"events":[]', b'"events":[1.00]'),
            'leading-zero': base.replace(b'"events":[]', b'"events":[01]'),
            'credential': base.replace(b'"events":[]', b'"events":[{"password":"synthetic"}]'),
            'missing-field': base.replace(b'"guard":null,', b''),
            'control': base.replace(b'"events":[]', b'"events":["bad\ncontrol"]'),
        }
        cuts = {'array': b'[', 'string': b'[{"name":"unfinished', 'utf8': b'[{"name":"valid-\xc3'}
        for placement in ('orphan', 'temporary'):
            for fault, malformed in faults.items():
                self.assertNotEqual(malformed, base, fault)
                for cut, ending in cuts.items():
                    label = placement + '-' + fault + '-' + cut
                    with self.subTest(fragment=label):
                        case = self.prepare(self.independent_journal('invalid-state-prefix-' + label))
                        raw = case['raw']
                        start = raw.index(b',"state":') + len(b',"state":')
                        partial = raw[:start] + malformed + b'"owned":' + ending
                        try:
                            recognized = ledger._partial_generation(partial, case['record']['generation'],
                                case['binding']['header_sha256'], case['checkpoint']['sha256'], case['binding']['uuid'])
                        except mc.GuardError:
                            recognized = False
                        self.assertFalse(recognized)
                        source = case['next_path'] if placement == 'orphan' else case['path'].parent / ('.head-' + 'd' * 32)
                        source.write_bytes(partial)
                        source.chmod(0o600)
                        before = {path.name: path.read_bytes() for path in case['path'].parent.iterdir()}
                        evidence = self.root.parent / ('invalid-state-evidence-' + label)
                        authorized, normalized = [], []
                        with self.assertRaises(mc.GuardError):
                            self.recover(case, authorize=lambda *args: authorized.append(args),
                                         close_state=lambda *args: normalized.append(args), evidence=evidence)
                        self.assertEqual(authorized, [])
                        self.assertEqual(normalized, [])
                        self.assertEqual({path.name: path.read_bytes() for path in case['path'].parent.iterdir()}, before)
                        self.assertFalse(evidence.exists())
                        self.assertFalse((case['path'].parent / ledger.RECOVERY).exists())

    def test_valid_completed_state_fields_allow_later_partial_arrays_strings_and_utf8(self):
        for placement in ('orphan', 'temporary'):
            for mode in sorted(ledger.MODES):
                for cut in ('array', 'string', 'utf8'):
                    label = placement + '-' + mode + '-' + cut
                    with self.subTest(fragment=label):
                        case = self.prepare(self.independent_journal('valid-state-prefix-' + label))
                        candidate = copy.deepcopy(case['candidate'])
                        candidate['mode'] = mode
                        candidate['owned'][0]['proof'] = 'writer-é-evidence'
                        self.rewrite_orphan(case, candidate)
                        raw = case['raw']
                        owned = raw.index(b',"owned":[') + len(b',"owned":[')
                        if cut == 'array': offset = owned
                        if cut == 'string': offset = raw.index(b'writer-') + len(b'writer-')
                        if cut == 'utf8': offset = raw.index('é'.encode()) + 1
                        partial = raw[:offset]
                        source = case['next_path'] if placement == 'orphan' else case['path'].parent / ('.head-' + 'd' * 32)
                        source.write_bytes(partial)
                        source.chmod(0o600)
                        recovered = self.recover(case)
                        current = recovered.read()
                        self.assertEqual(current['mode'], 'CLOSING')
                        self.assertIsNone(current['active_lease'])
                        self.assertEqual(current['intents'][0]['state'], 'UNKNOWN')
                        self.assertEqual(current['owned'], [] if placement == 'orphan' else candidate['owned'])
                        self.assertEqual(current['events'][-1]['evidence']['candidate_source'],
                                         'headed' if placement == 'orphan' else 'orphan')
                        self.assert_archived_partial(current, source.name, partial)
                        self.assertTrue(recovered.is_ancestor(case['checkpoint']))

    def assert_token_partial_recovers(self, label, value, cut, placement, field='evidence'):
        case = self.prepare(orphan_state='SENDING', initial_name='token-' + label + '-' + placement)
        candidate = copy.deepcopy(case['candidate'])
        if field == 'mode':
            candidate['mode'] = value
        else:
            candidate['events'].append({field: value})
        self.rewrite_orphan(case, candidate)
        marker = ledger.canonical_json(field) + b':'
        start = case['raw'].index(marker) + len(marker)
        token = ledger.canonical_json(value)
        self.assertEqual(case['raw'][start:start + len(token)], token)
        self.assertGreater(cut, 0)
        self.assertLessEqual(cut, len(token))
        partial = case['raw'][:start + cut]
        source = case['next_path'] if placement == 'orphan' else case['path'].parent / ('.head-' + 'd' * 32)
        source.write_bytes(partial)
        source.chmod(0o600)
        current = self.recover(case).read()
        self.assertEqual(current['mode'], 'CLOSING')
        self.assertIsNone(current['active_lease'])
        self.assertEqual(current['intents'][0]['state'], 'UNKNOWN')
        self.assertIsNone(current['intents'][0]['ack'])
        self.assertIsNone(current['intents'][0]['resource_id'])
        self.assertEqual(current['owned'], [])
        self.assertEqual(current['associations'], [])
        expected = case['headed'] if placement == 'orphan' else candidate
        self.assertEqual(current['events'][:len(expected['events'])], expected['events'])
        self.assertEqual(current['events'][-1]['evidence']['candidate_source'],
                         'headed' if placement == 'orphan' else 'orphan')
        self.assert_archived_partial(current, source.name, partial)
        self.assert_archived_partial(current, case['path'].name, case['head_raw'])

    def assert_state_fragment_refused(self, label, fragment, placement):
        case = self.prepare(self.independent_journal('refused-token-' + label + '-' + placement))
        start = case['raw'].index(b',"state":') + len(b',"state":')
        partial = case['raw'][:start] + fragment
        try:
            recognized = ledger._partial_generation(partial, case['record']['generation'],
                case['binding']['header_sha256'], case['checkpoint']['sha256'], case['binding']['uuid'])
        except mc.GuardError:
            recognized = False
        self.assertFalse(recognized, fragment)
        source = case['next_path'] if placement == 'orphan' else case['path'].parent / ('.head-' + 'd' * 32)
        source.write_bytes(partial)
        source.chmod(0o600)
        before = {path.name: path.read_bytes() for path in case['path'].parent.iterdir()}
        evidence = self.root.parent / ('refused-token-evidence-' + label + '-' + placement)
        authorized, normalized = [], []
        with self.assertRaises(mc.GuardError):
            self.recover(case, authorize=lambda *args: authorized.append(args),
                         close_state=lambda *args: normalized.append(args), evidence=evidence)
        self.assertEqual(authorized, [])
        self.assertEqual(normalized, [])
        self.assertEqual({path.name: path.read_bytes() for path in case['path'].parent.iterdir()}, before)
        self.assertFalse(evidence.exists())
        self.assertFalse((case['path'].parent / ledger.RECOVERY).exists())

    def test_real_numeric_token_cuts_recover_all_serializer_forms(self):
        numbers = {'integer': -12345, 'large-integer': 10 ** 100 + 12345, 'decimal': -12.375,
                   'negative-zero': -0.0, 'small-fixed-threshold': 0.0001, 'small-scientific-threshold': 1e-5,
                   'large-fixed-threshold': 1e15, 'large-scientific-threshold': 1e16,
                   'maximum': 1.7976931348623157e308, 'minimum-normal': 2.2250738585072014e-308,
                   'minimum-subnormal': 5e-324, 'largest-subnormal': 2.225073858507201e-308,
                   'negative-maximum': -1.7976931348623157e308, 'negative-subnormal': -5e-324,
                   'scaled-mantissa': 1.0000000000000001e18, 'negative-exponent': -1.25e-20}
        for name, number in numbers.items():
            token = ledger.canonical_json(number)
            cuts = {1, len(token) // 2, len(token) - 1, len(token)}
            if b'.' in token:
                dot = token.index(b'.')
                cuts.update((dot + 1, dot + 2))
            if b'e' in token:
                exponent = token.index(b'e')
                cuts.update(range(exponent + 1, len(token) + 1))
            for cut in sorted(cut for cut in cuts if 0 < cut <= len(token)):
                for placement in ('orphan', 'temporary'):
                    with self.subTest(number=name, cut=cut, placement=placement):
                        self.assert_token_partial_recovers(name + '-' + str(cut), number, cut, placement)

    def test_real_string_escape_token_cuts_recover_and_archive_exact_bytes(self):
        strings = {'backslash': ('left\\right', b'\\\\'), 'quote': ('left"right', b'\\"'),
                   'newline': ('left\nright', b'\\n'), 'control': ('left\x01right', b'\\u0001')}
        for name, (value, escape) in strings.items():
            token = ledger.canonical_json(value)
            start = token.index(escape)
            for interior in range(1, len(escape) + 1):
                for placement in ('orphan', 'temporary'):
                    with self.subTest(escape=name, cut=interior, placement=placement):
                        self.assert_token_partial_recovers(name + '-' + str(interior), value, start + interior, placement)

    def test_constrained_mode_digest_and_redaction_prefixes_recover(self):
        values = [('mode', mode) for mode in sorted(ledger.MODES)]
        values += [(field, 'a' * 64) for field in ('root_password_sha256', 'root_password_hash')]
        values += [(field, value) for field in ('root_password_present', 'root_password_redacted') for value in (True, False)]
        for field, value in values:
            token = ledger.canonical_json(value)
            cuts = {1, len(token) // 2, len(token) - 1}
            for cut in sorted(cuts):
                for placement in ('orphan', 'temporary'):
                    label = field + '-' + str(value)[:8] + '-' + str(cut)
                    with self.subTest(field=field, value=value, cut=cut, placement=placement):
                        self.assert_token_partial_recovers(label, value, cut, placement, field=field)

    def test_impossible_numeric_and_escape_eof_fragments_refuse_before_callbacks(self):
        events = b'{"active_lease":null,"associations":[],"events":['
        tokens = (b'0e+1', b'0e-', b'-0e+1', b'1.0e+2', b'1.20e-', b'00', b'01', b'-00', b'+1',
                  b'1.e', b'1E', b'1e++', b'1e+00', b'1e-000', b'1e+309', b'1e-325',
                  b'"bad\\q', b'"bad\\/', b'"bad\\u002', b'"bad\\u000g')
        for index, token in enumerate(tokens):
            for placement in ('orphan', 'temporary'):
                with self.subTest(token=token, placement=placement):
                    self.assert_state_fragment_refused('numeric-escape-' + str(index), events + token, placement)

    def test_impossible_constrained_value_eof_fragments_refuse_before_callbacks(self):
        events = b'{"active_lease":null,"associations":[],"events":['
        mode = encoded(state())[:-1].split(b'"mode":')[0] + b'"mode":'
        fragments = [mode + token for token in (b'"INVALID', b'"OPENx', b'"clos', b'"O\\', b'"O\xc3')]
        for key in ('password', 'Authorization', 'private_key', 'user_data'):
            for value in (b'"unfinished', b'[', b'{', b'n'):
                fragments.append(events + b'{' + ledger.canonical_json(key) + b':' + value)
        for key in ('root_password_sha256', 'root_password_hash'):
            for value in (b'"aG', b'"' + b'a' * 65, b'"a\xc3', b'"\\u0061', b'tr', b'n', b'{'):
                fragments.append(events + b'{' + ledger.canonical_json(key) + b':' + value)
        for key in ('root_password_present', 'root_password_redacted'):
            for value in (b'"tru', b'n', b'0', b'['):
                fragments.append(events + b'{' + ledger.canonical_json(key) + b':' + value)
        for index, fragment in enumerate(fragments):
            for placement in ('orphan', 'temporary'):
                with self.subTest(fragment=fragment, placement=placement):
                    self.assert_state_fragment_refused('constrained-' + str(index), fragment, placement)

    def test_impossible_unfinished_object_keys_refuse_before_callbacks(self):
        fragments = (b'{"z', b'{"active_m',
                     b'{"active_lease":null,"associations":[],"events":[],"guard":null,"intents":[],"ow',
                     b'{"active_lease":null,"associations":[],"events":[{"z":0,"a',
                     b'{"active_lease":null,"associations":[],"events":[{"z":0,"\\',
                     b'{"active_lease":null,"associations":[],"events":[{"z":0,"\\u00',
                     b'{"\xc3',
                     b'{"active_lease":null,"associations":[],"events":[{"' + 'Ω'.encode() + b'":0,"\xc3',
                     b'{"active_lease":null,"associations":[],"events":[{"a' + 'é'.encode() + b'":0,"a\xc2')
        for index, fragment in enumerate(fragments):
            for placement in ('orphan', 'temporary'):
                with self.subTest(fragment=fragment, placement=placement):
                    self.assert_state_fragment_refused('object-key-' + str(index), fragment, placement)

    def test_writer_object_key_prefixes_preserve_order_escape_and_unicode_language(self):
        roots = ('', 'a', 'active_leas', 'zé€😀', '\x01-alpha', '"-alpha', '\\-alpha', '\x01é😀')
        nested = [({'z': 0, 'zz': 1}, 'zz'), ({'a': 0, 'a\x01': 1}, 'a\x01'),
                  ({'aΩ': 0, 'a😀': 1}, 'a😀'), ({'aé0': 0, 'aé1': 1}, 'aé1')]
        records = [('root-' + str(index), None, key) for index, key in enumerate(roots)]
        records += [('nested-' + str(index), fields, key) for index, (fields, key) in enumerate(nested)]
        for label, fields, key in records:
            token = ledger.canonical_json(key)
            for cut in range(1, len(token)):
                for placement in ('orphan', 'temporary'):
                    name = label + '-' + str(cut) + '-' + placement
                    with self.subTest(key=key, cut=cut, placement=placement):
                        case = self.prepare(orphan_state='SENDING', initial_name='writer-key-' + name)
                        candidate = copy.deepcopy(case['candidate'])
                        if fields is None: candidate[key] = 17
                        else: candidate['events'].append(copy.deepcopy(fields))
                        self.rewrite_orphan(case, candidate)
                        state_start = case['raw'].index(b',"state":') + len(b',"state":')
                        start = case['raw'].index(token + b':', state_start)
                        partial = case['raw'][:start + cut]
                        source = case['next_path'] if placement == 'orphan' else case['path'].parent / ('.head-' + 'd' * 32)
                        source.write_bytes(partial)
                        source.chmod(0o600)
                        current = self.recover(case).read()
                        self.assertEqual(current['mode'], 'CLOSING')
                        self.assertIsNone(current['active_lease'])
                        self.assertEqual(current['intents'][0]['state'], 'UNKNOWN')
                        self.assertEqual(current['owned'], [])
                        self.assertEqual(current['associations'], [])
                        expected = case['headed'] if placement == 'orphan' else candidate
                        self.assertEqual(current['events'][:len(expected['events'])], expected['events'])
                        if fields is None and placement == 'temporary': self.assertEqual(current[key], 17)
                        if fields is None and placement == 'orphan': self.assertNotIn(key, current)
                        self.assertEqual(current['events'][-1]['evidence']['candidate_source'],
                                         'headed' if placement == 'orphan' else 'orphan')
                        self.assert_archived_partial(current, source.name, partial)
                        self.assert_archived_partial(current, case['path'].name, case['head_raw'])

    def test_missing_only_final_lf_recovers_orphans_and_temporary_heads_conservatively(self):
        for placement in ('orphan', 'temporary'):
            for headed in ('SENDING', 'PREPARED'):
                label = placement + '-' + headed
                with self.subTest(fragment=label):
                    case = self.prepare(self.independent_journal('missing-lf-' + label), headed_state=headed,
                                        orphan_state='ACKNOWLEDGED' if headed == 'SENDING' else 'SENDING')
                    partial = case['raw'][:-1]
                    self.assertEqual(partial, ledger.canonical_json(case['record']))
                    self.assertEqual(json.loads(partial), case['record'])
                    source = case['next_path'] if placement == 'orphan' else case['path'].parent / ('.head-' + 'd' * 32)
                    source.write_bytes(partial)
                    source.chmod(0o600)
                    with self.assertRaises(mc.GuardError):
                        ledger.Journal(case['path'])
                    recovered = self.recover(case)
                    current = recovered.read()
                    self.assertEqual(current['mode'], 'CLOSING')
                    self.assertIsNone(current['active_lease'])
                    self.assertEqual(current['intents'][0]['state'], 'UNKNOWN' if headed == 'SENDING' else 'NOT_DISPATCHED')
                    expected = case['headed'] if placement == 'orphan' else case['candidate']
                    self.assertEqual(current['intents'][0]['resource_id'], expected['intents'][0]['resource_id'])
                    self.assertEqual(current['intents'][0]['ack'], expected['intents'][0]['ack'])
                    self.assertEqual(current['intents'][0]['actions'], expected['intents'][0]['actions'])
                    self.assertEqual(current['owned'], expected['owned'])
                    self.assertEqual(current['associations'], expected['associations'])
                    self.assertEqual(current['events'][-1]['evidence']['candidate_source'],
                                     'headed' if placement == 'orphan' else 'orphan')
                    self.assert_archived_partial(current, source.name, partial)
                    self.assert_archived_partial(current, case['path'].name, case['head_raw'])
                    self.assertTrue(recovered.is_ancestor(case['checkpoint']))
                    if placement == 'temporary':
                        self.assertFalse(source.exists())
                        self.assertEqual(case['next_path'].read_bytes(), case['raw'])
                        self.assert_archived_partial(current, case['next_path'].name, case['raw'])
                    else:
                        self.assertEqual(case['next_path'].read_bytes(), case['path'].read_bytes())
                    self.assertFalse((case['path'].parent / ledger.RECOVERY).exists())

    def test_parseable_missing_lf_still_requires_canonical_authenticated_generation_bindings(self):
        faults = ('mac', 'format', 'generation', 'uuid', 'header', 'previous', 'state', 'whitespace')
        for placement in ('orphan', 'temporary'):
            for fault in faults:
                label = placement + '-' + fault
                with self.subTest(fragment=label):
                    case = self.prepare(self.independent_journal('invalid-missing-lf-' + label))
                    record = copy.deepcopy(case['record'])
                    if fault == 'mac': record['mac'] = ('0' if record['mac'] != '0' * 64 else '1') * 64
                    if fault == 'format': record['format'] = 'unsupported-generation/v9'
                    if fault == 'generation': record['generation'] += 1
                    if fault == 'uuid': record['uuid'] = '00000000-0000-0000-0000-000000000000'
                    if fault == 'header': record['header_sha256'] = 'f' * 64
                    if fault == 'previous': record['previous_sha256'] = 'f' * 64
                    if fault == 'state': record['state']['mode'] = 'INVALID'
                    if fault != 'mac':
                        record.pop('mac')
                        key = (case['path'].parent / ledger.KEY).read_bytes()
                        record['mac'] = hmac.new(key, encoded(record)[:-1], hashlib.sha256).hexdigest()
                    partial = encoded(record)[:-1]
                    if fault == 'whitespace': partial = b'{ ' + partial[1:]
                    self.assertEqual(json.loads(partial), record)
                    reason = ('MAC differs' if fault == 'mac' else 'unknown recovery generation format' if fault == 'format' else
                              'recovery predecessor differs' if fault == 'previous' else 'invalid journal state mode' if fault == 'state' else
                              'noncanonical recovery generation' if fault == 'whitespace' else 'recovery generation binding differs')
                    with self.assertRaisesRegex(mc.GuardError, reason):
                        case['journal']._recovery_generation(partial, case['checkpoint'])
                    source = case['next_path'] if placement == 'orphan' else case['path'].parent / ('.head-' + 'd' * 32)
                    source.write_bytes(partial)
                    source.chmod(0o600)
                    self.assert_orphan_refused_before_callbacks(case,
                        reason if placement == 'orphan' else 'forked or unbound recovery temporary head', 'missing-lf-' + label)

    def test_partial_orphan_uses_headed_dispatch_state_not_empty_provider_evidence(self):
        case = self.prepare(partial=1)
        current = self.recover(case).read()
        self.assertEqual(current['mode'], 'CLOSING')
        self.assertEqual(current['intents'][0]['state'], 'UNKNOWN')
        self.assertIsNone(current['intents'][0]['resource_id'])
        self.assertEqual(current['owned'], [])
        self.assertEqual(case['next_path'].read_bytes(), self.path.read_bytes())
        archive = Path(current['events'][-1]['evidence']['archive'])
        manifest = json.loads((archive / 'manifest.json').read_bytes())
        original = next(row for row in manifest['files'] if row['source'] == case['next_path'].name)
        self.assertEqual((archive / original['archive_name']).read_bytes(), b'{')
        self.assertEqual(current['events'][-1]['evidence']['candidate_source'], 'headed')

    def test_partial_pre_sending_write_is_provably_not_dispatched_not_permanently_pending(self):
        for partial in (0, 1):
            with self.subTest(partial=partial):
                case = self.prepare(self.independent_journal(f'pre-send-partial-{partial}'),
                                    headed_state='PREPARED', orphan_state='SENDING', partial=partial)
                current = self.recover(case).read()
                self.assertEqual(current['mode'], 'CLOSING')
                self.assertEqual(current['intents'][0]['state'], 'NOT_DISPATCHED')
                self.assertIsNone(current['intents'][0]['ack'])
                self.assertIsNone(current['intents'][0]['resource_id'])
                self.assertEqual(current['owned'], [])

    def test_previously_unknown_head_cannot_be_cleared_by_orphan_ack_adoption(self):
        case = self.prepare(headed_state='UNKNOWN')
        current = self.recover(case).read()
        self.assertEqual(current['intents'][0]['state'], 'UNKNOWN')
        self.assertEqual(current['intents'][0]['resource_id'], 201)
        self.assertEqual(current['intents'][0]['actions'], [501])
        self.assertEqual(current['owned'], case['candidate']['owned'])

    def test_provable_pre_sending_create_and_known_target_mutation_are_not_dispatched(self):
        for operation, headed in [('create', 'PREPARED'), ('delete', 'PREPARED'), ('create', None)]:
            with self.subTest(operation=operation, headed=headed):
                case = self.prepare(self.independent_journal(operation + '-' + str(headed)),
                                    headed_state=headed, orphan_state='SENDING', operation=operation)
                current = self.recover(case).read()
                self.assertEqual(current['mode'], 'CLOSING')
                self.assertEqual(current['intents'][0]['state'], 'NOT_DISPATCHED')
                self.assertEqual(current['intents'][0]['resource_id'], None if operation == 'create' else 201)
                self.assertEqual(current['owned'], [])

    def test_busy_actor_lock_refuses_recovery_without_blocking_or_calling_authorizer(self):
        case = self.prepare()
        calls = []
        with open(self.root / 'journal.lock', 'rb') as stream:
            ledger.fcntl.flock(stream.fileno(), ledger.fcntl.LOCK_EX)
            try:
                with self.assertRaisesRegex(mc.GuardError, 'quiescent unlocked actors'):
                    self.recover(case, authorize=lambda *args: calls.append(args))
            finally:
                ledger.fcntl.flock(stream.fileno(), ledger.fcntl.LOCK_UN)
        self.assertEqual(calls, [])
        self.assertFalse((self.root / ledger.RECOVERY).exists())
        self.assertEqual(self.recover(case).read()['mode'], 'CLOSING')

    def test_unauthorized_recovery_cannot_change_journal_or_create_archive(self):
        case = self.prepare()
        before = {path.name: path.read_bytes() for path in self.root.iterdir()}
        def refused(*args):
            raise mc.GuardError('producer is not quiescent')
        with self.assertRaisesRegex(mc.GuardError, 'producer is not quiescent'):
            self.recover(case, authorize=refused)
        self.assertEqual({path.name: path.read_bytes() for path in self.root.iterdir()}, before)
        self.assertFalse((self.root.parent / 'recovery-evidence').exists())
        self.assertEqual(self.recover(case).read()['mode'], 'CLOSING')

    def test_authenticated_orphan_cannot_drop_preexisting_headed_evidence(self):
        for fault in ('owned', 'associations', 'intents', 'ack-missing', 'ack-changed'):
            with self.subTest(fault=fault):
                case = self.retained_evidence_case('headed-evidence-loss-' + fault)
                candidate = copy.deepcopy(case['candidate'])
                if fault in ('owned', 'associations', 'intents'):
                    candidate[fault] = []
                elif fault == 'ack-missing':
                    candidate['intents'][0]['ack'] = None
                else:
                    candidate['intents'][0]['ack'] = {'status': 500, 'body_sha256': 'd' * 64}
                self.rewrite_orphan(case, candidate)
                self.assertEqual(json.loads(case['raw'])['state'], candidate)
                before = {path.name: path.read_bytes() for path in case['path'].parent.iterdir()}
                evidence = self.root.parent / ('refused-orphan-' + fault)
                authorized, normalized = [], []
                reason = ('orphan discarded headed ownership evidence' if fault in ('owned', 'associations') else
                          'orphan discarded headed intents' if fault == 'intents' else
                          'orphan discarded headed acknowledgement')
                with self.assertRaisesRegex(mc.GuardError, reason):
                    self.recover(case, authorize=lambda *args: authorized.append(args),
                                 close_state=lambda *args: normalized.append(args), evidence=evidence)
                self.assertEqual(authorized, [])
                self.assertEqual(normalized, [])
                self.assertEqual({path.name: path.read_bytes() for path in case['path'].parent.iterdir()}, before)
                self.assertFalse(evidence.exists())
                self.assertFalse((case['path'].parent / ledger.RECOVERY).exists())

    def test_authenticated_orphan_cannot_drop_headed_events_protections_or_known_actions(self):
        faults = ('events-drop', 'events-change', 'tombstone', 'protections-missing', 'protection-name',
                  'protection-id', 'protection-server', 'protection-ip', 'actions-all', 'action-sibling')
        for fault in faults:
            with self.subTest(fault=fault):
                case = self.retained_evidence_case('safety-evidence-loss-' + fault)
                candidate = copy.deepcopy(case['candidate'])
                if fault == 'events-drop': candidate['events'] = []
                if fault == 'events-change': candidate['events'][0]['event'] = 'rewritten adverse history'
                if fault == 'tombstone':
                    candidate['events'] = [event for event in candidate['events'] if event.get('event') != 'observed-retirement']
                if fault == 'protections-missing': candidate.pop('protections')
                if fault == 'protection-name': candidate['protections']['names'] = []
                if fault == 'protection-id': candidate['protections']['ids'] = []
                if fault == 'protection-server': candidate['protections']['by_kind']['server'] = []
                if fault == 'protection-ip': candidate['protections']['by_kind'].pop('primary-ip')
                if fault == 'actions-all': candidate['intents'][0]['actions'] = []
                if fault == 'action-sibling': candidate['intents'][0]['actions'] = [501]
                self.rewrite_orphan(case, candidate)
                before = {path.name: path.read_bytes() for path in case['path'].parent.iterdir()}
                evidence = self.root.parent / ('refused-safety-orphan-' + fault)
                authorized, normalized = [], []
                reason = ('orphan discarded headed events' if fault in ('events-drop', 'events-change', 'tombstone') else
                          'orphan discarded headed action IDs' if fault in ('actions-all', 'action-sibling') else
                          'orphan discarded headed protections')
                with self.assertRaisesRegex(mc.GuardError, reason):
                    self.recover(case, authorize=lambda *args: authorized.append(args),
                                 close_state=lambda *args: normalized.append(args), evidence=evidence)
                self.assertEqual(authorized, [])
                self.assertEqual(normalized, [])
                self.assertEqual({path.name: path.read_bytes() for path in case['path'].parent.iterdir()}, before)
                self.assertFalse(evidence.exists())
                self.assertFalse((case['path'].parent / ledger.RECOVERY).exists())

    def test_authenticated_orphan_can_add_but_not_replace_safety_evidence(self):
        case = self.retained_evidence_case('additive-safety-evidence')
        candidate = copy.deepcopy(case['candidate'])
        candidate['events'].append({'event': 'additional adverse observation'})
        candidate['protections']['names'].append('another-protected')
        candidate['protections']['ids'].append(702)
        candidate['protections']['by_kind']['server'].append(802)
        candidate['protections']['by_kind']['primary-ip'].append(902)
        candidate['intents'][0]['actions'].append(503)
        self.rewrite_orphan(case, candidate)
        current = self.recover(case).read()
        self.assertEqual(current['mode'], 'CLOSING')
        self.assertEqual(current['events'][:len(candidate['events'])], candidate['events'])
        self.assertEqual(current['protections'], candidate['protections'])
        self.assertEqual(current['intents'][0]['actions'], [501, 502, 503])
        self.assertEqual(current['owned'], case['headed']['owned'])
        self.assertEqual(current['associations'], case['headed']['associations'])
        self.assert_archived_partial(current, case['path'].name, case['head_raw'])
        self.assert_archived_partial(current, case['next_path'].name, case['raw'])

    def test_authenticated_orphan_cannot_rewrite_or_drop_headed_intent_bindings(self):
        fields = ('sequence', 'operation', 'kind', 'name', 'nonce', 'descriptor', 'request_sha256', 'pre_ids',
                  'observed_start_ns', 'observed_end_ns', 'producer', 'send_lease', 'send_generation', 'request_proof')
        for field in fields:
            for fault in ('change', 'drop'):
                label = field + '-' + fault
                with self.subTest(field=field, fault=fault):
                    case = self.intent_binding_case('binding-' + label)
                    before = case['headed']['intents'][0]
                    self.assertEqual((before['state'], before['resource_id'], before['ack'], before['actions']),
                                     ('UNKNOWN', None, None, []))
                    candidate = copy.deepcopy(case['candidate'])
                    intent = candidate['intents'][0]
                    if fault == 'drop':
                        intent.pop(field)
                    elif field in ('sequence', 'observed_end_ns', 'send_generation'):
                        intent[field] += 1
                    elif field == 'observed_start_ns': intent[field] -= 1
                    elif field == 'operation': intent[field] = 'delete'
                    elif field == 'kind': intent[field] = 'server'
                    elif field == 'name': intent[field] = 'synthetic-other'
                    elif field == 'nonce': intent[field] = '2' * 32
                    elif field == 'descriptor': intent[field]['ip_range'] = '10.43.0.0/24'
                    elif field == 'request_sha256': intent[field] = 'b' * 64
                    elif field == 'pre_ids': intent[field]['network'] = [999]
                    elif field == 'producer': intent[field]['pid'] += 1
                    elif field == 'send_lease': intent[field]['deadline_ns'] += 1
                    elif field == 'request_proof': intent[field]['sha256'] = 'f' * 64
                    self.rewrite_orphan(case, candidate)
                    self.assert_orphan_refused_before_callbacks(case, 'orphan changed headed intent binding', label)

    def test_authenticated_orphan_cannot_change_known_target_or_existing_recovery_proof(self):
        for fault in ('target-change', 'target-clear', 'target-drop', 'proof-drop', 'proof-origin', 'proof-candidate'):
            with self.subTest(fault=fault):
                case = self.retained_evidence_case('target-proof-' + fault)
                headed = copy.deepcopy(case['headed'])
                headed['intents'][0]['recovery'] = dict(origin='conditional-nonce-recovery', candidate={'id': 201})
                self.publish_fixture_head(case, headed)
                candidate = copy.deepcopy(case['candidate'])
                intent = candidate['intents'][0]
                if fault == 'target-change': intent['resource_id'] = 202
                if fault == 'target-clear': intent['resource_id'] = None
                if fault == 'target-drop': intent.pop('resource_id')
                if fault == 'proof-drop': intent.pop('recovery')
                if fault == 'proof-origin': intent['recovery']['origin'] = 'different-origin'
                if fault == 'proof-candidate': intent['recovery']['candidate']['id'] = 202
                self.rewrite_orphan(case, candidate)
                reason = 'orphan changed headed target ID' if fault.startswith('target-') else 'orphan changed headed recovery proof'
                self.assert_orphan_refused_before_callbacks(case, reason, fault)

    def test_authenticated_orphan_retains_every_preexisting_ack_alternative(self):
        for fault in ('original-drop', 'original-change', 'late-first', 'late-second', 'late-clear', 'late-change', 'relocate'):
            with self.subTest(fault=fault):
                case = self.retained_evidence_case('ack-alternatives-' + fault)
                headed = copy.deepcopy(case['headed'])
                intent = headed['intents'][0]
                intent['original_ack'] = {'status': 500, 'body_sha256': 'd' * 64}
                intent['late_acknowledgements'] = [{'status': 502, 'body_sha256': 'e' * 64},
                                                  {'status': 503, 'body_sha256': 'f' * 64}]
                self.publish_fixture_head(case, headed)
                candidate = copy.deepcopy(case['candidate'])
                current = candidate['intents'][0]
                if fault == 'original-drop': current.pop('original_ack')
                if fault == 'original-change': current['original_ack']['status'] = 501
                if fault == 'late-first': current['late_acknowledgements'].pop(0)
                if fault == 'late-second': current['late_acknowledgements'].pop(1)
                if fault == 'late-clear': current['late_acknowledgements'] = []
                if fault == 'late-change': current['late_acknowledgements'][0]['status'] = 504
                if fault == 'relocate':
                    current['ack'], current['original_ack'] = copy.deepcopy(intent['late_acknowledgements'][1]), copy.deepcopy(intent['late_acknowledgements'][0])
                    current['late_acknowledgements'] = [copy.deepcopy(intent['ack']), copy.deepcopy(intent['original_ack']),
                                                       {'status': 504, 'body_sha256': '0' * 64}]
                self.rewrite_orphan(case, candidate)
                if fault != 'relocate':
                    self.assert_orphan_refused_before_callbacks(case, 'orphan discarded headed acknowledgement', fault)
                else:
                    recovered = self.recover(case)
                    current = recovered.read()
                    self.assertEqual(current['intents'], candidate['intents'])
                    self.assertEqual(current['owned'], case['headed']['owned'])
                    self.assertEqual(current['associations'], case['headed']['associations'])
                    self.assert_archived_partial(current, case['path'].name, case['head_raw'])
                    self.assert_archived_partial(current, case['next_path'].name, case['raw'])

    def test_retained_bindings_allow_writer_lifecycle_progress_and_new_identity_evidence(self):
        for variant in ('ack-progress', 'new-send-lease', 'recovered-identity', 'recovery-corroboration'):
            with self.subTest(variant=variant):
                if variant == 'new-send-lease':
                    case = self.prepare(self.independent_journal('binding-positive-' + variant),
                                        headed_state='PREPARED', orphan_state='SENDING')
                    candidate = copy.deepcopy(case['candidate'])
                    candidate['intents'][0].update(send_lease=dict(intent=0, actor=header()['producer'],
                        deadline_ns=1234, monotonic_deadline_ns=5678), send_generation=case['record']['generation'])
                    expected_state = 'NOT_DISPATCHED'
                elif variant == 'ack-progress':
                    case = self.retained_evidence_case('binding-positive-' + variant)
                    headed = copy.deepcopy(case['headed'])
                    headed['intents'][0].update(acknowledgement_validated=False, required_action_identified=False,
                                                pending_actions=[501])
                    self.publish_fixture_head(case, headed)
                    candidate = copy.deepcopy(case['candidate'])
                    candidate['intents'][0].update(state='DONE', acknowledgement_validated=True,
                        required_action_identified=True, pending_actions=[], actions=[501, 502, 503])
                    expected_state = 'DONE'
                else:
                    case = self.intent_binding_case('binding-positive-' + variant)
                    proof = dict(origin='conditional-nonce-recovery', candidate={'id': 201}, census_sha256='c' * 64)
                    owned = dict(kind='network', id=201, intent=0, evidence_sha256='c' * 64)
                    if variant == 'recovery-corroboration':
                        headed = copy.deepcopy(case['headed'])
                        headed['intents'][0].update(resource_id=201, recovered=True, recovery=copy.deepcopy(proof), original_ack=None)
                        headed['owned'].append(copy.deepcopy(owned))
                        self.publish_fixture_head(case, headed)
                    candidate = copy.deepcopy(case['candidate'])
                    if variant == 'recovered-identity':
                        candidate['intents'][0].update(resource_id=201, recovered=True, recovery=copy.deepcopy(proof), original_ack=None)
                        candidate['owned'].append(copy.deepcopy(owned))
                    else:
                        candidate['intents'][0]['recovery']['corroborating_create_action'] = {'id': 501, 'command': 'create_server'}
                        candidate['intents'][0].update(state='ACKNOWLEDGED', required_action_identified=True, actions=[501])
                    expected_state = 'UNKNOWN'
                self.rewrite_orphan(case, candidate)
                recovered = self.recover(case)
                current = recovered.read()
                self.assertEqual(current['mode'], 'CLOSING')
                self.assertIsNone(current['active_lease'])
                self.assertEqual(current['intents'][0]['state'], expected_state)
                expected_intent = copy.deepcopy(candidate['intents'][0])
                expected_intent['state'] = expected_state
                self.assertEqual(current['intents'][0], expected_intent)
                self.assertEqual(current['owned'], candidate['owned'])
                self.assert_archived_partial(current, case['path'].name, case['head_raw'])
                self.assert_archived_partial(current, case['next_path'].name, case['raw'])

    def test_authenticated_orphan_retains_existing_ack_in_each_supported_representation(self):
        for representation in ('ack', 'original_ack', 'late_acknowledgements'):
            with self.subTest(representation=representation):
                case = self.retained_evidence_case('headed-ack-preserved-' + representation)
                candidate = copy.deepcopy(case['candidate'])
                original = copy.deepcopy(case['headed']['intents'][0]['ack'])
                if representation != 'ack':
                    candidate['intents'][0]['ack'] = {'status': 500, 'body_sha256': 'd' * 64}
                    candidate['intents'][0][representation] = original if representation == 'original_ack' else [original]
                self.rewrite_orphan(case, candidate)
                recovered = self.recover(case)
                current = recovered.read()
                self.assertEqual(current['mode'], 'CLOSING')
                self.assertIsNone(current['active_lease'])
                self.assertEqual(current['owned'], case['headed']['owned'])
                self.assertEqual(current['associations'], case['headed']['associations'])
                self.assertEqual(current['intents'], candidate['intents'])
                self.assertEqual(current['events'][:len(candidate['events'])], candidate['events'])
                self.assertEqual(current['events'][-1]['evidence']['candidate_source'], 'orphan')
                self.assertTrue(recovered.is_ancestor(case['checkpoint']))
                self.assertEqual(recovered.checkpoint['generation'], case['checkpoint']['generation'] + 2)
                self.assert_archived_partial(current, case['path'].name, case['head_raw'])
                self.assert_archived_partial(current, case['next_path'].name, case['raw'])

    def test_normalizer_cannot_reopen_clear_unknown_or_discard_any_evidence(self):
        faults = [('OPEN', 'must close admission'), ('INCIDENT', 'must close admission'),
                  ('lease', 'must close admission'), ('owned', 'discarded state evidence'),
                  ('associations', 'discarded state evidence'), ('events', 'discarded adverse events'),
                  ('ack', 'discarded intent proof'), ('actions', 'discarded intent proof'),
                  ('DONE', 'cleared an ambiguous dispatch')]
        for fault, reason in faults:
            with self.subTest(fault=fault):
                case = self.prepare(self.independent_journal('normalizer-' + fault))
                def bad(candidate, receipt):
                    candidate = self.normalize(candidate, receipt)
                    if fault in ('OPEN', 'INCIDENT'): candidate['mode'] = fault
                    if fault == 'lease': candidate['active_lease'] = {'deadline_ns': 99999}
                    if fault in ('owned', 'associations', 'events'): candidate[fault] = []
                    if fault == 'ack': candidate['intents'][0].pop('ack')
                    if fault == 'actions': candidate['intents'][0]['actions'] = []
                    if fault == 'DONE': candidate['intents'][0]['state'] = 'DONE'
                    return candidate
                with self.assertRaisesRegex(mc.GuardError, reason):
                    self.recover(case, close_state=bad)
                self.assertEqual(case['path'].read_bytes(), case['head_raw'])
                self.assertFalse((case['path'].parent / ledger.RECOVERY).exists())

    def test_bad_mac_forks_gaps_versions_states_and_partial_garbage_are_not_repairable(self):
        for fault, reason in [('mac', 'MAC differs'), ('fork', 'forked or unbound'), ('gap', 'generation gap'),
                              ('version', 'unknown recovery generation'), ('state', 'invalid journal state mode'),
                              ('garbage', 'unrecognized partial'), ('previous', 'recovery predecessor differs')]:
            with self.subTest(fault=fault):
                case = self.prepare(self.independent_journal('unrepairable-' + fault))
                record = copy.deepcopy(case['record'])
                if fault == 'mac': record['state']['mode'] = 'INCIDENT'
                if fault == 'fork':
                    record['state']['events'].append({'event': 'different valid fork'})
                if fault == 'version': record['format'] = 'unsupported-generation/v9'
                if fault == 'state': record['state']['mode'] = 'INVALID'
                if fault == 'previous': record['previous_sha256'] = 'f' * 64
                if fault not in ('mac', 'gap', 'garbage'):
                    record.pop('mac')
                    record['mac'] = hmac.new((case['path'].parent / 'integrity.key').read_bytes(), encoded(record)[:-1], hashlib.sha256).hexdigest()
                if fault == 'gap':
                    case['next_path'].rename(case['path'].parent / 'generation-00000000000000000003.json')
                elif fault == 'garbage':
                    case['next_path'].write_bytes(b'{not-a-canonical-write')
                elif fault == 'fork':
                    temp = case['path'].parent / ('.head-' + 'e' * 32)
                    temp.write_bytes(encoded(record))
                    temp.chmod(0o600)
                else:
                    case['next_path'].write_bytes(encoded(record))
                with self.assertRaisesRegex(mc.GuardError, reason):
                    self.recover(case)
                self.assertEqual(case['path'].read_bytes(), case['head_raw'])
                self.assertFalse((case['path'].parent / ledger.RECOVERY).exists())

    def assert_inside_evidence_refused_then_external_recovers(self, case):
        directory = case['path'].parent
        evidence = directory / 'evidence'
        checkpoint = copy.deepcopy(case['checkpoint'])
        def snapshot():
            result = {}
            for path in [directory, *directory.rglob('*')]:
                info = path.lstat()
                metadata = (info.st_dev, info.st_ino, info.st_mode, info.st_nlink, info.st_uid,
                            info.st_gid, info.st_size, info.st_mtime_ns, info.st_ctime_ns,
                            info.st_rdev, info.st_blksize, info.st_blocks)
                content = path.read_bytes() if stat.S_ISREG(info.st_mode) else (
                    os.readlink(path) if stat.S_ISLNK(info.st_mode) else None)
                result[str(path.relative_to(directory))] = (metadata, content)
            return result
        before = snapshot()
        self.assertFalse(evidence.exists())
        self.assertFalse((directory / ledger.RECOVERY).exists())
        with self.assertRaisesRegex(mc.GuardError, '^recovery evidence must be outside journal$'):
            self.recover(case, evidence=evidence)
        self.assertEqual(snapshot(), before)
        self.assertEqual(case['path'].read_bytes(), case['head_raw'])
        self.assertEqual(case['journal'].checkpoint, checkpoint)
        self.assertEqual(dict(generation=self.load(case['path'])['generation'],
                              sha256=hashlib.sha256(case['path'].read_bytes()[:-1]).hexdigest()), checkpoint)
        self.assertFalse(evidence.exists())
        self.assertFalse((directory / ledger.RECOVERY).exists())

        external = self.root.parent / 'evidence-inside'
        self.assertFalse(external.exists())
        recovered = self.recover(case, evidence=external)
        current = recovered.read()
        self.assertEqual(current['mode'], 'CLOSING')
        self.assertIsNone(current['active_lease'])
        self.assertEqual(current['intents'][0]['state'], 'UNKNOWN')
        self.assertEqual(current['owned'], case['candidate']['owned'])
        self.assertEqual(current['associations'], case['candidate']['associations'])
        self.assertEqual(recovered.binding, case['binding'])
        orphan_checkpoint = dict(generation=checkpoint['generation'] + 1,
                                 sha256=hashlib.sha256(case['raw'][:-1]).hexdigest())
        self.assertEqual(recovered.checkpoint['generation'], orphan_checkpoint['generation'] + 1)
        self.assertEqual(recovered.checkpoint['sha256'], hashlib.sha256(case['path'].read_bytes()[:-1]).hexdigest())
        self.assertTrue(recovered.is_ancestor(checkpoint))
        self.assertTrue(recovered.is_ancestor(orphan_checkpoint))
        self.assertFalse(evidence.exists())
        self.assertFalse((directory / ledger.RECOVERY).exists())
        receipt = current['events'][-1]['evidence']
        self.assertEqual(current['events'][-1]['event'], 'journal-recovery-closed')
        archive = Path(receipt['archive'])
        self.assertEqual(archive.parent, external)
        self.assertNotIn(directory, archive.parents)
        manifest_raw = (archive / 'manifest.json').read_bytes()
        manifest = json.loads(manifest_raw)
        self.assertEqual(hashlib.sha256(manifest_raw).hexdigest(), receipt['manifest_sha256'])
        self.assertEqual(manifest['binding'], case['binding'])
        self.assertEqual(manifest['headed_checkpoint'], checkpoint)
        self.assertEqual(manifest['orphan_checkpoint'], orphan_checkpoint)
        self.assertEqual(manifest['candidate_source'], 'orphan')
        self.assertEqual(manifest['authorization'],
                         {'producer_quiesced': True, 'old_guard_gone': True, 'recovery_guard': {'pid': 777}})
        self.assertEqual(receipt['owner_proof'], manifest['authorization'])
        body = {name: value for name, value in manifest.items() if name != 'mac'}
        self.assertEqual(manifest['mac'], hmac.new((directory / ledger.KEY).read_bytes(), encoded(body)[:-1],
                                                 hashlib.sha256).hexdigest())
        for source, raw in [(case['path'].name, case['head_raw']), (case['next_path'].name, case['raw']),
                            (ledger.ANCHOR, before[ledger.ANCHOR][1])]:
            self.assert_archived_partial(current, source, raw)
        self.assertEqual(stat.S_IMODE(external.stat().st_mode), 0o700)
        self.assertEqual(stat.S_IMODE(archive.stat().st_mode), 0o700)
        self.assertTrue(all(stat.S_IMODE(path.stat().st_mode) == 0o600 for path in archive.iterdir()))

    def test_changed_anchor_key_and_recovery_directory_are_not_repair_shortcuts(self):
        for fault in ('anchor', 'key', 'inside', 'symlink'):
            with self.subTest(fault=fault):
                case = self.prepare(self.independent_journal('bad-pins-' + fault))
                evidence = self.root.parent / ('evidence-' + fault)
                if fault in ('anchor', 'key'):
                    target = case['path'].parent / ('anchor.json' if fault == 'anchor' else 'integrity.key')
                    replacement = case['path'].parent / 'replacement'
                    replacement.write_bytes(target.read_bytes())
                    replacement.chmod(0o600)
                    os.replace(replacement, target)
                if fault == 'inside':
                    self.assert_inside_evidence_refused_then_external_recovers(case)
                    continue
                if fault == 'symlink': evidence.symlink_to(self.root.parent, target_is_directory=True)
                with self.assertRaises((mc.GuardError, OSError)):
                    self.recover(case, evidence=evidence)
                self.assertEqual(case['path'].read_bytes(), case['head_raw'])

    def test_every_repair_sync_failure_retains_refusal_and_never_returns_open_state(self):
        evidence = self.root.parent / 'durability-evidence'
        evidence.mkdir(mode=0o700)
        for failure in range(1, 15):
            with self.subTest(sync=failure):
                case = self.prepare(self.independent_journal(f'repair-sync-{failure}'))
                original = ledger.os.fsync
                calls = []
                def sync(fd):
                    calls.append(fd)
                    if len(calls) == failure:
                        raise OSError('synthetic repair fsync failure')
                    return original(fd)
                with patch.object(ledger.os, 'fsync', side_effect=sync):
                    with self.assertRaisesRegex(OSError, 'synthetic repair fsync failure'):
                        self.recover(case, evidence=evidence)
                self.assertGreaterEqual(len(calls), failure)
                self.assertTrue((case['path'].parent / ledger.RECOVERY).exists())
                with self.assertRaises(mc.GuardError): ledger.Journal(case['path'])
                with self.assertRaisesRegex(mc.GuardError, 'prior journal recovery requires owner incident'):
                    self.recover(case, evidence=evidence)
                # No earlier evidence is erased even if archive sync itself failed.
                archived = list(evidence.glob('*/evidence-*.bin'))
                original_still_present = case['next_path'].read_bytes() == case['raw']
                self.assertTrue(original_still_present or any(path.read_bytes() == case['raw'] for path in archived))

    def test_repair_write_and_rename_failures_leave_marker_and_durable_adverse_archive(self):
        for fault in ('write', 'rename'):
            with self.subTest(fault=fault):
                case = self.prepare(self.independent_journal('repair-' + fault))
                original_create = ledger.Journal._create
                def create(journal, name, data, directory=None):
                    if fault == 'write' and name == 'generation-00000000000000000003.json':
                        fd = os.open(name, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600, dir_fd=journal._directory)
                        with os.fdopen(fd, 'wb') as stream:
                            stream.write(b'{')
                        raise OSError('synthetic recovery write failure')
                    return original_create(journal, name, data, directory)
                with patch.object(ledger.Journal, '_create', new=create):
                    if fault == 'rename':
                        with patch.object(ledger.os, 'rename', side_effect=OSError('synthetic recovery rename failure')):
                            with self.assertRaisesRegex(OSError, 'recovery rename failure'): self.recover(case)
                    else:
                        with self.assertRaisesRegex(OSError, 'recovery write failure'): self.recover(case)
                marker = json.loads((case['path'].parent / ledger.RECOVERY).read_bytes())
                archive = Path(marker['evidence']['archive'])
                manifest = json.loads((archive / 'manifest.json').read_bytes())
                saved = next(item for item in manifest['files'] if item['source'] == case['next_path'].name)
                self.assertEqual((archive / saved['archive_name']).read_bytes(), case['raw'])
                self.assertEqual(case['path'].read_bytes(), case['head_raw'])
                with self.assertRaises(mc.GuardError): ledger.Journal(case['path'])

    def test_retirement_or_marker_unlink_failure_retains_one_shot_incident_barrier(self):
        for phase in ('retire', 'marker'):
            with self.subTest(phase=phase):
                case = self.prepare(self.independent_journal('unlink-' + phase), temporary='full')
                original = ledger.os.unlink
                def unlink(name, *args, **kwargs):
                    if (phase == 'marker' and name == ledger.RECOVERY) or (phase == 'retire' and str(name).startswith('.head-')):
                        raise OSError('synthetic recovery unlink failure')
                    return original(name, *args, **kwargs)
                with patch.object(ledger.os, 'unlink', side_effect=unlink):
                    with self.assertRaisesRegex(OSError, 'synthetic recovery unlink failure'):
                        self.recover(case)
                self.assertTrue((case['path'].parent / ledger.RECOVERY).exists())
                with self.assertRaises(mc.GuardError): ledger.Journal(case['path'])
                with self.assertRaisesRegex(mc.GuardError, 'prior journal recovery requires owner incident'):
                    self.recover(case)
                marker = json.loads((case['path'].parent / ledger.RECOVERY).read_bytes())
                self.assertTrue((Path(marker['evidence']['archive']) / 'manifest.json').exists())

    def test_mid_repair_evidence_changes_refuse_without_head_publication_and_keep_barrier(self):
        faults = ('early-archive', 'early-manifest', 'early-source', 'final-archive', 'final-manifest',
                  'final-archive-directory', 'final-archive-root', 'final-source', 'final-marker', 'final-successor')
        for fault in faults:
            with self.subTest(fault=fault):
                case = self.prepare(self.independent_journal('mid-repair-' + fault), temporary='full')
                directory = case['path'].parent
                evidence = self.root.parent / ('mid-repair-evidence-' + fault)
                original_sync, original_rename = ledger.os.fsync, ledger.os.rename
                changed, published, authorized, normalized = [], [], [], []
                def authorize(*args):
                    authorized.append(args)
                    return {'producer_quiesced': True, 'old_guard_gone': True}
                def normalize(candidate, receipt):
                    normalized.append(copy.deepcopy(receipt))
                    return self.normalize(candidate, receipt)
                def sync(fd):
                    original_sync(fd)
                    if changed or not (directory / ledger.RECOVERY).exists():
                        return
                    target = Path(os.readlink(f'/proc/self/fd/{fd}'))
                    boundary = (target == evidence if fault.startswith('early-') else
                                target.parent == directory and target.name.startswith('.head-'))
                    if not boundary:
                        return
                    marker = json.loads((directory / ledger.RECOVERY).read_bytes())
                    archive = Path(marker['evidence']['archive'])
                    manifest = json.loads((archive / 'manifest.json').read_bytes())
                    row = next(item for item in manifest['files'] if item['source'] == case['next_path'].name)
                    changed.append(fault)
                    if fault.endswith('-archive'):
                        path = archive / row['archive_name']
                        path.write_bytes(path.read_bytes() + b'changed archive bytes')
                    elif fault.endswith('-manifest'):
                        path = archive / 'manifest.json'
                        path.write_bytes(path.read_bytes() + b'changed manifest bytes')
                    elif fault.endswith('-source'):
                        replacement = directory / 'replacement-source'
                        replacement.write_bytes(case['next_path'].read_bytes())
                        replacement.chmod(0o600)
                        previous = case['next_path'].stat().st_ino
                        os.replace(replacement, case['next_path'])
                        self.assertNotEqual(case['next_path'].stat().st_ino, previous)
                    elif fault == 'final-archive-directory':
                        moved = evidence / ('moved-' + archive.name)
                        archive.rename(moved)
                        shutil.copytree(moved, archive)
                        self.assertNotEqual(archive.stat().st_ino, marker['evidence']['archive_identity']['inode'])
                    elif fault == 'final-archive-root':
                        moved = self.root.parent / ('moved-evidence-' + fault)
                        evidence.rename(moved)
                        shutil.copytree(moved, evidence)
                        self.assertNotEqual(evidence.stat().st_ino, marker['evidence']['archive_root_identity']['inode'])
                    elif fault == 'final-marker':
                        path = directory / ledger.RECOVERY
                        path.write_bytes(path.read_bytes() + b'changed marker bytes')
                    elif fault == 'final-successor':
                        path = directory / ledger._generation(case['record']['generation'] + 1)
                        path.write_bytes(path.read_bytes() + b'changed successor bytes')
                def rename(source, target, **kwargs):
                    if target == case['path'].name and kwargs.get('dst_dir_fd') is not None:
                        published.append((source, target))
                    return original_rename(source, target, **kwargs)
                reason = ('recovery evidence changed before retirement' if fault == 'early-source' else
                          'retained recovery evidence changed' if fault == 'final-source' else
                          'recovery archive directory changed' if fault in ('final-archive-directory', 'final-archive-root') else
                          'recovery successor or marker changed' if fault in ('final-marker', 'final-successor') else
                          'recovery archive evidence changed')
                with patch.object(ledger.os, 'fsync', side_effect=sync), patch.object(ledger.os, 'rename', side_effect=rename):
                    with self.assertRaisesRegex(mc.GuardError, reason):
                        self.recover(case, authorize=authorize, close_state=normalize, evidence=evidence)
                self.assertEqual(changed, [fault])
                self.assertEqual(len(authorized), 1)
                self.assertEqual(len(normalized), 1)
                self.assertEqual(published, [])
                self.assertEqual(case['path'].read_bytes(), case['head_raw'])
                self.assertTrue((directory / ledger.RECOVERY).exists())
                self.assertEqual(stat.S_IMODE((directory / ledger.RECOVERY).stat().st_mode), 0o600)
                with self.assertRaises(mc.GuardError):
                    ledger.Journal(case['path'])
                retries = []
                with self.assertRaisesRegex(mc.GuardError, 'prior journal recovery requires owner incident'):
                    self.recover(case, authorize=lambda *args: retries.append(args), evidence=evidence)
                self.assertEqual(retries, [])
                self.assertEqual(case['path'].read_bytes(), case['head_raw'])
                self.assertTrue((directory / ledger.RECOVERY).exists())

    def test_racing_head_change_during_authorization_is_not_overwritten_by_recovery(self):
        case = self.prepare()
        def changed(header_value, binding, checkpoint, headed):
            self.path.write_bytes(self.generation(0).read_bytes())
            return {'producer_quiesced': True}
        with self.assertRaisesRegex(mc.GuardError, 'headed journal changed during recovery'):
            self.recover(case, authorize=changed)
        self.assertEqual(self.path.read_bytes(), self.generation(0).read_bytes())
        self.assertFalse((self.root / ledger.RECOVERY).exists())


if __name__ == '__main__':
    unittest.main()
