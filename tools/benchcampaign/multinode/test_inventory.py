#!/usr/bin/env python3
"""Unit tests for the protect file, inventory.py and primary_ip_check.py.
Temporary fixtures and fake runners only: no hcloud call is ever made."""
import contextlib
import io
import json
import os
from pathlib import Path
import sys
import tempfile
import unittest

HERE = Path(os.path.abspath(__file__)).parent
sys.path.insert(0, str(HERE))

import inventory  # noqa: E402
import mn_common as mc  # noqa: E402
import primary_ip_check as pic  # noqa: E402
import provision  # noqa: E402
import teardown_guard as tg  # noqa: E402

CTL = '203.0.113.7/32'  # RFC 5737 documentation address


class Temp(unittest.TestCase):
    def setUp(self):
        self._tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self._tmp.cleanup)
        self.tmp = Path(self._tmp.name)
        # The tools refuse outputs inside a Git work tree; a TMPDIR there is a broken fixture.
        self.assertFalse(mc.inside_git_worktree(self.tmp), f'TMPDIR {self.tmp} lies inside a Git work tree')

    def protect(self, text):
        path = self.tmp / 'protect.tsv'
        path.write_text(text)
        return str(path)


def project():
    """A fake project: one entry per kind for a few kinds, empty for the rest."""
    inv = {kind: [] for kind in inventory.KINDS}
    inv['server'] = [dict(id=101, name='ci-runner-1'), dict(id=102, name='odd*name')]
    inv['network'] = [dict(id=201, name='ci-net', ip_range='10.66.0.0/16')]
    inv['ssh-key'] = [dict(id=301, name='ops-key')]
    return inv


class Lister:
    def __init__(self, inv, fail=()):
        self.inv, self.fail, self.calls = inv, set(fail), []

    def __call__(self, argv, timeout=None):
        self.calls.append(list(argv))
        assert argv[2:] == ['list', '-o', 'json'], argv
        if argv[1] in self.fail:
            return 1, '', 'unauthorized'
        return 0, json.dumps(self.inv[argv[1]]), ''


class ProtectFileTest(Temp):
    def test_reads_ids_names_and_cidrs(self):
        got = mc.read_protect_file(self.protect(
            '# existing resources\nserver\t101\tci-runner-1\n\nnetwork\t201\tci-net\t10.66.0.0/16\nprimary-ip\t401\t\n'))
        self.assertEqual(got, dict(names=['ci-runner-1', 'ci-net'], ids=[101, 201, 401], cidrs=['10.66.0.0/16']))

    def test_refusals(self):
        for text in ('', '# only a comment\n', 'server\tabc\tx\n', 'robot\t1\tx\n', 'server 1 x\n',
                     'server\t1\tx\ty\tz\n', 'network\t1\tx\tnot-a-cidr\n'):
            with self.subTest(text=text), self.assertRaises((mc.GuardError, ValueError)):
                mc.read_protect_file(self.protect(text))

    def test_refuses_a_file_inside_a_git_worktree(self):
        repo = self.tmp / 'repo'
        (repo / '.git').mkdir(parents=True)
        (repo / 'protect.tsv').write_text('server\t1\tx\n')
        with self.assertRaises(mc.GuardError):
            mc.read_protect_file(repo / 'protect.tsv')

    def test_teardown_protects_ids_and_literal_names(self):
        path = self.protect('server\t101\tci-runner-1\nserver\t102\todd*name\n')
        args = tg.parse_args(['--label', 'campaign=mn-test', '--prefix', 'fdb-bench-vpc', '--protect-file', path])
        self.assertEqual(args.protect_id, [101, 102])
        mine = {'campaign': 'mn-test'}
        owned, refused = tg.classify(args, 'server', [
            dict(id=101, name='fdb-bench-vpc-client', labels=mine),  # owned name, protected ID
            dict(id=7, name='odd*name', labels=mine),                # protected literal name
            dict(id=8, name='oddXname', labels=mine),                # not protected by the literal '*'
            dict(id=9, name='fdb-bench-vpc-node1', labels=mine)])
        self.assertEqual([e['id'] for e in owned], [9])
        reasons = {e['id']: e['reasons'] for e in refused}
        self.assertIn('protected id', reasons[101])
        self.assertIn('protected name', reasons[7])
        self.assertNotIn('protected name', reasons[8])

    def test_provision_protects_names_and_cidrs(self):
        def args(text, *extra):
            return provision.parse_args(['--label', 'campaign=mn-test', '--controller-cidr', CTL, '--ssh-key-name', 'k',
                                         '--cloud-init-dir', str(self.tmp), '--work-dir', str(self.tmp),
                                         '--protect-file', self.protect(text), *extra])
        a = args('network\t201\tci-net\t10.66.0.0/16\n')
        self.assertEqual((a.protected_name, a.protected_cidr), (['ci-net'], ['10.66.0.0/16']))
        provision.validate(a)
        with self.assertRaisesRegex(mc.GuardError, 'overlaps protected range'):
            provision.validate(args('network\t201\tci-net\t10.88.0.0/16\n'))
        with self.assertRaisesRegex(mc.GuardError, 'name is protected'):
            provision.validate(args('firewall\t5\tfdb-bench-vpc-fw\n'))


