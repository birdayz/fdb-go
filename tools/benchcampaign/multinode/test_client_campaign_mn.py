#!/usr/bin/env python3
"""Tests for cluster_gate.py and client_campaign_mn.py (R1). Temporary fixtures and
fakes only: no fdbcli, FDB, ssh, cloud or benchmark process is ever run.

The end-to-end adapter tests seal plans through the real frozen
tools/clientbench/scripts and skip when those scripts are absent."""
import copy
import json
import os
from pathlib import Path
import sys
import tempfile
import types
import unittest
from unittest import mock

HERE = Path(os.path.abspath(__file__)).parent
sys.path.insert(0, str(HERE))

import cluster_gate as cg  # noqa: E402
import client_campaign_mn as r1  # noqa: E402
import fdb_cluster  # noqa: E402
import mn_common as mc  # noqa: E402
import seal_multinode as sm  # noqa: E402

generic = cg.generic
NODES = ['10.88.0.11', '10.88.0.12', '10.88.0.13']
VPC = '10.88.0.0/24'
REAL_SCRIPTS = HERE.parent.parent / 'clientbench' / 'scripts'
HAVE_SCRIPTS = (REAL_SCRIPTS / 'matrix_plan.py').is_file() and (REAL_SCRIPTS / 'run.py').is_file()


def layout(engine='ssd-2'):
    return fdb_cluster.build_layout(NODES, '10.88.0.10', VPC, 'fdbbenchvpc', 'mn20261014', engine,
                                    '/srv/fdb-bench/cluster', '/usr/sbin/fdbserver', 'fdb-bench-vpc', [])


ZERO = dict(in_flight_bytes=0, in_queue_bytes=0)


def status(lay, dd_disabled=False, generation=3, mutate=None, moving_data=None):
    """A healthy status json for the layout, with the fields the server gate reads.

    As FDB 7.3.77 Status.actor.cpp: moving_data exists only while data
    distribution reports a shard size, so it is absent with DD off unless given.
    """
    roles_of = {'stateless': ['cluster_controller', 'master', 'data_distributor', 'ratekeeper'],
                'commit_proxy': ['commit_proxy'], 'grv_proxy': ['grv_proxy'], 'resolution': ['resolver'],
                'transaction': ['log'], 'storage': ['storage'], 'coordinator': []}
    procs = {}
    for i, p in enumerate(lay['processes']):
        roles = list(roles_of[p['process_class']]) + (['coordinator'] if p['address'] in lay['coordinators'] else [])
        procs[f'p{i}'] = dict(address=p['address'], class_type=p['process_class'],
                              locality=dict(machineid=p['node'], zoneid=p['node']),
                              roles=[dict(role=r, id=f'{r}{i}') for r in roles])
    cluster = dict(database_available=True, processes=procs, generation=generation,
                   recovery_state=dict(name='fully_recovered'),
                   qos=dict(worst_durability_lag_storage_server=dict(seconds=0.2, versions=200000),
                            worst_queue_bytes_log_server=4096),
                   data=dict(moving_data=dict(moving_data or ZERO)) if (moving_data or not dd_disabled) else dict(),
                   configuration=dict(redundancy_mode='double', storage_engine=lay['engine'],
                                      commit_proxies=1, grv_proxies=1, resolvers=1, logs=2))
    if dd_disabled:
        cluster['data_distribution_disabled'] = True
    s = dict(client=dict(coordinators=dict(coordinators=[dict(address=a, reachable=True) for a in lay['coordinators']]),
                         database_status=dict(available=True, healthy=True)), cluster=cluster)
    if mutate:
        mutate(s)
    return s


def protocol(client_workloads=('get100',), new_harness=()):
    """A small version-2 multi-node protocol with one client and one testing.B phase."""
    return dict(
        version=2, campaign='mn-test', frozen_harness_revision='d' * 40, new_harness_revision=None,
        production_revisions=dict(final='b' * 40, before_allocation_work_after_read_parity='a' * 40),
        runtime=dict(GOMAXPROCS=8, GOGC=100, GOMEMLIMIT='off', GODEBUG='', TZ='UTC', LC_ALL='C', go='1.26.9', api=730,
                     client_vm=dict(housekeeping_cpus='0-1', idle_sentinel_cpus='2-7', client_cpus='8-15')),
        environment_policy=dict(pre_arm_thresholds=dict(
            client_vm_all_cpu_aggregate_busy=0.15, client_vm_housekeeping_aggregate_busy=0.3,
            client_vm_housekeeping_max_core_busy=0.8, client_vm_idle_sentinel_aggregate_busy=0.15,
            client_vm_idle_sentinel_max_core_busy=0.5, client_vm_client_aggregate_busy=0.05,
            client_vm_client_max_core_busy=0.15, any_cpu_steal=0.01)),
        sample_policy=dict(cooldown_seconds=2, quiescence_seconds=1, max_steal_fraction=0.01),
        phases=[
            dict(id='M1w1', kind='client', engine='ssd-2', arms=['final-Go', 'CGo-7.3.77'], workloads=list(client_workloads),
                 new_harness_workloads=list(new_harness), workers=1, iterations=3000, warmup=200, blocks=6, repeats_per_arm_process=1,
                 order_seed=742001, frozen_order_key='M1w1', per_workload_campaign_wall_cap_seconds=900,
                 per_process_timeout_seconds=180, wall_cap_seconds=2700),
            dict(id='M2rl', kind='testingb', engine='ssd-2', binary_kind='fetch', arms=['final-ordinary', 'final-remote'],
                 payloads=['small', 'split'], cells=['rows10'], dirs=['forward'], new_harness_cells=['rows100'],
                 placement_required=True, repeats_per_arm_process=3, benchtime='3s', blocks=6, order_seed=742401,
                 frozen_order_key='M2rl', process_timeout_seconds=600, wall_cap_seconds=6300),
        ])


