#!/usr/bin/env python3
"""Fake-runner tests: no cloud, SSH, keygen, build, or subprocess effects."""
import copy
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import re
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
        self.creation_delays = {}
        self.creation_exceptions = {}
        self.creation_outputs = {}
        self.creation_clock = None
        self.creation_invocations = []
        self.guard_ready = True
        self.guard_dead = False
        self.readiness_results = []
        self.readiness_calls = []
        self.attach_post_status = 'success'
        self.attach_poll_statuses = []
        self.attach_actions = {}
        self.attach_api_calls = []
        self.attach_post_exception = None
        self.attach_action_mutator = None
        self.attach_server_mutator = None
        self.attach_call_hook = None

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

    def _server_view(self, server):
        value = copy.deepcopy(server)
        gid = (value.get('placement_group') or {}).get('id')
        groups = [g for g in self.items['placement-group'] if g['id'] == gid]
        if groups:
            value['placement_group'] = copy.deepcopy(groups[0])
            value['placement_group']['servers'] = [s['id'] for s in self.items['server']
                if (s.get('placement_group') or {}).get('id') == gid]
        return value

    def _attach_api(self, argv, timeout):
        path = argv[2]
        method = argv[argv.index('--method')+1] if '--method' in argv else 'GET'
        self.attach_api_calls.append(dict(path=path, method=method, timeout=timeout, argv=list(argv)))
        if self.attach_call_hook:
            self.attach_call_hook(path, method, timeout)
        post = re.fullmatch(r'/servers/(\d+)/actions/attach_to_network', path)
        poll = re.fullmatch(r'/networks/actions/(\d+)', path)
        describe = re.fullmatch(r'/servers/(\d+)', path)
        if post:
            if method != 'POST' or 'guard-ready' not in self.events:
                raise AssertionError('attach POST method/guard mismatch')
            sid = int(post[1])
            data = json.loads(argv[argv.index('--data')+1])
            aid = self.ident()
            pending = dict(sid=sid, network=data['network'], ip=data['ip'], status=self.attach_post_status)
            self.attach_actions[aid] = pending
            phase = 'post'
            if self.attach_post_exception:
                raise self.attach_post_exception
        elif poll:
            if method != 'GET':
                raise AssertionError('action poll must be GET')
            aid = int(poll[1])
            pending = self.attach_actions[aid]
            sid = pending['sid']
            if self.attach_poll_statuses:
                pending['status'] = self.attach_poll_statuses.pop(0)
            phase = 'poll'
        elif describe:
            if method != 'GET':
                raise AssertionError('server readback must be GET')
            sid = int(describe[1])
            server = self._server_view(next(s for s in self.items['server'] if s['id'] == sid))
            if self.attach_server_mutator:
                self.attach_server_mutator(server)
            return 0, json.dumps(dict(server=server)), ''
        else:
            raise AssertionError('unexpected API path: '+path)
        action = dict(id=aid, command='attach_to_network', status=pending['status'], error=None,
                      started='2026-10-29T09:30:32Z',
                      finished='2026-10-29T09:30:51Z' if pending['status'] == 'success' else None,
                      resources=[dict(type='server', id=sid), dict(type='network', id=pending['network'])])
        if pending['status'] == 'error':
            action['error'] = dict(code='attach_failed', message='simulated attach failure')
        if pending['status'] == 'success':
            server = next(s for s in self.items['server'] if s['id'] == sid)
            server['private_net'] = [dict(network=pending['network'], ip=pending['ip'], alias_ips=[])]
        if self.attach_action_mutator:
            self.attach_action_mutator(action, phase)
        return 0, json.dumps(dict(action=action)), ''

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
            if shlex.split(argv[-1]) == ['cloud-init', 'status', '--wait']:
                self.readiness_calls.append(list(argv))
                result = self.readiness_results.pop(0) if self.readiness_results else (0, 'status: done\n', '')
                if isinstance(result, BaseException):
                    raise result
                return result
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
        if kind == 'api':
            return self._attach_api(argv, timeout)
        if kind == 'server-type':
            return 0, json.dumps(dict(name='ccx43', cpu_type='dedicated',
                    prices=[dict(location='hel1', price_hourly=dict(net='0.3000', gross='0.3570'))])), ''
        if verb == 'list':
            items = copy.deepcopy(self.items[kind])
            if kind == 'server':
                items = [self._server_view(server) for server in items]
            return 0, json.dumps(items), ''
        self.events.append(kind+' '+verb)
        if verb == 'create':
            ordinal = self.create_counts.get(kind, 0)+1
            self.creation_invocations.append(dict(kind=kind, ordinal=ordinal, timeout=timeout, argv=list(argv)))
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
            key = (kind, self.create_counts[kind])
            delay = self.creation_delays.get(key, 0)
            if self.creation_clock:
                self.creation_clock(min(delay, timeout))
            if delay > timeout:
                raise p.subprocess.TimeoutExpired(argv, timeout,
                    output='created-resource-pending-output', stderr='compound-create-timeout-stderr')
            if key in self.creation_exceptions:
                raise self.creation_exceptions[key]
            if self.fail_after_create == key:
                return 1, '', 'simulated response lost after API creation'
            stdout, stderr = self.creation_outputs.get(key, ('', ''))
            return 0, stdout, stderr
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

    def diagnostic_plan(self):
        self.plan.update(schema='otel_native_diagnostic_v1', count=100000,
                         probe_count=1000000, probe_seconds=120)
        return self.plan

    def test_schema_roles_are_exact_and_unknown_or_untyped_schemas_fail_before_effects(self):
        self.assertEqual(p._roles(self.plan), ('node1', 'node2', 'node3', 'client1', 'client2'))
        self.assertIs(p._roles(self.plan), p.ROLES)
        self.diagnostic_plan()
        self.assertEqual(p._roles(self.plan), ('node1', 'client1'))
        self.assertEqual(p._names(self.plan)['server'],
                         {self.plan['prefix']+'-node1', self.plan['prefix']+'-client1'})
        for bad in (None, True, 1, [], {}, '', 'otel_native_diagnostic_v2', 'otel_horizontal_v2'):
            with self.subTest(schema=bad):
                self.plan['schema'] = bad
                with self.assertRaises(p.ProvisionError):
                    self.prepare()
        for bad_plan in (None, [], False, 'otel_native_diagnostic_v1'):
            with self.assertRaises(p.ProvisionError):
                p._roles(bad_plan)
        self.assertFalse(self.cloud.calls)
        self.assertFalse(self.cloud.create_counts)

    def test_diagnostic_plan_requires_exact_typed_counts_and_no_comparative_arrays(self):
        self.diagnostic_plan()
        p.validate_plan(self.plan, self.out)
        good = copy.deepcopy(self.plan)
        for key in ('count', 'probe_count', 'probe_seconds'):
            for bad in (None, True, str(good[key]), float(good[key]), good[key]+1):
                self.plan.clear(); self.plan.update(good)
                self.plan[key] = bad
                with self.subTest(key=key, bad=bad), self.assertRaises(p.ProvisionError):
                    self.prepare()
            self.plan.clear(); self.plan.update(good); del self.plan[key]
            with self.assertRaises(p.ProvisionError):
                self.prepare()
        for key in ('topologies', 'measured_topologies', 'prerequisite_topologies', 'selected_topologies', 'measured_count'):
            self.plan.clear(); self.plan.update(good); self.plan[key] = 1000000 if key == 'measured_count' else [1]
            with self.subTest(key=key), self.assertRaises(p.ProvisionError):
                self.prepare()
        self.assertFalse(self.cloud.calls)

    def test_diagnostic_prepare_creates_exact_two_pinned_owned_hosts_and_preserves_caps(self):
        self.diagnostic_plan()
        resources = self.prepare()
        self.assertTrue(resources['prepared'])
        self.assertEqual(set(resources['nodes']), {'node1', 'client1'})
        self.assertEqual(len(resources['ids']['server']), 2)
        self.assertEqual(len(resources['primary_ip_proof']), 2)
        self.assertEqual({role: node['private_ip'] for role, node in resources['nodes'].items()},
                         {'node1': '10.93.0.11', 'client1': '10.93.0.21'})
        created = [argv for argv, _ in self.cloud.calls if argv[1:3] == ['server', 'create']]
        self.assertEqual({a[a.index('--name')+1] for a in created},
                         {self.plan['prefix']+'-node1', self.plan['prefix']+'-client1'})
        self.assertEqual(self.cloud.create_counts['server'], 2)
        self.assertLess(self.cloud.events.index('guard-ready'), self.cloud.events.index('network create'))
        state = p._state(self.out)
        proof = p._load(state/'placement-proof.json')
        self.assertEqual(set(proof['server_ids']), set(resources['ids']['server']))
        self.assertEqual(len(proof['server_ids']), 2)
        snapshots = p._load(resources['primary_ip_snapshot_receipts'][-1])
        owned = [s for s in json.loads(snapshots['raw']['server']['stdout'])
                 if s['id'] in resources['ids']['server']]
        self.assertEqual(len(owned), 2)
        for server in owned:
            self.assertEqual(set(server['placement_group']['servers']), set(resources['ids']['server']))
        for role, node in resources['nodes'].items():
            argv = p.ssh_argv(self.plan, self.out, node, ['true'])
            self.assertIn('StrictHostKeyChecking=yes', argv)
            self.assertIn('HostKeyAlias='+node['name'], argv)
            self.assertIn('UserKnownHostsFile='+node['known_hosts'], argv)
            self.assertEqual(Path(node['known_hosts']).stat().st_mode & 0o777, 0o600)
            self.assertEqual(node['slice'], 'horizontal-fdb.slice' if role == 'node1' else 'horizontal-client.slice')
            receipt = p._load(state/(role+'-native.json'))
            self.assertEqual(receipt['filesystem'], 'ext4')
            self.assertEqual(receipt['floor_bytes'], p.disk_floor(receipt['total_bytes']))
            self.assertTrue(receipt['fsync_preserved'])
            self.assertTrue(receipt['checksums_preserved'])
        self.assertFalse((state/'host-keys'/(self.plan['prefix']+'-node2')).exists())
        self.assertIn('CPUQuota=400%', p._slice('fdb'))
        self.assertIn('MemoryMax=16G', p._slice('client'))
        forged = dict(resources['nodes']['node1'], id=self.cloud.baseline['server'][0]['id'])
        before = len(self.cloud.calls)
        with self.assertRaises(p.ProvisionError):
            p.run_on_node(self.plan, self.out, forged, ['true'], runner=self.cloud)
        self.assertEqual(len(self.cloud.calls), before)

    def test_diagnostic_render_and_setup_run_only_six_processes_on_one_fdb_host(self):
        self.diagnostic_plan()
        resources = self.prepare()
        nodes = resources['nodes']
        rendered = p.render_cluster(self.plan, nodes, 1, 'rendered')
        self.assertEqual(rendered['storage_hosts'], ['node1'])
        self.assertEqual(len(rendered['processes']), 6)
        self.assertEqual({x['node'] for x in rendered['processes']}, {'node1'})
        self.assertEqual({x['process_class'] for x in rendered['processes']},
                         {'stateless', 'commit_proxy', 'grv_proxy', 'resolution', 'transaction', 'storage'})
        self.assertEqual(sum(x['memory_limit_bytes'] for x in rendered['processes']), 9*p.GIB)
        storage = [x for x in rendered['processes'] if x['process_class'] == 'storage']
        self.assertEqual(len(storage), 1)
        self.assertEqual(storage[0]['memory_limit_bytes'], 4*p.GIB)
        self.assertEqual(storage[0]['cache_mib'], 2048)
        first = p.setup_cluster(self.plan, self.out, nodes, 1, 'diagnostic1', runner=self.cloud)
        second = p.setup_cluster(self.plan, self.out, nodes, 1, 'diagnostic2', runner=self.cloud)
        self.assertNotEqual(first['root'], second['root'])
        self.assertEqual(first['configure'], p.CONFIGURE)
        self.assertEqual(first['budgets'], second['budgets'])
        active = p._load(p._state(self.out)/'active-arm.json')
        self.assertEqual(set(active['units']), {'node1'})
        self.assertEqual(len(active['units']['node1']), 6)
        stops = [argv for argv, _ in self.cloud.calls if Path(argv[0]).name == 'ssh'
                 and shlex.split(argv[-1])[:2] == ['systemctl', 'stop']]
        self.assertEqual(len(stops), 1)
        self.assertIn('root@'+nodes['node1']['host'], stops[0])
        self.assertEqual(len(shlex.split(stops[0][-1])[2:]), 6)

    def test_diagnostic_rejects_extra_missing_nodes_or_more_than_one_storage_host_without_commands(self):
        self.diagnostic_plan()
        resources = self.prepare()
        nodes = resources['nodes']
        before = len(self.cloud.calls)
        for changed in ({'node1': nodes['node1']}, {'client1': nodes['client1']}, {},
                        dict(nodes, node2=nodes['node1']), dict(nodes, client2=nodes['client1'])):
            with self.subTest(keys=set(changed)):
                with self.assertRaises(p.ProvisionError):
                    p.render_cluster(self.plan, changed, 1, 'invalid')
                with self.assertRaises(p.ProvisionError):
                    p.setup_cluster(self.plan, self.out, changed, 1, 'invalid', runner=self.cloud)
        for bad in (0, 2, 3, True, None, 1.0, '1', [], ['node2'], ['node1', 'node2']):
            with self.subTest(storage_hosts=bad):
                with self.assertRaises(p.ProvisionError):
                    p.render_cluster(self.plan, nodes, bad, 'invalid')
                with self.assertRaises(p.ProvisionError):
                    p.setup_cluster(self.plan, self.out, nodes, bad, 'invalid', runner=self.cloud)
        self.assertEqual(len(self.cloud.calls), before)

    def test_diagnostic_teardown_restores_all_ten_protected_baseline_sets(self):
        self.diagnostic_plan()
        resources = self.prepare()
        result = p.teardown(self.plan, self.out, resources, runner=self.cloud, sleep=lambda _: None)
        self.assertTrue(result['passed'])
        self.assertEqual(set(result['inventory']['kinds']), set(p.KINDS))
        self.assertEqual(self.cloud.items, self.cloud.baseline)
        self.assertEqual(sum(kind == 'server' for kind, _ in self.cloud.deleted), 2)
        self.assertEqual(sum(kind == 'placement-group' for kind, _ in self.cloud.deleted), 1)
        self.assertTrue(all(rid > 10 for _, rid in self.cloud.deleted))
        self.assertFalse(any(kind == 'primary-ip' for kind, _ in self.cloud.deleted))

    def test_diagnostic_partial_second_create_failure_cleans_only_its_two_owned_roles(self):
        self.diagnostic_plan()
        self.cloud.fail_after_create = ('server', 2)
        with self.assertRaises(p.ProvisionError):
            self.prepare()
        self.assertEqual(self.cloud.items, self.cloud.baseline)
        self.assertEqual(self.cloud.create_counts['server'], 2)
        self.assertEqual(sum(kind == 'server' for kind, _ in self.cloud.deleted), 2)
        self.assertTrue(p._load(p._state(self.out)/'teardown-complete.json')['passed'])

    def test_diagnostic_cleanup_refuses_undeclared_horizontal_role(self):
        self.diagnostic_plan()
        resources = self.prepare()
        rogue = dict(id=999, name=self.plan['prefix']+'-node2', labels=dict(campaign=self.plan['campaign']))
        self.cloud.items['server'].append(rogue)
        result = p.teardown(self.plan, self.out, resources, runner=self.cloud, sleep=lambda _: None)
        self.assertFalse(result['passed'])
        self.assertNotIn(('server', 999), self.cloud.deleted)
        self.assertIn(rogue, self.cloud.items['server'])
        self.assertTrue(result['refused'])
        self.assertFalse((p._state(self.out)/'teardown-complete.json').exists())

    def creation_fixture(self):
        serial = getattr(self, '_creation_serial', 0)+1
        self._creation_serial = serial
        out = self.root / ('creation-'+str(serial))
        state = p._state(out)
        state.mkdir(mode=0o700, parents=True)
        cloud = FakeCloud(self.plan, out)
        cloud.events.append('guard-ready')
        baseline = state / 'baseline'
        baseline.mkdir(mode=0o700)
        for kind in p.KINDS:
            p._json(baseline / ('before-'+kind+'.json'), cloud.baseline[kind])
        resources = dict(campaign=self.plan['campaign'], prefix=self.plan['prefix'], nodes={},
                         ids={kind: [] for kind in p.KINDS},
                         protected_ids={kind: [i['id'] for i in cloud.baseline[kind]] for kind in p.KINDS})
        p._save_resources(state, resources)
        now = [float(self.plan['first_epoch']+1)]
        cloud.creation_clock = lambda seconds: now.__setitem__(0, now[0]+seconds)
        guard = mock.Mock()
        guard.poll.return_value = None
        argv = [self.plan['hcloud'], 'server', 'create', '--name', self.plan['prefix']+'-node1',
                '--placement-group', '900', '--label', 'campaign='+self.plan['campaign']]
        return dict(out=out, state=state, cloud=cloud, resources=resources, now=now, guard=guard, argv=argv)

    def create(self, fixture, runner=None):
        return p._create(self.plan, fixture['state'], fixture['resources'], 'server', fixture['argv'],
                         runner or fixture['cloud'], lambda: fixture['now'][0], fixture['guard'])

    def test_compound_create_takes_twenty_three_seconds_and_journals_raw_success(self):
        f = self.creation_fixture()
        f['cloud'].creation_delays[('server', 1)] = 23
        f['cloud'].creation_outputs[('server', 1)] = ('raw-create-success\\n', 'raw-create-notice\\n')
        start = f['now'][0]
        created = self.create(f)
        self.assertEqual(f['now'][0]-start, 23)
        self.assertEqual(f['resources']['ids']['server'], [created['id']])
        self.assertEqual(len(f['cloud'].creation_invocations), 1)
        self.assertEqual(f['cloud'].creation_invocations[0]['timeout'], 120)
        attempts = f['resources']['creation_attempts']
        self.assertEqual(len(attempts), 1)
        receipt = p._load(attempts[0])
        self.assertEqual(receipt['kind'], 'server')
        self.assertEqual(receipt['argv'], f['argv'])
        self.assertEqual(receipt['returncode'], 0)
        self.assertFalse(receipt['timed_out'])
        self.assertEqual(receipt['timeout_seconds'], 120)
        self.assertEqual(receipt['finished_epoch']-receipt['epoch'], 23)
        self.assertEqual(receipt['stdout'], 'raw-create-success\\n')
        self.assertEqual(receipt['stderr'], 'raw-create-notice\\n')
        self.assertEqual(Path(attempts[0]).stat().st_mode & 0o777, 0o600)

    def test_compound_create_timeout_after_120_seconds_is_not_resubmitted_and_cleans_ambiguous_id(self):
        f = self.creation_fixture()
        f['cloud'].creation_delays[('server', 1)] = 121
        with self.assertRaises(p.ProvisionError):
            self.create(f)
        self.assertEqual(len(f['cloud'].creation_invocations), 1)
        self.assertEqual(f['cloud'].creation_invocations[0]['timeout'], 120)
        self.assertFalse(f['resources']['ids']['server'])
        attempts = p._load(f['state'] / 'resources.json')['creation_attempts']
        self.assertEqual(len(attempts), 1)
        receipt = p._load(attempts[0])
        self.assertTrue(receipt['timed_out'])
        self.assertIsNone(receipt['returncode'])
        self.assertIn('created-resource-pending-output', receipt['stdout'])
        self.assertIn('compound-create-timeout-stderr', receipt['stderr'])
        self.assertEqual(Path(attempts[0]).stat().st_mode & 0o777, 0o600)
        result = p.teardown(self.plan, f['out'], f['resources'], runner=f['cloud'],
                            clock=lambda: f['now'][0], sleep=lambda _: None)
        self.assertTrue(result['passed'])
        self.assertEqual(f['cloud'].items, f['cloud'].baseline)
        self.assertEqual(len(f['cloud'].creation_invocations), 1)
        self.assertTrue(all(rid > 10 for _, rid in f['cloud'].deleted))

    def test_compound_create_uses_original_active_budget_and_does_not_adopt_after_expiry(self):
        for delay in (3, 23):
            with self.subTest(delay=delay):
                f = self.creation_fixture()
                f['now'][0] = self.plan['first_epoch']+5997
                f['cloud'].creation_delays[('server', 1)] = delay
                with self.assertRaises(p.ProvisionError):
                    self.create(f)
                self.assertEqual(len(f['cloud'].creation_invocations), 1)
                self.assertEqual(f['cloud'].creation_invocations[0]['timeout'], 3)
                self.assertFalse(f['resources']['ids']['server'])
                self.assertFalse(any(a[1:3] == ['server', 'list'] for a, _ in f['cloud'].calls))
                receipt = p._load(f['resources']['creation_attempts'][0])
                self.assertEqual(receipt['timeout_seconds'], 3)
                self.assertEqual(receipt['timed_out'], delay > 3)
                self.assertEqual(receipt['returncode'], None if delay > 3 else 0)
        f = self.creation_fixture()
        f['now'][0] = self.plan['first_epoch']+6000
        with self.assertRaises(p.ProvisionError):
            self.create(f)
        self.assertFalse(f['cloud'].creation_invocations)

    def test_compound_create_os_and_subprocess_errors_are_journaled_without_resubmission(self):
        for error in (OSError('simulated-create-os-error'), p.subprocess.SubprocessError('simulated-create-subprocess-error')):
            f = self.creation_fixture()
            f['cloud'].creation_exceptions[('server', 1)] = error
            with self.assertRaises(p.ProvisionError):
                self.create(f)
            self.assertEqual(len(f['cloud'].creation_invocations), 1)
            receipt = p._load(f['resources']['creation_attempts'][0])
            self.assertIsNone(receipt['returncode'])
            self.assertFalse(receipt['timed_out'])
            self.assertIn(str(error), receipt['stderr'])
            self.assertEqual(Path(f['resources']['creation_attempts'][0]).stat().st_mode & 0o777, 0o600)
            result = p.teardown(self.plan, f['out'], f['resources'], runner=f['cloud'],
                                clock=lambda: f['now'][0], sleep=lambda _: None)
            self.assertTrue(result['passed'])
            self.assertEqual(f['cloud'].items, f['cloud'].baseline)

    def test_compound_create_never_adopts_protected_id_from_success_readback(self):
        f = self.creation_fixture()
        original = copy.deepcopy(f['cloud'].baseline['server'])

        def protected_readback(argv, timeout=120):
            rc, stdout, stderr = f['cloud'](argv, timeout=timeout)
            if argv[1:3] == ['server', 'list']:
                items = json.loads(stdout)
                candidates = [item for item in items if item['name'] == self.plan['prefix']+'-node1']
                candidates[0]['id'] = original[0]['id']
                return rc, json.dumps(candidates), stderr
            return rc, stdout, stderr

        with self.assertRaises(p.ProvisionError):
            self.create(f, runner=protected_readback)
        self.assertFalse(f['resources']['ids']['server'])
        self.assertEqual(f['cloud'].items['server'][0], original[0])
        self.assertFalse(f['cloud'].deleted)
        self.assertEqual(len(f['cloud'].creation_invocations), 1)
        self.assertEqual(len(f['resources']['creation_attempts']), 1)

    def attachment_fixture(self):
        serial = getattr(self, '_attachment_serial', 0) + 1
        self._attachment_serial = serial
        out = self.root / ('attachment-'+str(serial))
        state = p._state(out)
        state.mkdir(mode=0o700, parents=True)
        cloud = FakeCloud(self.plan, out)
        server = dict(id=101, name=self.plan['prefix']+'-node1', private_net=[],
                      labels=dict(campaign=self.plan['campaign']),
                      public_net=dict(ipv4=dict(id=103, ip='192.0.2.10'),
                                      ipv6=copy.deepcopy(cloud.ipv6_descriptor)))
        network = dict(id=102, name=self.plan['prefix']+'-net', ip_range='10.93.0.0/24',
                       labels=dict(campaign=self.plan['campaign']))
        cloud.items['server'].append(server)
        cloud.items['network'].append(network)
        cloud.next_id = 200
        cloud.events.append('guard-ready')
        resources = dict(campaign=self.plan['campaign'], prefix=self.plan['prefix'],
                         ids={kind: [] for kind in p.KINDS}, nodes={},
                         protected_ids={kind: [item['id'] for item in cloud.baseline[kind]] for kind in p.KINDS})
        resources['ids']['server'] = [server['id']]
        resources['ids']['network'] = [network['id']]
        p._save_resources(state, resources)
        now = [float(self.plan['first_epoch']+1)]
        guard = mock.Mock()
        guard.poll.return_value = None
        return dict(out=out, state=state, cloud=cloud, server=server, network=network,
                    resources=resources, now=now, guard=guard, private='10.93.0.11')

    def attach(self, fixture, sleep=None):
        if sleep is None:
            def sleep(seconds):
                fixture['now'][0] += seconds
        return p._attach_network(self.plan, fixture['state'], fixture['resources'],
                                 fixture['server'], fixture['network'], fixture['private'],
                                 fixture['cloud'], lambda: fixture['now'][0], fixture['guard'], sleep)

    def test_attach_success_persists_action_before_private_network_readback(self):
        f = self.attachment_fixture()
        self.attach(f)
        calls = f['cloud'].attach_api_calls
        self.assertEqual(sum(c['method'] == 'POST' for c in calls), 1)
        self.assertEqual(calls[-1]['path'], '/servers/101')
        self.assertTrue(any(c['path'] == '/networks/actions/201' for c in calls))
        self.assertFalse(any(re.fullmatch(r'/servers/\d+/actions/\d+', c['path']) for c in calls))
        self.assertEqual(f['server']['private_net'], [dict(network=102, ip=f['private'], alias_ips=[])])
        journal = p._load(f['state'] / 'resources.json')
        self.assertTrue(journal['network_attachments'])
        self.assertIn('201', json.dumps(journal['network_attachments']))
        self.assertTrue(all(0 < c['timeout'] <= 20 for c in calls))
        raw = list(f['state'].glob('attach-*-call*.json'))
        self.assertGreaterEqual(len(raw), 2)
        for receipt in raw:
            self.assertEqual(receipt.stat().st_mode & 0o777, 0o600)
        evidence = json.dumps([p._load(path) for path in raw])
        self.assertIn('attach_to_network', evidence)
        self.assertIn('stdout', evidence)
        self.assertIn('stderr', evidence)
        self.assertNotIn('server poweron', f['cloud'].events)

    def test_network_attach_actual_action_shape_and_no_resubmission(self):
        actual = dict(action=dict(id=664525933777883, command='attach_to_network', status='success',
                      started='2026-10-29T09:30:32Z', finished='2026-10-29T09:30:51Z',
                      resources=[dict(type='server', id=173318580), dict(type='network', id=12786571)], error=None))
        self.assertEqual(p._attach_action(actual, 173318580, 12786571, 664525933777883)['status'], 'success')
        f = self.attachment_fixture()
        self.attach(f)
        before = len(f['cloud'].attach_api_calls)
        with self.assertRaisesRegex(p.ProvisionError, 'resubmission'):
            self.attach(f)
        self.assertEqual(len(f['cloud'].attach_api_calls), before)

    def test_attach_poll_rejects_changed_action_id_and_missing_error(self):
        for field in ('id', 'error'):
            f = self.attachment_fixture()
            f['cloud'].attach_post_status = 'running'

            def corrupt(action, phase):
                if phase == 'poll':
                    if field == 'id':
                        action['id'] += 1
                    else:
                        del action['error']

            f['cloud'].attach_action_mutator = corrupt
            with self.assertRaises(p.ProvisionError):
                self.attach(f)
            self.assertEqual(sum(c['method'] == 'POST' for c in f['cloud'].attach_api_calls), 1)
            self.assertFalse(any(c['path'] == '/servers/101' for c in f['cloud'].attach_api_calls))

    def test_attach_running_longer_than_twenty_seconds_eventually_succeeds(self):
        f = self.attachment_fixture()
        cloud = f['cloud']
        cloud.attach_post_status = 'running'
        cloud.attach_poll_statuses = ['running']*6 + ['success']
        started = f['now'][0]

        def elapsed(path, method, timeout):
            if method == 'GET' and '/actions/' in path:
                f['now'][0] += 4

        cloud.attach_call_hook = elapsed
        self.attach(f)
        self.assertGreater(f['now'][0]-started, 20)
        self.assertLess(f['now'][0]-started, 120)
        self.assertEqual(sum(c['method'] == 'POST' for c in cloud.attach_api_calls), 1)
        self.assertEqual(cloud.attach_api_calls[-1]['path'], '/servers/101')
        self.assertTrue(all(0 < c['timeout'] <= 20 for c in cloud.attach_api_calls))

    def test_attach_action_may_omit_optional_network_resource(self):
        f = self.attachment_fixture()
        f['cloud'].attach_action_mutator = lambda action, phase: action.update(
            resources=[dict(type='server', id=101)])
        self.attach(f)
        self.assertEqual(f['server']['private_net'][0]['network'], 102)

    def test_attach_error_and_running_expiry_never_power_on_or_repeat_post(self):
        for status in ('error', 'running'):
            with self.subTest(status=status):
                f = self.attachment_fixture()
                f['cloud'].attach_post_status = status

                def elapsed(path, method, timeout):
                    if method == 'GET' and '/actions/' in path:
                        f['now'][0] += min(timeout, 10)

                f['cloud'].attach_call_hook = elapsed
                with self.assertRaises(p.ProvisionError):
                    self.attach(f)
                self.assertEqual(sum(c['method'] == 'POST' for c in f['cloud'].attach_api_calls), 1)
                self.assertFalse(any(c['path'] == '/servers/101' for c in f['cloud'].attach_api_calls))
                self.assertNotIn('server poweron', f['cloud'].events)
                self.assertTrue(p._load(f['state'] / 'resources.json')['network_attachments'])

    def test_attach_malformed_or_foreign_actions_fail_closed_with_raw_evidence(self):
        mutations = [('id', 0), ('id', True), ('id', '201'), ('command', 'rebuild'),
                     ('command', None), ('status', 'unknown'), ('status', None),
                     ('error', dict(code='unexpected', message='failure')),
                     ('resources', []), ('resources', [dict(type='server', id=1)]),
                     ('resources', [dict(type='server', id=101), dict(type='network', id=2)]),
                     ('resources', [dict(type='server', id=True)]),
                     ('resources', [dict(type='server', id=101), dict(type='server', id=101)]),
                     ('resources', [dict(type='server', id=101, extra='unknown')]),
                     ('resources', [dict(type='volume', id=101)])]
        for field, value in mutations:
            with self.subTest(field=field, value=value):
                f = self.attachment_fixture()
                f['cloud'].attach_action_mutator = lambda action, phase, k=field, v=value: action.update({k:v})
                with self.assertRaises(p.ProvisionError):
                    self.attach(f)
                self.assertEqual(sum(c['method'] == 'POST' for c in f['cloud'].attach_api_calls), 1)
                self.assertTrue(list(f['state'].glob('attach-*-call*.json')))
                if field != 'id':
                    journal = p._load(f['state'] / 'resources.json')
                    self.assertIn('201', json.dumps(journal['network_attachments']))
                self.assertNotIn('server poweron', f['cloud'].events)

    def test_attach_success_requires_exact_owned_server_and_private_network_readback(self):
        mutations = [lambda server: server.update(id=1),
                     lambda server: server.update(name='foreign-server'),
                     lambda server: server.update(labels=dict(campaign='foreign')),
                     lambda server: server.update(private_net=[]),
                     lambda server: server.update(private_net=[dict(network=2, ip='10.93.0.11', alias_ips=[])]),
                     lambda server: server.update(private_net=[dict(network=102, ip='10.93.0.99', alias_ips=[])]),
                     lambda server: server.update(private_net=[dict(network=102, ip='10.93.0.11', alias_ips=['10.93.0.99'])])]
        for mutate in mutations:
            f = self.attachment_fixture()
            f['cloud'].attach_server_mutator = mutate
            with self.assertRaises(p.ProvisionError):
                self.attach(f)
            self.assertEqual(sum(c['method'] == 'POST' for c in f['cloud'].attach_api_calls), 1)
            self.assertTrue(list(f['state'].glob('attach-*-call*.json')))
            self.assertNotIn('server poweron', f['cloud'].events)

    def test_attach_post_timeout_is_not_replayed_and_raw_partial_output_survives_cleanup(self):
        self.cloud.attach_post_exception = p.subprocess.TimeoutExpired(
            ['hcloud', 'api'], 20, output='ambiguous-post-output', stderr='ambiguous-post-stderr')
        with self.assertRaises((p.ProvisionError, p.subprocess.TimeoutExpired)):
            self.prepare()
        self.assertEqual(sum(c['method'] == 'POST' for c in self.cloud.attach_api_calls), 1)
        self.assertEqual(self.cloud.items, self.cloud.baseline)
        self.assertTrue(p._load(p._state(self.out) / 'teardown-complete.json')['passed'])
        raw = list(p._state(self.out).glob('attach-*-call*.json'))
        self.assertTrue(raw)
        evidence = json.dumps([p._load(path) for path in raw])
        self.assertIn('ambiguous-post-output', evidence)
        self.assertIn('ambiguous-post-stderr', evidence)

    def test_attach_aborts_on_original_active_deadline_dead_guard_or_cleanup_marker(self):
        for reason in ('deadline', 'guard', 'marker'):
            with self.subTest(reason=reason):
                f = self.attachment_fixture()
                if reason == 'deadline':
                    f['now'][0] = self.plan['first_epoch']+6000
                elif reason == 'guard':
                    f['guard'].poll.return_value = 1
                else:
                    p._json(f['state'] / 'cleanup-started.json', dict(epoch=f['now'][0]))
                with self.assertRaises(p.ProvisionError):
                    self.attach(f)
                self.assertFalse(f['cloud'].attach_api_calls)
        for reason in ('deadline', 'guard', 'marker'):
            f = self.attachment_fixture()
            f['cloud'].attach_post_status = 'running'

            def interrupt(seconds):
                f['now'][0] += seconds
                if reason == 'deadline':
                    f['now'][0] = self.plan['first_epoch']+6000
                elif reason == 'guard':
                    f['guard'].poll.return_value = 1
                elif not (f['state'] / 'cleanup-started.json').exists():
                    p._json(f['state'] / 'cleanup-started.json', dict(epoch=f['now'][0]))

            with self.assertRaises(p.ProvisionError):
                self.attach(f, sleep=interrupt)
            self.assertEqual(sum(c['method'] == 'POST' for c in f['cloud'].attach_api_calls), 1)
            self.assertNotIn('server poweron', f['cloud'].events)

    def test_prepare_exact_five_spread_native_hosts_and_pinned_transports(self):
        resources = self.prepare()
        self.assertTrue(resources['prepared'])
        self.assertEqual(set(resources['nodes']), set(p.ROLES))
        self.assertEqual({role: node['private_ip'] for role, node in resources['nodes'].items()},
                         {'node1': '10.93.0.11', 'node2': '10.93.0.12', 'node3': '10.93.0.13',
                          'client1': '10.93.0.21', 'client2': '10.93.0.22'})
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

    def test_horizontal_cloud_init_has_nonempty_key_types_and_preserves_pinned_keys(self):
        resources = self.prepare()
        state = p._state(self.out)
        known = []
        controller = p._cloud_init.read_public_key(self.plan['controller_public_key'])
        for node in resources['nodes'].values():
            host = node['name']
            private, public = p._cloud_init.read_host_key(state / 'host-keys', host)
            original = p._cloud_init.cloud_config(host, private, public, controller)
            self.assertIn('ssh_genkeytypes: []\n', original)  # Shared/old emission is untouched.
            emitted = (state / 'cloud-init' / (host + '.user-data')).read_text()
            expected = original.replace('package_update: false', 'package_update: true')
            expected = expected.replace('ssh_genkeytypes: []\n', 'ssh_genkeytypes: [ed25519]\n')
            expected = expected.replace('package_upgrade: false',
                                        'package_upgrade: false\npackages: [python3, chrony, jq]')
            self.assertEqual(emitted, expected)
            self.assertEqual(emitted.count('ssh_genkeytypes: [ed25519]\n'), 1)
            self.assertIn('ssh_keys:\n  ed25519_private: |\n' + '\n'.join('    ' + line for line in private), emitted)
            self.assertIn('  ed25519_public: ' + public + '\n', emitted)
            known.append(host + ' ' + ' '.join(public.split()[:2]))
            self.assertIn('UserKnownHostsFile=' + node['known_hosts'], node['ssh'])
        self.assertEqual((state / 'cloud-init' / 'known_hosts').read_text(), '\n'.join(known) + '\n')

    def test_unexpected_cloud_init_key_field_fails_before_cloud_creation(self):
        original = p._cloud_init.cloud_config
        with mock.patch.object(p._cloud_init, 'cloud_config', side_effect=lambda *args:
                               original(*args).replace('ssh_genkeytypes: []\n', '')):
            with self.assertRaisesRegex(p.ProvisionError, 'unexpected shared cloud-init'):
                self.prepare()
        self.assertFalse(self.cloud.create_counts)

    def test_readiness_success_is_journaled_privately_before_admission(self):
        resources = self.prepare()
        state = p._state(self.out)
        receipts = sorted(state.glob('*-readiness-*.json'))
        self.assertEqual(len(receipts), 5)
        self.assertEqual(len(self.cloud.readiness_calls), 5)
        for path in receipts:
            receipt = p._load(path)
            self.assertEqual(receipt['returncode'], 0)
            self.assertFalse(receipt['timed_out'])
            self.assertEqual(receipt['stdout'], 'status: done\n')
            self.assertEqual(receipt['stderr'], '')
            self.assertGreater(receipt['timeout_seconds'], 0)
            self.assertLessEqual(receipt['timeout_seconds'], 30)
            self.assertEqual(path.stat().st_mode & 0o777, 0o600)
        self.assertTrue(resources['prepared'])

    def test_degraded_completed_readiness_fails_fast_keeps_raw_output_and_cleans(self):
        self.cloud.readiness_results = [(2, 'status: done\n', 'ssh_genkeytypes: [] is too short\n')]
        with self.assertRaisesRegex(p.ProvisionError, 'readiness failed \\(2\\).*ssh_genkeytypes'):
            self.prepare()
        state = p._state(self.out)
        self.assertEqual(len(self.cloud.readiness_calls), 1)
        receipt = p._load(state / 'node1-readiness-1.json')
        self.assertEqual(receipt['returncode'], 2)
        self.assertFalse(receipt['timed_out'])
        self.assertEqual(receipt['stdout'], 'status: done\n')
        self.assertEqual(receipt['stderr'], 'ssh_genkeytypes: [] is too short\n')
        self.assertFalse(list(state.glob('*-native.json')))
        self.assertTrue(p._load(state / 'teardown-complete.json')['passed'])
        self.assertEqual(self.cloud.items, self.cloud.baseline)

    def test_permanent_readiness_error_never_retries_or_accepts_later_success(self):
        self.cloud.readiness_results = [(1, 'status: error\n', 'permanent module failure\n'), (0, 'status: done\n', '')]
        with self.assertRaisesRegex(p.ProvisionError, 'readiness failed \\(1\\).*permanent module failure'):
            self.prepare()
        self.assertEqual(len(self.cloud.readiness_calls), 1)
        self.assertEqual(len(self.cloud.readiness_results), 1)
        self.assertEqual(self.cloud.items, self.cloud.baseline)
        self.assertEqual(p._load(p._state(self.out) / 'node1-readiness-1.json')['returncode'], 1)

    def test_readiness_timeout_and_ssh255_retry_but_never_count_as_success(self):
        self.cloud.readiness_results = [p.subprocess.TimeoutExpired(['ssh'], 30, output=b'booting\n', stderr=b'not ready\n'),
                                        (255, '', 'Connection refused\n'), (0, 'status: done\n', '')]
        self.prepare()
        state = p._state(self.out)
        receipts = [p._load(state / ('node1-readiness-%d.json' % i)) for i in (1, 2, 3)]
        self.assertTrue(receipts[0]['timed_out'])
        self.assertIsNone(receipts[0]['returncode'])
        self.assertEqual(receipts[0]['stdout'], 'booting\n')
        self.assertEqual(receipts[0]['stderr'], 'not ready\n')
        self.assertEqual(receipts[1]['returncode'], 255)
        self.assertFalse(receipts[1]['timed_out'])
        self.assertEqual(receipts[1]['stderr'], 'Connection refused\n')
        self.assertEqual(receipts[2]['returncode'], 0)
        self.assertEqual(len(self.cloud.readiness_calls), 7)

    def test_readiness_timeout_then_degraded_is_fatal_not_timeout_waiver(self):
        self.cloud.readiness_results = [p.subprocess.TimeoutExpired(['ssh'], 30), (2, 'degraded\n', 'invalid user-data\n')]
        with self.assertRaisesRegex(p.ProvisionError, 'readiness failed \\(2\\)'):
            self.prepare()
        self.assertEqual(len(self.cloud.readiness_calls), 2)
        self.assertEqual(self.cloud.items, self.cloud.baseline)
        receipt = p._load(p._state(self.out) / 'node1-readiness-2.json')
        self.assertEqual(receipt['stderr'], 'invalid user-data\n')

    def test_readiness_transport_exception_is_journaled_then_fatal(self):
        self.cloud.readiness_results = [OSError('SSH executable unavailable')]
        with self.assertRaisesRegex(p.ProvisionError, 'SSH executable unavailable'):
            self.prepare()
        receipt = p._load(p._state(self.out) / 'node1-readiness-1.json')
        self.assertIsNone(receipt['returncode'])
        self.assertFalse(receipt['timed_out'])
        self.assertEqual(receipt['stderr'], 'SSH executable unavailable')
        self.assertEqual(self.cloud.items, self.cloud.baseline)

    def test_readiness_timeout_cannot_reset_original_preparation_cap(self):
        self.cloud.readiness_results = [p.subprocess.TimeoutExpired(['ssh'], 30), (0, 'status: done\n', '')]
        now = [time.time()]
        def sleep(seconds):
            now[0] += 600
        with self.assertRaisesRegex(p.ProvisionError, 'preparation budget'):
            p.prepare(self.plan, self.out, runner=self.cloud, popen=self.cloud.popen,
                      clock=lambda: now[0], sleep=sleep)
        self.assertEqual(len(self.cloud.readiness_calls), 1)
        self.assertEqual(len(self.cloud.readiness_results), 1)
        self.assertTrue(p._load(p._state(self.out) / 'node1-readiness-1.json')['timed_out'])
        self.assertEqual(self.cloud.items, self.cloud.baseline)

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
