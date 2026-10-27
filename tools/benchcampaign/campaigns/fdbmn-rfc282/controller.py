#!/usr/bin/env python3
"""Extend the frozen ops60 controller with bounded cgroup CPU sidecars only."""
import argparse
import ipaddress
import json
import os
from pathlib import Path
import re
import shlex
import subprocess
import sys

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE.parent / 'fdbmn-20261020' / 'execution'))
sys.path.insert(0, str(HERE.parent.parent / 'multinode'))
import run_phase_ops60 as legacy
import remote_ops60
import mn_common
import server_cpu


class CampaignSSH:
    """Same pinned-host-key/created-ID boundary, with an explicit campaign root."""
    def __init__(self, campaign):
        self.base = Path(campaign)

    def __call__(self, role, command, **kwargs):
        state = json.loads((self.base / 'state.json').read_text())
        config = json.loads((self.base / 'campaign.json').read_text())
        legacy.need(role in ('client', *legacy.NODES), 'unknown campaign role')
        host = state['hosts'][role]
        protected = {int(line.split('\t')[1]) for line in (self.base / 'inventory/protect.tsv').read_text().splitlines()}
        legacy.need(host['id'] in state['campaign_server_ids'] and host['id'] not in protected
                    and host['name'] == config['prefix'] + '-' + role, 'unowned/protected SSH target')
        ipaddress.ip_address(host['public_ipv4'])
        argv = ['ssh', '-i', state['ssh_private_key'], '-o', 'IdentitiesOnly=yes', '-o', 'BatchMode=yes',
                '-o', 'ControlMaster=auto', '-o', 'ControlPersist=120s',
                '-o', 'ControlPath=' + str(self.base / 'private/cm/%h-%p'), '-o', 'ConnectTimeout=10',
                '-o', 'StrictHostKeyChecking=yes', '-o', 'UserKnownHostsFile=' + str(self.base / 'private/cloud-init/known_hosts'),
                '-o', 'HostKeyAlias=' + host['name'], 'root@' + host['public_ipv4'], command]
        return subprocess.run(argv, **kwargs)