def cpu_stat(n, steal=0, cpus=16):
    return '\n'.join([f'cpu {n * 160} 0 0 {n * 1440} 0 0 0 {steal}'] +
                     [f'cpu{i} {n * 10} 0 0 {n * 90} 0 0 0 {steal}' for i in range(cpus)])


def sample(n, steal=0, processes=()):
    return {'monotonic_ns': int(n * 1e9), 'processes': list(processes), '/proc/stat': cpu_stat(n, steal),
            '/proc/vmstat': 'oom_kill 0\n'}


class Temp(unittest.TestCase):
    def setUp(self):
        self._tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self._tmp.cleanup)
        self.tmp = Path(self._tmp.name)
        if mc.inside_git_worktree(self.tmp):
            self.skipTest('temporary directory lies inside a Git work tree')


class ClusterFileGuardTest(Temp):
    def write(self, text):
        path = self.tmp / 'fdb.cluster'
        path.write_text(text + '\n')
        return path

    def test_generated_three_coordinator_file_passes(self):
        lay = layout()
        self.assertEqual(cg.cluster_file_guard(self.write(lay['cluster_file']), lay, VPC), lay['coordinators'])

    def test_refusals(self):
        lay = layout()
        single = dict(lay, cluster_file='fdbbenchvpc:mn20261014@10.88.0.11:4500', coordinators=['10.88.0.11:4500'])
        same_host = dict(lay, cluster_file='fdbbenchvpc:mn20261014@10.88.0.11:4500,10.88.0.11:4501,10.88.0.13:4500',
                         coordinators=['10.88.0.11:4500', '10.88.0.11:4501', '10.88.0.13:4500'])
        outside = ['192.0.2.11:4500', '10.88.0.12:4500', '10.88.0.13:4500']
        foreign = dict(lay, cluster_file='fdbbenchvpc:mn20261014@' + ','.join(outside), coordinators=outside)
        cases = [
            ('single coordinator', single['cluster_file'], single, VPC),
            ('one machine twice', same_host['cluster_file'], same_host, VPC),
            ('coordinator outside the VPC', foreign['cluster_file'], foreign, VPC),
            ('file is not the layout file', 'fdbbenchvpc:other@' + ','.join(lay['coordinators']), lay, VPC),
            ('coordinator order differs', 'fdbbenchvpc:mn20261014@10.88.0.12:4500,10.88.0.11:4500,10.88.0.13:4500', lay, VPC),
            ('flag VPC differs from layout', lay['cluster_file'], lay, '10.88.1.0/24'),
            ('malformed', 'not a cluster file', lay, VPC),
        ]
        for name, text, lay_, vpc in cases:
            with self.subTest(name), self.assertRaises(cg.EnvironmentFailure):
                cg.cluster_file_guard(self.write(text), lay_, vpc)


