#!/usr/bin/env python3
"""Generate and verify the three-node FoundationDB layout from private IPs.

render: writes, under a new --output-dir,
  fdb.cluster              DESC:ID@node1:4500,node2:4500,node3:4500 (3 coordinators)
  configure.fdbcli         the one-time `configure new ...` command
  layout.json              every process: node, IP, port, class, CPUs, args, expected roles
  nodeN/foundationdb.conf  fdbmonitor-format equivalent (provenance; NOT used to start)
  nodeN/fdb-bench-PORT.service  one systemd unit per process: CPUAffinity pinned to a
                           full SMT sibling pair, Restart=no (a crash is an
                           environmental failure, never silently healed)
  nodeN/install-units.sh   installs the units, creates data dirs, starts them
  nodeN/nftables.conf, client/nftables.conf  host firewall: VPC + controller SSH only
  client/fdb.cluster

check-roles: compares an `fdbcli --exec 'status json'` document against
layout.json: process classes/addresses, every role on its expected process and
nowhere else, nine storage servers, three coordinators, redundancy and engine.

Layout (16-vCPU nodes, CPUs 0-1 housekeeping, sibling pairs assumed (2k,2k+1)
and verified on the machine before use):
  node1  4500 stateless (CC, master, DD, RK) + coordinator, 4501 commit_proxy,
         4502 grv_proxy, 4503 resolution, 4504-4506 storage
  node2  4500 coordinator, 4501 transaction (log), 4502-4504 storage
  node3  as node2
"""
import argparse
import ipaddress
import json
from pathlib import Path
import re
import sys

import mn_common as mc

NODES = ('node1', 'node2', 'node3')
# (port, class, cpus)
LAYOUT = {
    'node1': [(4500, 'stateless', (2, 3)), (4501, 'commit_proxy', (4, 5)), (4502, 'grv_proxy', (6, 7)),
              (4503, 'resolution', (8, 9)), (4504, 'storage', (10, 11)), (4505, 'storage', (12, 13)),
              (4506, 'storage', (14, 15))],
    'node2': [(4500, 'coordinator', (2, 3)), (4501, 'transaction', (4, 5)), (4502, 'storage', (6, 7)),
              (4503, 'storage', (8, 9)), (4504, 'storage', (10, 11))],
    'node3': [(4500, 'coordinator', (2, 3)), (4501, 'transaction', (4, 5)), (4502, 'storage', (6, 7)),
              (4503, 'storage', (8, 9)), (4504, 'storage', (10, 11))],
}
COORDINATOR_PORT = 4500
# Roles a process of this class must hold (required) and may hold (allowed).
CLASS_ROLES = {
    'stateless': ({'cluster_controller', 'master'}, {'cluster_controller', 'master', 'data_distributor', 'ratekeeper', 'coordinator'}),
    'commit_proxy': ({'commit_proxy'}, {'commit_proxy'}),
    'grv_proxy': ({'grv_proxy'}, {'grv_proxy'}),
    'resolution': ({'resolver'}, {'resolver'}),
    'transaction': ({'log'}, {'log'}),
    'storage': ({'storage'}, {'storage'}),
    'coordinator': (set(), {'coordinator'}),
}
ENGINES = {'ssd-2', 'memory'}
MEMORY = {'storage': '8GiB', 'transaction': '8GiB'}
DEFAULT_MEMORY = '4GiB'


def parse_args(argv=None):
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = ap.add_subparsers(dest='command', required=True)
    r = sub.add_parser('render')
    r.add_argument('--node-ip', action='append', required=True, help='node1..node3 private IPs, in order')
    r.add_argument('--client-ip', required=True)
    r.add_argument('--vpc-cidr', default='10.88.0.0/24')
    r.add_argument('--controller-cidr', action='append', required=True)
    r.add_argument('--private-iface', help='restrict VPC acceptance to this interface name (e.g. enp7s0)')
    r.add_argument('--description', required=True, help='cluster file description [A-Za-z0-9_]')
    r.add_argument('--cluster-id', required=True, help='cluster file ID [A-Za-z0-9_]')
    r.add_argument('--engine', choices=sorted(ENGINES), default='ssd-2')
    r.add_argument('--root', default='/srv/fdb-bench/cluster', help='on-node data/log root')
    r.add_argument('--fdbserver', default='/usr/sbin/fdbserver')
    r.add_argument('--loggroup', default='fdb-bench-vpc')
    r.add_argument('--knob', action='append', default=[], help='server knob NAME=VALUE for every process (repeat)')
    r.add_argument('--output-dir', required=True)
    c = sub.add_parser('check-roles')
    c.add_argument('--layout', required=True)
    c.add_argument('--status', required=True, help='fdbcli status json output')
    c.add_argument('--output', help='write the receipt here (exclusive)')
    return ap.parse_args(argv)


