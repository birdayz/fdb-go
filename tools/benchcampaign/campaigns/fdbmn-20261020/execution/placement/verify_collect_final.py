#!/usr/bin/env python3
"""Tiny fake tar validation; no SSH, remote commands, or cluster access."""
import ast
import contextlib
import copy
import gzip
import hashlib
import io
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tarfile
import tempfile
import types
import unittest
from unittest.mock import patch

sys.dont_write_bytecode = True
HERE = Path(__file__).absolute().parent
sys.path.insert(0, str(HERE.parent))
import collect_final as cf

OPEN = tarfile.open
PAYLOAD = 'files' + cf.R + '/protocol.json'


def blob(members=None, mutate=None, extra=None, manifest_first=False):
    members = members if members is not None else [(PAYLOAD, b'{"campaign":"fake"}', tarfile.REGTYPE)]
    records = {name: dict(size=len(data), sha256=hashlib.sha256(data).hexdigest()) for name, data, _ in members}
    manifest = dict(format='fdb-final-collection/v1', role='client', root=cf.R, files=records,
        hash_only={p: dict(runtime=True, sha256=h) for p, h in cf.RUNTIME.items()}, errors=[],
        artifact_files=sum(n.startswith('files/') for n, _, _ in members), trace_files=0,
        remote_program_sha256=hashlib.sha256(cf.REMOTE_PROGRAM.encode()).hexdigest())
    if mutate:
        mutate(manifest)
    contents = io.BytesIO()
    with OPEN(fileobj=contents, mode='w:gz') as archive:
        def put(name, data, kind=tarfile.REGTYPE):
            info = tarfile.TarInfo(name)
            info.size = len(data)
            info.type = kind
            if kind in (tarfile.SYMTYPE, tarfile.LNKTYPE):
                info.linkname = '/etc/shadow'
                info.size = 0
            archive.addfile(info, io.BytesIO(data) if kind == tarfile.REGTYPE else None)
        encoded = json.dumps(manifest).encode()
        if manifest_first:
            put('meta/manifest.json', encoded)
        for name, data, kind in members:
            put(name, data, kind)
        if extra:
            put(*extra)
        if not manifest_first:
            put('meta/manifest.json', encoded)
    return contents.getvalue()


def check(data, available=None, role='client'):
    with patch.object(cf.tarfile, 'open', side_effect=lambda *a, **kw: OPEN(fileobj=io.BytesIO(data), **kw)):
        return cf.validate_archive('never-opened', role, [], available or {})


