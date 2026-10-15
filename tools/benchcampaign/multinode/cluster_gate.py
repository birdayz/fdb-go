"""Cluster and client-host guards shared by client_campaign_mn.py (R1) and bench_runner_mn.py (R2).

They replace the single-node guards that do not hold on the multi-node client:
  * the cluster file is the generated three-coordinator file (fdb_cluster.py
    render), every coordinator a distinct machine inside the VPC CIDR, instead
    of one coordinator at a fixed address;
  * the engine and the server state come from `status json`, not engine.txt;
  * the client machine runs NO fdbserver (the single-node telemetry guard
    demands exactly one), and there is no network namespace;
  * the server pre-arm gate (protocol environment_policy.server_pre_arm and
    placement.data_distribution): database available, check-roles passed, no
    recovery in progress, data distribution on (M1) or frozen (M2), no data
    movement in flight, storage durability lag and log queue below 1 s / 100 MB,
    and roles/generation unchanged within a case.

The single-node modules (bench_runner.py, client_campaign.py) are imported, not
copied: from this directory, else from ../singlenode. Both must be pinned.
"""
import datetime as dt
import ipaddress
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import time

# abspath, not resolve(): under Bazel, stay inside runfiles.
HERE = Path(os.path.abspath(__file__)).parent


def singlenode_dir():
    for d in (HERE, HERE.parent / 'singlenode'):
        if (d / 'bench_runner.py').is_file() and (d / 'client_campaign.py').is_file():
            return d
    raise ImportError('single-node bench_runner.py and client_campaign.py not found here or in ../singlenode')


SINGLENODE = singlenode_dir()
if str(SINGLENODE) not in sys.path:
    sys.path.append(str(SINGLENODE))

import bench_runner as generic  # noqa: E402  (single-node runner: parsing, pins, failure classes)
import fdb_cluster  # noqa: E402
import mn_common as mc  # noqa: E402

EnvironmentFailure = generic.EnvironmentFailure
CorrectnessFailure = generic.CorrectnessFailure

COORDINATORS = 3
MAX_DATA_LAG_SECONDS = 1.0
MAX_DURABILITY_LAG_SECONDS = 60.0
MAX_LOG_QUEUE_BYTES = 100 * 1000 * 1000
MOVEMENT_WAIT_SECONDS = 60  # protocol placement.data_distribution: wait up to 60 s, then abort
MOVEMENT_POLL_SECONDS = 5
LIFETIME_RESERVE_SECONDS = 600 + 300  # per-phase collection + final teardown verification
CLUSTER_RE = re.compile(r'([A-Za-z0-9_]+):([A-Za-z0-9_]+)@([^\s@]+)')
_MISSING = object()


def code_files(*modules):
    """Absolute paths of the campaign code actually imported; each must be pinned."""
    mods = [sys.modules[__name__], generic, fdb_cluster, mc, *modules]
    return {str(Path(m.__file__).resolve()) for m in mods}


# --- cluster file ----------------------------------------------------------

def cluster_file_guard(path, layout, vpc_cidr):
    """The generated three-coordinator cluster file, coordinators on distinct VPC machines."""
    text = Path(path).read_text().strip()
    match = CLUSTER_RE.fullmatch(text)
    if not match:
        raise EnvironmentFailure(f'malformed cluster file {path}')
    coords = match.group(3).split(',')
    if text != layout['cluster_file'] or coords != layout['coordinators']:
        raise EnvironmentFailure('cluster file is not the generated layout cluster file')
    if layout.get('vpc_cidr') != vpc_cidr:
        raise EnvironmentFailure(f'layout VPC {layout.get("vpc_cidr")} != --vpc-cidr {vpc_cidr}')
    network = ipaddress.ip_network(vpc_cidr, strict=True)
    hosts = set()
    for c in coords:
        host, sep, port = c.rpartition(':')
        try:
            ip = ipaddress.ip_address(host)
        except ValueError as exc:
            raise EnvironmentFailure(f'coordinator {c} is not an IP:port') from exc
        if not sep or not port.isdecimal() or not 0 < int(port) < 65536:
            raise EnvironmentFailure(f'coordinator {c} has no valid port')
        if ip not in network:
            raise EnvironmentFailure(f'coordinator {c} outside the VPC {vpc_cidr}')
        hosts.add(ip)
    if len(coords) != COORDINATORS or len(set(coords)) != COORDINATORS or len(hosts) != COORDINATORS:
        raise EnvironmentFailure('cluster file needs exactly three coordinators on three distinct machines')
    return coords


