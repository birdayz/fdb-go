"""Observed exclusive FDB cgroup accounting deltas, not execution-window CPU.

M1 client fences and M2 lifecycle timestamps select actual sample-read brackets.
Kernel scheduler publication debt is unquantified: accounted runtime can originate
outside those brackets. No execution bounds, CPU/op, rates or proc fallback.
"""
from decimal import Decimal, ROUND_CEILING
import datetime as dt
import re

FORMAT = 'fdb-bench-process-cpu/v1'
CGROUP_FORMAT = 'fdb-bench-cgroup-cpu/v1'
NODES = ('node1', 'node2', 'node3')
MAX_PHASE_CLOCK_RANGE_NS = 1_000_000
CLOCK_RESERVE_NS = 2_000_000


class CPUUnqualified(ValueError):
    """A public-safe qualification reason composed only from static labels."""


def need(ok, message):
    if not ok:
        raise CPUUnqualified(message)


def mapping(value, label):
    need(isinstance(value, dict), 'invalid ' + label + ' object')
    return value


def sequence(value, label):
    need(isinstance(value, list), 'invalid ' + label + ' list')
    return value


def integer(value, label, minimum=0):
    need(type(value) is int and value >= minimum, 'invalid ' + label)
    return value


def utc_ns(text):
    need(isinstance(text, str), 'window timestamp is not explicit UTC')
    match = re.fullmatch(r'(\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d)(?:\.(\d{1,9}))?(?:Z|\+00:00)', text)
    need(match is not None, 'window timestamp is not explicit UTC')
    stamp = dt.datetime.fromisoformat(match[1]).replace(tzinfo=dt.timezone.utc)
    delta = stamp - dt.datetime(1970, 1, 1, tzinfo=dt.timezone.utc)
    return (delta.days * 86400 + delta.seconds) * 10**9 + int((match[2] or '').ljust(9, '0'))


def unit_population(layout):
    expected = {}
    for node, count in zip(NODES, (7, 5, 5)):
        units = {f'fdb-bench-{p["port"]}.service' for p in layout['processes'] if p['node'] == node}
        need(len(units) == count, 'incomplete/duplicate FDB process layout')
        need(sum(p['node'] == node for p in layout['processes']) == count, 'duplicate FDB process')
        expected[node] = units
    need(len(layout['processes']) == 17, 'expected exactly seventeen FDB processes')
    return expected


