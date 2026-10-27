#!/usr/bin/env python3
"""Per-host server telemetry for the multi-node campaign (R3). Standard library only.

run:     pin to the housekeeping CPUs, write receipt-start.json, append one sample
         per second to samples.jsonl, and on SIGTERM/SIGINT or --max-seconds write
         receipt-stop.json. --output-dir must be new and outside any Git work tree.
receipt: write one receipt (--kind before|after) to --output; used on the client
         host for its chrony receipts, and for extra checkpoints on a node.

A sample is one JSON line:
  seq, slot                  write order; 1 s schedule slot (CLOCK_MONOTONIC based)
  realtime_ns, monotonic_ns  CLOCK_REALTIME / CLOCK_MONOTONIC read just before /proc
  realtime_after_ns          CLOCK_REALTIME read just after /proc (bounds the read)
  stat                       /proc/stat lines cpu*, ctxt, btime, processes, procs_*
                             (verbatim; intr/softirq omitted for size)
  vmstat                     /proc/vmstat verbatim
  procs                      {unit: /proc/<MainPID>/stat starttime or null}, for the
                             MainPIDs recorded in receipt-start.json
  process_cpu                {unit: {pid, starttime, user_ticks, system_ticks} or null};
                             additive process_cpu_format v1, from the same stat read
  process_cpu_errors         per-unit read/parse errors, when CPU data is unavailable
  cgroup_cpu                 opt-in cgroup v2 usage_usec and proved unit membership;
                             unavailable entries have cgroup_cpu_errors, never fallback
  error                      present when a read failed (the sample still counts as
                             written; reconcile_telemetry.py treats it as unparsable)

Raw text is stored and parsed later by reconcile_telemetry.py with the parsers below,
so the sampler cannot hide a parse problem. A receipt holds the host identity
(boot_id, hostname), both clocks, this script's sha256, its CPU affinity, `systemctl
show` for every fdb-bench-*.service unit (MainPID, NRestarts, ActiveState, SubState,
ActiveEnterTimestamp[Monotonic]) with each MainPID's starttime, `chronyc -n tracking`
verbatim, and optionally `fdbcli status json`. Receipts also record a positive
clock_ticks_per_second (SC_CLK_TCK) for process CPU counters. Existing receipt and
sample formats and the R4 procs identity checks are unchanged; CPU consumers must
reject missing counters or a changed (pid, starttime) instead of joining restarts.
The separate --cgroup-cpu sidecar requires explicit units and records canonical
cgroup v2 paths, filesystem identities and exact process/thread membership. It
only reads cgroups; it never moves tasks or changes controller configuration.

The caller starts it detached on each node, e.g.
  taskset -c 0,1 python3 -B node_sampler.py run --host node1 --phase M2rl \
    --output-dir /srv/fdb-bench/telemetry/M2rl --max-seconds 9000
and stops it with SIGTERM after the phase. Copy mn_common.py alongside.
"""
import argparse
import datetime as dt
from decimal import Decimal, InvalidOperation
import hashlib
import json
import os
from pathlib import Path
import re
import signal
import socket
import sys
import time

import mn_common as mc

RECEIPT_FORMAT = 'fdb-bench-node-receipt/v1'
SAMPLE_FORMAT = 'fdb-bench-node-sample/v1'
PROCESS_CPU_FORMAT = 'fdb-bench-process-cpu/v1'
CGROUP_CPU_FORMAT = 'fdb-bench-cgroup-cpu/v1'
CGROUP_ROOT = '/sys/fs/cgroup'
UNIT_GLOB = 'fdb-bench-*.service'
UNIT_PROPERTIES = ('Id', 'LoadState', 'ActiveState', 'SubState', 'MainPID', 'NRestarts',
                   'ActiveEnterTimestamp', 'ActiveEnterTimestampMonotonic')
STAT_KEEP = re.compile(r'^(cpu[0-9]*|ctxt|btime|processes|procs_running|procs_blocked) ')
LEAP_STATES = ('Normal', 'Insert second', 'Delete second', 'Not synchronised')


# --- parsers shared with reconcile_telemetry.py ------------------------------