class Checks(unittest.TestCase):
    def test_valid_members_every_hash_and_no_extraction(self):
        with patch.object(tarfile.TarFile, 'extractall', side_effect=AssertionError('no extraction')):
            manifest = check(blob())
        self.assertEqual(manifest['artifact_files'], 1)
        self.assertEqual(manifest['errors'], [])

    def test_traversal_data_secrets_and_unknown_names_rejected(self):
        for name in ('/etc/passwd', '../escape', 'files/../../escape', 'files/srv/fdb-bench/cluster/data/4500/a',
                     'files/root/.ssh/id_ed25519', 'files/srv/fdb-bench/provenance/id_rsa',
                     'files/srv/fdb-bench/provenance/x.key', 'files/var/lib/cloud/instance/user-data.txt',
                     'unknown/file', 'meta/../shadow', 'meta/sub/path', 'files\\escape'):
            with self.subTest(name=name), self.assertRaises(cf.Refused):
                check(blob([(name, b'fake', tarfile.REGTYPE)]))

    def test_links_devices_duplicates_and_manifest_position_rejected(self):
        for kind in (tarfile.SYMTYPE, tarfile.LNKTYPE, tarfile.CHRTYPE, tarfile.FIFOTYPE):
            with self.subTest(kind=kind), self.assertRaises(cf.Refused):
                check(blob([(PAYLOAD, b'', kind)]))
        with self.assertRaises(cf.Refused):
            check(blob(extra=(PAYLOAD, b'duplicate', tarfile.REGTYPE)))
        with self.assertRaises(cf.Refused):
            check(blob(manifest_first=True))

    def test_manifest_hash_population_and_runtime_are_not_vacuous(self):
        for mutate in (lambda m: m['files'][PAYLOAD].update(sha256='0' * 64),
                       lambda m: m.update(files={}), lambda m: m.update(hash_only={}),
                       lambda m: m.update(artifact_files=0), lambda m: m.update(role='node1')):
            with self.subTest(mutate=mutate), self.assertRaises(cf.Refused):
                check(blob(mutate=mutate))

    def test_hash_only_payload_needs_same_retained_local_bytes(self):
        with tempfile.TemporaryDirectory(dir=HERE) as tmp:
            path = Path(tmp) / 'retained-binary'
            path.write_bytes(b'fake retained binary')
            digest = cf.sha_file(path)
            remote = cf.R + '/bin/fake'
            available = {remote: {digest: str(path)}}
            def mutate(m):
                m['hash_only'][remote] = dict(sha256=digest, retained_local=str(path))
            check(blob(mutate=mutate), available)
            with self.assertRaises(cf.Refused):
                check(blob(mutate=mutate), {})
            path.write_bytes(b'changed bytes')
            with self.assertRaises(cf.Refused):
                check(blob(mutate=mutate), available)

    def test_nodes_need_trace_population_and_deadlines_cover_preflight(self):
        with self.assertRaises(cf.Refused):
            check(blob(mutate=lambda m: m.update(role='node1')), role='node1')
        with tempfile.TemporaryDirectory(dir=HERE) as tmp, patch.object(cf.time, 'monotonic', return_value=10):
            with self.assertRaises(cf.Refused):
                cf.local_payloads(Path(tmp), 9)
            p = Path(tmp) / 'empty'
            p.write_bytes(b'')
            with self.assertRaises(cf.Refused):
                cf.sha_file(p, 9)

    def test_collect_role_streams_fake_archive_and_checks_it_without_ssh(self):
        with tempfile.TemporaryDirectory(dir=HERE) as tmp, patch.object(subprocess, 'run', side_effect=AssertionError('NO COMMANDS')):
            layout = dict(processes=[dict(node='node1', port=4500)])
            spec = cf.role_spec('client', layout, {}, 1, 30)
            def transport(role, command, **kwargs):
                self.assertEqual(role, 'client')
                self.assertIn('timeout', command)
                kwargs['stdout'].write(blob())
                return types.SimpleNamespace(returncode=0)
            report = cf.collect_role('client', spec, Path(tmp), cf.time.monotonic() + 60, transport=transport)
            self.assertTrue(report['ok'], report)
            self.assertTrue((Path(tmp) / 'client/artifacts.tar.gz').is_file())
            self.assertTrue((Path(tmp) / 'client/manifest.json').is_file())

    def test_unloaded_phase_units_are_discovered_from_journal_fields(self):
        tree = ast.parse(cf.REMOTE_PROGRAM)
        writer = next(n for n in tree.body if isinstance(n, ast.With))
        body = writer.body[0].body  # collection try block
        first = next(i for i, n in enumerate(body) if isinstance(n, ast.Assign) and any(isinstance(t, ast.Name) and t.id == 'listing' for t in n.targets))
        last = next(i for i, n in enumerate(body) if isinstance(n, ast.Expr) and isinstance(n.value, ast.Call) and
                    isinstance(n.value.func, ast.Name) and n.value.func.id == 'json_put' and
                    n.value.args and isinstance(n.value.args[0], ast.Constant) and n.value.args[0].value == 'meta/selected-units.json')
        selected = []
        def command(label, *_args, **_kwargs):
            return 'fdb-mn-runner-current.service loaded active\n' if label == 'unit-discovery' else 'fdb-mn-sampler-old.service\nssh.service\n'
        ns = dict(command=command, s=dict(fdb_units=['fdb-bench-4500.service'], since_epoch=1), re=re,
                  json_put=lambda _name, units: selected.extend(units))
        exec(compile(ast.fix_missing_locations(ast.Module(body=body[first:last + 1], type_ignores=[])), '<fake-unit-discovery>', 'exec'), ns)
        self.assertIn('fdb-mn-sampler-old.service', selected)
        self.assertIn('fdb-mn-runner-current.service', selected)
        self.assertNotIn('ssh.service', selected)

    def test_remote_stream_writer_uses_python312_compatible_constructor(self):
        tree = ast.parse(cf.REMOTE_PROGRAM)
        writer = next(n for n in tree.body if isinstance(n, ast.With))
        body = ast.parse("info=tarfile.TarInfo('tiny');info.size=4;tar.addfile(info,io.BytesIO(b'data'))").body
        block = ast.With(items=copy.deepcopy(writer.items), body=body, type_comment=None)
        output = io.BytesIO()
        def python312_open(*args, **kwargs):
            if '|' in kwargs.get('mode', '') and 'compresslevel' in kwargs:
                raise TypeError('Python3.12 stream mode does not accept compresslevel')
            return OPEN(*args, **kwargs)
        ns = dict(gzip=gzip, io=io, sys=types.SimpleNamespace(stdout=types.SimpleNamespace(buffer=output)),
                  tarfile=types.SimpleNamespace(open=python312_open, TarInfo=tarfile.TarInfo))
        code = ast.fix_missing_locations(ast.Module(body=[block], type_ignores=[]))
        exec(compile(code, '<actual-stream-constructor-only>', 'exec'), ns)
        with OPEN(fileobj=io.BytesIO(output.getvalue()), mode='r:gz') as archive:
            self.assertEqual(archive.extractfile('tiny').read(), b'data')

    def test_remote_program_syntax_and_private_command_output_redacted(self):
        tree = ast.parse(cf.REMOTE_PROGRAM)
        # Execute ONLY these pure helpers with a fake subprocess function.
        definitions = [n for n in tree.body if isinstance(n, ast.FunctionDef) and n.name == 'command']
        records, errors, commands = {}, [], []
        def fake_run(*args, **kwargs):
            return types.SimpleNamespace(returncode=0,
                stdout=b'-----BEGIN OPENSSH PRIVATE KEY-----\nnever-archive-me\n', stderr=b'')  # secretscan:allow (fixture for the collector's refusal)
        ns = dict(remaining=lambda: 100, time=types.SimpleNamespace(time=lambda: 1),
            subprocess=types.SimpleNamespace(run=fake_run, PIPE=-1), errors=errors, commands=commands,
            secret=re.compile(br'(?m)^-----BEGIN (?:OPENSSH |RSA |EC |DSA |ENCRYPTED )?PRIVATE KEY-----\r?$'),
            json_put=lambda name, value: records.update({name: value}))
        exec(compile(ast.Module(body=definitions, type_ignores=[]), '<fake-command-only>', 'exec'), ns)
        ns['command']('fake', ['never-execute'])
        self.assertIn('private key block refused', errors[0])
        self.assertNotIn('never-archive-me', json.dumps(records))

    def test_no_execute_refuses_without_any_subprocess(self):
        with patch.object(subprocess, 'run', side_effect=AssertionError('NO COMMANDS')), contextlib.redirect_stderr(io.StringIO()):
            self.assertEqual(cf.main(['--output', '/never-create']), 2)


if __name__ == '__main__':
    os.umask(0o077)
    unittest.main(verbosity=2)
