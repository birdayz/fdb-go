#!/usr/bin/env python3
"""Unit tests for the multi-node campaign tooling. Temporary fixtures only:
no hcloud, ssh, fdbcli or network call is ever made (runners are fakes)."""
import argparse
import collections
import contextlib
import io
import itertools
import json
import math
import os
from pathlib import Path
import random
import shutil
import subprocess
import sys
import tempfile
import textwrap
import unittest

# abspath, not resolve(): under Bazel, stay inside runfiles instead of following
# the symlink back into the source tree (undeclared inputs).
HERE = Path(os.path.abspath(__file__)).parent
sys.path.insert(0, str(HERE))

import fdb_cluster  # noqa: E402
import mn_common as mc  # noqa: E402
import placement_receipt as pr  # noqa: E402
import provision  # noqa: E402
import render_cloud_init as rci  # noqa: E402
import seal_multinode as seal  # noqa: E402
import teardown_guard as tg  # noqa: E402

CTL = '203.0.113.7/32'  # RFC 5737 documentation address
NODES = ['10.88.0.11', '10.88.0.12', '10.88.0.13']


class Temp(unittest.TestCase):
    def setUp(self):
        self._tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self._tmp.cleanup)
        self.tmp = Path(self._tmp.name)
        if mc.inside_git_worktree(self.tmp):
            self.skipTest('temporary directory lies inside a Git work tree')


class FakeRunner:
    """Records argv; answers from a {tuple(prefix): (rc, stdout, stderr)} table."""

    def __init__(self, answers=None, default=(0, '', '')):
        self.calls, self.answers, self.default = [], answers or {}, default

    def __call__(self, argv, timeout=None):
        argv = [str(a) for a in argv]
        self.calls.append(argv)
        for prefix, answer in self.answers.items():
            if tuple(argv[:len(prefix)]) == prefix:
                return answer(argv) if callable(answer) else answer
        return self.default


class CommonTest(unittest.TestCase):
    def test_label_and_names(self):
        self.assertEqual(mc.parse_campaign_label('campaign=hcloud-multinode-vpc-20261014'), 'hcloud-multinode-vpc-20261014')
        for bad in ['campaign', 'owner=x', 'campaign=', 'campaign=-x', 'campaign=' + 'a' * 64, 'campaign=a b']:
            with self.assertRaises(mc.GuardError):
                mc.parse_campaign_label(bad)
        self.assertTrue(mc.owned_name('fdb-bench-vpc', 'fdb-bench-vpc'))
        self.assertTrue(mc.owned_name('fdb-bench-vpc-node1', 'fdb-bench-vpc'))
        self.assertFalse(mc.owned_name('fdb-bench-vpcx', 'fdb-bench-vpc'))
        self.assertFalse(mc.owned_name('ci-fleet', 'fdb-bench-vpc'))

    def test_deadline(self):
        self.assertEqual(mc.parse_deadline('2026-10-15T06:00:00Z'), 1792044000)
        self.assertEqual(mc.parse_deadline('1792044000'), 1792044000)
        self.assertEqual(mc.iso(1792044000), '2026-10-15T06:00:00Z')
        with self.assertRaises(mc.GuardError):
            mc.parse_deadline('2026-10-15T06:00:00+02:00')
        with self.assertRaises(ValueError):
            mc.parse_deadline('tomorrow')

    def test_git_worktree_detection(self):
        with tempfile.TemporaryDirectory() as d:
            (Path(d) / 'repo' / '.git').mkdir(parents=True)
            self.assertTrue(mc.inside_git_worktree(Path(d) / 'repo' / 'a' / 'b'))
            with self.assertRaises(mc.GuardError):
                mc.require_outside_repo(Path(d) / 'repo' / 'out', 'x')