def affinity_guard(cpus):
    if sorted(os.sched_getaffinity(0)) != list(cpus):
        raise EnvironmentFailure('runner/telemetry must be pinned to the housekeeping CPUs')


# --- client host -----------------------------------------------------------

def process_table():
    found = subprocess.run(['ps', '-eo', 'pid=,comm='], check=True, capture_output=True, text=True)
    return [(int(pid), comm) for pid, comm in (line.split(maxsplit=1) for line in found.stdout.splitlines())]


def client_vm_policy(protocol):
    """The client machine's CPU groups and pre-arm thresholds, as matrix_plan names them.

    The 'server' group is the idle sentinel: no fdbserver runs on the client.
    """
    vm = protocol['runtime']['client_vm']
    t = protocol['environment_policy']['pre_arm_thresholds']
    sp = protocol['sample_policy']
    groups = [('housekeeping', vm['housekeeping_cpus'], 'housekeeping'),
              ('server', vm['idle_sentinel_cpus'], 'idle_sentinel'), ('client', vm['client_cpus'], 'client')]
    cpu_sets = [dict(name=name, cpus=generic.parse_cpus(cpus), max_busy_fraction=t[f'client_vm_{key}_aggregate_busy'],
                     max_cpu_busy_fraction=t[f'client_vm_{key}_max_core_busy']) for name, cpus, key in groups]
    population = [c for s in cpu_sets for c in s['cpus']]
    if len(set(population)) != len(population):
        raise ValueError('client VM CPU groups overlap')
    if protocol['runtime']['GOMAXPROCS'] != len(cpu_sets[2]['cpus']):
        raise ValueError('GOMAXPROCS must equal the client CPU count')
    if sp['max_steal_fraction'] != t['any_cpu_steal']:
        raise ValueError('sample_policy and pre-arm steal thresholds differ')
    return dict(cpu_sets=cpu_sets, max_busy_fraction=t['client_vm_all_cpu_aggregate_busy'],
                max_steal_fraction=sp['max_steal_fraction'], cooldown_seconds=sp['cooldown_seconds'],
                quiescence_seconds=sp['quiescence_seconds'], telemetry_seconds=1)


def telemetry_guard(samples, plan):
    """The single-node guard, except that the client machine must run no fdbserver."""
    if len(samples) < 2:
        raise EnvironmentFailure('missing telemetry endpoints')
    if any(p.get('comm') == 'fdbserver' for s in samples for p in s.get('processes', [])):
        raise EnvironmentFailure('an fdbserver runs on the client machine')

    def counters(sample):
        return {f[0]: list(map(int, f[1:9])) for line in sample['/proc/stat'].splitlines()
                if (f := line.split()) and re.fullmatch('cpu[0-9]+', f[0])}
    measured = generic.cpu_set(plan, 'server') + generic.cpu_set(plan, 'client')
    for a, b in zip(samples, samples[1:]):
        seconds = (b['monotonic_ns'] - a['monotonic_ns']) / 1e9
        if seconds > 2 or seconds <= 0:
            raise EnvironmentFailure('missing/nonmonotonic 1Hz telemetry interval')
        if seconds < 0.95:
            continue
        ac, bc = counters(a), counters(b)
        if not all('cpu' + str(i) in ac and 'cpu' + str(i) in bc for i in measured):
            raise EnvironmentFailure('missing CPU telemetry')
        for i in measured:
            delta = [y - x for x, y in zip(ac['cpu' + str(i)], bc['cpu' + str(i)])]
            if min(delta) < 0 or sum(delta) <= 0 or delta[7] / sum(delta) > plan['max_steal_fraction']:
                raise EnvironmentFailure('contaminated block: CPU steal or invalid counters')


