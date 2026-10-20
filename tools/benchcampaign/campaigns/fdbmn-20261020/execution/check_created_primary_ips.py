#!/usr/bin/env python3
"""Read-only ownership/auto-delete check after ONE provision.py server invocation.

Inputs are fresh hcloud list snapshots. This file never invokes hcloud or changes
an IP; failures stop the operator before the next create. Record only IDs/names
and assignment ownership; never adopt protected resources.
"""
import argparse
import json
from pathlib import Path
import sys

C = Path(__file__).resolve().parents[1]


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument('--role', choices=('client', 'node1', 'node2', 'node3'), required=True)
    ap.add_argument('--server-json', type=Path, required=True)
    ap.add_argument('--primary-ip-json', type=Path, required=True)
    args = ap.parse_args()
    cfg = json.loads((C / 'campaign.json').read_text())
    state = json.loads((C / 'state.json').read_text())
    protected = {int(line.split('\t')[1]) for line in (C / 'inventory/protect.tsv').read_text().splitlines()}
    servers = json.loads(args.server_json.read_text())
    ips = json.loads(args.primary_ip_json.read_text())
    prefix = cfg['prefix']
    expected_roles = ('client', 'node1', 'node2', 'node3')[:('client', 'node1', 'node2', 'node3').index(args.role)+1]
    assert isinstance(servers, list) and isinstance(ips, list)
    assert {s['name'] for s in servers} == {prefix+'-'+r for r in expected_roles}, 'Unexpected campaign server set'
    owned_servers = {}
    for s in servers:
        assert s['labels'].get('campaign') == prefix and s['id'] not in protected, f'UNOWNED/PROTECTED SERVER {s.get("id")}'
        owned_servers[s['id']] = s
    assert set(state.get('campaign_server_ids', [])) <= set(owned_servers), 'Previously recorded campaign server disappeared'
    # Record newly observed explicit server IDs before checking IP policy, so a
    # failed check still leaves an exact ID for the owner/approved guard.
    state['campaign_server_ids'] = sorted(owned_servers)
    state['hosts'] = {r: dict(id=s['id'], name=s['name'],
                             public_ipv4=(s.get('public_net', {}).get('ipv4') or {}).get('ip'),
                             private=[n['ip'] for n in s.get('private_net', [])])
                      for r in expected_roles for s in servers if s['name'] == prefix+'-'+r}
    (C / 'state.json').write_text(json.dumps(state, indent=2)+'\n')
    by_id = {p['id']: p for p in ips}
    owned_ips = {}
    for sid, server in owned_servers.items():
        referenced = {v['id'] for family in ('ipv4', 'ipv6')
                      if (v := (server.get('public_net') or {}).get(family)) and v.get('id')}
        assigned = {p['id'] for p in ips if p.get('assignee_type') == 'server' and p.get('assignee_id') == sid}
        assert referenced and referenced == assigned, f'SERVER {sid}: referenced={sorted(referenced)} assigned={sorted(assigned)}'
        for rid in referenced:
            ip = by_id[rid]
            assert rid not in protected, f'PROTECTED PRIMARY IP {rid}'
            assert ip.get('assignee_type') == 'server' and ip.get('assignee_id') in owned_servers, f'PRIMARY IP {rid}: unowned assignment'
            assert ip.get('auto_delete') is True, f'PRIMARY IP {rid}: auto_delete is not true; OWNER EXACT-ID FIX REQUIRED'
            owned_ips[rid] = {'id': rid, 'server_id': sid, 'server_name': server['name'], 'auto_delete': True}
    assert set(state.get('campaign_primary_ip_ids', [])) <= set(owned_ips), 'Previously recorded campaign IP disappeared'
    state['campaign_primary_ip_ids'] = sorted(owned_ips)
    state['status'] = 'provisioned-through-'+args.role
    (C / 'campaign-owned-primary-ips.json').write_text(json.dumps({'campaign': prefix, 'server_ids': sorted(owned_servers),
          'primary_ips': [owned_ips[i] for i in sorted(owned_ips)], 'direct_primary_ip_deletion_authorized': False}, indent=2)+'\n')
    (C / 'state.json').write_text(json.dumps(state, indent=2)+'\n')
    print('PASS: campaign server IDs', sorted(owned_servers), '; primary-IP IDs', sorted(owned_ips), '; every IP assigned to an owned server and auto_delete=true.')


if __name__ == '__main__':
    try:
        main()
    except Exception as exc:
        print('STOP: primary-IP/ownership check failed. Report exact IDs to owner; do not continue or mutate IPs: '+str(exc), file=sys.stderr)
        sys.exit(2)
