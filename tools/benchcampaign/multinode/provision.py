#!/usr/bin/env python3
"""Emit (default) or execute the hcloud commands for the same-VPC FDB bench topology.

Topology: one isolated private network with one cloud subnet, one firewall that
admits only controller SSH/ICMP on the public interface, one client machine and
three FoundationDB server machines, each with a fixed private address. Every
resource carries the campaign label (and a deadline label read by
teardown_guard.py); every name starts with --prefix.

Default is a dry run: print the commands, call nothing. --execute additionally
requires --teardown-deadline (UTC ISO or epoch, in the future and within
--max-lifetime-hours) and existing cloud-init files outside the repository; it
first makes read-only hcloud list calls and refuses any name collision, any
existing network overlapping --ip-range, and any overlap with --protected-cidr.
Servers are created stopped, attached to the network with their fixed private
IP, then powered on, so cloud-init's first boot already sees the private NIC.

Hetzner Cloud Firewalls filter only public interfaces; private-network traffic
is restricted on the hosts themselves (nftables rules from fdb_cluster.py).
This script never reads or passes an API token: hcloud uses its own context.
"""
import argparse
import ipaddress
import json
from pathlib import Path
import sys
import time

import mn_common as mc

ROLE_NAMES = ('client', 'node1', 'node2', 'node3')


def parse_args(argv=None):
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument('--label', required=True, help='campaign=VALUE; applied to every resource')
    ap.add_argument('--prefix', default='fdb-bench-vpc', help='every resource name starts with this')
    ap.add_argument('--network-name', help='default: the prefix itself')
    ap.add_argument('--ip-range', default='10.88.0.0/24')
    ap.add_argument('--subnet-range', default='10.88.0.0/24')
    ap.add_argument('--network-zone', default='eu-central')
    ap.add_argument('--location', default='fsn1')
    ap.add_argument('--image', default='ubuntu-24.04')
    ap.add_argument('--client-type', default='ccx43')
    ap.add_argument('--server-type', default='ccx43')
    ap.add_argument('--client-ip', default='10.88.0.10')
    ap.add_argument('--node-ip', action='append', help='three private IPs, node1..node3 (default 10.88.0.11-13)')
    ap.add_argument('--controller-cidr', action='append', required=True,
                    help='public source CIDR allowed to SSH (repeat); no default by design')
    ap.add_argument('--protected-cidr', action='append', default=[],
                    help='an existing private range the campaign network must not overlap (repeat)')
    ap.add_argument('--protected-name', action='append', default=[],
                    help='a resource name the campaign must never use (repeat)')
    ap.add_argument('--protect-file', action='append', default=[],
                    help='private kind/id/name[/cidr] list (inventory.py snapshot): its names and network ranges are protected (repeat)')
    key = ap.add_mutually_exclusive_group(required=True)
    key.add_argument('--ssh-key-name', help='existing hcloud project SSH key (public) to install for root')
    key.add_argument('--ssh-public-key', help='controller public key file; creates a campaign-labelled hcloud SSH key')
    ap.add_argument('--cloud-init-dir', required=True,
                    help='render_cloud_init.py output (outside the repo): <server-name>.user-data per host')
    ap.add_argument('--work-dir', required=True, help='outside the repo: firewall rules file and receipts')
    ap.add_argument('--teardown-deadline', help='UTC ISO or epoch; required with --execute; labelled on resources')
    ap.add_argument('--max-lifetime-hours', type=float, default=6.0)
    ap.add_argument('--role', action='append', choices=ROLE_NAMES,
                    help='servers to create (repeat; default all four), e.g. the client first to build on it')
    ap.add_argument('--skip-shared', action='store_true',
                    help='second stage: network, firewall and SSH key already exist (must carry the campaign label)')
    ap.add_argument('--hcloud', default='hcloud')
    ap.add_argument('--execute', action='store_true', help='actually create resources (default: print only)')
    args = ap.parse_args(argv)
    args.role = [r for r in ROLE_NAMES if r in (args.role or ROLE_NAMES)]
    args.campaign = mc.parse_campaign_label(args.label)
    args.network_name = args.network_name or args.prefix
    args.node_ip = args.node_ip or ['10.88.0.11', '10.88.0.12', '10.88.0.13']
    for path in args.protect_file:
        protected = mc.read_protect_file(path)
        args.protected_name += protected['names']
        args.protected_cidr += protected['cidrs']
    return args


def firewall_rules(controller_cidrs):
    sources = []
    for cidr in controller_cidrs:
        net = ipaddress.ip_network(cidr, strict=True)
        if net.prefixlen < (24 if net.version == 4 else 48):
            raise mc.GuardError('controller CIDR is too broad for an SSH allow rule: ' + cidr)
        sources.append(str(net))
    return [
        dict(direction='in', protocol='tcp', port='22', source_ips=sources, description='controller ssh only'),
        dict(direction='in', protocol='icmp', source_ips=sources, description='controller icmp only'),
    ]