class ProvisionTest(Temp):
    def args(self, *extra):
        return provision.parse_args(['--label', 'campaign=mn-test', '--controller-cidr', CTL,
                                     '--ssh-key-name', 'existing-key', '--cloud-init-dir', str(self.tmp / 'ci'),
                                     '--work-dir', str(self.tmp / 'work'), '--protected-cidr', '10.77.0.0/16',
                                     '--protected-name', 'ci-fleet', *extra])

    def test_dry_run_prints_and_executes_nothing(self):
        runner, out = FakeRunner(), io.StringIO()
        result = provision.run(self.args(), runner=runner, now=1792020000, out=out)
        self.assertFalse(result['executed'])
        self.assertEqual(runner.calls, [])
        self.assertFalse((self.tmp / 'work').exists())
        cmds = result['commands']
        creates = [c for c in cmds if c[2] == 'create']
        self.assertEqual(len(creates), 2 + 4)  # network, firewall, four servers
        for c in creates:
            self.assertIn('campaign=mn-test', c)
            self.assertTrue(c[c.index('--name') + 1].startswith('fdb-bench-vpc'))
        servers = [c for c in creates if c[1] == 'server']
        for c in servers:
            for flag in ['--start-after-create=false', '--without-ipv6']:
                self.assertIn(flag, c)
            self.assertEqual(c[c.index('--firewall') + 1], 'fdb-bench-vpc-fw')
            self.assertEqual(c[c.index('--location') + 1], 'fsn1')
        attaches = {c[3]: c[-1] for c in cmds if c[2] == 'attach-to-network'}
        self.assertEqual(attaches, {'fdb-bench-vpc-client': '10.88.0.10', 'fdb-bench-vpc-node1': '10.88.0.11',
                                    'fdb-bench-vpc-node2': '10.88.0.12', 'fdb-bench-vpc-node3': '10.88.0.13'})
        # power on only after every server is attached
        first_poweron = min(i for i, c in enumerate(cmds) if c[2] == 'poweron')
        self.assertTrue(all(i < first_poweron for i, c in enumerate(cmds) if c[2] == 'attach-to-network'))
        self.assertEqual(result['rules'][0]['source_ips'], [CTL])
        self.assertIn('DRY RUN', out.getvalue())

    def test_ssh_public_key_creates_labelled_key(self):
        cmds = provision.build_commands(provision.parse_args([
            '--label', 'campaign=mn-test', '--controller-cidr', CTL, '--ssh-public-key', '/x/id.pub',
            '--cloud-init-dir', 'ci', '--work-dir', 'w']))[0]
        key = [c for c in cmds if c[1] == 'ssh-key']
        self.assertEqual(len(key), 1)
        self.assertIn('campaign=mn-test', key[0])
        self.assertTrue(all(c[c.index('--ssh-key') + 1] == 'fdb-bench-vpc-controller' for c in cmds if c[1:3] == ['server', 'create']))

    def test_refusals(self):
        bad = [
            ['--ip-range', '10.77.1.0/24', '--subnet-range', '10.77.1.0/24', '--client-ip', '10.77.1.10',
             '--node-ip', '10.77.1.11', '--node-ip', '10.77.1.12', '--node-ip', '10.77.1.13'],
            ['--client-ip', '10.88.0.1'],          # Hetzner gateway
            ['--client-ip', '10.88.1.5'],          # outside subnet
            ['--node-ip', '10.88.0.11', '--node-ip', '10.88.0.11', '--node-ip', '10.88.0.13'],
            ['--controller-cidr', '0.0.0.0/0'],
            ['--network-name', 'ci-fleet'],
            ['--prefix', 'Bad_Prefix'],
        ]
        for extra in bad:
            with self.subTest(extra=extra), self.assertRaises((mc.GuardError, ValueError)):
                provision.build_commands(self.args(*extra))

    def test_execute_requires_deadline_and_private_cloud_init(self):
        runner = FakeRunner()
        with self.assertRaisesRegex(mc.GuardError, 'teardown-deadline'):
            provision.run(self.args('--execute'), runner=runner, now=1792020000, out=io.StringIO())
        with self.assertRaisesRegex(mc.GuardError, 'within'):
            provision.run(self.args('--execute', '--teardown-deadline', str(1792020000 + 7 * 3600)),
                          runner=runner, now=1792020000, out=io.StringIO())
        with self.assertRaisesRegex(mc.GuardError, 'missing cloud-init'):
            provision.run(self.args('--execute', '--teardown-deadline', str(1792020000 + 3600)),
                          runner=runner, now=1792020000, out=io.StringIO())
        ci = self.tmp / 'ci'
        ci.mkdir()
        for role in provision.ROLE_NAMES:
            (ci / f'fdb-bench-vpc-{role}.user-data').write_text('#cloud-config\n')
        with self.assertRaisesRegex(mc.GuardError, '0600'):
            provision.run(self.args('--execute', '--teardown-deadline', str(1792020000 + 3600)),
                          runner=runner, now=1792020000, out=io.StringIO())
        self.assertEqual(runner.calls, [])

    def _ready(self):
        ci = self.tmp / 'ci'
        ci.mkdir()
        for role in provision.ROLE_NAMES:
            f = ci / f'fdb-bench-vpc-{role}.user-data'
            f.write_text('#cloud-config\n')
            f.chmod(0o600)
        return self.args('--execute', '--teardown-deadline', str(1792020000 + 5 * 3600))

    def test_preflight_refuses_collisions_before_any_create(self):
        args = self._ready()
        runner = FakeRunner({('hcloud', 'network', 'list'): (0, json.dumps([{'name': 'ci-fleet', 'ip_range': '10.88.0.0/16'}]), '')},
                            default=(0, '[]', ''))
        with self.assertRaisesRegex(mc.GuardError, 'overlaps'):
            provision.run(args, runner=runner, now=1792020000, out=io.StringIO())
        self.assertTrue(all(c[2] == 'list' for c in runner.calls))

    def test_execute_runs_in_order_and_records_hosts(self):
        args = self._ready()

        def describe(argv):
            name = argv[3]
            return 0, json.dumps(dict(id=1, name=name, public_net=dict(ipv4=dict(ip='198.51.100.9')),
                                      private_net=[dict(ip='10.88.0.10')], labels={'campaign': 'mn-test'})), ''
        runner = FakeRunner({('hcloud', 'server', 'list'): (0, 'null', ''), ('hcloud', 'network', 'list'): (0, '[]', ''),
                             ('hcloud', 'firewall', 'list'): (0, '[]', ''), ('hcloud', 'server', 'describe'): describe})
        result = provision.run(args, runner=runner, now=1792020000, out=io.StringIO())
        self.assertTrue(result['executed'])
        mutating = [c for c in runner.calls if c[2] not in ('list', 'describe')]
        self.assertEqual(mutating, [[str(x) for x in c] for c in result['commands']])
        self.assertTrue(all(f'deadline={1792020000 + 5 * 3600}' in c for c in mutating if c[2] == 'create'))
        hosts = json.loads((self.tmp / 'work' / 'hosts-client-node1-node2-node3.json').read_text())
        self.assertEqual(set(hosts), set(provision.ROLE_NAMES))
        rules = json.loads((self.tmp / 'work' / 'firewall-rules.json').read_text())
        self.assertEqual({r['protocol'] for r in rules}, {'tcp', 'icmp'})

    def test_staged_client_then_nodes(self):
        stage1 = provision.build_commands(self.args('--role', 'client'))[0]
        self.assertEqual([c[1:3] for c in stage1], [['network', 'create'], ['network', 'add-subnet'], ['firewall', 'create'],
                                                    ['server', 'create'], ['server', 'attach-to-network'], ['server', 'poweron']])
        args = self._ready()
        args.role, args.skip_shared = ['node1', 'node2', 'node3'], True
        stage2 = provision.build_commands(args)[0]
        self.assertFalse(any(c[1] in ('network', 'firewall', 'ssh-key') for c in stage2))
        self.assertEqual(sum(c[2] == 'create' for c in stage2), 3)
        shared = {('hcloud', 'server', 'list'): (0, json.dumps([dict(name='fdb-bench-vpc-client')]), '')}
        for kind, extra in [('network', dict(ip_range='10.88.0.0/24')), ('firewall', {})]:
            shared[('hcloud', kind, 'list')] = (0, json.dumps([dict(name='fdb-bench-vpc' + ('-fw' if kind == 'firewall' else ''),
                                                                   labels={'campaign': 'mn-test'}, **extra)]), '')
        provision.preflight(args, FakeRunner(shared))
        for kind in ['network', 'firewall']:
            stranger = dict(shared)
            stranger[('hcloud', kind, 'list')] = (0, json.dumps([dict(name='fdb-bench-vpc' + ('-fw' if kind == 'firewall' else ''),
                                                                     labels={'campaign': 'other'}, ip_range='10.88.0.0/24')]), '')
            with self.subTest(kind=kind), self.assertRaisesRegex(mc.GuardError, 'without label'):
                provision.preflight(args, FakeRunner(stranger))
        args.skip_shared = False
        with self.assertRaisesRegex(mc.GuardError, 'already exists'):
            provision.preflight(args, FakeRunner(shared))

    def test_execute_stops_on_first_failure(self):
        args = self._ready()
        runner = FakeRunner({('hcloud', 'firewall', 'create'): (1, '', 'boom')}, default=(0, '[]', ''))
        with self.assertRaisesRegex(mc.GuardError, 'teardown_guard'):
            provision.run(args, runner=runner, now=1792020000, out=io.StringIO())
        self.assertFalse(any(c[1] == 'server' and c[2] == 'create' for c in runner.calls))