def build_layout(node_ips, client_ip, vpc_cidr, description, cluster_id, engine, root, fdbserver, loggroup, knobs):
    if len(node_ips) != 3 or len({*node_ips, client_ip}) != 4:
        raise mc.GuardError('three distinct node IPs and a distinct client IP required')
    for ip in [*node_ips, client_ip]:
        if not mc.ip_in(vpc_cidr, ip):
            raise mc.GuardError(f'{ip} outside VPC {vpc_cidr}')
    for value, what in [(description, 'description'), (cluster_id, 'cluster id')]:
        if not re.fullmatch(r'[A-Za-z0-9_]{1,64}', value):
            raise mc.GuardError(f'unsafe cluster {what}: {value}')
    if engine not in ENGINES:
        raise mc.GuardError('engine must be ssd-2 or memory')
    parsed_knobs = []
    for k in knobs:
        name, sep, value = k.partition('=')
        if not sep or not re.fullmatch(r'[a-z0-9_]+', name) or not re.fullmatch(r'[A-Za-z0-9_.:-]+', value):
            raise mc.GuardError('knob must be lower_snake_name=VALUE: ' + k)
        parsed_knobs.append((name, value))
    ips = dict(zip(NODES, node_ips))
    coordinators = [f'{ips[n]}:{COORDINATOR_PORT}' for n in NODES]
    cluster_file = f'{description}:{cluster_id}@{",".join(coordinators)}'
    processes = []
    used = set()
    for node in NODES:
        cpus_seen = set()
        for port, cls, cpus in LAYOUT[node]:
            if set(cpus) & cpus_seen or set(cpus) & {0, 1}:
                raise mc.GuardError('CPU pinning overlaps another process or housekeeping')
            cpus_seen |= set(cpus)
            addr = f'{ips[node]}:{port}'
            used.add(addr)
            options = [('cluster-file', f'{root}/fdb.cluster'), ('public-address', addr),
                       ('listen-address', addr), ('datadir', f'{root}/data/{port}'), ('logdir', f'{root}/logs'),
                       ('class', cls), ('locality-machineid', node), ('locality-zoneid', node),
                       ('memory', MEMORY.get(cls, DEFAULT_MEMORY))]
            if cls == 'storage':
                options += [('cache-memory', '2GiB'), ('storage-memory', '2GiB' if engine == 'memory' else '1GiB')]
            options += [('trace-format', 'json'), ('maxlogssize', '256MiB'), ('loggroup', loggroup)]
            options += [(f'knob_{n}', v) for n, v in parsed_knobs]
            args = [fdbserver]
            for k, v in options:
                # Knobs take --knob_NAME=VALUE; every other option is --name VALUE.
                args += [f'--{k}={v}'] if k.startswith('knob_') else [f'--{k}', v]
            required, allowed = CLASS_ROLES[cls]
            if addr in coordinators:
                required, allowed = required | {'coordinator'}, allowed | {'coordinator'}
            processes.append(dict(node=node, ip=ips[node], port=port, address=addr, process_class=cls,
                                  cpus=list(cpus), options=[list(o) for o in options], args=args, required_roles=sorted(required),
                                  allowed_roles=sorted(allowed)))
    configure = (f'configure new double {engine} commit_proxies=1 grv_proxies=1 resolvers=1 logs=2')
    return dict(version=1, cluster_file=cluster_file, coordinators=coordinators, engine=engine,
                redundancy_mode='double', configure=configure, client_ip=client_ip, vpc_cidr=vpc_cidr,
                root=root, knobs=dict(parsed_knobs), processes=processes,
                storage_servers=sum(p['process_class'] == 'storage' for p in processes))


