#!/usr/bin/env python3
"""Role-scoped new native-FDB campaign hosts; no API call occurs on import.

Public API: prepare(plan, out), setup_cluster(plan, out, nodes, storage_hosts,
armid), teardown(plan, out, resources), run_on_node, copy_to_node, ssh_argv.
Runners return (returncode, stdout, stderr) and accept argv, timeout=seconds.
State, generated host keys, inventories and receipts must be outside Git.
An auxiliary ordinary child guard is acknowledged before any cloud mutation.
The caller must also arm a separately supervised foreground lifetime guard,
unless scaling explicitly selects foreground_only without independent survival.
Active work stops at first_epoch+6000; cleanup starts by first_epoch+6600,
with an absolute deadline no later than first_epoch+7200.
"""
import argparse
import contextlib
import fcntl
import hashlib
import ipaddress
import json
import math
import os
from pathlib import Path
import re
import shlex
import subprocess
import sys
import time
from types import SimpleNamespace
from decimal import Decimal, InvalidOperation

_MULTINODE = Path(__file__).resolve().parents[5] / 'tools/benchcampaign/multinode'
if str(_MULTINODE) not in sys.path:
    sys.path.insert(0, str(_MULTINODE))
import inventory as _inventory
import mn_common as _mc
import render_cloud_init as _cloud_init
import teardown_guard as _teardown_guard

KINDS = _inventory.KINDS
ROLES = ('node1', 'node2', 'node3', 'client1', 'client2')
DELETE_KINDS = ('server', 'firewall', 'ssh-key', 'network', 'placement-group')
CONFIGURE = ('configure new single ssd-redwood-1 logs:=1 commit_proxies:=1 '
             'grv_proxies:=1 resolvers:=1')
REMOTE_BASE = '/srv/otel-horizontal'
FDB_VERSION = '7.3.77'
GIB = 1024 ** 3


class ProvisionError(RuntimeError):
    """Refused input, failed command, or incomplete cleanup."""


def _state(out):
    return Path(out).resolve() / 'provision'


def _json(path, value, *, replace=False):
    path = Path(path)
    if path.is_symlink() or (path.exists() and not replace):
        raise ProvisionError(f'refusing to overwrite {path}')
    temp = path.with_name(path.name + f'.{os.getpid()}.tmp')
    _mc.write_private(temp, json.dumps(value, indent=2, sort_keys=True, allow_nan=False) + '\n')
    os.replace(temp, path)


def _load(path):
    return json.loads(Path(path).read_text())


def _sha(path):
    digest = hashlib.sha256()
    with open(path, 'rb') as stream:
        for block in iter(lambda: stream.read(1024 * 1024), b''):
            digest.update(block)
    return digest.hexdigest()


def _runner(argv, timeout=120):
    done = subprocess.run([str(a) for a in argv], capture_output=True, text=True,
                          stdin=subprocess.DEVNULL, timeout=timeout)
    return done.returncode, done.stdout, done.stderr


def _call(runner, argv, timeout=20):
    return (runner or _runner)([str(a) for a in argv], timeout=timeout)


def _checked(runner, argv, timeout=20):
    rc, stdout, stderr = _call(runner, argv, timeout)
    if rc:
        raise ProvisionError(f'command failed ({rc}): {shlex.join(argv)}: {stderr.strip()}')
    return stdout


def _roles(plan):
    schema = plan.get('schema') if type(plan) is dict else None
    if schema == 'otel_horizontal_v1':
        return ROLES
    if schema in ('otel_native_diagnostic_v1', 'otel_native_session_v1'):
        return ('node1', 'client1')
    if schema == 'otel_native_scaling_v1':
        return ('node1', 'node2', 'node3', 'client1', 'client2', 'client3')
    raise ProvisionError('unknown campaign schema; no resource roles authorized')


def _storage_max_outstanding(plan):
    if plan.get('schema') == 'otel_native_scaling_v1':
        if type(plan.get('storage_max_outstanding')) is not int or plan['storage_max_outstanding'] != 64:
            raise ProvisionError('scaling requires exact integer storage_max_outstanding 64')
        return 64
    if 'storage_max_outstanding' not in plan:
        return 64
    value = plan['storage_max_outstanding']
    if (plan.get('schema') not in ('otel_native_diagnostic_v1', 'otel_native_session_v1')
            or type(value) is not int or value not in (64, 128)
            or plan['schema'] == 'otel_native_session_v1' and value != 64):
        raise ProvisionError('storage_max_outstanding requires diagnostic schema and exact integer 64 or 128')
    return value


def _storage_process_count(plan):
    if plan.get('schema') == 'otel_native_scaling_v1':
        if type(plan.get('storage_process_count')) is not int or plan['storage_process_count'] != 3:
            raise ProvisionError('scaling requires exact integer storage_process_count 3')
        _storage_max_outstanding(plan)
        return 3
    if 'storage_process_count' not in plan:
        return 1
    value = plan['storage_process_count']
    if (plan.get('schema') not in ('otel_native_diagnostic_v1', 'otel_native_session_v1')
            or type(value) is not int or value not in (1, 2)
            or plan['schema'] == 'otel_native_session_v1' and value != 2):
        raise ProvisionError('storage_process_count requires diagnostic schema and exact integer 1 or 2')
    if value == 2 and _storage_max_outstanding(plan) != 64:
        raise ProvisionError('two storage processes require default max_outstanding 64')
    return value


def _storage_memory_profile(plan):
    if plan.get('schema') == 'otel_native_scaling_v1':
        if type(plan.get('storage_memory_profile')) is not str or plan['storage_memory_profile'] != 'scaling3ss3g':
            raise ProvisionError('scaling requires storage_memory_profile scaling3ss3g')
        _storage_process_count(plan)
        return 'scaling3ss3g'
    if 'storage_memory_profile' not in plan:
        return 'baseline'
    value = plan['storage_memory_profile']
    if (plan.get('schema') != 'otel_native_diagnostic_v1' or type(value) is not str
            or value not in ('baseline', 'capacity16g')):
        raise ProvisionError('storage_memory_profile is diagnostic-only baseline or capacity16g')
    if value == 'capacity16g' and _storage_process_count(plan) != 2:
        raise ProvisionError('capacity16g requires exactly two storage processes')
    return value


def _scaling_mode(plan):
    import policy
    try:
        return policy.scaling_execution_mode(plan)
    except ValueError as exc:
        raise ProvisionError(str(exc)) from exc


def _managed_watch_config(plan):
    import supervisor
    try:
        return supervisor.validate_config(plan)
    except (OSError, ValueError) as exc:
        raise ProvisionError('invalid managed supervisor configuration: ' + str(exc)) from exc


def _admit_managed_watch(plan, out, clock):
    """Active-work gate only; teardown must remain possible after admission fails."""
    import supervisor
    config = _managed_watch_config(plan)
    if Path(out).resolve() != Path(config['output']) / 'provision':
        raise ProvisionError('provision output does not match the managed supervisor configuration')
    try:
        supervisor.admit(plan, out, clock=clock)
    except (OSError, ValueError, subprocess.SubprocessError) as exc:
        raise ProvisionError('managed supervisor admission refused: ' + str(exc)) from exc


def _validate_scaling(plan):
    if plan.get('schema') != 'otel_native_scaling_v1':
        return
    stages = plan.get('stages')
    if (type(stages) is not list or len(stages) != 3
            or any(type(stage) is not list or len(stage) != 2
                   or any(type(value) is not int for value in stage) for stage in stages)
            or stages != [[1, 1], [2, 2], [3, 3]]):
        raise ProvisionError('scaling stages must be exactly typed [[1,1],[2,2],[3,3]]')
    for key, value in (('count', 100000), ('measured_count', 375000), ('measured_seconds', 120),
                       ('batch', 64), ('workers', 8), ('gomaxprocs', 4)):
        if type(plan.get(key)) is not int or plan[key] != value:
            raise ProvisionError('scaling plan requires exact typed ' + key)
    if (type(plan.get('prior_rows_upper')) is not int or plan['prior_rows_upper'] < 0
            or plan['prior_rows_upper'] + 2850000 > 10000000):
        raise ProvisionError('scaling cumulative row authorization exceeds ten million')
    if plan.get('replication') != 'single':
        raise ProvisionError('scaling requires single-replica capacity experiment')
    if any(key in plan for key in ('topologies', 'measured_topologies', 'prerequisite_topologies',
                                  'selected_topologies', 'probe_count', 'probe_seconds', 'cases')):
        raise ProvisionError('scaling plans cannot declare comparative or diagnostic case fields')
    if _scaling_mode(plan) == 'managed_watch':
        _managed_watch_config(plan)


def _names(plan):
    prefix = plan['prefix']
    suffixes = {'network': 'net', 'firewall': 'fw', 'ssh-key': 'key', 'placement-group': 'spread'}
    return {kind: ({f'{prefix}-{r}' for r in _roles(plan)} if kind == 'server' else
                   {f'{prefix}-{suffixes[kind]}'}) for kind in DELETE_KINDS}


def _network(plan):
    return ipaddress.ip_network(plan.get('network_cidr', '10.93.0.0/24'), strict=True)


