#!/usr/bin/env python3
"""Post-hoc server-telemetry reconciliation for one phase (R4). Standard library only.

  reconcile_telemetry.py --input reconcile-input.json --output report.json

Writes one verdict per case, clean | contaminated | unverifiable, with every reason
and the exact node samples counted. Contaminated and unverifiable cases are both
omitted from analysis. The report, including its phase_summary, is written once,
after every expected case has a verdict; a refused input writes nothing. Exit 0:
every case clean; 3: complete, some cases omitted; 2: input refused.

Input (fdb-bench-reconcile-input/v1); relative paths resolve against its directory:
  {
    "format": "fdb-bench-reconcile-input/v1",
    "phase": "M2rl",
    "layout": "cluster-ssd/layout.json",          fdb_cluster.py render output, exactly
    "placement_required": true,                    M2 phases (requires "frozen")
    "expected_dd_state": "frozen" | "on",          data distribution during the phase:
                                                   frozen for testing.B/M2, on for client/M1
    "client": {"host": "client",
               "receipts": ["client/before.json", "client/after.json"]},
                                                   node_sampler.py receipt --kind before|after
    "nodes": {"node1": "telemetry/node1", ...},    node_sampler.py run --output-dir, one per
                                                   layout node: receipt-start.json,
                                                   samples.jsonl, receipt-stop.json
    "expected_cases": ["<case id>", ...],          every sealed case of the phase
    "cases": [{
      "case": "<case id>",
      "runs": [{"run_id": "...", "start_utc": "...", "end_utc": "..."}],
      "run_results": ["results/.../result.json"],  run.py result.json files (run_id,
                                                   started_utc, ended_utc); either or both
      "status_before": "...", "status_after": "...",   fdbcli status json
      "roles_before": "...", "roles_after": "...",     fdb_cluster.py check-roles receipts
      "placement_compares": ["..."],                   placement_receipt.py compare output
      "runner_verdict": {"omitted": bool, "adverse": [str], "source": "<runner record>",
                         "source_sha256": "<hex>"}     REQUIRED: R1/R2's own verdict
    }]
  }
Run times are client CLOCK_REALTIME as ISO-8601 UTC with an explicit zero offset.

Scope of a node's samples: any unparsable or malformed sample line or field on a
node (bad JSON, wrong sequence, missing stamp, read error, malformed /proc/stat or
/proc/vmstat text, missing FDB CPU or oom_kill), a missing or unsealed samples.jsonl,
or a missing receipt makes EVERY case of the phase unverifiable. Interval findings
stay local to the run windows they touch: a gap > 2 s, a CLOCK_REALTIME step,
invalid counter deltas, steal, OOM kills and a vanished MainPID.

Refused (exit 2, nothing written): an input that does not name the generated layout
(fdb_cluster.LAYOUT: node1..node3, every process with its port, class and CPUs), a
telemetry directory for exactly those nodes, nonempty expected_cases, and an
expected_dd_state. Unverifiable: a node's unit receipt that does not cover exactly
the layout's fdb-bench-PORT.service units (missing or extra), no counted sample
interval, a missing required status field, or a runner_verdict that is absent,
has omitted null, or whose source no longer hashes to source_sha256. Contaminated:
any cluster_gate.status_verdict finding on the case's status before or after (the
runners' own gate, with the phase's DD state), data movement before the case or,
with DD frozen, after it, and a runner verdict that omitted the case or lists any
adverse finding.

Clock alignment (protocol environment_policy.server_telemetry_reconciliation): per
(case, node) u = max over the client's phase receipts of (|off| + root_dispersion +
0.5 * root_delay) + the same maximum over the node's phase receipts + 1 s + 0.001 s;
each run window [s, e] widens to [s - u, e + u], and every node sample interval
[realtime_i, realtime_after_(i+1)] that touches it counts.
"""
import argparse
import copy
import datetime as dt
from decimal import Decimal, ROUND_CEILING
import hashlib
import json
import os
from pathlib import Path
import sys

import cluster_gate as cg
import fdb_cluster
import mn_common as mc
import node_sampler as ns

INPUT_FORMAT = 'fdb-bench-reconcile-input/v1'
REPORT_FORMAT = 'fdb-bench-telemetry-reconciliation/v1'
SAMPLER_GRANULARITY_S = Decimal('1')
TIMESTAMP_PRECISION_S = Decimal('0.001')
STEAL_LIMIT_PERCENT = 1          # contaminated iff 100 * steal > total jiffies
MAX_GAP_NS = 2_000_000_000       # consecutive samples further apart than 2 s
MAX_CLOCK_STEP_NS = 1_000_000    # |delta realtime - delta monotonic| per interval
# /proc/stat fields user nice system idle iowait irq softirq steal. proc(5): iowait
# (and idle on some kernels) may decrease; every other decrease is invalid data.
CPU_FIELDS = 8
MAY_DECREASE = {3, 4}
STEAL = 7
DD_STATES = {'frozen': 'disabled', 'on': 'enabled'}   # input name -> cluster_gate name
PROCESS_KEYS = ('node', 'port', 'process_class', 'cpus', 'address', 'required_roles', 'allowed_roles')