def validate_node(start, stop, samples, node, phase, units, max_gap_ns=2 * 10**9):
    """Validate the entire phase population before estimating any clean run."""
    for receipt, kind in ((start, 'start'), (stop, 'stop')):
        mapping(receipt, 'sampler receipt')
        mapping(receipt.get('cgroup_units'), 'cgroup units')
        units_receipt = mapping(receipt.get('units'), 'sampler units')
        sequence(units_receipt.get('names'), 'sampler unit names')
        mapping(units_receipt.get('starttimes'), 'sampler starttimes')
        mapping(receipt.get('cgroup_v2'), 'cgroup mount')
        need(receipt.get('host') == node and receipt.get('phase') == phase and receipt.get('kind') == kind,
             'foreign sampler receipt')
        need(receipt.get('process_cpu_format') == FORMAT, 'missing process CPU instrumentation')
        need(receipt.get('cgroup_cpu_format') == CGROUP_FORMAT and set(receipt.get('cgroup_units', {})) == units,
             'missing exclusive cgroup CPU source')
        integer(receipt.get('clock_ticks_per_second'), 'CLK_TCK', 1)
        need(receipt.get('boot_id') and receipt['units']['names'] and set(receipt['units']['names']) == units,
             'missing/foreign FDB units')
        need(set(receipt['units']['starttimes']) == units, 'missing PID population')
    hz = start['clock_ticks_per_second']
    need(stop['clock_ticks_per_second'] == hz and stop['boot_id'] == start['boot_id'], 'HZ/boot changed')
    identities = start['units']['starttimes']
    need(identities == stop['units']['starttimes'], 'MainPID changed across phase')
    groups = start['cgroup_units']
    mount = start.get('cgroup_v2', {})
    need(mount == stop.get('cgroup_v2') and mount.get('path') == '/sys/fs/cgroup'
         and 'cpu' in mount.get('controllers', []), 'missing/changed cgroup v2 CPU mount')
    integer(mount.get('device'), 'cgroup mount device')
    integer(mount.get('inode'), 'cgroup mount inode', 1)
    need(groups == stop['cgroup_units'], 'cgroup identity/membership changed across phase')
    for unit, group in groups.items():
        mapping(group, 'cgroup identity')
        mapping(identities[unit], 'process identity')
        sequence(group.get('threads'), 'cgroup threads')
        sequence(group.get('procs'), 'cgroup processes')
        need(group['path'] == '/sys/fs/cgroup/system.slice/' + unit
             and group['pid'] == identities[unit]['pid'] and group['starttime'] == identities[unit]['starttime']
             and group['procs'] == [group['pid']], 'foreign cgroup/process membership')
        integer(group.get('device'), 'cgroup device')
        integer(group.get('inode'), 'cgroup inode', 1)
        need(group['device'] == mount['device'] and group['inode'] != mount['inode'], 'foreign cgroup mount/root group')
        need(group['threads'] == sorted(set(group['threads'])) and group['pid'] in group['threads']
             and all(type(t) is int and t > 0 for t in group['threads']), 'invalid FDB thread membership')
    need(len({(g['device'], g['inode']) for g in groups.values()}) == len(units), 'shared cgroup double count')
    for identity in identities.values():
        integer(identity['pid'], 'PID', 1)
        need(isinstance(identity['starttime'], str) and identity['starttime'].isdigit(), 'bad process starttime')
    need(len({p['pid'] for p in identities.values()}) == len(units), 'shared PID double count')
    sequence(samples, 'CPU samples')
    need(len(samples) >= 2 and stop.get('samples') == len(samples), 'empty/incomplete CPU samples')
    for seq, sample in enumerate(samples):
        mapping(sample, 'CPU sample')
        mapping(sample.get('cgroup_cpu'), 'sampled cgroup CPU')
        mapping(sample.get('process_cpu'), 'sampled process CPU')
        mapping(sample.get('procs'), 'sampled process identities')
        need(sample.get('seq') == seq and sample.get('process_cpu_format') == FORMAT,
             'bad CPU sequence/format')
        need(not sample.get('error') and not sample.get('process_cpu_errors'), 'unreadable process CPU sample')
        need(sample.get('cgroup_cpu_format') == CGROUP_FORMAT and not sample.get('cgroup_cpu_errors')
             and set(sample.get('cgroup_cpu', {})) == units, 'missing/unreadable cgroup CPU sample')
        for name in ('realtime_ns', 'realtime_after_ns', 'monotonic_ns'):
            integer(sample.get(name), name, 1)
        need(sample['realtime_ns'] <= sample['realtime_after_ns'], 'negative sample read bracket')
        need(set(sample['process_cpu']) == units and set(sample['procs']) == units, 'missing/foreign sampled PID')
        for unit, identity in identities.items():
            counter = sample['process_cpu'][unit]
            need(isinstance(counter, dict) and counter.get('pid') == identity['pid']
                 and counter.get('starttime') == identity['starttime']
                 and sample['procs'][unit] == identity['starttime'], 'PID/starttime changed in CPU sample')
            integer(counter.get('user_ticks'), 'utime')
            integer(counter.get('system_ticks'), 'stime')
            group = sample['cgroup_cpu'][unit]
            need(isinstance(group, dict) and all(group.get(k) == v for k, v in groups[unit].items()),
                 'cgroup identity/membership changed in CPU sample')
            integer(group.get('usage_usec'), 'cgroup usage_usec')
            components = {'user_usec', 'system_usec'} & set(group)
            need(not components or components == {'user_usec', 'system_usec'}, 'incomplete cgroup CPU components')
            for field in components:
                integer(group[field], 'cgroup ' + field)
    correlations = [sample['realtime_ns'] - sample['monotonic_ns'] for sample in samples]
    correlation_range = max(correlations) - min(correlations)
    need(correlation_range <= MAX_PHASE_CLOCK_RANGE_NS, 'phase CPU clock correlation range exceeds 1ms')
    intervals = []
    for before, after in zip(samples, samples[1:]):
        elapsed = after['monotonic_ns'] - before['monotonic_ns']
        need(0 < elapsed <= max_gap_ns and before['realtime_after_ns'] <= after['realtime_ns'],
             'CPU gap or overlapping sample reads')
        need(abs(after['realtime_ns'] - before['realtime_ns'] - elapsed) <= 10**6, 'CPU clock step')
        usage, user, system = 0, 0, 0
        components_available = True
        for unit in units:
            for field in ('user_ticks', 'system_ticks'):
                delta = after['process_cpu'][unit][field] - before['process_cpu'][unit][field]
                need(delta >= 0, 'decreasing process CPU diagnostic counter')
            before_cpu, after_cpu = before['cgroup_cpu'][unit], after['cgroup_cpu'][unit]
            need(('user_usec' in before_cpu) == ('user_usec' in after_cpu), 'cgroup CPU component population changed')
            delta = after_cpu['usage_usec'] - before_cpu['usage_usec']
            need(delta >= 0, 'cgroup CPU reset/decrease')
            usage += delta
            if 'user_usec' in before_cpu:
                du, ds = (after_cpu[f] - before_cpu[f] for f in ('user_usec', 'system_usec'))
                need(du >= 0 and ds >= 0, 'cgroup component counter decreased')
                user, system = user + du, system + ds
            else:
                components_available = False
        intervals.append(dict(start_ns=before['realtime_ns'], end_ns=after['realtime_after_ns'],
                              seq=[before['seq'], after['seq']], usage_usec=usage,
                              user_usec=user if components_available else None,
                              system_usec=system if components_available else None))
    return dict(hz=hz, intervals=intervals, units=len(units), correlation_range_ns=correlation_range,
                coverage_start_ns=samples[0]['realtime_after_ns'], coverage_end_ns=samples[-1]['realtime_ns'])