def unit_text(p):
    return f"""[Unit]
Description=FDB bench {p['node']} {p['address']} ({p['process_class']})
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=foundationdb
Group=foundationdb
CPUAffinity={' '.join(map(str, p['cpus']))}
LimitNOFILE=100000
ExecStart={mc.quote(p['args'])}
Restart=no

[Install]
WantedBy=multi-user.target
"""


def monitor_conf(layout, node):
    lines = ['# fdbmonitor-format record of the processes this node runs. Provenance only:',
             '# the campaign starts the generated systemd units (CPU affinity, no auto-restart).',
             '[fdbmonitor]', 'user = foundationdb', 'group = foundationdb', '',
             '[general]', f'cluster-file = {layout["root"]}/fdb.cluster', 'restart-delay = 60', '',
             '[fdbserver]', 'command = /usr/sbin/fdbserver', '']
    for p in layout['processes']:
        if p['node'] != node:
            continue
        lines.append(f'[fdbserver.{p["port"]}]')
        lines += [f'{k} = {v}' for k, v in p['options'] if k != 'cluster-file']
        lines.append(f'# cpus = {",".join(map(str, p["cpus"]))}')
        lines.append('')
    return '\n'.join(lines)


def nft_text(vpc_cidr, controller_cidrs, iface=None):
    for c in controller_cidrs:
        ipaddress.ip_network(c, strict=True)
    ctl = ', '.join(controller_cidrs)
    vpc = (f'iifname "{iface}" ' if iface else '') + f'ip saddr {vpc_cidr} accept'
    return f"""#!/usr/sbin/nft -f
# Host firewall: Hetzner Cloud Firewalls do not filter the private network.
table inet fdbbench
delete table inet fdbbench
table inet fdbbench {{
  chain input {{
    type filter hook input priority 0; policy drop;
    iif "lo" accept
    ct state established,related accept
    {vpc}
    ip saddr {{ {ctl} }} tcp dport 22 accept
    ip saddr {{ {ctl} }} icmp type echo-request accept
    udp sport 67 udp dport 68 accept
  }}
}}
"""


def install_script(layout, node):
    ports = [p['port'] for p in layout['processes'] if p['node'] == node]
    root = layout['root']
    units = ' '.join(f'fdb-bench-{p}.service' for p in ports)
    dirs = ' '.join(f'"{root}/data/{p}"' for p in ports)
    return f"""#!/usr/bin/env bash
# Generated by fdb_cluster.py for {node}. Run as root from this directory after the
# pinned FoundationDB 7.3.77 packages are installed and foundationdb.service is masked.
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
if pgrep -x fdbserver >/dev/null; then echo 'refusing: an fdbserver is already running' >&2; exit 1; fi
install -d -m 0755 -o foundationdb -g foundationdb "{root}" "{root}/data" "{root}/logs"
for d in {dirs}; do install -d -m 0700 -o foundationdb -g foundationdb "$d"; done
install -m 0644 -o foundationdb -g foundationdb "$here/../fdb.cluster" "{root}/fdb.cluster"
nft -f "$here/nftables.conf"
for u in {units}; do install -m 0644 "$here/$u" "/etc/systemd/system/$u"; done
systemctl daemon-reload
systemctl start {units}
sleep 2
for u in {units}; do systemctl is-active --quiet "$u" || {{ echo "$u not active" >&2; exit 1; }}; done
for u in {units}; do printf '%s %s\\n' "$u" "$(systemctl show -p MainPID --value "$u")"; done
"""