# --- status json -----------------------------------------------------------

def _get(value, path):
    for key in path.split('.'):
        if not isinstance(value, dict) or key not in value:
            return _MISSING
        value = value[key]
    return value


def _number(v):
    return isinstance(v, (int, float)) and not isinstance(v, bool)


def status_verdict(status, layout, engine, data_distribution):
    """(errors, movement, check-roles receipt) for one status json.

    data_distribution is 'enabled' (M1: DD on and settled) or 'disabled' (M2:
    placement frozen). Movement in flight is reported separately so a caller
    may wait for it; every other finding is an error.
    """
    roles = fdb_cluster.check_roles(layout, status)
    errors = ['check-roles: ' + e for e in roles['errors']]
    if layout['engine'] != engine:
        errors.append(f'layout engine {layout["engine"]} != phase engine {engine}')
    if _get(status, 'cluster.configuration.storage_engine') != engine:
        errors.append(f'status storage_engine {_get(status, "cluster.configuration.storage_engine")!r} != {engine}')
    recovery = _get(status, 'cluster.recovery_state.name')
    if recovery != 'fully_recovered':
        errors.append(f'recovery_state {recovery if recovery is not _MISSING else "missing"!r}, not fully_recovered')
    # When present (live cluster status json), worst_data_lag_storage_server must be < 1 s.
    data_lag = _get(status, 'cluster.qos.worst_data_lag_storage_server.seconds')
    if data_lag is not _MISSING and (not _number(data_lag) or not data_lag < MAX_DATA_LAG_SECONDS):
        errors.append(f'storage data lag {data_lag!r} not below 1 s')
    lag = _get(status, 'cluster.qos.worst_durability_lag_storage_server.seconds')
    if not _number(lag) or not lag < MAX_DURABILITY_LAG_SECONDS:
        errors.append(f'storage durability lag {lag if lag is not _MISSING else "missing"!r} not below 60 s')
    # Reported only while tlogs exist: absence is unverifiable, never zero.
    queue = _get(status, 'cluster.qos.worst_queue_bytes_log_server')
    if not _number(queue) or not queue < MAX_LOG_QUEUE_BYTES:
        errors.append(f'log queue {queue if queue is not _MISSING else "missing"!r} not below 100 MB')
    disabled = _get(status, 'cluster.data_distribution_disabled') is True
    if data_distribution == 'enabled' and disabled:
        errors.append('data distribution is disabled; M1 needs it on and settled')
    elif data_distribution == 'disabled' and not disabled:
        errors.append('data distribution is enabled; M2 placement requires it frozen')
    elif data_distribution not in ('enabled', 'disabled'):
        raise ValueError('data_distribution must be enabled or disabled')
    movement = []
    # FDB 7.3 writes moving_data only while data distribution reports a shard
    # size: never with DD off. Frozen DD may omit it; with DD on (or re-initialising)
    # its absence leaves movement unverifiable, which refuses.
    if not (data_distribution == 'disabled' and _get(status, 'cluster.data.moving_data') is _MISSING):
        for key in ('in_flight_bytes', 'in_queue_bytes'):
            v = _get(status, 'cluster.data.moving_data.' + key)
            if not _number(v):
                errors.append(f'moving_data.{key} missing or not a number: data movement unverifiable')
            elif v != 0:
                movement.append(f'moving_data.{key}={v}')
    return errors, movement, roles