def validate_plan(plan, out, clock=time.time):
    """Local-only validation. It neither calls hcloud nor executes sealed assets."""
    roles = _roles(plan)
    if plan.get('approved') is not True:
        raise ProvisionError('an explicitly approved supported campaign plan is required')
    _storage_max_outstanding(plan)
    _storage_process_count(plan)
    _storage_memory_profile(plan)
    _validate_scaling(plan)
    if plan['schema'] == 'otel_native_session_v1':
        if type(plan.get('cases')) is not list or plan['cases'] != ['capacity16g', 'baseline']:
            raise ProvisionError('session cases must be exactly capacity16g then baseline')
        if (type(plan.get('storage_process_count')) is not int or plan['storage_process_count'] != 2
                or type(plan.get('storage_max_outstanding')) is not int or plan['storage_max_outstanding'] != 64):
            raise ProvisionError('session requires two storage processes and max_outstanding 64')
        if (type(plan.get('prior_rows_upper')) is not int or plan['prior_rows_upper'] < 0
                or plan['prior_rows_upper'] + 2200000 > 10000000):
            raise ProvisionError('session cumulative row authorization exceeds ten million')
    if plan['schema'] in ('otel_native_diagnostic_v1', 'otel_native_session_v1'):
        for key, value in (('count', 100000), ('probe_count', 1000000), ('probe_seconds', 120)):
            if type(plan.get(key)) is not int or plan[key] != value:
                raise ProvisionError('diagnostic plan requires exact typed ' + key)
        if any(key in plan for key in ('topologies', 'measured_topologies', 'prerequisite_topologies', 'selected_topologies', 'measured_count')):
            raise ProvisionError('diagnostic plans cannot declare comparative topology fields')
    _mc.check_name(plan['prefix'])
    _mc.parse_campaign_label('campaign=' + plan['campaign'])
    for names in _names(plan).values():
        for name in names:
            _mc.check_name(name)
    if plan.get('location') != 'hel1' or plan.get('server_type') != 'ccx43':
        raise ProvisionError(f'only {len(roles)} new ccx43 servers in hel1 are authorized')
    first, deadline = plan['first_epoch'], plan['deadline_epoch']
    if (not all(isinstance(v, int) and not isinstance(v, bool) for v in (first, deadline))
            or not 600 < deadline - first <= 7200 or first > clock()
            or clock() >= min(first + 6000, deadline - 600)):
        raise ProvisionError('invalid absolute lifetime or cleanup window')
    controller = ipaddress.ip_network(plan['controller_cidr'], strict=True)
    if controller.prefixlen < (24 if controller.version == 4 else 48):
        raise ProvisionError('controller SSH CIDR is too broad')
    net = _network(plan)
    if net.version != 4 or not net.is_private or net.prefixlen != 24:
        raise ProvisionError('campaign network must be an isolated private IPv4 /24')
    _mc.require_outside_repo(out, 'campaign output')
    for key in ('hcloud', 'controller_private_key', 'controller_public_key', 'binary',
                'fdbserver', 'fdbcli', 'source_manifest'):
        value = Path(plan[key])
        if (not value.is_absolute() or not value.is_file()
                or (key in ('binary', 'fdbserver', 'fdbcli', 'source_manifest') and value.is_symlink())):
            raise ProvisionError(f'{key} must name an existing absolute local file (sealed assets cannot be symlinks)')
    if not os.access(plan['hcloud'], os.X_OK):
        raise ProvisionError('hcloud context wrapper is not executable')
    if Path(plan['controller_private_key']).stat().st_mode & 0o077:
        raise ProvisionError('controller private key must be mode 0600')
    _cloud_init.read_public_key(plan['controller_public_key'])
    for key in ('binary', 'fdbserver', 'fdbcli'):
        expected = plan[key + '_sha256']
        if not re.fullmatch(r'[a-f0-9]{64}', expected) or _sha(plan[key]) != expected:
            raise ProvisionError(f'{key} SHA-256 does not match the sealed plan')
    manifest_sha = plan.get('input_manifest_sha256')
    if not manifest_sha or _sha(plan['source_manifest']) != manifest_sha:
        raise ProvisionError('source manifest SHA-256 does not match the sealed plan')


def _baseline(path):
    return {kind: _mc.parse_json_list((Path(path) / f'before-{kind}.json').read_text(), kind)
            for kind in KINDS}


def _active_deadline(plan):
    return min(plan['first_epoch'] + 6000, plan['deadline_epoch'] - 600)


def _remaining(plan, timeout, clock=time.time, *, active=False):
    remaining = (_active_deadline(plan) if active else plan['deadline_epoch']) - clock()
    if remaining <= 0:
        raise ProvisionError('absolute command deadline reached')
    return min(timeout, remaining)


def _inventory_run(plan, runner, argv, clock=time.time, *, active=False):
    try:
        return _call(runner, argv, timeout=_remaining(plan, 10, clock, active=active))
    except (OSError, ProvisionError, subprocess.SubprocessError) as exc:
        return 1, '', str(exc)


def _list(plan, kind, runner):
    result = _inventory.list_kind(plan['hcloud'], kind,
                                  lambda argv: _inventory_run(plan, runner, argv))
    _inventory.ids(result, kind)
    return result


def _preflight(plan, baseline):
    _roles(plan)
    protected_ids = {k: set(_inventory.ids(v, k)) for k, v in baseline.items()}
    for kind, items in baseline.items():
        for item in items:
            if ((item.get('labels') or {}).get('campaign') == plan['campaign']
                    or _mc.owned_name(item.get('name', ''), plan['prefix'])):
                raise ProvisionError('refusing to adopt existing campaign/prefix resource')
            if kind == 'network' and _network(plan).overlaps(ipaddress.ip_network(item['ip_range'])):
                raise ProvisionError('campaign network overlaps an existing network')
    return protected_ids


@contextlib.contextmanager
def _lock(state):
    with open(Path(state) / 'mutation.lock', 'a') as stream:
        os.chmod(stream.name, 0o600)
        fcntl.flock(stream, fcntl.LOCK_EX)
        yield


def _check_window(plan, state, clock, guard=None):
    lease_path = state / 'guard-plan.json'
    if plan.get('schema') == 'otel_native_scaling_v1' and not lease_path.exists():
        raise ProvisionError('immutable scaling lease is missing')
    if lease_path.exists():
        lease = _load(lease_path)
        if any(plan.get(key) != lease.get(key) for key in ('campaign', 'prefix', 'first_epoch', 'deadline_epoch')):
            raise ProvisionError('campaign identity or original lease deadline changed')
        if 'otel_native_scaling_v1' in (lease.get('schema'), plan.get('schema')):
            if plan != lease or json.dumps(plan, sort_keys=True) != json.dumps(lease, sort_keys=True):
                raise ProvisionError('immutable scaling lease or stage parameters changed')
        if lease.get('schema') == 'otel_native_session_v1':
            if plan.get('schema') == 'otel_native_session_v1':
                expected, actual = lease, plan
            elif plan.get('schema') == 'otel_native_diagnostic_v1' and plan.get('storage_memory_profile') in lease['cases']:
                skip = {'schema', 'cases', 'prior_rows_upper', 'storage_memory_profile'}
                expected = {k: v for k, v in lease.items() if k not in skip}
                actual = {k: v for k, v in plan.items() if k not in skip}
            else:
                raise ProvisionError('session only authorizes explicit diagnostic case profiles')
            if json.dumps(actual, sort_keys=True) != json.dumps(expected, sort_keys=True):
                raise ProvisionError('immutable session lease or common case parameters changed')
    if (clock() >= _active_deadline(plan)
            or (state / 'cleanup-started.json').exists()
            or (guard is not None and guard.poll() is not None)):
        raise ProvisionError('cleanup has started, deadline reached, or lifetime guard exited')
    if plan.get('schema') == 'otel_native_scaling_v1' and _scaling_mode(plan) == 'managed_watch':
        _admit_managed_watch(plan, state.parent, clock)


def _save_resources(state, resources):
    _json(state / 'resources.json', resources, replace=True)


def _command_receipt(plan, state, argv, clock):
    first = _load(state / 'resources.json').get('first_create_epoch', clock())
    with open(state / 'api-commands.jsonl', 'a', encoding='utf-8') as stream:
        os.chmod(stream.name, 0o600)
        stream.write(json.dumps(dict(argv=argv, epoch=clock(), since_first_seconds=clock()-first,
                                     deadline_epoch=plan['deadline_epoch'])) + '\n')
        stream.flush()
        os.fsync(stream.fileno())


def _create(plan, state, resources, kind, argv, runner, clock, guard):
    """Record an exact newly created ID before the next mutation."""
    with _lock(state):
        _check_window(plan, state, clock, guard)
        if 'first_create_epoch' not in resources:
            resources['first_create_epoch'] = clock()
            resources['hard_deadline_epoch'] = plan['deadline_epoch']
            resources['cleanup_epoch'] = plan['deadline_epoch'] - 600
            _save_resources(state, resources)
        _command_receipt(plan, state, argv, clock)
        started = clock()
        timeout = _remaining(plan, 120, clock, active=True)
        timed_out = False
        try:
            rc, stdout, stderr = _call(runner, argv, timeout=timeout)
        except subprocess.TimeoutExpired as exc:
            rc, stdout, stderr, timed_out = None, exc.stdout or '', exc.stderr or '', True
        except (OSError, subprocess.SubprocessError) as exc:
            rc, stdout, stderr = None, '', str(exc)
        stdout = stdout.decode('utf-8', errors='replace') if isinstance(stdout, bytes) else stdout
        stderr = stderr.decode('utf-8', errors='replace') if isinstance(stderr, bytes) else stderr
        receipt = state / f'create-{kind}-{time.time_ns()}.json'
        _json(receipt, dict(kind=kind, argv=argv, returncode=rc, stdout=stdout, stderr=stderr,
                            timed_out=timed_out, timeout_seconds=timeout,
                            epoch=started, finished_epoch=clock()))
        resources.setdefault('creation_attempts', []).append(str(receipt))
        _save_resources(state, resources)
        if timed_out or type(rc) is not int or rc != 0:
            raise ProvisionError(f'{kind}: create failed; no resubmission: stdout={stdout!r}; stderr={stderr!r}')
        _check_window(plan, state, clock, guard)
        items = _list(plan, kind, runner)
        expected = argv[argv.index('--name') + 1]
        candidates = [i for i in items if i.get('name') == expected]
        if len(candidates) != 1:
            raise ProvisionError(f'{kind}: create did not yield one exact named resource')
        item = candidates[0]
        protected = resources['protected_ids'][kind]
        if (item['id'] in protected or (item.get('labels') or {}).get('campaign') != plan['campaign']):
            raise ProvisionError(f'{kind}: created resource is unowned or protected')
        resources['ids'][kind].append(item['id'])
        _save_resources(state, resources)
        return item


def _mutate(plan, state, argv, runner, clock, guard):
    with _lock(state):
        _check_window(plan, state, clock, guard)
        _command_receipt(plan, state, argv, clock)
        return _checked(runner, argv, timeout=_remaining(plan, 20, clock, active=True))


def _attach_action(value, sid, nid, expected_id=None):
    action = value.get('action') if type(value) is dict else None
    if (type(action) is not dict or type(action.get('id')) is not int or action['id'] <= 0
            or expected_id is not None and action['id'] != expected_id
            or action.get('command') != 'attach_to_network'
            or action.get('status') not in ('running', 'success', 'error')
            or 'error' not in action or action['error'] is not None
            or type(action.get('resources')) is not list):
        raise ProvisionError('malformed, foreign, or failed network-attach action')
    identities = []
    for resource in action['resources']:
        if (type(resource) is not dict or set(resource) != {'id', 'type'}
                or type(resource['id']) is not int
                or (resource['type'], resource['id']) not in (('server', sid), ('network', nid))):
            raise ProvisionError('network-attach action names a foreign or malformed resource')
        identities.append((resource['type'], resource['id']))
    if (len(identities) not in (1, 2) or len(set(identities)) != len(identities)
            or ('server', sid) not in identities or action['status'] == 'error'):
        raise ProvisionError('network-attach action lacks its sole owned server or failed')
    return action


