#!/usr/bin/env python3
"""Unit tests for node_sampler.py. Synthetic /proc text and fake command runners
only: no systemctl, chronyc, fdbcli or real clock is used."""
import contextlib
import io
import json
import os
from pathlib import Path
import signal
import sys
import tempfile
import unittest

HERE = Path(os.path.abspath(__file__)).parent
sys.path.insert(0, str(HERE))

import mn_common as mc  # noqa: E402
import node_sampler as ns  # noqa: E402

T0_NS = 1_792_000_000 * 10**9   # 2026-10-14 (synthetic)
M0_NS = 5_000 * 10**9           # CLOCK_MONOTONIC at t = 0
READ_NS = 10_000                # every fake read takes 10 us
BOOT_ID = '00000000-0000-4000-8000-000000000001'


def chrony_text(off='0.000100000', disp='0.000200000', delay='0.000400000', leap='Normal', drop=()):
    """`chronyc -n tracking` output; off is signed seconds, negative = slow of NTP."""
    sign = 'slow' if off.startswith('-') else 'fast'
    lines = {'Reference ID': 'C0000201 (192.0.2.1)', 'Stratum': '3',
             'Ref time (UTC)': 'Wed Oct 14 20:00:00 2026',
             'System time': f'{off.lstrip("-")} seconds {sign} of NTP time',
             'Last offset': '-0.000001234 seconds', 'RMS offset': '0.000010000 seconds',
             'Frequency': '1.234 ppm slow', 'Residual freq': '-0.001 ppm', 'Skew': '0.012 ppm',
             'Root delay': f'{delay} seconds', 'Root dispersion': f'{disp} seconds',
             'Update interval': '64.2 seconds', 'Leap status': leap}
    return ''.join(f'{k:<16}: {v}\n' for k, v in lines.items() if k not in drop)


def proc_pid_stat(pid, starttime, user_ticks=1, system_ticks=1):
    fields = ['1'] * 18 + [str(starttime)] + ['0'] * 30
    fields[10], fields[11] = str(user_ticks), str(system_ticks)
    return f'{pid} (fdbserver) S ' + ' '.join(fields) + '\n'