class TeardownTest(Temp):
    def inventory(self):
        mine = {'campaign': 'mn-test'}
        return {
            'server': [dict(id=11, name='fdb-bench-vpc-client', labels=mine), dict(id=12, name='fdb-bench-vpc-node1', labels=mine),
                       dict(id=13, name='gh-runner-7', labels=mine), dict(id=14, name='sommer2026', labels=mine)],
            'firewall': [dict(id=21, name='fdb-bench-vpc-fw', labels=mine)],
            'ssh-key': [],
            'network': [dict(id=31, name='fdb-bench-vpc', labels=mine), dict(id=32, name='ci-fleet', labels={'campaign': 'other'})],
        }

    def runner(self, inventory, fail_first=()):
        failures = collections.Counter(fail_first)

        def lister(argv):
            return 0, json.dumps(inventory[argv[1]]), ''

        def deleter(argv):
            kind, rid = argv[1], int(argv[3])
            if failures[(kind, rid)]:
                failures[(kind, rid)] -= 1
                return 1, '', 'resource in use'
            inventory[kind] = [x for x in inventory[kind] if x['id'] != rid]
            return 0, '', ''
        answers = {}
        for kind in tg.KINDS:
            answers[('hcloud', kind, 'list')] = lister
            answers[('hcloud', kind, 'delete')] = deleter
        return FakeRunner(answers)

    def args(self, *extra):
        return tg.parse_args(['--label', 'campaign=mn-test', '--prefix', 'fdb-bench-vpc', '--protect-name', 'sommer2026',
                              '--protect-name', 'gh-runner-*', '--retry-seconds', '0', *extra])

    def test_dry_run_deletes_nothing_and_refuses_strangers(self):
        inv = self.inventory()
        runner = self.runner(inv)
        ok, receipt = tg.run(self.args(), runner=runner, clock=lambda: 1792020000, sleep=lambda s: None, out=io.StringIO())
        self.assertFalse(ok)  # labelled resources outside the prefix are suspicious
        self.assertFalse(any(c[2] == 'delete' for c in runner.calls))
        self.assertTrue(all('--selector' in c and 'campaign=mn-test' in c for c in runner.calls))
        owned = {e['name'] for k in tg.KINDS for e in receipt['found'][k]['owned']}
        refused = {e['name'] for k in tg.KINDS for e in receipt['found'][k]['refused']}
        self.assertEqual(owned, {'fdb-bench-vpc-client', 'fdb-bench-vpc-node1', 'fdb-bench-vpc-fw', 'fdb-bench-vpc'})
        self.assertEqual(refused, {'gh-runner-7', 'sommer2026', 'ci-fleet'})

    def test_execute_deletes_only_owned_in_dependency_order(self):
        inv = self.inventory()
        runner = self.runner(inv, fail_first=[('network', 31), ('network', 31)])
        ok, receipt = tg.run(self.args('--execute', '--protect-id', '999'), runner=runner,
                             clock=lambda: 1792020000, sleep=lambda s: None, out=io.StringIO())
        deletes = [(c[1], int(c[3])) for c in runner.calls if c[2] == 'delete']
        self.assertEqual(deletes, [('server', 11), ('server', 12), ('firewall', 21), ('network', 31),
                                   ('network', 31), ('network', 31)])
        self.assertEqual(receipt['remaining'], [])
        self.assertEqual({x['name'] for x in inv['server']}, {'gh-runner-7', 'sommer2026'})
        self.assertEqual([x['name'] for x in inv['network']], ['ci-fleet'])
        self.assertFalse(ok)  # still nonzero: refused labelled strangers were reported

    def test_survivor_makes_failure(self):
        inv = {k: [] for k in tg.KINDS}
        inv['server'] = [dict(id=11, name='fdb-bench-vpc-client', labels={'campaign': 'mn-test'})]
        runner = self.runner(inv, fail_first=[('server', 11)])
        ok, receipt = tg.run(self.args('--execute'), runner=runner, clock=lambda: 0, sleep=lambda s: None, out=io.StringIO())
        self.assertFalse(ok)
        self.assertEqual(receipt['remaining'], [dict(kind='server', id=11, name='fdb-bench-vpc-client')])

    def test_label_rechecked_client_side(self):
        args = self.args()
        owned, refused = tg.classify(args, 'server', [dict(id=1, name='fdb-bench-vpc-x', labels={'campaign': 'other'})])
        self.assertEqual(owned, [])
        self.assertIn('label mismatch', refused[0]['reasons'])

    def test_deadline_waits_then_acts(self):
        inv = {k: [] for k in tg.KINDS}
        inv['server'] = [dict(id=11, name='fdb-bench-vpc-client', labels={'campaign': 'mn-test'})]
        now = [1792020000.0]
        sleeps = []

        def sleep(s):
            sleeps.append(s)
            now[0] += s
        runner = self.runner(inv)
        ok, _ = tg.run(self.args('--execute', '--deadline', str(1792020000 + 100), '--poll-seconds', '30',
                                 '--receipt-dir', str(self.tmp / 'r')),
                       runner=runner, clock=lambda: now[0], sleep=sleep, out=io.StringIO())
        self.assertTrue(ok)
        self.assertEqual(sleeps, [30, 30, 30, 10])
        self.assertEqual(inv['server'], [])
        self.assertEqual(len(list((self.tmp / 'r').glob('teardown-*-execute.json'))), 1)


