#!/usr/bin/env python3
"""Ownership check after one staged `provision.py --role R` server create.

Reads fresh `hcloud server list -o json` and `hcloud primary-ip list -o json`
snapshots (it never calls hcloud and never changes an IP). Passes only if the
campaign's servers are exactly PREFIX-<role> for the roles created so far, each
carries the campaign label and no protected ID, and every public IP each server
references is a primary IP assigned to that server with auto_delete=true. Run it
before the next create; a failure stops provisioning for an owner decision.

Records the server and primary-IP IDs in --state (a JSON object, created if
absent) and writes --owned-output, so teardown can account for implicitly
created primary IPs that carry no campaign label. Exit 0 pass, 2 refused.
"""
import argparse
import json
from pathlib import Path
import sys

import mn_common as mc

ROLES = ('client', 'node1', 'node2', 'node3')


def check(args):
    campaign = mc.parse_campaign_label(args.label)
    protected = set()
    for path in args.protect_file:
        protected |= set(mc.read_protect_file(path)['ids'])
    for path in (args.state, args.owned_output):
        mc.require_outside_repo(path, 'output')
    state_path = Path(args.state)
    state = json.loads(state_path.read_text()) if state_path.exists() else {}
    servers = json.loads(Path(args.server_json).read_text())
    ips = json.loads(Path(args.primary_ip_json).read_text())
    if not isinstance(servers, list) or not isinstance(ips, list):
        raise mc.GuardError('server and primary-ip snapshots must be JSON lists')
    roles = ROLES[:ROLES.index(args.role) + 1]
    mine = [s for s in servers if mc.owned_name(s.get('name', ''), args.prefix)]
    if {s['name'] for s in mine} != {f'{args.prefix}-{r}' for r in roles}:
        raise mc.GuardError('unexpected campaign server set: ' + ', '.join(sorted(s['name'] for s in mine)))
    owned = {}
    for s in mine:
        if (s.get('labels') or {}).get('campaign') != campaign or s['id'] in protected:
            raise mc.GuardError(f'server {s["id"]} is unowned or protected')
        owned[s['id']] = s
    if not set(state.get('campaign_server_ids', [])) <= set(owned):
        raise mc.GuardError('a previously recorded campaign server disappeared')
    # Record the server IDs before the IP checks, so a failure still leaves exact IDs.
    state['campaign_server_ids'] = sorted(owned)
    state['hosts'] = {r: dict(id=s['id'], name=s['name'],
                              public_ipv4=((s.get('public_net') or {}).get('ipv4') or {}).get('ip'),
                              private=[n['ip'] for n in s.get('private_net', [])])
                      for r in roles for s in mine if s['name'] == f'{args.prefix}-{r}'}
    mc.write_private(state_path, json.dumps(state, indent=2) + '\n', exclusive=False)
    by_id = {p['id']: p for p in ips}
    owned_ips = {}
    for sid, server in owned.items():
        referenced = {v['id'] for family in ('ipv4', 'ipv6')
                      if (v := (server.get('public_net') or {}).get(family)) and v.get('id')}
        assigned = {p['id'] for p in ips if p.get('assignee_type') == 'server' and p.get('assignee_id') == sid}
        if not referenced or referenced != assigned:
            raise mc.GuardError(f'server {sid}: referenced IPs {sorted(referenced)} != assigned {sorted(assigned)}')
        for rid in referenced:
            ip = by_id[rid]
            if rid in protected:
                raise mc.GuardError(f'primary IP {rid} is protected')
            if ip.get('auto_delete') is not True:
                raise mc.GuardError(f'primary IP {rid}: auto_delete is not true')
            owned_ips[rid] = dict(id=rid, server_id=sid, server_name=server['name'], auto_delete=True)
    if not set(state.get('campaign_primary_ip_ids', [])) <= set(owned_ips):
        raise mc.GuardError('a previously recorded campaign primary IP disappeared')
    state['campaign_primary_ip_ids'] = sorted(owned_ips)
    state['status'] = 'provisioned-through-' + args.role
    mc.write_private(Path(args.owned_output), json.dumps(dict(
        campaign=campaign, server_ids=sorted(owned), primary_ips=[owned_ips[i] for i in sorted(owned_ips)],
        direct_primary_ip_deletion_authorized=False), indent=2) + '\n', exclusive=False)
    mc.write_private(state_path, json.dumps(state, indent=2) + '\n', exclusive=False)
    return sorted(owned), sorted(owned_ips)


def parse_args(argv=None):
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument('--label', required=True, help='campaign=VALUE')
    ap.add_argument('--prefix', required=True)
    ap.add_argument('--role', choices=ROLES, required=True, help='the last role created')
    ap.add_argument('--server-json', required=True)
    ap.add_argument('--primary-ip-json', required=True)
    ap.add_argument('--protect-file', action='append', default=[])
    ap.add_argument('--state', required=True, help='campaign state JSON, outside the repo')
    ap.add_argument('--owned-output', required=True, help='owned server/primary-IP record, outside the repo')
    return ap.parse_args(argv)


def main(argv=None):
    args = parse_args(argv)
    try:
        servers, ips = check(args)
    except (OSError, ValueError, KeyError, mc.GuardError) as exc:
        print(f'STOP: ownership check failed; report the IDs to the owner, do not continue or change IPs: {exc}',
              file=sys.stderr)
        return 2
    print(f'PASS: campaign servers {servers}; primary IPs {ips}; each assigned to an owned server with auto_delete=true.')
    return 0


if __name__ == '__main__':
    sys.exit(main())