def _attach_network(plan, state, resources, server, network, private, runner, clock, guard, sleep):
    """One POST, then bounded action polling and exact binding readback under the mutation lock."""
    sid, nid = server.get('id'), network.get('id')
    if (type(sid) is not int or sid <= 0 or sid not in resources['ids']['server']
            or sid in resources['protected_ids']['server'] or server.get('name') not in _names(plan)['server']
            or (server.get('labels') or {}).get('campaign') != plan['campaign']
            or type(nid) is not int or nid <= 0 or nid not in resources['ids']['network']
            or nid in resources['protected_ids']['network'] or network.get('name') != plan['prefix']+'-net'
            or (network.get('labels') or {}).get('campaign') != plan['campaign']
            or network.get('ip_range') != str(_network(plan))
            or ipaddress.IPv4Address(private) not in _network(plan)):
        raise ProvisionError('network attach requires exact new owned server/network IDs')
    with _lock(state):
        _check_window(plan, state, clock, guard)
        until = min(clock() + 120, _active_deadline(plan))
        if any(item.get('server_id') == sid for item in resources.get('network_attachments', [])):
            raise ProvisionError('network attach was already submitted or attempted; refusing resubmission')
        entry = dict(server_id=sid, network_id=nid, private_ip=private, started_epoch=clock(), calls=[])
        resources.setdefault('network_attachments', []).append(entry)
        _save_resources(state, resources)

        def api(path, method='GET', data=None):
            _check_window(plan, state, clock, guard)
            left = until - clock()
            if left <= 0:
                raise ProvisionError('network-attach action exceeded 120-second operation budget')
            argv = [plan['hcloud'], 'api', path, '--method', method]
            if data is not None:
                argv += ['--data', json.dumps(data, sort_keys=True)]
            timeout = min(left, _remaining(plan, 20, clock, active=True))
            started, timed_out = clock(), False
            _command_receipt(plan, state, argv, clock)
            try:
                rc, stdout, stderr = _call(runner, argv, timeout=timeout)
            except subprocess.TimeoutExpired as exc:
                rc, stdout, stderr, timed_out = None, exc.stdout or '', exc.stderr or '', True
            except (OSError, subprocess.SubprocessError) as exc:
                rc, stdout, stderr = None, '', str(exc)
            stdout = stdout.decode('utf-8', errors='replace') if isinstance(stdout, bytes) else stdout
            stderr = stderr.decode('utf-8', errors='replace') if isinstance(stderr, bytes) else stderr
            receipt = state / f'attach-{sid}-call{len(entry["calls"])+1}.json'
            _json(receipt, dict(argv=argv, returncode=rc, stdout=stdout, stderr=stderr,
                                timed_out=timed_out, timeout_seconds=timeout,
                                epoch=started, finished_epoch=clock()))
            entry['calls'].append(str(receipt))
            _save_resources(state, resources)
            if timed_out or type(rc) is not int or rc != 0:
                raise ProvisionError(f'network-attach API call failed; no resubmission: stdout={stdout!r}; stderr={stderr!r}')
            def pairs(items):
                result = {}
                for key, value in items:
                    if key in result:
                        raise ProvisionError('duplicate network-attach response key')
                    result[key] = value
                return result
            def constant(_):
                raise ProvisionError('nonfinite attach response')
            def finite(text):
                value = float(text)
                if not math.isfinite(value):
                    raise ProvisionError('nonfinite attach response')
                return value
            try:
                value = json.loads(stdout, object_pairs_hook=pairs, parse_constant=constant, parse_float=finite)
            except (TypeError, ValueError) as exc:
                raise ProvisionError('invalid network-attach API JSON') from exc
            return value

        response = api(f'/servers/{sid}/actions/attach_to_network', 'POST', dict(network=nid, ip=private))
        candidate = response.get('action') if type(response) is dict else None
        if type(candidate) is dict and type(candidate.get('id')) is int and candidate['id'] > 0:
            entry['action_id'] = candidate['id']
            _save_resources(state, resources)  # Preserve the returned action ID before validating other fields.
        action = _attach_action(response, sid, nid)
        aid = action['id']
        while True:
            action = _attach_action(api(f'/networks/actions/{aid}'), sid, nid, aid)
            _check_window(plan, state, clock, guard)
            if action['status'] == 'success':
                break
            if clock() >= until:
                raise ProvisionError('network-attach action exceeded 120-second operation budget')
            sleep(min(2, max(0, until-clock())))
        response = api(f'/servers/{sid}')
        actual = response.get('server') if type(response) is dict else None
        private_net = actual.get('private_net') if type(actual) is dict else None
        labels = actual.get('labels') if type(actual) is dict else None
        if (type(actual) is not dict or type(actual.get('id')) is not int or actual['id'] != sid
                or actual.get('name') != server['name']
                or type(labels) is not dict or labels.get('campaign') != plan['campaign']
                or type(private_net) is not list or len(private_net) != 1 or type(private_net[0]) is not dict
                or type(private_net[0].get('network')) is not int or private_net[0]['network'] != nid
                or private_net[0].get('ip') != private
                or private_net[0].get('alias_ips') != []):
            raise ProvisionError('successful network action lacks exact owned server private-network binding')
        _check_window(plan, state, clock, guard)
        entry.update(completed=True, finished_epoch=clock())
        _save_resources(state, resources)
        return actual


def _arm(plan, out, state, popen, clock, sleep):
    plan_path = state / 'guard-plan.json'
    _json(plan_path, plan)
    log = open(state / 'guard.log', 'x', encoding='utf-8')
    os.chmod(log.name, 0o600)
    try:
        child = (popen or subprocess.Popen)(
            [sys.executable, '-B', str(Path(__file__).resolve()), '--guard-plan', str(plan_path),
             '--guard-out', str(Path(out).resolve())],
            stdin=subprocess.DEVNULL, stdout=log, stderr=subprocess.STDOUT, close_fds=True)
    finally:
        log.close()
    until = min(clock() + 20, _active_deadline(plan))
    while clock() < until:
        if child.poll() is not None:
            raise ProvisionError('lifetime guard exited before acknowledgment')
        ready = state / 'guard-ready.json'
        if ready.exists():
            value = _load(ready)
            if (value.get('pid') == child.pid and value.get('plan_sha256') == _sha(plan_path)
                    and value.get('cleanup_epoch') == plan['deadline_epoch'] - 600):
                return child
            raise ProvisionError('lifetime guard acknowledgment does not match the sealed plan')
        sleep(0.1)
    raise ProvisionError('lifetime guard did not acknowledge readiness')


def _ssh_options(plan, out, node):
    known = node.get('known_hosts') or str(_state(out) / 'cloud-init' / 'known_hosts')
    return ['-i', plan['controller_private_key'], '-o', 'BatchMode=yes',
            '-o', 'IdentitiesOnly=yes', '-o', 'StrictHostKeyChecking=yes',
            '-o', 'UserKnownHostsFile=' + known, '-o', 'GlobalKnownHostsFile=/dev/null',
            '-o', 'HostKeyAlias=' + node['name'], '-o', 'ConnectTimeout=5',
            '-o', 'ConnectionAttempts=1', '-o', 'ServerAliveInterval=5',
            '-o', 'ServerAliveCountMax=2']


def ssh_argv(plan, out, node, remote_args):
    """Pinned SSH command; remote argv is quoted once, never interpolated."""
    return ['ssh', *_ssh_options(plan, out, node), 'root@' + node['host'],
            shlex.join([str(a) for a in remote_args])]


def _authorized_node(plan, out, node):
    resources = _load(_state(out) / 'resources.json')
    matches = [n for n in resources['nodes'].values() if n['id'] == node.get('id')]
    if (set(resources['nodes']) != set(_roles(plan))
            or len(matches) != 1 or any(node.get(k) != matches[0].get(k)
                                  for k in ('name', 'host', 'private_ip', 'known_hosts'))
            or plan.get('schema') == 'otel_native_scaling_v1' and node.get('role') != matches[0].get('role')
            or node['id'] in resources['protected_ids']['server']
            or resources['campaign'] != plan['campaign']):
        raise ProvisionError('SSH target is not an exact campaign-owned node')


def run_on_node(plan, out, node, remote_args, *, runner=None, timeout=120):
    _authorized_node(plan, out, node)
    _check_window(plan, _state(out), time.time)
    return _call(runner, ssh_argv(plan, out, node, remote_args),
                 timeout=_remaining(plan, timeout, active=True))


def copy_to_node(plan, out, node, local_path, remote_path, *, runner=None, timeout=120):
    _authorized_node(plan, out, node)
    _check_window(plan, _state(out), time.time)
    # SFTP-mode scp does not expand a remote shell path. Restrict its destination
    # nevertheless, so legacy scp implementations cannot introduce a shell.
    if not re.fullmatch(r'/[A-Za-z0-9_./-]+', str(remote_path)):
        raise ProvisionError('unsafe remote copy path')
    return _call(runner, ['scp', *_ssh_options(plan, out, node), '--', str(Path(local_path).resolve()),
                          f'root@{node["host"]}:{remote_path}'], timeout=_remaining(plan, timeout, active=True))


def disk_floor(total_bytes):
    return 264 * GIB + (6 * int(total_bytes) + 99) // 100


def _host_prepare_script(plan):
    root = REMOTE_BASE + '/' + plan['campaign']
    checks = '\n'.join(f"echo '{plan[k + '_sha256']}  {root}/bin/{name}' | sha256sum -c -"
                       for k, name in [('binary', 'otel-demo'), ('fdbserver', 'fdbserver'), ('fdbcli', 'fdbcli')])
    return f'''set -eu
chmod 0755 {root}/bin/*
{checks}
{root}/bin/fdbserver --version | grep -F '{FDB_VERSION}'
{root}/bin/fdbcli --version | grep -F '{FDB_VERSION}'
id foundationdb >/dev/null 2>&1 || useradd --system --no-create-home --shell /usr/sbin/nologin foundationdb
systemctl is-active --quiet chrony
findmnt -n -o FSTYPE -T {root} | grep -x ext4
opts=$(findmnt -n -o OPTIONS -T {root})
case ",$opts," in *,nobarrier,*|*,noload,*|*,data=writeback,*|*,ro,*) exit 1;; esac
dev=$(findmnt -n -o SOURCE -T {root})
tune2fs -l "$dev" | grep '^Filesystem features:' | grep -w metadata_csum | grep -w has_journal
python3 - {root} <<'PY'
import json, math, os, sys
root = sys.argv[1]
s = os.statvfs(root)
total, free = s.f_blocks*s.f_frsize, s.f_bavail*s.f_frsize
floor = 264*1024**3 + (6*total+99)//100
if free < floor: raise SystemExit('initial disk floor not met: %d < %d' % (free, floor))
print(json.dumps(dict(filesystem='ext4', total_bytes=total, free_bytes=free, floor_bytes=floor,
                     native=True, fsync_preserved=True, checksums_preserved=True)))
PY
'''


def _require_result(value, what):
    rc, stdout, stderr = value
    if rc:
        raise ProvisionError(f'{what} failed ({rc}): {stderr.strip()}')
    return stdout


