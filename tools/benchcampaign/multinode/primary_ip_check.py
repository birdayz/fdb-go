#!/usr/bin/env python3
"""Fresh IP/cascade validation for exact journal-owned servers; never mutate an IP.

Historical selector/snapshot inputs are not ownership evidence. The checker reads
fresh authenticated provider metadata, authorizes the committed server IDs and
records complete allowed associations in the same creation journal. Known server
provenance never waives a failed cascade check or authorizes deleting that server.
"""
import argparse
import contextlib
import json
from pathlib import Path
import sys

import mn_common as mc
import campaign_cloud as cc
import campaign_safety as cs
import creation_ledger as cl

ROLES = ('client', 'node1', 'node2', 'node3')


def check(args):
    cc.need(args.creation_ledger, 'primary-IP validation requires --creation-ledger; selectors are not ownership')
    campaign = mc.parse_campaign_label(args.label)
    protected = {kind: set() for kind in mc.PROTECT_KINDS}
    protected_names = []
    for path in args.protect_file:
        policy = mc.read_protect_file(path, typed=True)
        protected_names.extend(''.join(f'[{c}]' if c in '*?[' else c for c in name) for name in policy['names'])
        for kind, ids in policy['by_kind'].items():
            protected[kind].update(ids)
    for path in (args.state, args.owned_output):
        mc.require_outside_repo(path, 'output')
    previous = json.loads(cc.private_bytes(args.state)) if Path(args.state).exists() else {}
    with contextlib.closing(cl.Journal(args.creation_ledger)) as journal:
        header = journal.header
        cc.need(campaign == header['campaign'] and args.prefix == header['prefix'], 'primary-IP campaign binding differs')
        profile = cc.Profile(header['context']['context'], header['context']['config'])
        cloud = cc.Cloud(profile, header['context'])
        checker = cs.Campaign(journal, cloud)
        checker.validate_pins()
        checker.bind_protections(cs.protection_policy(protected_names, by_kind={kind: sorted(ids) for kind, ids in protected.items()}))
        roles = ROLES[:ROLES.index(args.role) + 1]
        with journal.locked() as state:
            records = [r for r in state['owned'] if r['kind'] == 'server']
            cc.need({r['name'] for r in records} == {f'{args.prefix}-{r}' for r in roles},
                    'unexpected ledger-owned server set')
            reads = lambda: checker._phase_budget(state)
            servers = []
            for record in records:
                server = checker.describe_owned(state, 'server', record['id'], 'primary-check-exact-readback', reads)
                if server is None:
                    raise mc.GuardError('owned server absent or additionally protected')
                servers.append(server)
            ips = cloud.list('primary-ip', reads)
            checker._observe_inventory(state, 'primary-ip', ips, 'primary-check-census')
            for record, server in zip(records, servers):
                cc.need(server['id'] not in protected['server'], 'owned server absent or additionally protected')
                checker._authorize(state, 'server', server)
                cc.need(not any(ip['id'] in protected['primary-ip'] and ip.get('assignee_id') == server['id'] for ip in ips),
                        'additional primary-IP protection conflict')
                checker.cascade(state, server, ips, reads)
            server_ids = sorted(r['id'] for r in records)
            associations = [a for a in state['associations'] if a['server_id'] in server_ids]
            ip_ids = sorted(a['id'] for a in associations)
            cc.need(set(previous.get('campaign_server_ids', [])) <= set(server_ids)
                    and set(previous.get('campaign_primary_ip_ids', [])) <= set(ip_ids), 'previously recorded IDs disappeared')
            state['events'].append(dict(event='primary-ip-check', server_ids=server_ids, unix_ns=checker.clock()))
            journal.commit(state)
    previous.update(campaign_server_ids=server_ids, campaign_primary_ip_ids=ip_ids,
        status='provisioned-through-' + args.role,
        hosts={role: dict(id=s['id'], name=s['name'], public_ipv4=s['public_net']['ipv4']['ip'],
                         private=[n['ip'] for n in s['private_net']])
               for role in roles for s in servers if s['name'] == f'{args.prefix}-{role}'})
    owned = dict(campaign=campaign, server_ids=server_ids, primary_ips=associations,
                 direct_primary_ip_deletion_authorized=False)
    for path, value in [(args.state, previous), (args.owned_output, owned)]:
        mc.private_dir(Path(path).parent)
        cs.atomic_private(path, value)
    return server_ids, ip_ids


def parse_args(argv=None):
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument('--label', required=True, help='campaign=VALUE')
    ap.add_argument('--prefix', required=True)
    ap.add_argument('--role', choices=ROLES, required=True, help='the last role created')
    ap.add_argument('--creation-ledger', help='mandatory exact creation provenance')
    ap.add_argument('--server-json', help='historical snapshot option; never ownership authority')
    ap.add_argument('--primary-ip-json', help='historical snapshot option; never ownership authority')
    ap.add_argument('--protect-file', action='append', default=[])
    ap.add_argument('--state', required=True, help='campaign state JSON, outside the repo')
    ap.add_argument('--owned-output', required=True, help='owned server/primary-IP record, outside the repo')
    return ap.parse_args(argv)


def main(argv=None):
    args = parse_args(argv)
    try:
        servers, ips = check(args)
    except (OSError, ValueError, KeyError, TypeError, mc.GuardError) as exc:
        reason = str(exc) if isinstance(exc, mc.GuardError) else type(exc).__name__
        print('STOP: ownership check failed; retain IDs for the owner; never change IPs: ' + reason, file=sys.stderr)
        return 2
    print(f'PASS: exact journal-owned servers {servers}; attributed auto-delete primary IPs {ips}.')
    return 0


if __name__ == '__main__':
    sys.exit(main())
