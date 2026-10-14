#!/usr/bin/env python3
"""Unit tests for reconcile_telemetry.py. Each test builds one synthetic phase: three
fake nodes run the real node_sampler loop on synthetic /proc text and fake clocks,
the fake client writes chrony receipts, and every case gets synthetic status json,
check-roles and placement-compare receipts. No command or network is used."""
import contextlib
import copy
import datetime as dt
import io
import json
import os
from pathlib import Path
import sys
import unittest
from unittest import mock

HERE = Path(os.path.abspath(__file__)).parent
sys.path.insert(0, str(HERE))

import fdb_cluster  # noqa: E402
import mn_common as mc  # noqa: E402
import node_sampler as ns  # noqa: E402
import reconcile_telemetry as rt  # noqa: E402
from test_node_sampler import M0_NS, T0_NS, FakeHost, Temp, chrony_text, node_units  # noqa: E402

NODE_IPS = ['10.88.0.11', '10.88.0.12', '10.88.0.13']
PHASE = 'M2rl'


def utc(t):
    """Seconds after T0 -> run.py-style ISO UTC string (microseconds, +00:00)."""
    ns_ = T0_NS + round(t * 1e9)
    return dt.datetime.fromtimestamp(ns_ // 10**9, dt.timezone.utc).replace(microsecond=(ns_ % 10**9) // 1000).isoformat()


def status_from_layout(layout):
    roles_of = {'stateless': ['cluster_controller', 'master', 'data_distributor', 'ratekeeper'],
                'commit_proxy': ['commit_proxy'], 'grv_proxy': ['grv_proxy'], 'resolution': ['resolver'],
                'transaction': ['log'], 'storage': ['storage'], 'coordinator': []}
    procs = {}
    for i, p in enumerate(layout['processes']):
        roles = list(roles_of[p['process_class']]) + (['coordinator'] if p['address'] in layout['coordinators'] else [])
        procs[f'p{i}'] = dict(address=p['address'], class_type=p['process_class'],
                              locality=dict(machineid=p['node'], zoneid=p['node']),
                              roles=[dict(role=r, id=f'{r}{i}') for r in roles], uptime_seconds=100.0)
    return dict(client=dict(coordinators=dict(coordinators=[dict(address=a) for a in layout['coordinators']]),
                            database_status=dict(available=True, healthy=True)),
                cluster=dict(database_available=True, generation=7, recovery_state=dict(name='fully_recovered'),
                             processes=procs, data_distribution_disabled=True,
                             qos=dict(worst_durability_lag_storage_server=dict(seconds=0.2),
                                      worst_queue_bytes_log_server=1000),
                             data=dict(moving_data=dict(in_flight_bytes=0, in_queue_bytes=0)),
                             configuration=dict(redundancy_mode='double', storage_engine=layout['engine'],
                                                commit_proxies=1, grv_proxies=1, resolvers=1, logs=2)))


def stat_with(host, k, cpu, change):
    """/proc/stat at second k with cpu's counters edited by change(list) (None drops the line)."""
    rows = host.stat(k).splitlines()
    out = []
    for line in rows:
        if line.startswith(f'cpu{cpu} '):
            fields = [int(x) for x in line.split()[1:]]
            fields = change(fields)
            if fields is None:
                continue
            line = f'cpu{cpu} ' + ' '.join(map(str, fields))
        out.append(line)
    return '\n'.join(out) + '\n'


class Phase:
    """One synthetic phase; tweak attributes, then materialize() and reconcile()."""

    def __init__(self, tmp):
        self.tmp = tmp
        self.layout = fdb_cluster.build_layout(NODE_IPS, '10.88.0.10', '10.88.0.0/24', 'fdbbenchvpc', 'mn20261014',
                                               'ssd-2', '/srv/fdb-bench/cluster', '/usr/sbin/fdbserver',
                                               'fdb-bench-vpc', [])
        self.hosts = {}
        for i, node in enumerate(('node1', 'node2', 'node3')):
            ports = [p['port'] for p in self.layout['processes'] if p['node'] == node]
            self.hosts[node] = FakeHost(node, units=node_units(ports, base_pid=1000 * (i + 1)), stop_at=60)
        self.client_chrony = {'before': chrony_text(), 'after': chrony_text()}
        self.client_at = {'before': -5.0, 'after': 65.0}
        self.client_kinds = ['before', 'after']
        self.cases = {'A': [(10.0, 14.0), (16.0, 20.0)], 'B': [(35.0, 45.0)]}
        self.b_from_results = True
        self.expected = ['A', 'B']
        base = status_from_layout(self.layout)
        self.status = {c: {'before': copy.deepcopy(base), 'after': copy.deepcopy(base)} for c in self.cases}
        self.roles_override = {}      # (case, when) -> receipt dict
        self.placement_required = True
        self.dd = 'frozen'
        self.compare = {c: dict(equal=True, differences=[]) for c in self.cases}
        self.runner = {c: dict(omitted=False, adverse=[]) for c in self.cases}  # case -> runner_verdict fields
        self.manifest_edit = None
        self.after_sampling = None    # callback(phase) to corrupt sampler output

    def node_dir(self, node):
        return self.tmp / 'telemetry' / node

    def materialize(self):
        t = self.tmp
        mc.dump_json(t / 'layout.json', self.layout)
        for node, host in self.hosts.items():
            ns.run_sampler(host, self.node_dir(node), node, PHASE, {0, 1}, 1.0, 3600, ns.UNIT_GLOB, [], None,
                           ['node_sampler.py'], host.stop)
        (t / 'client').mkdir()
        receipts = []
        for kind in self.client_kinds:
            h = FakeHost('client', units={})
            h.mono = M0_NS + int(self.client_at[kind] * 1e9)
            h.chrony = [self.client_chrony[kind]]
            mc.dump_json(t / 'client' / f'{kind}.json',
                         ns.build_receipt(h, kind, 'client', PHASE, {}, False, ns.UNIT_GLOB, [], None, []))
            receipts.append(f'client/{kind}.json')
        cases = []
        for case, runs in self.cases.items():
            d = t / 'cases' / case
            d.mkdir(parents=True)
            entry = dict(case=case)
            if case == 'B' and self.b_from_results:
                entry['run_results'] = []
                for i, (s, e) in enumerate(runs):
                    mc.dump_json(d / f'result{i}.json', dict(run_id=f'{case}-{i}', started_utc=utc(s), ended_utc=utc(e),
                                                             process_wall_ns=1, returncode=0))
                    entry['run_results'].append(f'cases/{case}/result{i}.json')
            else:
                entry['runs'] = [dict(run_id=f'{case}-{i}', start_utc=utc(s), end_utc=utc(e)) for i, (s, e) in enumerate(runs)]
            for when in ('before', 'after'):
                status = self.status[case][when]
                (d / f'status-{when}.json').write_text(json.dumps(status))
                roles = self.roles_override.get((case, when)) or fdb_cluster.check_roles(self.layout, status)
                mc.dump_json(d / f'roles-{when}.json', roles)
                entry[f'status_{when}'] = f'cases/{case}/status-{when}.json'
                entry[f'roles_{when}'] = f'cases/{case}/roles-{when}.json'
            mc.dump_json(d / 'compare.json', self.compare[case])
            entry['placement_compares'] = [f'cases/{case}/compare.json']
            (d / 'runner.json').write_text(json.dumps({case: 'synthetic runner record'}))
            entry['runner_verdict'] = dict(source=str(d / 'runner.json'), source_sha256=ns.sha256_file(d / 'runner.json'),
                                           **self.runner[case])
            cases.append(entry)
        manifest = dict(format=rt.INPUT_FORMAT, phase=PHASE, layout='layout.json',
                        placement_required=self.placement_required, expected_dd_state=self.dd,
                        client=dict(host='client', receipts=receipts),
                        nodes={n: f'telemetry/{n}' for n in self.hosts}, expected_cases=self.expected, cases=cases)
        if self.after_sampling:
            self.after_sampling(self)
        if self.manifest_edit:
            self.manifest_edit(manifest)
        (t / 'input.json').write_text(json.dumps(manifest))
        return t / 'input.json'

    def reconcile(self):
        path = self.materialize()
        out = self.tmp / 'report.json'
        with contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(io.StringIO()):
            rc = rt.main(['--input', str(path), '--output', str(out)])
        return rc, (json.loads(out.read_text()) if out.exists() else None)


def rewrite_samples(phase, node, edit):
    """Edit a node's samples.jsonl lines and re-seal the stop receipt's sha256."""
    d = phase.node_dir(node)
    lines = (d / 'samples.jsonl').read_text().splitlines()
    (d / 'samples.jsonl').write_text('\n'.join(edit(lines)) + '\n')
    rewrite_receipt(phase, node, 'stop', lambda r: r.update(samples_sha256=ns.sha256_file(d / 'samples.jsonl')))


def rewrite_receipt(phase, node, kind, edit):
    path = phase.node_dir(node) / f'receipt-{kind}.json'
    r = json.loads(path.read_text())
    edit(r)
    path.write_text(json.dumps(r))


class Base(Temp):
    def setUp(self):
        super().setUp()
        self.p = Phase(self.tmp)

    def run_phase(self):
        rc, report = self.p.reconcile()
        self.assertIsNotNone(report, 'no report written')
        self.report = report
        return {c['case']: c['verdict'] for c in report['cases']}

    def case(self, name):
        return next(c for c in self.report['cases'] if c['case'] == name)

    def assertVerdicts(self, want, reason=None, cases=None):
        got = self.run_phase()
        self.assertEqual(got, want, json.dumps([self.case(c)[k] for c in got for k in ('contaminated_reasons', 'unverifiable_reasons')], indent=1)[:3000])
        if reason:
            for c in cases or [c for c, v in want.items() if v != 'clean']:
                kind = 'contaminated_reasons' if want[c] == 'contaminated' else 'unverifiable_reasons'
                self.assertTrue(any(reason in r for r in self.case(c)[kind]), (reason, self.case(c)[kind]))


CLEAN = {'A': 'clean', 'B': 'clean'}
ALL_UNVERIFIABLE = {'A': 'unverifiable', 'B': 'unverifiable'}
ALL_CONTAMINATED = {'A': 'contaminated', 'B': 'contaminated'}


class CleanPhaseTest(Base):
    def test_clean_phase_and_exact_samples(self):
        rc, report = self.p.reconcile()
        self.assertEqual(rc, 0)
        self.report = report
        self.assertEqual({c['case']: c['verdict'] for c in report['cases']}, CLEAN)
        self.assertTrue(report['reconciliation_complete'])
        self.assertEqual(report['phase_summary'], dict(cases=2, clean=2, contaminated=0, unverifiable=0,
                                                       analysis_eligible_cases=['A', 'B'], omitted_cases={}))
        b = self.case('B')['runs'][0]
        self.assertEqual(b['run_id'], 'B-0')
        node = b['nodes']['node2']
        # u = 0.0005 + 0.0005 + 1 + 0.001; window [35, 45] widens to [33.998, 46.002].
        self.assertEqual(node['u_s'], '1.002000000')
        self.assertEqual(node['samples_used'], [33, 47])
        self.assertEqual(node['intervals_counted'], 14)
        self.assertEqual(node['widened_start_ns'], T0_NS + 33_998_000_000)
        self.assertEqual(node['max_fdb_steal'], 0.0)
        self.assertEqual(self.case('A')['runs'][1]['nodes']['node1']['samples_used'], [14, 22])
        self.assertEqual(report['nodes']['node2']['fdb_cpus'], list(range(2, 12)))
        self.assertEqual(report['nodes']['node1']['fdb_cpus'], list(range(2, 16)))
        self.assertIn('telemetry/node3/samples.jsonl', report['inputs_sha256'])
        self.assertIn('cases/B/result0.json', report['inputs_sha256'])

    def test_report_exclusive_and_outside_repo(self):
        path = self.p.materialize()
        out = self.tmp / 'report.json'
        quiet = io.StringIO()
        with contextlib.redirect_stdout(quiet), contextlib.redirect_stderr(quiet):
            self.assertEqual(rt.main(['--input', str(path), '--output', str(out)]), 0)
            self.assertEqual(rt.main(['--input', str(path), '--output', str(out)]), 2)
            (self.tmp / 'repo' / '.git').mkdir(parents=True)
            self.assertEqual(rt.main(['--input', str(path), '--output', str(self.tmp / 'repo' / 'report.json')]), 2)
        self.assertFalse((self.tmp / 'repo' / 'report.json').exists())

    def test_no_report_unless_every_case_reconciled(self):
        path = self.p.materialize()
        out = self.tmp / 'report.json'
        real = rt.case_receipts
        calls = []

        def fail_second(*a):
            calls.append(1)
            if len(calls) == 2:
                raise RuntimeError('reconciliation interrupted')
            return real(*a)
        with mock.patch.object(rt, 'case_receipts', fail_second), self.assertRaises(RuntimeError):
            rt.main(['--input', str(path), '--output', str(out)])
        self.assertFalse(out.exists())


class ClockTest(Base):
    def test_u_formula_exact(self):
        self.p.client_chrony = {k: chrony_text(off='-0.000123456', disp='0.000010000', delay='0.000200000')
                                for k in ('before', 'after')}
        self.p.hosts['node1'].chrony = [chrony_text(off='0.000050000', disp='0.000005000', delay='0.000020000')] * 2
        self.assertVerdicts(CLEAN)
        clock = self.report['clock']
        self.assertEqual(clock['client']['bound_s'], '0.000233456')
        self.assertEqual(clock['nodes']['node1']['bound_s'], '0.000065000')
        self.assertEqual(clock['nodes']['node1']['u_s'], '1.001298456')
        self.assertEqual(clock['nodes']['node2']['u_s'], '1.001733456')

    def test_u_is_the_phase_maximum_per_host(self):
        self.p.hosts['node1'].chrony = [chrony_text(), chrony_text(off='0.300000000')]
        self.p.client_chrony['before'] = chrony_text(off='-0.200000000')
        self.assertVerdicts(CLEAN)
        clock = self.report['clock']
        # |off| + 0.0002 + 0.0004 / 2, the larger of each host's two receipts
        self.assertEqual(clock['client']['bound_s'], '0.200400000')
        self.assertEqual(clock['nodes']['node1']['bound_s'], '0.300400000')
        self.assertEqual(clock['nodes']['node1']['u_s'], '1.501800000')

    def opposite(self, run_end):
        self.p.client_chrony = {k: chrony_text(off='0.400000000', disp='0.000000000', delay='0.000000000')
                                for k in ('before', 'after')}
        self.p.hosts['node1'].chrony = [chrony_text(off='-0.400000000', disp='0.000000000', delay='0.000000000')] * 2
        self.p.cases['B'] = [(35.0, run_end)]
        self.p.hosts['node1'].cpu_events[(46, 3)] = (40, 50, 10)

    def test_opposite_sign_offsets_add(self):
        # u = |+0.4| + |-0.4| + 1.001 = 1.801: the interval starting at 46 s touches
        # [35 - u, 44.5 + u]. A signed sum (u = 1.001) or max(|off|) (1.401) would miss it.
        self.opposite(44.5)
        self.assertVerdicts({'A': 'clean', 'B': 'contaminated'}, 'node1 run B-0 samples 46-47: steal 10/100 on cpu3')
        self.assertEqual(self.report['clock']['nodes']['node1']['u_s'], '1.801000000')

    def test_opposite_sign_offsets_boundary_control(self):
        self.opposite(44.1)   # 44.1 + 1.801 < 46: the interval is outside
        self.assertVerdicts(CLEAN)

    def test_steal_just_inside_and_outside_widening(self):
        self.p.hosts['node2'].cpu_events[(46, 6)] = (48, 50, 2)
        self.assertVerdicts({'A': 'clean', 'B': 'contaminated'}, 'samples 46-47')

    def test_steal_beyond_widening_is_not_counted(self):
        self.p.hosts['node2'].cpu_events[(47, 6)] = (48, 50, 2)
        self.assertVerdicts(CLEAN)

    def test_missing_client_receipt(self):
        self.p.client_kinds = ['before']
        self.assertVerdicts(ALL_UNVERIFIABLE, 'no client after receipt')

    def test_client_receipt_file_missing(self):
        self.p.manifest_edit = lambda m: m['client']['receipts'].append('client/gone.json')
        self.assertVerdicts(ALL_UNVERIFIABLE, 'missing file client/gone.json')

    def test_missing_chrony_field(self):
        for field in ('System time', 'Root dispersion', 'Root delay'):
            with self.subTest(field=field):
                self.p = Phase(self.tmp / field.replace(' ', '_'))
                self.p.tmp.mkdir()
                self.p.hosts['node2'].chrony = [chrony_text(), chrony_text(drop=(field,))]
                self.assertVerdicts(ALL_UNVERIFIABLE, 'clock bound undefined: node2 stop receipt')

    def test_unsynchronised_clock(self):
        self.p.client_chrony['after'] = chrony_text(leap='Not synchronised')
        self.assertVerdicts(ALL_UNVERIFIABLE, 'Not synchronised')

    def test_pending_leap_second(self):
        self.p.hosts['node3'].chrony = [chrony_text(leap='Insert second'), chrony_text()]
        self.assertVerdicts(ALL_UNVERIFIABLE, 'Insert second')

    def test_chronyc_failed(self):
        self.p.hosts['node1'].chrony_rc = 1
        self.assertVerdicts(ALL_UNVERIFIABLE, 'chronyc tracking failed')

    def test_client_receipts_must_bracket_runs(self):
        self.p.client_at['before'] = 12.0
        self.assertVerdicts(ALL_UNVERIFIABLE, 'do not bracket')

    def test_receipt_for_another_phase(self):
        self.p.after_sampling = lambda p: rewrite_receipt(p, 'node1', 'start', lambda r: r.update(phase='M2skip'))
        self.assertVerdicts(ALL_UNVERIFIABLE, "phase='M2skip'")


class SampleDataTest(Base):
    def test_missing_sampler_file(self):
        self.p.after_sampling = lambda p: (p.node_dir('node2') / 'samples.jsonl').unlink()
        self.assertVerdicts(ALL_UNVERIFIABLE, 'node2 samples: missing')

    def test_missing_stop_receipt(self):
        self.p.after_sampling = lambda p: (p.node_dir('node3') / 'receipt-stop.json').unlink()
        self.assertVerdicts(ALL_UNVERIFIABLE, 'node3 receipt-stop: missing file')

    def test_unparsable_sample_line(self):
        self.p.after_sampling = lambda p: rewrite_samples(p, 'node1', lambda ls: ls[:30] + [ls[30][:50]] + ls[31:])
        self.assertVerdicts(ALL_UNVERIFIABLE, 'node1 samples line 31 unparsable')

    def test_samples_differ_from_stop_receipt(self):
        def append(p):
            with open(p.node_dir('node1') / 'samples.jsonl', 'a') as s:
                s.write('{}\n')
        self.p.after_sampling = append
        self.assertVerdicts(ALL_UNVERIFIABLE, 'differs from the stop receipt')

    def test_gap_over_two_seconds_in_window(self):
        self.p.hosts['node2'].extra_sleep = {15: 2.5}
        self.assertVerdicts({'A': 'unverifiable', 'B': 'clean'}, 'sample gap')

    def test_gap_outside_windows_is_harmless(self):
        self.p.hosts['node2'].extra_sleep = {27: 2.5}
        self.assertVerdicts(CLEAN)

    def test_window_not_covered(self):
        self.p.cases['A'] = [(0.5, 14.0)]
        self.assertVerdicts({'A': 'unverifiable', 'B': 'clean'}, 'do not cover the widened window')

    def test_sampler_stopped_early(self):
        self.p.hosts['node3'].stop_at = 40
        self.assertVerdicts({'A': 'clean', 'B': 'unverifiable'}, 'node3 run B-0: samples do not cover')

    # Sealed scope: one malformed sample anywhere on a node makes every case unverifiable,
    # even when it lies outside every run window (second 40 is inside B only, 27 in none).
    def test_unparsable_proc_stat(self):
        self.p.hosts['node1'].stat_override[15] = 'cpu 1 2 3\n'
        self.assertVerdicts(ALL_UNVERIFIABLE, 'node1 sample seq 15 unparsable')

    def test_malformed_proc_stat_cpu_line_anywhere(self):
        h = self.p.hosts['node2']
        h.stat_override[40] = stat_with(h, 40, 5, lambda f: f[:3] + ['x'] + f[4:])
        self.assertVerdicts(ALL_UNVERIFIABLE, 'node2 sample seq 40 unparsable: bad cpu counters')

    def test_malformed_vmstat_line_anywhere(self):
        self.p.hosts['node3'].vm_override[27] = 'nr_free_pages 1000\noom_kill 0\npgmajfault three\n'
        self.assertVerdicts(ALL_UNVERIFIABLE, 'node3 sample seq 27 unparsable: bad vmstat line')

    def test_missing_oom_kill(self):
        self.p.hosts['node1'].vm_override[40] = 'nr_free_pages 1\n'
        self.assertVerdicts(ALL_UNVERIFIABLE, 'vmstat lacks oom_kill')

    def test_missing_fdb_cpu(self):
        h = self.p.hosts['node2']
        h.stat_override[17] = stat_with(h, 17, 9, lambda f: None)
        self.assertVerdicts(ALL_UNVERIFIABLE, 'cpu9 missing')

    def test_missing_housekeeping_cpu_is_not_required(self):
        h = self.p.hosts['node2']
        h.stat_override[17] = stat_with(h, 17, 0, lambda f: None)
        self.assertVerdicts(CLEAN)

    def test_decreasing_counter(self):
        h = self.p.hosts['node2']
        # user falls by 10 while idle rises by 50: the total stays positive, the counter is still invalid
        h.stat_override[17] = stat_with(h, 17, 4, lambda f: [f[0] - 60] + f[1:])
        self.assertVerdicts({'A': 'unverifiable', 'B': 'clean'}, 'cpu4 counters invalid')

    def test_decreasing_iowait_tolerated(self):
        h = self.p.hosts['node2']
        h.stat_override[17] = stat_with(h, 17, 4, lambda f: f[:4] + [5] + f[5:])  # iowait 0 -> 5 -> 0
        self.assertVerdicts(CLEAN)

    def test_realtime_step(self):
        self.p.hosts['node3'].rt_steps = [(40.5, 500_000_000)]
        self.assertVerdicts({'A': 'clean', 'B': 'unverifiable'}, 'CLOCK_REALTIME stepped')

    def test_sample_read_error(self):
        self.p.after_sampling = lambda p: rewrite_samples(
            p, 'node1', lambda ls: [json.dumps({**json.loads(l), 'error': 'PermissionError()'}) if i == 40 else l
                                    for i, l in enumerate(ls)])
        self.assertVerdicts(ALL_UNVERIFIABLE, 'PermissionError')


class ContaminationTest(Base):
    def test_steal_over_one_percent(self):
        self.p.hosts['node2'].cpu_events[(15, 6)] = (48, 50, 2)
        self.assertVerdicts({'A': 'contaminated', 'B': 'clean'}, 'steal 2/100 on cpu6')

    def test_steal_exactly_one_percent_is_clean(self):
        self.p.hosts['node2'].cpu_events[(15, 6)] = (49, 50, 1)
        self.assertVerdicts(CLEAN)
        self.assertEqual(self.case('A')['runs'][0]['nodes']['node2']['max_fdb_steal'], 0.01)

    def test_steal_on_housekeeping_cpu_only(self):
        self.p.hosts['node2'].cpu_events[(15, 1)] = (40, 50, 10)
        self.assertVerdicts(CLEAN)

    def test_steal_between_cases(self):
        self.p.hosts['node2'].cpu_events[(27, 6)] = (40, 50, 10)
        self.assertVerdicts(CLEAN)

    def test_oom_kill(self):
        self.p.hosts['node3'].oom_events = {16}
        self.assertVerdicts({'A': 'contaminated', 'B': 'clean'}, 'oom_kill delta 1')

    def test_main_pid_change(self):
        self.p.hosts['node2'].unit_changes = [(30, 'fdb-bench-4502.service', dict(pid=7777, starttime=9999, enter=2))]
        self.assertVerdicts(ALL_CONTAMINATED, 'fdb-bench-4502.service MainPID changed 2002 -> 7777')
        self.assertTrue(any('MainPID gone or restarted' in r for r in self.case('B')['contaminated_reasons']))

    def test_nrestarts(self):
        self.p.hosts['node1'].unit_changes = [(55, 'fdb-bench-4504.service', dict(NRestarts=1))]
        self.assertVerdicts(ALL_CONTAMINATED, 'NRestarts=1')

    def test_unit_inactive(self):
        self.p.hosts['node3'].unit_changes = [(50, 'fdb-bench-4503.service', dict(ActiveState='failed', SubState='failed'))]
        self.assertVerdicts(ALL_CONTAMINATED, 'fdb-bench-4503.service inactive (failed/failed)')

    def test_unit_absent(self):
        del self.p.hosts['node3'].units['fdb-bench-4504.service']
        self.assertVerdicts(ALL_CONTAMINATED, 'fdb-bench-4504.service absent or not loaded')

    def test_active_enter_changed(self):
        self.p.hosts['node1'].unit_changes = [(55, 'fdb-bench-4500.service', dict(enter=42))]
        self.assertVerdicts(ALL_CONTAMINATED, 'ActiveEnterTimestampMonotonic changed')

    def test_boot_id_changed(self):
        self.p.hosts['node1'].boot_ids = ['00000000-0000-4000-8000-000000000001', '00000000-0000-4000-8000-000000000002']
        self.assertVerdicts(ALL_CONTAMINATED, 'boot_id changed')

    def test_process_gone_in_interval(self):
        h = self.p.hosts['node2']
        h.proc_hidden = {(16, 2003)}
        self.assertVerdicts({'A': 'contaminated', 'B': 'clean'}, 'fdb-bench-4503.service MainPID gone or restarted')

    def test_systemctl_failure_is_unverifiable(self):
        self.p.hosts['node2'].systemctl_rc = 1
        self.assertVerdicts(ALL_UNVERIFIABLE, 'systemctl failed')

    def mutate_after(self, case, fn):
        fn(self.p.status[case]['after'])

    def test_role_change(self):
        self.mutate_after('A', lambda s: s['cluster']['processes']['p5']['roles'][0].update(id='storage99'))
        self.assertVerdicts({'A': 'contaminated', 'B': 'clean'}, 'role or process-class change at 10.88.0.11:4505')

    def test_process_class_change(self):
        self.mutate_after('B', lambda s: s['cluster']['processes']['p9'].update(class_type='unset'))
        self.assertVerdicts({'A': 'clean', 'B': 'contaminated'}, 'role or process-class change at 10.88.0.12:4502')

    def test_check_roles_failed(self):
        self.p.roles_override[('A', 'after')] = dict(passed=False, errors=['x'])
        self.assertVerdicts({'A': 'contaminated', 'B': 'clean'}, 'check-roles after failed')

    def test_generation_change(self):
        self.mutate_after('B', lambda s: s['cluster'].update(generation=9))
        self.assertVerdicts({'A': 'clean', 'B': 'contaminated'}, 'recovery: generation 7 -> 9')

    def test_recovery_state(self):
        self.mutate_after('A', lambda s: s['cluster']['recovery_state'].update(name='accepting_commits'))
        self.assertVerdicts({'A': 'contaminated', 'B': 'clean'},
                            "status verdict after: recovery_state 'accepting_commits', not fully_recovered")

    def test_database_unavailable(self):
        self.mutate_after('A', lambda s: s['cluster'].update(database_available=False))
        self.assertVerdicts({'A': 'contaminated', 'B': 'clean'}, 'status verdict after: check-roles: database not available')

    def test_placement_changed(self):
        self.p.compare['B'] = dict(equal=False, differences=['1 keys changed servers'])
        self.assertVerdicts({'A': 'clean', 'B': 'contaminated'}, 'placement changed')

    def test_contamination_wins_over_unverifiable(self):
        self.p.hosts['node2'].cpu_events[(15, 6)] = (40, 50, 10)
        self.p.hosts['node1'].stat_override[12] = 'garbage\n'
        self.assertVerdicts({'A': 'contaminated', 'B': 'unverifiable'})
        self.assertTrue(self.case('A')['unverifiable_reasons'])
        self.assertEqual(self.report['phase_summary']['omitted_cases'], {'A': 'contaminated', 'B': 'unverifiable'})


class CaseReceiptTest(Base):
    def edit_case(self, case, fn):
        def edit(m):
            fn(next(c for c in m['cases'] if c['case'] == case))
        self.p.manifest_edit = edit

    def test_missing_status(self):
        self.edit_case('A', lambda c: c.pop('status_before'))
        self.assertVerdicts({'A': 'unverifiable', 'B': 'clean'}, 'status json before: no path given')

    def test_unparsable_status(self):
        self.p.after_sampling = lambda p: (p.tmp / 'cases/B/status-after.json').write_text('{"cluster": ')
        self.assertVerdicts({'A': 'clean', 'B': 'unverifiable'}, 'unparsable cases/B/status-after.json')

    def test_status_without_generation(self):
        del self.p.status['A']['before']['cluster']['generation']
        self.assertVerdicts({'A': 'unverifiable', 'B': 'clean'}, 'status json before: cluster.generation missing')

    def test_roles_receipt_without_verdict(self):
        self.p.roles_override[('B', 'before')] = dict(errors=[])
        self.assertVerdicts({'A': 'clean', 'B': 'unverifiable'}, 'check-roles before: no passed verdict')

    def test_missing_placement_compare(self):
        self.edit_case('A', lambda c: c.pop('placement_compares'))
        self.assertVerdicts({'A': 'unverifiable', 'B': 'clean'}, 'placement compare receipt missing')

    def test_placement_compare_without_verdict(self):
        self.p.compare['A'] = dict(differences=[])
        self.assertVerdicts({'A': 'unverifiable', 'B': 'clean'}, 'no equal verdict')

    def test_placement_not_required(self):
        self.p.placement_required = False
        self.edit_case('A', lambda c: c.pop('placement_compares'))
        self.assertVerdicts(CLEAN)

    def test_run_window_not_utc(self):
        self.edit_case('A', lambda c: c['runs'][0].update(start_utc=c['runs'][0]['start_utc'].replace('+00:00', '+02:00')))
        self.assertVerdicts({'A': 'unverifiable', 'B': 'clean'}, 'not UTC')

    def test_run_window_naive(self):
        self.edit_case('A', lambda c: c['runs'][0].update(start_utc=c['runs'][0]['start_utc'].replace('+00:00', '')))
        self.assertVerdicts({'A': 'unverifiable', 'B': 'clean'}, 'unusable window')

    def test_run_window_null(self):
        # build_reconcile_input.py writes this for a sealed run that left no result
        self.edit_case('A', lambda c: c['runs'].append(dict(run_id='A-9', start_utc=None, end_utc=None)))
        self.assertVerdicts({'A': 'unverifiable', 'B': 'clean'}, 'run A-9: unusable window')

    def test_run_ends_before_start(self):
        self.p.cases['A'] = [(14.0, 10.0)]
        self.assertVerdicts({'A': 'unverifiable', 'B': 'clean'}, 'ends before it starts')

    def test_missing_run_result(self):
        self.edit_case('B', lambda c: c['run_results'].append('cases/B/gone.json'))
        self.assertVerdicts({'A': 'clean', 'B': 'unverifiable'}, 'run result: missing file')

    def test_no_runs(self):
        self.edit_case('A', lambda c: c.update(runs=[]))
        self.assertVerdicts({'A': 'unverifiable', 'B': 'clean'}, 'no run windows')

    def test_missing_expected_case(self):
        self.p.expected = ['A', 'B', 'C']
        rc, report = self.p.reconcile()
        self.assertEqual(rc, 3)
        self.report = report
        self.assertEqual(self.case('C')['verdict'], 'unverifiable')
        self.assertEqual(self.case('C')['unverifiable_reasons'], ['no case receipts'])
        self.assertEqual(report['phase_summary']['analysis_eligible_cases'], ['A', 'B'])
        self.assertEqual(report['phase_summary']['omitted_cases'], {'C': 'unverifiable'})


class RefusalTest(Base):
    def refused(self, edit):
        self.p.manifest_edit = edit
        rc, report = self.p.reconcile()
        self.assertEqual(rc, 2)
        self.assertIsNone(report)

    def test_bad_format(self):
        self.refused(lambda m: m.update(format='v0'))

    def test_case_not_expected(self):
        self.refused(lambda m: m.update(expected_cases=['A']))

    def test_duplicate_case(self):
        self.refused(lambda m: m['cases'].append(dict(m['cases'][0])))

    def test_nodes_differ_from_layout(self):
        self.refused(lambda m: m['nodes'].pop('node3'))

    def test_placement_flag_required(self):
        self.refused(lambda m: m.pop('placement_required'))

    def test_layout_missing(self):
        self.refused(lambda m: m.update(layout='gone.json'))


class FailClosedTest(Base):
    """No empty or partial collection may make a check vacuous."""

    def refused(self, edit=None, layout=None):
        def edit_layout(p):
            lay = json.loads((p.tmp / 'layout.json').read_text())
            layout(lay)
            (p.tmp / 'layout.json').write_text(json.dumps(lay))
        if layout:
            self.p.after_sampling = edit_layout
        self.p.manifest_edit = edit
        rc, report = self.p.reconcile()
        self.assertEqual(rc, 2)
        self.assertIsNone(report)

    def test_empty_layout_and_no_nodes_refused(self):
        self.refused(lambda m: m.update(nodes={}), lambda lay: lay.update(processes=[], storage_servers=0))

    def test_layout_missing_a_node_refused(self):
        def drop(lay):
            lay['processes'] = [p for p in lay['processes'] if p['node'] != 'node3']
            lay['storage_servers'] = 6
        self.refused(lambda m: m['nodes'].pop('node3'), drop)

    def test_layout_process_without_cpus_refused(self):
        self.refused(layout=lambda lay: lay['processes'][3].update(cpus=[]))

    def test_layout_process_moved_cpus_refused(self):
        self.refused(layout=lambda lay: lay['processes'][3].update(cpus=[0, 1]))

    def test_layout_extra_process_refused(self):
        self.refused(layout=lambda lay: lay['processes'].append(dict(lay['processes'][-1], port=4599)))

    def test_layout_without_engine_refused(self):
        self.refused(layout=lambda lay: lay.pop('engine'))

    def test_node_dir_not_a_path_refused(self):
        self.refused(lambda m: m['nodes'].update(node2=''))

    def test_empty_expected_cases_refused(self):
        self.refused(lambda m: m.update(expected_cases=[], cases=[]))

    def test_dd_state_required(self):
        self.refused(lambda m: m.pop('expected_dd_state'))

    def test_dd_state_vocabulary(self):
        self.refused(lambda m: m.update(expected_dd_state='enabled', placement_required=False))

    def test_placement_needs_frozen_dd(self):
        self.refused(lambda m: m.update(expected_dd_state='on'))

    def test_unit_receipt_with_extra_unit(self):
        self.p.hosts['node2'].units['fdb-bench-4599.service'] = dict(pid=99, starttime=1, NRestarts=0,
                                                                      ActiveState='active', SubState='running', enter=1)
        self.assertVerdicts(ALL_UNVERIFIABLE, 'not exactly the layout units')

    def test_unit_receipt_missing_a_unit(self):
        def drop(p):
            rewrite_receipt(p, 'node1', 'stop', lambda r: r['units']['names'].remove('fdb-bench-4503.service'))
        self.p.after_sampling = drop
        self.assertVerdicts(ALL_UNVERIFIABLE, 'not exactly the layout units')

    def test_runs_not_a_list(self):
        self.p.manifest_edit = lambda m: m['cases'][0].update(runs='A-0')
        self.assertVerdicts({'A': 'unverifiable', 'B': 'clean'}, 'runs is not a list')

    def test_no_client_receipts(self):
        self.p.manifest_edit = lambda m: m['client'].update(receipts=[])
        self.assertVerdicts(ALL_UNVERIFIABLE, 'no chrony receipt')

    def test_undefined_bound_without_reason_still_unverifiable(self):
        with mock.patch.object(rt, 'host_bound', lambda receipts: (None, [])):
            self.assertVerdicts(ALL_UNVERIFIABLE, 'no clock bound')

    def test_node_without_cpus_or_units(self):
        self.p.materialize()
        node = rt.Node('node1', [], rt.Inputs(self.tmp), 'telemetry/node1', PHASE)
        self.assertIn('node1: no FDB CPUs or units to check', node.unverifiable)

    def test_window_without_counted_interval(self):
        self.p.materialize()
        procs = [p for p in self.p.layout['processes'] if p['node'] == 'node1']
        node = rt.Node('node1', procs, rt.Inputs(self.tmp), 'telemetry/node1', PHASE)
        node.intervals = []
        counted, reasons = node.window(T0_NS + 10 * 10**9, T0_NS + 11 * 10**9)
        self.assertEqual((counted, reasons), ([], ['no sample interval counted']))


class StatusVerdictTest(Base):
    """Each case's status json is re-judged by the runners' cluster_gate.status_verdict."""

    def after(self, case, fn):
        fn(self.p.status[case]['after'])

    def test_qos_lag_after_case_with_roles_unchanged(self):
        self.after('A', lambda s: s['cluster']['qos']['worst_durability_lag_storage_server'].update(seconds=64.5))
        self.assertVerdicts({'A': 'contaminated', 'B': 'clean'}, 'status verdict after: storage durability lag 64.5')

    def test_log_queue_before_case(self):
        self.p.status['B']['before']['cluster']['qos']['worst_queue_bytes_log_server'] = 200 * 1000 * 1000
        self.assertVerdicts({'A': 'clean', 'B': 'contaminated'}, 'status verdict before: log queue')

    def test_dd_enabled_in_frozen_phase(self):
        self.after('B', lambda s: s['cluster'].pop('data_distribution_disabled'))
        self.assertVerdicts({'A': 'clean', 'B': 'contaminated'}, 'data distribution is enabled')

    def test_movement_with_dd_frozen(self):
        self.after('A', lambda s: s['cluster']['data']['moving_data'].update(in_queue_bytes=5))
        self.assertVerdicts({'A': 'contaminated', 'B': 'clean'}, 'data movement after case: moving_data.in_queue_bytes=5')

    def dd_on(self):
        self.p.dd, self.p.placement_required = 'on', False
        for case in self.p.status.values():
            for st in case.values():
                st['cluster'].pop('data_distribution_disabled')

    def test_dd_on_phase_clean(self):
        self.dd_on()
        self.assertVerdicts(CLEAN)

    def test_dd_frozen_in_dd_on_phase(self):
        self.dd_on()
        self.p.status['A']['before']['cluster']['data_distribution_disabled'] = True
        self.assertVerdicts({'A': 'contaminated', 'B': 'clean'}, 'data distribution is disabled')

    def test_movement_after_case_with_dd_on_is_recorded_only(self):
        self.dd_on()
        self.after('A', lambda s: s['cluster']['data']['moving_data'].update(in_flight_bytes=7))
        self.assertVerdicts(CLEAN)
        self.assertEqual(self.case('A')['notes'], ['data movement after case (DD on): moving_data.in_flight_bytes=7'])

    def test_movement_before_case_with_dd_on(self):
        self.dd_on()
        self.p.status['B']['before']['cluster']['data']['moving_data']['in_flight_bytes'] = 7
        self.assertVerdicts({'A': 'clean', 'B': 'contaminated'}, 'data movement before case')

    def test_missing_qos_field_is_unverifiable(self):
        self.after('A', lambda s: s['cluster']['qos'].pop('worst_queue_bytes_log_server'))
        self.assertVerdicts({'A': 'unverifiable', 'B': 'clean'},
                            'status json after: cluster.qos.worst_queue_bytes_log_server missing')

    def test_missing_moving_data_with_dd_on_is_unverifiable(self):
        self.dd_on()
        self.after('B', lambda s: s['cluster'].pop('data'))
        self.assertVerdicts({'A': 'clean', 'B': 'unverifiable'}, 'cluster.data.moving_data.in_flight_bytes missing')

    def test_missing_moving_data_with_dd_frozen_is_accepted(self):
        self.after('B', lambda s: s['cluster'].pop('data'))
        self.assertVerdicts(CLEAN)


class RunnerVerdictTest(Base):
    def test_runner_omitted(self):
        self.p.runner['A'] = dict(omitted=True, adverse=['ABORTED: workload failure'])
        self.assertVerdicts({'A': 'contaminated', 'B': 'clean'}, 'runner omitted/adverse: ABORTED: workload failure')

    def test_runner_omitted_without_reason(self):
        self.p.runner['A'] = dict(omitted=True, adverse=[])
        self.assertVerdicts({'A': 'contaminated', 'B': 'clean'}, 'runner omitted/adverse: omitted')

    def test_runner_adverse_not_omitted(self):
        self.p.runner['B'] = dict(omitted=False, adverse=['complete.json missing'])
        self.assertVerdicts({'A': 'clean', 'B': 'contaminated'}, 'runner omitted/adverse: complete.json missing')

    def test_missing_runner_verdict(self):
        self.p.manifest_edit = lambda m: m['cases'][1].pop('runner_verdict')
        self.assertVerdicts({'A': 'clean', 'B': 'unverifiable'}, 'runner verdict missing')

    def test_runner_record_missing(self):
        self.p.runner['A'] = dict(omitted=None, adverse=[])
        self.assertVerdicts({'A': 'unverifiable', 'B': 'clean'}, 'runner record missing or without a verdict')

    def test_runner_record_changed(self):
        self.p.after_sampling = lambda p: (p.tmp / 'cases/A/runner.json').write_text('{}')
        self.assertVerdicts({'A': 'unverifiable', 'B': 'clean'}, 'differs from source_sha256')

    def test_runner_record_gone(self):
        self.p.after_sampling = lambda p: (p.tmp / 'cases/B/runner.json').unlink()
        self.assertVerdicts({'A': 'clean', 'B': 'unverifiable'}, 'unreadable (FileNotFoundError)')

    def test_runner_adverse_malformed(self):
        self.p.runner['A'] = dict(omitted=False, adverse='bad')
        self.assertVerdicts({'A': 'unverifiable', 'B': 'clean'}, 'adverse is not a list of strings')


class FormulaTest(unittest.TestCase):
    def test_utc_to_ns_exact(self):
        self.assertEqual(rt.utc_to_ns('2026-10-14T20:00:00.000001+00:00') % 10**9, 1000)
        self.assertEqual(rt.utc_to_ns('1970-01-01T00:00:01Z'), 10**9)
        with self.assertRaises(ValueError):
            rt.utc_to_ns('2026-10-14T20:00:00+01:00')

    def test_ceil_ns(self):
        from decimal import Decimal
        self.assertEqual(rt.s_to_ns_ceil(Decimal('1.0000000001')), 1_000_000_001)
        self.assertEqual(rt.s_to_ns_ceil(Decimal('1.002')), 1_002_000_000)


if __name__ == '__main__':
    unittest.main()
