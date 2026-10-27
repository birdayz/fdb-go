"""Synthetic archives and temporary exports only; no private archive dependency."""
import copy
import difflib
import io
import json
import os
import subprocess
from pathlib import Path
import tarfile
import tempfile
import unittest
from unittest import mock

import prepare_harness as prepare

RUN = prepare.SOURCE_ROOT + '/internal/bench/run.go'
TEST = prepare.SOURCE_ROOT + '/internal/bench/bench_test.go'
BUILD = prepare.SOURCE_ROOT + '/scripts/BUILD.bazel'
SMOKE = prepare.SOURCE_ROOT + '/integration/smoke_test.go'
OTHER = prepare.SOURCE_ROOT + '/scripts/run.py'
VERSION_FILES = tuple(prepare.SOURCE_ROOT + '/scripts/' + name for name in
                      ('matrix_plan.py', 'r3_plan.py', 'test_campaign.py'))
CHANGED_FILES = (RUN, TEST, BUILD, SMOKE, *VERSION_FILES)


class PrepareTest(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.old = {RUN: b'package bench\nvar fence = 0\n', TEST: b'package bench\nvar checked = false\n',
                    BUILD: b'# fixture build before\n', SMOKE: b'# fixture smoke before\n', OTHER: b'# unchanged\n\xff\n'}
        self.old.update({name: b"go_version='go1.26.9'\n" for name in VERSION_FILES})
        self.new = dict(self.old, **{RUN: b'package bench\nvar fence = 1\n',
                                    TEST: b'package bench\nvar checked = true\n', BUILD: b'# fixture build after\n',
                                    SMOKE: b'# fixture smoke after\n'})
        self.new.update({name: b"go_version='go1.27.3'\n" for name in VERSION_FILES})
        self.patch = self.root / 'overlay.patch'
        self.patch.write_text(''.join(''.join(difflib.unified_diff(
            self.old[name].decode().splitlines(True), self.new[name].decode().splitlines(True),
            fromfile='a/' + name, tofile='b/' + name)) for name in CHANGED_FILES))
        self.spec = dict(base_revision='trusted-synthetic-fixture', patch_sha256=prepare.digest(self.patch.read_bytes()),
                         original_sha256={name: prepare.digest(self.old[name]) for name in CHANGED_FILES},
                         patched_sha256={name: prepare.digest(self.new[name]) for name in CHANGED_FILES})
        self.context = self.root / 'source-context.json'
        self.write_spec()
        self.archive = self.root / 'input.tar.gz'
        self.write_archive(self.old)

    def write_spec(self):
        self.context.write_bytes(prepare.json_bytes(self.spec))

    def write_archive(self, files, extra=None):
        # Independent fixture writer, not the deterministic output implementation.
        with tarfile.open(self.archive, 'w:gz') as archive:
            directories = set()
            for name in files:
                directories.update(str(p) for p in Path(name).parents if str(p) != '.')
            for name in sorted(directories):
                member = tarfile.TarInfo(name)
                member.type, member.mode = tarfile.DIRTYPE, 0o755
                archive.addfile(member)
            for name, body in files.items():
                member = tarfile.TarInfo(name)
                member.size, member.mode = len(body), 0o644
                archive.addfile(member, io.BytesIO(body))
            if extra is not None:
                member, body = extra
                archive.addfile(member, io.BytesIO(body) if member.isfile() else None)

    def prepare(self, name='prepared', expected_count=8):
        return prepare.prepare(self.archive, self.root / name, self.context, self.patch,
                               prepare.digest(self.archive.read_bytes()), expected_count)

    def verify(self, name='prepared'):
        root = self.root / name
        return prepare.verify_tree(root / 'tree', root / 'source-receipt.json', self.context, self.patch,
                                   prepare.digest(self.archive.read_bytes()))

    def snapshot(self, root):
        return {str(path.relative_to(root)): path.read_bytes() for path in root.rglob('*') if path.is_file()}

    def test_fixed_production_archive_pin_is_not_a_cli_override(self):
        self.assertEqual(prepare.BASE_ARCHIVE_SHA256,
                         '76990b867f337bdad0132cc69aa3f6f9a9bcf0cb0859cc8c7f8514def684f508')
        with self.assertRaisesRegex(ValueError, 'input archive SHA256 differs'):
            prepare.prepare(self.archive, self.root / 'wrong', self.context, self.patch)
        self.assertFalse((self.root / 'wrong').exists())

    def test_complete_sources_membership_and_unchanged_binary_member(self):
        receipt = self.prepare()
        self.assertEqual(set(receipt['changed_files']), set(CHANGED_FILES))
        self.assertEqual(set(receipt['member_sha256']), set(self.old))
        self.assertEqual(receipt['member_sha256'][OTHER], receipt['original_member_sha256'][OTHER])
        for name, data in self.new.items():
            self.assertEqual((self.root / 'prepared/tree' / name).read_bytes(), data)
        archive = self.root / 'prepared/client-harness.tar.gz'
        self.assertEqual(receipt['output_archive_sha256'], prepare.digest(archive.read_bytes()))
        with tarfile.open(archive) as source:
            regular = {member.name: source.extractfile(member).read() for member in source if member.isfile()}
        self.assertEqual(regular, self.new)
        self.assertEqual(self.verify()['regular_files'], 8)
        self.assertIn('just test', (self.root / 'prepared/qualify-harness.sh').read_text())
        self.assertEqual(receipt['status'], 'prepared-not-runtime-qualified')

    def test_two_preparations_are_byte_identical_verification_is_idempotent(self):
        with mock.patch('time.time', return_value=2000000000):
            first = self.prepare('one')
        with mock.patch('time.time', return_value=2000000100):
            second = self.prepare('two')
        self.assertEqual(first, second)
        self.assertEqual(self.snapshot(self.root / 'one'), self.snapshot(self.root / 'two'))
        for name in ('one', 'two'):
            archive = self.root / name / 'client-harness.tar.gz'
            header = archive.read_bytes()[:10]
            self.assertEqual(header[:3], b'\x1f\x8b\x08')
            self.assertEqual(int.from_bytes(header[4:8], 'little'), 0, 'gzip timestamp must be zero')
            with tarfile.open(archive) as source:
                members = source.getmembers()
            self.assertTrue(members)
            self.assertTrue(all(member.mtime == 0 for member in members))
        before = self.snapshot(self.root / 'one')
        self.assertEqual(self.verify('one'), self.verify('one'))
        self.assertEqual(self.snapshot(self.root / 'one'), before)
        with self.assertRaisesRegex(ValueError, 'already exists'):
            self.prepare('one')
        self.assertEqual(self.snapshot(self.root / 'one'), before)

    def test_archive_path_traversal_links_devices_duplicates_and_parent_file_refuse(self):
        cases = []
        for name in ('../escaped', '/absolute', 'tools/clientbench/../escaped', 'tools/clientbench/./bad',
                     'tools/clientbench/.git/config', 'tools\\clientbench\\bad'):
            member = tarfile.TarInfo(name)
            member.size = 1
            cases.append((member, b'x'))
        for kind in (tarfile.SYMTYPE, tarfile.LNKTYPE, tarfile.CHRTYPE, tarfile.FIFOTYPE):
            member = tarfile.TarInfo(prepare.SOURCE_ROOT + '/unsafe')
            member.type, member.linkname = kind, '../../escaped'
            cases.append((member, b''))
        duplicate = tarfile.TarInfo(RUN)
        duplicate.size = 1
        cases.append((duplicate, b'x'))
        parent_file = tarfile.TarInfo(prepare.SOURCE_ROOT)
        parent_file.size = 1
        cases.append((parent_file, b'x'))
        for index, extra in enumerate(cases):
            with self.subTest(index=index):
                self.write_archive(self.old, extra)
                with self.assertRaises((ValueError, tarfile.TarError)):
                    self.prepare(f'unsafe-{index}')
                self.assertFalse((self.root / f'unsafe-{index}').exists())
        self.assertFalse((self.root / 'escaped').exists())

    def test_archive_extra_file_count_full_source_hash_and_context_mismatch_refuse(self):
        extra = dict(self.old, **{prepare.SOURCE_ROOT + '/unexpected.go': b'extra\n'})
        self.write_archive(extra)
        with self.assertRaisesRegex(ValueError, 'regular-member population'):
            self.prepare('extra')
        stale = dict(self.old, **{RUN: b'package bench\nvar fence = 99\n'})
        self.write_archive(stale)
        with self.assertRaisesRegex(ValueError, 'complete original source SHA256'):
            self.prepare('stale')
        # A trusted fixture pin can match different bytes; the exact patch must still refuse them.
        self.spec['original_sha256'][RUN] = prepare.digest(stale[RUN])
        self.write_spec()
        with self.assertRaisesRegex(ValueError, 'source context changed'):
            self.prepare('context')
        self.assertFalse(any((self.root / name).exists() for name in ('extra', 'stale', 'context')))

    def test_patch_pin_post_hash_and_unexpected_file_population_refuse(self):
        original = copy.deepcopy(self.spec)
        self.spec['patch_sha256'] = '0' * 64
        self.write_spec()
        with self.assertRaisesRegex(ValueError, 'patch SHA256'):
            self.prepare('patch-pin')
        self.spec = copy.deepcopy(original)
        self.spec['patched_sha256'][RUN] = '0' * 64
        self.write_spec()
        with self.assertRaisesRegex(ValueError, 'complete patched source SHA256'):
            self.prepare('post-pin')
        self.spec = copy.deepcopy(original)
        self.spec['original_sha256']['unexpected'] = '0' * 64
        self.write_spec()
        with self.assertRaisesRegex(ValueError, 'patched-file population'):
            self.prepare('file-set')

    def test_verifier_allows_unrelated_production_build_outputs_but_rejects_harness_drift(self):
        self.prepare()
        tree = self.root / 'prepared/tree'
        (tree / 'unrelated-production.go').write_text('outside harness scope\n')
        (tree / '.bazel-bep.jsonl').write_text('build output outside harness scope\n')
        self.assertEqual(self.verify()['regular_files'], 8)
        (tree / OTHER).write_bytes(b'drift')
        with self.assertRaisesRegex(ValueError, 'bytes or modes changed'):
            self.verify()
        (tree / OTHER).write_bytes(self.old[OTHER])
        extra = tree / prepare.SOURCE_ROOT / 'extra.go'
        extra.write_text('unexpected\n')
        with self.assertRaisesRegex(ValueError, 'regular-member population'):
            self.verify()
        extra.unlink()
        empty = tree / prepare.SOURCE_ROOT / 'extra-directory'
        empty.mkdir()
        with self.assertRaisesRegex(ValueError, 'directory population'):
            self.verify()
        empty.rmdir()
        (tree / OTHER).chmod(0o600)
        with self.assertRaisesRegex(ValueError, 'bytes or modes changed'):
            self.verify()

    def test_verifier_rejects_symlinked_members_and_foreign_receipt(self):
        self.prepare()
        tree = self.root / 'prepared/tree'
        member = tree / OTHER
        member.unlink()
        member.symlink_to(tree / RUN)
        with self.assertRaisesRegex(ValueError, 'nonregular source file'):
            self.verify()
        member.unlink()
        member.write_bytes(self.old[OTHER])
        member.chmod(0o644)
        receipt_path = self.root / 'prepared/source-receipt.json'
        receipt = json.loads(receipt_path.read_bytes())
        receipt['input_archive_sha256'] = '0' * 64
        receipt_path.write_bytes(prepare.json_bytes(receipt))
        with self.assertRaisesRegex(ValueError, 'foreign source receipt'):
            self.verify()

    def test_qualification_recipe_locked_shutdown_and_after_failure_input_guard(self):
        self.prepare()
        bindir = self.root / 'bin'
        bindir.mkdir()
        events = self.root / 'events'
        verifier = self.root / 'verifier.py'
        verifier.write_text('import os, pathlib, sys\n'
            'p=pathlib.Path(os.environ["EVENTS"])\n'
            'old=p.read_text() if p.exists() else ""\n'
            'p.write_text(old+"verify\\n")\n'
            'sys.exit(9 if old and os.environ["BAD_AFTER"] == "1" else 0)\n')
        for name, body in [('just', 'echo test >> "$EVENTS"; exit "$JUST_RC"'),
                           ('bazelisk', 'echo shutdown >> "$EVENTS"; exit "$SHUTDOWN_RC"')]:
            path = bindir / name
            path.write_text('#!/usr/bin/env bash\n' + body + '\n')
            path.chmod(0o755)
        for just_rc, shutdown_rc, bad_after, expected in [('0', '0', '0', 0), ('7', '0', '0', 7),
                                                        ('0', '0', '1', 4), ('7', '0', '1', 4), ('0', '9', '0', 9)]:
            if events.exists(): events.unlink()
            env = dict(os.environ, PATH=str(bindir) + ':' + os.environ['PATH'],
                EVENTS=str(events), JUST_RC=just_rc, SHUTDOWN_RC=shutdown_rc, BAD_AFTER=bad_after,
                FROZEN_EXPORT=str(self.root / 'prepared/tree'), HARNESS_RECEIPT='synthetic-only',
                FENCES_PREPARER=str(verifier), FDB_LANE_LOCK=str(self.root / 'lane.lock'))
            result = subprocess.run(['bash', str(self.root / 'prepared/qualify-harness.sh')],
                                    env=env, capture_output=True, text=True, timeout=30)
            self.assertEqual(result.returncode, expected, result.stderr)
            self.assertEqual(events.read_text().splitlines(), ['verify', 'test', 'shutdown', 'verify'])

    def test_symlinked_output_or_tree_never_resolved_into_an_adopted_destination(self):
        link = self.root / 'link'
        link.symlink_to(self.root / 'missing')
        with self.assertRaisesRegex(ValueError, 'symlinked'):
            self.prepare('link')
        self.assertFalse((self.root / 'missing').exists())
        self.prepare()
        link.unlink()
        link.symlink_to(self.root / 'prepared/tree', target_is_directory=True)
        with self.assertRaisesRegex(ValueError, 'symlinked'):
            prepare.verify_tree(link, self.root / 'prepared/source-receipt.json', self.context, self.patch,
                                prepare.digest(self.archive.read_bytes()))

    def test_git_worktree_output_refuses_before_any_creation(self):
        repo = self.root / 'repo'
        repo.mkdir()
        (repo / '.git').write_text('gitdir: /not-a-real-git-directory\n')
        with self.assertRaisesRegex(ValueError, 'inside a Git worktree'):
            prepare.prepare(self.archive, repo / 'output', self.context, self.patch,
                            prepare.digest(self.archive.read_bytes()), 8)
        self.assertFalse((repo / 'output').exists())


if __name__ == '__main__':
    unittest.main()