class StatusVerdictTest(unittest.TestCase):
    def test_healthy_status_has_no_errors(self):
        lay = layout()
        self.assertEqual(cg.status_verdict(status(lay), lay, 'ssd-2', 'enabled')[:2], ([], []))
        self.assertEqual(cg.status_verdict(status(lay, dd_disabled=True), lay, 'ssd-2', 'disabled')[:2], ([], []))

    def test_each_server_pre_arm_condition_is_enforced(self):
        lay = layout()

        def edit(path, value):
            def mutate(s):
                keys = path.split('.')
                target = s
                for k in keys[:-1]:
                    target = target[k]
                if value is KeyError:
                    del target[keys[-1]]
                else:
                    target[keys[-1]] = value
            return mutate

        def move_proxy(s):
            s['cluster']['processes']['p1']['roles'] = [dict(role='storage', id='x')]
        cases = [
            ('unavailable', edit('cluster.database_available', False), 'available'),
            ('wrong engine', edit('cluster.configuration.storage_engine', 'memory'), 'storage_engine'),
            ('recovering', edit('cluster.recovery_state.name', 'recruiting_transaction_servers'), 'recovery_state'),
            ('no recovery state', edit('cluster.recovery_state', KeyError), 'recovery_state'),
            ('durability lag', edit('cluster.qos.worst_durability_lag_storage_server.seconds', 60.0), 'durability lag'),
            ('log queue', edit('cluster.qos.worst_queue_bytes_log_server', 100 * 1000 * 1000), 'log queue'),
            ('no qos', edit('cluster.qos', KeyError), 'durability lag'),
            ('DD disabled during M1', edit('cluster.data_distribution_disabled', True), 'data distribution is disabled'),
            ('no moving_data', edit('cluster.data.moving_data', KeyError), 'moving_data'),
            ('role moved', move_proxy, 'check-roles'),
        ]
        for name, mutate, text in cases:
            with self.subTest(name):
                errors, _, _ = cg.status_verdict(status(lay, mutate=mutate), lay, 'ssd-2', 'enabled')
                self.assertTrue(any(text in e for e in errors), errors)
        errors, _, _ = cg.status_verdict(status(lay), lay, 'ssd-2', 'disabled')
        self.assertTrue(any('M2 placement requires it frozen' in e for e in errors))
        errors, _, _ = cg.status_verdict(status(lay), layout('memory'), 'memory', 'enabled')
        self.assertTrue(errors)

    def test_moving_data_absent_only_acceptable_with_frozen_data_distribution(self):
        lay = layout()
        frozen = status(lay, dd_disabled=True)
        self.assertNotIn('moving_data', frozen['cluster']['data'])
        # DD off: FDB writes no moving_data; absent or all-zero both pass.
        self.assertEqual(cg.status_verdict(frozen, lay, 'ssd-2', 'disabled')[:2], ([], []))
        self.assertEqual(cg.status_verdict(status(lay, dd_disabled=True, moving_data=ZERO), lay, 'ssd-2', 'disabled')[:2],
                         ([], []))
        # ... but present and moving is still movement, and a half-written record is unverifiable.
        moving = status(lay, dd_disabled=True, moving_data=dict(in_flight_bytes=0, in_queue_bytes=3))
        self.assertEqual(cg.status_verdict(moving, lay, 'ssd-2', 'disabled')[:2], ([], ['moving_data.in_queue_bytes=3']))
        half = status(lay, dd_disabled=True, moving_data=dict(in_flight_bytes=0))
        self.assertTrue(any('unverifiable' in e for e in cg.status_verdict(half, lay, 'ssd-2', 'disabled')[0]))
        # DD on (M1 pre-arm, or DD re-initialising): absence is unverifiable and refuses.
        for name, doc in [('no moving_data', status(lay, mutate=lambda s: s['cluster']['data'].pop('moving_data'))),
                          ('no data section', status(lay, mutate=lambda s: s['cluster'].pop('data'))),
                          ('one field missing', status(lay, moving_data=dict(in_flight_bytes=0)))]:
            with self.subTest(name):
                errors, movement, _ = cg.status_verdict(doc, lay, 'ssd-2', 'enabled')
                self.assertTrue(any('data movement unverifiable' in e for e in errors), errors)
                self.assertEqual(movement, [])

    def test_absent_log_queue_is_unverifiable_not_zero(self):
        lay = layout()
        doc = status(lay, mutate=lambda s: s['cluster']['qos'].pop('worst_queue_bytes_log_server'))
        for dd, d in [('enabled', doc), ('disabled', status(lay, dd_disabled=True, mutate=lambda s: s['cluster']['qos'].pop('worst_queue_bytes_log_server')))]:
            with self.subTest(dd):
                errors, _, _ = cg.status_verdict(d, lay, 'ssd-2', dd)
                self.assertTrue(any('log queue' in e and 'missing' in e for e in errors), errors)

    def test_coordinator_class_processes_are_accepted(self):
        lay = layout()
        doc = status(lay)
        self.assertIn('coordinator', {p['class_type'] for p in doc['cluster']['processes'].values()})
        self.assertEqual(cg.status_verdict(doc, lay, 'ssd-2', 'enabled')[0], [])

    def test_movement_is_separate_from_errors(self):
        lay = layout()
        s = status(lay, mutate=lambda s: s['cluster']['data']['moving_data'].update(in_flight_bytes=5))
        errors, movement, _ = cg.status_verdict(s, lay, 'ssd-2', 'enabled')
        self.assertEqual((errors, movement), ([], ['moving_data.in_flight_bytes=5']))


class FakeCommand:
    """fdbcli stand-in: answers status json from a queue of documents."""

    def __init__(self, statuses, committed=True):
        self.statuses, self.calls, self.committed = list(statuses), [], committed

    def __call__(self, argv, folder, label, timeout):
        self.calls.append((argv, label))
        if argv[-1] == 'status json':
            value = self.statuses.pop(0) if len(self.statuses) > 1 else self.statuses[0]
            return json.dumps(value)
        if 'clearrange' in argv[-1]:
            return 'Committed (77)\n' if self.committed else 'ERROR\n'
        raise AssertionError(f'unexpected command {argv}')


class FakeClock:
    def __init__(self):
        self.now = 0.0

    def __call__(self):
        return self.now

    def sleep(self, seconds):
        self.now += seconds