def parse_proc_stat(text):
    """/proc/stat text -> {'cpu': [...], 'cpu0': [...], ...} of jiffy counters.

    Raises ValueError unless every cpu line has at least the eight fields
    user nice system idle iowait irq softirq steal as non-negative integers."""
    cpus = {}
    for line in text.splitlines():
        fields = line.split()
        if not fields or not fields[0].startswith('cpu'):
            continue
        if not re.fullmatch(r'cpu[0-9]*', fields[0]) or fields[0] in cpus:
            raise ValueError('bad or duplicate cpu line: ' + line[:80])
        if len(fields) < 9 or not all(re.fullmatch(r'[0-9]+', f) for f in fields[1:]):
            raise ValueError('bad cpu counters: ' + line[:80])
        cpus[fields[0]] = [int(f) for f in fields[1:]]
    if 'cpu' not in cpus:
        raise ValueError('no aggregate cpu line')
    return cpus


def parse_vmstat(text):
    """/proc/vmstat text -> {name: int}. Raises ValueError on a malformed line."""
    values = {}
    for line in text.splitlines():
        if not line.strip():
            continue
        m = re.fullmatch(r'([a-z0-9_]+) (-?[0-9]+)', line.strip())
        if not m or m.group(1) in values:
            raise ValueError('bad vmstat line: ' + line[:80])
        values[m.group(1)] = int(m.group(2))
    return values


def parse_chrony_tracking(text):
    """`chronyc tracking` text -> (fields, errors).

    fields: system_time_offset_s (signed Decimal string; fast of NTP is positive),
    root_delay_s, root_dispersion_s, leap_status. Any missing or malformed field is
    reported in errors and left out of fields."""
    fields, errors = {}, []
    found = {}
    for line in (text or '').splitlines():
        key, sep, value = line.partition(':')
        if sep:
            found.setdefault(key.strip(), []).append(value.strip())
    for key in ('System time', 'Root delay', 'Root dispersion', 'Leap status'):
        if len(found.get(key, [])) != 1:
            errors.append(f'chrony field {key!r} missing or repeated')
    if len(found.get('System time', [])) == 1:
        m = re.fullmatch(r'([0-9]+\.[0-9]+) seconds (slow|fast) of NTP time', found['System time'][0])
        if m:
            fields['system_time_offset_s'] = str(Decimal(m.group(1)) * (1 if m.group(2) == 'fast' else -1))
        else:
            errors.append('unparsable System time: ' + found['System time'][0][:80])
    for key, name in (('Root delay', 'root_delay_s'), ('Root dispersion', 'root_dispersion_s')):
        if len(found.get(key, [])) == 1:
            m = re.fullmatch(r'([0-9]+\.[0-9]+) seconds', found[key][0])
            if m:
                fields[name] = str(Decimal(m.group(1)))
            else:
                errors.append(f'unparsable {key}: ' + found[key][0][:80])
    if len(found.get('Leap status', [])) == 1:
        leap = found['Leap status'][0]
        if leap in LEAP_STATES:
            fields['leap_status'] = leap
        else:
            errors.append('unknown Leap status: ' + leap[:80])
    return fields, errors


def chrony_error_bound(fields):
    """chrony's root distance |offset| + root_dispersion + 0.5 * root_delay, as Decimal.

    Raises ValueError for a missing field or an unsynchronised clock. A pending leap
    second is refused too: a one-second step is not covered by the bound."""
    try:
        off = Decimal(fields['system_time_offset_s'])
        disp = Decimal(fields['root_dispersion_s'])
        delay = Decimal(fields['root_delay_s'])
        leap = fields['leap_status']
    except (KeyError, InvalidOperation, TypeError) as exc:
        raise ValueError(f'chrony field missing or invalid: {exc!r}') from None
    if leap != 'Normal':
        raise ValueError('clock not usable: Leap status ' + leap)
    if disp < 0 or delay < 0:
        raise ValueError('negative chrony root dispersion or delay')
    return abs(off) + disp + delay / 2