class InventoryTest(Temp):
    def snap(self, inv):
        out = self.tmp / 'before'
        rc = inventory.main(['snapshot', '--output-dir', str(out)], runner=Lister(inv))
        self.assertEqual(rc, 0)
        return out

    def verify(self, before, inv, fail=()):
        out = self.tmp / 'after'
        stderr = io.StringIO()
        args = inventory.parse_args(['verify', '--inventory', str(before), '--output-dir', str(out)])
        rc = inventory.verify(args, runner=Lister(inv, fail), out=io.StringIO(), err=stderr)
        return rc, json.loads((out / 'summary.json').read_text())

    def test_snapshot_writes_private_lists_and_protect_file(self):
        before = self.snap(project())
        self.assertEqual(sorted(p.name for p in before.iterdir()),
                         sorted([f'before-{k}.json' for k in inventory.KINDS] + ['protect.tsv']))
        self.assertEqual(before.stat().st_mode & 0o777, 0o700)
        self.assertEqual((before / 'protect.tsv').stat().st_mode & 0o777, 0o600)
        got = mc.read_protect_file(before / 'protect.tsv')
        self.assertEqual(got, dict(names=['ci-runner-1', 'odd*name', 'ci-net', 'ops-key'],
                                   ids=[101, 102, 201, 301], cidrs=['10.66.0.0/16']))

    def test_snapshot_refusals(self):
        inv = project()
        self.assertEqual(inventory.main(['snapshot', '--output-dir', str(self.tmp / 'x')], runner=Lister(inv, {'volume'})), 2)
        self.assertFalse((self.tmp / 'x').exists())  # nothing written unless every kind listed
        inv['server'].append(dict(id=101, name='dup'))
        self.assertEqual(inventory.main(['snapshot', '--output-dir', str(self.tmp / 'y')], runner=Lister(inv)), 2)
        repo = self.tmp / 'repo'
        (repo / '.git').mkdir(parents=True)
        self.assertEqual(inventory.main(['snapshot', '--output-dir', str(repo / 'snap')], runner=Lister(project())), 2)
        self.snap(project())
        self.assertEqual(inventory.main(['snapshot', '--output-dir', str(self.tmp / 'before')], runner=Lister(project())), 2)

    def test_verify_passes_on_the_same_id_set_despite_changed_fields(self):
        before = self.snap(project())
        inv = project()
        inv['server'][0]['name'] = 'renamed'
        inv['server'][0]['labels'] = {'x': 'y'}
        rc, summary = self.verify(before, inv)
        self.assertEqual(rc, 0)
        self.assertTrue(summary['passed'])
        self.assertEqual(summary['protected_resource_count'], 4)
        self.assertEqual(len(summary['kinds']), len(inventory.KINDS))

    def test_verify_fails_on_missing_extra_and_unlistable_kinds(self):
        before = self.snap(project())
        cases = [
            ('missing', lambda inv: inv['server'].pop(), 'server', dict(missing=[102], unexpected=[])),
            ('extra', lambda inv: inv['volume'].append(dict(id=999, name='v')), 'volume', dict(missing=[], unexpected=[999])),
        ]
        for name, mutate, kind, delta in cases:
            with self.subTest(name):
                for p in (self.tmp / 'after').glob('*'):
                    p.unlink()
                (self.tmp / 'after').exists() and (self.tmp / 'after').rmdir()
                inv = project()
                mutate(inv)
                rc, summary = self.verify(before, inv)
                self.assertEqual(rc, 1)
                self.assertFalse(summary['kinds'][kind]['passed'])
                self.assertEqual({k: summary['kinds'][kind][k] for k in delta}, delta)
                self.assertTrue((self.tmp / 'after' / f'{kind}.diff').read_text())
        for p in (self.tmp / 'after').glob('*'):
            p.unlink()
        (self.tmp / 'after').rmdir()
        rc, summary = self.verify(before, project(), fail={'certificate'})
        self.assertEqual(rc, 1)
        self.assertIn('list failed', summary['kinds']['certificate']['error'])

    def test_verify_fails_on_a_missing_baseline_file(self):
        before = self.snap(project())
        (before / 'before-firewall.json').unlink()
        rc, summary = self.verify(before, project())
        self.assertEqual(rc, 1)
        self.assertFalse(summary['kinds']['firewall']['passed'])
        self.assertTrue(summary['kinds']['server']['passed'])