def fake_host_key(d, host):
    k = d / host
    k.mkdir(parents=True)
    priv = k / 'ssh_host_ed25519_key'
    priv.write_text('\n'.join([rci.PRIVATE_BEGIN, 'AAAAfakefakefake', rci.PRIVATE_END]) + '\n')
    priv.chmod(0o600)
    (k / 'ssh_host_ed25519_key.pub').write_text(f'ssh-ed25519 AAAAC3NzaFAKE{host.replace("-", "")} root@{host}\n')


class CloudInitTest(Temp):
    def test_render_pins_host_key_and_controller_public_key(self):
        keys = self.tmp / 'keys'
        for h in ['fdb-bench-vpc-client', 'fdb-bench-vpc-node1']:
            fake_host_key(keys, h)
        pub = self.tmp / 'ctl.pub'
        pub.write_text('ssh-ed25519 AAAAC3NzaCONTROLLER me@controller\n')
        out = self.tmp / 'ci'
        written = rci.render(keys, pub, ['fdb-bench-vpc-client', 'fdb-bench-vpc-node1'], out)
        self.assertEqual(len(written), 2)
        doc = (out / 'fdb-bench-vpc-node1.user-data').read_text()
        self.assertTrue(doc.startswith('#cloud-config\n'))
        self.assertIn('ssh_pwauth: false', doc)
        self.assertIn('ssh_genkeytypes: []', doc)
        self.assertIn('    ' + rci.PRIVATE_BEGIN, doc)
        self.assertIn('ed25519_public: ssh-ed25519 AAAAC3NzaFAKEfdbbenchvpcnode1', doc)
        self.assertIn('  - ssh-ed25519 AAAAC3NzaCONTROLLER me@controller', doc)
        self.assertNotIn('fdb-bench-vpc-client root@', doc)
        self.assertEqual((out / 'fdb-bench-vpc-node1.user-data').stat().st_mode & 0o777, 0o600)
        self.assertEqual(out.stat().st_mode & 0o777, 0o700)
        self.assertEqual((out / 'known_hosts').read_text().splitlines(),
                         ['fdb-bench-vpc-client ssh-ed25519 AAAAC3NzaFAKEfdbbenchvpcclient',
                          'fdb-bench-vpc-node1 ssh-ed25519 AAAAC3NzaFAKEfdbbenchvpcnode1'])
        with self.assertRaises(FileExistsError):  # never silently overwrites
            rci.render(keys, pub, ['fdb-bench-vpc-client'], out)

    def test_refusals(self):
        keys = self.tmp / 'keys'
        fake_host_key(keys, 'h1')
        priv_as_pub = self.tmp / 'oops.pub'
        priv_as_pub.write_text(rci.PRIVATE_BEGIN + '\nAAAA\n' + rci.PRIVATE_END + '\n')
        with self.assertRaisesRegex(mc.GuardError, 'private key'):
            rci.render(keys, priv_as_pub, ['h1'], self.tmp / 'o1')
        pub = self.tmp / 'ctl.pub'
        pub.write_text('ssh-ed25519 AAAA me\n')
        repo = self.tmp / 'repo'
        (repo / '.git').mkdir(parents=True)
        with self.assertRaisesRegex(mc.GuardError, 'Git work tree'):
            rci.render(keys, pub, ['h1'], repo / 'out')
        (keys / 'h1' / 'ssh_host_ed25519_key').chmod(0o644)
        with self.assertRaisesRegex(mc.GuardError, '0600'):
            rci.render(keys, pub, ['h1'], self.tmp / 'o2')
        with self.assertRaises(mc.GuardError):
            rci.render(keys, pub, ['Bad Host'], self.tmp / 'o3')

    @unittest.skipUnless(shutil.which('ssh-keygen'), 'ssh-keygen not installed')
    def test_generate_real_throwaway_keys(self):
        keys = rci.generate_keys(self.tmp / 'keys', ['h1', 'h2'])
        for h in ['h1', 'h2']:
            priv = keys / h / 'ssh_host_ed25519_key'
            self.assertEqual(priv.stat().st_mode & 0o777, 0o600)
            self.assertTrue((keys / h / 'ssh_host_ed25519_key.pub').read_text().startswith('ssh-ed25519 '))
        with self.assertRaisesRegex(mc.GuardError, 'exists'):
            rci.generate_keys(self.tmp / 'keys', ['h1'])
        pub = self.tmp / 'ctl.pub'
        pub.write_text('ssh-ed25519 AAAA me\n')
        self.assertEqual(len(rci.render(keys, pub, ['h1', 'h2'], self.tmp / 'ci')), 2)


def status_from_layout(layout, mutate=None):
    procs = {}
    roles_of = {'stateless': ['cluster_controller', 'master', 'data_distributor', 'ratekeeper'],
                'commit_proxy': ['commit_proxy'], 'grv_proxy': ['grv_proxy'], 'resolution': ['resolver'],
                'transaction': ['log'], 'storage': ['storage'], 'coordinator': []}
    for i, p in enumerate(layout['processes']):
        roles = list(roles_of[p['process_class']])
        if p['address'] in layout['coordinators']:
            roles.append('coordinator')
        procs[f'p{i}'] = dict(address=p['address'], class_type=p['process_class'],
                              locality=dict(machineid=p['node'], zoneid=p['node']),
                              roles=[dict(role=r, id=f'{r}{i}') for r in roles])
    status = dict(client=dict(coordinators=dict(coordinators=[dict(address=a, reachable=True) for a in layout['coordinators']]),
                              database_status=dict(available=True)),
                  cluster=dict(database_available=True, processes=procs,
                               configuration=dict(redundancy_mode='double', storage_engine=layout['engine'],
                                                  commit_proxies=1, grv_proxies=1, resolvers=1, logs=2)))
    if mutate:
        mutate(status)
    return status