def _ipv6_absent(value):
    if value is None:
        return True
    return (type(value) is dict and set(value) == {'id', 'ip', 'blocked', 'dns_ptr'}
            and type(value['id']) is int and value['id'] == 0
            and type(value['ip']) is str and value['ip'] == ''
            and value['blocked'] is False
            and type(value['dns_ptr']) is list and value['dns_ptr'] == [])


def _capture_ip_snapshots(plan, state, resources, runner, clock):
    # Keep the exact API strings even when a subsequent shape or ownership
    # check refuses them. Capture both populations before validating either.
    raw = {}
    for kind in ('server', 'primary-ip'):
        argv = [plan['hcloud'], kind, 'list', '-o', 'json']
        started = clock()
        rc, stdout, stderr = _inventory_run(plan, runner, argv, clock, active=True)
        raw[kind] = dict(argv=argv, returncode=rc, stdout=stdout, stderr=stderr,
                         epoch=started, finished_epoch=clock())
    receipt = state / f'ip-snapshot-{time.time_ns()}.json'
    _json(receipt, dict(read_only=True, before_validation=True, raw=raw))
    resources.setdefault('primary_ip_snapshot_receipts', []).append(str(receipt))
    _save_resources(state, resources)
    result = {}
    for kind, captured in raw.items():
        if captured['returncode']:
            raise ProvisionError(f'{kind}: IP proof listing failed: {captured["stderr"].strip()}')
        items = _mc.parse_json_list(captured['stdout'], kind)
        _inventory.ids(items, kind)
        result[kind] = items
    return result['server'], result['primary-ip']


def _verified_ipv4(plan, resources, servers, ips, server):
    sid = server['id']
    if (sid not in resources['ids']['server'] or sid in resources['protected_ids']['server']
            or server.get('name') not in _names(plan)['server']
            or (server.get('labels') or {}).get('campaign') != plan['campaign']):
        raise ProvisionError('IPv4 repair server is not an exact new owned server')
    v4 = (server.get('public_net') or {}).get('ipv4') or {}
    rid = v4.get('id')
    assigned = {ip['id'] for ip in ips if ip.get('assignee_type') == 'server'
                and ip.get('assignee_id') == sid}
    references = {s['id'] for s in servers
                  if ((s.get('public_net') or {}).get('ipv4') or {}).get('id') == rid}
    by_id = {ip['id']: ip for ip in ips}
    if (not isinstance(rid, int) or rid in resources['protected_ids']['primary-ip']
            or assigned != {rid} or references != {sid} or rid not in by_id
            or not _ipv6_absent((server.get('public_net') or {}).get('ipv6'))):
        raise ProvisionError('public IP assignment is not an exact new IPv4-only server assignment')
    ip = by_id[rid]
    if (ip.get('type') != 'ipv4' or ip.get('assignee_type') != 'server'
            or ip.get('assignee_id') != sid or ip.get('ip') != v4.get('ip')):
        raise ProvisionError('IPv4 binding does not match its exact sole owned server')
    ipaddress.IPv4Address(v4['ip'])
    return ip


def _provider_location(server):
    names = []
    top = server.get('location')
    legacy = server.get('datacenter')
    if top is not None:
        if type(top) is not dict or type(top.get('name')) is not str:
            raise ProvisionError('malformed top-level server location')
        names.append(top['name'])
    if legacy is not None:
        if (type(legacy) is not dict or type(legacy.get('location')) is not dict
                or type(legacy['location'].get('name')) is not str):
            raise ProvisionError('malformed legacy server location')
        names.append(legacy['location']['name'])
    if not names or any(name != 'hel1' for name in names):
        raise ProvisionError('server location is missing, contradictory, or not hel1')
    return 'hel1'


def _verify_provider_server(plan, resources, node, server, network_id, group_id):
    _provider_location(server)
    st = server.get('server_type')
    group = server.get('placement_group')
    private = server.get('private_net')
    if (type(st) is not dict or st.get('name') != 'ccx43' or st.get('cpu_type') != 'dedicated'
            or type(st.get('cores')) is not int or st['cores'] != 16
            or type(st.get('memory')) not in (int, float) or st['memory'] != 64
            or type(st.get('disk')) is not int or st['disk'] != 360
            or st.get('storage_type') != 'local' or st.get('architecture') != 'x86'
            or type(server.get('primary_disk_size')) is not int or server['primary_disk_size'] != 360):
        raise ProvisionError('server is not exact dedicated ccx43 16-core/64-GiB/360-GB local x86')
    if (type(group) is not dict or group.get('id') != group_id
            or group_id not in resources['ids']['placement-group']
            or group_id in resources['protected_ids']['placement-group']
            or group.get('type') != 'spread' or group.get('name') != plan['prefix']+'-spread'
            or (group.get('labels') or {}).get('campaign') != plan['campaign']
            or type(group.get('servers')) is not list
            or any(type(sid) is not int for sid in group['servers'])
            or len(group['servers']) != len(_roles(plan))
            or set(group['servers']) != set(resources['ids']['server'])):
        raise ProvisionError('server lacks exact owned role-scoped physical-spread membership')
    if (server.get('id') != node['id'] or node['id'] in resources['protected_ids']['server']
            or server.get('name') != node['name']
            or (server.get('labels') or {}).get('campaign') != plan['campaign']
            or type(private) is not list or len(private) != 1 or type(private[0]) is not dict
            or private[0].get('network') != network_id
            or network_id not in resources['ids']['network']
            or network_id in resources['protected_ids']['network']
            or private[0].get('ip') != node['private_ip'] or private[0].get('alias_ips') != []):
        raise ProvisionError('server lacks exact owned isolated-network membership')


def _prove_ips(plan, state, resources, runner, clock=time.time, guard=None):
    servers, ips = _capture_ip_snapshots(plan, state, resources, runner, clock)
    mine = [s for s in servers if s['id'] in resources['ids']['server']]
    if len(mine) != len(resources['ids']['server']):
        raise ProvisionError('created server disappeared')
    proofs = []
    for server in mine:
        ip = _verified_ipv4(plan, resources, servers, ips, server)
        rid = ip['id']
        if rid not in resources['ids']['primary-ip']:
            resources['ids']['primary-ip'].append(rid)
            _save_resources(state, resources)
        if ip.get('auto_delete') is False:
            repair = dict(id=rid, server_id=server['id'], server_name=server['name'],
                          ip=ip['ip'], before=False, requested=True, verified=False,
                          epoch=clock(), argv=[plan['hcloud'], 'primary-ip', 'update',
                                              '--auto-delete=true', str(rid)])
            resources.setdefault('primary_ip_repairs', []).append(repair)
            _save_resources(state, resources)
            _mutate(plan, state, repair['argv'], runner, clock, guard)
            repair['finished_epoch'] = clock()
            _save_resources(state, resources)
            fresh_servers, ips = _capture_ip_snapshots(plan, state, resources, runner, clock)
            matches = [s for s in fresh_servers if s['id'] == server['id']]
            if len(matches) != 1:
                raise ProvisionError('IPv4 repair server disappeared before readback')
            ip = _verified_ipv4(plan, resources, fresh_servers, ips, matches[0])
            if ip['id'] != rid or ip.get('auto_delete') is not True:
                raise ProvisionError('new server IPv4 auto_delete repair readback is not true')
            servers = fresh_servers
            repair['verified'] = True
            repair['verified_epoch'] = clock()
            _save_resources(state, resources)
        elif ip.get('auto_delete') is not True:
            raise ProvisionError('new server IPv4 auto_delete is missing or malformed')
        proofs.append(dict(id=rid, server_id=server['id'], auto_delete=True, ip=ip['ip']))
    resources['ids']['primary-ip'] = sorted(p['id'] for p in proofs)
    resources['primary_ip_proof'] = proofs
    _save_resources(state, resources)
    return {s['id']: s for s in mine}