class PrimaryIPTest(Temp):
    def server(self, sid, role, ipv4=None, campaign='mn-test'):
        return dict(id=sid, name=f'mn-{role}', labels={'campaign': campaign},
                    public_net=dict(ipv4=dict(id=ipv4, ip='198.51.100.9') if ipv4 else None, ipv6=None),
                    private_net=[dict(ip='10.88.0.10')])

    def ip(self, rid, sid, auto=True):
        return dict(id=rid, assignee_type='server', assignee_id=sid, auto_delete=auto)

    def run_check(self, servers, ips, role='client', protect='server\t101\tci-runner-1\n'):
        (self.tmp / 'servers.json').write_text(json.dumps(servers))
        (self.tmp / 'ips.json').write_text(json.dumps(ips))
        argv = ['--label', 'campaign=mn-test', '--prefix', 'mn', '--role', role,
                '--server-json', str(self.tmp / 'servers.json'), '--primary-ip-json', str(self.tmp / 'ips.json'),
                '--state', str(self.tmp / 'state.json'), '--owned-output', str(self.tmp / 'owned.json'),
                '--protect-file', self.protect(protect)]
        return pic.main(argv)

    def test_staged_pass_records_ids(self):
        others = [dict(id=101, name='ci-runner-1', labels={})]
        self.assertEqual(self.run_check(others + [self.server(1, 'client', 11)], [self.ip(11, 1)]), 0)
        servers = others + [self.server(1, 'client', 11), self.server(2, 'node1', 12)]
        self.assertEqual(self.run_check(servers, [self.ip(11, 1), self.ip(12, 2)], role='node1'), 0)
        state = json.loads((self.tmp / 'state.json').read_text())
        self.assertEqual((state['campaign_server_ids'], state['campaign_primary_ip_ids']), ([1, 2], [11, 12]))
        self.assertEqual(state['status'], 'provisioned-through-node1')
        owned = json.loads((self.tmp / 'owned.json').read_text())
        self.assertFalse(owned['direct_primary_ip_deletion_authorized'])

    def test_refusals(self):
        cases = {
            'unexpected campaign server set': ([self.server(1, 'client', 11)], [self.ip(11, 1)], 'node1', ''),
            'server 1 is unowned': ([self.server(1, 'client', 11, campaign='other')], [self.ip(11, 1)], 'client', ''),
            'server 101 is unowned or protected': ([self.server(101, 'client', 11)], [self.ip(11, 101)], 'client',
                                                   'server\t101\tx\n'),
            'referenced IPs [] != assigned []': ([self.server(1, 'client')], [], 'client', ''),
            'referenced IPs [11] != assigned []': ([self.server(1, 'client', 11)], [self.ip(11, 5)], 'client', ''),
            'primary IP 11 is protected': ([self.server(1, 'client', 11)], [self.ip(11, 1)], 'client', 'primary-ip\t11\t\n'),
            'auto_delete is not true': ([self.server(1, 'client', 11)], [self.ip(11, 1, auto=False)], 'client', ''),
        }
        for reason, (servers, ips, role, protect) in cases.items():
            with self.subTest(reason):
                (self.tmp / 'state.json').unlink(missing_ok=True)
                err = io.StringIO()
                with contextlib.redirect_stderr(err):
                    rc = self.run_check(servers, ips, role, protect or 'server\t999\tx\n')
                self.assertEqual(rc, 2)
                self.assertIn(reason, err.getvalue())

    def test_disappeared_server_is_refused(self):
        self.assertEqual(self.run_check([self.server(1, 'client', 11)], [self.ip(11, 1)]), 0)
        err = io.StringIO()
        with contextlib.redirect_stderr(err):
            self.assertEqual(self.run_check([self.server(3, 'client', 13)], [self.ip(13, 3)]), 2)
        self.assertIn('previously recorded campaign server disappeared', err.getvalue())



