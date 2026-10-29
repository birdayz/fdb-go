#!/usr/bin/env python3
"""Plan legacy hcloud diagnostics or execute guarded HTTPS campaign provisioning.

Topology: one isolated private network with one cloud subnet, one firewall that
admits only controller SSH/ICMP on the public interface, one client machine and
three FoundationDB server machines, each with a fixed private address. Every
resource carries the campaign label (and a deadline label read by
teardown_guard.py); every name starts with --prefix.

Default is a diagnostic dry run: print legacy CLI plans, call nothing.
--execute uses the authenticated creation ledger, live supervised guard and fixed
seven-resource HTTPS plan; it requires the pinned image, private cloud-init and
fixed teardown deadline. Fresh complete provider censuses reject collisions and
network overlap. Existing-key, role-selection and staged plans are dry-run only.
Servers are created stopped, attached to the network with their fixed private
IP, then powered on, so cloud-init's first boot already sees the private NIC.

Hetzner Cloud Firewalls filter only public interfaces; private-network traffic
is restricted on the hosts themselves (nftables rules from fdb_cluster.py).
Dry runs never load credentials. Guarded execution uses the sealed controller-only
hcloud auth profile and one-shot HTTPS; no hcloud mutation command is executed.
"""
import argparse
import contextlib
import ipaddress
import json
from pathlib import Path
import sys
import time

import mn_common as mc
import campaign_cloud as cc
import campaign_safety as cs
import creation_ledger as cl

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
    key.add_argument('--ssh-key-name', help='diagnostic dry-run only: existing project SSH key (no guarded execution)')
    key.add_argument('--ssh-public-key', help='controller public key file; creates a campaign-labelled hcloud SSH key')
    ap.add_argument('--cloud-init-dir', required=True,
                    help='render_cloud_init.py output (outside the repo): <server-name>.user-data per host')
    ap.add_argument('--work-dir', required=True, help='outside the repo: firewall rules file and receipts')
    ap.add_argument('--teardown-deadline', help='UTC ISO or epoch; required with --execute; labelled on resources')
    ap.add_argument('--max-lifetime-hours', type=float, default=6.0)
    ap.add_argument('--role', action='append', choices=ROLE_NAMES,
                    help='diagnostic dry-run role selection (repeat); guarded execution requires all four')
    ap.add_argument('--skip-shared', action='store_true',
                    help='diagnostic dry-run only: show a later stage with pre-existing shared resources')
    ap.add_argument('--hcloud', default='hcloud')
    ap.add_argument('--creation-ledger', help='private exact-ownership journal; mandatory for execution')
    ap.add_argument('--initialize-ledger', action='store_true', help='initialize and register this one foreground producer')
    ap.add_argument('--inventory', help='sealed inventory.py snapshot directory; required for initialization')
    ap.add_argument('--activation-time', help='owner-selected fixed T0, before arming; never chosen automatically')
    ap.add_argument('--supervisor-pid', type=int, help='live owning controller supervisor PID')
    ap.add_argument('--context', help='sealed hcloud context (CLI precedence)')
    ap.add_argument('--hcloud-config', help='protected local CLI TOML config; no token argument')
    ap.add_argument('--accept-benign-writers', action='store_true')
    ap.add_argument('--reserve-owned-graph', action='store_true')
    ap.add_argument('--guard-wait-seconds', type=float, default=60)
    ap.add_argument('--execute', action='store_true', help='actually create resources (default: print only)')
    args = ap.parse_args(argv)
    args.role = [r for r in ROLE_NAMES if r in (args.role or ROLE_NAMES)]
    args.campaign = mc.parse_campaign_label(args.label)
    args.network_name = args.network_name or args.prefix
    args.node_ip = args.node_ip or ['10.88.0.11', '10.88.0.12', '10.88.0.13']
    args.protect_hashes = {}
    for path in args.protect_file:
        raw = cc.private_bytes(path) if args.execute else Path(path).read_bytes()
        protected = mc.read_protect_file(path, raw=raw)
        args.protect_hashes[str(Path(path).absolute())] = cc.digest(raw)
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


class GuardedPlan(list):
    def __init__(self, entries, private_inputs, protections):
        super().__init__(entries)
        self.private_inputs, self.protections = private_inputs, protections