class FakeHost:
    """A machine whose clocks, /proc and commands are synthetic and controllable.

    Counters: every CPU accrues (user, idle, steal) = (50, 50, 0) jiffies per second
    unless cpu_events[(second, cpu)] overrides that second. Sample k sees the sum over
    seconds < round(t)."""

    def __init__(self, name='node1', cpus=16, units=None, chrony=None, stop_at=60.0):
        self.name, self.cpus = name, cpus
        self.mono = M0_NS
        self.rt_steps = []            # (t, ns): realtime jumps by ns once t >= t
        self.extra_sleep = {}         # second -> extra seconds slept (overrun)
        self.cpu_events = {}          # (second, cpu) -> (user, idle, steal)
        self.oom_events = set()       # seconds in which one OOM kill happens
        self.stat_override = {}       # second -> /proc/stat text
        self.vm_override = {}         # second -> /proc/vmstat text
        self.proc_hidden = set()      # (second, pid): /proc/pid/stat unreadable
        self.units = dict(units or {})  # name -> dict(pid, starttime, NRestarts, ActiveState, SubState, enter)
        self.unit_changes = []        # (t, name, changes): applied once t >= t
        self.chrony = list(chrony or [chrony_text(), chrony_text()])
        self.chrony_rc = 0
        self.systemctl_rc = 0
        self.stop_at, self.stop = stop_at, ns.Stop()
        self.affinity, self.settable = {0, 1}, True
        self.boot_ids = [BOOT_ID]
        self.calls = []
        self.reads = []
        self.ticks_per_second = 100
        self.cgroup_overrides = {}
        self.realpaths = {}
        self.subdirs = {}
        self.directory_ids = {}
        self.task_overrides = {}

    def t(self):
        return (self.mono - M0_NS) / 1e9

    def tick(self):
        self.mono += READ_NS

    def clock_ns(self, which):
        self.tick()
        if which == 'monotonic':
            return self.mono
        return self.mono - M0_NS + T0_NS + sum(d for t, d in self.rt_steps if self.t() >= t)

    def sleep(self, seconds):
        second = round(self.t())
        self.mono += int(seconds * 1e9) + int(self.extra_sleep.get(second, 0) * 1e9)
        if self.t() >= self.stop_at and self.stop.reason is None:
            self.stop.reason = 'SIGTERM'

    def counters(self, k, cpu):
        user = idle = steal = 0
        for s in range(k):
            u, i, st = self.cpu_events.get((s, cpu), (50, 50, 0))
            user, idle, steal = user + u, idle + i, steal + st
        return [user, 0, 0, idle, 0, 0, 0, steal, 0, 0]

    def stat(self, k):
        rows = [self.counters(k, c) for c in range(self.cpus)]
        agg = [sum(col) for col in zip(*rows)]
        lines = ['cpu  ' + ' '.join(map(str, agg))] + [f'cpu{c} ' + ' '.join(map(str, r)) for c, r in enumerate(rows)]
        lines += ['intr 1 2 3 4 5', 'ctxt 100', 'btime 1791990000', 'processes 50', 'procs_running 1',
                  'procs_blocked 0', 'softirq 1 2 3']
        return '\n'.join(lines) + '\n'

    def apply_unit_changes(self):
        for t, name, changes in self.unit_changes:
            if self.t() >= t:
                self.units[name] = {**self.units[name], **changes}

    def read(self, path):
        self.reads.append(path)
        self.tick()
        k = round(self.t())
        if path in self.cgroup_overrides:
            value = self.cgroup_overrides[path]
            if isinstance(value, Exception):
                raise value
            return value
        if path == ns.CGROUP_ROOT + '/cgroup.controllers':
            return 'cpu io memory pids\n'
        if path.startswith(ns.CGROUP_ROOT + '/system.slice/'):
            unit, field = path[len(ns.CGROUP_ROOT + '/system.slice/'):].split('/')
            u = self.units[unit]
            if field == 'cgroup.procs':
                return str(u['pid']) + '\n'
            if field == 'cgroup.threads':
                return '\n'.join(map(str, u.get('threads', [u['pid']]))) + '\n'
            if field == 'cpu.stat':
                return f'usage_usec {100000 + k * 10000}\nuser_usec {60000 + k * 6000}\nsystem_usec {40000 + k * 4000}\n'
            raise FileNotFoundError(path)
        if path == '/proc/stat':
            return self.stat_override.get(k) or self.stat(k)
        if path == '/proc/vmstat':
            if k in self.vm_override:
                return self.vm_override[k]
            return f'nr_free_pages 1000\noom_kill {sum(1 for s in self.oom_events if s < k)}\npgmajfault 3\n'
        if path == '/proc/sys/kernel/random/boot_id':
            return (self.boot_ids.pop(0) if len(self.boot_ids) > 1 else self.boot_ids[0]) + '\n'
        if path.startswith('/proc/') and path.endswith('/stat'):
            pid = int(path.split('/')[2])
            self.apply_unit_changes()
            for u in self.units.values():
                if u['pid'] == pid and u['ActiveState'] == 'active' and (k, pid) not in self.proc_hidden:
                    return proc_pid_stat(pid, u['starttime'], u.get('user_ticks', 100 + 7 * k),
                                         u.get('system_ticks', 50 + 3 * k))
            raise FileNotFoundError(path)
        raise FileNotFoundError(path)

    def run(self, argv, timeout=30):
        self.calls.append(list(argv))
        self.tick()
        if argv[:2] == ['chronyc', '-n']:
            return self.chrony_rc, self.chrony.pop(0) if self.chrony else '', ''
        self.apply_unit_changes()
        if argv[:2] == ['systemctl', 'list-units']:
            return 0, ''.join(f'{n} loaded {u["ActiveState"]} {u["SubState"]} FDB bench\n'
                              for n, u in sorted(self.units.items())), ''
        if argv[:2] == ['systemctl', 'show']:
            blocks = []
            for n in argv[5:]:
                u = self.units.get(n)
                if u is None:
                    blocks.append(f'Id={n}\nLoadState=not-found\nActiveState=inactive\nSubState=dead\nMainPID=0\nNRestarts=0\n')
                    continue
                blocks.append(f'Id={n}\nLoadState=loaded\nActiveState={u["ActiveState"]}\nSubState={u["SubState"]}\n'
                              f'MainPID={u["pid"] if u["ActiveState"] == "active" else 0}\nNRestarts={u["NRestarts"]}\n'
                              f'ActiveEnterTimestamp=Wed 2026-10-14 20:00:00 UTC\n'
                              f'ActiveEnterTimestampMonotonic={u["enter"]}\n')
                if 'ControlGroup' in argv[4].split(','):
                    blocks[-1] += 'ControlGroup=' + u.get('ControlGroup', '/system.slice/' + n) + '\n'
            return self.systemctl_rc, '\n'.join(blocks), ''
        if argv[0] == 'fdbcli':
            return 0, '{"cluster": {}}', ''
        return 127, '', 'unexpected command'

    def get_affinity(self):
        return set(self.affinity)

    def set_affinity(self, cpus):
        if self.settable:
            self.affinity = set(cpus)

    def hostname(self):
        return 'fdb-bench-vpc-' + self.name

    def clock_ticks_per_second(self):
        return self.ticks_per_second

    def realpath(self, path):
        return self.realpaths.get(path, path)

    def subdirectories(self, path):
        return self.subdirs.get(path, [])

    def directory_identity(self, path):
        return self.directory_ids.get(path, dict(device=42, inode=10 if path == ns.CGROUP_ROOT else
                                                10000 + int(path.rsplit('fdb-bench-', 1)[1].split('.')[0])))

    def listdir(self, path):
        if path in self.task_overrides:
            return self.task_overrides[path]
        pid = int(path.split('/')[2])
        for unit in self.units.values():
            if unit['pid'] == pid:
                return list(map(str, unit.get('threads', [pid])))
        raise FileNotFoundError(path)


def node_units(ports, base_pid=1000):
    return {f'fdb-bench-{p}.service': dict(pid=base_pid + i, starttime=500 + i, NRestarts=0, ActiveState='active',
                                           SubState='running', enter=1_000_000 + i) for i, p in enumerate(ports)}


