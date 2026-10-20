#!/usr/bin/env python3
"""Unit tests for node_sampler.py. Synthetic /proc text and fake command runners
only: no systemctl, chronyc, fdbcli or real clock is used."""
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


def proc_pid_stat(pid, starttime):
    return f'{pid} (fdbserver) S ' + ' '.join(['1'] * 18 + [str(starttime)] + ['0'] * 30) + '\n'


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
        self.tick()
        k = round(self.t())
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
                    return proc_pid_stat(pid, u['starttime'])
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

    def test_read_error_recorded(self):
        h = FakeHost()
        h.read = lambda path: (_ for _ in ()).throw(PermissionError(path))
        s = ns.take_sample(h, 0, 0, {})
        self.assertIn('PermissionError', s['error'])


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
        self.assertEqual(start['units']['names'], ['fdb-bench-4500.service', 'fdb-bench-4501.service'])
        self.assertEqual(start['units']['starttimes']['fdb-bench-4501.service'], dict(pid=1001, starttime='501'))
        self.assertEqual(start['chrony']['fields']['leap_status'], 'Normal')
        self.assertEqual(start['boot_id'], BOOT_ID)
        self.assertEqual(samples[2]['procs'], {'fdb-bench-4500.service': '500', 'fdb-bench-4501.service': '501'})
        gaps = [b['monotonic_ns'] - a['monotonic_ns'] for a, b in zip(samples, samples[1:-1])]
        self.assertTrue(all(abs(g - 10**9) < 10**6 for g in gaps), gaps)

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