def guarded_plan(args, deadline):
    inputs = {}
    def captured(path):
        path = str(Path(path).absolute())
        raw = cc.private_bytes(path)
        inputs[path] = cc.digest(raw)
        return raw
    typed = {kind: [] for kind in mc.PROTECT_KINDS}
    for path in args.protect_file:
        raw = captured(path)
        cc.need(inputs[str(Path(path).absolute())] == args.protect_hashes[str(Path(path).absolute())],
                'protected campaign input changed after parsing')
        for kind, ids in mc.read_protect_file(path, typed=True, raw=raw)['by_kind'].items():
            typed[kind].extend(ids)
    names = [''.join(f'[{c}]' if c in '*?[' else c for c in name) for name in args.protected_name]
    protections = cs.protection_policy(names=names, by_kind=typed)
    base_labels = dict(campaign=args.campaign, deadline=str(deadline))
    rules = firewall_rules(args.controller_cidr)
    entries = [dict(kind='network', name=args.network_name,
                    body=dict(name=args.network_name, ip_range=args.ip_range, labels=base_labels)),
               dict(kind='firewall', name=firewall_name(args),
                    body=dict(name=firewall_name(args), rules=rules, labels=base_labels))]
    cc.need(args.ssh_public_key and not args.ssh_key_name, 'guarded plan requires a NEW campaign SSH key')
    key = captured(args.ssh_public_key).decode().strip()
    cc.need(key.startswith(('ssh-ed25519 ', 'ssh-rsa ', 'ecdsa-sha2-')) and 'PRIVATE KEY' not in key,
            'invalid campaign public SSH key')
    entries.append(dict(kind='ssh-key', name=ssh_key_name(args),
                        body=dict(name=ssh_key_name(args), public_key=key, labels=base_labels)))
    for role, ip in zip(ROLE_NAMES, [args.client_ip, *args.node_ip]):
        name = server_name(args, role)
        userdata = captured(Path(args.cloud_init_dir) / (name + '.user-data')).decode()
        entries.append(dict(kind='server', name=name, private_ip=ip, body=dict(name=name,
            server_type='ccx43', location='fsn1', image=161547269, ssh_keys=[ssh_key_name(args)],
            firewalls=[dict(firewall=firewall_name(args))], start_after_create=False,
            public_net=dict(enable_ipv4=True, enable_ipv6=False), user_data=userdata,
            labels=dict(base_labels, role='client' if role == 'client' else 'fdb-server', node=role))))
    return GuardedPlan(entries, inputs, protections)