class ServerGateTest(Temp):
    def gate(self, statuses, dd='enabled'):
        clock = FakeClock()
        command = FakeCommand(statuses)
        lay = layout()
        return cg.ServerGate(self.tmp / 'fdb.cluster', lay, 'ssd-2', dd, command, sleep=clock.sleep, clock=clock), command, clock

    def test_waits_for_data_movement_then_passes(self):
        lay = layout()
        moving = status(lay, mutate=lambda s: s['cluster']['data']['moving_data'].update(in_queue_bytes=10))
        gate, command, clock = self.gate([moving, moving, status(lay)])
        self.assertTrue(gate.begin_case(self.tmp, 'pre'))
        self.assertEqual(clock.now, 2 * cg.MOVEMENT_POLL_SECONDS)
        self.assertEqual([label for _, label in command.calls], ['pre', 'pre-poll1', 'pre-poll2'])
        self.assertTrue((self.tmp / 'pre-gate.json').is_file() and (self.tmp / 'pre-check-roles.json').is_file())
        self.assertFalse(gate.pre_arm(self.tmp, 'next'))

    def test_movement_for_60_seconds_aborts(self):
        lay = layout()
        moving = status(lay, mutate=lambda s: s['cluster']['data']['moving_data'].update(in_flight_bytes=1))
        gate, _, clock = self.gate([moving])
        with self.assertRaisesRegex(cg.EnvironmentFailure, 'data movement in flight after 60 s'):
            gate.pre_arm(self.tmp, 'pre')
        self.assertGreaterEqual(clock.now, 60)

    def test_frozen_data_distribution_without_moving_data_passes(self):
        gate, command, clock = self.gate([status(layout(), dd_disabled=True)], dd='disabled')
        self.assertFalse(gate.begin_case(self.tmp, 'pre'))
        self.assertEqual((len(command.calls), clock.now), (1, 0))
        gate, _, _ = self.gate([status(layout(), mutate=lambda s: s['cluster']['data'].pop('moving_data'))])
        with self.assertRaisesRegex(cg.EnvironmentFailure, 'unverifiable'):
            gate.pre_arm(self.tmp, 'dd-on')

    def test_frozen_data_distribution_allows_no_movement(self):
        lay = layout()
        moving = status(lay, dd_disabled=True, moving_data=dict(in_flight_bytes=1, in_queue_bytes=0))
        gate, _, clock = self.gate([moving], dd='disabled')
        with self.assertRaisesRegex(cg.EnvironmentFailure, 'data movement'):
            gate.pre_arm(self.tmp, 'pre')
        self.assertEqual(clock.now, 0)

    def test_roles_and_generation_must_not_change_within_a_case(self):
        lay = layout()

        def swap_storage_id(s):
            s['cluster']['processes']['p5']['roles'][0]['id'] = 'recruited-again'
        for name, later in [('recovery', status(lay, generation=4)), ('re-recruited storage', status(lay, mutate=swap_storage_id))]:
            with self.subTest(name):
                gate, _, _ = self.gate([status(lay), later])
                gate.begin_case(self.tmp, f'{name}-before')
                with self.assertRaisesRegex(cg.EnvironmentFailure, 'generation|roles'):
                    gate.check(self.tmp, f'{name}-after')
        gate, _, _ = self.gate([status(lay, mutate=lambda s: s['cluster'].pop('generation'))])
        with self.assertRaisesRegex(cg.EnvironmentFailure, 'generation'):
            gate.pre_arm(self.tmp, 'no-generation')

    def test_unparsable_status_and_uncommitted_clear(self):
        gate = cg.ServerGate(self.tmp / 'c', layout(), 'ssd-2', 'enabled', lambda *a: 'not json')
        with self.assertRaisesRegex(cg.EnvironmentFailure, 'unparsable'):
            gate.pre_arm(self.tmp, 'x')
        gate, _, _ = self.gate([status(layout())])
        gate.command.committed = False
        with self.assertRaisesRegex(cg.EnvironmentFailure, 'committed'):
            gate.clear_user_keys(self.tmp, 'clear')


class ClientHostTest(unittest.TestCase):
    PLAN = {'max_steal_fraction': .01, 'cpu_sets': [dict(name='housekeeping', cpus=[0, 1]),
                                                   dict(name='server', cpus=list(range(2, 8))),
                                                   dict(name='client', cpus=list(range(8, 16)))]}

    def test_telemetry_guard_requires_no_local_fdbserver(self):
        # The single-node guard demands exactly one local fdbserver; on the client machine that is wrong.
        with self.assertRaisesRegex(generic.EnvironmentFailure, 'owned FDB server'):
            generic.telemetry_guard([sample(1), sample(2)], self.PLAN)
        cg.telemetry_guard([sample(1), sample(2)], self.PLAN)
        server = [dict(pid=7, comm='fdbserver')]
        with self.assertRaisesRegex(cg.EnvironmentFailure, 'fdbserver runs on the client'):
            cg.telemetry_guard([sample(1), sample(2, processes=server)], self.PLAN)
        for bad in ([sample(1)], [sample(1), sample(2, steal=2)], [sample(1), sample(4)]):
            with self.assertRaises(cg.EnvironmentFailure):
                cg.telemetry_guard(bad, self.PLAN)
        r1.check_pair(sample(1), sample(1.1), self.PLAN)
        oom = sample(2)
        oom['/proc/vmstat'] = 'oom_kill 1\n'
        with self.assertRaisesRegex(cg.EnvironmentFailure, 'OOM'):
            r1.check_pair(sample(1), oom, self.PLAN)

    def test_guarded_processes_refuse_fdbserver_and_builds_but_not_owned(self):
        with mock.patch.object(cg, 'process_table', return_value=[(1, 'systemd'), (10, 'client-final-go')]):
            r1.guarded_processes({10})
            with self.assertRaises(cg.EnvironmentFailure):
                r1.guarded_processes(set())
        for comm in ('fdbserver', 'bazel', 'go'):
            with self.subTest(comm), mock.patch.object(cg, 'process_table', return_value=[(5, comm)]):
                with self.assertRaises(cg.EnvironmentFailure):
                    r1.guarded_processes(set())

    def test_client_vm_policy(self):
        policy = cg.client_vm_policy(protocol())
        self.assertEqual([(s['name'], s['cpus'][0], s['cpus'][-1]) for s in policy['cpu_sets']],
                         [('housekeeping', 0, 1), ('server', 2, 7), ('client', 8, 15)])
        self.assertEqual(policy['cpu_sets'][1]['max_cpu_busy_fraction'], 0.5)
        overlap = protocol()
        overlap['runtime']['client_vm']['idle_sentinel_cpus'] = '2-8'
        with self.assertRaisesRegex(ValueError, 'overlap'):
            cg.client_vm_policy(overlap)