class Refused(Exception):
    pass


def sha256(path):
    h = hashlib.sha256()
    with open(path, 'rb') as stream:
        for chunk in iter(lambda: stream.read(1 << 20), b''):
            h.update(chunk)
    return h.hexdigest()


class Inputs:
    """Reads input files relative to the manifest, recording each sha256."""

    def __init__(self, base):
        self.base, self.hashes = Path(base), {}

    def path(self, rel):
        return self.base / rel

    def read_text(self, rel):
        p = self.path(rel)
        text = p.read_text()
        self.hashes[str(rel)] = sha256(p)
        return text

    def json(self, rel, what):
        """-> (value, None) or (None, reason)."""
        if not isinstance(rel, str) or not rel:
            return None, f'{what}: no path given'
        try:
            return json.loads(self.read_text(rel)), None
        except FileNotFoundError:
            return None, f'{what}: missing file {rel}'
        except (OSError, UnicodeDecodeError, ValueError) as exc:
            return None, f'{what}: unreadable or unparsable {rel} ({type(exc).__name__})'


def utc_to_ns(text):
    """ISO-8601 UTC with explicit zero offset -> integer epoch ns (exact)."""
    if not isinstance(text, str):
        raise ValueError('not a string')
    value = dt.datetime.fromisoformat(text[:-1] + '+00:00' if text.endswith('Z') else text)
    if value.tzinfo is None or value.utcoffset() != dt.timedelta(0):
        raise ValueError('not UTC: ' + text)
    delta = value - dt.datetime(1970, 1, 1, tzinfo=dt.timezone.utc)
    return (delta.days * 86400 + delta.seconds) * 1_000_000_000 + delta.microseconds * 1000


def s_to_ns_ceil(seconds):
    return int((seconds * Decimal(1_000_000_000)).to_integral_value(rounding=ROUND_CEILING))


def receipt_bound(receipt, host, phase, kinds):
    """One receipt's chrony root distance, re-parsed from the raw text. -> (Decimal, None) or (None, reason)."""
    if not isinstance(receipt, dict) or receipt.get('format') != ns.RECEIPT_FORMAT:
        return None, 'not a node_sampler receipt'
    if receipt.get('host') != host or receipt.get('phase') != phase or receipt.get('kind') not in kinds:
        return None, f'receipt is for host={receipt.get("host")!r} phase={receipt.get("phase")!r} kind={receipt.get("kind")!r}'
    chrony = receipt.get('chrony')
    if not isinstance(chrony, dict) or chrony.get('rc') != 0 or not isinstance(chrony.get('raw'), str):
        return None, 'chronyc tracking failed or absent'
    fields, errors = ns.parse_chrony_tracking(chrony['raw'])
    if errors:
        return None, '; '.join(errors)
    try:
        return ns.chrony_error_bound(fields), None
    except ValueError as exc:
        return None, str(exc)


def host_bound(receipts):
    """max_phase over (kind, bound, reason) triples; None with reasons if any is undefined."""
    reasons = [f'{kind} receipt: {reason}' for kind, _, reason in receipts if reason]
    if reasons or not receipts:
        return None, reasons or ['no chrony receipt']
    return max(b for _, b, _ in receipts), []


# --- node telemetry ----------------------------------------------------------