def render(args):
    out = Path(args.output_dir)
    mc.require_outside_repo(out, 'cluster output dir')
    out.mkdir(parents=True, exist_ok=False)
    layout = build_layout(args.node_ip, args.client_ip, args.vpc_cidr, args.description, args.cluster_id,
                          args.engine, args.root, args.fdbserver, args.loggroup, args.knob)
    mc.dump_json(out / 'layout.json', layout)
    (out / 'fdb.cluster').write_text(layout['cluster_file'] + '\n')
    (out / 'configure.fdbcli').write_text(layout['configure'] + '\n')
    for node in NODES:
        d = out / node
        d.mkdir()
        (d / 'foundationdb.conf').write_text(monitor_conf(layout, node))
        (d / 'nftables.conf').write_text(nft_text(args.vpc_cidr, args.controller_cidr, args.private_iface))
        for p in layout['processes']:
            if p['node'] == node:
                (d / f'fdb-bench-{p["port"]}.service').write_text(unit_text(p))
        script = d / 'install-units.sh'
        script.write_text(install_script(layout, node))
        script.chmod(0o755)
    c = out / 'client'
    c.mkdir()
    (c / 'fdb.cluster').write_text(layout['cluster_file'] + '\n')
    (c / 'nftables.conf').write_text(nft_text(args.vpc_cidr, args.controller_cidr, args.private_iface))
    return layout


def _addr(a):
    return a[:-4] if a.endswith(':tls') else a


def check_roles(layout, status):
    errors = []
    cluster = status.get('cluster') or {}
    procs = {}
    for pid, p in (cluster.get('processes') or {}).items():
        procs[_addr(p.get('address', ''))] = dict(pid=pid, cls=p.get('class_type'),
                                                  roles={r.get('role') for r in p.get('roles') or []},
                                                  machine=(p.get('locality') or {}).get('machineid'))
    expected = {p['address']: p for p in layout['processes']}
    if set(procs) != set(expected):
        errors.append(f'process addresses differ: missing {sorted(set(expected) - set(procs))}, '
                      f'unexpected {sorted(set(procs) - set(expected))}')
    for addr, p in expected.items():
        got = procs.get(addr)
        if not got:
            continue
        if got['cls'] != p['process_class']:
            errors.append(f'{addr} class {got["cls"]} != {p["process_class"]}')
        if got['machine'] != p['node']:
            errors.append(f'{addr} machineid {got["machine"]} != {p["node"]}')
        missing = set(p['required_roles']) - got['roles']
        extra = got['roles'] - set(p['allowed_roles'])
        if missing:
            errors.append(f'{addr} lacks roles {sorted(missing)}')
        if extra:
            errors.append(f'{addr} holds unexpected roles {sorted(extra)}')
    storage = sum('storage' in p['roles'] for p in procs.values())
    if storage != layout['storage_servers']:
        errors.append(f'{storage} storage roles, expected {layout["storage_servers"]}')
    coords = sorted(_addr(c.get('address', '')) for c in ((status.get('client') or {}).get('coordinators') or {}).get('coordinators') or [])
    if coords != sorted(layout['coordinators']):
        errors.append(f'coordinators {coords} != {sorted(layout["coordinators"])}')
    conf = cluster.get('configuration') or {}
    want = dict(redundancy_mode=layout['redundancy_mode'], storage_engine=layout['engine'],
                commit_proxies=1, grv_proxies=1, resolvers=1, logs=2)
    for k, v in want.items():
        if conf.get(k) != v:
            errors.append(f'configuration {k}={conf.get(k)!r}, expected {v!r}')
    available = cluster.get('database_available')
    if available is None:
        available = ((status.get('client') or {}).get('database_status') or {}).get('available')
    if available is not True:
        errors.append('database not available')
    return dict(passed=not errors, errors=errors, processes=len(procs), storage_servers=storage, coordinators=coords)


def main(argv=None):
    args = parse_args(argv)
    try:
        if args.command == 'render':
            layout = render(args)
            print(f'{len(layout["processes"])} processes; cluster file {layout["cluster_file"]}')
            return 0
        receipt = check_roles(json.loads(Path(args.layout).read_text()), json.loads(Path(args.status).read_text()))
        if args.output:
            mc.dump_json(args.output, receipt)
        print(json.dumps(receipt, indent=2, sort_keys=True))
        return 0 if receipt['passed'] else 1
    except (mc.GuardError, ValueError) as exc:
        print('REFUSED: ' + str(exc), file=sys.stderr)
        return 2


if __name__ == '__main__':
    sys.exit(main())