def code_pins(*modules):
    return sorted(cg.code_files(*modules))


class CampaignFixture(Temp):
    """Plans sealed by seal_multinode through the real frozen scripts, plus fakes for run.py's effects."""

    def setUp(self):
        super().setUp()
        if not HAVE_SCRIPTS:
            self.skipTest('frozen clientbench scripts not present')
        t = self.tmp
        self.protocol_doc = self.make_protocol()
        self.lay = layout()
        (t / 'protocol.json').write_text(json.dumps(self.protocol_doc))
        (t / 'orders.json').write_text(json.dumps(sm.make_orders(self.protocol_doc)))
        (t / 'layout.json').write_text(json.dumps(self.lay))
        (t / 'fdb.cluster').write_text(self.lay['cluster_file'] + '\n')
        for name in ['final', 'cgo', 'libfdb_c.so', 'fdbserver', 'harness.tar.gz', 'final-src.tar.gz']:
            (t / name).write_text('stand-in ' + name)
        (t / 'source-receipt.json').write_text(json.dumps(dict(
            harness_commit='d' * 40, production_revisions=self.protocol_doc['production_revisions'],
            source_artifacts_sha256={str((t / 'final-src.tar.gz').resolve()): generic.sha(t / 'final-src.tar.gz')})))
        pins = [t / 'layout.json', t / 'source-receipt.json', t / 'final-src.tar.gz', *code_pins(r1.cc, r1)]
        self.seal(pins)

    def seal(self, pins, out='plans'):
        """seal_multinode client-plans with the R1 adapter sealed into every campaign's run command."""
        t = self.tmp
        argv = ['client-plans', '--adapter', str(Path(r1.__file__).resolve()), '--layout', str(t / 'layout.json'),
                '--source-receipt', str(t / 'source-receipt.json'), '--vpc-cidr', VPC, '--protocol', str(t / 'protocol.json'), '--orders', str(t / 'orders.json'),
                '--scripts-dir', str(REAL_SCRIPTS), '--cluster-file', str(t / 'fdb.cluster'),
                '--native-library', str(t / 'libfdb_c.so'), '--server-binary', str(t / 'fdbserver'),
                '--harness-archive', str(t / 'harness.tar.gz'), '--binary', f'final={t / "final"}',
                '--binary', f'cgo={t / "cgo"}', '--results-dir', str(t / 'results'), '--output-dir', str(t / out),
                *sum([['--pin', str(p)] for p in pins], [])]
        with mock.patch('sys.stdout'):
            sm.main(argv)

    def make_protocol(self):
        return protocol()

    def args(self, plans='plans', workload=None, **over):
        """Exactly what the driver runs: the sealed run argv, rendered by seal_multinode.render_run."""
        manifest_path = self.tmp / plans / 'manifest.json'
        campaigns = json.loads(manifest_path.read_text())['campaigns']
        campaign = next((c for c in campaigns if c['workload'] == workload), campaigns[0])
        argv = sm.render_run(campaign['run'], generic.sha(manifest_path), '2099-01-01T00:00:00Z')
        self.assertEqual(argv[:4], ['python3', '-B', str(Path(r1.__file__).resolve()), 'run'])
        args = r1.parser().parse_args(argv[3:])
        if workload is not None and workload != campaign['workload']:
            over['workload'] = workload  # a name with no sealed campaign
        for key, value in over.items():
            setattr(args, key, value)
        return args

    def helper(self, execute=None):
        """run.py's interface: the real make_schedule; execute and effects are fakes in run.py's call order."""
        real = generic.load_helper(REAL_SCRIPTS / 'run.py')
        ns = types.SimpleNamespace(executed=[])
        clock = iter(range(1, 10 ** 6))

        def snapshot(dest):
            dest.mkdir()
            (dest / 'proc_vmstat').write_text('oom_kill 0\n')

        def telemetry(pid):
            return sample(next(clock))

        def fake_execute(plan, schedule, plan_path, out, deadline):
            for entry in schedule:
                run = out / entry['run_id']
                run.mkdir()
                ns.snapshot(run / 'before')
                ns.telemetry(4242)
                ns.telemetry(4242)
                ns.snapshot(run / 'after')
                ns.validate_raw_output(run, entry['workload'])
                ns.executed.append(entry['run_id'])
            generic.dump(out / 'complete.json', dict(scheduled=len(schedule), completed=len(ns.executed)))
            return 0
        ns.make_schedule = real.make_schedule
        ns.execute = execute or fake_execute
        ns.snapshot, ns.telemetry = snapshot, telemetry
        ns.validate_raw_output = lambda folder, workload: None
        ns.check_builds = lambda: None
        ns.descendants = lambda pid: {pid}
        ns.quiescence = lambda plan: dict(passed=True)
        ns.stop_process_group = lambda p: None
        return ns

    def run_adapter(self, args, statuses=None, helper=None, processes=((1, 'systemd'),)):
        helper = helper or self.helper()
        command = FakeCommand(statuses or [status(self.lay)])
        clock = FakeClock()
        with mock.patch.object(cg, 'process_table', return_value=list(processes)), \
                mock.patch('os.sched_getaffinity', return_value={0, 1}):
            rc = r1.run(args, command=command, helper=helper, sleep=clock.sleep, clock=clock)
        return rc, helper, command