def unit_state(receipt, expected):
    """-> (units {name: props}, unverifiable reasons, contaminated reasons) from one receipt."""
    units = receipt.get('units')
    if not isinstance(units, dict):
        return {}, ['no unit receipt'], []
    if units.get('list_rc') not in (None, 0) or units.get('show_rc') != 0:
        return {}, ['systemctl failed'], []
    try:
        props = ns.parse_systemctl_show(units.get('show_raw'))
    except ValueError as exc:
        return {}, ['unparsable systemctl show: ' + str(exc)], []
    bad, contam = [], []
    names = units.get('names')
    if not isinstance(names, list) or set(names) != set(expected) or set(props) != set(expected):
        bad.append(f'unit receipt covers {sorted(names) if isinstance(names, list) else names} and shows '
                   f'{sorted(props)}, not exactly the layout units {sorted(expected)}')
    for name in sorted(expected):
        p = props.get(name)
        if p is None or p.get('LoadState') != 'loaded':
            contam.append(f'{name} absent or not loaded')
            continue
        if p.get('ActiveState') != 'active' or p.get('SubState') != 'running':
            contam.append(f'{name} inactive ({p.get("ActiveState")}/{p.get("SubState")})')
        if not (p.get('NRestarts') or '').isdigit() or not (p.get('MainPID') or '').isdigit():
            bad.append(f'{name} NRestarts/MainPID unparsable')
        elif int(p['NRestarts']) > 0:
            contam.append(f'{name} NRestarts={p["NRestarts"]}')
        elif int(p['MainPID']) == 0:
            contam.append(f'{name} has no MainPID')
        start = (units.get('starttimes') or {}).get(name) or {}
        if start.get('starttime') is None or str(start.get('pid')) != p.get('MainPID'):
            bad.append(f'{name} MainPID starttime not recorded')
    return props, bad, contam