class ClusterTest(Temp):
    def render(self, *extra):
        args = fdb_cluster.parse_args(['render', *sum([['--node-ip', ip] for ip in NODES], []), '--client-ip', '10.88.0.10',
                                       '--controller-cidr', CTL, '--description', 'fdbbenchvpc', '--cluster-id', 'mn20261014',
                                       '--output-dir', str(self.tmp / 'out'), *extra])
        return fdb_cluster.render(args)

    def test_render_layout(self):
        layout = self.render('--knob', 'min_shard_bytes=1000000')
        out = self.tmp / 'out'
        self.assertEqual((out / 'fdb.cluster').read_text(),
                         'fdbbenchvpc:mn20261014@10.88.0.11:4500,10.88.0.12:4500,10.88.0.13:4500\n')
        self.assertEqual((out / 'client' / 'fdb.cluster').read_text(), (out / 'fdb.cluster').read_text())
        self.assertEqual((out / 'configure.fdbcli').read_text().strip(),
                         'configure new double ssd-2 commit_proxies=1 grv_proxies=1 resolvers=1 logs=2')
        self.assertEqual(layout['storage_servers'], 9)
        classes = collections.Counter((p['node'], p['process_class']) for p in layout['processes'])
        self.assertEqual(classes[('node1', 'storage')], 3)
        self.assertEqual(classes[('node2', 'transaction')], 1)
        self.assertEqual(classes[('node3', 'transaction')], 1)
        for node in fdb_cluster.NODES:
            cpus = [c for p in layout['processes'] if p['node'] == node for c in p['cpus']]
            self.assertEqual(len(cpus), len(set(cpus)))
            self.assertFalse({0, 1} & set(cpus))
            for p in layout['processes']:
                if p['node'] != node:
                    continue
                self.assertEqual(p['cpus'][1], p['cpus'][0] + 1)  # one sibling pair
                unit = (out / node / f'fdb-bench-{p["port"]}.service').read_text()
                self.assertIn(f'CPUAffinity={p["cpus"][0]} {p["cpus"][1]}', unit)
                self.assertIn('Restart=no', unit)
                self.assertIn(f'--public-address {p["address"]}', unit)
                self.assertIn('--knob_min_shard_bytes=1000000', unit)
            conf = (out / node / 'foundationdb.conf').read_text()
            self.assertIn('knob_min_shard_bytes = 1000000', conf)
            self.assertIn('[fdbserver.4501]', conf)
            nft = (out / node / 'nftables.conf').read_text()
            self.assertIn('policy drop', nft)
            self.assertIn('ip saddr 10.88.0.0/24 accept', nft)
            self.assertIn(f'ip saddr {{ {CTL} }} tcp dport 22 accept', nft)
            if shutil.which('bash'):
                subprocess.run(['bash', '-n', str(out / node / 'install-units.sh')], check=True)
        memory = [p for p in layout['processes'] if p['process_class'] == 'storage'][0]['args']
        self.assertEqual(memory[memory.index('--storage-memory') + 1], '1GiB')

    def test_render_refusals(self):
        for extra in [['--client-ip', '10.88.0.11'], ['--knob', 'BAD=1'], ['--description', 'a:b'],
                      ['--client-ip', '10.99.0.10']]:
            with self.subTest(extra=extra), self.assertRaises((mc.GuardError, SystemExit)):
                self.render(*extra)
            shutil.rmtree(self.tmp / 'out', ignore_errors=True)

    def test_check_roles(self):
        layout = self.render()
        self.assertTrue(fdb_cluster.check_roles(layout, status_from_layout(layout))['passed'])

        def moved_proxy(s):
            for p in s['cluster']['processes'].values():
                if p['class_type'] == 'storage':
                    p['roles'].append(dict(role='commit_proxy'))
                    return

        def wrong_engine(s):
            s['cluster']['configuration']['storage_engine'] = 'memory'

        def lost_coordinator(s):
            s['client']['coordinators']['coordinators'].pop()

        def missing_process(s):
            s['cluster']['processes'].pop('p0')

        def unavailable(s):
            s['cluster']['database_available'] = False

        for mutate, text in [(moved_proxy, 'unexpected roles'), (wrong_engine, 'storage_engine'),
                             (lost_coordinator, 'coordinators'), (missing_process, 'missing'),
                             (unavailable, 'not available')]:
            with self.subTest(mutate=mutate.__name__):
                receipt = fdb_cluster.check_roles(layout, status_from_layout(layout, mutate))
                self.assertFalse(receipt['passed'])
                self.assertTrue(any(text in e for e in receipt['errors']), receipt['errors'])


SERVERS = {'A': '10.88.0.11:4504', 'B': '10.88.0.12:4502', 'C': '10.88.0.13:4502', 'D': '10.88.0.11:4505',
           'E': '10.88.0.12:4503'}
MACHINE = {'A': 'node1', 'B': 'node2', 'C': 'node3', 'D': 'node1', 'E': 'node2'}


def storage_status():
    return dict(cluster=dict(processes={
        k: dict(address=a, class_type='storage', locality=dict(machineid=MACHINE[k], zoneid=MACHINE[k]),
                roles=[dict(role='storage', id=k)]) for k, a in SERVERS.items()} | {
        'tlog': dict(address='10.88.0.12:4501', class_type='transaction', locality=dict(machineid='node2'),
                     roles=[dict(role='log')])}))


def probe(prefix_items=('benchfetch', 'small'), rows=20, index_servers='AB', record_servers='CD', chunks=1,
          boundary_after_records=True, override=None):
    prefix = pr.pack(list(prefix_items))
    keys = []
    for i in range(rows):
        keys.append(dict(kind='index', id=i, key_hex=(prefix + pr.pack([2, 'price_idx', i, i])).hex(),
                         addresses=[SERVERS[s] for s in index_servers]))
        for c in range(chunks):
            suffix = [] if chunks == 1 else [c + 1]
            servers = (override or {}).get(i, record_servers)
            keys.append(dict(kind='record', id=i, key_hex=(prefix + pr.pack([1, i, *suffix])).hex(),
                             addresses=[SERVERS[s] for s in servers]))
    boundaries = [(prefix + pr.pack([1, 1000000])).hex()] if boundary_after_records else []
    return dict(format=pr.PROBE_FORMAT, store='benchfetch/small', store_prefix_hex=prefix.hex(), read_version=7,
                boundaries_hex=boundaries, keys=keys)