def parse_systemctl_show(text):
    """`systemctl show -p ... UNIT...` -> {Id: {property: value}}; blocks split by blank lines."""
    units, block = {}, {}
    for line in (text or '').splitlines() + ['']:
        if not line.strip():
            if block:
                if 'Id' not in block or block['Id'] in units:
                    raise ValueError('systemctl block without a unique Id')
                units[block['Id']] = block
            block = {}
            continue
        key, sep, value = line.partition('=')
        if not sep:
            raise ValueError('bad systemctl show line: ' + line[:80])
        block[key] = value
    return units


def proc_starttime(stat_text):
    """/proc/PID/stat -> field 22 (starttime, clock ticks since boot) as a string."""
    rest = stat_text.rsplit(')', 1)[1].split()
    return rest[19]


def proc_cpu(stat_text, pid):
    """Read process identity and CPU ticks (fields 14, 15, 22), not child CPU."""
    rest = stat_text.rsplit(')', 1)[1].split()
    user, system, start = rest[11], rest[12], rest[19]
    if not all(re.fullmatch(r'[0-9]+', value) for value in (user, system, start)):
        raise ValueError('invalid process CPU counters or starttime')
    return dict(pid=pid, starttime=start, user_ticks=int(user), system_ticks=int(system))


# --- host access (injected in tests) -----------------------------------------

class Host:
    """Everything the sampler reads from the machine. Tests substitute a fake."""

    def read(self, path):
        return Path(path).read_text()

    def clock_ns(self, which):
        return time.clock_gettime_ns(time.CLOCK_REALTIME if which == 'realtime' else time.CLOCK_MONOTONIC)

    def sleep(self, seconds):
        time.sleep(seconds)

    def run(self, argv, timeout=30):
        return mc.default_runner(argv, timeout=timeout)

    def get_affinity(self):
        return set(os.sched_getaffinity(0))

    def set_affinity(self, cpus):
        os.sched_setaffinity(0, cpus)

    def hostname(self):
        return socket.gethostname()

    def clock_ticks_per_second(self):
        return os.sysconf('SC_CLK_TCK')

    def realpath(self, path):
        return str(Path(path).resolve(strict=True))

    def listdir(self, path):
        return os.listdir(path)

    def subdirectories(self, path):
        with os.scandir(path) as entries:
            return sorted(entry.name for entry in entries if entry.is_dir(follow_symlinks=False))

    def directory_identity(self, path):
        value = os.stat(path)
        return dict(device=value.st_dev, inode=value.st_ino)


def parse_cpus(text):
    cpus = set()
    for part in text.split(','):
        lo, sep, hi = part.partition('-')
        if not re.fullmatch(r'[0-9]+', lo) or (sep and not re.fullmatch(r'[0-9]+', hi)):
            raise mc.GuardError('bad CPU list: ' + text)
        cpus |= set(range(int(lo), int(hi) + 1)) if sep else {int(lo)}
    return cpus


def pin_housekeeping(host, cpus):
    """Run only on the housekeeping CPUs: keep a subset affinity, else set it; verify."""
    before = host.get_affinity()
    if not before <= cpus:
        host.set_affinity(cpus)
    after = host.get_affinity()
    if not after or not after <= cpus:
        raise mc.GuardError(f'sampler affinity {sorted(after)} is not within housekeeping CPUs {sorted(cpus)}')
    return dict(before=sorted(before), after=sorted(after), housekeeping=sorted(cpus))


def sha256_file(path):
    h = hashlib.sha256()
    with open(path, 'rb') as stream:
        for chunk in iter(lambda: stream.read(1 << 20), b''):
            h.update(chunk)
    return h.hexdigest()