class Node:
    def __init__(self, name, procs, inputs, rel, phase):
        self.name = name
        self.fdb_cpus = sorted({c for p in procs for c in p['cpus']})
        self.units = {unit_name(p['port']) for p in procs}
        self.unverifiable, self.contaminated, self.samples = [], [], []
        if not self.fdb_cpus or not self.units:
            self.unverifiable.append(f'{name}: no FDB CPUs or units to check')
        self.bound, self.bound_reasons, self.intervals = None, [], []
        self.receipts = {}
        for kind in ('start', 'stop'):
            value, err = inputs.json(f'{rel}/receipt-{kind}.json', f'{name} receipt-{kind}')
            if err:
                self.unverifiable.append(err)
            self.receipts[kind] = value if isinstance(value, dict) else None
        bounds = []
        for kind, receipt in self.receipts.items():
            if receipt is None:
                bounds.append((kind, None, 'missing'))
            else:
                bounds.append((kind, *receipt_bound(receipt, name, phase, {kind})))
        self.bound, self.bound_reasons = host_bound(bounds)
        self._check_units()
        self._load_samples(inputs, f'{rel}/samples.jsonl')
        self._intervals()

    def _check_units(self):
        start, stop = self.receipts['start'], self.receipts['stop']
        # Per-sample liveness compares against the MainPID starttimes of the start receipt.
        self.start_times = ((start.get('units') or {}).get('starttimes') or {}) if start else None
        if not start or not stop:
            return
        if not start.get('boot_id') or start.get('boot_id') != stop.get('boot_id'):
            self.contaminated.append(f'{self.name} boot_id changed or unknown between start and stop')
        state = {}
        for kind, receipt in (('start', start), ('stop', stop)):
            props, bad, contam = unit_state(receipt, self.units)
            self.unverifiable += [f'{self.name} {kind}: {r}' for r in bad]
            self.contaminated += [f'{self.name} {kind}: {r}' for r in contam]
            state[kind] = (props, (receipt.get('units') or {}).get('starttimes') or {})
        for name in sorted(self.units):
            a, b = state['start'][0].get(name), state['stop'][0].get(name)
            if a and b:
                for prop in ('MainPID', 'ActiveEnterTimestampMonotonic'):
                    if a.get(prop) != b.get(prop):
                        self.contaminated.append(f'{self.name} {name} {prop} changed {a.get(prop)} -> {b.get(prop)}')
            sa, sb = state['start'][1].get(name), state['stop'][1].get(name)
            if sa and sb and sa.get('starttime') != sb.get('starttime'):
                self.contaminated.append(f'{self.name} {name} MainPID starttime changed')

    def _load_samples(self, inputs, rel):
        try:
            text = inputs.read_text(rel)
        except (OSError, UnicodeDecodeError) as exc:
            self.unverifiable.append(f'{self.name} samples: missing or unreadable ({type(exc).__name__})')
            return
        stop = self.receipts['stop'] or {}
        if stop.get('samples_sha256') != inputs.hashes.get(rel):
            self.unverifiable.append(f'{self.name} samples.jsonl differs from the stop receipt sha256')
        for n, line in enumerate(text.split('\n')[:-1] if text.endswith('\n') else text.split('\n')):
            try:
                s = json.loads(line)
                if not isinstance(s, dict) or s.get('format') != ns.SAMPLE_FORMAT or s.get('seq') != n:
                    raise ValueError('bad sample record or sequence')
                for k in ('realtime_ns', 'monotonic_ns', 'realtime_after_ns'):
                    if not isinstance(s.get(k), int):
                        raise ValueError('missing ' + k)
            except ValueError as exc:
                self.unverifiable.append(f'{self.name} samples line {n + 1} unparsable: {exc}')
                continue
            s['bad'] = None
            try:
                if 'error' in s:
                    raise ValueError(s['error'])
                s['cpu'] = ns.parse_proc_stat(s['stat'])
                s['vm'] = ns.parse_vmstat(s['vmstat'])
                if 'oom_kill' not in s['vm']:
                    raise ValueError('vmstat lacks oom_kill')
                for c in self.fdb_cpus:
                    if f'cpu{c}' not in s['cpu']:
                        raise ValueError(f'cpu{c} missing')
                if not isinstance(s.get('procs'), dict):
                    raise ValueError('procs missing')
            except (KeyError, TypeError, ValueError) as exc:
                # Sealed scope: a malformed sample discredits the node's whole record for the phase.
                s['bad'] = f'sample seq {n} unparsable: {exc}'
                self.unverifiable.append(f'{self.name} {s["bad"]}')
            self.samples.append(s)
        if not self.samples:
            self.unverifiable.append(f'{self.name} samples: none')

    def _intervals(self):
        starts = None if self.start_times is None else \
            {u: (v or {}).get('starttime') for u, v in self.start_times.items()}
        for a, b in zip(self.samples, self.samples[1:]):
            iv = dict(seq=(a['seq'], b['seq']), start_ns=a['realtime_ns'], end_ns=b['realtime_after_ns'],
                      unverifiable=[], contaminated=[], max_fdb_steal=None, max_steal_cpu=None, oom_delta=None)
            self.intervals.append(iv)
            dmono = b['monotonic_ns'] - a['monotonic_ns']
            if dmono <= 0 or b['realtime_ns'] <= a['realtime_ns']:
                iv['unverifiable'].append('clock not increasing')
            if dmono > MAX_GAP_NS:
                iv['unverifiable'].append(f'sample gap {dmono / 1e9:.3f} s > 2 s')
            if abs((b['realtime_ns'] - a['realtime_ns']) - dmono) > MAX_CLOCK_STEP_NS:
                iv['unverifiable'].append('CLOCK_REALTIME stepped against CLOCK_MONOTONIC')
            if a['bad'] or b['bad']:
                continue  # already node-wide unverifiable (_load_samples)
            worst = None
            for c in self.fdb_cpus:
                ca, cb = a['cpu'][f'cpu{c}'][:CPU_FIELDS], b['cpu'][f'cpu{c}'][:CPU_FIELDS]
                delta = [y - x for x, y in zip(ca, cb)]
                total = sum(delta)
                if any(d < 0 for i, d in enumerate(delta) if i not in MAY_DECREASE) or total <= 0:
                    iv['unverifiable'].append(f'cpu{c} counters invalid {delta}')
                    continue
                if worst is None or delta[STEAL] * worst[2] > worst[1] * total:
                    worst = (c, delta[STEAL], total)
                if 100 * delta[STEAL] > STEAL_LIMIT_PERCENT * total:
                    iv['contaminated'].append(f'steal {delta[STEAL]}/{total} on cpu{c}')
            if worst:
                iv['max_steal_cpu'], iv['max_fdb_steal'] = worst[0], round(worst[1] / worst[2], 6)
            iv['oom_delta'] = b['vm']['oom_kill'] - a['vm']['oom_kill']
            if iv['oom_delta'] != 0:
                (iv['contaminated'] if iv['oom_delta'] > 0 else iv['unverifiable']).append(f'oom_kill delta {iv["oom_delta"]}')
            for unit in sorted(self.units) if starts is not None else []:
                if b['procs'].get(unit) is None or b['procs'].get(unit) != starts.get(unit):
                    iv['contaminated'].append(f'{unit} MainPID gone or restarted')

    def window(self, ws, we):
        """Every interval touching [ws, we] plus coverage findings -> (intervals, unverifiable reasons)."""
        if len(self.samples) < 2:
            return [], ['fewer than two samples']
        reasons = []
        if self.samples[0]['realtime_ns'] > ws or self.samples[-1]['realtime_after_ns'] < we:
            reasons.append('samples do not cover the widened window')
        counted = [iv for iv in self.intervals if iv['start_ns'] <= we and iv['end_ns'] >= ws]
        if not counted:
            reasons.append('no sample interval counted')
        return counted, reasons


# --- case receipts -----------------------------------------------------------

def _addr(a):
    return a[:-4] if isinstance(a, str) and a.endswith(':tls') else a