# hcloud 1.70 prints `[]` for an empty list; anything else that is not a JSON
# list of objects must fail closed rather than read as "nothing exists".
BAD_LISTS = ['', 'null', '{}', 'false', '0', '""', '"[]"', '[1]', '[null]', '{"id": 1}', 'not json']


class Recorder:
    """Fake hcloud: per-kind list output (a string, or a callable of the call count), deletes recorded."""

    def __init__(self, lists):
        self.lists, self.calls = lists, []

    def __call__(self, argv, timeout=None):
        argv = [str(a) for a in argv]
        self.calls.append(argv)
        kind, verb = argv[1], argv[2]
        if verb == 'list':
            out = self.lists.get(kind, '[]')
            return 0, out(self) if callable(out) else out, ''
        return 0, '', ''

    def deletes(self):
        return [c for c in self.calls if c[2] == 'delete']


class StrictListTest(Temp):
    def test_snapshot_refuses_non_list_output(self):
        for bad in BAD_LISTS:
            with self.subTest(output=bad):
                out = self.tmp / 'snap'
                inv = project()
                runner = Lister(inv)
                runner.inv = dict(inv)
                err = io.StringIO()
                with contextlib.redirect_stderr(err):
                    rc = inventory.main(['snapshot', '--output-dir', str(out)],
                                        runner=lambda argv, timeout=None: (0, bad, '') if argv[1] == 'volume' else runner(argv))
                self.assertEqual(rc, 2)
                self.assertFalse(out.exists())
                self.assertIn('volume', err.getvalue())

    def test_verify_fails_closed_on_non_list_output_and_baseline(self):
        before = self.tmp / 'before'
        self.assertEqual(inventory.main(['snapshot', '--output-dir', str(before)], runner=Lister(project())), 0)
        good = Lister(project())
        for n, bad in enumerate(BAD_LISTS):
            with self.subTest(output=bad):
                out = self.tmp / f'after-{n}'
                args = inventory.parse_args(['verify', '--inventory', str(before), '--output-dir', str(out)])
                rc = inventory.verify(args, out=io.StringIO(), err=io.StringIO(),
                                      runner=lambda argv, timeout=None: (0, bad, '') if argv[1] == 'certificate' else good(argv))
                self.assertEqual(rc, 1)
                summary = json.loads((out / 'summary.json').read_text())
                self.assertFalse(summary['kinds']['certificate']['passed'])
                self.assertIn('error', summary['kinds']['certificate'])
                self.assertTrue(summary['kinds']['server']['passed'])
            with self.subTest(baseline=bad):
                snap = self.tmp / f'baseline-{n}'
                snap.mkdir()
                for f in before.iterdir():
                    (snap / f.name).write_text(f.read_text())
                (snap / 'before-certificate.json').write_text(bad)
                out = self.tmp / f'after-baseline-{n}'
                args = inventory.parse_args(['verify', '--inventory', str(snap), '--output-dir', str(out)])
                self.assertEqual(inventory.verify(args, runner=Lister(project()), out=io.StringIO(), err=io.StringIO()), 1)

    def teardown_argv(self, *extra):
        return ['--label', 'campaign=mn-test', '--prefix', 'fdb-bench-vpc', '--retry-seconds', '0', *extra]

    def test_teardown_discovery_fails_closed(self):
        for bad in BAD_LISTS:
            with self.subTest(output=bad):
                runner = Recorder({'server': bad})
                err = io.StringIO()
                with contextlib.redirect_stderr(err), contextlib.redirect_stdout(io.StringIO()):
                    rc = tg.main(self.teardown_argv('--execute'), runner=runner)
                self.assertEqual(rc, 2)
                self.assertIn('REFUSED', err.getvalue())
                self.assertEqual(runner.deletes(), [])

    def test_teardown_post_delete_census_fails_closed(self):
        owned = json.dumps([dict(id=11, name='fdb-bench-vpc-client', labels={'campaign': 'mn-test'})])
        for bad in BAD_LISTS:
            with self.subTest(output=bad):
                # The first server listing finds the owned server; the census after the delete is unreadable.
                runner = Recorder({'server': lambda r: owned if not r.deletes() else bad})
                err = io.StringIO()
                with contextlib.redirect_stderr(err), contextlib.redirect_stdout(io.StringIO()):
                    rc = tg.main(self.teardown_argv('--execute'), runner=runner)
                self.assertEqual([c[3] for c in runner.deletes()], ['11'])
                self.assertEqual(rc, 2)
                self.assertIn('REFUSED', err.getvalue())

    def test_teardown_execute_deletes_nothing_without_a_valid_protect_file(self):
        owned = json.dumps([dict(id=11, name='fdb-bench-vpc-client', labels={'campaign': 'mn-test'})])
        cases = {'missing': None, 'empty': '', 'comments only': '# none\n\n', 'malformed id': 'server\tabc\tx\n',
                 'unknown kind': 'robot\t1\tx\n', 'too many fields': 'server\t1\tx\ty\tz\n',
                 'invalid range': 'network\t1\tx\tnot-a-cidr\n', 'not utf-8': b'server\t1\t\xff\n'}
        for name, content in cases.items():
            with self.subTest(name):
                path = self.tmp / f'protect-{len(name)}-{name.replace(" ", "-")}.tsv'
                if isinstance(content, bytes):
                    path.write_bytes(content)
                elif content is not None:
                    path.write_text(content)
                runner = Recorder({'server': owned})
                err = io.StringIO()
                with contextlib.redirect_stderr(err), contextlib.redirect_stdout(io.StringIO()):
                    rc = tg.main(self.teardown_argv('--execute', '--protect-file', str(path)), runner=runner)
                self.assertEqual(rc, 2)
                self.assertIn('REFUSED', err.getvalue())
                self.assertEqual(runner.deletes(), [])
                self.assertEqual(runner.calls, [])  # refused before discovery

    def test_provision_preflight_fails_closed(self):
        args = provision.parse_args(['--label', 'campaign=mn-test', '--controller-cidr', CTL, '--ssh-key-name', 'k',
                                     '--cloud-init-dir', str(self.tmp), '--work-dir', str(self.tmp)])
        for kind in ('server', 'network', 'firewall'):
            for bad in BAD_LISTS:
                with self.subTest(kind=kind, output=bad), self.assertRaises(mc.GuardError):
                    provision.preflight(args, Recorder({kind: bad}))


if __name__ == '__main__':
    unittest.main(verbosity=2)