class AdapterRunTest(CampaignFixture):
    def test_complete_campaign_runs_every_gate(self):
        rc, helper, command = self.run_adapter(self.args())
        out = self.tmp / 'results' / 'M1w1' / 'get100'
        self.assertEqual(rc, 0)
        self.assertEqual(len(helper.executed), 12)
        labels = [label for _, label in command.calls]
        self.assertEqual(labels[:3], ['adapter-campaign-before', 'adapter-campaign-clear-user-keys', 'adapter-campaign-after-clear'])
        self.assertEqual(labels[-1], 'adapter-campaign-after')
        self.assertEqual(labels.count('adapter-status-before'), 12)
        self.assertEqual(sum('clearrange' in argv[-1] for argv, _ in command.calls), 1)  # never per arm
        for argv, _ in command.calls:
            self.assertEqual(argv[:3], ['fdbcli', '-C', str((self.tmp / 'fdb.cluster').resolve())])
        for name in ('adapter-campaign-before-check-roles.json', 'adapter-campaign-after-check-roles.json', 'adapter.json'):
            self.assertTrue((out / name).is_file(), name)
        index = json.loads((out / 'case-receipts.json').read_text())
        self.assertEqual(sorted(index), ['check-roles-after.json', 'check-roles-before.json', 'status-after.json',
                                         'status-before.json'])
        self.assertTrue(all(generic.sha(out / n) == d for n, d in index.items()))
        run_dir = out / helper.executed[0]
        self.assertTrue(json.loads((run_dir / 'adapter-status-before-gate.json').read_text())['passed'])
        self.assertTrue(json.loads((run_dir / 'adapter-guard.json').read_text())['passed'])
        state = json.loads((out.parent / 'client-phase-state.json').read_text())
        self.assertEqual(state['cases']['get100'], dict(status='complete', runs=12))
        self.assertTrue((out.parent / 'client-phase-complete.json').is_file())
        self.assertEqual(json.loads((out / 'adapter.json').read_text())['harness_revision'], 'd' * 40)

    def test_wait_for_data_movement_retakes_quiescence(self):
        lay = self.lay
        moving = status(lay, mutate=lambda s: s['cluster']['data']['moving_data'].update(in_flight_bytes=9))
        # campaign-before, after-clear, first arm: moving, then settled for good.
        rc, helper, _ = self.run_adapter(self.args(), statuses=[status(lay), status(lay), moving, status(lay)])
        self.assertEqual(rc, 0)
        first = self.tmp / 'results' / 'M1w1' / 'get100' / helper.executed[0]
        self.assertTrue(json.loads((first / 'adapter-requiescence.json').read_text())['passed'])
        self.assertEqual(json.loads((first / 'adapter-status-before-gate.json').read_text())['polls'], 1)

    def adverse(self):
        return json.loads((self.tmp / 'results' / 'M1w1' / 'get100' / 'ADAPTER_ADVERSE.json').read_text())

    def test_environment_refusals_abort_the_whole_case(self):
        lay = self.lay
        moving = status(lay, mutate=lambda s: s['cluster']['data']['moving_data'].update(in_flight_bytes=9))
        cases = [
            ('single coordinator', dict(), 'not the generated layout'),
            ('memory engine', dict(statuses=[status(lay, mutate=lambda s: s['cluster']['configuration'].update(storage_engine='memory'))]), 'storage_engine'),
            ('DD frozen', dict(statuses=[status(lay, dd_disabled=True)]), 'data distribution is disabled'),
            ('movement never settles', dict(statuses=[status(lay), status(lay), moving]), 'data movement'),
            ('recovery during campaign', dict(statuses=[status(lay)] * 15 + [status(lay, generation=9)]), 'generation'),
            ('fdbserver on the client', dict(processes=[(1, 'systemd'), (77, 'fdbserver')]), 'fdbserver'),
        ]
        for i, (name, kw, text) in enumerate(cases):
            with self.subTest(name):
                self.setUp()
                if name == 'single coordinator':
                    (self.tmp / 'fdb.cluster').write_text('fdbbenchvpc:mn20261014@10.88.0.11:4500\n')
                    # Re-pin the edited cluster file so only the guard can refuse it.
                    self.seal([self.tmp / 'layout.json', self.tmp / 'source-receipt.json', self.tmp / 'final-src.tar.gz',
                               *code_pins(r1.cc, r1)], out='plans2')
                    args = self.args(plans='plans2')
                else:
                    args = self.args()
                with self.assertRaisesRegex(Exception, text):
                    self.run_adapter(args, **kw)
                adverse = self.adverse()
                self.assertEqual((adverse['status'], adverse['no_retry']), ('environment-aborted', True))

    def test_preflight_refusals_before_any_output(self):
        cases = [
            ('manifest not acknowledged', self.args(manifest_sha256='0' * 64), 'acknowledged'),
            ('wrong VPC flag', self.args(vpc_cidr='10.99.0.0/24'), None),
            ('unknown workload', self.args(workload='set100'), None),
        ]
        for name, args, text in cases:
            with self.subTest(name):
                with self.assertRaises(Exception) as caught:
                    self.run_adapter(args)
                if text:
                    self.assertIn(text, str(caught.exception))
        # The wrong VPC is refused by the cluster guard, inside the case; the others never create output.
        self.assertEqual(self.adverse()['status'], 'environment-aborted')

    def test_manifest_must_seal_plan_and_schedule_paths_and_hashes(self):
        manifest_path = self.tmp / 'plans' / 'manifest.json'
        original = manifest_path.read_text()
        schedule = self.tmp / 'plans' / 'M1w1' / 'get100' / 'schedule.json'
        for name, edit in [('no schedule_sha256', lambda c: c.pop('schedule_sha256')),
                           ('no schedule path', lambda c: c.pop('schedule')),
                           ('other plan path', lambda c: c.update(plan=c['plan'] + '.elsewhere')),
                           ('schedule hash differs', lambda c: c.update(schedule_sha256='0' * 64))]:
            with self.subTest(name):
                manifest = json.loads(original)
                edit(manifest['campaigns'][0])
                manifest_path.write_text(json.dumps(manifest))
                with self.assertRaisesRegex(ValueError, 'sealing manifest'):
                    self.run_adapter(self.args(manifest_sha256=generic.sha(manifest_path)))
        manifest_path.write_text(original)
        # Same schedule content, different bytes: only the sealed SHA-256 can tell.
        schedule.write_text(json.dumps(json.loads(schedule.read_text())))
        with self.assertRaisesRegex(ValueError, 'sealing manifest'):
            self.run_adapter(self.args())
        self.assertFalse((self.tmp / 'results' / 'M1w1' / 'get100').exists())

    def test_unpinned_adapter_code_is_refused(self):
        self.seal([self.tmp / 'layout.json', self.tmp / 'source-receipt.json', self.tmp / 'final-src.tar.gz'], out='bare')
        with self.assertRaisesRegex(ValueError, 'must be pinned'):
            self.run_adapter(self.args(plans='bare'))
        self.assertFalse((self.tmp / 'results' / 'M1w1' / 'get100').exists())

    def test_lifetime_gate_reserves_collection_and_teardown(self):
        import time
        deadline = str(int(time.time()) + 2700 + 600 + 299)  # cap + 600 s collection, but not + 300 s teardown
        rc, helper, _ = self.run_adapter(self.args(lifetime_deadline=deadline))
        self.assertEqual((rc, helper.executed), (2, []))
        self.assertTrue((self.tmp / 'results' / 'M1w1' / 'client-phase-unexecuted.json').is_file())

    def test_final_tree_correctness_failure_stops_the_phase(self):
        def failing(plan, schedule, plan_path, out, deadline):
            entry = next(e for e in schedule if e['arm'] == 'cgo')
            hooks_entry = entry
            run = out / hooks_entry['run_id']
            run.mkdir()
            helper.snapshot(run / 'before')
            raise ValueError('workload validation failed: wrong value')
        helper = self.helper(execute=failing)
        with self.assertRaisesRegex(ValueError, 'wrong value'):
            self.run_adapter(self.args(), helper=helper)
        adverse = self.adverse()
        self.assertEqual(adverse['status'], 'final correctness-failed')  # CGo is built from the final revision
        state = json.loads((self.tmp / 'results' / 'M1w1' / 'client-phase-state.json').read_text())
        self.assertTrue(state['final_failure'])

    def test_signal_killed_run_is_environmental_not_final(self):
        # run.py records GNU time's 128+9 exit for a SIGKILLed client, then raises; stderr may be empty.
        def killed(plan, schedule, plan_path, out, deadline):
            entry = next(e for e in schedule if e['arm'] == 'final')
            run = out / entry['run_id']
            run.mkdir()
            helper.snapshot(run / 'before')
            generic.dump(run / 'result.json', dict(run_id=entry['run_id'], returncode=137))
            (run / 'stderr.txt').write_text('')
            raise RuntimeError('workload failure: ' + entry['run_id'])
        helper = self.helper(execute=killed)
        with self.assertRaisesRegex(RuntimeError, 'workload failure'):
            self.run_adapter(self.args(), helper=helper)
        self.assertEqual(self.adverse()['status'], 'environment-aborted')
        state = json.loads((self.tmp / 'results' / 'M1w1' / 'client-phase-state.json').read_text())
        self.assertFalse(state.get('final_failure', False))

    def test_rerun_of_an_attempted_case_is_refused(self):
        self.run_adapter(self.args())
        import shutil
        shutil.rmtree(self.tmp / 'results' / 'M1w1' / 'get100')
        with self.assertRaisesRegex(cg.EnvironmentFailure, 'already attempted'):
            self.run_adapter(self.args())