class PlacementTest(Temp):
    def test_tuple_encoding_vectors(self):
        self.assertEqual(pr.pack(['benchfetch', 'small']), b'\x02benchfetch\x00\x02small\x00')
        self.assertEqual(pr.pack([1]), b'\x15\x01')
        self.assertEqual(pr.pack([0]), b'\x14')
        self.assertEqual(pr.pack([999]), b'\x16\x03\xe7')
        self.assertEqual(pr.pack([1000000]), b'\x17\x0f\x42\x40')
        self.assertEqual(pr.pack([-1]), b'\x13\xfe')
        self.assertEqual(pr.pack([-256]), b'\x12\xfe\xff')
        self.assertEqual(pr.pack([b'a\x00b', None]), b'\x01a\x00\xffb\x00\x00')
        self.assertLess(pr.pack([1, 999]), pr.pack([1, 1000000]))
        self.assertLess(pr.pack([1, 1000000]), pr.pack([2]))  # ballast sorts between records and index
        with self.assertRaises(pr.InputError):
            pr.pack([1.5])

    def receipt(self, p, **kw):
        cells = {'rows10': (0, 10), 'rows20': (0, 20)}
        kw.setdefault('layout', 'recordlayer')
        kw.setdefault('rows', 20)
        return pr.build_receipt(storage_status(), p, cells, **kw)

    def test_disjoint_placement_passes(self):
        r = self.receipt(probe(), chunks=1, expect_prefix=pr.pack(['benchfetch', 'small']))
        self.assertTrue(r['passed'], r['errors'])
        self.assertEqual(r['cells']['rows20']['server_disjoint_rows'], 20)
        self.assertEqual(r['cells']['rows20']['machine_disjoint_fraction'], 0.0)  # A,B vs C,D share node1
        self.assertEqual((r['index_shards'], r['record_shards']), (1, 1))

    def test_split_records_and_machine_level(self):
        r = self.receipt(probe(index_servers='BE', record_servers='CD', chunks=2), chunks=2)
        self.assertTrue(r['passed'], r['errors'])
        self.assertEqual(r['cells']['rows20']['machine_disjoint_rows'], 20)  # node2 vs node3,node1
        r = self.receipt(probe(index_servers='AB', record_servers='CE'), chunks=1)
        self.assertTrue(r['passed'], r['errors'])  # server-disjoint suffices for a pass
        self.assertEqual(r['cells']['rows20']['machine_disjoint_rows'], 0)  # both touch node2

    def test_partial_overlap_reports_fraction(self):
        p = probe(override={i: 'AC' for i in range(5)}, boundary_after_records=False)
        p['boundaries_hex'] = [(pr.pack(['benchfetch', 'small']) + pr.pack([1, 5])).hex(),
                               (pr.pack(['benchfetch', 'small']) + pr.pack([1, 1000000])).hex()]
        r = self.receipt(p, chunks=1, min_fraction=0.7)
        self.assertEqual(r['cells']['rows20']['server_disjoint_rows'], 15)
        self.assertTrue(r['cells']['rows20']['passed'])
        self.assertFalse(r['cells']['rows10']['passed'])  # 5/10 < 0.7
        self.assertFalse(r['passed'])

    def test_integrity_errors(self):
        cases = [
            (probe(boundary_after_records=False), 'hold both'),
            (probe(index_servers='A'), 'replicas'),
            (probe(chunks=2), 'expected 1'),
        ]
        bad_addr = probe()
        bad_addr['keys'][0]['addresses'] = ['10.88.0.12:4501', SERVERS['B']]
        cases.append((bad_addr, 'not a storage server'))
        inconsistent = probe(override={4: 'CE'})
        cases.append((inconsistent, 'different server sets'))
        wrong_key = probe()
        wrong_key['keys'][0]['key_hex'] = (pr.pack(['benchfetch', 'small']) + pr.pack([2, 'price_idx', 0, 1])).hex()
        cases.append((wrong_key, 'index key missing'))
        for p, text in cases:
            with self.subTest(text=text):
                r = self.receipt(p, chunks=1)
                self.assertFalse(r['passed'])
                self.assertTrue(any(text in e for e in r['errors']), r['errors'])
        r = self.receipt(probe(), chunks=1, expect_prefix=pr.pack(['benchfetch', 'split']))
        self.assertTrue(any('expected' in e for e in r['errors']))

    def test_missing_ids_fail_cell(self):
        r = pr.build_receipt(storage_status(), probe(rows=5), {'rows10': (0, 10)}, layout='opaque')
        self.assertEqual(r['cells']['rows10']['missing_ids'], 5)
        self.assertFalse(r['passed'])

    def test_compare_and_cli(self):
        status, p1 = self.tmp / 'status.json', self.tmp / 'p1.json'
        status.write_text(json.dumps(storage_status()))
        p1.write_text(json.dumps(probe(rows=1000)))
        common = ['receipt', '--status', str(status), '--preset', 'benchfetch', '--layout', 'recordlayer',
                  '--chunks', '1', '--expect-prefix-tuple', 'benchfetch,small']
        with contextlib.redirect_stdout(io.StringIO()):
            self.assertEqual(pr.main(common + ['--probe', str(p1), '--output', str(self.tmp / 'r1.json')]), 0)
            self.assertEqual(pr.main(common + ['--probe', str(p1), '--output', str(self.tmp / 'r2.json')]), 0)
            self.assertEqual(pr.main(['compare', str(self.tmp / 'r1.json'), str(self.tmp / 'r2.json')]), 0)
            moved = probe(rows=1000, record_servers='CE')
            (self.tmp / 'p3.json').write_text(json.dumps(moved))
            self.assertEqual(pr.main(common + ['--probe', str(self.tmp / 'p3.json'), '--output', str(self.tmp / 'r3.json')]), 0)
            self.assertEqual(pr.main(['compare', str(self.tmp / 'r1.json'), str(self.tmp / 'r3.json')]), 1)
        r1 = json.loads((self.tmp / 'r1.json').read_text())
        self.assertEqual(set(r1['cells']), set(pr.PRESETS['benchfetch']))
        self.assertEqual(r1['cells']['rows100']['rows'], 100)
        self.assertEqual(r1['inputs']['probe_sha256'], pr.sha(p1))
        (self.tmp / 'bad.json').write_text(json.dumps(dict(format='other')))
        with contextlib.redirect_stderr(io.StringIO()):
            self.assertEqual(pr.main(common + ['--probe', str(self.tmp / 'bad.json'), '--output', str(self.tmp / 'r4.json')]), 2)