def prepare(plan, out, *, runner=None, popen=None, clock=time.time, sleep=time.sleep):
    """Provision exactly the schema's new servers and install sealed native binaries.

    Does not start FDB, run a workload, or make a health/readiness claim. A failed
    partial preparation immediately invokes cleanup. The auxiliary guard is not
    canceled before exact baseline equality; a separately supervised foreground
    lifetime guard must be armed by the caller before launching the controller,
    except explicitly approved foreground_only scaling with no survival claim.
    """
    validate_plan(plan, out, clock)
    if plan['schema'] == 'otel_native_scaling_v1' and _scaling_mode(plan) == 'managed_watch':
        _admit_managed_watch(plan, out, clock)
    roles = _roles(plan)
    state = _state(out)
    state.mkdir(mode=0o700, parents=True, exist_ok=False)
    baseline_dir = state / 'baseline'
    _inventory.snapshot(SimpleNamespace(hcloud=plan['hcloud'], output_dir=str(baseline_dir)),
                        runner=lambda argv: _inventory_run(plan, runner, argv, clock))
    baseline = _baseline(baseline_dir)
    if plan.get('inventory_dir'):
        original = _baseline(plan['inventory_dir'])
        if any(_inventory.ids(original[k], k) != _inventory.ids(baseline[k], k) for k in KINDS):
            raise ProvisionError('live inventory differs from the approved baseline')
    protected = _preflight(plan, baseline)
    resources = dict(campaign=plan['campaign'], prefix=plan['prefix'], baseline_dir=str(baseline_dir),
                     protected_ids={k: sorted(v) for k, v in protected.items()},
                     ids={k: [] for k in KINDS}, nodes={})
    _save_resources(state, resources)
    # Pricing is a read-only evidence capture, not a quoted-price substitution.
    pricing = _checked(runner, [plan['hcloud'], 'server-type', 'describe', 'ccx43', '-o', 'json'],
                       timeout=_remaining(plan, 20, clock))
    value = json.loads(pricing)
    if (not isinstance(value, dict) or value.get('name') != 'ccx43'
            or not any(p.get('location') == 'hel1' and p.get('price_hourly')
                       for p in value.get('prices', []))):
        raise ProvisionError('live ccx43/hel1 pricing capture is empty or malformed')
    hourly = next(p['price_hourly'] for p in value['prices'] if p.get('location') == 'hel1')
    try:
        if not all(isinstance(hourly.get(k), str) for k in ('net', 'gross')):
            raise ValueError('catalog prices must be decimal strings')
        net, gross = Decimal(hourly['net']), Decimal(hourly['gross'])
        if not net.is_finite() or not gross.is_finite() or net <= 0 or gross < net:
            raise ValueError('invalid hourly price')
    except (KeyError, TypeError, ValueError, InvalidOperation) as exc:
        raise ProvisionError('invalid live hourly price') from exc
    pricing_receipt = state / 'live-pricing.json'
    _json(pricing_receipt, dict(captured_epoch=clock(), read_only=True, server_type=value,
                               currency=plan.get('pricing_currency'), vat_rate=plan.get('pricing_vat_rate'),
                               metadata_source=plan.get('pricing_metadata_source')))
    resources.update(price_hourly_net=hourly['net'], price_hourly_gross=hourly['gross'],
                     price_currency=plan.get('pricing_currency'), price_vat_rate=plan.get('pricing_vat_rate'),
                     currency=plan.get('pricing_currency'), vat_rate=plan.get('pricing_vat_rate'),
                     pricing_receipt_sha256=_sha(pricing_receipt))
    guard = _arm(plan, out, state, popen, clock, sleep)
    resources['guard_pid'] = guard.pid
    resources['guard_ready'] = str(state / 'guard-ready.json')
    resources['pricing_receipt'] = str(state / 'live-pricing.json')
    _save_resources(state, resources)
    try:
        hosts = [f'{plan["prefix"]}-{role}' for role in roles]
        _cloud_init.generate_keys(state / 'host-keys', hosts,
                                   runner=lambda argv: _call(runner, argv, timeout=_remaining(plan, 20, clock, active=True)))
        cloud_dir = state / 'cloud-init'
        _cloud_init.render(state / 'host-keys', plan['controller_public_key'], hosts, cloud_dir)
        for host in hosts:
            path = cloud_dir / f'{host}.user-data'
            text = path.read_text().replace('package_update: false', 'package_update: true')
            # Cloud-init 25.2 requires a nonempty list; provided ssh_keys still
            # selects its key-install branch rather than generating replacement keys.
            if text.count('ssh_genkeytypes: []\n') != 1:
                raise ProvisionError('unexpected shared cloud-init key-generation field')
            text = text.replace('ssh_genkeytypes: []\n', 'ssh_genkeytypes: [ed25519]\n')
            text = text.replace('package_upgrade: false',
                                'package_upgrade: false\npackages: [python3, chrony, jq]')
            _mc.write_private(path, text, exclusive=False)
        labels = ['--label', 'campaign=' + plan['campaign'], '--label',
                  'deadline=' + str(plan['deadline_epoch'])]
        h = plan['hcloud']
        net = _create(plan, state, resources, 'network',
                      [h, 'network', 'create', '--name', plan['prefix'] + '-net',
                       '--ip-range', str(_network(plan)), *labels], runner, clock, guard)
        _mutate(plan, state, [h, 'network', 'add-subnet', str(net['id']), '--network-zone',
                             'eu-central', '--type', 'cloud', '--ip-range', str(_network(plan))],
                runner, clock, guard)
        group = _create(plan, state, resources, 'placement-group',
                        [h, 'placement-group', 'create', '--name', plan['prefix'] + '-spread',
                         '--type', 'spread', *labels], runner, clock, guard)
        if group.get('type') != 'spread':
            raise ProvisionError('created placement group is not physical spread')
        rules = state / 'firewall-rules.json'
        _json(rules, [dict(direction='in', protocol='tcp', port='22',
                           source_ips=[plan['controller_cidr']], description='controller SSH only'),
                      dict(direction='in', protocol='icmp', source_ips=[plan['controller_cidr']])])
        firewall = _create(plan, state, resources, 'firewall',
                           [h, 'firewall', 'create', '--name', plan['prefix'] + '-fw',
                            '--rules-file', str(rules), *labels], runner, clock, guard)
        key = _create(plan, state, resources, 'ssh-key',
                      [h, 'ssh-key', 'create', '--name', plan['prefix'] + '-key',
                       '--public-key-from-file', plan['controller_public_key'], *labels], runner, clock, guard)
        for role in roles:
            name = f'{plan["prefix"]}-{role}'
            offset = 10+int(role[4:]) if role.startswith('node') else 20+int(role[6:])
            private = str(_network(plan).network_address + offset)
            server = _create(plan, state, resources, 'server',
                             [h, 'server', 'create', '--name', name, '--type', 'ccx43',
                              '--location', 'hel1', '--image', 'ubuntu-24.04', '--ssh-key', str(key['id']),
                              '--firewall', str(firewall['id']), '--placement-group', str(group['id']),
                              '--without-ipv6', '--start-after-create=false', '--user-data-from-file',
                              str(cloud_dir / f'{name}.user-data'), *labels, '--label', 'role=' + role],
                             runner, clock, guard)
            current = _prove_ips(plan, state, resources, runner, clock, guard)[server['id']]
            current = _attach_network(plan, state, resources, current, net, private, runner, clock, guard, sleep)
            _mutate(plan, state, [h, 'server', 'poweron', str(server['id'])], runner, clock, guard)
            node = dict(id=server['id'], name=name, role=role, created=current.get('created'),
                        host=current['public_net']['ipv4']['ip'], public_ipv4=current['public_net']['ipv4']['ip'],
                        private_ip=private, known_hosts=str(cloud_dir / 'known_hosts'),
                        private_key=plan['controller_private_key'])
            node['ssh'] = ssh_argv(plan, out, node, [])[:-1]
            node['scp_options'] = _ssh_options(plan, out, node)
            resources['nodes'][role] = node
            _save_resources(state, resources)
        final_servers, final_ips = _capture_ip_snapshots(plan, state, resources, runner, clock)
        fresh = {s['id']: s for s in final_servers}
        members = [s['id'] for s in final_servers
                   if (s.get('labels') or {}).get('campaign') == plan['campaign']
                   or _mc.owned_name(s.get('name', ''), plan['prefix'])]
        if len(members) != len(roles) or set(members) != set(resources['ids']['server']):
            raise ProvisionError('final provider campaign membership does not match its exact owned roles')
        for node in resources['nodes'].values():
            server = fresh[node['id']]
            _verified_ipv4(plan, resources, final_servers, final_ips, server)
            _verify_provider_server(plan, resources, node, server, net['id'], group['id'])
        _json(state / 'placement-proof.json', dict(type='spread', id=group['id'],
                                                   server_ids=resources['ids']['server']))
        remote = REMOTE_BASE + '/' + plan['campaign']
        resources['remote_root'] = remote
        for kind in ('fdb', 'client'):
            _mc.write_private(state / ('horizontal-' + kind + '.slice'),
                              _slice(kind, io_accounting=plan['schema'] in ('otel_native_diagnostic_v1', 'otel_native_session_v1', 'otel_native_scaling_v1')))
        for role, node in resources['nodes'].items():
            until = min(clock() + 600, _active_deadline(plan))
            attempt = 0
            readiness_argv = ssh_argv(plan, out, node, ['cloud-init', 'status', '--wait'])
            while True:
                _check_window(plan, state, clock, guard)
                if clock() >= until:
                    raise ProvisionError('cloud-init did not finish within preparation budget')
                attempt += 1
                started = clock()
                timeout = _remaining(plan, min(30, max(1, until-clock())), clock, active=True)
                timed_out = False
                try:
                    ready = _call(runner, readiness_argv, timeout=timeout)
                except subprocess.TimeoutExpired as exc:
                    timed_out = True
                    ready = (None, exc.stdout or '', exc.stderr or '')
                except OSError as exc:
                    ready = (None, '', str(exc))
                rc, stdout, stderr = ready
                stdout = stdout.decode('utf-8', errors='replace') if isinstance(stdout, bytes) else stdout
                stderr = stderr.decode('utf-8', errors='replace') if isinstance(stderr, bytes) else stderr
                _json(state / f'{role}-readiness-{attempt}.json',
                      dict(node_id=node['id'], argv=readiness_argv, returncode=rc, timed_out=timed_out,
                           stdout=stdout, stderr=stderr, timeout_seconds=timeout,
                           epoch=started, finished_epoch=clock()))
                if not timed_out and type(rc) is int and rc == 0:
                    break
                if not timed_out and (type(rc) is not int or rc != 255):
                    raise ProvisionError(f'cloud-init readiness failed ({rc}): stdout={stdout!r}; stderr={stderr!r}')
                if clock() >= until:
                    raise ProvisionError('cloud-init did not finish within preparation budget')
                sleep(2)
            _require_result(run_on_node(plan, out, node, ['install', '-d', '-m', '0755', remote + '/bin'],
                                        runner=runner), 'create native binary directory')
            for asset, dest in [('binary', 'otel-demo'), ('fdbserver', 'fdbserver'), ('fdbcli', 'fdbcli')]:
                _require_result(copy_to_node(plan, out, node, plan[asset], remote + '/bin/' + dest,
                                             runner=runner), 'copy sealed ' + asset)
            output = _require_result(run_on_node(plan, out, node,
                                     ['sh', '-c', _host_prepare_script(plan)], runner=runner), 'native host prepare')
            lines = output.strip().splitlines()
            receipt = json.loads(lines[-1]) if lines else {}
            if (receipt.get('filesystem') != 'ext4' or receipt.get('native') is not True
                    or receipt.get('floor_bytes') != disk_floor(receipt.get('total_bytes', 0))
                    or receipt.get('free_bytes', -1) < receipt['floor_bytes']
                    or receipt.get('fsync_preserved') is not True or receipt.get('checksums_preserved') is not True):
                raise ProvisionError('native disk/fsync/checksum preparation receipt failed')
            _json(state / f'{role}-native.json', receipt)
            kind = 'fdb' if role.startswith('node') else 'client'
            slice_name = 'horizontal-' + kind + '.slice'
            _require_result(copy_to_node(plan, out, node, state / slice_name,
                                         '/etc/systemd/system/' + slice_name, runner=runner), 'install active resource slice')
            _require_result(run_on_node(plan, out, node, ['systemctl', 'daemon-reload'],
                                        runner=runner), 'reload resource slice')
            _require_result(run_on_node(plan, out, node, ['systemctl', 'start', slice_name],
                                        runner=runner), 'activate resource slice before quiet admission')
            node['slice'] = slice_name
        resources['prepared'] = True
        _save_resources(state, resources)
        return resources
    except BaseException as exc:
        try:
            cleanup = teardown(plan, out, resources, runner=runner, clock=clock, sleep=sleep)
        except BaseException as cleanup_exc:
            raise ProvisionError(f'prepare failed: {exc}; cleanup failed: {cleanup_exc}; guard remains armed') from exc
        if not cleanup['passed']:
            raise ProvisionError(f'prepare failed: {exc}; cleanup not verified; guard remains armed') from exc
        raise


def _cleanup_owned(plan, baseline, kind, items):
    """Recover an ambiguous create only by new ID + exact expected name + label."""
    old_ids = set(_inventory.ids(baseline[kind], kind))
    old_names = {item.get('name') for values in baseline.values() for item in values if item.get('name')}
    args = SimpleNamespace(campaign=plan['campaign'], prefix=plan['prefix'],
                           protect_id=list(old_ids), protect_name=[])
    selected = [i for i in items if (i.get('labels') or {}).get('campaign') == plan['campaign']]
    owned, refused = _teardown_guard.classify(args, kind, selected)
    allowed = _names(plan)[kind]
    result = []
    for entry in owned:
        if entry['name'] not in allowed or entry['name'] in old_names:
            refused.append(dict(entry, reasons=['not an exact authorized new resource name']))
        else:
            result.append(entry)
    return result, refused