def status_facts(status):
    """-> dict(roles, generation); raises ValueError for a missing field."""
    cluster = status.get('cluster') if isinstance(status, dict) else None
    if not isinstance(cluster, dict) or not isinstance(cluster.get('processes'), dict) or not cluster['processes']:
        raise ValueError('no cluster.processes')
    roles = {}
    for p in cluster['processes'].values():
        if not isinstance(p, dict) or not isinstance(p.get('address'), str) or not isinstance(p.get('roles'), list):
            raise ValueError('process without address or roles')
        roles[_addr(p['address'])] = [p.get('class_type'), sorted([str(r.get('role')), str(r.get('id'))] for r in p['roles'])]
    gen = cluster.get('generation')
    if not isinstance(gen, int) or isinstance(gen, bool):
        raise ValueError('cluster.generation missing')
    available = cluster.get('database_available')
    if available is None:
        available = ((status.get('client') or {}).get('database_status') or {}).get('available')
    if not isinstance(available, bool):
        raise ValueError('database availability missing')
    return dict(roles=roles, generation=gen)


_MISSING = object()


def _get(value, path):
    for key in path.split('.'):
        if not isinstance(value, dict) or key not in value:
            return _MISSING
        value = value[key]
    return value


def status_judgement(status, layout, dd):
    """The runners' gate (cluster_gate.status_verdict) on one status json.

    -> (missing fields, findings, movement). A missing field is unverifiable; it is
    filled with a passing value in a copy so that every other field is still judged."""
    fill = {'cluster.recovery_state.name': 'fully_recovered',
            'cluster.qos.worst_durability_lag_storage_server.seconds': 0,
            'cluster.qos.worst_queue_bytes_log_server': 0,
            'cluster.configuration.storage_engine': layout['engine']}
    # status_verdict accepts moving_data absent only with DD frozen.
    if not (dd == 'disabled' and _get(status, 'cluster.data.moving_data') is _MISSING):
        fill.update({'cluster.data.moving_data.in_flight_bytes': 0, 'cluster.data.moving_data.in_queue_bytes': 0})
    missing = [path for path in fill if _get(status, path) is _MISSING]
    status = copy.deepcopy(status)
    for path in missing:
        node, *_, leaf = [status] + path.split('.')
        for key in path.split('.')[:-1]:
            if not isinstance(node.get(key), dict):
                node[key] = {}
            node = node[key]
        node[leaf] = fill[path]
    errors, movement, _ = cg.status_verdict(status, layout, layout['engine'], dd)
    return missing, errors, movement


def runner_judgement(case, inputs):
    """R1/R2's own verdict on the case. -> (unverifiable, contaminated)."""
    rv = case.get('runner_verdict')
    if not isinstance(rv, dict):
        return ['runner verdict missing'], []
    bad, contam = [], []
    omitted, adverse, src = rv.get('omitted'), rv.get('adverse'), rv.get('source')
    if not isinstance(adverse, list) or not all(isinstance(a, str) for a in adverse):
        bad.append('runner verdict adverse is not a list of strings')
        adverse = []
    if not isinstance(omitted, bool):
        bad.append(f'runner record missing or without a verdict for the case ({src})')
    if not isinstance(src, str) or not src:
        bad.append('runner verdict source missing')
    else:
        try:
            digest = sha256(inputs.path(src))
            inputs.hashes[src] = digest
            if digest != rv.get('source_sha256'):
                bad.append(f'runner record {src} differs from source_sha256')
        except OSError as exc:
            bad.append(f'runner record {src} unreadable ({type(exc).__name__})')
    if omitted is True or adverse:
        contam.append('runner omitted/adverse: ' + ('; '.join(adverse) or 'omitted'))
    return bad, contam