def validate(args):
    for name in [args.prefix, args.network_name]:
        mc.check_name(name)
    if not mc.owned_name(args.network_name, args.prefix):
        raise mc.GuardError('network name must be the prefix or start with prefix-')
    net = ipaddress.ip_network(args.ip_range, strict=True)
    sub = ipaddress.ip_network(args.subnet_range, strict=True)
    if not net.is_private or not sub.subnet_of(net):
        raise mc.GuardError('subnet must be a private range inside the network range')
    for cidr in args.protected_cidr:
        if net.overlaps(ipaddress.ip_network(cidr, strict=False)):
            raise mc.GuardError(f'network range {net} overlaps protected range {cidr}')
    ips = [args.client_ip, *args.node_ip]
    if len(args.node_ip) != 3 or len(set(ips)) != 4:
        raise mc.GuardError('exactly three distinct node IPs plus a distinct client IP required')
    for ip in ips:
        addr = ipaddress.ip_address(ip)
        # Hetzner reserves the first usable address of a subnet for its gateway.
        if addr not in sub or addr in (sub.network_address, sub.broadcast_address, sub.network_address + 1):
            raise mc.GuardError(f'{ip} is not a usable host address in {sub}')
    names = [server_name(args, r) for r in ROLE_NAMES] + [args.network_name, firewall_name(args)]
    if set(names) & set(args.protected_name):
        raise mc.GuardError('a campaign resource name is protected: ' + ', '.join(sorted(set(names) & set(args.protected_name))))
    firewall_rules(args.controller_cidr)


def server_name(args, role):
    return f'{args.prefix}-{role}'


def firewall_name(args):
    return f'{args.prefix}-fw'


def ssh_key_name(args):
    return args.ssh_key_name or f'{args.prefix}-controller'


def labels(args, deadline, **extra):
    out = ['--label', f'campaign={args.campaign}']
    if deadline is not None:
        out += ['--label', f'deadline={deadline}']
    for k, v in extra.items():
        out += ['--label', f'{k}={v}']
    return out


def build_commands(args, deadline=None):
    """The complete ordered argv list; nothing is executed here."""
    validate(args)
    h = args.hcloud
    work = Path(args.work_dir)
    rules = work / 'firewall-rules.json'
    cmds = [] if args.skip_shared else [
        [h, 'network', 'create', '--name', args.network_name, '--ip-range', args.ip_range, *labels(args, deadline)],
        [h, 'network', 'add-subnet', args.network_name, '--network-zone', args.network_zone,
         '--type', 'cloud', '--ip-range', args.subnet_range],
        [h, 'firewall', 'create', '--name', firewall_name(args), '--rules-file', str(rules), *labels(args, deadline)],
    ]
    if args.ssh_public_key and not args.skip_shared:
        cmds.append([h, 'ssh-key', 'create', '--name', ssh_key_name(args),
                     '--public-key-from-file', str(Path(args.ssh_public_key)), *labels(args, deadline)])
    ips = dict(zip(ROLE_NAMES, [args.client_ip, *args.node_ip]))
    for role in args.role:
        name = server_name(args, role)
        kind = 'client' if role == 'client' else 'fdb-server'
        stype = args.client_type if role == 'client' else args.server_type
        cmds.append([h, 'server', 'create', '--name', name, '--type', stype, '--image', args.image,
                     '--location', args.location, '--ssh-key', ssh_key_name(args), '--firewall', firewall_name(args),
                     '--user-data-from-file', str(Path(args.cloud_init_dir) / f'{name}.user-data'),
                     '--without-ipv6', '--start-after-create=false',
                     *labels(args, deadline, role=kind, node=role)])
        cmds.append([h, 'server', 'attach-to-network', name, '--network', args.network_name, '--ip', ips[role]])
    for role in args.role:
        cmds.append([h, 'server', 'poweron', server_name(args, role)])
    return cmds, rules, firewall_rules(args.controller_cidr)


def _json_list(runner, argv):
    rc, out, err = runner(argv)
    if rc:
        raise mc.GuardError(f'read-only preflight failed: {mc.quote(argv)}: {err.strip()}')
    return mc.parse_json_list(out, mc.quote(argv))


