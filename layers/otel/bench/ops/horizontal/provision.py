#!/usr/bin/env python3
"""Five new, native-FDB campaign hosts; no API call occurs on import.

Public API: prepare(plan, out), setup_cluster(plan, out, nodes, storage_hosts,
armid), teardown(plan, out, resources), run_on_node, copy_to_node, ssh_argv.
Runners return (returncode, stdout, stderr) and accept argv, timeout=seconds.
State, generated host keys, inventories and receipts must be outside Git.
An auxiliary ordinary child guard is acknowledged before any cloud mutation.
The caller must also arm a separately supervised foreground lifetime guard.
Active work stops at first_epoch+6000; cleanup starts by first_epoch+6600,
with an absolute deadline no later than first_epoch+7200.
"""
import argparse
import contextlib
import fcntl
import hashlib
import ipaddress
import json
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


def _names(plan):
    prefix = plan['prefix']
    suffixes = {'network': 'net', 'firewall': 'fw', 'ssh-key': 'key', 'placement-group': 'spread'}
    return {kind: ({f'{prefix}-{r}' for r in ROLES} if kind == 'server' else
                   {f'{prefix}-{suffixes[kind]}'}) for kind in DELETE_KINDS}


def _network(plan):
    return ipaddress.ip_network(plan.get('network_cidr', '10.93.0.0/24'), strict=True)


def validate_plan(plan, out, clock=time.time):
    """Local-only validation. It neither calls hcloud nor executes sealed assets."""
    if plan.get('schema') != 'otel_horizontal_v1' or plan.get('approved') is not True:
        raise ProvisionError('an approved otel_horizontal_v1 plan is required')
    _mc.check_name(plan['prefix'])
    _mc.parse_campaign_label('campaign=' + plan['campaign'])
    for names in _names(plan).values():
        for name in names:
            _mc.check_name(name)
    if plan.get('location') != 'hel1' or plan.get('server_type') != 'ccx43':
        raise ProvisionError('only five new ccx43 servers in hel1 are authorized')
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
    if (clock() >= _active_deadline(plan)
            or (state / 'cleanup-started.json').exists()
            or (guard is not None and guard.poll() is not None)):
        raise ProvisionError('cleanup has started, deadline reached, or lifetime guard exited')


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
        _checked(runner, argv, timeout=_remaining(plan, 20, clock, active=True))
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
    if (len(matches) != 1 or any(node.get(k) != matches[0].get(k)
                                  for k in ('name', 'host', 'private_ip', 'known_hosts'))
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
    """Provision exactly five new servers and install sealed native binaries.

    Does not start FDB, run a workload, or make a health/readiness claim. A failed
    partial preparation immediately invokes cleanup. The auxiliary guard is not
    canceled before exact baseline equality; a separately supervised foreground
    lifetime guard must be armed by the caller before launching the controller.
    """
    validate_plan(plan, out, clock)
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
        hosts = [f'{plan["prefix"]}-{role}' for role in ROLES]
        _cloud_init.generate_keys(state / 'host-keys', hosts,
                                   runner=lambda argv: _call(runner, argv, timeout=_remaining(plan, 20, clock, active=True)))
        cloud_dir = state / 'cloud-init'
        _cloud_init.render(state / 'host-keys', plan['controller_public_key'], hosts, cloud_dir)
        for host in hosts:
            path = cloud_dir / f'{host}.user-data'
            text = path.read_text().replace('package_update: false', 'package_update: true')
            # Reuse the pre-generated host keys, not ssh-keyscan or TOFU.
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
        for i, role in enumerate(ROLES):
            name = f'{plan["prefix"]}-{role}'
            private = str(_network(plan).network_address + (11 + i if i < 3 else 18 + i))
            server = _create(plan, state, resources, 'server',
                             [h, 'server', 'create', '--name', name, '--type', 'ccx43',
                              '--location', 'hel1', '--image', 'ubuntu-24.04', '--ssh-key', str(key['id']),
                              '--firewall', str(firewall['id']), '--placement-group', str(group['id']),
                              '--without-ipv6', '--start-after-create=false', '--user-data-from-file',
                              str(cloud_dir / f'{name}.user-data'), *labels, '--label', 'role=' + role],
                             runner, clock, guard)
            current = _prove_ips(plan, state, resources, runner, clock, guard)[server['id']]
            _mutate(plan, state, [h, 'server', 'attach-to-network', str(server['id']),
                                 '--network', str(net['id']), '--ip', private], runner, clock, guard)
            _mutate(plan, state, [h, 'server', 'poweron', str(server['id'])], runner, clock, guard)
            node = dict(id=server['id'], name=name, role=role, created=current.get('created'),
                        host=current['public_net']['ipv4']['ip'], public_ipv4=current['public_net']['ipv4']['ip'],
                        private_ip=private, known_hosts=str(cloud_dir / 'known_hosts'),
                        private_key=plan['controller_private_key'])
            node['ssh'] = ssh_argv(plan, out, node, [])[:-1]
            node['scp_options'] = _ssh_options(plan, out, node)
            resources['nodes'][role] = node
            _save_resources(state, resources)
        fresh = {s['id']: s for s in _list(plan, 'server', runner)}
        for node in resources['nodes'].values():
            server = fresh[node['id']]
            if ((server.get('placement_group') or {}).get('id') != group['id']
                    or (server.get('server_type') or {}).get('name') != 'ccx43'
                    or (server.get('server_type') or {}).get('cpu_type') != 'dedicated'
                    or ((server.get('datacenter') or {}).get('location') or {}).get('name') != 'hel1'
                    or (server.get('labels') or {}).get('campaign') != plan['campaign']
                    or server.get('name') != node['name']
                    or len(server.get('private_net') or []) != 1
                    or server['private_net'][0].get('network') != net['id']
                    or server['private_net'][0].get('ip') != node['private_ip']):
                raise ProvisionError('server lacks exact isolated network / physical-spread assignment')
        _json(state / 'placement-proof.json', dict(type='spread', id=group['id'],
                                                   server_ids=resources['ids']['server']))
        remote = REMOTE_BASE + '/' + plan['campaign']
        resources['remote_root'] = remote
        for kind in ('fdb', 'client'):
            _mc.write_private(state / ('horizontal-' + kind + '.slice'), _slice(kind))
        for role, node in resources['nodes'].items():
            until = min(clock() + 600, _active_deadline(plan))
            while True:
                _check_window(plan, state, clock, guard)
                try:
                    ready = _call(runner, ssh_argv(plan, out, node,
                                  ['cloud-init', 'status', '--wait']),
                                  timeout=_remaining(plan, min(30, max(1, until-clock())), clock, active=True))
                except subprocess.TimeoutExpired:
                    ready = (1, '', 'cloud-init not ready')
                if ready[0] == 0:
                    break
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
    """Pure native R1 layout: fixed control placement; 1/2/3 storage hosts."""
    if not re.fullmatch(r'[A-Za-z0-9_-]{1,40}', str(armid)):
        raise ProvisionError('unsafe arm ID')
    if isinstance(storage_hosts, int) and not isinstance(storage_hosts, bool):
        selected = list(ROLES[:storage_hosts]) if storage_hosts in (1, 2, 3) else []
    else:
        selected = list(storage_hosts)
    if selected not in [list(ROLES[:n]) for n in (1, 2, 3)]:
        raise ProvisionError('storage hosts must be the first 1, 2 or 3 campaign FDB hosts')
    root = REMOTE_BASE + '/' + plan['campaign'] + '/arms/' + str(armid)
    cluster = 'horizontal_' + str(armid).replace('-', '_') + ':' + hashlib.sha256(
        (plan['campaign'] + ':' + str(armid)).encode()).hexdigest()[:24] + '@' + nodes['node1']['private_ip'] + ':4500'
    classes = [(4500, 'stateless'), (4501, 'commit_proxy'), (4502, 'grv_proxy'),
               (4503, 'resolution'), (4504, 'transaction')]
    processes = []
    for node in ROLES[:3]:
        specs = (classes if node == 'node1' else []) + ([(4510, 'storage')] if node in selected else [])
        for port, cls in specs:
            storage = cls == 'storage'
            address = f'{nodes[node]["private_ip"]}:{port}'
            name = f'oh-{plan["prefix"]}-{armid}-{port}'
            args = [REMOTE_BASE + '/' + plan['campaign'] + '/bin/fdbserver',
                    '--cluster-file', root + '/fdb.cluster', '--public-address', address,
                    '--listen-address', address, '--class', cls, '--datadir', f'{root}/data/{port}',
                    '--logdir', root + '/logs', '--locality-machineid', node,
                    '--locality-zoneid', node, '--memory', '4GiB' if storage else '1GiB',
                    '--cache-memory', '2GiB' if storage else '64MiB', '--trace-format', 'json',
                    '--maxlogssize', '128MiB', '--loggroup', plan['campaign']]
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
                                  memory_gib=4 if storage else 1, cache_mib=2048 if storage else 64,
                                  memory_limit_bytes=(4 if storage else 1) * GIB,
                                  machineid=node, zoneid=node, required_roles=required))
    return dict(root=root, cluster_text=cluster + '\n', remote_cluster_file=root + '/fdb.cluster',
                storage_hosts=selected, processes=processes, configure=CONFIGURE,
                budgets=dict(fdb_host=dict(cpu_quota_percent=400, allowed_cpus='0-3', memory_max_gib=16,
                                           max_process_memory_gib=9, max_processes=6),
                             client_host=dict(cpu_quota_percent=400, allowed_cpus='0-3', memory_max_gib=16)),
                storage_engine='ssd-redwood-1', log_engine='ssd-2', redundancy_mode='single')