class Temp(unittest.TestCase):
    def setUp(self):
        self._tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self._tmp.cleanup)
        self.tmp = Path(self._tmp.name)
        # The tools refuse outputs inside a Git work tree; a TMPDIR there is a broken fixture.
        self.assertFalse(mc.inside_git_worktree(self.tmp), f'TMPDIR {self.tmp} lies inside a Git work tree')


class ParserTest(unittest.TestCase):
    def test_proc_stat(self):
        cpus = ns.parse_proc_stat(FakeHost().stat(3))
        self.assertEqual(cpus['cpu5'][:8], [150, 0, 0, 150, 0, 0, 0, 0])
        self.assertEqual(cpus['cpu'][0], 16 * 150)
        for bad in ['cpu0 1 2 3\ncpu 1 2 3 4 5 6 7 8\n', 'cpu 1 2 3 4 5 6 7 x\n', 'cpu0 1 2 3 4 5 6 7 8\n',
                    'cpu 1 2 3 4 5 6 7 8\ncpu 1 2 3 4 5 6 7 8\n', 'cpux 1 2 3 4 5 6 7 8\n']:
            with self.subTest(bad=bad), self.assertRaises(ValueError):
                ns.parse_proc_stat(bad)

    def test_vmstat(self):
        self.assertEqual(ns.parse_vmstat('oom_kill 2\nnr_free_pages 9\n'), dict(oom_kill=2, nr_free_pages=9))
        for bad in ['oom_kill two\n', 'oom_kill 1\noom_kill 1\n', 'oom kill 1\n']:
            with self.subTest(bad=bad), self.assertRaises(ValueError):
                ns.parse_vmstat(bad)

    def test_chrony_signs_and_fields(self):
        fields, errors = ns.parse_chrony_tracking(chrony_text(off='-0.000123456', disp='0.000010000', delay='0.000200000'))
        self.assertEqual(errors, [])
        self.assertEqual(fields, dict(system_time_offset_s='-0.000123456', root_dispersion_s='0.000010000',
                                      root_delay_s='0.000200000', leap_status='Normal'))
        fast, _ = ns.parse_chrony_tracking(chrony_text(off='0.5'))
        self.assertEqual(fast['system_time_offset_s'], '0.5')
        from decimal import Decimal
        self.assertEqual(ns.chrony_error_bound(fields), Decimal('0.000233456'))

    def test_chrony_missing_or_bad_fields(self):
        for drop in ('System time', 'Root delay', 'Root dispersion', 'Leap status'):
            with self.subTest(drop=drop):
                fields, errors = ns.parse_chrony_tracking(chrony_text(drop=(drop,)))
                self.assertTrue(any(drop in e for e in errors), errors)
                with self.assertRaises(ValueError):
                    ns.chrony_error_bound(fields)
        _, errors = ns.parse_chrony_tracking(chrony_text().replace('0.000400000 seconds', 'n/a seconds'))
        self.assertTrue(any('Root delay' in e for e in errors))
        _, errors = ns.parse_chrony_tracking(chrony_text() + 'Root delay      : 0.1 seconds\n')
        self.assertTrue(any('repeated' in e for e in errors))
        _, errors = ns.parse_chrony_tracking(chrony_text(leap='Sometimes'))
        self.assertTrue(any('Leap status' in e for e in errors))
        self.assertEqual(ns.parse_chrony_tracking('')[1][0], "chrony field 'System time' missing or repeated")

    def test_chrony_unsynchronised_and_leap_refused(self):
        for leap in ('Not synchronised', 'Insert second', 'Delete second'):
            with self.subTest(leap=leap):
                fields, errors = ns.parse_chrony_tracking(chrony_text(leap=leap))
                self.assertEqual(errors, [])
                with self.assertRaisesRegex(ValueError, leap):
                    ns.chrony_error_bound(fields)

    def test_systemctl_show(self):
        units = ns.parse_systemctl_show('Id=a.service\nMainPID=3\n\nId=b.service\nMainPID=0\n')
        self.assertEqual(units['a.service']['MainPID'], '3')
        for bad in ['MainPID=3\n', 'Id=a\n\nId=a\n', 'Id=a\nnonsense\n']:
            with self.subTest(bad=bad), self.assertRaises(ValueError):
                ns.parse_systemctl_show(bad)

    def test_proc_starttime(self):
        self.assertEqual(ns.proc_starttime(proc_pid_stat(7, 4242)), '4242')
        self.assertEqual(ns.proc_starttime('7 (fdb server) S ' + ' '.join(['1'] * 18 + ['99'])), '99')

    def test_proc_cpu(self):
        text = proc_pid_stat(7, 4242, 123, 45).replace('(fdbserver)', '(fdb ) server)')
        self.assertEqual(ns.proc_cpu(text, 7), dict(pid=7, starttime='4242', user_ticks=123, system_ticks=45))
        self.assertEqual(ns.proc_cpu(proc_pid_stat(7, 4242, 0, 0), 7)['user_ticks'], 0)
        for user, system, start in [('x', 1, 99), (1, -1, 99), (1, 1, 'bad')]:
            with self.subTest(user=user, system=system, start=start), self.assertRaises(ValueError):
                ns.proc_cpu(proc_pid_stat(7, start, user, system), 7)
        with self.assertRaises(IndexError):
            ns.proc_cpu('7 (short) S 1', 7)

    def test_cpu_list(self):
        self.assertEqual(ns.parse_cpus('0,1'), {0, 1})
        self.assertEqual(ns.parse_cpus('0-2,5'), {0, 1, 2, 5})
        with self.assertRaises(mc.GuardError):
            ns.parse_cpus('0,a')