def initialize_guarded(args, deadline, profile, cloud, plan):
    cc.need(args.inventory and args.activation_time and args.supervisor_pid,
            'initialization requires inventory, fixed activation time and owning supervisor')
    cc.need(args.accept_benign_writers and args.reserve_owned_graph, 'conditional ownership/graph policy must be accepted')
    t0 = mc.parse_deadline(args.activation_time) * cs.NS
    now, mono = time.time_ns(), time.monotonic_ns()
    cc.need(t0 <= now < t0 + 1800 * cs.NS and deadline * cs.NS == t0 + 15 * 3600 * cs.NS,
            'activation/cutover must be fixed T0 and T0+15h, with setup admission still open')
    effective = lambda: max(time.time_ns(), now + time.monotonic_ns() - mono)
    def read_budget():
        phase_end = t0 + 1800 * cs.NS - 2 * cs.NS
        seconds = min(20, (phase_end - effective()) / cs.NS)
        cc.need(seconds > 0, 'initial inventory phase budget exhausted')
        return cc.CallBudget(seconds, time.monotonic_ns() + int(seconds * cs.NS),
                             time.monotonic_ns, phase_end, effective)
    work = Path(mc.private_dir(args.work_dir)).absolute()
    ledger = Path(args.creation_ledger).absolute()
    cc.need(ledger.parent.parent == work and not ledger.exists(), 'fresh ledger must be in its own work-dir subdirectory')
    inventory = Path(args.inventory).absolute()
    mc.require_outside_repo(inventory, 'baseline inventory')
    baseline, inputs = {}, {}
    for kind in mc.PROTECT_KINDS:
        path = inventory / f'before-{kind}.json'
        raw = cc.private_bytes(path)
        rows = mc.parse_json_list(raw.decode(), 'sealed baseline')
        ids = [r.get('id') if isinstance(r, dict) else None for r in rows]
        cc.need(all(type(rid) is int and rid > 0 for rid in ids) and len(ids) == len(set(ids)), 'invalid baseline IDs')
        baseline[kind] = sorted(ids)
        inputs[str(path)] = cc.digest(raw)
        fresh = cloud.list(kind, read_budget)
        cc.need(sorted(r['id'] for r in fresh) == baseline[kind], 'fresh project census differs from sealed baseline')
        if kind == 'network':
            cc.need(all(not ipaddress.ip_network(n['ip_range']).overlaps(ipaddress.ip_network(args.ip_range)) for n in fresh),
                    'fresh existing network overlaps campaign range')
        cc.need(not any(r.get('name') in {p['name'] for p in plan} for r in fresh), 'planned name already exists')
    cc.need(isinstance(plan, GuardedPlan), 'guarded plan lacks captured private input evidence')
    for path, expected in plan.private_inputs.items():
        cc.need(cc.digest(cc.private_bytes(path)) == expected, 'pinned private campaign input changed during initialization')
        inputs[path] = expected
    guard_dir = work / 'guard'
    guard_dir.mkdir(mode=0o700, exist_ok=False)
    header = dict(campaign=args.campaign, prefix=args.prefix,
        boot_id=Path('/proc/sys/kernel/random/boot_id').read_text().strip(), sources=cs.source_pins(),
        private_inputs=inputs, baseline=baseline, context=profile.binding,
        protections=plan.protections,
        producer=cs.identity(), supervisor=cs.identity(args.supervisor_pid),
        guard_directory=cs.private_directory(guard_dir), threat_model=cs.THREAT_MODEL, graph_reserved=True,
        time=dict(t0_ns=t0, c_ns=t0 + 1800 * cs.NS, g_ns=t0 + 15 * 3600 * cs.NS,
                  h_ns=t0 + 16 * 3600 * cs.NS, setup_ns=t0 + 10800 * cs.NS,
                  monotonic_t0_ns=mono - (now - t0)),
        subnet=dict(type='cloud', network_zone=args.network_zone, ip_range=args.subnet_range),
        plan=[dict(kind=p['kind'], name=p['name'], descriptor=cs.descriptor(p['body']),
                   **({'private_ip': p['private_ip']} if 'private_ip' in p else {})) for p in plan])
    cc.need(cs.supervised(header['producer'], header['supervisor']), 'producer is not owned by selected supervisor')
    cc.need(not cc.contains_credential(header, profile._credential), 'controller credential found in sealed campaign inputs')
    state = dict(mode='OPEN', intents=[], owned=[], associations=[], events=[], guard=None, active_lease=None)
    return cl.Journal.initialize(ledger, header, state)


def run_guarded(args, deadline, out=sys.stdout):
    cc.need(not args.skip_shared and args.role == list(ROLE_NAMES), 'guarded creation is one registered seven-resource invocation')
    cc.need(args.client_type == args.server_type == 'ccx43' and args.location == 'fsn1'
            and str(args.image) == '161547269', 'guarded execution requires pinned ccx43/fsn1/image161547269')
    cc.need(0 < args.guard_wait_seconds <= 60, 'guard arming wait must be bounded to60s')
    profile = cc.Profile(args.context, args.hcloud_config)
    cloud = cc.Cloud(profile, profile.binding)
    plan = guarded_plan(args, deadline)
    cc.need(not cc.contains_credential(plan, profile._credential), 'controller credential found in deployment input')
    journal = initialize_guarded(args, deadline, profile, cloud, plan) if args.initialize_ledger else cl.Journal(args.creation_ledger)
    with contextlib.closing(journal):
        return execute_guarded(args, journal, profile, cloud, plan, out)