def case_receipts(case, inputs, placement_required, layout, dd):
    """-> (unverifiable, contaminated, notes) from the case's status, roles, placement and runner receipts."""
    bad, contam, notes = [], [], []
    facts = {}
    for when in ('before', 'after'):
        status, err = inputs.json(case.get(f'status_{when}'), f'status json {when}')
        if err:
            bad.append(err)
            continue
        try:
            facts[when] = status_facts(status)
            missing, errors, movement = status_judgement(status, layout, dd)
        except ValueError as exc:
            bad.append(f'status json {when}: {exc}')
            continue
        except (AttributeError, KeyError, TypeError) as exc:
            bad.append(f'status json {when}: malformed ({type(exc).__name__})')
            continue
        bad += [f'status json {when}: {path} missing' for path in missing]
        contam += [f'status verdict {when}: {e}' for e in errors]
        # The runners refuse to arm with movement; with DD frozen, any movement is a placement risk.
        if movement and (when == 'before' or dd == 'disabled'):
            contam.append(f'data movement {when} case: ' + ', '.join(movement))
        elif movement:
            notes.append(f'data movement after case (DD on): ' + ', '.join(movement))
        roles, err = inputs.json(case.get(f'roles_{when}'), f'check-roles {when}')
        if err:
            bad.append(err)
        elif not isinstance(roles, dict) or not isinstance(roles.get('passed'), bool):
            bad.append(f'check-roles {when}: no passed verdict')
        elif not roles['passed']:
            contam.append(f'check-roles {when} failed')
    if len(facts) == 2:
        a, b = facts['before'], facts['after']
        if a['generation'] != b['generation']:
            contam.append(f'recovery: generation {a["generation"]} -> {b["generation"]}')
        for addr in sorted(set(a['roles']) | set(b['roles'])):
            if a['roles'].get(addr) != b['roles'].get(addr):
                contam.append(f'role or process-class change at {addr}')
    if placement_required:
        compares = case.get('placement_compares')
        if not isinstance(compares, list) or not compares:
            bad.append('placement compare receipt missing')
        for rel in compares if isinstance(compares, list) else []:
            value, err = inputs.json(rel, 'placement compare')
            if err:
                bad.append(err)
            elif not isinstance(value, dict) or not isinstance(value.get('equal'), bool):
                bad.append(f'placement compare {rel}: no equal verdict')
            elif not value['equal']:
                contam.append(f'placement changed ({rel})')
    rbad, rcontam = runner_judgement(case, inputs)
    return bad + rbad, contam + rcontam, notes


def case_runs(case, inputs):
    """-> (runs [(run_id, start_ns, end_ns, start_text, end_text)], unverifiable reasons)."""
    raw, bad = [], []
    for key in ('runs', 'run_results'):
        if not isinstance(case.get(key, []), list):
            return [], [f'{key} is not a list']
    for r in case.get('runs') or []:
        raw.append((r.get('run_id') if isinstance(r, dict) else None,
                    r.get('start_utc') if isinstance(r, dict) else None,
                    r.get('end_utc') if isinstance(r, dict) else None))
    for rel in case.get('run_results') or []:
        value, err = inputs.json(rel, 'run result')
        if err:
            bad.append(err)
        elif isinstance(value, dict):
            raw.append((value.get('run_id'), value.get('started_utc'), value.get('ended_utc')))
        else:
            bad.append(f'run result {rel}: not an object')
    runs = []
    for run_id, s, e in raw:
        try:
            start, end = utc_to_ns(s), utc_to_ns(e)
            if end < start:
                raise ValueError('ends before it starts')
            runs.append((run_id, start, end, s, e))
        except (TypeError, ValueError) as exc:
            bad.append(f'run {run_id}: unusable window ({exc})')
    if not raw:
        bad.append('no run windows')
    return runs, bad


# --- reconciliation ----------------------------------------------------------

def load_manifest(path):
    try:
        m = json.loads(Path(path).read_text())
    except (OSError, ValueError) as exc:
        raise Refused(f'input unreadable: {exc}') from None
    if not isinstance(m, dict) or m.get('format') != INPUT_FORMAT:
        raise Refused('input format must be ' + INPUT_FORMAT)
    if not isinstance(m.get('phase'), str) or not m['phase']:
        raise Refused('phase missing')
    if not isinstance(m.get('placement_required'), bool):
        raise Refused('placement_required must be true or false')
    if m.get('expected_dd_state') not in DD_STATES:
        raise Refused('expected_dd_state must be frozen or on')
    if m['placement_required'] and m['expected_dd_state'] != 'frozen':
        raise Refused('placement_required needs expected_dd_state frozen')
    expected = m.get('expected_cases')
    if not isinstance(expected, list) or not expected or not all(isinstance(c, str) for c in expected) \
            or len(set(expected)) != len(expected):
        raise Refused('expected_cases must be a nonempty list of unique case ids')
    cases = m.get('cases')
    if not isinstance(cases, list) or not all(isinstance(c, dict) and isinstance(c.get('case'), str) for c in cases):
        raise Refused('cases must be a list of objects with a case id')
    ids = [c['case'] for c in cases]
    if len(set(ids)) != len(ids) or not set(ids) <= set(expected):
        raise Refused('case ids must be unique and among expected_cases')
    client = m.get('client')
    if not isinstance(client, dict) or not isinstance(client.get('receipts'), list):
        raise Refused('client.receipts must be a list')
    if not isinstance(m.get('nodes'), dict) or not all(isinstance(v, str) and v for v in m['nodes'].values()):
        raise Refused('nodes must map node name to telemetry dir')
    return m


def unit_name(port):
    """The systemd unit fdb_cluster.py renders for a process (fdb-bench-PORT.service)."""
    return f'fdb-bench-{port}.service'