class AffinityTest(unittest.TestCase):
    def test_subset_kept(self):
        h = FakeHost()
        h.affinity = {1}
        self.assertEqual(ns.pin_housekeeping(h, {0, 1}), dict(before=[1], after=[1], housekeeping=[0, 1]))

    def test_pins_when_outside(self):
        h = FakeHost()
        h.affinity = set(range(16))
        self.assertEqual(ns.pin_housekeeping(h, {0, 1})['after'], [0, 1])

    def test_refuses_when_pin_fails(self):
        h = FakeHost()
        h.affinity, h.settable = set(range(16)), False
        with self.assertRaisesRegex(mc.GuardError, 'not within housekeeping'):
            ns.pin_housekeeping(h, {0, 1})


class SampleTest(unittest.TestCase):
    def test_sample_stamps_and_raw_text(self):
        h = FakeHost()
        s = ns.take_sample(h, 4, 4, {'fdb-bench-4500.service': 1000})
        self.assertEqual(s['format'], ns.SAMPLE_FORMAT)
        self.assertLess(s['realtime_ns'], s['realtime_after_ns'])
        self.assertEqual(s['monotonic_ns'] - M0_NS, s['realtime_ns'] - T0_NS + READ_NS)
        self.assertNotIn('intr', s['stat'])
        self.assertNotIn('softirq', s['stat'])
        self.assertIn('procs_running 1', s['stat'])
        self.assertEqual(ns.parse_proc_stat(s['stat'])['cpu0'][0], 0)
        self.assertIn('oom_kill 0', s['vmstat'])
        self.assertEqual(s['procs'], {'fdb-bench-4500.service': None})
        self.assertEqual(s['process_cpu_format'], ns.PROCESS_CPU_FORMAT)
        self.assertEqual(s['process_cpu'], {'fdb-bench-4500.service': None})
        self.assertIn('FileNotFoundError', s['process_cpu_errors']['fdb-bench-4500.service'])

    def test_process_cpu_uses_one_stat_read_and_keeps_legacy_identity(self):
        unit = 'fdb-bench-4500.service'
        h = FakeHost(units=node_units([4500]))
        s = ns.take_sample(h, 0, 0, {unit: 1000})
        self.assertEqual(s['procs'], {unit: '500'})
        self.assertEqual(s['process_cpu'], {unit: dict(pid=1000, starttime='500', user_ticks=100, system_ticks=50)})
        self.assertEqual(h.reads, ['/proc/stat', '/proc/vmstat', '/proc/1000/stat'])
        self.assertNotIn('process_cpu_errors', s)
        h.units[unit]['user_ticks'] = 'bad'
        s = ns.take_sample(h, 1, 1, {unit: 1000})
        self.assertEqual(s['procs'], {unit: '500'})
        self.assertEqual(s['process_cpu'], {unit: None})
        self.assertIn('ValueError', s['process_cpu_errors'][unit])
        self.assertNotIn('error', s)  # Legacy R4 fields do not gain a CPU parse error.

    def test_process_cpu_restart_boundaries(self):
        unit = 'fdb-bench-4500.service'
        h = FakeHost(units=node_units([4500]))
        before = ns.take_sample(h, 0, 0, {unit: 1000})
        h.units[unit].update(starttime=900, user_ticks=0, system_ticks=0)
        reused = ns.take_sample(h, 1, 1, {unit: 1000})
        self.assertEqual(before['process_cpu'][unit]['starttime'], '500')
        self.assertEqual(reused['process_cpu'][unit], dict(pid=1000, starttime='900', user_ticks=0, system_ticks=0))
        self.assertEqual(reused['procs'][unit], '900')
        h.units[unit]['pid'] = 2000
        replaced = ns.take_sample(h, 2, 2, {unit: 1000})
        self.assertIsNone(replaced['procs'][unit])
        self.assertIsNone(replaced['process_cpu'][unit])
        self.assertIn('FileNotFoundError', replaced['process_cpu_errors'][unit])
        receipt = ns.unit_receipt(h, ns.UNIT_GLOB, [unit])
        self.assertEqual(receipt['starttimes'][unit], dict(pid=2000, starttime='900'))

    def test_read_error_recorded(self):
        h = FakeHost()
        h.read = lambda path: (_ for _ in ()).throw(PermissionError(path))
        s = ns.take_sample(h, 0, 0, {})
        self.assertIn('PermissionError', s['error'])