def role_identity(status):
    """Process classes, roles (with role IDs) and the recovery generation: must not change within a case."""
    procs = {}
    for p in (_get(status, 'cluster.processes') or {}).values():
        addr = p.get('address', '')
        procs[addr[:-4] if addr.endswith(':tls') else addr] = [
            p.get('class_type'), sorted([r.get('role'), str(r.get('id', ''))] for r in p.get('roles') or [])]
    generation = _get(status, 'cluster.generation')
    if not (isinstance(generation, int) and not isinstance(generation, bool)):
        raise EnvironmentFailure('status json lacks the recovery generation')
    return dict(generation=generation, processes=procs)


def identity_changes(before, after):
    out = []
    if before['generation'] != after['generation']:
        out.append(f'generation {before["generation"]} -> {after["generation"]} (recovery)')
    moved = sorted(a for a in set(before['processes']) | set(after['processes'])
                   if before['processes'].get(a) != after['processes'].get(a))
    if moved:
        out.append(f'roles or classes changed on {moved[:5]}')
    return out


class ServerGate:
    """Status json through fdbcli -C <cluster file>, judged by status_verdict.

    command(argv, folder, label, timeout) runs a controlled command and returns
    its stdout; the default is the single-node controlled_command, which keeps
    <label>.stdout/.stderr in folder. Tests inject a fake.
    """

    def __init__(self, cluster_file, layout, engine, data_distribution, command, sleep=time.sleep,
                 clock=time.monotonic, movement_wait_seconds=None, poll_seconds=MOVEMENT_POLL_SECONDS):
        self.cluster_file, self.layout, self.engine, self.dd = str(cluster_file), layout, engine, data_distribution
        if data_distribution not in ('enabled', 'disabled'):
            raise ValueError('data_distribution must be enabled or disabled')
        self.command, self.sleep, self.clock, self.poll = command, sleep, clock, poll_seconds
        # Frozen DD (M2) must show no movement at all; M1 waits for DD to settle.
        if movement_wait_seconds is None:
            movement_wait_seconds = MOVEMENT_WAIT_SECONDS if data_distribution == 'enabled' else 0
        self.movement_wait = movement_wait_seconds
        self.identity = self.last_identity = None
        self.last_text = self.last_roles = None

    def status(self, folder, label):
        self.last_text = self.last_roles = None
        text = self.command(['fdbcli', '-C', self.cluster_file, '--exec', 'status json'], folder, label, 30)
        self.last_text = text
        try:
            value = json.loads(text)
        except ValueError as exc:
            raise EnvironmentFailure('unparsable status json: ' + label) from exc
        if not isinstance(value, dict):
            raise EnvironmentFailure('status json is not an object: ' + label)
        return value

    def _judge(self, folder, label, status):
        errors, movement, roles = status_verdict(status, self.layout, self.engine, self.dd)
        self.last_roles = roles
        generic.dump(Path(folder) / (label + '-check-roles.json'), roles)
        identity = role_identity(status)
        if self.identity is not None:
            errors += identity_changes(self.identity, identity)
        if errors:
            raise EnvironmentFailure(f'{label}: ' + '; '.join(errors))
        self.last_identity = identity
        return movement

    def pre_arm(self, folder, label):
        """Server pre-arm gate. Returns True if it had to wait for data movement."""
        start, polls = self.clock(), 0
        while True:
            name = label if not polls else f'{label}-poll{polls}'
            movement = self._judge(folder, name, self.status(folder, name))
            if not movement:
                generic.dump(Path(folder) / (label + '-gate.json'),
                             dict(passed=True, polls=polls, waited_seconds=self.clock() - start, data_distribution=self.dd))
                return polls > 0
            if self.clock() - start >= self.movement_wait:
                raise EnvironmentFailure(f'{label}: data movement in flight after {self.movement_wait} s: '
                                         + ', '.join(movement))
            polls += 1
            self.sleep(self.poll)

    def begin_case(self, folder, label):
        """Pre-arm gate whose roles and generation become the case baseline."""
        self.identity = None
        waited = self.pre_arm(folder, label)
        self.identity = self.last_identity
        return waited

    def check(self, folder, label):
        """Post-run/post-case check: errors and role changes fail; movement is only recorded."""
        return self._judge(folder, label, self.status(folder, label))

    def clear_user_keys(self, folder, label):
        text = self.command(['fdbcli', '-C', self.cluster_file, '--exec', 'writemode on; clearrange "" "\\xff"'],
                            folder, label, 30)
        if 'Committed' not in text:
            raise EnvironmentFailure('missing committed user-key clear receipt')