def mini_protocol():
    client = dict(kind='client', arms=['final-Go', 'CGo-7.3.77'], workloads=['get100', 'range1000'],
                  new_harness_workloads=['range1000'], blocks=6, repeats_per_arm_process=1,
                  per_workload_campaign_wall_cap_seconds=900, per_process_timeout_seconds=180, engine='ssd-2')
    return dict(
        campaign='mn-test', production_revisions=dict(final='b' * 40, before_allocation_work_after_read_parity='a' * 40),
        runtime=dict(GOMAXPROCS=8, client_vm=dict(housekeeping_cpus='0-1', idle_sentinel_cpus='2-7', client_cpus='8-15')),
        phases=[
            dict(client, id='M1w1', workers=1, iterations=3000, warmup=200, order_seed=742001, frozen_order_key='M1w1'),
            dict(client, id='M1w64', workers=64, iterations=32000, warmup=1600, order_seed=742301, frozen_order_key='M1w64'),
            dict(id='M2rl', kind='testingb', binary_kind='fetch', arms=['final-ordinary', 'final-remote'],
                 payloads=['small', 'split'], cells=['limit1', 'rows10', 'rows100', 'rows1000'], dirs=['forward', 'reverse'],
                 new_harness_cells=['rows100'], placement_required=True, repeats_per_arm_process=3, benchtime='3s',
                 blocks=6, order_seed=742401, frozen_order_key='M2rl', process_timeout_seconds=600,
                 wall_cap_seconds=6300, engine='ssd-2'),
            dict(id='M2sql', kind='testingb', binary_kind='sqlfetch', arms=['final-ordinary', 'final-remote'],
                 cells=['rows10', 'rows100', 'rows1000'], new_harness_cells=['rows100'], new_harness_phase=True,
                 placement_required=True, repeats_per_arm_process=3, benchtime='3s', blocks=6, order_seed=742601,
                 frozen_order_key='M2sql', process_timeout_seconds=600, wall_cap_seconds=1200, engine='ssd-2'),
        ])