def validate_layout(layout):
    """The fdb_cluster.py render output, exactly: -> {node: [process, ...]}; Refused otherwise."""
    if not isinstance(layout, dict) or not isinstance(layout.get('processes'), list):
        raise Refused('layout without processes')
    if layout.get('engine') not in fdb_cluster.ENGINES or not isinstance(layout.get('redundancy_mode'), str) or \
            not isinstance(layout.get('coordinators'), list) or len(layout['coordinators']) != 3:
        raise Refused('layout lacks engine, redundancy mode or three coordinators')
    by_node = {}
    for p in layout['processes']:
        if not isinstance(p, dict) or any(k not in p for k in PROCESS_KEYS) or type(p['port']) is not int or \
                not isinstance(p['cpus'], list) or not p['cpus'] or not all(type(c) is int for c in p['cpus']):
            raise Refused('layout process without node, port, class, CPUs, address or roles')
        by_node.setdefault(p['node'], []).append(p)
    got = {n: sorted((p['port'], p['process_class'], tuple(p['cpus'])) for p in procs) for n, procs in by_node.items()}
    want = {n: sorted(fdb_cluster.LAYOUT[n]) for n in fdb_cluster.NODES}
    if got != want:
        raise Refused('layout is not the generated fdb_cluster.py layout (nodes, ports, classes, CPUs)')
    if layout.get('storage_servers') != sum(p['process_class'] == 'storage' for p in layout['processes']):
        raise Refused('layout storage_servers disagrees with its processes')
    return by_node