def teardown(plan, out, resources=None, *, runner=None, clock=time.time, sleep=time.sleep):
    """Immediately delete new owned IDs; verify exact equality across all ten kinds.

    No SSH, no modifications to a protected resource, and no direct IP deletion.
    Unknown/malformed listings fail closed. Failed cleanup remains guard-owned.
    """
    state = _state(out)
    _mc.require_outside_repo(state, 'campaign state')
    baseline = _baseline(state / 'baseline')
    if not (state / 'cleanup-started.json').exists():
        try:
            _json(state / 'cleanup-started.json', dict(epoch=clock(), deadline_epoch=plan['deadline_epoch']))
        except (FileExistsError, ProvisionError):
            if not (state / 'cleanup-started.json').exists():
                raise
    receipt = dict(passed=False, actions=[], refused=[], errors=[], deadline_epoch=plan['deadline_epoch'])
    with _lock(state):
        # Re-discover on each pass: a timed-out create may have reached the API.
        for attempt in range(4):
            remaining = 0
            for kind in DELETE_KINDS:
                try:
                    owned, refused = _cleanup_owned(plan, baseline, kind, _list(plan, kind, runner))
                    receipt['refused'] += refused
                    remaining += len(owned)
                    for entry in owned:
                        if clock() >= plan['deadline_epoch'] - 105:
                            raise ProvisionError('cleanup deletion budget exhausted; reserved final inventory window')
                        argv = [plan['hcloud'], kind, 'delete', str(entry['id'])]
                        try:
                            _command_receipt(plan, state, argv, clock)
                            started = clock()
                            rc, stdout, stderr = _call(runner, argv, timeout=_remaining(plan, 10, clock))
                            receipt['actions'].append(dict(kind=kind, id=entry['id'], returncode=rc,
                                                           stdout=stdout, stderr=stderr, attempt=attempt,
                                                           epoch=started, finished_epoch=clock()))
                        except (OSError, subprocess.SubprocessError) as exc:
                            receipt['errors'].append(str(exc))
                except (OSError, ValueError, _mc.GuardError, ProvisionError) as exc:
                    receipt['errors'].append(f'{kind}: {exc}')
            if not remaining:
                break
            if clock() >= plan['deadline_epoch'] - 110:
                break
            sleep(2)
        verify_dir = state / f'verify-{time.time_ns()}'
        args = SimpleNamespace(inventory=str(state / 'baseline'), output_dir=str(verify_dir), hcloud=plan['hcloud'])
        try:
            rc = _inventory.verify(args, runner=lambda argv: _inventory_run(plan, runner, argv, clock))
            summary = _load(verify_dir / 'summary.json')
            receipt['inventory'] = summary
            receipt['inventory_dir'] = str(verify_dir)
            receipt['passed'] = rc == 0 and not receipt['refused']
        except (OSError, ValueError, _mc.GuardError, ProvisionError, subprocess.SubprocessError) as exc:
            receipt['errors'].append('inventory verification: ' + str(exc))
        receipt['finished_epoch'] = clock()
        receipt['within_deadline'] = receipt['finished_epoch'] <= plan['deadline_epoch']
        receipt['passed'] = receipt['passed'] and receipt['within_deadline']
        receipt['receipt_path'] = str(state / f'teardown-{time.time_ns()}.json')
        _json(receipt['receipt_path'], receipt)
        if receipt['passed'] and not (state / 'teardown-complete.json').exists():
            _json(state / 'teardown-complete.json', receipt)
    return receipt


def _guard(plan_path, out, *, runner=None, clock=time.time, sleep=time.sleep):
    plan = _load(plan_path)
    state = _state(out)
    baseline = _baseline(state / 'baseline')
    _preflight(plan, baseline)
    first, deadline = plan['first_epoch'], plan['deadline_epoch']
    if not 600 < deadline-first <= 7200:
        raise ProvisionError('guard lifetime exceeds authorization')
    _json(state / 'guard-ready.json', dict(pid=os.getpid(), plan_sha256=_sha(plan_path),
                                          cleanup_epoch=deadline-600, deadline_epoch=deadline))
    while clock() < deadline-600 and not (state / 'cleanup-started.json').exists():
        if (state / 'teardown-complete.json').exists():
            return 0
        sleep(min(1, max(0, deadline-600-clock())))
    while clock() < deadline:
        if (state / 'teardown-complete.json').exists():
            return 0
        try:
            if teardown(plan, out, runner=runner, clock=clock, sleep=sleep)['passed']:
                return 0
        except Exception as exc:
            print('guard cleanup failed; retrying: ' + str(exc), file=sys.stderr, flush=True)
        sleep(min(2, max(0, deadline-clock())))
    return 1


def render_cluster(plan, nodes, storage_hosts, armid):
    """Pure native R1 layout: fixed control placement and schema-scoped storage hosts."""
    roles = _roles(plan)
    max_outstanding = _storage_max_outstanding(plan)
    storage_count = _storage_process_count(plan)
    memory_profile = _storage_memory_profile(plan)
    _validate_scaling(plan)
    scaling = plan['schema'] == 'otel_native_scaling_v1'
    if plan['schema'] == 'otel_native_session_v1':
        raise ProvisionError('session clusters must use an explicitly validated diagnostic case plan')
    storage_memory_gib = 4 if memory_profile == 'capacity16g' or storage_count == 1 else 2
    storage_cache_mib = 2048 if memory_profile == 'capacity16g' or storage_count == 1 else 1024
    if scaling:
        storage_memory_gib = 3
    fdb_roles = tuple(role for role in roles if role.startswith('node'))
    if plan['schema'] == 'otel_native_diagnostic_v1' and (type(nodes) is not dict or set(nodes) != set(roles)):
        raise ProvisionError('diagnostic layout requires exactly node1 and client1')
    if scaling and (type(nodes) is not dict or set(nodes) != set(roles)):
        raise ProvisionError('scaling layout requires exactly all six prepared roles')
    if not re.fullmatch(r'[A-Za-z0-9_-]{1,40}', str(armid)):
        raise ProvisionError('unsafe arm ID')
    if type(storage_hosts) is int:
        selected = list(fdb_roles[:storage_hosts]) if 1 <= storage_hosts <= len(fdb_roles) else []
    else:
        try:
            selected = list(storage_hosts)
        except TypeError as exc:
            raise ProvisionError('storage hosts must name the schema-scoped FDB hosts') from exc
    if selected not in [list(fdb_roles[:n]) for n in range(1, len(fdb_roles)+1)]:
        raise ProvisionError('storage hosts must be an allowed prefix of the schema-scoped FDB hosts')
    if scaling and (type(armid) is not str or armid != 'scaling-' + str(len(selected))):
        raise ProvisionError('scaling stage arm ID must match its active host prefix')
    root = REMOTE_BASE + '/' + plan['campaign'] + '/arms/' + str(armid)
    cluster = 'horizontal_' + str(armid).replace('-', '_') + ':' + hashlib.sha256(
        (plan['campaign'] + ':' + str(armid)).encode()).hexdigest()[:24] + '@' + nodes['node1']['private_ip'] + ':4500'
    classes = [(4500, 'stateless'), (4501, 'commit_proxy'), (4502, 'grv_proxy'),
               (4503, 'resolution'), (4504, 'transaction')]
    processes = []
    for node in fdb_roles:
        specs = (classes if node == 'node1' else []) + (
            [(4510+i, 'storage') for i in range(storage_count)] if node in selected else [])
        for port, cls in specs:
            storage = cls == 'storage'
            address = f'{nodes[node]["private_ip"]}:{port}'
            name = f'oh-{plan["prefix"]}-{armid}-{port}'
            args = [REMOTE_BASE + '/' + plan['campaign'] + '/bin/fdbserver',
                    '--cluster-file', root + '/fdb.cluster', '--public-address', address,
                    '--listen-address', address, '--class', cls, '--datadir', f'{root}/data/{port}',
                    '--logdir', root + '/logs', '--locality-machineid', node,
                    '--locality-zoneid', node, '--memory', f'{storage_memory_gib}GiB' if storage else '1GiB',
                    '--cache-memory', f'{storage_cache_mib//1024}GiB' if storage else '64MiB', '--trace-format', 'json',
                    '--maxlogssize', '128MiB', '--loggroup', plan['campaign']]
            if storage and max_outstanding == 128:
                args += ['--knob_max_outstanding', '128']
            unit = f'''[Unit]
Description=Native horizontal campaign {plan['campaign']} {armid} {node} {cls}
After=network-online.target
[Service]
Type=simple
User=foundationdb
Group=foundationdb
Slice=horizontal-fdb.slice
LimitNOFILE=100000
ExecStart={shlex.join(args)}
Restart=no
'''
            required = {'stateless': ['master', 'cluster_controller'], 'commit_proxy': ['commit_proxy'],
                        'grv_proxy': ['grv_proxy'], 'resolution': ['resolver'],
                        'transaction': ['log'], 'storage': ['storage']}[cls]
            processes.append(dict(node=node, address=address, port=port, process_class=cls,
                                  args=args, unit_name=name + '.service', unit=unit,
                                  memory_gib=storage_memory_gib if storage else 1, cache_mib=storage_cache_mib if storage else 64,
                                  memory_limit_bytes=(storage_memory_gib if storage else 1) * GIB,
                                  machineid=node, zoneid=node, required_roles=required))
    layout = dict(root=root, cluster_text=cluster + '\n', remote_cluster_file=root + '/fdb.cluster',
                storage_hosts=selected, processes=processes, configure=CONFIGURE,
                budgets=dict(fdb_host=dict(cpu_quota_percent=400, allowed_cpus='0-3', memory_max_gib=16,
                                           max_process_memory_gib=5+storage_count*storage_memory_gib, max_processes=5+storage_count),
                             client_host=dict(cpu_quota_percent=400, allowed_cpus='0-3', memory_max_gib=16)),
                storage_engine='ssd-redwood-1', log_engine='ssd-2', redundancy_mode='single')
    if scaling:
        layout['schema'] = 'otel_native_scaling_v1'
    if plan['schema'] in ('otel_native_diagnostic_v1', 'otel_native_scaling_v1'):
        layout['storage_max_outstanding'] = max_outstanding
        layout['storage_process_count'] = storage_count
        layout['storage_memory_profile'] = memory_profile
    return layout


def _slice(kind, *, io_accounting=False):
    accounting = 'IOAccounting=yes\n' if io_accounting else ''
    return f'''[Unit]
Description=Horizontal campaign {kind} resource boundary
[Slice]
CPUAccounting=yes
MemoryAccounting=yes
{accounting}AllowedCPUs=0-3
CPUQuota=400%
MemoryMax=16G
MemorySwapMax=0
'''