class CgroupTest(Temp):
    unit = 'fdb-bench-4500.service'
    path = ns.CGROUP_ROOT + '/system.slice/' + unit

    def host_and_receipt(self):
        host = FakeHost(units=node_units([4500]))
        host.units[self.unit]['threads'] = [1000, 1002, 1001]
        return host, self.receipt(host)

    def receipt(self, host):
        return ns.build_receipt(host, 'start', 'node1', 'M1w1', {}, True, ns.UNIT_GLOB, [self.unit], None, [],
                                cgroup_cpu=True)

    def test_cpu_stat_parser_requires_usage_and_paired_optional_breakdown(self):
        self.assertEqual(ns.parse_cpu_stat('usage_usec 0\n'), dict(usage_usec=0))
        self.assertEqual(ns.parse_cpu_stat('usage_usec 123\nuser_usec 100\nsystem_usec 23\nnr_throttled 0\n'),
                         dict(usage_usec=123, user_usec=100, system_usec=23))
        for text in ('', 'user_usec 1\nsystem_usec 1\n', 'usage_usec -1\n', 'usage_usec 1.5\n',
                     'usage_usec 1\nusage_usec 2\n', 'usage_usec 1\nuser_usec 1\n',
                     'usage_usec 1\nsystem_usec 1\n', 'usage_usec true\n'):
            with self.subTest(text=text), self.assertRaises(mc.GuardError):
                ns.parse_cpu_stat(text)

    def test_receipt_and_sample_prove_precise_source_without_sample_cli(self):
        host, receipt = self.host_and_receipt()
        identity = dict(path=self.path, pid=1000, starttime='500', procs=[1000], threads=[1000, 1001, 1002],
                        device=42, inode=14500)
        self.assertEqual(receipt['cgroup_cpu_format'], ns.CGROUP_CPU_FORMAT)
        self.assertEqual(receipt['cgroup_units'], {self.unit: identity})
        self.assertEqual(receipt['cgroup_v2'], dict(path=ns.CGROUP_ROOT, controllers=['cpu', 'io', 'memory', 'pids'],
                                                   device=42, inode=10))
        calls = list(host.calls)
        sample = ns.take_sample(host, 0, 0, {self.unit: 1000}, receipt['cgroup_units'], {})
        self.assertEqual(host.calls, calls)
        self.assertEqual(sample['cgroup_cpu'][self.unit], dict(**identity, usage_usec=100000, user_usec=60000,
                                                              system_usec=40000))
        self.assertNotIn('cgroup_cpu_errors', sample)
        self.assertEqual(sample['procs'], {self.unit: '500'})
        self.assertEqual(sample['process_cpu'][self.unit]['pid'], 1000)

    def test_positive_receipt_cli_forwards_explicit_cgroup_source(self):
        host, expected = self.host_and_receipt()
        output = self.tmp / 'cgroup-before.json'
        with contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(io.StringIO()):
            rc = ns.main(['receipt', '--host', 'node1', '--phase', 'M1w1', '--kind', 'before',
                          '--unit', self.unit, '--cgroup-cpu', '--output', str(output)], host=host)
        self.assertEqual(rc, 0)
        receipt = json.loads(output.read_text())
        self.assertEqual(receipt['kind'], 'before')
        self.assertEqual(receipt['cgroup_cpu_format'], 'fdb-bench-cgroup-cpu/v1')
        self.assertEqual(receipt['cgroup_v2'], expected['cgroup_v2'])
        self.assertEqual(receipt['cgroup_units'], expected['cgroup_units'])
        self.assertEqual(receipt['units']['names'], [self.unit])

    def test_positive_run_cli_forwards_explicit_cgroup_source_and_collects_nonempty_stream(self):
        host, expected = self.host_and_receipt()
        output = self.tmp / 'cgroup-cli-run'
        handlers = {sig: signal.getsignal(sig) for sig in (signal.SIGTERM, signal.SIGINT)}
        try:
            with contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(io.StringIO()):
                rc = ns.main(['run', '--host', 'node1', '--phase', 'M1w1', '--unit', self.unit,
                              '--cgroup-cpu', '--interval', '0.01', '--max-seconds', '1',
                              '--output-dir', str(output)], host=host)
        finally:
            for sig, handler in handlers.items():
                signal.signal(sig, handler)
        self.assertEqual(rc, 0)
        start, stop = (json.loads((output / ('receipt-' + kind + '.json')).read_text()) for kind in ('start', 'stop'))
        samples = [json.loads(line) for line in (output / 'samples.jsonl').read_text().splitlines()]
        self.assertGreater(len(samples), 10)
        self.assertEqual(stop['samples'], len(samples))
        self.assertEqual(stop['stop_reason'], 'max-seconds')
        self.assertEqual(stop['samples_sha256'], ns.sha256_file(output / 'samples.jsonl'))
        for receipt in (start, stop):
            self.assertEqual(receipt['cgroup_cpu_format'], 'fdb-bench-cgroup-cpu/v1')
            self.assertEqual(receipt['cgroup_v2'], expected['cgroup_v2'])
            self.assertEqual(receipt['cgroup_units'], expected['cgroup_units'])
        for seq, sample in enumerate(samples):
            self.assertEqual(sample['seq'], seq)
            self.assertEqual(sample['cgroup_cpu_format'], 'fdb-bench-cgroup-cpu/v1')
            self.assertNotIn('cgroup_cpu_errors', sample)
            counter = sample['cgroup_cpu'][self.unit]
            self.assertEqual({key: counter[key] for key in expected['cgroup_units'][self.unit]},
                             expected['cgroup_units'][self.unit])
            self.assertGreaterEqual(counter['usage_usec'], 100000)
            self.assertEqual(set(sample['cgroup_cpu']), {self.unit})

    def test_source_proof_refuses_unsupported_foreign_empty_duplicate_and_nested_groups(self):
        for mutation in ('v1', 'controller', 'foreign_group', 'path_escape', 'nested', 'foreign_pid', 'empty_pid',
                         'duplicate_pid', 'threads', 'missing_main_thread', 'duplicate_thread', 'inactive', 'cpu_stat'):
            with self.subTest(mutation=mutation):
                host = FakeHost(units=node_units([4500]))
                if mutation == 'v1': host.cgroup_overrides[ns.CGROUP_ROOT + '/cgroup.controllers'] = FileNotFoundError('v1')
                if mutation == 'controller': host.cgroup_overrides[ns.CGROUP_ROOT + '/cgroup.controllers'] = 'memory\n'
                if mutation == 'foreign_group': host.units[self.unit]['ControlGroup'] = '/unrelated.service'
                if mutation == 'path_escape': host.realpaths[self.path] = '/somewhere/else'
                if mutation == 'nested': host.subdirs[self.path] = ['nested']
                if mutation == 'foreign_pid': host.cgroup_overrides[self.path + '/cgroup.procs'] = '1000\n2000\n'
                if mutation == 'empty_pid': host.cgroup_overrides[self.path + '/cgroup.procs'] = ''
                if mutation == 'duplicate_pid': host.cgroup_overrides[self.path + '/cgroup.procs'] = '1000\n1000\n'
                if mutation == 'threads': host.cgroup_overrides[self.path + '/cgroup.threads'] = '1000\n1001\n'
                if mutation == 'missing_main_thread': host.cgroup_overrides[self.path + '/cgroup.threads'] = '1001\n'
                if mutation == 'duplicate_thread': host.cgroup_overrides[self.path + '/cgroup.threads'] = '1000\n1000\n'
                if mutation == 'inactive': host.units[self.unit]['ActiveState'] = 'inactive'
                if mutation == 'cpu_stat': host.cgroup_overrides[self.path + '/cpu.stat'] = 'usage_usec -1\n'
                with self.assertRaises((mc.GuardError, OSError)):
                    self.receipt(host)

    def test_changed_sample_identity_membership_or_counter_disqualifies_without_proc_fallback(self):
        for mutation in ('reset', 'optional_disappears', 'foreign_pid', 'threads', 'tasks', 'inode', 'pid_reuse', 'read'):
            with self.subTest(mutation=mutation):
                host, receipt = self.host_and_receipt()
                previous = {}
                ns.take_sample(host, 0, 0, {self.unit: 1000}, receipt['cgroup_units'], previous)
                if mutation == 'reset': host.cgroup_overrides[self.path + '/cpu.stat'] = 'usage_usec 1\nuser_usec 1\nsystem_usec 0\n'
                if mutation == 'optional_disappears': host.cgroup_overrides[self.path + '/cpu.stat'] = 'usage_usec 100000\n'
                if mutation == 'foreign_pid': host.cgroup_overrides[self.path + '/cgroup.procs'] = '1000\n2000\n'
                if mutation == 'threads': host.units[self.unit]['threads'] = [1000, 1001, 1002, 1003]
                if mutation == 'tasks': host.task_overrides['/proc/1000/task'] = ['1000']
                if mutation == 'inode': host.directory_ids[self.path] = dict(device=42, inode=99999)
                if mutation == 'pid_reuse': host.units[self.unit]['starttime'] = 999
                if mutation == 'read': host.cgroup_overrides[self.path + '/cpu.stat'] = PermissionError('cpu.stat')
                sample = ns.take_sample(host, 1, 1, {self.unit: 1000}, receipt['cgroup_units'], previous)
                self.assertIsNone(sample['cgroup_cpu'][self.unit])
                self.assertIn(self.unit, sample['cgroup_cpu_errors'])
                self.assertIsNotNone(sample['process_cpu'][self.unit])

    def test_flag_off_has_no_cgroup_reads_or_extended_systemctl_properties(self):
        host = FakeHost(units=node_units([4500]))
        receipt = ns.build_receipt(host, 'start', 'node1', 'M1w1', {}, True, ns.UNIT_GLOB, [self.unit], None, [])
        sample = ns.take_sample(host, 0, 0, {self.unit: 1000})
        self.assertNotIn('cgroup_cpu_format', receipt)
        self.assertNotIn('cgroup_cpu', sample)
        self.assertFalse(any(path.startswith(ns.CGROUP_ROOT) for path in host.reads))
        self.assertFalse(any('ControlGroup' in arg for command in host.calls for arg in command))

    def test_run_start_stop_have_same_source_and_membership(self):
        host = FakeHost(units=node_units([4500]), stop_at=1)
        out = self.tmp / 'cgroup'
        stop = ns.run_sampler(host, out, 'node1', 'M1w1', {0, 1}, 0.01, 2, ns.UNIT_GLOB, [self.unit], None, [],
                              host.stop, cgroup_cpu=True)
        start = json.loads((out / 'receipt-start.json').read_text())
        self.assertEqual(start['cgroup_units'], stop['cgroup_units'])
        self.assertEqual(start['cgroup_v2'], stop['cgroup_v2'])
        samples = [json.loads(line) for line in (out / 'samples.jsonl').read_text().splitlines()]
        self.assertTrue(samples)
        self.assertTrue(all(sample['cgroup_cpu_format'] == ns.CGROUP_CPU_FORMAT for sample in samples))
        self.assertTrue(all(not sample.get('cgroup_cpu_errors') for sample in samples))

    def test_changed_stop_identity_does_not_publish_a_valid_stop_receipt(self):
        host = FakeHost(units=node_units([4500]), stop_at=1)
        host.unit_changes = [(1, self.unit, dict(starttime=999))]
        out = self.tmp / 'changed-cgroup'
        with self.assertRaisesRegex(mc.GuardError, 'cgroup identity or membership changed'):
            ns.run_sampler(host, out, 'node1', 'M1w1', {0, 1}, 0.01, 2, ns.UNIT_GLOB, [self.unit], None, [],
                           host.stop, cgroup_cpu=True)
        self.assertFalse((out / 'receipt-stop.json').exists())

    def test_cgroup_cli_rejects_implicit_duplicate_or_client_population(self):
        with contextlib.redirect_stderr(io.StringIO()):
            for units in ([], ['--unit', self.unit, '--unit', self.unit], ['--unit', self.unit, '--no-units']):
                with self.subTest(units=units), self.assertRaises(SystemExit):
                    ns.parse_args(['receipt', '--host', 'node1', '--phase', 'M1w1', '--kind', 'before',
                                   '--output', '/not-created', '--cgroup-cpu', *units])

    def test_cgroup_requires_explicit_unique_units(self):
        host = FakeHost(units=node_units([4500]))
        for explicit in ([], [self.unit, self.unit]):
            with self.subTest(explicit=explicit), self.assertRaises(mc.GuardError):
                ns.build_receipt(host, 'start', 'node1', 'M1w1', {}, True, ns.UNIT_GLOB, explicit, None, [],
                                 cgroup_cpu=True)