def utc_ns(ns):
    return dt.datetime.fromtimestamp(ns // 1_000_000_000, dt.timezone.utc).strftime('%Y-%m-%dT%H:%M:%S') + \
        f'.{ns % 1_000_000_000:09d}Z'


def unit_receipt(host, glob, explicit, cgroup_cpu=False):
    """systemctl state of every campaign unit, plus each MainPID's starttime."""
    out = dict(glob=glob, explicit=list(explicit))
    if explicit:
        names = list(explicit)
    else:
        rc, stdout, stderr = host.run(['systemctl', 'list-units', '--all', '--plain', '--no-legend', '--no-pager', glob])
        out.update(list_rc=rc, list_raw=stdout, list_stderr=stderr)
        names = sorted({line.split()[0] for line in stdout.splitlines() if line.split()})
    out['names'] = names
    out['starttimes'] = {}
    if not names:
        out.update(show_rc=None, show_raw='')
        return out
    properties = UNIT_PROPERTIES + (('ControlGroup',) if cgroup_cpu else ())
    rc, stdout, stderr = host.run(['systemctl', 'show', '--no-pager', '-p', ','.join(properties), *names])
    out.update(show_rc=rc, show_raw=stdout, show_stderr=stderr)
    try:
        for name, props in parse_systemctl_show(stdout).items():
            pid = props.get('MainPID', '0')
            if re.fullmatch(r'[1-9][0-9]*', pid):
                try:
                    out['starttimes'][name] = dict(pid=int(pid), starttime=proc_starttime(host.read(f'/proc/{pid}/stat')))
                except (OSError, IndexError) as exc:
                    out['starttimes'][name] = dict(pid=int(pid), starttime=None, error=repr(exc))
    except ValueError as exc:
        out['parse_error'] = str(exc)
    return out


def parse_cpu_stat(text):
    """cgroup v2 cpu.stat microseconds; optional user/system fields travel together."""
    values = {}
    for line in text.splitlines():
        match = re.fullmatch(r'([a-z][a-z0-9_.]*) ([0-9]+)', line)
        if not match or match[1] in values:
            raise mc.GuardError('malformed or duplicate cgroup cpu.stat field')
        values[match[1]] = int(match[2])
    if 'usage_usec' not in values or ('user_usec' in values) != ('system_usec' in values):
        raise mc.GuardError('missing cgroup CPU usage or incomplete user/system counters')
    return {field: values[field] for field in ('usage_usec', 'user_usec', 'system_usec') if field in values}


def cgroup_members(text):
    values = text.split()
    if not values or any(re.fullmatch(r'[1-9][0-9]*', value) is None for value in values):
        raise mc.GuardError('empty or malformed cgroup membership')
    members = sorted(int(value) for value in values)
    if len(members) != len(set(members)):
        raise mc.GuardError('duplicate cgroup member')
    return members


def cgroup_identity(host, path, pid, starttime):
    if host.realpath(path) != path or host.subdirectories(path):
        raise mc.GuardError('noncanonical cgroup path or nested subgroup')
    identity = host.directory_identity(path)
    if (set(identity) != {'device', 'inode'} or any(type(v) is not int or v < 0 for v in identity.values())
            or identity['inode'] == 0):
        raise mc.GuardError('invalid cgroup filesystem identity')
    procs = cgroup_members(host.read(path + '/cgroup.procs'))
    threads = cgroup_members(host.read(path + '/cgroup.threads'))
    tasks = cgroup_members(' '.join(host.listdir(f'/proc/{pid}/task')))
    if procs != [pid] or threads != tasks or pid not in threads:
        raise mc.GuardError('foreign or missing cgroup process/thread membership')
    return dict(path=path, pid=pid, starttime=starttime, procs=procs, threads=threads, **identity)


def read_cgroup_unit(host, baseline):
    current = cgroup_identity(host, baseline['path'], baseline['pid'], baseline['starttime'])
    if current != baseline:
        raise mc.GuardError('cgroup identity or membership differs from baseline')
    return dict(**current, **parse_cpu_stat(host.read(current['path'] + '/cpu.stat')))


def cgroup_receipt(host, units, explicit):
    if not units or not explicit or len(explicit) != len(set(explicit)) or units.get('show_rc') != 0:
        raise mc.GuardError('cgroup CPU requires successful explicit-unit discovery')
    if host.realpath(CGROUP_ROOT) != CGROUP_ROOT:
        raise mc.GuardError('noncanonical cgroup v2 root')
    controllers = host.read(CGROUP_ROOT + '/cgroup.controllers').split()
    if 'cpu' not in controllers or len(controllers) != len(set(controllers)):
        raise mc.GuardError('cgroup v2 CPU controller unavailable')
    evidence = dict(path=CGROUP_ROOT, controllers=sorted(controllers), **host.directory_identity(CGROUP_ROOT))
    properties = parse_systemctl_show(units['show_raw'])
    if set(properties) != set(explicit) or set(units['names']) != set(explicit) or set(units['starttimes']) != set(explicit):
        raise mc.GuardError('incomplete cgroup unit population')
    groups = {}
    for unit in sorted(explicit):
        match = re.fullmatch(r'fdb-bench-([1-9][0-9]{0,4})\.service', unit)
        if not match or int(match[1]) > 65535:
            raise mc.GuardError('noncanonical FDB unit name')
        props, identity = properties[unit], units['starttimes'][unit]
        relative = '/system.slice/' + unit
        if props.get('ControlGroup') != relative or props.get('ActiveState') != 'active' or props.get('SubState') != 'running':
            raise mc.GuardError('unexpected ControlGroup or inactive FDB unit')
        if identity.get('error') or re.fullmatch(r'[0-9]+', identity.get('starttime') or '') is None:
            raise mc.GuardError('missing cgroup MainPID starttime')
        groups[unit] = cgroup_identity(host, CGROUP_ROOT + relative, identity['pid'], identity['starttime'])
        read_cgroup_unit(host, groups[unit])  # cpu.stat support is required at both receipt boundaries.
    return evidence, groups


def build_receipt(host, kind, name, phase, affinity, units, glob, explicit_units, cluster_file, argv, cgroup_cpu=False):
    receipt = dict(format=RECEIPT_FORMAT, kind=kind, host=name, phase=phase,
                   realtime_ns=host.clock_ns('realtime'), monotonic_ns=host.clock_ns('monotonic'))
    ticks = host.clock_ticks_per_second()
    if type(ticks) is not int or ticks <= 0:
        raise mc.GuardError('SC_CLK_TCK must be a positive integer')
    receipt.update(process_cpu_format=PROCESS_CPU_FORMAT, clock_ticks_per_second=ticks)
    receipt['utc'] = utc_ns(receipt['realtime_ns'])
    try:
        receipt['boot_id'] = host.read('/proc/sys/kernel/random/boot_id').strip()
    except OSError as exc:
        receipt['boot_id'] = None
        receipt['boot_id_error'] = repr(exc)
    receipt.update(hostname=host.hostname(), affinity=affinity, argv=list(argv),
                   sampler_sha256=sha256_file(__file__))
    receipt['units'] = unit_receipt(host, glob, explicit_units, cgroup_cpu) if units else None
    if cgroup_cpu:
        evidence, groups = cgroup_receipt(host, receipt['units'], explicit_units)
        receipt.update(cgroup_cpu_format=CGROUP_CPU_FORMAT, cgroup_v2=evidence, cgroup_units=groups)
    rc, stdout, stderr = host.run(['chronyc', '-n', 'tracking'])
    fields, errors = parse_chrony_tracking(stdout)
    receipt['chrony'] = dict(rc=rc, raw=stdout, stderr=stderr, fields=fields, errors=errors)
    if cluster_file:
        rc, stdout, stderr = host.run(['fdbcli', '-C', cluster_file, '--timeout', '10', '--exec', 'status json'])
        receipt['status'] = dict(rc=rc, raw=stdout, stderr=stderr)
    receipt['realtime_after_ns'] = host.clock_ns('realtime')
    return receipt


def take_sample(host, seq, slot, pids, cgroup_units=None, cgroup_previous=None):
    sample = dict(format=SAMPLE_FORMAT, seq=seq, slot=slot, realtime_ns=host.clock_ns('realtime'),
                  monotonic_ns=host.clock_ns('monotonic'))
    try:
        sample['stat'] = '\n'.join(l for l in host.read('/proc/stat').splitlines() if STAT_KEEP.match(l))
        sample['vmstat'] = host.read('/proc/vmstat')
    except OSError as exc:
        sample['error'] = repr(exc)
    procs, process_cpu, cpu_errors = {}, {}, {}
    for unit, pid in sorted(pids.items()):
        process_cpu[unit] = None
        try:
            text = host.read(f'/proc/{pid}/stat')
            procs[unit] = proc_starttime(text)
            try:
                process_cpu[unit] = proc_cpu(text, pid)
            except (ValueError, IndexError) as exc:
                cpu_errors[unit] = repr(exc)
        except (OSError, IndexError) as exc:
            procs[unit] = None
            cpu_errors[unit] = repr(exc)
    sample.update(procs=procs, process_cpu=process_cpu, process_cpu_format=PROCESS_CPU_FORMAT)
    if cpu_errors:
        sample['process_cpu_errors'] = cpu_errors
    if cgroup_units is not None:
        counters, errors = {}, {}
        for unit, identity in sorted(cgroup_units.items()):
            counters[unit] = None
            try:
                if procs.get(unit) != identity['starttime'] or pids.get(unit) != identity['pid']:
                    raise mc.GuardError('cgroup MainPID/starttime changed')
                current = read_cgroup_unit(host, identity)
                if cgroup_previous is not None and unit in cgroup_previous:
                    previous = cgroup_previous[unit]
                    for field in ('usage_usec', 'user_usec', 'system_usec'):
                        if (field in previous) != (field in current) or (field in current and current[field] < previous[field]):
                            raise mc.GuardError('cgroup CPU counter reset or schema changed')
                counters[unit] = current
                if cgroup_previous is not None:
                    cgroup_previous[unit] = current
            except (OSError, ValueError, mc.GuardError) as exc:
                errors[unit] = repr(exc)
        sample.update(cgroup_cpu_format=CGROUP_CPU_FORMAT, cgroup_cpu=counters)
        if errors:
            sample['cgroup_cpu_errors'] = errors
    sample['realtime_after_ns'] = host.clock_ns('realtime')
    return sample


class Stop:
    def __init__(self):
        self.reason = None

    def __call__(self, signum, frame):
        self.reason = signal.Signals(signum).name


def run_sampler(host, out, name, phase, cpus, interval_s, max_seconds, glob, explicit_units, cluster_file,
                argv, stop, units=True, cgroup_cpu=False):
    """Start receipt, 1 Hz samples until stop.reason or max_seconds, stop receipt."""
    mc.require_outside_repo(out, 'telemetry output dir')
    out = Path(out)
    out.mkdir(parents=True, exist_ok=False)
    affinity = pin_housekeeping(host, cpus)
    start = build_receipt(host, 'start', name, phase, affinity, units, glob, explicit_units, cluster_file, argv,
                          cgroup_cpu=cgroup_cpu)
    mc.dump_json(out / 'receipt-start.json', start)
    pids = {u: s['pid'] for u, s in ((start['units'] or {}).get('starttimes') or {}).items()}
    interval_ns = int(interval_s * 1e9)
    t0 = host.clock_ns('monotonic')
    seq = slot = 0
    cgroup_previous = {}
    path = out / 'samples.jsonl'
    with open(path, 'x') as stream:
        while True:
            sample = take_sample(host, seq, slot, pids, start.get('cgroup_units'), cgroup_previous)
            stream.write(json.dumps(sample, sort_keys=True, allow_nan=False) + '\n')
            stream.flush()
            seq += 1
            if stop.reason is None and (sample['monotonic_ns'] - t0) >= max_seconds * 1_000_000_000:
                stop.reason = 'max-seconds'
            if stop.reason is not None:
                break
            now = host.clock_ns('monotonic')
            # Next 1 s slot strictly after now; an overrun skips slots (the gap is visible).
            slot = max(slot + 1, (now - t0) // interval_ns + 1)
            host.sleep(max(0, t0 + slot * interval_ns - now) / 1e9)
        os.fsync(stream.fileno())
    stop_receipt = build_receipt(host, 'stop', name, phase, pin_housekeeping(host, cpus), units, glob,
                                 explicit_units, cluster_file, argv, cgroup_cpu=cgroup_cpu)
    if cgroup_cpu and (stop_receipt['cgroup_units'] != start['cgroup_units'] or
                       stop_receipt['cgroup_v2'] != start['cgroup_v2']):
        raise mc.GuardError('cgroup identity or membership changed during sampling')
    if stop_receipt['clock_ticks_per_second'] != start['clock_ticks_per_second']:
        raise mc.GuardError('SC_CLK_TCK changed during sampling')
    stop_receipt.update(stop_reason=stop.reason, samples=seq, samples_sha256=sha256_file(path))
    mc.dump_json(out / 'receipt-stop.json', stop_receipt)
    return stop_receipt


def parse_args(argv=None):
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = ap.add_subparsers(dest='command', required=True)
    for name in ('run', 'receipt'):
        p = sub.add_parser(name)
        p.add_argument('--host', required=True, help='client|node1|node2|node3 (the campaign host name)')
        p.add_argument('--phase', required=True)
        p.add_argument('--cpus', default='0,1', help='housekeeping CPUs the sampler must run on')
        p.add_argument('--unit-glob', default=UNIT_GLOB)
        p.add_argument('--unit', action='append', default=[], help='explicit unit (repeat); default: discover by glob')
        p.add_argument('--no-units', action='store_true', help='host without fdb-bench units (the client)')
        p.add_argument('--cgroup-cpu', action='store_true', help='opt-in cgroup v2 CPU sidecar; requires explicit --unit population')
        p.add_argument('--cluster-file', help='also snapshot fdbcli status json with this cluster file')
    r = sub.choices['run']
    r.add_argument('--output-dir', required=True, help='new directory outside any Git work tree')
    r.add_argument('--interval', type=float, default=1.0)
    r.add_argument('--max-seconds', type=int, required=True, help='hard stop (phase wall cap + margin)')
    c = sub.choices['receipt']
    c.add_argument('--kind', choices=('before', 'after'), required=True)
    c.add_argument('--output', required=True, help='receipt file (exclusive, outside any Git work tree)')
    args = ap.parse_args(argv)
    if not re.fullmatch(r'[A-Za-z0-9_.-]{1,64}', args.host) or not re.fullmatch(r'[A-Za-z0-9_.-]{1,64}', args.phase):
        ap.error('unsafe --host or --phase')
    if args.cgroup_cpu and (args.no_units or not args.unit or len(args.unit) != len(set(args.unit))):
        ap.error('--cgroup-cpu requires unique explicit --unit values and cannot use --no-units')
    if args.command == 'run' and not (0 < args.interval <= 1 and args.max_seconds > 0):
        ap.error('--interval must be in (0, 1] and --max-seconds positive')
    return args


def main(argv=None, host=None):
    args = parse_args(argv)
    host = host or Host()
    try:
        cpus = parse_cpus(args.cpus)
        if args.command == 'run':
            stop = Stop()
            for sig in (signal.SIGTERM, signal.SIGINT):
                signal.signal(sig, stop)
            receipt = run_sampler(host, args.output_dir, args.host, args.phase, cpus, args.interval, args.max_seconds,
                                  args.unit_glob, args.unit, args.cluster_file, sys.argv if argv is None else argv,
                                  stop, units=not args.no_units, cgroup_cpu=args.cgroup_cpu)
            print(f'{receipt["samples"]} samples; stop: {receipt["stop_reason"]}')
            return 0
        mc.require_outside_repo(args.output, 'receipt')
        receipt = build_receipt(host, args.kind, args.host, args.phase, pin_housekeeping(host, cpus), not args.no_units,
                                args.unit_glob, args.unit, args.cluster_file, sys.argv if argv is None else argv,
                                cgroup_cpu=args.cgroup_cpu)
        mc.dump_json(args.output, receipt)
        print(json.dumps(dict(chrony=receipt['chrony']['fields'], errors=receipt['chrony']['errors']), sort_keys=True))
        return 0 if not receipt['chrony']['errors'] and receipt['chrony']['rc'] == 0 else 1
    except (mc.GuardError, OSError, ValueError) as exc:
        print('REFUSED: ' + str(exc), file=sys.stderr)
        return 2


if __name__ == '__main__':
    sys.exit(main())