def reconcile(manifest_path):
    m = load_manifest(manifest_path)
    inputs = Inputs(Path(manifest_path).parent)
    inputs.hashes[str(manifest_path)] = sha256(manifest_path)
    layout, err = inputs.json(m.get('layout'), 'layout')
    if err:
        raise Refused(err)
    by_node = validate_layout(layout)
    dd = DD_STATES[m['expected_dd_state']]
    if set(m['nodes']) != set(by_node):
        raise Refused(f'nodes {sorted(m["nodes"])} differ from layout nodes {sorted(by_node)}')
    phase = m['phase']
    client_host = m['client'].get('host', 'client')

    # Client clock bound over all of the client's receipts for the phase.
    client_receipts, kinds = [], {}
    for rel in m['client']['receipts']:
        value, err = inputs.json(rel, 'client receipt')
        if err:
            client_receipts.append((rel, None, err))
            continue
        bound, reason = receipt_bound(value, client_host, phase, {'before', 'after'})
        client_receipts.append((rel, bound, reason))
        if isinstance(value, dict) and isinstance(value.get('realtime_ns'), int):
            kinds.setdefault(value.get('kind'), []).append(value['realtime_ns'])
    client_bound, client_reasons = host_bound(client_receipts)
    for kind in ('before', 'after'):
        if kind not in kinds:
            client_reasons.append(f'no client {kind} receipt')
    nodes = {name: Node(name, by_node[name], inputs, rel, phase) for name, rel in sorted(m['nodes'].items())}
    u_ns = {}
    for name, node in nodes.items():
        if client_bound is not None and node.bound is not None and not client_reasons:
            u = client_bound + node.bound + SAMPLER_GRANULARITY_S + TIMESTAMP_PRECISION_S
            u_ns[name] = (u, s_to_ns_ceil(u))

    by_id = {c['case']: c for c in m['cases']}
    all_starts, all_ends = [], []
    verdicts = []
    for case_id in m['expected_cases']:
        case = by_id.get(case_id)
        out = dict(case=case_id, unverifiable_reasons=[], contaminated_reasons=[], runs=[])
        verdicts.append(out)
        if case is None:
            out['unverifiable_reasons'].append('no case receipts')
            continue
        runs, bad = case_runs(case, inputs)
        out['unverifiable_reasons'] += bad
        bad, contam, notes = case_receipts(case, inputs, m['placement_required'], layout, dd)
        out['unverifiable_reasons'] += bad
        out['contaminated_reasons'] += contam
        out['notes'] = notes
        out['unverifiable_reasons'] += [f'clock bound undefined: client {r}' for r in client_reasons]
        for name, node in nodes.items():
            out['unverifiable_reasons'] += [f'clock bound undefined: {name} {r}' for r in node.bound_reasons]
            out['unverifiable_reasons'] += node.unverifiable
            out['contaminated_reasons'] += node.contaminated
        for run_id, start, end, s_text, e_text in runs:
            all_starts.append(start)
            all_ends.append(end)
            r = dict(run_id=run_id, start_utc=s_text, end_utc=e_text, nodes={})
            out['runs'].append(r)
            for name, node in nodes.items():
                if name not in u_ns:
                    r['nodes'][name] = dict(u_s=None)
                    out['unverifiable_reasons'].append(f'{name} run {run_id}: no clock bound')
                    continue
                u, u_n = u_ns[name]
                counted, reasons = node.window(start - u_n, end + u_n)
                seqs = [iv['seq'] for iv in counted]
                steals = [iv for iv in counted if iv['max_fdb_steal'] is not None]
                worst = max(steals, key=lambda iv: iv['max_fdb_steal'], default=None)
                r['nodes'][name] = dict(
                    u_s=str(u), widened_start_ns=start - u_n, widened_end_ns=end + u_n,
                    intervals_counted=len(counted),
                    samples_used=[seqs[0][0], seqs[-1][1]] if seqs else None,
                    max_fdb_steal=worst['max_fdb_steal'] if worst else None,
                    max_fdb_steal_cpu=worst['max_steal_cpu'] if worst else None,
                    oom_kill_delta=sum(iv['oom_delta'] or 0 for iv in counted))
                for reason in reasons:
                    out['unverifiable_reasons'].append(f'{name} run {run_id}: {reason}')
                for iv in counted:
                    where = f'{name} run {run_id} samples {iv["seq"][0]}-{iv["seq"][1]}'
                    out['unverifiable_reasons'] += [f'{where}: {x}' for x in iv['unverifiable']]
                    out['contaminated_reasons'] += [f'{where}: {x}' for x in iv['contaminated']]
    # The client receipts must bracket every run of the phase, else max_phase is not over the phase.
    if all_starts and kinds.get('before') and kinds.get('after') and \
            (min(kinds['before']) > min(all_starts) or max(kinds['after']) < max(all_ends)):
        for out in verdicts:
            out['unverifiable_reasons'].append('clock bound undefined: client receipts do not bracket the phase runs')
    for out in verdicts:
        out['verdict'] = ('contaminated' if out['contaminated_reasons'] else
                          'unverifiable' if out['unverifiable_reasons'] else 'clean')
    # Only now, with every expected case decided, is a phase summary formed.
    counts = {v: sum(o['verdict'] == v for o in verdicts) for v in ('clean', 'contaminated', 'unverifiable')}
    report = dict(
        format=REPORT_FORMAT, phase=phase, generated_utc=dt.datetime.now(dt.timezone.utc).isoformat(),
        tool_sha256={Path(__file__).name: sha256(__file__), Path(ns.__file__).name: sha256(ns.__file__)},
        constants=dict(sampler_granularity_s=str(SAMPLER_GRANULARITY_S), timestamp_precision_s=str(TIMESTAMP_PRECISION_S),
                       steal_limit_fraction='0.01', max_gap_s=MAX_GAP_NS / 1e9, max_clock_step_s=MAX_CLOCK_STEP_NS / 1e9),
        clock=dict(client=dict(bound_s=None if client_bound is None else str(client_bound), reasons=client_reasons,
                               receipts={rel: (None if b is None else str(b)) for rel, b, _ in client_receipts}),
                   nodes={n: dict(bound_s=None if node.bound is None else str(node.bound), reasons=node.bound_reasons,
                                  u_s=str(u_ns[n][0]) if n in u_ns else None) for n, node in nodes.items()}),
        nodes={n: dict(samples=len(node.samples), fdb_cpus=node.fdb_cpus, units=sorted(node.units),
                       unverifiable=node.unverifiable, contaminated=node.contaminated) for n, node in nodes.items()},
        cases=verdicts, inputs_sha256=dict(sorted(inputs.hashes.items())), reconciliation_complete=True,
        phase_summary=dict(cases=len(verdicts), **counts,
                           analysis_eligible_cases=[o['case'] for o in verdicts if o['verdict'] == 'clean'],
                           omitted_cases={o['case']: o['verdict'] for o in verdicts if o['verdict'] != 'clean'}))
    return report


def main(argv=None):
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument('--input', required=True, help='fdb-bench-reconcile-input/v1 manifest')
    ap.add_argument('--output', required=True, help='report path (exclusive, outside any Git work tree)')
    args = ap.parse_args(argv)
    try:
        mc.require_outside_repo(args.output, 'report')
        if os.path.exists(args.output):
            raise Refused('report exists: ' + args.output)
        report = reconcile(args.input)
        mc.dump_json(args.output, report)
    except (Refused, mc.GuardError) as exc:
        print('REFUSED: ' + str(exc), file=sys.stderr)
        return 2
    s = report['phase_summary']
    print(f'{report["phase"]}: {s["cases"]} cases, {s["clean"]} clean, {s["contaminated"]} contaminated, '
          f'{s["unverifiable"]} unverifiable')
    return 0 if s['clean'] == s['cases'] else 3


if __name__ == '__main__':
    sys.exit(main())