_STOP_PROBE = r'''import json, os, pathlib, re, subprocess, sys
request = json.loads(sys.argv[1])
def proc(pid):
    root = pathlib.Path('/proc') / str(pid)
    try:
        stat = (root / 'stat').read_text()
        fields = stat[stat.rfind(') ') + 2:].split()
        raw = (root / 'cmdline').read_bytes()
        if len(raw) > 65536: raise RuntimeError('process command exceeds bound')
        args = raw.rstrip(b'\0').decode().split('\0') if raw else []
        groups = (root / 'cgroup').read_text().splitlines()
        unified = [line[3:] for line in groups if line.startswith('0::')]
        if len(unified) != 1: raise RuntimeError('process unified cgroup missing')
        return dict(pid=pid, starttime_ticks=int(fields[19]), args=args,
                    exe=os.readlink(root / 'exe'), cgroup=unified[0])
    except (FileNotFoundError, ProcessLookupError):
        return None
units = []
for item in request['units']:
    done = subprocess.run(['systemctl', 'show', item['unit'], '--property=LoadState',
        '--property=ActiveState', '--property=MainPID', '--property=ControlGroup'],
        capture_output=True, text=True, timeout=5)
    props = dict(line.split('=', 1) for line in done.stdout.splitlines() if '=' in line)
    if done.returncode and props.get('LoadState') != 'not-found':
        raise RuntimeError('cannot read exact owned unit: ' + done.stderr)
    cg = pathlib.Path('/sys/fs/cgroup' + item['cgroup'])
    if not cg.resolve().is_relative_to('/sys/fs/cgroup'): raise RuntimeError('cgroup escaped')
    pids = set()
    if cg.exists():
        for file in [cg / 'cgroup.procs', *cg.rglob('*/cgroup.procs')]:
            if file.exists():
                for value in file.read_text().split():
                    if not re.fullmatch('[0-9]+', value): raise RuntimeError('invalid cgroup PID')
                    pids.add(int(value))
    main = int(props.get('MainPID', '0'))
    units.append(dict(unit=item['unit'], load_state=props.get('LoadState'),
        active_state=props.get('ActiveState'), control_group=props.get('ControlGroup', ''),
        main_pid=main, main=proc(main) if main else None,
        pids=[value for pid in sorted(pids) if (value := proc(pid)) is not None]))
historical = {str(pid): proc(pid) for pid in request['historical']}
residual = []
for entry in pathlib.Path('/proc').iterdir():
    if entry.name.isdigit():
        value = proc(int(entry.name))
        if value and (value['exe'].removesuffix(' (deleted)') in request['binaries']
                      or value['args'] and value['args'][0] in request['binaries']):
            residual.append(value)
print(json.dumps(dict(units=units, historical=historical, owned_residual=residual)))
'''


def _stop_specs(plan, nodes, layout, active, scopes):
    scaling = plan['schema'] == 'otel_native_scaling_v1'
    roles = _roles(plan)
    if (plan['schema'] not in ('otel_native_diagnostic_v1', 'otel_native_session_v1', 'otel_native_scaling_v1')
            or type(nodes) is not dict or set(nodes) != set(roles)):
        raise ProvisionError('owned-arm stop requires exactly the schema-scoped native nodes')
    armid = active.get('armid')
    if type(armid) is not str or not re.fullmatch(r'[A-Za-z0-9_-]{1,40}', armid):
        raise ProvisionError('invalid active arm ID')
    case = dict(plan)
    if plan['schema'] == 'otel_native_session_v1':
        profile = layout.get('storage_memory_profile')
        if profile not in plan.get('cases', []):
            raise ProvisionError('active arm is not an approved session case')
        case.update(schema='otel_native_diagnostic_v1', storage_memory_profile=profile)
    hosts = layout.get('storage_hosts') if scaling else 1
    expected = render_cluster(case, nodes, hosts, armid)
    if layout.get('root') != expected['root'] or layout.get('processes') != expected['processes']:
        raise ProvisionError('active layout root or exact native process specifications changed')
    if scaling and any(layout.get(key) != value for key, value in expected.items()):
        raise ProvisionError('scaling layout differs from its exact stage specification')
    units = ({role: [item['unit_name'] for item in expected['processes'] if item['node'] == role]
              for role in roles} if scaling else {'node1': [item['unit_name'] for item in expected['processes']]})
    if active != dict(campaign=plan['campaign'], armid=armid, units=units):
        raise ProvisionError('active-arm journal does not exactly match the owned layout')
    specs = {role: [] for role in roles}
    for item in expected['processes']:
        specs[item['node']].append(dict(unit=item['unit_name'], args=item['args'],
            cgroup='/horizontal.slice/horizontal-fdb.slice/'+item['unit_name']))
    if type(scopes) not in (tuple, list):
        raise ProvisionError('owned producer scopes must be an explicit sequence')
    clients = {'client' + str(n) for n in range(1, len(expected['storage_hosts'])+1)} if scaling else {'client1'}
    seen = set()
    for item in scopes:
        if type(item) is not dict or item.get('node') not in clients:
            raise ProvisionError('producer scope is not on the exact owned client')
        unit, args = item.get('unit'), item.get('args')
        match = re.fullmatch(r'otel-native-(prof20k[0-9]{4})-(ingest|audit|query)\.scope', unit) if type(unit) is str else None
        identity = (item['node'], unit)
        if not match or identity in seen or type(args) is not list or not args or not all(type(x) is str for x in args):
            raise ProvisionError('invalid, duplicate or unbound producer scope')
        def value(flag):
            if args.count(flag) != 1 or args.index(flag)+1 >= len(args):
                raise ProvisionError('missing or duplicate producer scope argument: '+flag)
            return args[args.index(flag)+1]
        cg = '/horizontal.slice/horizontal-client.slice/'+unit
        root = REMOTE_BASE+'/'+plan['campaign']
        output = value('--output')
        if (item.get('cgroup') != cg or args[0] != root+'/bin/otel-demo'
                or value('--cluster-file') != expected['remote_cluster_file']
                or value('--run') != match[1] or value('--mode') != match[2]
                or value('--timeout') not in (('120s', '300s') if match[2] == 'ingest' else ('120s',))
                or not output.startswith(root+'/producers/') or '..' in Path(output).parts):
            raise ProvisionError('producer scope path, run, mode or cgroup is not owned')
        if ('pid' in item) != ('starttime_ticks' in item) or any(
                type(item[key]) is not int or item[key] <= 0 for key in ('pid', 'starttime_ticks') if key in item):
            raise ProvisionError('invalid historical producer identity')
        seen.add(identity)
        specs[item['node']].append(dict(item, cgroup=cg))
    return specs


def _validate_stop_snapshot(snapshot, specs, historical, *, stopped=False):
    if type(snapshot) is not dict or type(snapshot.get('units')) is not list or type(snapshot.get('historical')) is not dict:
        raise ProvisionError('malformed owned-arm process snapshot')
    expected = {item['unit']: item for item in specs}
    if len(snapshot['units']) != len(expected) or {item.get('unit') for item in snapshot['units']} != set(expected):
        raise ProvisionError('owned-arm snapshot has missing or foreign units')
    live, quiet = {}, True
    def verify(value, spec):
        if (type(value) is not dict or type(value.get('pid')) is not int or value['pid'] <= 0
                or type(value.get('starttime_ticks')) is not int or value['starttime_ticks'] <= 0
                or value.get('args') != spec['args'] or value.get('exe') != spec['args'][0]
                or value.get('cgroup') != spec['cgroup']):
            raise ProvisionError('live PID/start/full command/cgroup does not match its owned unit')
        if value['pid'] in live and live[value['pid']] != value:
            raise ProvisionError('PID identity changed within its snapshot')
        live[value['pid']] = value
    for unit in snapshot['units']:
        spec = expected[unit['unit']]
        if (unit.get('load_state') not in ('loaded', 'not-found')
                or unit.get('active_state') not in ('active', 'activating', 'deactivating', 'inactive', 'failed')
                or unit.get('control_group') not in ('', spec['cgroup'])
                or type(unit.get('main_pid')) is not int or unit['main_pid'] < 0
                or type(unit.get('pids')) is not list):
            raise ProvisionError('owned unit/cgroup state is unknown or foreign')
        if bool(unit['main_pid']) != (unit.get('main') is not None):
            raise ProvisionError('MainPID is not bound to a complete process snapshot')
        if (unit['pids'] or unit.get('main') is not None) and unit['control_group'] != spec['cgroup']:
            raise ProvisionError('live owned unit has an unknown control group')
        for value in unit['pids']:
            verify(value, spec)
        if unit.get('main') is not None:
            verify(unit['main'], spec)
            if unit['main_pid'] != unit['main']['pid']:
                raise ProvisionError('MainPID does not match its process snapshot')
        if stopped and (unit['pids'] or unit.get('main') is not None or unit['main_pid']
                        or unit['active_state'] not in ('inactive', 'failed')):
            quiet = False
    if set(snapshot['historical']) != {str(pid) for pid in historical}:
        raise ProvisionError('historical PID proof population changed')
    for raw_pid, spec in historical.items():
        value = snapshot['historical'][str(raw_pid)]
        if value is not None:
            if type(value) is not dict or type(value.get('starttime_ticks')) is not int:
                raise ProvisionError('malformed historical PID snapshot')
            if value['starttime_ticks'] != spec['starttime_ticks']:
                if raw_pid in live:
                    raise ProvisionError('live owned unit PID was reused or changed identity')
                continue  # A different start time is a different process, never ours to stop.
            verify(value, spec)
            if stopped:
                quiet = False
    residual = snapshot.get('owned_residual')
    if type(residual) is not list or any(type(x) is not dict or x.get('pid') not in live
                                       or x != live[x['pid']] for x in residual):
        raise ProvisionError('unmapped owned FDB/application residual process')
    if len(residual) != len(live) or {x['pid'] for x in residual} != set(live):
        raise ProvisionError('owned process snapshot and residual census disagree')
    return quiet and not live if stopped else True, live


