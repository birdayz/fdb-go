#!/usr/bin/env python3
"""SSH only to recorded campaign servers, with the pre-generated pinned host key."""
import argparse
import ipaddress
import json
from pathlib import Path
import subprocess
import sys

C = Path(__file__).resolve().parents[1]
R = '/srv/fdb-bench'


def ssh(role):
    state = json.loads((C/'state.json').read_text())
    cfg = json.loads((C/'campaign.json').read_text())
    assert role in ('client','node1','node2','node3')
    host = state['hosts'][role]
    protected = {int(l.split('\t')[1]) for l in (C/'inventory/protect.tsv').read_text().splitlines()}
    assert host['id'] in state['campaign_server_ids'] and host['id'] not in protected
    assert host['name'] == cfg['prefix']+'-'+role
    ipaddress.ip_address(host['public_ipv4'])
    return ['ssh','-i',state['ssh_private_key'],'-o','IdentitiesOnly=yes','-o','BatchMode=yes',
            '-o','ControlMaster=auto','-o','ControlPersist=120s',
            '-o','ControlPath='+str(C/'private/cm/%h-%p'),
            '-o','ConnectTimeout=10','-o','StrictHostKeyChecking=yes',
            '-o','UserKnownHostsFile='+str(C/'private/cloud-init/known_hosts'),
            '-o','HostKeyAlias='+host['name'],'root@'+host['public_ipv4']]


def run(role, command, **kwargs):
    return subprocess.run([*ssh(role),command], **kwargs)


def main():
    p=argparse.ArgumentParser();p.add_argument('--role',required=True);p.add_argument('--script',type=Path);p.add_argument('command',nargs='?')
    args=p.parse_args()
    if args.script:
        return run(args.role,'bash -s',input=args.script.read_bytes()).returncode
    return run(args.role,args.command or 'true').returncode


if __name__=='__main__':
    sys.exit(main())
