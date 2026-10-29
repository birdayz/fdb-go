#!/usr/bin/env python3
"""Fake-runner tests: no cloud, SSH, keygen, build, or subprocess effects."""
import copy
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import shlex
import tempfile
import time
import unittest
from unittest import mock

_SPEC = importlib.util.spec_from_file_location('horizontal_provision', Path(__file__).with_name('provision.py'))
p = importlib.util.module_from_spec(_SPEC)
_SPEC.loader.exec_module(p)


class FakeCloud:
    def __init__(self, plan, out):
        self.plan, self.out = plan, out
        self.items = {kind: [dict(id=i+1, name='protected-' + kind, labels={})]
                      for i, kind in enumerate(p.KINDS)}
        self.items['network'][0]['ip_range'] = '10.88.0.0/24'
        self.baseline = copy.deepcopy(self.items)
        self.next_id = 100
        self.events, self.calls, self.deleted = [], [], []
        self.fail_after_create = None
        self.bad_auto_delete = False
        self.auto_delete_update_effect = True
        self.primary_ip_updates = []
        self.ipv6_descriptor = dict(id=0, ip='', blocked=False, dns_ptr=[])
        self.low_disk = False
        self.create_counts = {}
        self.guard_ready = True
        self.guard_dead = False

    def ident(self):
        self.next_id += 1
        return self.next_id

    def popen(self, argv, **kwargs):
        self.guard_kwargs = kwargs
        self.guard_argv = list(argv)
        self.events.append('guard-start')
        plan_path = Path(argv[argv.index('--guard-plan')+1])
        plan = json.loads(plan_path.read_text())
        if self.guard_ready:
            p._json(p._state(self.out) / 'guard-ready.json',
                    dict(pid=9999, plan_sha256=p._sha(plan_path), cleanup_epoch=plan['deadline_epoch']-600))
            self.events.append('guard-ready')
        cloud = self

        class Child:
            pid = 9999

            def poll(self):
                return 1 if cloud.guard_dead else None

        return Child()

    def __call__(self, argv, timeout=120):
        self.calls.append((list(argv), timeout))
        if timeout <= 0:
            raise AssertionError('nonpositive command timeout')
        exe = Path(argv[0]).name
        if exe == 'ssh-keygen':
            self.events.append('keygen')
            path = Path(argv[argv.index('-f')+1])
            path.write_text(p._cloud_init.PRIVATE_BEGIN + '\nfake\n' + p._cloud_init.PRIVATE_END + '\n')
            path.with_name(path.name+'.pub').write_text('ssh-ed25519 QUJD fake\n')
            return 0, '', ''
        if exe == 'ssh':
            if 'initial disk floor' in argv[-1]:
                total = 360*p.GIB
                result = dict(filesystem='ext4', native=True, total_bytes=total,
                              free_bytes=(100 if self.low_disk else 330)*p.GIB,
                              floor_bytes=p.disk_floor(total), fsync_preserved=True, checksums_preserved=True)
                return 0, 'FoundationDB 7.3.77\n'+json.dumps(result)+'\n', ''
            return 0, '', ''
        if exe == 'scp':
            return 0, '', ''
        if argv[0] != self.plan['hcloud']:
            raise AssertionError('unexpected executable: ' + repr(argv))
        kind, verb = argv[1:3]
        if kind == 'server-type':
            return 0, json.dumps(dict(name='ccx43', cpu_type='dedicated',
                    prices=[dict(location='hel1', price_hourly=dict(net='0.3000', gross='0.3570'))])), ''
        if verb == 'list':
            items = copy.deepcopy(self.items[kind])
            if kind == 'server':
                for server in items:
                    gid = (server.get('placement_group') or {}).get('id')
                    groups = [g for g in self.items['placement-group'] if g['id'] == gid]
                    if groups:
                        server['placement_group'] = copy.deepcopy(groups[0])
                        server['placement_group']['servers'] = [s['id'] for s in self.items['server']
                            if (s.get('placement_group') or {}).get('id') == gid]
            return 0, json.dumps(items), ''
        self.events.append(kind+' '+verb)
        if verb == 'create':
            if 'guard-ready' not in self.events:
                raise AssertionError('cloud mutation before guard acknowledgment')
            labels = dict(argv[i+1].split('=', 1) for i, a in enumerate(argv) if a == '--label')
            item = dict(id=self.ident(), name=argv[argv.index('--name')+1], labels=labels)
            if kind == 'network':
                item['ip_range'] = argv[argv.index('--ip-range')+1]
            if kind == 'placement-group':
                item['type'] = argv[argv.index('--type')+1]
            if kind == 'server':
                item['created'] = p._mc.iso(int(time.time()))
                item['server_type'] = dict(name='ccx43', cpu_type='dedicated', cores=16,
                                           memory=64, disk=360, storage_type='local', architecture='x86')
                item['primary_disk_size'] = 360
                item['location'] = dict(id=3, name='hel1', network_zone='eu-central')
                item['datacenter'] = None
                ipid = self.ident()
                ip = '192.0.2.'+str(item['id']-90)
                item.update(public_net=dict(ipv4=dict(id=ipid, ip=ip),
                                            ipv6=copy.deepcopy(self.ipv6_descriptor)), private_net=[],
                            placement_group=dict(id=int(argv[argv.index('--placement-group')+1])))
                self.items['primary-ip'].append(dict(id=ipid, name='', type='ipv4', ip=ip,
                    auto_delete=not self.bad_auto_delete, assignee_id=item['id'], assignee_type='server'))
            self.items[kind].append(item)
            self.create_counts[kind] = self.create_counts.get(kind, 0)+1
            if self.fail_after_create == (kind, self.create_counts[kind]):
                return 1, '', 'simulated response lost after API creation'
            return 0, '', ''
        if kind == 'primary-ip' and verb == 'update':
            rid = int(argv[-1])
            if rid <= 10 or argv[3] != '--auto-delete=true':
                raise AssertionError('IP mutation is not an exact new ID auto-delete enable')
            ip = next(i for i in self.items[kind] if i['id'] == rid)
            if ip.get('assignee_type') != 'server' or ip.get('assignee_id', 0) <= 10:
                raise AssertionError('IP mutation is not bound to a new server')
            self.primary_ip_updates.append(rid)
            if self.auto_delete_update_effect:
                ip['auto_delete'] = True
            return 0, '', ''
        if verb == 'attach-to-network':
            node = next(i for i in self.items['server'] if i['id'] == int(argv[3]))
            node['private_net'] = [dict(network=int(argv[argv.index('--network')+1]),
                                        ip=argv[argv.index('--ip')+1], alias_ips=[])]
            return 0, '', ''
        if verb in ('add-subnet', 'poweron'):
            # Existing resources must never be mutated by name or protected ID.
            if not argv[3].isdigit() or int(argv[3]) <= 10:
                raise AssertionError('mutation is not by exact new ID')
            return 0, '', ''
        if verb == 'delete':
            rid = int(argv[3])
            if rid <= 10:
                raise AssertionError('protected ID deletion')
            self.deleted.append((kind, rid))
            self.items[kind] = [i for i in self.items[kind] if i['id'] != rid]
            if kind == 'server':
                self.items['primary-ip'] = [i for i in self.items['primary-ip']
                    if not (i.get('assignee_id') == rid and i.get('auto_delete') is True)]
            return 0, '', ''
        raise AssertionError('unhandled fake command: ' + repr(argv))


class ProvisionTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(prefix='horizontal-provision-test-')
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.out = self.root / 'campaign'
        self.block = mock.patch.object(p.subprocess, 'run', side_effect=AssertionError('real subprocess forbidden'))
        self.block.start()
        self.addCleanup(self.block.stop)
        now = int(time.time())
        self.plan = dict(schema='otel_horizontal_v1', approved=True, campaign='horizontal-test',
                         prefix='horizontal-test', location='hel1', server_type='ccx43',
                         controller_cidr='198.51.100.1/32', first_epoch=now-1, deadline_epoch=now+7199)
        for key in ('hcloud', 'binary', 'fdbserver', 'fdbcli', 'source_manifest', 'controller_private_key'):
            path = self.root / key
            path.write_text('sealed '+key+'\n')
            path.chmod(0o700 if key == 'hcloud' else 0o600)
            self.plan[key] = str(path)
            if key in ('binary', 'fdbserver', 'fdbcli'):
                self.plan[key+'_sha256'] = hashlib.sha256(path.read_bytes()).hexdigest()
        public = self.root / 'controller.pub'
        public.write_text('ssh-ed25519 QUJD test\n')
        self.plan['controller_public_key'] = str(public)
        self.plan['input_manifest_sha256'] = p._sha(self.plan['source_manifest'])
        self.cloud = FakeCloud(self.plan, self.out)

    def prepare(self):
        return p.prepare(self.plan, self.out, runner=self.cloud, popen=self.cloud.popen,
                         sleep=lambda seconds: None)

    def test_prepare_exact_five_spread_native_hosts_and_pinned_transports(self):
        resources = self.prepare()
        self.assertTrue(resources['prepared'])
        self.assertEqual(set(resources['nodes']), set(p.ROLES))
        self.assertEqual(len(resources['ids']['server']), 5)
        self.assertEqual(len(resources['primary_ip_proof']), 5)
        self.assertEqual(len(resources['ids']['placement-group']), 1)
        self.assertNotIn('start_new_session', self.cloud.guard_kwargs)
        self.assertEqual(self.cloud.guard_argv[:2], [p.sys.executable, '-B'])
        self.assertEqual(resources['price_hourly_net'], '0.3000')
        self.assertEqual(resources['price_hourly_gross'], '0.3570')
        self.assertEqual(p._sha(resources['pricing_receipt']), resources['pricing_receipt_sha256'])
        starts = [shlex.split(a[-1]) for a, _ in self.cloud.calls if Path(a[0]).name == 'ssh'
                  and shlex.split(a[-1])[:2] == ['systemctl', 'start']]
        self.assertEqual(sum(s[2:] == ['horizontal-client.slice'] for s in starts), 2)
        self.assertEqual(sum(s[2:] == ['horizontal-fdb.slice'] for s in starts), 3)
        self.assertLess(self.cloud.events.index('guard-ready'), self.cloud.events.index('network create'))
        self.assertLess(self.cloud.events.index('guard-ready'), self.cloud.events.index('keygen'))
        self.assertLessEqual(resources['first_create_epoch'], resources['cleanup_epoch'])
        self.assertEqual(resources['cleanup_epoch'], self.plan['deadline_epoch']-600)
        self.assertEqual(len(p._load(p._state(self.out) / 'placement-proof.json')['server_ids']), 5)
        creates = [a for a, _ in self.cloud.calls if len(a)>2 and a[1:3] == ['server', 'create']]
        self.assertEqual(len(creates), 5)
        for argv in creates:
            self.assertIn('--without-ipv6', argv)
            self.assertIn('--start-after-create=false', argv)
            self.assertEqual(argv[argv.index('--type')+1], 'ccx43')
            self.assertEqual(argv[argv.index('--location')+1], 'hel1')
        for node in resources['nodes'].values():
            argv = p.ssh_argv(self.plan, self.out, node, ['printf', '%s', 'a; touch /not-executed'])
            self.assertIn('StrictHostKeyChecking=yes', argv)
            self.assertIn('HostKeyAlias='+node['name'], argv)
            self.assertIn('UserKnownHostsFile='+node['known_hosts'], argv)
            self.assertEqual(shlex.split(argv[-1]), ['printf', '%s', 'a; touch /not-executed'])
            self.assertEqual(os.stat(node['known_hosts']).st_mode & 0o777, 0o600)
            self.assertIsInstance(node['created'], str)
        pricing = p._load(resources['pricing_receipt'])
        self.assertTrue(pricing['read_only'])
        self.assertEqual(pricing['server_type']['prices'][0]['location'], 'hel1')

    def test_failed_guard_acknowledgment_means_no_cloud_mutations(self):
        self.cloud.guard_ready, self.cloud.guard_dead = False, True
        with self.assertRaisesRegex(p.ProvisionError, 'before acknowledgment'):
            self.prepare()
        self.assertFalse(self.cloud.create_counts)
        self.assertFalse(self.cloud.deleted)

    def test_guard_ack_must_match_exact_plan_hash(self):
        original = self.cloud.popen

        def corrupt(argv, **kwargs):
            child = original(argv, **kwargs)
            ready_path = p._state(self.out) / 'guard-ready.json'
            ready = p._load(ready_path)
            ready['plan_sha256'] = '0'*64
            p._json(ready_path, ready, replace=True)
            return child

        with self.assertRaisesRegex(p.ProvisionError, 'does not match'):
            p.prepare(self.plan, self.out, runner=self.cloud, popen=corrupt)
        self.assertFalse(self.cloud.create_counts)

    def test_invalid_lifetime_and_asset_hash_are_local_refusals(self):
        self.plan['deadline_epoch'] += 1
        with self.assertRaisesRegex(p.ProvisionError, 'lifetime'):
            self.prepare()
        self.plan['deadline_epoch'] -= 1
        self.plan['binary_sha256'] = '0'*64
        with self.assertRaisesRegex(p.ProvisionError, 'SHA-256'):
            self.prepare()
        self.assertFalse(self.cloud.calls)

    def test_ambiguous_partial_create_is_cleaned_by_exact_new_id(self):
        self.cloud.fail_after_create = ('server', 3)
        with self.assertRaisesRegex(p.ProvisionError, 'response lost'):
            self.prepare()
        self.assertEqual(self.cloud.items, self.cloud.baseline)
        receipt = p._load(p._state(self.out) / 'teardown-complete.json')
        self.assertTrue(receipt['passed'])
        self.assertEqual(len(receipt['inventory']['kinds']), 10)
        self.assertTrue(any(kind == 'placement-group' for kind, _ in self.cloud.deleted))
        self.assertEqual(sum(kind == 'server' for kind, _ in self.cloud.deleted), 3)
        self.assertFalse(any(kind == 'primary-ip' for kind, _ in self.cloud.deleted))

    def test_provider_locations_accept_sdk_and_legacy_but_never_disagree(self):
        for server in (dict(location=dict(name='hel1')),
                       dict(location=dict(name='hel1'), datacenter=None),
                       dict(datacenter=dict(location=dict(name='hel1'))),
                       dict(location=dict(name='hel1'), datacenter=dict(location=dict(name='hel1')))):
            self.assertEqual(p._provider_location(server), 'hel1')
        bad = [{}, dict(location=None), dict(location={}), dict(location='hel1'),
               dict(location=dict(name='fsn1')), dict(datacenter={}),
               dict(location=dict(name='hel1'), datacenter=dict(location=dict(name='fsn1'))),
               dict(location=dict(name='fsn1'), datacenter=dict(location=dict(name='hel1'))),
               dict(location=dict(name='hel1'), datacenter=dict(location=None))]
        for server in bad:
            with self.subTest(server=server), self.assertRaises(p.ProvisionError):
                p._provider_location(server)

    def test_final_sdk_provider_shape_and_wrong_missing_membership_fail_closed(self):
        resources = self.prepare()
        snapshots = resources['primary_ip_snapshot_receipts']
        final = p._load(snapshots[-1])
        servers = json.loads(final['raw']['server']['stdout'])
        node = resources['nodes']['node1']
        server = next(s for s in servers if s['id'] == node['id'])
        net, group = resources['ids']['network'][0], resources['ids']['placement-group'][0]
        p._verify_provider_server(self.plan, resources, node, server, net, group)
        self.assertIsNone(server['datacenter'])
        self.assertEqual(server['location']['name'], 'hel1')
        cases = [('server_type', 'name', 'ccx33'), ('server_type', 'cpu_type', 'shared'),
                 ('server_type', 'cores', 8), ('server_type', 'cores', True),
                 ('server_type', 'memory', 32), ('server_type', 'disk', 180),
                 ('server_type', 'storage_type', 'network'), ('server_type', 'architecture', 'arm'),
                 ('placement_group', 'type', 'unknown'), ('placement_group', 'id', 1)]
        for section, field, wrong in cases:
            for missing in (False, True):
                changed = copy.deepcopy(server)
                if missing:
                    del changed[section][field]
                else:
                    changed[section][field] = wrong
                with self.subTest(section=section, field=field, missing=missing), self.assertRaises(p.ProvisionError):
                    p._verify_provider_server(self.plan, resources, node, changed, net, group)
        for field, wrong in [('location', {}), ('primary_disk_size', 180), ('private_net', []),
                             ('name', 'other-host'), ('labels', {})]:
            changed = copy.deepcopy(server)
            changed[field] = wrong
            with self.subTest(field=field), self.assertRaises(p.ProvisionError):
                p._verify_provider_server(self.plan, resources, node, changed, net, group)
        changed = copy.deepcopy(server)
        changed['private_net'][0]['network'] = 2
        with self.assertRaises(p.ProvisionError):
            p._verify_provider_server(self.plan, resources, node, changed, net, group)

    def test_exact_disabled_ipv6_shape_and_missing_null_are_absent(self):
        resources = self.prepare()
        servers = copy.deepcopy(self.cloud.items['server'])
        ips = copy.deepcopy(self.cloud.items['primary-ip'])
        server = next(s for s in servers if s['id'] == resources['nodes']['node1']['id'])
        self.assertEqual(server['public_net']['ipv6'], dict(id=0, ip='', blocked=False, dns_ptr=[]))
        for value in (None, dict(id=0, ip='', blocked=False, dns_ptr=[])):
            with self.subTest(value=value):
                server['public_net']['ipv6'] = value
                self.assertTrue(p._ipv6_absent(value))
                self.assertEqual(p._verified_ipv4(self.plan, resources, servers, ips, server)['type'], 'ipv4')
        del server['public_net']['ipv6']
        self.assertEqual(p._verified_ipv4(self.plan, resources, servers, ips, server)['type'], 'ipv4')

    def test_populated_or_malformed_ipv6_descriptor_fails_closed(self):
        resources = self.prepare()
        servers = copy.deepcopy(self.cloud.items['server'])
        ips = copy.deepcopy(self.cloud.items['primary-ip'])
        server = next(s for s in servers if s['id'] == resources['nodes']['node1']['id'])
        bad = [dict(id=123, ip='2001:db8::/64', blocked=False, dns_ptr=[]),
               dict(id=0, ip='2001:db8::/64', blocked=False, dns_ptr=[]),
               dict(id=True, ip='', blocked=False, dns_ptr=[]),
               dict(id=0, ip='', blocked=0, dns_ptr=[]),
               dict(id=0, ip='', blocked=True, dns_ptr=[]),
               dict(id=0, ip='', blocked=False, dns_ptr=['unexpected']),
               dict(id=0, ip='', blocked=False, dns_ptr=[], unknown=False),
               dict(id=0, ip='', blocked=False), {}, [], False, 0, '']
        for value in bad:
            with self.subTest(value=value):
                server['public_net']['ipv6'] = value
                self.assertFalse(p._ipv6_absent(value))
                with self.assertRaisesRegex(p.ProvisionError, 'exact new IPv4-only'):
                    p._verified_ipv4(self.plan, resources, servers, ips, server)
        self.assertFalse(self.cloud.primary_ip_updates)

    def test_raw_ip_snapshots_survive_ipv6_validation_failure(self):
        self.cloud.ipv6_descriptor = dict(id=0, ip='', blocked=False, dns_ptr=[], unknown=False)
        with self.assertRaisesRegex(p.ProvisionError, 'exact new IPv4-only'):
            self.prepare()
        state = p._state(self.out)
        snapshots = sorted(state.glob('ip-snapshot-*.json'))
        self.assertEqual(len(snapshots), 1)
        receipt = p._load(snapshots[0])
        self.assertTrue(receipt['read_only'])
        self.assertTrue(receipt['before_validation'])
        self.assertEqual(set(receipt['raw']), {'server', 'primary-ip'})
        raw_servers = json.loads(receipt['raw']['server']['stdout'])
        created = [s for s in raw_servers if s['id'] > 10]
        self.assertEqual(len(created), 1)
        self.assertEqual(created[0]['public_net']['ipv6'], self.cloud.ipv6_descriptor)
        self.assertTrue(json.loads(receipt['raw']['primary-ip']['stdout']))
        self.assertEqual(os.stat(snapshots[0]).st_mode & 0o777, 0o600)
        self.assertEqual(self.cloud.items, self.cloud.baseline)
        self.assertFalse(self.cloud.primary_ip_updates)

    def test_auxiliary_guard_disables_bytecode_and_fake_fixture_has_no_cache_writes(self):
        # Only inspect this test's temporary fixture, never mutable source trees.
        fixture = self.root / 'input-fixture'
        fixture.mkdir()
        source = fixture / 'module.py'
        source.write_text('VALUE = 1' + chr(10))
        before = source.read_bytes()
        self.prepare()
        self.assertEqual(self.cloud.guard_argv[0], p.sys.executable)
        self.assertEqual(self.cloud.guard_argv[1], '-B')
        self.assertEqual(source.read_bytes(), before)
        self.assertFalse(list(fixture.rglob('__pycache__')))
        self.assertFalse(list(fixture.rglob('*.pyc')))

    def test_raw_ip_capture_uses_active_budget_but_cleanup_inventory_does_not(self):
        state = self.root / 'capture-state'
        state.mkdir()
        resources = {}
        clock = lambda: self.plan['first_epoch'] + 5999.75
        p._capture_ip_snapshots(self.plan, state, resources, self.cloud, clock)
        self.assertEqual([timeout for _, timeout in self.cloud.calls], [.25, .25])
        argv = [self.plan['hcloud'], 'server', 'list', '-o', 'json']
        before = len(self.cloud.calls)
        refused = p._inventory_run(self.plan, self.cloud, argv,
                                   lambda: self.plan['first_epoch']+6000, active=True)
        self.assertNotEqual(refused[0], 0)
        self.assertEqual(len(self.cloud.calls), before)
        cleanup = p._inventory_run(self.plan, self.cloud, argv,
                                   lambda: self.plan['first_epoch']+6600)
        self.assertEqual(cleanup[0], 0)
        self.assertEqual(self.cloud.calls[-1][1], 10)

    def test_auto_delete_false_is_repaired_and_read_back_before_proceeding(self):
        self.cloud.bad_auto_delete = True
        resources = self.prepare()
        self.assertEqual(len(self.cloud.primary_ip_updates), 5)
        self.assertEqual(set(self.cloud.primary_ip_updates), set(resources['ids']['primary-ip']))
        repairs = resources['primary_ip_repairs']
        self.assertEqual(len(repairs), 5)
        self.assertTrue(all(r['before'] is False and r['requested'] is True and r['verified'] is True
                            and r['verified_epoch'] >= r['finished_epoch'] >= r['epoch'] for r in repairs))
        commands = [json.loads(line)['argv'] for line in
                    (p._state(self.out) / 'api-commands.jsonl').read_text().splitlines()]
        updates = [a for a in commands if a[1:3] == ['primary-ip', 'update']]
        self.assertEqual(len(updates), 5)
        self.assertTrue(all(a[3] == '--auto-delete=true' and int(a[-1]) > 10 for a in updates))
        result = p.teardown(self.plan, self.out, resources, runner=self.cloud, sleep=lambda _: None)
        self.assertTrue(result['passed'])
        self.assertEqual(self.cloud.items, self.cloud.baseline)
        self.assertFalse(any(kind == 'primary-ip' for kind, _ in self.cloud.deleted))

    def test_auto_delete_repair_requires_true_readback_before_next_create(self):
        self.cloud.bad_auto_delete = True
        self.cloud.auto_delete_update_effect = False
        with self.assertRaisesRegex(p.ProvisionError, 'cleanup not verified'):
            self.prepare()
        self.assertEqual(self.cloud.create_counts['server'], 1)
        self.assertEqual(len(self.cloud.primary_ip_updates), 1)
        self.assertFalse(p._load(p._state(self.out) / 'resources.json')['primary_ip_repairs'][0]['verified'])
        self.assertFalse((p._state(self.out) / 'teardown-complete.json').exists())
        self.assertTrue((p._state(self.out) / 'guard-ready.json').exists())
        self.assertFalse(any(kind == 'primary-ip' for kind, _ in self.cloud.deleted))

    def test_protected_ipv4_is_never_updated_even_when_bound_to_new_server(self):
        resources = self.prepare()
        server = next(s for s in self.cloud.items['server'] if s['id'] == resources['nodes']['node1']['id'])
        original_ip = next(i for i in self.cloud.items['primary-ip']
                           if i['id'] == server['public_net']['ipv4']['id'])
        original_ip.update(assignee_type=None, assignee_id=None)
        protected = self.cloud.items['primary-ip'][0]
        protected.update(type='ipv4', ip='198.51.100.7', auto_delete=False,
                         assignee_type='server', assignee_id=server['id'])
        server['public_net']['ipv4'] = dict(id=protected['id'], ip=protected['ip'])
        before = copy.deepcopy(protected)
        with self.assertRaisesRegex(p.ProvisionError, 'exact new IPv4-only'):
            p._prove_ips(self.plan, p._state(self.out), resources, self.cloud)
        self.assertEqual(protected, before)
        self.assertFalse(self.cloud.primary_ip_updates)

    def test_ipv4_referenced_by_another_server_is_never_updated(self):
        resources = self.prepare()
        server = next(s for s in self.cloud.items['server'] if s['id'] == resources['nodes']['node1']['id'])
        ipid = server['public_net']['ipv4']['id']
        ip = next(i for i in self.cloud.items['primary-ip'] if i['id'] == ipid)
        ip['auto_delete'] = False
        self.cloud.items['server'][0]['public_net'] = copy.deepcopy(server['public_net'])
        with self.assertRaisesRegex(p.ProvisionError, 'exact new IPv4-only'):
            p._prove_ips(self.plan, p._state(self.out), resources, self.cloud)
        self.assertFalse(self.cloud.primary_ip_updates)
        self.assertFalse(ip['auto_delete'])

    def test_initial_disk_floor_failure_tears_down_all_five(self):
        self.cloud.low_disk = True
        with self.assertRaisesRegex(p.ProvisionError, 'disk/fsync/checksum'):
            self.prepare()
        self.assertEqual(self.cloud.items, self.cloud.baseline)
        self.assertEqual(sum(kind == 'server' for kind, _ in self.cloud.deleted), 5)
        self.assertEqual(p.disk_floor(1), 264*p.GIB+1)
        self.assertEqual(p.disk_floor(100), 264*p.GIB+6)

    def test_teardown_immediate_and_all_ten_exact_baseline_sets(self):
        resources = self.prepare()
        result = p.teardown(self.plan, self.out, resources, runner=self.cloud, sleep=lambda _: None)
        self.assertTrue(result['passed'])
        self.assertTrue(result['within_deadline'])
        self.assertTrue(Path(result['receipt_path']).is_file())
        self.assertTrue(all(a['finished_epoch'] >= a['epoch'] for a in result['actions']))
        self.assertEqual(set(result['inventory']['kinds']), set(p.KINDS))
        self.assertEqual(self.cloud.items, self.cloud.baseline)
        again = p.teardown(self.plan, self.out, resources, runner=self.cloud, sleep=lambda _: None)
        self.assertTrue(again['passed'])

    def test_protected_missing_id_and_unowned_resource_never_pass_verification(self):
        resources = self.prepare()
        missing = self.cloud.items['certificate'].pop()
        rogue = dict(id=99999, name=self.plan['prefix']+'-rogue', labels=dict(campaign=self.plan['campaign']))
        self.cloud.items['placement-group'].append(rogue)
        result = p.teardown(self.plan, self.out, resources, runner=self.cloud, sleep=lambda _: None)
        self.assertFalse(result['passed'])
        self.assertEqual(len(result['inventory']['kinds']), 10)
        self.assertEqual(result['inventory']['kinds']['certificate']['missing'], [missing['id']])
        self.assertTrue(result['refused'])
        self.assertNotIn(('placement-group', rogue['id']), self.cloud.deleted)
        self.assertFalse((p._state(self.out) / 'teardown-complete.json').exists())

    def test_no_machine_outside_exact_journal_can_be_accessed(self):
        resources = self.prepare()
        node = dict(resources['nodes']['node1'], host='198.51.100.55')
        before = len(self.cloud.calls)
        with self.assertRaisesRegex(p.ProvisionError, 'exact campaign-owned'):
            p.run_on_node(self.plan, self.out, node, ['true'], runner=self.cloud)
        with self.assertRaisesRegex(p.ProvisionError, 'exact campaign-owned'):
            p.copy_to_node(self.plan, self.out, node, self.plan['binary'], '/tmp/binary', runner=self.cloud)
        self.assertEqual(before, len(self.cloud.calls))

    def test_native_r1_layout_and_all_three_arm_transitions(self):
        resources = self.prepare()
        nodes = resources['nodes']
        last_controls = None
        for count in (1, 2, 3):
            layout = p.setup_cluster(self.plan, self.out, nodes, count, 'arm'+str(count), runner=self.cloud)
            self.assertEqual(layout['storage_hosts'], list(p.ROLES[:count]))
            self.assertEqual(len(layout['processes']), 5+count)
            controls = [(x['node'], x['address'], x['process_class']) for x in layout['processes']
                        if x['process_class'] != 'storage']
            if last_controls is not None:
                self.assertEqual(controls, last_controls)
            last_controls = controls
            self.assertEqual(layout['configure'], p.CONFIGURE)
            self.assertEqual(layout['storage_engine'], 'ssd-redwood-1')
            for process in layout['processes']:
                self.assertEqual(process['node'], process['machineid'])
                self.assertEqual(process['node'], process['zoneid'])
                self.assertTrue(process['required_roles'])
                self.assertEqual(process['memory_limit_bytes'], (4 if process['process_class']=='storage' else 1)*p.GIB)
                self.assertIn('Slice=horizontal-fdb.slice', process['unit'])
                self.assertIn('Restart=no', process['unit'])
                self.assertFalse(any('knob' in value for value in process['args']))
                self.assertNotIn('Docker', process['unit'])
            for node in p.ROLES[:3]:
                per_host = [x for x in layout['processes'] if x['node']==node]
                self.assertLessEqual(sum(x['memory_limit_bytes'] for x in per_host), 16*p.GIB)
            self.assertTrue(Path(layout['cluster_file']).is_file())
        stops = [shlex.split(argv[-1]) for argv, _ in self.cloud.calls if Path(argv[0]).name == 'ssh'
                 and shlex.split(argv[-1])[:2] == ['systemctl', 'stop']]
        self.assertTrue(stops)
        self.assertTrue(all(unit.startswith('oh-'+self.plan['prefix']+'-') for stop in stops for unit in stop[2:]))
        self.assertIn('MemoryMax=16G', p._slice('fdb'))
        self.assertIn('CPUQuota=400%', p._slice('client'))
        with self.assertRaises(FileExistsError):
            p.setup_cluster(self.plan, self.out, nodes, 3, 'arm3', runner=self.cloud)

    def test_guard_fires_at_6600_and_never_waits_for_controller(self):
        resources = self.prepare()
        state = p._state(self.out)
        (state / 'guard-ready.json').unlink()
        now = [self.plan['first_epoch']+6599]

        def sleep(seconds):
            now[0] += seconds

        rc = p._guard(state / 'guard-plan.json', self.out, runner=self.cloud,
                      clock=lambda: now[0], sleep=sleep)
        self.assertEqual(rc, 0)
        self.assertEqual(p._load(state / 'cleanup-started.json')['epoch'], self.plan['first_epoch']+6600)
        self.assertEqual(self.cloud.items, self.cloud.baseline)
        self.assertTrue(p._load(state / 'teardown-complete.json')['passed'])
        self.assertEqual(p._load(state / 'guard-ready.json')['cleanup_epoch'], self.plan['deadline_epoch']-600)

    def test_timeout_budget_clamps_to_absolute_deadline_and_cleanup_window(self):
        deadline = self.plan['deadline_epoch']
        self.assertEqual(p._remaining(self.plan, 120, lambda: deadline-.25), .25)
        self.assertEqual(p._remaining(self.plan, 120, lambda: self.plan['first_epoch']+5999.5, active=True), .5)
        with self.assertRaisesRegex(p.ProvisionError, 'deadline'):
            p._remaining(self.plan, 120, lambda: deadline, active=False)
        with self.assertRaisesRegex(p.ProvisionError, 'cleanup'):
            p._check_window(self.plan, self.root, lambda: self.plan['first_epoch']+6000)

    def test_existing_prefix_is_never_adopted_or_touched(self):
        self.cloud.items['server'][0]['name'] = self.plan['prefix']+'-node1'
        with self.assertRaisesRegex(p.ProvisionError, 'adopt'):
            self.prepare()
        self.assertFalse(self.cloud.create_counts)
        self.assertFalse(self.cloud.deleted)


if __name__ == '__main__':
    unittest.main()