def stop_owned_arm(plan, out, nodes, layout, *, scopes=(), runner=None, clock=time.time, sleep=time.sleep):
    """Stop only an exactly journaled native arm; retain all data and the original lease."""
    state = _state(out)
    receipt = dict(passed=False, started_epoch=clock(), root=layout.get('root'), before={}, after={}, actions=[],
                   boundary='owned process/unit quiescence only; host quiet admission and disk floors remain mandatory')
    path = state / f'stop-arm-{time.time_ns()}.json'
    receipt['receipt_path'] = str(path)
    try:
        with _lock(state):
            _check_window(plan, state, clock)
            for role, node in nodes.items():
                if plan['schema'] == 'otel_native_scaling_v1' and node.get('role') != role:
                    raise ProvisionError('scaling role does not match its prepared node')
                _authorized_node(plan, out, node)
            active = _load(state / 'active-arm.json')
            specs = _stop_specs(plan, nodes, layout, active, scopes)
            receipt['armid'] = active['armid']
            historical = {role: {item['pid']: item for item in values if 'pid' in item} for role, values in specs.items()}
            until = min(clock()+120, _active_deadline(plan))
            def probe(role, phase):
                _check_window(plan, state, clock)
                if clock() >= until:
                    raise ProvisionError('owned-arm quiescence proof exceeded its operation budget')
                root = REMOTE_BASE+'/'+plan['campaign']
                request = dict(units=specs[role], historical=list(historical[role]),
                               binaries=[root+'/bin/fdbserver', root+'/bin/otel-demo'])
                timed_out = False
                try:
                    rc, stdout, stderr = run_on_node(plan, out, nodes[role],
                        ['python3', '-c', _STOP_PROBE, json.dumps(request)], runner=runner,
                        timeout=min(20, until-clock()))
                except subprocess.TimeoutExpired as exc:
                    rc, stdout, stderr, timed_out = None, exc.stdout or '', exc.stderr or '', True
                stdout = stdout.decode(errors='replace') if isinstance(stdout, bytes) else stdout
                stderr = stderr.decode(errors='replace') if isinstance(stderr, bytes) else stderr
                receipt[phase][role] = dict(returncode=rc, stdout=stdout, stderr=stderr, timed_out=timed_out, epoch=clock())
                _json(path, receipt, replace=True)
                if type(rc) is not int or rc != 0:
                    raise ProvisionError('owned-arm identity probe failed: '+stderr)
                return json.loads(stdout)
            for role in specs:
                value = probe(role, 'before')
                _, live = _validate_stop_snapshot(value, specs[role], historical[role])
                for pid, process in live.items():
                    spec = next(item for item in specs[role] if item['cgroup'] == process['cgroup'])
                    historical[role][pid] = dict(spec, pid=pid, starttime_ticks=process['starttime_ticks'])
            for role in specs:
                value = json.loads(receipt['before'][role]['stdout'])
                units = [item['unit'] for item in value['units'] if item['load_state'] != 'not-found']
                if units:
                    _check_window(plan, state, clock)
                    if plan['schema'] == 'otel_native_scaling_v1' and clock() >= until:
                        raise ProvisionError('owned-arm quiescence proof exceeded its operation budget')
                    rc, stdout, stderr = run_on_node(plan, out, nodes[role], ['systemctl', 'stop', *units],
                                                    runner=runner, timeout=min(20, max(0.001, until-clock())))
                    receipt['actions'].append(dict(node=role, units=units, returncode=rc, stdout=stdout, stderr=stderr))
                    _json(path, receipt, replace=True)
                    if rc:
                        raise ProvisionError('failed to stop exact owned units: '+stderr)
            while True:
                quiet = True
                for role in specs:
                    value = probe(role, 'after')
                    passed, _ = _validate_stop_snapshot(value, specs[role], historical[role], stopped=True)
                    quiet &= passed
                if quiet:
                    break
                sleep(min(1, max(0, until-clock())))
            if _load(state / 'active-arm.json') != active:
                raise ProvisionError('active arm changed during its quiescence proof')
            receipt.update(passed=True, quiescent=True, finished_epoch=clock())
            _json(path, receipt, replace=True)
            return receipt
    except BaseException as exc:
        receipt.update(error=str(exc), finished_epoch=clock())
        _json(path, receipt, replace=True)
        if not (state / 'cleanup-started.json').exists():
            try:
                _json(state / 'cleanup-started.json', dict(epoch=clock(), reason='owned-arm stop proof failed'))
            except (FileExistsError, ProvisionError):
                if not (state / 'cleanup-started.json').exists():
                    raise
        raise ProvisionError('owned-arm stop proof failed; hard teardown required; receipt='+str(path)+': '+str(exc)) from exc


def setup_cluster(plan, out, nodes, storage_hosts, armid, *, runner=None, clock=time.time, sleep=time.sleep):
    """Start a fresh native R1 arm, stopping only units in this campaign's journal.

    Status/roles, memory quiet, kernel warnings, queue/lag and readiness checks
    belong to the caller. No previous data directory is reused or deleted.
    """
    state = _state(out)
    _check_window(plan, state, clock)
    roles = _roles(plan)
    fdb_roles = tuple(role for role in roles if role.startswith('node'))
    if type(nodes) is not dict or set(nodes) != set(roles):
        raise ProvisionError('exactly the schema-scoped prepared campaign nodes are required')
    for role, node in nodes.items():
        if plan['schema'] == 'otel_native_scaling_v1' and node.get('role') != role:
            raise ProvisionError('scaling role does not match its prepared node')
        _authorized_node(plan, out, node)
    layout = render_cluster(plan, nodes, storage_hosts, armid)
    arm = state / 'arms' / str(armid)
    arm.mkdir(mode=0o700, parents=True, exist_ok=False)
    active_path = state / 'active-arm.json'
    if active_path.exists():
        previous = _load(active_path)
        if previous.get('campaign') != plan['campaign']:
            raise ProvisionError('previous arm is not campaign-owned')
        if plan['schema'] == 'otel_native_scaling_v1':
            prior_id = previous.get('armid')
            if prior_id not in ('scaling-1', 'scaling-2', 'scaling-3'):
                raise ProvisionError('previous scaling stage arm ID is not owned')
            prior = render_cluster(plan, nodes, int(prior_id[-1]), prior_id)
            stop_owned_arm(plan, out, nodes, prior, scopes=(), runner=runner, clock=clock, sleep=sleep)
        else:
            for node in fdb_roles:
                units = previous.get('units', {}).get(node, [])
                if any(not re.fullmatch(r'oh-' + re.escape(plan['prefix']) + r'-[A-Za-z0-9_-]+-\d+\.service', u)
                       for u in units):
                    raise ProvisionError('previous arm has an unowned unit name')
                if units:
                    _require_result(run_on_node(plan, out, nodes[node], ['systemctl', 'stop', *units],
                                                runner=runner), 'stop previous campaign arm')
    _mc.write_private(arm / 'fdb.cluster', layout['cluster_text'])
    layout['cluster_file'] = str(arm / 'fdb.cluster')
    native_bin = REMOTE_BASE + '/' + plan['campaign'] + '/bin'
    # Journal the complete intended unit set before any start. A partial start is
    # therefore stopped by the next arm, even when its receipt was never written.
    active = dict(campaign=plan['campaign'], armid=str(armid),
                  units={node: [p['unit_name'] for p in layout['processes'] if p['node'] == node]
                         for node in (roles if plan['schema'] == 'otel_native_scaling_v1' else fdb_roles)})
    _json(active_path, active, replace=True)
    try:
        for role in roles:
            node = nodes[role]
            root = layout['root']
            _require_result(run_on_node(plan, out, node,
                            ['sh', '-c', f'test ! -e {root} && install -d -m 0755 {root} {root}/logs {root}/data'],
                            runner=runner), 'create fresh persistent arm directory')
            _require_result(copy_to_node(plan, out, node, arm / 'fdb.cluster', root + '/fdb.cluster',
                                         runner=runner), 'copy cluster file')
            kind = 'fdb' if role.startswith('node') else 'client'
            slice_path = arm / ('horizontal-' + kind + '.slice')
            if not slice_path.exists():
                _mc.write_private(slice_path,
                                  _slice(kind, io_accounting=plan['schema'] in ('otel_native_diagnostic_v1', 'otel_native_session_v1', 'otel_native_scaling_v1')))
            _require_result(copy_to_node(plan, out, node, slice_path,
                                         '/etc/systemd/system/horizontal-' + kind + '.slice', runner=runner), 'copy resource slice')
            for p in [p for p in layout['processes'] if p['node'] == role]:
                local = arm / (role + '-' + p['unit_name'])
                _mc.write_private(local, p['unit'])
                _require_result(copy_to_node(plan, out, node, local,
                                             '/etc/systemd/system/' + p['unit_name'], runner=runner), 'copy native FDB unit')
                _require_result(run_on_node(plan, out, node,
                                ['install', '-d', '-m', '0700', '-o', 'foundationdb', '-g', 'foundationdb',
                                 f'{root}/data/{p["port"]}'], runner=runner), 'create native process data directory')
            if role.startswith('node'):
                _require_result(run_on_node(plan, out, node,
                                ['chown', '-R', 'foundationdb:foundationdb', root], runner=runner), 'set arm ownership')
            _require_result(run_on_node(plan, out, node, ['systemctl', 'daemon-reload'], runner=runner), 'reload own units')
            _require_result(run_on_node(plan, out, node, ['systemctl', 'start', 'horizontal-' + kind + '.slice'],
                                        runner=runner), 'activate own slice')
        for role in fdb_roles:
            if active['units'][role]:
                _require_result(run_on_node(plan, out, nodes[role], ['systemctl', 'start', *active['units'][role]],
                                            runner=runner), 'start native FDB processes')
        _require_result(run_on_node(plan, out, nodes['node1'],
                        [native_bin + '/fdbcli', '-C', layout['remote_cluster_file'], '--timeout', '30',
                         '--exec', CONFIGURE], runner=runner, timeout=35), 'configure fresh R1 cluster')
        _json(arm / 'layout.json', layout)
        layout['receipt'] = str(arm / 'layout.json')
        return layout
    except BaseException:
        # Never globally stop FDB or access a machine outside the exact inventory.
        for role in fdb_roles:
            if active['units'][role]:
                try:
                    run_on_node(plan, out, nodes[role], ['systemctl', 'stop', *active['units'][role]],
                                runner=runner, timeout=20)
                except (OSError, ProvisionError, subprocess.SubprocessError):
                    pass
        raise


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    action = parser.add_mutually_exclusive_group(required=True)
    action.add_argument('--guard-plan', help='auxiliary child mode; not an independent supervisor')
    action.add_argument('--teardown-plan', help='immediate cleanup for the external lifetime supervisor')
    parser.add_argument('--guard-out')
    parser.add_argument('--teardown-out')
    args = parser.parse_args(argv)
    try:
        if args.guard_plan:
            if not args.guard_out:
                parser.error('--guard-plan requires --guard-out')
            return _guard(args.guard_plan, args.guard_out)
        if not args.teardown_out:
            parser.error('--teardown-plan requires --teardown-out')
        return 0 if teardown(_load(args.teardown_plan), args.teardown_out)['passed'] else 1
    except (OSError, ValueError, KeyError, ProvisionError, _mc.GuardError) as exc:
        print('cleanup not verified: ' + str(exc), file=sys.stderr)
        return 2


if __name__ == '__main__':
    sys.exit(main())