class SealedPopulationTest(CampaignFixture):
    """The real nine-workload M1 spec; range1000 and mixed80_20 need the unreviewed new harness."""
    WORKLOADS = ('grv', 'get100', 'get1000', 'range100', 'range1000', 'set100', 'batchset50', 'batchset50x1000',
                 'mixed80_20')

    def make_protocol(self):
        return protocol(self.WORKLOADS, new_harness=('range1000', 'mixed80_20'))

    def test_all_sealed_workloads_run_in_order_past_the_unsealed_ones(self):
        manifest = json.loads((self.tmp / 'plans' / 'manifest.json').read_text())
        sealed = [c['workload'] for c in manifest['campaigns']]
        self.assertEqual(sealed, ['grv', 'get100', 'get1000', 'range100', 'set100', 'batchset50', 'batchset50x1000'])
        for workload in sealed:
            with self.subTest(workload):
                rc, _, _ = self.run_adapter(self.args(workload=workload))
                self.assertEqual(rc, 0)
        state = json.loads((self.tmp / 'results' / 'M1w1' / 'client-phase-state.json').read_text())
        self.assertEqual(state['expected_cases'], sealed)
        self.assertTrue((self.tmp / 'results' / 'M1w1' / 'client-phase-complete.json').is_file())
        self.assertEqual(json.loads((self.tmp / 'results' / 'M1w1' / 'set100' / 'adapter.json').read_text())
                         ['expected_case_population'], 7)

    def test_out_of_order_and_unsealed_workloads_are_refused(self):
        with self.assertRaisesRegex(cg.EnvironmentFailure, 'sealed workload prefix'):
            self.run_adapter(self.args(workload='get100'))
        with self.assertRaisesRegex(ValueError, 'differs from protocol|not a sealed workload'):
            self.run_adapter(self.args(workload='range1000'))

    def test_manifest_that_silently_drops_a_sealed_workload_is_refused(self):
        manifest_path = self.tmp / 'plans' / 'manifest.json'
        original = json.loads(manifest_path.read_text())
        for name, edit in [
                ('campaign dropped', lambda m: m.update(campaigns=[c for c in m['campaigns'] if c['workload'] != 'set100'])),
                ('frozen workload passed off as unsealed', lambda m: (
                    m.update(campaigns=[c for c in m['campaigns'] if c['workload'] != 'set100']),
                    m['unsealed'].append(dict(phase='M1w1', workload='set100', reason='forged')))),
                ('order changed', lambda m: m['campaigns'].insert(0, m['campaigns'].pop(1)))]:
            with self.subTest(name):
                manifest = json.loads(json.dumps(original))
                edit(manifest)
                manifest_path.write_text(json.dumps(manifest))
                with self.assertRaisesRegex(ValueError, 'protocol order minus'):
                    self.run_adapter(self.args(workload='grv', manifest_sha256=generic.sha(manifest_path)))
        self.assertFalse((self.tmp / 'results' / 'M1w1' / 'grv').exists())