def _slice(kind):
    return f'''[Unit]
Description=Horizontal campaign {kind} resource boundary
[Slice]
CPUAccounting=yes
MemoryAccounting=yes
AllowedCPUs=0-3
CPUQuota=400%
MemoryMax=16G
MemorySwapMax=0
'''


def setup_cluster(plan, out, nodes, storage_hosts, armid, *, runner=None, clock=time.time, sleep=time.sleep):
    """Start a fresh native R1 arm, stopping only units in this campaign's journal.

    Status/roles, memory quiet, kernel warnings, queue/lag and readiness checks
    belong to the caller. No previous data directory is reused or deleted.
    """
    state = _state(out)
    _check_window(plan, state, clock)
    for node in nodes.values():
        _authorized_node(plan, out, node)
    if set(nodes) != set(ROLES):
        raise ProvisionError('exactly the five prepared campaign nodes are required')
    layout = render_cluster(plan, nodes, storage_hosts, armid)
    arm = state / 'arms' / str(armid)
    arm.mkdir(mode=0o700, parents=True, exist_ok=False)
    active_path = state / 'active-arm.json'
    if active_path.exists():
        previous = _load(active_path)
        if previous.get('campaign') != plan['campaign']:
            raise ProvisionError('previous arm is not campaign-owned')
        for node in ROLES[:3]:
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
                         for node in ROLES[:3]})
    _json(active_path, active, replace=True)
    try:
        for role in ROLES:
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
                _mc.write_private(slice_path, _slice(kind))
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
        for role in ROLES[:3]:
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
        for role in ROLES[:3]:
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