def sealed_client_workloads(manifest, phase):
    """The phase's runnable workload order: the acknowledged manifest's sealed campaigns.

    It must equal the protocol order minus exactly the workloads the manifest
    records as unsealed, and only new-harness workloads may be unsealed; a
    manifest that silently drops a sealed workload is refused."""
    sealed = [c['workload'] for c in manifest['campaigns'] if c['phase'] == phase['id']]
    unsealed = [u.get('workload') for u in manifest.get('unsealed', []) if u.get('phase') == phase['id']]
    if (not sealed or len(set(sealed)) != len(sealed) or len(set(unsealed)) != len(unsealed)
            or not set(unsealed) <= set(phase.get('new_harness_workloads', []))
            or sealed != [w for w in phase['workloads'] if w not in set(unsealed)]):
        raise ValueError(f'sealed campaigns of {phase["id"]} {sealed} are not the protocol order minus the recorded '
                         f'unsealed new-harness workloads {unsealed}')
    return sealed


def sealed_testingb_cases(manifest, phase, plan):
    """The sealed plan's cases must be the protocol's cases minus exactly the manifest's recorded unsealed ones."""
    import seal_multinode as sm  # only the testing.B callers pin it
    cells = {c['key']: c['cell'] for c in sm.testingb_cells(phase)}
    keys = [c['key'] for c in plan['cases']]
    unsealed = [u.get('case') for u in manifest.get('unsealed', []) if u.get('phase') == phase['id']]
    allowed_new = set(phase.get('new_harness_cells', []))
    if (not keys or len(set(unsealed)) != len(unsealed) or not set(unsealed) <= set(cells)
            or not all(phase.get('new_harness_phase') or cells[k] in allowed_new for k in unsealed)
            or keys != [k for k in cells if k not in set(unsealed)]):
        raise ValueError(f'sealed cases of {phase["id"]} are not the protocol cases minus the recorded unsealed '
                         f'new-harness cases {unsealed}')
    return keys


def persist_receipts(gate, folder, when):
    """Keep the status json and check-roles receipt the gate judged last, as
    status-<when>.json and check-roles-<when>.json, with their SHA-256 in
    case-receipts.json: the per-case inputs of reconcile_telemetry.py (R4).
    Whatever was not obtained is not written; R4 then reports the case unverifiable."""
    folder = Path(folder)
    index_path = folder / 'case-receipts.json'
    index = json.loads(index_path.read_text()) if index_path.exists() else {}
    if gate.last_text is not None:
        path = folder / f'status-{when}.json'
        with open(path, 'x') as stream:
            stream.write(gate.last_text)
        index[path.name] = generic.sha(path)
    if gate.last_roles is not None:
        path = folder / f'check-roles-{when}.json'
        generic.dump(path, gate.last_roles, exclusive=True)
        index[path.name] = generic.sha(path)
    generic.dump(index_path, index)
    return index


def utc_now():
    """run.py's run-window format: ISO-8601 UTC with microseconds and +00:00."""
    return dt.datetime.now(dt.timezone.utc).isoformat()


def default_command(helper, env):
    def run(argv, folder, label, timeout):
        return generic.controlled_command(argv, Path(folder), label, helper, env, timeout=timeout)
    return run


def lifetime_left(deadline):
    return (mc.parse_deadline(deadline) - time.time())