class ValidatePlanTest(CampaignFixture):
    def test_sealed_plan_validates_and_tampering_is_refused(self):
        t = self.tmp
        plan = json.loads((t / 'plans' / 'M1w1' / 'get100' / 'plan.json').read_text())
        schedule = json.loads((t / 'plans' / 'M1w1' / 'get100' / 'schedule.json').read_text())
        orders = json.loads((t / 'orders.json').read_text())
        p = self.protocol_doc
        r1.validate_plan(plan, schedule, p, orders, 'M1w1', 'get100')

        def tamper(fn, target='plan'):
            pl, sc, pr_ = copy.deepcopy(plan), copy.deepcopy(schedule), copy.deepcopy(p)
            fn({'plan': pl, 'schedule': sc, 'protocol': pr_}[target])
            return pl, sc, pr_
        cases = [
            ('workers', tamper(lambda x: x['workloads'][0].update(workers=8))),
            ('cap', tamper(lambda x: x.update(campaign_wall_cap_seconds=600))),
            ('arm order', tamper(lambda x: x['arms'].reverse())),
            ('revision', tamper(lambda x: x['arms'][0].update(source_revision='c' * 40))),
            ('GOMAXPROCS', tamper(lambda x: x['environment'].update(GOMAXPROCS='16'))),
            ('cpu groups', tamper(lambda x: x['cpu_sets'][1].update(cpus=[2, 3]))),
            ('affinity', tamper(lambda x: x[0]['command'].__setitem__(2, '0-15'), 'schedule')),
            ('block order', tamper(lambda x: x.reverse() or [e.update(block=5 - e['block']) for e in x], 'schedule')),
            ('prefix length', tamper(lambda x: x[0]['command'].__setitem__(x[0]['command'].index('--prefix') + 1, 'short/'), 'schedule')),
            ('protocol client cpus', tamper(lambda x: x['runtime']['client_vm'].update(client_cpus='8-14'), 'protocol')),
        ]
        for name, (pl, sc, pr_) in cases:
            with self.subTest(name), self.assertRaises(ValueError):
                r1.validate_plan(pl, sc, pr_, orders, 'M1w1', 'get100')
        with self.assertRaises(ValueError):
            r1.validate_plan(plan, schedule, p, orders, 'M2rl', 'get100')


if __name__ == '__main__':
    unittest.main()