class Controller(legacy.Controller):
    def __init__(self, args, **kwargs):
        base = Path(args.campaign_dir).resolve(strict=True)
        # This controller writes private receipts; never turn a checkout into a campaign.
        mn_common.require_outside_repo(base, 'campaign directory')
        super().__init__(args, base=base, transport=kwargs.pop('transport', None) or CampaignSSH(base), **kwargs)
        self.cpu_unit = legacy.unit_name('cpu', self.label)
        self.cpu_telemetry = legacy.R + '/telemetry-cpu/' + self.label
        self.cpu_starts = {}
        self.cpu_intended = []

    def preflight(self):
        super().preflight()
        protocol = self.read_json('client', self.a.protocol)
        policy = protocol['measurement_extensions']
        legacy.need(policy['server_cpu_source'] == 'exclusive-cgroup-v2-usage-usec'
                    and policy['process_cpu_interval_ns'] == 10_000_000
                    and policy['process_cpu_min_inner_intervals'] == 10
                    and policy['process_cpu_max_time_envelope_ratio'] == 1.2
                    and policy['process_cpu_clock_reserve_ns'] == server_cpu.CLOCK_RESERVE_NS
                    and policy['process_cpu_phase_correlation_range_ns'] == server_cpu.MAX_PHASE_CLOCK_RANGE_NS,
                    'unsealed CPU source/policy')
        self.cpu_units = server_cpu.unit_population(self.read_json('client', legacy.LAYOUT))
        paths = {
            'tools/benchcampaign/campaigns/fdbmn-rfc282/controller.py': Path(__file__),
            'tools/benchcampaign/campaigns/fdbmn-rfc282/server_cpu.py': Path(server_cpu.__file__),
            'tools/benchcampaign/campaigns/fdbmn-20261020/execution/run_phase_ops60.py': Path(legacy.__file__),
            'tools/benchcampaign/campaigns/fdbmn-20261020/execution/remote_ops60.py': Path(remote_ops60.__file__),
        }
        for relative, path in paths.items():
            remote = legacy.R + '/tooling/' + relative
            legacy.need(self.pins.get(remote) == legacy.sha(path.read_bytes()), 'unsealed controller/helper: ' + relative)
        self.dump('cpu-extension-pins.json', {p: legacy.sha(f.read_bytes()) for p, f in paths.items()})
        for node in legacy.NODES:
            self.require_fresh_unit(node, self.cpu_unit)
            self.call(node, 'test ! -e ' + legacy.q(self.cpu_telemetry), check=True, timeout=30)
        self.call('client', 'test ! -e ' + legacy.q(self.cpu_telemetry) + ' && mkdir -p ' + legacy.q(self.cpu_telemetry),
                  check=True, timeout=30)
        self.require_phase_budget('after CPU-sidecar preflight')

    def cpu_receipt(self, node, kind):
        receipt = self.read_json(node, self.cpu_telemetry + '/receipt-' + kind + '.json')
        legacy.need(receipt.get('format') == legacy.RECEIPT_FORMAT and receipt.get('host') == node
                    and receipt.get('phase') == self.a.phase and receipt.get('kind') == kind
                    and receipt.get('sampler_sha256') == self.sampler_hash, 'foreign/unpinned CPU receipt')
        legacy.need(receipt.get('process_cpu_format') == server_cpu.FORMAT
                    and receipt.get('cgroup_cpu_format') == server_cpu.CGROUP_FORMAT
                    and set(receipt.get('cgroup_units', {})) == self.cpu_units[node], 'missing CPU cgroup population')
        mount = receipt.get('cgroup_v2', {})
        legacy.need(mount.get('path') == '/sys/fs/cgroup' and 'cpu' in mount.get('controllers', []), 'unsupported CPU cgroup mount')
        baseline = json.loads((self.folder / (node + '-receipt-' + kind + '.json')).read_text())
        legacy.need(receipt.get('boot_id') == baseline.get('boot_id')
                    and receipt['units']['starttimes'] == baseline['units']['starttimes'], 'R3/CPU process identity differs')
        identities = []
        for unit, group in receipt['cgroup_units'].items():
            pid = receipt['units']['starttimes'][unit]['pid']
            legacy.need(group['path'] == '/sys/fs/cgroup/system.slice/' + unit and group['pid'] == pid
                        and group['starttime'] == receipt['units']['starttimes'][unit]['starttime']
                        and group['procs'] == [pid] and pid in group['threads'], 'unowned CPU group membership')
            identities.append((group['device'], group['inode']))
        legacy.need(len(set(identities)) == len(self.cpu_units[node]), 'shared CPU group double count')
        if kind == 'start':
            self.cpu_starts[node] = receipt
        else:
            legacy.need(type(receipt.get('samples')) is int and receipt['samples'] > 1
                        and re.fullmatch('[0-9a-f]{64}', receipt.get('samples_sha256', ''))
                        and receipt['cgroup_units'] == self.cpu_starts[node]['cgroup_units']
                        and receipt['cgroup_v2'] == self.cpu_starts[node]['cgroup_v2'], 'CPU stop identity/population changed')
        self.dump(node + '-cpu-receipt-' + kind + '.json', receipt)
        return receipt

    def start_samplers(self):
        super().start_samplers()  # unchanged original 1Hz R3 and readiness proof
        self.cpu_intended = [(node, self.cpu_unit) for node in legacy.NODES]
        self.register_intents(self.cpu_intended)  # before first dispatch, including lost SSH acknowledgement
        for node in legacy.NODES:
            argv = ['systemd-run', '--unit=' + self.cpu_unit, '--property=Type=exec', '--property=CPUAffinity=0-1',
                    '--property=KillMode=control-group', '--property=TimeoutStopSec=150s',
                    '--property=RuntimeMaxSec=' + str(self.cap + 900) + 's', 'python3', '-B', legacy.T + '/node_sampler.py',
                    'run', '--host', node, '--phase', self.a.phase, '--output-dir', self.cpu_telemetry,
                    '--interval', '0.01', '--cgroup-cpu', '--max-seconds', str(self.cap + 900)]
            for unit in sorted(self.cpu_units[node]):
                argv += ['--unit', unit]
            self.call(node, shlex.join(argv), check=True, timeout=60)
        for node in legacy.NODES:
            end = self.clock() + 90
            while True:
                reply = self.call(node, 'test -f ' + legacy.q(self.cpu_telemetry + '/receipt-start.json'),
                                  capture_output=True, text=True, timeout=15)
                if reply.returncode == 0:
                    self.cpu_receipt(node, 'start')
                    status = self.unit_status(node, self.cpu_unit)
                    legacy.need(status.get('ActiveState') == 'active' and status.get('SubState') == 'running'
                                and status.get('MainPID', '0') != '0', 'CPU sampler died after start')
                    break
                legacy.need(self.clock() < end, node + ': CPU sampler never ready')
                self.sleep(.5)
        self.require_phase_budget('after CPU sampler readiness')

    def collect(self):
        # The historical collector assumes every server intent is its one R3
        # unit. Restrict its view, then restore the complete durable intent list.
        all_intents = self.intents
        self.intents = [i for i in all_intents if i['unit'] == self.sampler_unit]
        try:
            super().collect()
        except Exception as error:
            self.errors.append('original R3 collection: ' + repr(error))
        finally:
            self.intents = all_intents
        for node, unit in self.cpu_intended:
            if not self.stop_and_prove(node, unit, 180):
                continue  # bounded owned unit remains a controller failure, never assumed stopped
            try:
                self.cpu_receipt(node, 'stop')
            except Exception as error:
                self.errors.append(node + '-CPU-stop-receipt: ' + repr(error))
            try:
                data = self.pull(node, self.cpu_telemetry, self.base / 'telemetry-cpu' / self.label / node)
                target = self.cpu_telemetry + '/' + node
                self.call('client', 'test ! -e ' + legacy.q(target) + ' && mkdir -p ' + legacy.q(target) +
                          ' && tar -C ' + legacy.q(target) + ' -xzf -', input=data, check=True, timeout=120)
            except Exception as error:
                self.errors.append(node + '-CPU-collection: ' + repr(error))
        self.dump('cpu-extension-collection.json', dict(nodes=sorted(self.cpu_starts), intents=self.cpu_intended,
                                                       errors=list(self.errors)))


def main(argv=None):
    os.umask(0o077)
    ap = argparse.ArgumentParser(description=__doc__)
    for name in ('campaign-dir', 'phase', 'manifest', 'manifest-sha256', 'protocol', 'orders', 'results'):
        ap.add_argument('--' + name, required=True)
    ap.add_argument('--label')
    ap.add_argument('--placement-template', default=legacy.R + '/provenance/p0/placement-command.json')
    ap.add_argument('--diagnostic', action='store_true')
    args = ap.parse_args(argv)
    try:
        return Controller(args).execute()
    except (legacy.Refused, mn_common.GuardError, OSError, ValueError, KeyError) as error:
        print('REFUSED/FAILED: ' + str(error), file=sys.stderr)
        return 2


if __name__ == '__main__':
    raise SystemExit(main())