def execute_guarded(args, journal, profile, cloud, plan, out):
    campaign = cs.Campaign(journal, cloud)
    cc.need(profile.binding == campaign.header['context'], 'provisioning profile differs from journal')
    until = time.monotonic() + args.guard_wait_seconds
    try:
        while True:
            try:
                with journal.locked() as state:
                    campaign.validate_pins()
                    campaign._producer(state)
                cc.need(time.monotonic() < until, 'live supervised guard did not arm; no create permitted')
                break
            except (mc.GuardError, FileNotFoundError) as error:
                if (isinstance(error, FileNotFoundError)
                        and error.filename != str(Path(campaign.header['guard_directory']['path']) / 'heartbeat.json')):
                    raise
                cc.need(time.monotonic() < until, 'live supervised guard did not arm; no create permitted')
                time.sleep(0.1)
        ids = {}
        def wait_actions():
            while not campaign.poll_actions():
                state = journal.read()
                cc.need(not any(i['state'] == 'FAILED' or i.get('operation_failed') for i in state['intents']), 'provider action failed')
                with journal.locked() as current:
                    campaign._producer(current)
                    campaign.lease_seconds(current, provisioning=True)
                time.sleep(0.25)
        for entry in plan:
            body = json.loads(json.dumps(entry['body']))
            if entry['kind'] == 'server':
                body['ssh_keys'] = [ids[ssh_key_name(args)]]
                body['firewalls'] = [dict(firewall=ids[firewall_name(args)])]
            rid = campaign.create(entry['kind'], body)
            ids[entry['name']] = rid
            wait_actions()
            if entry['kind'] == 'network':
                campaign.mutate('network', rid, 'subnet', campaign.header['subnet'])
                wait_actions()
            if entry['kind'] == 'server':
                with journal.locked() as state:
                    item = campaign.describe_owned(state, 'server', rid, 'provision-cascade-readback')
                    if item is None:
                        raise mc.GuardError('owned server absent during provisioning')
                    campaign.cascade(state, item, cloud.list('primary-ip', lambda: campaign._phase_budget(state)),
                                     lambda: campaign._phase_budget(state))
                    journal.commit(state)
                campaign.mutate('server', rid, 'attach', dict(network=ids[args.network_name], ip=entry['private_ip']))
                wait_actions()
        for role in ROLE_NAMES:
            campaign.mutate('server', ids[server_name(args, role)], 'poweron', {})
            wait_actions()
        with journal.locked() as state:
            cc.need(len(state['owned']) == 7, 'incomplete owned resource population')
            state['events'].append(dict(event='provisioning-complete', unix_ns=campaign.clock()))
            journal.commit(state)
        hosts = {}
        for role in ROLE_NAMES:
            rid = ids[server_name(args, role)]
            item = campaign.read_owned('server', rid, 'provision-host-readback')
            if item is None:
                raise mc.GuardError('owned server absent during host publication')
            hosts[role] = dict(id=item['id'], name=item['name'], public_ipv4=item['public_net']['ipv4']['ip'],
                               private=[n['ip'] for n in item['private_net']], labels=item['labels'])
        cs.atomic_private(Path(args.work_dir) / 'hosts-all.json', hosts)
        print('guarded provisioning complete: seven durably owned resources', file=out)
        return dict(executed=True, hosts=hosts)
    except BaseException:
        campaign.request_stop()
        raise


def run(args, runner=mc.default_runner, now=None, out=sys.stdout):
    now = int(time.time()) if now is None else now
    deadline = mc.parse_deadline(args.teardown_deadline) if args.teardown_deadline else None
    if args.execute:
        deadline = check_execute_inputs(args, now)
        validate(args)
    if not args.execute:
        cmds, rules_path, rules = build_commands(args, deadline)
        print('# DRY RUN: nothing executed. Firewall rules file content for ' + str(rules_path) + ':', file=out)
        print('# ' + json.dumps(rules), file=out)
        for c in cmds:
            print(mc.quote(c), file=out)
        return dict(executed=False, commands=cmds, rules=rules)
    # Legacy command plans remain readable, but never authorize a mutation.
    cc.need(args.creation_ledger, '--execute requires --creation-ledger; selector-only execution is retired')
    return run_guarded(args, deadline, out=out)


def main(argv=None):
    try:
        run(parse_args(argv))
    except (mc.GuardError, OSError, ValueError, KeyError, TypeError) as exc:
        print('REFUSED: ' + (str(exc) if isinstance(exc, mc.GuardError) else type(exc).__name__), file=sys.stderr)
        return 2
    return 0


if __name__ == '__main__':
    sys.exit(main())