def preflight(args, runner):
    """Read-only checks against the live project; raises on any conflict."""
    h = args.hcloud
    for item in _json_list(runner, [h, 'server', 'list', '-o', 'json']):
        if item.get('name') in {server_name(args, r) for r in args.role}:
            raise mc.GuardError(f'server {item["name"]} already exists; refusing to adopt it')
    shared = {'network': args.network_name, 'firewall': firewall_name(args)}
    for kind, name in shared.items():
        found = False
        for item in _json_list(runner, [h, kind, 'list', '-o', 'json']):
            mine = item.get('name') == name
            if mine and not args.skip_shared:
                raise mc.GuardError(f'{kind} {name} already exists; refusing to adopt it')
            if mine and (item.get('labels') or {}).get('campaign') != args.campaign:
                raise mc.GuardError(f'{kind} {name} exists without label campaign={args.campaign}')
            found |= mine
            if kind == 'network' and not mine and ipaddress.ip_network(item['ip_range']).overlaps(ipaddress.ip_network(args.ip_range)):
                raise mc.GuardError(f'existing network {item["name"]} {item["ip_range"]} overlaps {args.ip_range}')
        if args.skip_shared and not found:
            raise mc.GuardError(f'--skip-shared but {kind} {name} does not exist')
    if args.ssh_public_key and not args.skip_shared:
        for item in _json_list(runner, [h, 'ssh-key', 'list', '-o', 'json']):
            if item.get('name') == ssh_key_name(args):
                raise mc.GuardError('ssh key name already exists; pass --ssh-key-name to reuse it')


def check_execute_inputs(args, now):
    if not args.teardown_deadline:
        raise mc.GuardError('--execute requires --teardown-deadline (arm teardown_guard.py first)')
    deadline = mc.parse_deadline(args.teardown_deadline)
    if deadline <= now or deadline > now + args.max_lifetime_hours * 3600:
        raise mc.GuardError(f'deadline {mc.iso(deadline)} must be in the future and within {args.max_lifetime_hours}h')
    for path, what in [(args.cloud_init_dir, 'cloud-init dir'), (args.work_dir, 'work dir')]:
        mc.require_outside_repo(path, what)
    for role in args.role:
        f = Path(args.cloud_init_dir) / f'{server_name(args, role)}.user-data'
        if not f.is_file():
            raise mc.GuardError('missing cloud-init document ' + str(f))
        if f.stat().st_mode & 0o077:
            raise mc.GuardError(f'{f} holds a host private key and must be mode 0600')
    if args.ssh_public_key:
        text = Path(args.ssh_public_key).read_text()
        if 'PRIVATE KEY' in text or not text.startswith(('ssh-ed25519 ', 'ssh-rsa ', 'ecdsa-sha2-')):
            raise mc.GuardError('--ssh-public-key must be an OpenSSH public key file')
    return deadline


def run(args, runner=mc.default_runner, now=None, out=sys.stdout):
    now = int(time.time()) if now is None else now
    deadline = mc.parse_deadline(args.teardown_deadline) if args.teardown_deadline else None
    if args.execute:
        deadline = check_execute_inputs(args, now)
    cmds, rules_path, rules = build_commands(args, deadline)
    if not args.execute:
        print('# DRY RUN: nothing executed. Firewall rules file content for ' + str(rules_path) + ':', file=out)
        print('# ' + json.dumps(rules), file=out)
        for c in cmds:
            print(mc.quote(c), file=out)
        return dict(executed=False, commands=cmds, rules=rules)
    preflight(args, runner)
    work = mc.private_dir(args.work_dir)
    if not args.skip_shared:
        mc.dump_json(rules_path, rules)
    log = []
    try:
        for c in cmds:
            rc, stdout, stderr = runner(c)
            log.append(dict(argv=c, returncode=rc, stdout=stdout, stderr=stderr, utc=mc.iso(int(time.time()))))
            print(('OK   ' if rc == 0 else 'FAIL ') + mc.quote(c), file=out)
            if rc:
                raise mc.GuardError('command failed; run teardown_guard.py --execute now: ' + stderr.strip())
        hosts = {}
        for role in args.role:
            rc, stdout, stderr = runner([args.hcloud, 'server', 'describe', server_name(args, role), '-o', 'json'])
            if rc:
                raise mc.GuardError('describe failed: ' + stderr.strip())
            info = json.loads(stdout)
            hosts[role] = dict(id=info['id'], name=info['name'],
                               public_ipv4=((info.get('public_net') or {}).get('ipv4') or {}).get('ip'),
                               private=[n.get('ip') for n in info.get('private_net') or []],
                               labels=info.get('labels'))
        mc.dump_json(work / f'hosts-{"-".join(args.role)}.json', hosts)
    finally:
        mc.dump_json(work / f'provision-log-{int(time.time())}.json', log)
    return dict(executed=True, commands=cmds, hosts=hosts)


def main(argv=None):
    try:
        run(parse_args(argv))
    except mc.GuardError as exc:
        print('REFUSED: ' + str(exc), file=sys.stderr)
        return 2
    return 0


if __name__ == '__main__':
    sys.exit(main())
