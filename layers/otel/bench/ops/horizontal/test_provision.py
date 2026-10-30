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


def legacy_slice_bytes(kind):
    return f'''[Unit]
Description=Horizontal campaign {kind} resource boundary
[Slice]
CPUAccounting=yes
MemoryAccounting=yes
AllowedCPUs=0-3
CPUQuota=400%
MemoryMax=16G
MemorySwapMax=0
'''.encode()


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


class StopRunner:
    """Fake exact /proc+cgroup snapshots for the owned-arm stop protocol."""
    def __init__(self, cloud, nodes, layout, scopes=()):
        self.cloud, self.nodes = cloud, nodes
        self.records, self.stopped, self.stop_commands = {}, set(), []
        self.reused, self.linger, self.mutate = {}, False, None
        for index, item in enumerate(layout['processes']):
            self.records[(item['node'], item['unit_name'])] = dict(pid=500+index, starttime_ticks=10000+index,
                args=item['args'], exe=item['args'][0],
                cgroup='/horizontal.slice/horizontal-fdb.slice/'+item['unit_name'])
        for index, item in enumerate(scopes):
            self.records[(item['node'], item['unit'])] = dict(pid=item.get('pid', 900+index),
                starttime_ticks=item.get('starttime_ticks', 9000+index), args=item['args'],
                exe=item['args'][0], cgroup=item['cgroup'])

    def live(self, role):
        return {unit: value for (node, unit), value in self.records.items()
                if node == role and (self.linger or (node, unit) not in self.stopped)}

    def __call__(self, argv, timeout=120):
        if Path(argv[0]).name != 'ssh':
            return self.cloud(argv, timeout=timeout)
        target = next(value for value in argv if value.startswith('root@'))[5:]
        role = next(role for role, node in self.nodes.items() if node['host'] == target)
        args = shlex.split(argv[-1])
        if args[:2] == ['systemctl', 'stop']:
            for unit in args[2:]:
                if (role, unit) not in self.records:
                    raise AssertionError('stop attempted an undeclared unit')
                self.stopped.add((role, unit))
            self.stop_commands.append((role, args[2:]))
            return 0, '', ''
        if args[:3] != ['python3', '-c', p._STOP_PROBE]:
            return self.cloud(argv, timeout=timeout)
        request = json.loads(args[3]); current = self.live(role)
        units = []
        for spec in request['units']:
            value = current.get(spec['unit'])
            service = spec['unit'].endswith('.service')
            units.append(dict(unit=spec['unit'], load_state='loaded' if value else 'not-found',
                active_state='active' if value else 'inactive', control_group=spec['cgroup'] if value else '',
                main_pid=value['pid'] if value and service else 0,
                main=copy.deepcopy(value) if value and service else None,
                pids=[copy.deepcopy(value)] if value else []))
        historical = {}
        for pid in request['historical']:
            values = [value for value in current.values() if value['pid'] == pid]
            historical[str(pid)] = copy.deepcopy(values[0] if values else self.reused.get(pid))
        result = dict(units=units, historical=historical, owned_residual=copy.deepcopy(list(current.values())))
        if self.mutate:
            self.mutate(role, 'after' if self.stop_commands else 'before', result)
        return 0, json.dumps(result), ''


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

    def session_plan(self):
        self.diagnostic_plan()
        self.plan.update(schema='otel_native_session_v1', cases=['capacity16g', 'baseline'],
                         storage_process_count=2, storage_max_outstanding=64, prior_rows_upper=0)
        return self.plan

    def scaling_plan(self, *, managed_admitted=True):
        output = self.root / 'campaign'
        self.out = output / 'provision'
        self.cloud.out = self.out
        self.plan.update(schema='otel_native_scaling_v1', replication='single', stages=[[1, 1], [2, 2], [3, 3]],
                         count=100000, measured_count=375000, measured_seconds=120, prior_rows_upper=7000000,
                         batch=64, workers=8, gomaxprocs=4, storage_process_count=3,
                         storage_max_outstanding=64, storage_memory_profile='scaling3ss3g',
                         managed_watch=dict(root=str(self.root / 'managed-watch'), output=str(output)))
        if managed_admitted:
            import supervisor
            patch = mock.patch.object(supervisor, 'admit', return_value={'admitted': True})
            self.managed_admit = patch.start()
            self.addCleanup(patch.stop)
        return self.plan

    def scaling_log_plan(self):
        self.scaling_plan(managed_admitted=False)
        del self.plan['managed_watch']
        self.plan.update(foreground_only=True, log_memory_gib=2, stages=[[2, 2], [1, 1]], prior_rows_upper=8425000)
        return self.plan

    def scaling_nodes(self):
        return {role: dict(private_ip='10.93.0.'+str(offset)) for role, offset in
                (('node1', 11), ('node2', 12), ('node3', 13), ('client1', 21), ('client2', 22), ('client3', 23))}

    def scaling_journal(self, layout, stage):
        return dict(campaign=self.plan['campaign'], armid='scaling-'+str(stage),
                    units={role: [item['unit_name'] for item in layout['processes'] if item['node'] == role]
                           for role in p._roles(self.plan)})

    def scaling_scope(self, layout, client, mode='ingest', history=True):
        scope = self.owned_scope(layout, mode, history)
        old = 'prof20k9000'; run = 'prof20k'+str(9300+client)
        scope['node'] = 'client'+str(client)
        scope['unit'] = scope['unit'].replace(old, run)
        scope['cgroup'] = scope['cgroup'].replace(old, run)
        scope['args'] = [value.replace(old, run) for value in scope['args']]
        for flag, value in (('--count', '375000'), ('--batch', '64'), ('--workers', '8')):
            scope['args'][scope['args'].index(flag)+1] = value
        return scope

    def case_plan(self, profile):
        case = dict(self.plan, schema='otel_native_diagnostic_v1', storage_memory_profile=profile)
        case.pop('cases', None); case.pop('prior_rows_upper', None)
        return case

    def owned_scope(self, layout, mode='ingest', history=True):
        root = p.REMOTE_BASE+'/'+self.plan['campaign']; run = 'prof20k9000'
        unit = 'otel-native-'+run+'-'+mode+'.scope'
        args = [root+'/bin/otel-demo', '--backend', 'fdb', '--order', 'time', '--cluster-file', layout['remote_cluster_file'],
                '--run', run, '--seed', '283', '--count', '1000000', '--tenants', '1', '--attrs', '8',
                '--match-every', '100', '--batch', '32', '--save-mode', 'batch', '--workers', '4', '--diagnostics',
                '--k', '100', '--max-candidates', '2000000', '--max-bytes', '268435456', '--revision', 'a'*40,
                '--record-compression', 'brotli-4', '--posting-value-format', 'tuple', '--bulk-index-conflicts=true',
                '--hash-bytes', '8', '--mode', mode, '--timeout', '120s', '--output', root+'/producers/'+run+'/'+mode+'.json']
        if mode == 'query':
            args += ['--suite', 'all', '--iterations', '1600']
        scope = dict(node='client1', unit=unit, args=args, cgroup='/horizontal.slice/horizontal-client.slice/'+unit)
        if history:
            scope.update(pid=900, starttime_ticks=9000)
        return scope

    def test_scaling_plan_is_exact_typed_six_role_fixed_geometry_and_bounded_row_authorization(self):
        self.scaling_plan()
        roles = ('node1', 'node2', 'node3', 'client1', 'client2', 'client3')
        self.assertEqual(p._roles(self.plan), roles)
        self.assertEqual(p._names(self.plan)['server'], {self.plan['prefix']+'-'+role for role in roles})
        for prior in (0, 7000000, 7150000):
            p.validate_plan(dict(self.plan, prior_rows_upper=prior), self.out)
        good = copy.deepcopy(self.plan)
        for key in ('count', 'measured_count', 'measured_seconds', 'batch', 'workers', 'gomaxprocs',
                    'storage_process_count', 'storage_max_outstanding'):
            for bad in (None, True, False, str(good[key]), float(good[key]), good[key]+1):
                candidate = dict(good); candidate[key] = bad
                with self.subTest(key=key, bad=bad), self.assertRaises(p.ProvisionError):
                    p.validate_plan(candidate, self.out)
        for key in ('stages', 'count', 'measured_count', 'measured_seconds', 'prior_rows_upper', 'batch', 'workers',
                    'gomaxprocs', 'storage_process_count', 'storage_max_outstanding', 'storage_memory_profile', 'replication', 'managed_watch'):
            candidate = dict(good); del candidate[key]
            with self.subTest(missing=key), self.assertRaises(p.ProvisionError):
                p.validate_plan(candidate, self.out)
        for stages in (None, True, [], [[1, 1]], [[3, 3], [2, 2], [1, 1]], [[1, 2], [2, 2], [3, 3]],
                       [[True, 1], [2, 2], [3, 3]], [[1, 1], [2.0, 2], [3, 3]], [[1, 1], [2, 2], [3, True]],
                       [(1, 1), [2, 2], [3, 3]], ((1, 1), (2, 2), (3, 3)), [[1, 1], [2, 2], [3, 3], [3, 3]]):
            with self.subTest(stages=stages), self.assertRaises(p.ProvisionError):
                p.validate_plan(dict(good, stages=stages), self.out)
        for prior in (None, True, -1, 7150001, 7000000.0, '7000000'):
            with self.subTest(prior=prior), self.assertRaises(p.ProvisionError):
                p.validate_plan(dict(good, prior_rows_upper=prior), self.out)
        for profile in (None, True, 'baseline', 'capacity16g', 'scaling3ss3g ', 3):
            with self.subTest(profile=profile), self.assertRaises(p.ProvisionError):
                p.validate_plan(dict(good, storage_memory_profile=profile), self.out)
        for key in ('topologies', 'measured_topologies', 'selected_topologies', 'prerequisite_topologies',
                    'probe_count', 'probe_seconds', 'cases'):
            with self.subTest(forbidden=key), self.assertRaises(p.ProvisionError):
                p.validate_plan(dict(good, **{key: []}), self.out)
        self.assertFalse(self.cloud.calls)
        self.assertFalse(self.out.exists())

    def test_foreground_scaling_prepares_and_cleans_without_managed_register_or_admission(self):
        self.scaling_plan(managed_admitted=False)
        del self.plan['managed_watch']; self.plan['foreground_only'] = True
        import supervisor
        with mock.patch.object(supervisor, 'validate_config', side_effect=AssertionError('foreground consulted managed config')), \
             mock.patch.object(supervisor, 'register', side_effect=AssertionError('foreground registered managed watch')) as register, \
             mock.patch.object(supervisor, 'admit', side_effect=AssertionError('foreground requested managed admission')) as admit:
            resources = self.prepare()
            self.assertIs(resources['prepared'], True)
            self.assertEqual(len(resources['nodes']), 6)
            layout = p.setup_cluster(self.plan, self.out, resources['nodes'], 1, 'scaling-1', runner=self.cloud)
            self.assertEqual(layout['storage_process_count'], 3)
            cleanup = p.teardown(self.plan, self.out, resources, runner=self.cloud, sleep=lambda seconds: None)
            self.assertTrue(cleanup['passed'])
            register.assert_not_called(); admit.assert_not_called()

    def test_foreground_scaling_requires_exactly_one_explicit_typed_mode_before_effects(self):
        self.scaling_plan(managed_admitted=False)
        base = copy.deepcopy(self.plan); del base['managed_watch']
        candidates = [base, dict(self.plan, foreground_only=True), dict(self.plan, foreground_only=False)]
        candidates += [dict(base, foreground_only=value) for value in (False, 1, 0, 1.0, 0.0, None, 'true')]
        for value in candidates:
            self.plan.clear(); self.plan.update(value)
            with self.subTest(plan=value), self.assertRaises(p.ProvisionError):
                self.prepare()
        self.assertFalse(self.cloud.calls)
        self.assertFalse(self.cloud.create_counts)

    def test_scaling_managed_watch_configuration_is_required_and_exact_without_admission_effects(self):
        self.scaling_plan(managed_admitted=False)
        import supervisor
        good = copy.deepcopy(self.plan)
        for config in (None, True, {}, [], {'root': str(self.root/'watch')}, {'output': str(self.root/'campaign')},
                       dict(good['managed_watch'], extra=True), dict(good['managed_watch'], root='relative'),
                       dict(good['managed_watch'], output='relative')):
            changed = dict(good, managed_watch=config)
            with self.subTest(config=config), self.assertRaises(p.ProvisionError):
                p.validate_plan(changed, self.out)
        with mock.patch.object(supervisor, 'admit', side_effect=AssertionError('pure validation cannot admit')):
            p.validate_plan(good, self.out)
        self.assertFalse(self.cloud.calls)
        self.assertFalse(self.out.exists())

    def test_scaling_prepare_requires_managed_admission_before_even_inventory_or_guard_start(self):
        self.scaling_plan(managed_admitted=False)
        import supervisor
        for reason in ('controller and watcher share runtime cgroup', 'unknown supervisor unit or process identity'):
            with self.subTest(reason=reason), mock.patch.object(supervisor, 'admit', side_effect=ValueError(reason)) as admit:
                with self.assertRaisesRegex(p.ProvisionError, reason):
                    self.prepare()
                admit.assert_called_once()
                self.assertEqual(admit.call_args.args, (self.plan, self.out))
                self.assertIs(admit.call_args.kwargs['clock'], time.time)
                self.assertFalse(self.cloud.calls)
                self.assertFalse(self.cloud.events)
                self.assertFalse(self.cloud.create_counts)
                self.assertFalse(self.out.exists())

    def test_scaling_prepare_actual_admission_rejects_unknown_supervisor_before_any_cloud_call(self):
        self.scaling_plan(managed_admitted=False)
        import supervisor
        actual_admit = supervisor.admit
        proc_root = self.root / 'unknown-proc'
        proc_root.mkdir(mode=0o700)
        def fake_manager(argv, **kwargs):
            return 1, '', 'unknown managed USER supervisor'
        def admit(plan, out, *, clock):
            return actual_admit(plan, out, runner=fake_manager, clock=clock, proc_root=proc_root)
        with mock.patch.object(supervisor, 'admit', side_effect=admit) as invoked:
            with self.assertRaisesRegex(p.ProvisionError, 'managed supervisor admission refused'):
                self.prepare()
            invoked.assert_called_once()
        self.assertFalse(self.cloud.calls)
        self.assertFalse(self.cloud.events)
        self.assertFalse(self.cloud.create_counts)
        self.assertFalse(self.out.exists())

    def test_scaling_prepare_refuses_output_outside_exact_managed_run_before_admission(self):
        self.scaling_plan(managed_admitted=False)
        import supervisor
        with mock.patch.object(supervisor, 'admit', side_effect=AssertionError('mismatched output must fail first')):
            for out in (self.root/'other', Path(self.plan['managed_watch']['output']), self.out/'provision'):
                with self.subTest(out=out), self.assertRaisesRegex(p.ProvisionError, 'output does not match'):
                    p.prepare(self.plan, out, runner=self.cloud, popen=self.cloud.popen)
        self.assertFalse(self.cloud.calls)
        self.assertFalse(self.cloud.events)

    def test_scaling_managed_admission_is_rechecked_before_create_attach_poweron_ssh_and_copy(self):
        self.scaling_plan(); resources = self.prepare(); state = p._state(self.out)
        server = next(item for item in self.cloud.items['server'] if item['id'] == resources['nodes']['node1']['id'])
        network = next(item for item in self.cloud.items['network'] if item['id'] == resources['ids']['network'][0])
        guard = mock.Mock(); guard.poll.return_value = None
        before = len(self.cloud.calls)
        self.managed_admit.reset_mock()
        self.managed_admit.side_effect = ValueError('managed supervisor identity changed')
        operations = {
            'create': lambda: p._create(self.plan, state, resources, 'server',
                [self.plan['hcloud'], 'server', 'create', '--name', self.plan['prefix']+'-node1'],
                self.cloud, time.time, guard),
            'attach': lambda: p._attach_network(self.plan, state, resources, server, network, server['private_net'][0]['ip'],
                self.cloud, time.time, guard, lambda _: None),
            'poweron': lambda: p._mutate(self.plan, state,
                [self.plan['hcloud'], 'server', 'poweron', str(server['id'])], self.cloud, time.time, guard),
            'ssh': lambda: p.run_on_node(self.plan, self.out, resources['nodes']['client3'], ['true'], runner=self.cloud),
            'copy': lambda: p.copy_to_node(self.plan, self.out, resources['nodes']['client3'], self.plan['binary'],
                p.REMOTE_BASE+'/'+self.plan['campaign']+'/bin/otel-demo', runner=self.cloud),
        }
        for name, operation in operations.items():
            with self.subTest(operation=name), self.assertRaisesRegex(p.ProvisionError, 'supervisor identity changed'):
                operation()
            self.assertEqual(len(self.cloud.calls), before)
            self.assertFalse(self.cloud.deleted)
        self.assertEqual(self.managed_admit.call_count, len(operations))
        self.assertTrue(all(call.args == (self.plan, self.out) for call in self.managed_admit.call_args_list))

    def test_scaling_admission_loss_during_prepare_prevents_next_mutation_and_still_cleans_partial_resources(self):
        self.scaling_plan()
        for phase in ('first-create', 'attach', 'poweron'):
            output = self.root / ('managed-'+phase)
            self.out = output / 'provision'
            self.plan['managed_watch']['output'] = str(output)
            self.cloud = FakeCloud(self.plan, self.out)
            def admit(plan, out, *, clock):
                self.assertEqual(Path(out), self.out)
                reached = {'first-create': 'keygen' in self.cloud.events,
                           'attach': self.cloud.create_counts.get('server', 0) == 1,
                           'poweron': any(call['path'].startswith('/servers/') and call['method'] == 'GET'
                                          for call in self.cloud.attach_api_calls)}[phase]
                if reached:
                    raise ValueError('managed admission lost before '+phase)
                return {'admitted': True}
            self.managed_admit.side_effect = admit
            with self.subTest(phase=phase), self.assertRaisesRegex(p.ProvisionError, 'admission lost before '+phase):
                self.prepare()
            self.assertTrue(p._load(p._state(self.out)/'teardown-complete.json')['passed'])
            self.assertEqual(self.cloud.items, self.cloud.baseline)
            self.assertNotIn('server poweron', self.cloud.events)
            if phase == 'first-create':
                self.assertFalse(self.cloud.create_counts)
                self.assertFalse(self.cloud.attach_api_calls)
                self.assertFalse(self.cloud.deleted)
            elif phase == 'attach':
                self.assertEqual(self.cloud.create_counts['server'], 1)
                self.assertFalse(self.cloud.attach_api_calls)
                self.assertTrue(self.cloud.deleted)
            else:
                self.assertEqual(self.cloud.create_counts['server'], 1)
                self.assertEqual(sum(call['method'] == 'POST' for call in self.cloud.attach_api_calls), 1)
                self.assertTrue(self.cloud.deleted)

    def test_scaling_immutable_lease_refusal_precedes_managed_admission_and_teardown_bypasses_it(self):
        self.scaling_plan(); resources = self.prepare(); state = p._state(self.out)
        self.managed_admit.reset_mock()
        self.managed_admit.side_effect = ValueError('managed supervisor admission lost')
        changed = copy.deepcopy(self.plan); changed['managed_watch']['root'] = str(self.root/'different-watch')
        with self.assertRaisesRegex(p.ProvisionError, 'immutable scaling lease'):
            p._check_window(changed, state, time.time)
        self.managed_admit.assert_not_called()
        with self.assertRaisesRegex(p.ProvisionError, 'admission lost'):
            p._check_window(self.plan, state, time.time)
        self.assertEqual(self.managed_admit.call_count, 1)
        cleanup = p.teardown(self.plan, self.out, resources, runner=self.cloud, sleep=lambda _: None)
        self.assertTrue(cleanup['passed'])
        self.assertEqual(self.cloud.items, self.cloud.baseline)
        self.assertEqual(self.managed_admit.call_count, 1)

    def test_legacy_prepare_does_not_require_or_consult_managed_supervisor(self):
        import supervisor
        with (mock.patch.object(supervisor, 'validate_config', side_effect=AssertionError('legacy config changed')),
              mock.patch.object(supervisor, 'admit', side_effect=AssertionError('legacy admission changed'))):
            resources = self.prepare()
        self.assertTrue(resources['prepared'])
        self.assertEqual(set(resources['nodes']), set(p.ROLES))

    def test_scaling_render_has_three_native_storage_processes_per_active_host_and_unchanged_controls(self):
        self.scaling_plan(); nodes = self.scaling_nodes()
        original = copy.deepcopy(self.plan)
        for stage in (1, 2, 3):
            armid = 'scaling-'+str(stage)
            layout = p.render_cluster(self.plan, nodes, stage, armid)
            self.assertEqual(layout, p.render_cluster(self.plan, nodes, ['node'+str(i) for i in range(1, stage+1)], armid))
            self.assertEqual(layout['schema'], 'otel_native_scaling_v1')
            self.assertEqual(layout['storage_memory_profile'], 'scaling3ss3g')
            self.assertEqual(layout['storage_process_count'], 3)
            self.assertEqual(layout['storage_max_outstanding'], 64)
            self.assertEqual(layout['storage_hosts'], ['node'+str(i) for i in range(1, stage+1)])
            self.assertEqual(len(layout['processes']), 5+3*stage)
            controls = [x for x in layout['processes'] if x['process_class'] != 'storage']
            legacy = dict(self.plan, schema='otel_horizontal_v1')
            for key in ('storage_process_count', 'storage_max_outstanding', 'storage_memory_profile'):
                del legacy[key]
            old_controls = [x for x in p.render_cluster(legacy, nodes, stage, armid)['processes'] if x['process_class'] != 'storage']
            self.assertEqual(controls, old_controls)
            self.assertEqual([x['port'] for x in controls], [4500, 4501, 4502, 4503, 4504])
            self.assertEqual({x['node'] for x in controls}, {'node1'})
            self.assertEqual([x['memory_limit_bytes'] for x in controls], [p.GIB]*5)
            storage = [x for x in layout['processes'] if x['process_class'] == 'storage']
            self.assertEqual(len(storage), 3*stage)
            for index in range(1, 4):
                role = 'node'+str(index)
                items = [x for x in layout['processes'] if x['node'] == role]
                self.assertEqual(sum(x['memory_gib'] for x in items), (14 if index == 1 else 9) if index <= stage else 0)
                self.assertEqual([x['port'] for x in storage if x['node'] == role], [4510, 4511, 4512] if index <= stage else [])
            for item in storage:
                self.assertEqual(item['args'][item['args'].index('--memory')+1], '3GiB')
                self.assertEqual(item['args'][item['args'].index('--cache-memory')+1], '1GiB')
                self.assertEqual(item['memory_limit_bytes'], 3*p.GIB)
                self.assertEqual(item['cache_mib'], 1024)
                self.assertEqual(item['machineid'], item['node']); self.assertEqual(item['zoneid'], item['node'])
                self.assertNotIn('--knob_max_outstanding', item['args'])
                self.assertIn('ExecStart='+shlex.join(item['args'])+'\n', item['unit'])
            self.assertEqual(layout['budgets']['fdb_host'], dict(cpu_quota_percent=400, allowed_cpus='0-3',
                             memory_max_gib=16, max_process_memory_gib=14, max_processes=8))
            self.assertEqual(layout['budgets']['client_host'], dict(cpu_quota_percent=400, allowed_cpus='0-3', memory_max_gib=16))
        self.assertEqual(self.plan, original)
        self.assertFalse(self.cloud.calls)

    def test_scaling_log_memory_flag_changes_only_log_4504_memory_and_node1_budget(self):
        self.scaling_log_plan(); nodes = self.scaling_nodes()
        default = dict(self.plan, stages=[[1, 1], [2, 2], [3, 3]], prior_rows_upper=7000000)
        del default['log_memory_gib']
        for stage in (2, 1):
            armid = 'scaling-'+str(stage)
            baseline = p.render_cluster(default, nodes, stage, armid)
            changed = p.render_cluster(self.plan, nodes, stage, armid)
            self.assertNotIn('log_memory_gib', baseline)
            self.assertEqual(changed['log_memory_gib'], 2)
            self.assertEqual(changed['budgets']['fdb_host']['max_process_memory_gib'], 15)
            self.assertEqual(sum(item['memory_gib'] for item in changed['processes'] if item['node'] == 'node1'), 15)
            self.assertEqual(sum(item['memory_gib'] for item in changed['processes'] if item['node'] == 'node2'), 9 if stage == 2 else 0)
            self.assertEqual(len(changed['processes']), 5+3*stage)
            normalized = copy.deepcopy(changed)
            del normalized['log_memory_gib']
            normalized['budgets']['fdb_host']['max_process_memory_gib'] = 14
            log_changes = 0
            for index, (new, old) in enumerate(zip(changed['processes'], baseline['processes'])):
                if new['port'] != 4504:
                    self.assertEqual(new, old)
                    continue
                log_changes += 1
                self.assertEqual(new['node'], 'node1')
                self.assertEqual(new['process_class'], 'transaction')
                self.assertEqual(new['required_roles'], ['log'])
                expected = copy.deepcopy(old)
                self.assertEqual(expected['args'][expected['args'].index('--memory')+1], '1GiB')
                expected['args'][expected['args'].index('--memory')+1] = '2GiB'
                expected['unit'] = expected['unit'].replace('ExecStart='+shlex.join(old['args'])+'\n',
                                                          'ExecStart='+shlex.join(expected['args'])+'\n')
                expected.update(memory_gib=2, memory_limit_bytes=2*p.GIB)
                self.assertEqual(new, expected)
                self.assertEqual(new['cache_mib'], 64)
                self.assertEqual(new['args'][new['args'].index('--cache-memory')+1], '64MiB')
                normalized['processes'][index] = old
            self.assertEqual(log_changes, 1)
            self.assertEqual(normalized, baseline)
        self.assertFalse(self.cloud.calls)

    def test_scaling_log_memory_flag_requires_typed_exact_foreground_2_then_1_and_exact_prior(self):
        self.scaling_log_plan()
        p.validate_plan(self.plan, self.out)
        import policy
        self.assertEqual(policy.scaling_stage_pairs(self.plan), [[2, 2], [1, 1]])
        self.assertEqual(policy.scaling_authorized_rows(self.plan), 1425000)
        self.assertEqual(self.plan['prior_rows_upper']+policy.scaling_authorized_rows(self.plan), 9850000)
        good = copy.deepcopy(self.plan)
        for value in (None, True, False, 0, 1, 3, 2.0, '2', [], {}):
            with self.subTest(log_memory_gib=value), self.assertRaises(p.ProvisionError):
                p.validate_plan(dict(good, log_memory_gib=value), self.out)
        for stages in (None, [], [[1, 1], [2, 2]], [[2, 2]], [[1, 1], [2, 2], [3, 3]],
                       [[2, 2], [1, True]], [[2.0, 2], [1, 1]], [(2, 2), [1, 1]], ((2, 2), (1, 1))):
            with self.subTest(stages=stages), self.assertRaises(p.ProvisionError):
                p.validate_plan(dict(good, stages=stages), self.out)
        for prior in (None, True, -1, 0, 7000000, 8424999, 8425001, 8575001, 10000000, 8425000.0, '8425000'):
            with self.subTest(prior=prior), self.assertRaises(p.ProvisionError):
                p.validate_plan(dict(good, prior_rows_upper=prior), self.out)
        for foreground in (None, False, 1, 1.0, 'true'):
            with self.subTest(foreground_only=foreground), self.assertRaises(p.ProvisionError):
                p.validate_plan(dict(good, foreground_only=foreground), self.out)
        managed = dict(good, managed_watch=dict(root=str(self.root/'watch'), output=str(self.root/'campaign')))
        del managed['foreground_only']
        with self.assertRaises(p.ProvisionError):
            p.validate_plan(managed, self.out)
        missing_mode = dict(good); del missing_mode['foreground_only']
        with self.assertRaises(p.ProvisionError):
            p.validate_plan(missing_mode, self.out)
        default = dict(good); del default['log_memory_gib']
        with self.assertRaises(p.ProvisionError):
            p.validate_plan(default, self.out)
        nodes = self.scaling_nodes()
        for hosts, armid in ((3, 'scaling-3'), (2, 'scaling-1'), (1, 'scaling-2')):
            with self.subTest(hosts=hosts, armid=armid), self.assertRaises(p.ProvisionError):
                p.render_cluster(good, nodes, hosts, armid)
        self.assertFalse(self.cloud.calls)
        self.assertFalse(self.out.exists())

    def test_log_memory_flag_is_never_a_legacy_schema_override_even_at_one_gib(self):
        original = copy.deepcopy(self.plan)
        for schema in ('otel_horizontal_v1', 'otel_native_diagnostic_v1', 'otel_native_session_v1'):
            for value in (None, True, 1, 2, 2.0):
                changed = dict(original, schema=schema, log_memory_gib=value)
                with self.subTest(schema=schema, value=value):
                    with self.assertRaisesRegex(p.ProvisionError, 'scaling-only'):
                        p.prepare(changed, self.out, runner=self.cloud, popen=self.cloud.popen)
                    with self.assertRaisesRegex(p.ProvisionError, 'scaling-only'):
                        p.render_cluster(changed, self.scaling_nodes(), 1, 'legacy')
        self.assertFalse(self.cloud.calls)
        self.assertFalse(self.cloud.events)

    def test_scaling_log_two_gib_prepares_once_sets_up_2_then_1_and_stop_keeps_native_guards(self):
        self.scaling_log_plan(); resources = self.prepare(); nodes = resources['nodes']; state = p._state(self.out)
        lease = (state/'guard-plan.json').read_bytes()
        prior_runner = self.cloud
        for stage in (2, 1):
            layout = p.setup_cluster(self.plan, self.out, nodes, stage, 'scaling-'+str(stage), runner=prior_runner)
            self.assertEqual(p._load(state/'active-arm.json'), self.scaling_journal(layout, stage))
            self.assertEqual(layout['log_memory_gib'], 2)
            self.assertEqual(layout['budgets']['fdb_host']['max_process_memory_gib'], 15)
            self.assertEqual(layout['budgets']['fdb_host']['memory_max_gib'], 16)
            self.assertEqual(layout['budgets']['fdb_host']['cpu_quota_percent'], 400)
            self.assertEqual(layout['budgets']['fdb_host']['allowed_cpus'], '0-3')
            self.assertEqual(layout['storage_max_outstanding'], 64)
            self.assertEqual(layout['storage_process_count'], 3)
            self.assertEqual(layout['storage_memory_profile'], 'scaling3ss3g')
            for item in layout['processes']:
                emitted = Path(layout['cluster_file']).parent/(item['node']+'-'+item['unit_name'])
                self.assertEqual(emitted.read_bytes(), item['unit'].encode())
                if item['port'] == 4504:
                    self.assertEqual(item['memory_limit_bytes'], 2*p.GIB)
                elif item['process_class'] == 'storage':
                    self.assertEqual(item['memory_limit_bytes'], 3*p.GIB)
                    self.assertEqual(item['cache_mib'], 1024)
                else:
                    self.assertEqual(item['memory_limit_bytes'], p.GIB)
            scopes = tuple(self.scaling_scope(layout, client) for client in range(1, stage+1))
            runner = StopRunner(self.cloud, nodes, layout, scopes)
            receipt = p.stop_owned_arm(self.plan, self.out, nodes, layout, scopes=scopes, runner=runner)
            self.assertTrue(receipt['passed']); self.assertEqual(set(receipt['before']), set(nodes))
            self.assertEqual((state/'guard-plan.json').read_bytes(), lease)
            prior_runner = runner
        self.assertEqual(self.cloud.create_counts['server'], 6)
        self.assertEqual(self.cloud.events.count('guard-start'), 1)
        for kind in ('fdb', 'client'):
            text = (state/('horizontal-'+kind+'.slice')).read_text()
            self.assertIn('MemoryMax=16G\nMemorySwapMax=0\n', text)
            self.assertIn('IOAccounting=yes\n', text)
        cleanup = p.teardown(self.plan, self.out, resources, runner=self.cloud, sleep=lambda _: None)
        self.assertTrue(cleanup['passed']); self.assertEqual(self.cloud.items, self.cloud.baseline)

    def test_scaling_render_rejects_partial_nodes_nonprefix_and_cross_stage_ids_without_effects(self):
        self.scaling_plan(); nodes = self.scaling_nodes()
        for role in p._roles(self.plan):
            partial = dict(nodes); del partial[role]
            with self.subTest(missing=role), self.assertRaises(p.ProvisionError):
                p.render_cluster(self.plan, partial, 1, 'scaling-1')
        for hosts in (True, False, None, 0, 4, 1.0, [], ['node2'], ['node1', 'node3'], ['node1', 'node1'], ['node1', 'client1']):
            with self.subTest(hosts=hosts), self.assertRaises(p.ProvisionError):
                p.render_cluster(self.plan, nodes, hosts, 'scaling-1')
        for stage in (1, 2, 3):
            for armid in ('scaling-0', 'scaling-4', 'stage-'+str(stage), 'scaling-'+str(4-stage), None, True, '../bad'):
                if armid == 'scaling-'+str(stage):
                    continue
                with self.subTest(stage=stage, armid=armid), self.assertRaises(p.ProvisionError):
                    p.render_cluster(self.plan, nodes, stage, armid)
        for key in ('count', 'stages', 'storage_process_count', 'storage_max_outstanding', 'storage_memory_profile'):
            invalid = dict(self.plan); del invalid[key]
            with self.subTest(missing=key), self.assertRaises(p.ProvisionError):
                p.render_cluster(invalid, nodes, 1, 'scaling-1')
        self.assertFalse(self.cloud.calls)

    def test_scaling_prepares_all_six_once_installs_client3_assets_and_keeps_one_immutable_stage_lease(self):
        self.scaling_plan(); resources = self.prepare(); nodes = resources['nodes']; state = p._state(self.out)
        lease = (state/'guard-plan.json').read_bytes()
        baseline = {kind: (state/'baseline'/('before-'+kind+'.json')).read_bytes() for kind in p.KINDS}
        self.assertEqual(set(nodes), set(p._roles(self.plan)))
        self.assertEqual(self.cloud.create_counts['server'], 6)
        self.assertEqual(self.cloud.events.count('guard-start'), 1)
        client = nodes['client3']
        self.assertEqual(client['private_ip'], '10.93.0.23')
        self.assertEqual(client['slice'], 'horizontal-client.slice')
        self.assertTrue((state/'client3-native.json').is_file())
        self.assertTrue((state/'client3-readiness-1.json').is_file())
        for kind in ('fdb', 'client'):
            text = (state/('horizontal-'+kind+'.slice')).read_text()
            self.assertEqual(text.encode(), legacy_slice_bytes(kind).replace(b'AllowedCPUs', b'IOAccounting=yes\nAllowedCPUs'))
        client_copies = [argv for argv, _ in self.cloud.calls if argv[0] == 'scp' and argv[-1].startswith('root@'+client['host']+':')]
        for asset, destination in (('binary', 'otel-demo'), ('fdbserver', 'fdbserver'), ('fdbcli', 'fdbcli')):
            matching = [argv for argv in client_copies if argv[-1].endswith('/bin/'+destination)]
            self.assertEqual(len(matching), 1)
            self.assertEqual(matching[0][-2], self.plan[asset])
            self.assertIn('StrictHostKeyChecking=yes', matching[0])
            self.assertIn('HostKeyAlias='+client['name'], matching[0])
        prior_runner = self.cloud
        for stage in (1, 2, 3):
            prior_stops = list(prior_runner.stop_commands) if stage > 1 else []
            layout = p.setup_cluster(self.plan, self.out, nodes, stage, 'scaling-'+str(stage), runner=prior_runner)
            if stage > 1:
                self.assertEqual(prior_runner.stop_commands, prior_stops)
            active = p._load(state/'active-arm.json')
            self.assertEqual(active, self.scaling_journal(layout, stage))
            self.assertEqual({key for key, values in active['units'].items() if values}, {'node'+str(i) for i in range(1, stage+1)})
            scopes = tuple(self.scaling_scope(layout, client) for client in range(1, stage+1))
            runner = StopRunner(self.cloud, nodes, layout, scopes)
            arm = Path(layout['cluster_file']).parent
            before = {path.name: path.read_bytes() for path in arm.iterdir() if path.is_file()}
            receipt = p.stop_owned_arm(self.plan, self.out, nodes, layout, scopes=scopes, runner=runner)
            prior_runner = runner
            self.assertTrue(receipt['passed']); self.assertTrue(receipt['quiescent'])
            self.assertEqual(set(receipt['before']), set(nodes)); self.assertEqual(set(receipt['after']), set(nodes))
            self.assertEqual({role for role, _ in runner.stop_commands},
                             {'node'+str(i) for i in range(1, stage+1)} | {'client'+str(i) for i in range(1, stage+1)})
            self.assertEqual(sum(len(units) for _, units in runner.stop_commands), 5+4*stage)
            self.assertEqual({path.name: path.read_bytes() for path in arm.iterdir() if path.is_file()}, before)
            self.assertEqual(p._load(state/'active-arm.json'), active)
            self.assertEqual((state/'guard-plan.json').read_bytes(), lease)
            self.assertFalse((state/'cleanup-started.json').exists())
        self.assertEqual(self.cloud.create_counts['server'], 6)
        self.assertEqual(self.cloud.events.count('guard-start'), 1)
        self.assertFalse(self.cloud.deleted)
        self.assertEqual({kind: (state/'baseline'/('before-'+kind+'.json')).read_bytes() for kind in p.KINDS}, baseline)
        result = p.teardown(self.plan, self.out, resources, runner=self.cloud, sleep=lambda _: None)
        self.assertTrue(result['passed']); self.assertEqual(self.cloud.items, self.cloud.baseline)

    def test_scaling_runtime_rejects_any_lease_mutation_schema_projection_or_role_alias_before_effects(self):
        self.scaling_plan(); resources = self.prepare(); nodes = resources['nodes']; state = p._state(self.out)
        original = (state/'guard-plan.json').read_bytes()
        for key, value in (('schema', 'otel_native_diagnostic_v1'), ('stages', [[3, 3], [2, 2], [1, 1]]),
                           ('first_epoch', self.plan['first_epoch']+1), ('deadline_epoch', self.plan['deadline_epoch']+1),
                           ('prior_rows_upper', 0), ('measured_count', 100000), ('workers', 4),
                           ('stages', [(1, 1), (2, 2), (3, 3)]), ('batch', 64.0),
                           ('storage_process_count', 2), ('storage_memory_profile', 'capacity16g'), ('unapproved_extra', True)):
            changed = dict(self.plan); changed[key] = value
            before = len(self.cloud.calls)
            with self.subTest(key=key), self.assertRaises(p.ProvisionError):
                p.setup_cluster(changed, self.out, nodes, 1, 'scaling-1', runner=self.cloud)
            self.assertEqual(len(self.cloud.calls), before)
        reordered = dict(reversed(list(self.plan.items())))
        p._check_window(reordered, state, time.time)
        aliased = dict(nodes); aliased['client2'], aliased['client3'] = aliased['client3'], aliased['client2']
        before = len(self.cloud.calls)
        with self.assertRaises(p.ProvisionError):
            p.setup_cluster(self.plan, self.out, aliased, 1, 'scaling-1', runner=self.cloud)
        self.assertEqual(len(self.cloud.calls), before)
        self.assertEqual((state/'guard-plan.json').read_bytes(), original)
        (state/'guard-plan.json').unlink()
        with self.assertRaisesRegex(p.ProvisionError, 'lease is missing'):
            p._check_window(self.plan, state, time.time)

    def test_scaling_next_setup_reproves_prior_main_pid_identity_before_any_stop_or_new_start(self):
        self.scaling_plan(); nodes = self.prepare()['nodes']; state = p._state(self.out)
        layout = p.setup_cluster(self.plan, self.out, nodes, 1, 'scaling-1', runner=self.cloud)
        runner = StopRunner(self.cloud, nodes, layout)
        stopped = p.stop_owned_arm(self.plan, self.out, nodes, layout, runner=runner)
        self.assertTrue(stopped['passed'])
        active_before = (state/'active-arm.json').read_bytes()
        runner.stop_commands.clear()
        # The prior quiet proof cannot authorize a later changed MainPID.
        unit = layout['processes'][0]['unit_name']
        runner.stopped.remove(('node1', unit))
        runner.records[('node1', unit)].update(pid=999, starttime_ticks=99999,
                                             args=['/foreign/process'], exe='/foreign/process')
        before = len(self.cloud.calls)
        with self.assertRaisesRegex(p.ProvisionError, 'full command/cgroup'):
            p.setup_cluster(self.plan, self.out, nodes, 2, 'scaling-2', runner=runner)
        self.assertFalse(runner.stop_commands)
        self.assertFalse(any(argv[0] == 'ssh' and shlex.split(argv[-1])[:2] == ['systemctl', 'start']
                             for argv, _ in self.cloud.calls[before:]))
        self.assertEqual((state/'active-arm.json').read_bytes(), active_before)
        self.assertTrue((state/'cleanup-started.json').exists())
        self.assertFalse(self.cloud.deleted)

    def test_scaling_next_setup_refuses_unmapped_prior_application_residual_without_stopping_it(self):
        self.scaling_plan(); nodes = self.prepare()['nodes']; state = p._state(self.out)
        layout = p.setup_cluster(self.plan, self.out, nodes, 1, 'scaling-1', runner=self.cloud)
        scope = self.scaling_scope(layout, 1)
        runner = StopRunner(self.cloud, nodes, layout, (scope,))
        with self.assertRaisesRegex(p.ProvisionError, 'unmapped owned'):
            p.setup_cluster(self.plan, self.out, nodes, 2, 'scaling-2', runner=runner)
        self.assertFalse(runner.stop_commands)
        self.assertEqual(p._load(state/'active-arm.json')['armid'], 'scaling-1')
        self.assertTrue((state/'cleanup-started.json').exists())

    def test_scaling_stop_specs_derive_exact_stage_journal_and_only_active_client_scopes(self):
        self.scaling_plan(); nodes = self.scaling_nodes()
        for stage in (1, 2, 3):
            layout = p.render_cluster(self.plan, nodes, stage, 'scaling-'+str(stage))
            active = self.scaling_journal(layout, stage)
            scopes = tuple(self.scaling_scope(layout, client, mode, False) for client in range(1, stage+1)
                           for mode in ('ingest', 'audit', 'query'))
            specs = p._stop_specs(self.plan, nodes, layout, active, scopes)
            self.assertEqual(set(specs), set(nodes))
            for index in range(1, 4):
                self.assertEqual(len(specs['node'+str(index)]), (8 if index == 1 else 3) if index <= stage else 0)
                self.assertEqual(len(specs['client'+str(index)]), 3 if index <= stage else 0)
            for role in nodes:
                changed = copy.deepcopy(active); del changed['units'][role]
                with self.subTest(stage=stage, missing=role), self.assertRaises(p.ProvisionError):
                    p._stop_specs(self.plan, nodes, layout, changed, ())
            for role in nodes:
                changed = copy.deepcopy(active); changed['units'][role].append('protected.service')
                with self.subTest(stage=stage, extra=role), self.assertRaises(p.ProvisionError):
                    p._stop_specs(self.plan, nodes, layout, changed, ())
            changed = copy.deepcopy(layout); changed['processes'][0]['args'].append('--foreign')
            with self.assertRaises(p.ProvisionError):
                p._stop_specs(self.plan, nodes, changed, active, ())
            for key, bad in (('schema', None), ('storage_hosts', ['node2']), ('storage_process_count', 2),
                             ('storage_memory_profile', 'capacity16g'), ('root', '/foreign'), ('remote_cluster_file', '/foreign')):
                changed = dict(layout); changed[key] = bad
                with self.subTest(stage=stage, key=key), self.assertRaises(p.ProvisionError):
                    p._stop_specs(self.plan, nodes, changed, active, ())
            scope = self.scaling_scope(layout, 1)
            for bad in (None, True, {}, [scope, scope], [dict(scope, node='node1')], [dict(scope, unit='foreign.scope')],
                        [dict(scope, cgroup='/foreign')], [dict(scope, pid=True)], [dict(scope, starttime_ticks=-1)]):
                with self.subTest(stage=stage, scopes=bad), self.assertRaises(p.ProvisionError):
                    p._stop_specs(self.plan, nodes, layout, active, bad)
            if stage < 3:
                with self.assertRaises(p.ProvisionError):
                    p._stop_specs(self.plan, nodes, layout, active, (self.scaling_scope(layout, stage+1),))
        self.assertFalse(self.cloud.calls)

    def test_scaling_stop_rejects_idle_host_residual_vacuity_before_any_stop_and_arms_cleanup(self):
        self.scaling_plan(); nodes = self.prepare()['nodes']
        layout = p.setup_cluster(self.plan, self.out, nodes, 1, 'scaling-1', runner=self.cloud)
        runner = StopRunner(self.cloud, nodes, layout)
        def residual(role, phase, snapshot):
            if role == 'client3':
                self.assertEqual(snapshot['units'], [])
                snapshot['owned_residual'].append(dict(pid=999, args=['foreign']))
        runner.mutate = residual
        with self.assertRaisesRegex(p.ProvisionError, 'unmapped owned'):
            p.stop_owned_arm(self.plan, self.out, nodes, layout, runner=runner)
        self.assertFalse(runner.stop_commands)
        self.assertTrue((p._state(self.out)/'cleanup-started.json').is_file())
        self.assertFalse(self.cloud.deleted)

    def test_scaling_stop_requires_every_active_pid_command_cgroup_and_final_absence(self):
        self.scaling_plan(); nodes = self.prepare()['nodes']
        layout = p.setup_cluster(self.plan, self.out, nodes, 3, 'scaling-3', runner=self.cloud)
        scopes = tuple(self.scaling_scope(layout, client) for client in (1, 2, 3))
        specs = p._stop_specs(self.plan, nodes, layout, self.scaling_journal(layout, 3), scopes)
        for role in nodes:
            spec = specs[role][0]
            value = dict(pid=1, starttime_ticks=100, args=spec['args'], exe=spec['args'][0], cgroup=spec['cgroup'])
            snapshot = dict(units=[dict(unit=spec['unit'], load_state='loaded', active_state='active',
                            control_group=spec['cgroup'], main_pid=1, main=value, pids=[value])],
                            historical={}, owned_residual=[value])
            self.assertTrue(p._validate_stop_snapshot(snapshot, [spec], {})[0])
            self.assertFalse(p._validate_stop_snapshot(snapshot, [spec], {}, stopped=True)[0])
            for empty in (dict(units=[], historical={}, owned_residual=[]),
                          dict(snapshot, owned_residual=[])):
                with self.subTest(role=role, snapshot=empty), self.assertRaises(p.ProvisionError):
                    p._validate_stop_snapshot(empty, [spec], {})
            for key, bad in (('pid', True), ('starttime_ticks', 0), ('args', ['/foreign']), ('exe', '/foreign'), ('cgroup', '/foreign')):
                changed = copy.deepcopy(snapshot)
                for process in (changed['units'][0]['main'], changed['units'][0]['pids'][0], changed['owned_residual'][0]):
                    process[key] = bad
                with self.subTest(role=role, key=key), self.assertRaises(p.ProvisionError):
                    p._validate_stop_snapshot(changed, [spec], {})
        scope = scopes[2]; runner = StopRunner(self.cloud, nodes, layout, scopes)
        runner.records[('client3', scope['unit'])]['cgroup'] = '/foreign'
        with self.assertRaisesRegex(p.ProvisionError, 'full command/cgroup'):
            p.stop_owned_arm(self.plan, self.out, nodes, layout, scopes=scopes, runner=runner)
        self.assertFalse(runner.stop_commands)

    def test_scaling_stop_obeys_operation_budget_absolute_deadline_and_cannot_renew_lease(self):
        self.scaling_plan(); nodes = self.prepare()['nodes']; state = p._state(self.out)
        layout = p.setup_cluster(self.plan, self.out, nodes, 3, 'scaling-3', runner=self.cloud)
        lease = (state/'guard-plan.json').read_bytes()
        runner = StopRunner(self.cloud, nodes, layout); runner.linger = True
        now = [float(self.plan['first_epoch']+5995)]
        def sleep(seconds): now[0] += seconds
        with self.assertRaises(p.ProvisionError):
            p.stop_owned_arm(self.plan, self.out, nodes, layout, runner=runner, clock=lambda: now[0], sleep=sleep)
        self.assertLessEqual(now[0], self.plan['first_epoch']+6000)
        self.assertEqual((state/'guard-plan.json').read_bytes(), lease)
        self.assertTrue((state/'cleanup-started.json').exists())
        before = len(self.cloud.calls)
        with self.assertRaises(p.ProvisionError):
            p.setup_cluster(self.plan, self.out, nodes, 1, 'scaling-1', runner=self.cloud)
        self.assertEqual(len(self.cloud.calls), before)
        self.assertFalse(self.cloud.deleted)

    def test_scaling_stop_operation_budget_cannot_expire_during_all_six_before_proofs_then_stop(self):
        self.scaling_plan(); nodes = self.prepare()['nodes']
        layout = p.setup_cluster(self.plan, self.out, nodes, 1, 'scaling-1', runner=self.cloud)
        runner = StopRunner(self.cloud, nodes, layout); now = [float(self.plan['first_epoch']+1)]
        def elapsed(role, phase, snapshot):
            if phase == 'before' and role == 'client3':
                now[0] += 121
        runner.mutate = elapsed
        with self.assertRaisesRegex(p.ProvisionError, 'operation budget'):
            p.stop_owned_arm(self.plan, self.out, nodes, layout, runner=runner, clock=lambda: now[0])
        self.assertFalse(runner.stop_commands)
        self.assertTrue((p._state(self.out)/'cleanup-started.json').exists())

    def test_session_plan_has_two_immutable_roles_cases_rows_and_original_deadline(self):
        self.session_plan()
        self.assertEqual(p._roles(self.plan), ('node1', 'client1'))
        for prior in (0, 7800000):
            self.plan['prior_rows_upper'] = prior
            p.validate_plan(self.plan, self.out)
        base = copy.deepcopy(self.plan)
        bads = [('cases', ['baseline', 'capacity16g']), ('cases', ['capacity16g']), ('cases', True),
                ('prior_rows_upper', True), ('prior_rows_upper', -1), ('prior_rows_upper', 7800001),
                ('prior_rows_upper', 0.0), ('storage_process_count', 1), ('storage_process_count', True),
                ('storage_max_outstanding', 128), ('storage_max_outstanding', 64.0),
                ('storage_memory_profile', 'baseline')]
        for key, bad in bads:
            self.plan.clear(); self.plan.update(base); self.plan[key] = bad
            with self.subTest(key=key, value=bad), self.assertRaises(p.ProvisionError):
                self.prepare()
        self.plan.clear(); self.plan.update(base)
        for key in ('cases', 'prior_rows_upper', 'storage_process_count', 'storage_max_outstanding'):
            changed = dict(base); del changed[key]
            with self.subTest(missing=key), self.assertRaises(p.ProvisionError):
                p.validate_plan(changed, self.out)
        self.assertFalse(self.cloud.calls)

    def test_capacity_profile_changes_only_two_storage_memory_cache_within_same_role_cap(self):
        self.diagnostic_plan(); self.plan['storage_process_count'] = 2
        nodes = {'node1': {'private_ip': '10.93.0.11'}, 'client1': {'private_ip': '10.93.0.21'}}
        baseline = p.render_cluster(self.plan, nodes, 1, 'same-arm')
        explicit = dict(self.plan, storage_memory_profile='baseline')
        self.assertEqual(p.render_cluster(explicit, nodes, 1, 'same-arm'), baseline)
        capacity = dict(self.plan, storage_memory_profile='capacity16g')
        p.validate_plan(capacity, self.out)
        wide = p.render_cluster(capacity, nodes, 1, 'same-arm')
        self.assertEqual(wide['storage_memory_profile'], 'capacity16g')
        self.assertEqual(sum(x['memory_limit_bytes'] for x in wide['processes']), 13*p.GIB)
        self.assertEqual(sum(x['memory_limit_bytes'] for x in wide['processes'] if x['process_class'] == 'storage'), 8*p.GIB)
        self.assertEqual(sum(x['cache_mib'] for x in wide['processes'] if x['process_class'] == 'storage'), 4096)
        self.assertEqual(wide['budgets']['fdb_host']['max_process_memory_gib'], 13)
        self.assertEqual(wide['budgets']['fdb_host']['max_processes'], 7)
        self.assertEqual(wide['budgets']['fdb_host']['memory_max_gib'], 16)
        self.assertEqual(wide['budgets']['fdb_host']['cpu_quota_percent'], 400)
        self.assertEqual(p._host_prepare_script(capacity), p._host_prepare_script(explicit))
        for new, old in zip(wide['processes'], baseline['processes']):
            if new['process_class'] != 'storage':
                self.assertEqual(new, old)
            else:
                args = list(old['args']); args[args.index('--memory')+1] = '4GiB'; args[args.index('--cache-memory')+1] = '2GiB'
                self.assertEqual(new['args'], args)
                self.assertEqual(new['memory_limit_bytes'], 4*p.GIB)
                self.assertEqual(new['cache_mib'], 2048)
        for schema, count, profile in (('otel_horizontal_v1', 2, 'baseline'), ('otel_native_session_v1', 2, 'baseline'),
                                       ('otel_native_diagnostic_v1', 1, 'capacity16g'),
                                       ('otel_native_diagnostic_v1', 2, True), ('otel_native_diagnostic_v1', 2, 'other')):
            changed = dict(self.plan, schema=schema, storage_process_count=count, storage_memory_profile=profile)
            with self.subTest(schema=schema, count=count, profile=profile), self.assertRaises(p.ProvisionError):
                p.validate_plan(changed, self.out)
        self.assertFalse(self.cloud.calls)

    def test_session_prepares_once_and_only_case_memory_profile_can_vary_under_original_lease(self):
        self.session_plan()
        resources = self.prepare()
        self.assertEqual(set(resources['nodes']), {'node1', 'client1'})
        state = p._state(self.out)
        baseline_before = {kind: (state/'baseline'/('before-'+kind+'.json')).read_bytes() for kind in p.KINDS}
        for kind in ('fdb', 'client'):
            self.assertIn('IOAccounting=yes\n', (state/('horizontal-'+kind+'.slice')).read_text())
        case = self.case_plan('capacity16g')
        layout = p.setup_cluster(case, self.out, resources['nodes'], 1, 'capacity16g', runner=self.cloud)
        self.assertEqual(layout['budgets']['fdb_host']['max_process_memory_gib'], 13)
        self.assertEqual(self.cloud.create_counts['server'], 2)
        self.assertEqual(self.cloud.events.count('guard-start'), 1)
        for key, bad in (('first_epoch', self.plan['first_epoch']+1), ('deadline_epoch', self.plan['deadline_epoch']+1),
                         ('campaign', 'other-campaign'), ('prefix', 'other-prefix'), ('location', 'fsn1'), ('binary_sha256', '0'*64)):
            changed = dict(case); changed[key] = bad
            before = len(self.cloud.calls)
            with self.subTest(key=key), self.assertRaises(p.ProvisionError):
                p.setup_cluster(changed, self.out, resources['nodes'], 1, 'forbidden', runner=self.cloud)
            self.assertEqual(len(self.cloud.calls), before)
        self.assertEqual({kind: (state/'baseline'/('before-'+kind+'.json')).read_bytes() for kind in p.KINDS}, baseline_before)
        with self.assertRaises(p.ProvisionError):
            p.render_cluster(self.plan, resources['nodes'], 1, 'unselected-session')

    def test_stop_owned_arm_proves_native_and_scope_identity_then_preserves_old_arm_data(self):
        self.session_plan(); resources = self.prepare(); nodes = resources['nodes']; case = self.case_plan('capacity16g')
        layout = p.setup_cluster(case, self.out, nodes, 1, 'capacity16g', runner=self.cloud)
        scope = self.owned_scope(layout); runner = StopRunner(self.cloud, nodes, layout, (scope,))
        arm = Path(layout['cluster_file']).parent
        before = {path.name: path.read_bytes() for path in arm.iterdir() if path.is_file()}
        active_before = p._load(p._state(self.out)/'active-arm.json')
        receipt = p.stop_owned_arm(case, self.out, nodes, layout, scopes=(scope,), runner=runner)
        self.assertTrue(receipt['passed']); self.assertTrue(receipt['quiescent'])
        self.assertEqual(Path(receipt['receipt_path']).stat().st_mode & 0o777, 0o600)
        self.assertEqual(set(receipt['before']), {'node1', 'client1'})
        self.assertEqual(set(receipt['after']), {'node1', 'client1'})
        self.assertEqual(len(runner.stop_commands), 2)
        self.assertEqual(set(runner.stop_commands[0][1]), set(active_before['units']['node1']))
        self.assertEqual(runner.stop_commands[1], ('client1', [scope['unit']]))
        self.assertEqual(p._load(p._state(self.out)/'active-arm.json'), active_before)
        self.assertEqual({path.name: path.read_bytes() for path in arm.iterdir() if path.is_file()}, before)
        self.assertFalse((p._state(self.out)/'cleanup-started.json').exists())
        baseline = self.case_plan('baseline')
        second = p.setup_cluster(baseline, self.out, nodes, 1, 'baseline', runner=self.cloud)
        self.assertNotEqual(second['root'], layout['root'])
        self.assertEqual(second['budgets']['fdb_host']['max_process_memory_gib'], 9)
        self.assertEqual(self.cloud.create_counts['server'], 2)
        result = p.teardown(self.plan, self.out, resources, runner=self.cloud, sleep=lambda _: None)
        self.assertTrue(result['passed']); self.assertEqual(self.cloud.items, self.cloud.baseline)

    def test_stop_owned_arm_allows_inactive_scope_and_historical_pid_reuse_without_killing_it(self):
        self.session_plan(); resources = self.prepare(); nodes = resources['nodes']; case = self.case_plan('baseline')
        layout = p.setup_cluster(case, self.out, nodes, 1, 'baseline', runner=self.cloud)
        scope = self.owned_scope(layout); runner = StopRunner(self.cloud, nodes, layout, (scope,))
        runner.stopped.add(('client1', scope['unit']))
        runner.reused[scope['pid']] = dict(pid=scope['pid'], starttime_ticks=99999, args=['/usr/bin/sleep', '1'],
                                         exe='/usr/bin/sleep', cgroup='/system.slice/unrelated.service')
        receipt = p.stop_owned_arm(self.plan, self.out, nodes, layout, scopes=(scope,), runner=runner)
        self.assertTrue(receipt['passed'])
        self.assertEqual([role for role, _ in runner.stop_commands], ['node1'])
        self.assertFalse(any(scope['unit'] in units for _, units in runner.stop_commands))

    def test_stop_owned_arm_accepts_audit_query_scopes_without_historical_pid_receipts(self):
        self.session_plan(); resources = self.prepare(); nodes = resources['nodes']; case = self.case_plan('baseline')
        layout = p.setup_cluster(case, self.out, nodes, 1, 'baseline', runner=self.cloud)
        scopes = (self.owned_scope(layout, 'audit', False), self.owned_scope(layout, 'query', False))
        runner = StopRunner(self.cloud, nodes, layout, scopes)
        receipt = p.stop_owned_arm(case, self.out, nodes, layout, scopes=scopes, runner=runner)
        self.assertTrue(receipt['passed'])
        self.assertEqual(set(runner.stop_commands[-1][1]), {item['unit'] for item in scopes})

    def test_stop_owned_arm_rejects_mismatched_live_process_and_arms_hard_cleanup(self):
        self.session_plan(); resources = self.prepare(); nodes = resources['nodes']; case = self.case_plan('baseline')
        layout = p.setup_cluster(case, self.out, nodes, 1, 'baseline', runner=self.cloud)
        runner = StopRunner(self.cloud, nodes, layout)
        unit = layout['processes'][0]['unit_name']; runner.records[('node1', unit)]['args'] = ['/foreign/fdbserver']
        with self.assertRaisesRegex(p.ProvisionError, 'hard teardown required'):
            p.stop_owned_arm(case, self.out, nodes, layout, runner=runner)
        self.assertFalse(runner.stop_commands)
        self.assertTrue((p._state(self.out)/'cleanup-started.json').is_file())
        receipts = list(p._state(self.out).glob('stop-arm-*.json'))
        self.assertEqual(len(receipts), 1)
        self.assertFalse(p._load(receipts[0])['passed'])
        self.assertFalse(self.cloud.deleted)

    def test_stop_owned_arm_never_stops_a_main_pid_without_its_before_identity_snapshot(self):
        self.session_plan(); resources = self.prepare(); nodes = resources['nodes']; case = self.case_plan('baseline')
        layout = p.setup_cluster(case, self.out, nodes, 1, 'baseline', runner=self.cloud)
        runner = StopRunner(self.cloud, nodes, layout)
        def omit_main(role, phase, value):
            if role == 'node1' and phase == 'before':
                self.assertGreater(value['units'][0]['main_pid'], 0)
                value['units'][0]['main'] = None
        runner.mutate = omit_main
        with self.assertRaisesRegex(p.ProvisionError, 'MainPID is not bound'):
            p.stop_owned_arm(case, self.out, nodes, layout, runner=runner)
        self.assertFalse(runner.stop_commands)
        self.assertTrue((p._state(self.out)/'cleanup-started.json').is_file())
        receipts = list(p._state(self.out).glob('stop-arm-*.json'))
        self.assertEqual(len(receipts), 1)
        receipt = p._load(receipts[0])
        self.assertFalse(receipt['passed'])
        raw = json.loads(receipt['before']['node1']['stdout'])
        self.assertGreater(raw['units'][0]['main_pid'], 0)
        self.assertIsNone(raw['units'][0]['main'])
        self.assertFalse(self.cloud.deleted)

    def test_stop_owned_arm_rejects_foreign_journal_scope_and_unmapped_residual_without_stopping_them(self):
        self.session_plan(); resources = self.prepare(); nodes = resources['nodes']; case = self.case_plan('baseline')
        layout = p.setup_cluster(case, self.out, nodes, 1, 'baseline', runner=self.cloud)
        active = p._load(p._state(self.out)/'active-arm.json')
        scope = self.owned_scope(layout); scope['unit'] = 'other.scope'
        runner = StopRunner(self.cloud, nodes, layout)
        with self.assertRaises(p.ProvisionError):
            p._stop_specs(case, nodes, layout, active, (scope,))
        forged = copy.deepcopy(active); forged['units']['node1'].append('protected.service')
        with self.assertRaises(p.ProvisionError):
            p._stop_specs(case, nodes, layout, forged, ())
        def residual(role, phase, value):
            if role == 'client1':
                value['owned_residual'].append(dict(pid=999, args=['foreign']))
        runner.mutate = residual
        with self.assertRaises(p.ProvisionError):
            p.stop_owned_arm(case, self.out, nodes, layout, runner=runner)
        self.assertFalse(runner.stop_commands)
        self.assertTrue((p._state(self.out)/'cleanup-started.json').exists())

    def test_stop_owned_arm_quiescence_timeout_cannot_continue_or_reset_the_original_lease(self):
        self.session_plan(); resources = self.prepare(); nodes = resources['nodes']; case = self.case_plan('baseline')
        layout = p.setup_cluster(case, self.out, nodes, 1, 'baseline', runner=self.cloud)
        runner = StopRunner(self.cloud, nodes, layout); runner.linger = True
        now = [float(self.plan['first_epoch']+1)]
        def sleep(seconds): now[0] += seconds
        with self.assertRaises(p.ProvisionError):
            p.stop_owned_arm(case, self.out, nodes, layout, runner=runner, clock=lambda: now[0], sleep=sleep)
        self.assertLessEqual(now[0], self.plan['first_epoch']+121)
        self.assertTrue((p._state(self.out)/'cleanup-started.json').exists())
        with self.assertRaises(p.ProvisionError):
            p.setup_cluster(self.case_plan('capacity16g'), self.out, nodes, 1, 'must-not-run', runner=self.cloud)

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

    def test_storage_max_outstanding_rejects_malformed_and_all_legacy_usage_before_effects(self):
        self.diagnostic_plan()
        base = copy.deepcopy(self.plan)
        for bad in (None, True, False, 0, 63, 65, 127, 129, 64.0, 128.0, '128', [], {}):
            self.plan.clear(); self.plan.update(base); self.plan['storage_max_outstanding'] = bad
            with self.subTest(schema='diagnostic', value=bad), self.assertRaises(p.ProvisionError):
                self.prepare()
            with self.assertRaises(p.ProvisionError):
                p.render_cluster(self.plan, {'node1': {'private_ip': '10.93.0.11'},
                                            'client1': {'private_ip': '10.93.0.21'}}, 1, 'invalid')
        for value in (64, 128, None, True):
            self.plan.clear(); self.plan.update(base); self.plan['schema'] = 'otel_horizontal_v1'
            self.plan['storage_max_outstanding'] = value
            with self.subTest(schema='legacy', value=value), self.assertRaises(p.ProvisionError):
                self.prepare()
            with self.assertRaises(p.ProvisionError):
                p.render_cluster(self.plan, {}, 1, 'invalid')
        self.assertFalse(self.cloud.calls)
        self.assertFalse(self.cloud.create_counts)

    def test_diagnostic_default_and_explicit_64_keep_legacy_argv_and_unit_bytes_identical(self):
        self.diagnostic_plan()
        nodes = {'node1': {'private_ip': '10.93.0.11'}, 'client1': {'private_ip': '10.93.0.21'}}
        omitted = p.render_cluster(self.plan, nodes, 1, 'same-arm')
        self.plan['storage_max_outstanding'] = 64
        p.validate_plan(self.plan, self.out)
        explicit = p.render_cluster(self.plan, nodes, 1, 'same-arm')
        self.assertEqual(omitted, explicit)
        self.assertEqual(omitted['storage_max_outstanding'], 64)
        legacy_plan = dict(self.plan, schema='otel_horizontal_v1'); del legacy_plan['storage_max_outstanding']
        legacy_nodes = {role: {'private_ip': '10.93.0.'+str(offset)} for role, offset in
                        (('node1', 11), ('node2', 12), ('node3', 13), ('client1', 21), ('client2', 22))}
        legacy = p.render_cluster(legacy_plan, legacy_nodes, 1, 'same-arm')
        self.assertNotIn('storage_max_outstanding', legacy)
        self.assertEqual(omitted['processes'], legacy['processes'])
        for new, old in zip(omitted['processes'], legacy['processes']):
            self.assertEqual(new['args'], old['args'])
            self.assertEqual(new['unit'].encode(), old['unit'].encode())
            self.assertNotIn('--knob_max_outstanding', new['args'])
            self.assertNotIn('knob_max_outstanding', new['unit'])
        self.assertFalse(self.cloud.calls)

    def test_diagnostic_128_changes_only_storage_argv_and_preserves_all_resource_and_durability_guards(self):
        self.diagnostic_plan()
        base = copy.deepcopy(self.plan)
        self.plan['storage_max_outstanding'] = 128
        resources = self.prepare()
        nodes = resources['nodes']
        default = p.render_cluster(base, nodes, 1, 'same-arm')
        changed = p.render_cluster(self.plan, nodes, 1, 'same-arm')
        self.assertEqual(changed['storage_max_outstanding'], 128)
        self.assertEqual(changed['budgets'], default['budgets'])
        self.assertEqual(p._host_prepare_script(self.plan), p._host_prepare_script(base))
        self.assertEqual(changed['configure'], default['configure'])
        self.assertEqual(changed['storage_engine'], 'ssd-redwood-1')
        self.assertEqual(changed['log_engine'], 'ssd-2')
        self.assertEqual(len(changed['processes']), 6)
        for new, old in zip(changed['processes'], default['processes']):
            self.assertEqual({k: v for k, v in new.items() if k not in ('args', 'unit')},
                             {k: v for k, v in old.items() if k not in ('args', 'unit')})
            if new['process_class'] == 'storage':
                self.assertEqual(new['args'], old['args']+['--knob_max_outstanding', '128'])
                self.assertEqual(new['unit'].encode(), old['unit'].replace(
                    'ExecStart='+shlex.join(old['args'])+'\n', 'ExecStart='+shlex.join(new['args'])+'\n').encode())
                self.assertEqual(new['memory_limit_bytes'], 4*p.GIB)
                self.assertEqual(new['cache_mib'], 2048)
            else:
                self.assertEqual(new, old)
                self.assertNotIn('--knob_max_outstanding', new['args'])
        layout = p.setup_cluster(self.plan, self.out, nodes, 1, 'storage-128', runner=self.cloud)
        self.assertEqual(p._load(layout['receipt'])['storage_max_outstanding'], 128)
        arm = Path(layout['cluster_file']).parent
        for process in layout['processes']:
            emitted = (arm/(process['node']+'-'+process['unit_name'])).read_bytes()
            self.assertEqual(emitted, process['unit'].encode())
            self.assertEqual(b'--knob_max_outstanding 128' in emitted, process['process_class'] == 'storage')
        for role in ('node1', 'client1'):
            receipt = p._load(p._state(self.out)/(role+'-native.json'))
            self.assertEqual(receipt['filesystem'], 'ext4')
            self.assertTrue(receipt['fsync_preserved'])
            self.assertTrue(receipt['checksums_preserved'])
            self.assertEqual(receipt['floor_bytes'], p.disk_floor(receipt['total_bytes']))

    def test_storage_process_count_rejects_untyped_legacy_and_two_ss_128_before_effects(self):
        self.diagnostic_plan()
        base = copy.deepcopy(self.plan)
        nodes = {'node1': {'private_ip': '10.93.0.11'}, 'client1': {'private_ip': '10.93.0.21'}}
        for bad in (None, True, False, 0, 3, 1.0, 2.0, '2', [], {}):
            self.plan.clear(); self.plan.update(base); self.plan['storage_process_count'] = bad
            with self.subTest(value=bad), self.assertRaises(p.ProvisionError):
                self.prepare()
            with self.assertRaises(p.ProvisionError):
                p.render_cluster(self.plan, nodes, 1, 'invalid')
        self.plan.clear(); self.plan.update(base)
        self.plan.update(storage_process_count=2, storage_max_outstanding=128)
        with self.assertRaises(p.ProvisionError):
            self.prepare()
        with self.assertRaises(p.ProvisionError):
            p.render_cluster(self.plan, nodes, 1, 'invalid')
        for count in (1, 2, None, True):
            self.plan.clear(); self.plan.update(base)
            self.plan.update(schema='otel_horizontal_v1', storage_process_count=count)
            with self.subTest(schema='legacy', count=count), self.assertRaises(p.ProvisionError):
                self.prepare()
            with self.assertRaises(p.ProvisionError):
                p.render_cluster(self.plan, {}, 1, 'invalid')
        self.assertFalse(self.cloud.calls)
        self.assertFalse(self.cloud.create_counts)

    def test_storage_process_count_one_default_and_explicit_keep_exact_existing_process_bytes(self):
        self.diagnostic_plan()
        nodes = {'node1': {'private_ip': '10.93.0.11'}, 'client1': {'private_ip': '10.93.0.21'}}
        omitted = p.render_cluster(self.plan, nodes, 1, 'same-arm')
        self.plan['storage_process_count'] = 1
        p.validate_plan(self.plan, self.out)
        explicit = p.render_cluster(self.plan, nodes, 1, 'same-arm')
        self.assertEqual(omitted, explicit)
        self.assertEqual(omitted['storage_process_count'], 1)
        self.assertEqual(omitted['budgets']['fdb_host']['max_processes'], 6)
        legacy_plan = dict(self.plan, schema='otel_horizontal_v1'); del legacy_plan['storage_process_count']
        legacy_nodes = {role: {'private_ip': '10.93.0.'+str(offset)} for role, offset in
                        (('node1', 11), ('node2', 12), ('node3', 13), ('client1', 21), ('client2', 22))}
        legacy = p.render_cluster(legacy_plan, legacy_nodes, 1, 'same-arm')
        self.assertNotIn('storage_process_count', legacy)
        self.assertEqual(omitted['processes'], legacy['processes'])
        for new, old in zip(omitted['processes'], legacy['processes']):
            self.assertEqual(new['args'], old['args'])
            self.assertEqual(new['unit'].encode(), old['unit'].encode())
        self.plan['storage_max_outstanding'] = 128
        p.validate_plan(self.plan, self.out)
        allowed = p.render_cluster(self.plan, nodes, 1, 'same-arm')
        self.assertEqual(allowed['storage_process_count'], 1)
        self.assertEqual(allowed['storage_max_outstanding'], 128)
        self.assertEqual(len(allowed['processes']), 6)
        self.assertFalse(self.cloud.calls)

    def test_two_ss_default_64_uses_ports_4510_4511_with_same_aggregate_budgets_and_seven_units(self):
        self.diagnostic_plan()
        base = copy.deepcopy(self.plan)
        self.plan['storage_process_count'] = 2
        resources = self.prepare()
        self.assertEqual(set(resources['nodes']), {'node1', 'client1'})
        nodes = resources['nodes']
        default = p.render_cluster(base, nodes, 1, 'same-arm')
        changed = p.render_cluster(self.plan, nodes, 1, 'same-arm')
        explicit_64 = dict(self.plan, storage_max_outstanding=64)
        p.validate_plan(explicit_64, self.out)
        self.assertEqual(p.render_cluster(explicit_64, nodes, 1, 'same-arm'), changed)
        self.assertEqual(changed['storage_process_count'], 2)
        self.assertEqual(changed['storage_max_outstanding'], 64)
        self.assertEqual(changed['storage_hosts'], ['node1'])
        self.assertEqual(len(changed['processes']), 7)
        old_controls = [x for x in default['processes'] if x['process_class'] != 'storage']
        new_controls = [x for x in changed['processes'] if x['process_class'] != 'storage']
        self.assertEqual(len(new_controls), 5)
        self.assertEqual(new_controls, old_controls)
        storage = [x for x in changed['processes'] if x['process_class'] == 'storage']
        self.assertEqual({x['port'] for x in storage}, {4510, 4511})
        self.assertEqual({x['node'] for x in storage}, {'node1'})
        old_storage = next(x for x in default['processes'] if x['process_class'] == 'storage')
        for process in storage:
            expected = list(old_storage['args'])
            for flag, value in (('--public-address', '10.93.0.11:'+str(process['port'])),
                                ('--listen-address', '10.93.0.11:'+str(process['port'])),
                                ('--datadir', changed['root']+'/data/'+str(process['port'])),
                                ('--memory', '2GiB'), ('--cache-memory', '1GiB')):
                expected[expected.index(flag)+1] = value
            self.assertEqual(process['args'], expected)
            self.assertNotIn('--knob_max_outstanding', process['args'])
            self.assertEqual(process['memory_limit_bytes'], 2*p.GIB)
            self.assertEqual(process['cache_mib'], 1024)
            self.assertEqual(process['unit'].encode(), old_storage['unit'].replace(
                'ExecStart='+shlex.join(old_storage['args'])+'\n', 'ExecStart='+shlex.join(expected)+'\n').encode())
        self.assertEqual(sum(x['memory_limit_bytes'] for x in storage), 4*p.GIB)
        self.assertEqual(sum(x['cache_mib'] for x in storage), 2048)
        self.assertEqual(sum(x['memory_limit_bytes'] for x in changed['processes']), 9*p.GIB)
        expected_budgets = copy.deepcopy(default['budgets']); expected_budgets['fdb_host']['max_processes'] = 7
        self.assertEqual(changed['budgets'], expected_budgets)
        self.assertEqual(changed['budgets']['fdb_host']['cpu_quota_percent'], 400)
        self.assertEqual(changed['budgets']['fdb_host']['memory_max_gib'], 16)
        self.assertEqual(changed['configure'], default['configure'])
        self.assertEqual(p._host_prepare_script(self.plan), p._host_prepare_script(base))
        layout = p.setup_cluster(self.plan, self.out, nodes, 1, 'two-ss', runner=self.cloud)
        self.assertEqual(p._load(layout['receipt'])['storage_process_count'], 2)
        active = p._load(p._state(self.out)/'active-arm.json')
        self.assertEqual(set(active['units']), {'node1'})
        self.assertEqual(len(active['units']['node1']), 7)
        arm = Path(layout['cluster_file']).parent
        for process in layout['processes']:
            self.assertEqual((arm/(process['node']+'-'+process['unit_name'])).read_bytes(), process['unit'].encode())
        starts = [shlex.split(argv[-1]) for argv, _ in self.cloud.calls if Path(argv[0]).name == 'ssh'
                  and shlex.split(argv[-1])[:2] == ['systemctl', 'start']
                  and any(x.startswith('oh-') for x in shlex.split(argv[-1])[2:])]
        self.assertEqual(len(starts), 1)
        self.assertEqual(set(starts[0][2:]), set(active['units']['node1']))

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

    def test_diagnostic_prepare_and_setup_slices_enable_only_io_accounting(self):
        self.diagnostic_plan()
        resources = self.prepare()
        self.assertEqual(set(resources['nodes']), {'node1', 'client1'})
        state = p._state(self.out)
        for kind in ('fdb', 'client'):
            legacy = legacy_slice_bytes(kind)
            expected = legacy.replace(b'MemoryAccounting=yes\n', b'MemoryAccounting=yes\nIOAccounting=yes\n')
            self.assertEqual(p._slice(kind).encode(), legacy)
            self.assertEqual(p._slice(kind, io_accounting=True).encode(), expected)
            self.assertEqual((state/('horizontal-'+kind+'.slice')).read_bytes(), expected)
        layout = p.setup_cluster(self.plan, self.out, resources['nodes'], 1, 'diagnostic-slices', runner=self.cloud)
        arm = Path(layout['cluster_file']).parent
        for kind in ('fdb', 'client'):
            name = 'horizontal-'+kind+'.slice'
            expected = legacy_slice_bytes(kind).replace(b'MemoryAccounting=yes\n',
                                                        b'MemoryAccounting=yes\nIOAccounting=yes\n')
            self.assertEqual((arm/name).read_bytes(), expected)
            self.assertEqual((state/name).read_bytes(), expected)
            self.assertEqual(expected.count(b'IOAccounting=yes\n'), 1)
            copies = [argv for argv, _ in self.cloud.calls if Path(argv[0]).name == 'scp'
                      and argv[-1].endswith('/etc/systemd/system/'+name)]
            self.assertEqual(len(copies), 2)
            self.assertEqual({Path(argv[-2]) for argv in copies}, {state/name, arm/name})

    def test_legacy_five_host_prepare_and_setup_slice_bytes_are_exactly_unchanged(self):
        for kind in ('fdb', 'client'):
            self.assertEqual(p._slice(kind).encode(), legacy_slice_bytes(kind))
            self.assertEqual(p._slice(kind, io_accounting=False).encode(), legacy_slice_bytes(kind))
        resources = self.prepare()
        self.assertEqual(set(resources['nodes']), set(p.ROLES))
        state = p._state(self.out)
        for kind in ('fdb', 'client'):
            self.assertEqual((state/('horizontal-'+kind+'.slice')).read_bytes(), legacy_slice_bytes(kind))
        layout = p.setup_cluster(self.plan, self.out, resources['nodes'], 1, 'legacy-slices', runner=self.cloud)
        arm = Path(layout['cluster_file']).parent
        for kind, hosts in (('fdb', 3), ('client', 2)):
            name = 'horizontal-'+kind+'.slice'
            self.assertEqual((state/name).read_bytes(), legacy_slice_bytes(kind))
            self.assertEqual((arm/name).read_bytes(), legacy_slice_bytes(kind))
            copies = [argv for argv, _ in self.cloud.calls if Path(argv[0]).name == 'scp'
                      and argv[-1].endswith('/etc/systemd/system/'+name)]
            self.assertEqual(len(copies), hosts*2)
            self.assertEqual({Path(argv[-2]) for argv in copies}, {state/name, arm/name})

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