class RunTest(Temp):
    def run_node(self, host, **kw):
        out = self.tmp / 'tele'
        args = dict(name=host.name, phase='M2rl', cpus={0, 1}, interval_s=1.0, max_seconds=3600,
                    glob=ns.UNIT_GLOB, explicit_units=[], cluster_file=None, argv=['x'], stop=host.stop)
        args.update(kw)
        receipt = ns.run_sampler(host, out, **args)
        return out, receipt

    def test_run_writes_samples_and_receipts(self):
        h = FakeHost(units=node_units([4500, 4501]), stop_at=5)
        out, stop = self.run_node(h)
        lines = (out / 'samples.jsonl').read_text().splitlines()
        samples = [json.loads(l) for l in lines]
        self.assertEqual([s['seq'] for s in samples], list(range(len(samples))))
        self.assertEqual(len(samples), 6)  # t = 0..4, then t = 5 taken on the stop signal
        self.assertEqual(stop['stop_reason'], 'SIGTERM')
        self.assertEqual(stop['samples'], 6)
        self.assertEqual(stop['samples_sha256'], ns.sha256_file(out / 'samples.jsonl'))
        start = json.loads((out / 'receipt-start.json').read_text())
        self.assertEqual(start['kind'], 'start')
        for receipt in (start, stop):
            self.assertEqual(receipt['process_cpu_format'], ns.PROCESS_CPU_FORMAT)
            self.assertEqual(receipt['clock_ticks_per_second'], 100)
        self.assertEqual(start['units']['names'], ['fdb-bench-4500.service', 'fdb-bench-4501.service'])
        self.assertEqual(start['units']['starttimes']['fdb-bench-4501.service'], dict(pid=1001, starttime='501'))
        self.assertEqual(start['chrony']['fields']['leap_status'], 'Normal')
        self.assertEqual(start['boot_id'], BOOT_ID)
        self.assertEqual(samples[2]['procs'], {'fdb-bench-4500.service': '500', 'fdb-bench-4501.service': '501'})
        self.assertEqual(samples[2]['process_cpu']['fdb-bench-4500.service'],
                         dict(pid=1000, starttime='500', user_ticks=114, system_ticks=56))
        self.assertEqual(h.reads.count('/proc/1000/stat'), len(samples) + 2)
        gaps = [b['monotonic_ns'] - a['monotonic_ns'] for a, b in zip(samples, samples[1:-1])]
        self.assertTrue(all(abs(g - 10**9) < 10**6 for g in gaps), gaps)

    def test_clock_tick_receipt_refuses_invalid_values(self):
        for ticks in (0, -1, 1.5, True, None):
            with self.subTest(ticks=ticks), self.assertRaisesRegex(mc.GuardError, 'SC_CLK_TCK'):
                h = FakeHost()
                h.ticks_per_second = ticks
                ns.build_receipt(h, 'start', 'node1', 'M1w1', {}, False, ns.UNIT_GLOB, [], None, [])

    def test_clock_tick_change_refused(self):
        h = FakeHost(units=node_units([4500]), stop_at=1)
        ticks = iter((100, 250))
        h.clock_ticks_per_second = lambda: next(ticks)
        with self.assertRaisesRegex(mc.GuardError, 'SC_CLK_TCK changed'):
            self.run_node(h)
        self.assertTrue((self.tmp / 'tele' / 'receipt-start.json').exists())
        self.assertFalse((self.tmp / 'tele' / 'receipt-stop.json').exists())

    def test_max_seconds_stops(self):
        h = FakeHost(units=node_units([4500]), stop_at=10**6)
        _, stop = self.run_node(h, max_seconds=3)
        self.assertEqual(stop['stop_reason'], 'max-seconds')
        self.assertEqual(stop['samples'], 4)

    def test_overrun_skips_slots_visibly(self):
        h = FakeHost(units=node_units([4500]), stop_at=6)
        h.extra_sleep = {2: 2.5}
        out, _ = self.run_node(h)
        samples = [json.loads(l) for l in (out / 'samples.jsonl').read_text().splitlines()]
        gaps = [(b['monotonic_ns'] - a['monotonic_ns']) / 1e9 for a, b in zip(samples, samples[1:])]
        self.assertTrue(any(g > 2 for g in gaps), gaps)
        self.assertGreater(min(gaps), 0.4, gaps)  # no burst of catch-up samples after the overrun
        self.assertEqual(len({s['slot'] for s in samples}), len(samples))

    def test_output_dir_refusals(self):
        h = FakeHost(units=node_units([4500]), stop_at=1)
        (self.tmp / 'repo' / '.git').mkdir(parents=True)
        with self.assertRaisesRegex(mc.GuardError, 'Git work tree'):
            ns.run_sampler(h, self.tmp / 'repo' / 'tele', 'node1', 'M2rl', {0, 1}, 1.0, 10, ns.UNIT_GLOB, [], None, [],
                           h.stop)
        self.assertFalse((self.tmp / 'repo' / 'tele').exists())
        (self.tmp / 'tele').mkdir()
        with self.assertRaises(FileExistsError):
            self.run_node(h)

    def test_client_receipt_without_units(self):
        h = FakeHost(name='client', units={})
        r = ns.build_receipt(h, 'before', 'client', 'M1w1', {}, False, ns.UNIT_GLOB, [], None, [])
        self.assertIsNone(r['units'])
        self.assertFalse(any(c[0] == 'systemctl' for c in h.calls))
        self.assertEqual(r['chrony']['errors'], [])

    def test_receipt_records_failures(self):
        h = FakeHost(units=node_units([4500]))
        h.chrony_rc, h.systemctl_rc = 1, 1
        h.chrony = ['506 Cannot talk to daemon\n']
        r = ns.build_receipt(h, 'start', 'node1', 'M1w1', {}, True, ns.UNIT_GLOB, [], '/x/fdb.cluster', [])
        self.assertEqual(r['chrony']['rc'], 1)
        self.assertTrue(r['chrony']['errors'])
        self.assertEqual(r['units']['show_rc'], 1)
        self.assertEqual(r['status']['rc'], 0)
        self.assertIn(['fdbcli', '-C', '/x/fdb.cluster', '--timeout', '10', '--exec', 'status json'], h.calls)

    def test_explicit_units_skip_discovery(self):
        h = FakeHost(units=node_units([4500]))
        r = ns.unit_receipt(h, ns.UNIT_GLOB, ['fdb-bench-4500.service'])
        self.assertFalse(any(c[:2] == ['systemctl', 'list-units'] for c in h.calls))
        self.assertEqual(r['names'], ['fdb-bench-4500.service'])

    def test_main_receipt_cli(self):
        quiet = contextlib.ExitStack()
        quiet.enter_context(contextlib.redirect_stdout(io.StringIO()))
        quiet.enter_context(contextlib.redirect_stderr(io.StringIO()))
        self.addCleanup(quiet.close)
        h = FakeHost(name='client')
        out = self.tmp / 'before.json'
        rc = ns.main(['receipt', '--host', 'client', '--phase', 'M1w1', '--kind', 'before', '--no-units',
                      '--output', str(out)], host=h)
        self.assertEqual(rc, 0)
        self.assertEqual(json.loads(out.read_text())['kind'], 'before')
        (self.tmp / 'repo' / '.git').mkdir(parents=True)
        self.assertEqual(ns.main(['receipt', '--host', 'client', '--phase', 'M1w1', '--kind', 'after', '--no-units',
                                  '--output', str(self.tmp / 'repo' / 'r.json')], host=FakeHost()), 2)
        self.assertFalse((self.tmp / 'repo' / 'r.json').exists())
        h = FakeHost(name='client')
        h.chrony = [chrony_text(drop=('Root delay',))]
        self.assertEqual(ns.main(['receipt', '--host', 'client', '--phase', 'M1w1', '--kind', 'after', '--no-units',
                                  '--output', str(self.tmp / 'after.json')], host=h), 1)


if __name__ == '__main__':
    unittest.main()