def observe_run(run, nodes, measured_window=None):
    lifecycle_start, lifecycle_end = utc_ns(run['start_utc']), utc_ns(run['end_utc'])
    start, end = measured_window if measured_window is not None else (lifecycle_start, lifecycle_end)
    need(lifecycle_start <= start < end <= lifecycle_end and set(run['nodes']) == set(NODES),
         'invalid window/node population')
    result = {}
    for node in NODES:
        evidence = run['nodes'][node]
        bound = Decimal(evidence['u_s'])
        need(bound.is_finite() and bound >= Decimal('1.001'), 'undefined/unsafe R4 clock bound')
        r4_u = int((bound * 10**9).to_integral_value(rounding=ROUND_CEILING))
        need((evidence['widened_start_ns'], evidence['widened_end_ns']) ==
             (lifecycle_start - r4_u, lifecycle_end + r4_u), 'R4 widened window differs')
        # R4's 1Hz reserve is separate from CPU read intervals and chrony
        # bounds; the sidecar contributes its own chrony receipts.
        cpu_bound = max(bound - Decimal('1.001'), nodes[node].get('clock_bound_s', Decimal('0')))
        quantum_ns = 1 if measured_window is not None else 1_000_000
        u = int((cpu_bound * 10**9).to_integral_value(rounding=ROUND_CEILING)) + CLOCK_RESERVE_NS + quantum_ns
        lo, hi = start - u, end + u
        data = nodes[node]
        intervals = data['intervals']
        need(data['coverage_start_ns'] <= lo and data['coverage_end_ns'] >= hi,
             'CPU counter reads do not certainly bracket run')
        outer = [iv for iv in intervals if iv['start_ns'] <= hi and iv['end_ns'] >= lo]
        inner = [iv for iv in intervals if iv['start_ns'] >= start + u and iv['end_ns'] <= end - u]
        need(outer, 'empty CPU interval population')
        if measured_window is not None:
            need(len(inner) >= 10 and 5 * (outer[-1]['end_ns'] - outer[0]['start_ns']) <= 6 * (end - start),
                 'accounting temporal resolution unqualified: need ten inner intervals and <=20% sample-bracket excess')
        usage = sum(iv['usage_usec'] for iv in outer)
        # These are observed published-counter differences, not runtime assigned
        # to this bracket: scheduler publication debt has no qualified bound.
        result[node] = dict(observed_accounted_cpu_usec=usage, observed_accounted_cpu_seconds=usage / 1e6,
                            sample_bracket_started_unix_ns=outer[0]['start_ns'],
                            sample_bracket_ended_unix_ns=outer[-1]['end_ns'],
                            sample_bracket_wall_ns=outer[-1]['end_ns'] - outer[0]['start_ns'],
                            observed_accounted_user_cpu_seconds=(sum(iv['user_usec'] for iv in outer) / 1e6
                                                                if all(iv['user_usec'] is not None for iv in outer) else None),
                            observed_accounted_system_cpu_seconds=(sum(iv['system_usec'] for iv in outer) / 1e6
                                                                  if all(iv['system_usec'] is not None for iv in outer) else None),
                            sample_sequence=[outer[0]['seq'][0], outer[-1]['seq'][1]],
                            inner_intervals=len(inner), observed_intervals=len(outer),
                            temporal_selection_allowance_ns=u, phase_correlation_range_ns=data['correlation_range_ns'],
                            processes=data['units'])
    total = sum(value['observed_accounted_cpu_usec'] for value in result.values())
    return dict(nodes=result, observed_accounted_cpu_usec=total, observed_accounted_cpu_seconds=total / 1e6,
                actual_window_cpu_available=False,
                requested_window_started_unix_ns=start, requested_window_ended_unix_ns=end,
                window_kind='measured-client-UTC-fences' if measured_window is not None else 'process-lifecycle-UTC',
                scope='17 exclusive FDB unit cgroup published usage_usec deltas over three node-specific sample-read brackets; '
                      'scheduler accounting debt is unquantified and runtime may originate outside the brackets')