def run_py_orders(seed, n, blocks):
    """What tools/clientbench/scripts/run.py derives for one workload."""
    rng = random.Random(seed)
    orders = list(itertools.permutations(range(n))) * (blocks // math.factorial(n))
    rng.shuffle(orders)
    return orders


class SealTest(Temp):
    def test_orders_balanced_and_match_run_py(self):
        protocol = mini_protocol()
        orders = seal.make_orders(protocol)
        self.assertEqual(orders, seal.make_orders(protocol))
        for phase in protocol['phases']:
            o = orders[phase['id']]
            self.assertEqual(o['seed'], phase['order_seed'])
            want = [[phase['arms'][i] for i in order] for order in run_py_orders(phase['order_seed'], 2, 6)]
            self.assertEqual(o['orders'], want)
            self.assertEqual(collections.Counter(b[0] for b in o['orders']), {a: 3 for a in phase['arms']})
        three = dict(protocol['phases'][0], arms=['before-Go', 'final-Go', 'CGo-7.3.77'])
        self.assertEqual(len(set(map(tuple, seal.phase_orders(three)))), 6)
        with self.assertRaises(ValueError):
            seal.phase_orders(dict(three, blocks=4))
        dup = mini_protocol()
        dup['phases'][1]['order_seed'] = dup['phases'][0]['order_seed']
        with self.assertRaises(ValueError):
            seal.make_orders(dup)

    def test_check_orders_cli(self):
        p, o = self.tmp / 'protocol.json', self.tmp / 'orders.json'
        p.write_text(json.dumps(mini_protocol()))
        with contextlib.redirect_stdout(io.StringIO()):
            self.assertEqual(seal.main(['orders', '--protocol', str(p), '--output', str(o)]), 0)
            self.assertEqual(seal.main(['check-orders', '--protocol', str(p), '--orders', str(o)]), 0)
            with self.assertRaises(FileExistsError):
                seal.main(['orders', '--protocol', str(p), '--output', str(o)])
            tampered = json.loads(o.read_text())
            tampered['M1w1']['orders'].reverse()
            o.write_text(json.dumps(tampered))
            self.assertEqual(seal.main(['check-orders', '--protocol', str(p), '--orders', str(o)]), 1)

    def fixture_files(self):
        p, o = self.tmp / 'protocol.json', self.tmp / 'orders.json'
        p.write_text(json.dumps(mini_protocol()))
        o.write_text(json.dumps(seal.make_orders(mini_protocol())))
        for name in ['fetch', 'sqlfetch', 'cluster', 'final', 'cgo', 'native', 'server', 'harness.tar.gz']:
            (self.tmp / name).write_text(name)
        return p, o

    def test_testingb_plans_leave_new_harness_unsealed(self):
        p, o = self.fixture_files()
        common = ['testingb-plans', '--protocol', str(p), '--orders', str(o), '--cluster-file', str(self.tmp / 'cluster'),
                  '--binary', f'fetch={self.tmp / "fetch"}', '--binary', f'sqlfetch={self.tmp / "sqlfetch"}']
        with contextlib.redirect_stdout(io.StringIO()):
            seal.main(common + ['--output-dir', str(self.tmp / 'tb1')])
        manifest = json.loads((self.tmp / 'tb1' / 'manifest.json').read_text())
        self.assertEqual([x['phase'] for x in manifest['phases']], ['M2rl'])
        unsealed = {(u['phase'], u['case']) for u in manifest['unsealed']}
        self.assertIn(('M2rl', 'small-rows100-forward'), unsealed)
        self.assertEqual({c for ph, c in unsealed if ph == 'M2sql'}, {'sql-rows10', 'sql-rows100', 'sql-rows1000'})
        plan = json.loads((self.tmp / 'tb1' / 'M2rl.plan.json').read_text())
        self.assertEqual(len(plan['cases']), 12)
        self.assertEqual(len(plan['jobs']), 12 * 6 * 2)
        job = plan['jobs'][0]
        self.assertEqual(job['command'][:3], ['taskset', '--cpu-list', '8-15'])
        self.assertIn('-test.bench=^BenchmarkIndexFetch$/^payload=small$/^cell=limit1$/^dir=forward$/^method=', job['command'][6])
        self.assertIn('-test.count=3', job['command'])
        self.assertEqual(plan['env']['FDB_BENCH_CLUSTER_FILE'], str((self.tmp / 'cluster').resolve()))
        orders = json.loads(o.read_text())['M2rl']['orders']
        for case in plan['cases']:
            for b in range(6):
                arms = [j['arm'] for j in sorted((j for j in plan['jobs'] if j['case'] == case['key'] and j['block'] == b),
                                                 key=lambda j: j['position'])]
                self.assertEqual(arms, orders[b])
        limit = [c for c in plan['cases'] if c['key'] == 'split-limit1-reverse'][0]
        self.assertEqual((limit['placement_cell'], limit['chunks']), ('limit1-reverse', 2))
        self.assertTrue(set(c['placement_cell'] for c in plan['cases']) <= set(pr.PRESETS['benchfetch']))
        with contextlib.redirect_stdout(io.StringIO()):
            seal.main(common + ['--output-dir', str(self.tmp / 'tb2'), '--allow-new-harness', 'rows100',
                                '--allow-new-harness', 'M2sql'])
        manifest = json.loads((self.tmp / 'tb2' / 'manifest.json').read_text())
        self.assertEqual({x['phase']: x['cases'] for x in manifest['phases']}, {'M2rl': 16, 'M2sql': 3})
        self.assertEqual(manifest['unsealed'], [])

    def stub_scripts(self):
        """A stand-in for the frozen scripts, with the same interfaces."""
        d = self.tmp / 'stub-scripts'
        d.mkdir()
        (d / 'matrix_plan.py').write_text(textwrap.dedent('''
            import hashlib
            def make_plan(args):
                arms = []
                for a in args.arm:
                    name, rest = a.split('=', 1)
                    binary, revision = rest.rsplit('@', 1)
                    arms.append(dict(name=name, binary=binary, source_revision=revision, command=[]))
                return dict(campaign='mx-' + args.campaign_id, seed=args.seed, blocks=args.blocks, arms=arms,
                            workloads=[dict(name=w, iterations=args.iterations, warmup=args.warmup, workers=1)
                                       for w in args.workload],
                            campaign_wall_cap_seconds=args.campaign_wall_cap_seconds,
                            timeout_seconds=args.timeout_seconds,
                            cpu=[args.housekeeping_cpus, args.server_cpus, args.client_cpus])
        '''))
        (d / 'run.py').write_text(textwrap.dedent('''
            import itertools, math, random
            def make_schedule(plan, out):
                n = len(plan['arms'])
                rng = random.Random(plan['seed'])
                orders = list(itertools.permutations(range(n))) * (plan['blocks'] // math.factorial(n))
                rng.shuffle(orders)
                return [dict(block=b, position=p, arm=plan['arms'][i]['name'])
                        for b in range(plan['blocks']) for p, i in enumerate(orders[b])]
        '''))
        return d

    def client_args(self, scripts, out, *extra):
        p, o = self.fixture_files()
        return ['client-plans', '--protocol', str(p), '--orders', str(o), '--scripts-dir', str(scripts),
                '--cluster-file', str(self.tmp / 'cluster'), '--native-library', str(self.tmp / 'native'),
                '--server-binary', str(self.tmp / 'server'), '--harness-archive', str(self.tmp / 'harness.tar.gz'),
                '--binary', f'final={self.tmp / "final"}', '--binary', f'cgo={self.tmp / "cgo"}',
                '--results-dir', str(self.tmp / 'results'), '--output-dir', str(out), *extra]

    def test_client_plans_with_stub_scripts(self):
        scripts = self.stub_scripts()
        with contextlib.redirect_stdout(io.StringIO()):
            seal.main(self.client_args(scripts, self.tmp / 'c1'))
        manifest = json.loads((self.tmp / 'c1' / 'manifest.json').read_text())
        self.assertEqual([(c['phase'], c['workload'], c['workers']) for c in manifest['campaigns']],
                         [('M1w1', 'get100', 1), ('M1w64', 'get100', 64)])
        self.assertEqual({(u['phase'], u['workload']) for u in manifest['unsealed']},
                         {('M1w1', 'range1000'), ('M1w64', 'range1000')})
        plan = json.loads((self.tmp / 'c1' / 'M1w64' / 'get100' / 'plan.json').read_text())
        self.assertEqual(plan['workloads'], [dict(name='get100', iterations=32000, warmup=1600, workers=64)])
        self.assertEqual(plan['cpu'], ['0-1', '2-7', '8-15'])
        self.assertEqual([a['source_revision'] for a in plan['arms']], ['b' * 40, 'b' * 40])

    def test_client_plans_reject_bad_cap_and_tampered_orders(self):
        scripts = self.stub_scripts()
        argv = self.client_args(scripts, self.tmp / 'c2')
        protocol = mini_protocol()
        protocol['phases'][0]['per_workload_campaign_wall_cap_seconds'] = 1200
        Path(argv[argv.index('--protocol') + 1]).write_text(json.dumps(protocol))
        Path(argv[argv.index('--orders') + 1]).write_text(json.dumps(seal.make_orders(protocol)))
        with self.assertRaisesRegex(ValueError, '900'):
            seal.main(argv)
        argv = self.client_args(scripts, self.tmp / 'c3')
        tampered = seal.make_orders(mini_protocol())
        tampered['M1w1']['orders'][0].reverse()
        Path(argv[argv.index('--orders') + 1]).write_text(json.dumps(tampered))
        with self.assertRaisesRegex(ValueError, 'reseal'):
            seal.main(argv)

    REAL_SCRIPTS = HERE.parent.parent / 'clientbench' / 'scripts'

    @unittest.skipUnless((REAL_SCRIPTS / 'matrix_plan.py').is_file() and (REAL_SCRIPTS / 'run.py').is_file(),
                         'frozen clientbench scripts not present (Bazel sandbox)')
    def test_client_plans_with_frozen_scripts(self):
        with contextlib.redirect_stdout(io.StringIO()):
            seal.main(self.client_args(self.REAL_SCRIPTS, self.tmp / 'real', '--allow-new-harness', 'range1000'))
        manifest = json.loads((self.tmp / 'real' / 'manifest.json').read_text())
        self.assertEqual(len(manifest['campaigns']), 4)
        plan = json.loads((self.tmp / 'real' / 'M1w64' / 'range1000' / 'plan.json').read_text())
        self.assertEqual(plan['workloads'][0]['workers'], 64)
        self.assertEqual(plan['campaign_wall_cap_seconds'], 900)
        self.assertEqual([g['cpus'] for g in plan['cpu_sets']], [[0, 1], [2, 3, 4, 5, 6, 7], list(range(8, 16))])
        self.assertEqual(plan['arms'][0]['command'][:3], ['taskset', '--cpu-list', '8-15'])


if __name__ == '__main__':
    unittest.main(verbosity=1)
