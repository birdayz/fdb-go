#!/usr/bin/env python3
"""Controller: one sealed phase, bounded owned units, collection and mandatory R4."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import shlex
import subprocess
import sys
import time

from remote_ops60 import C, R, run

T = R + '/tooling/tools/benchcampaign/multinode'
S = R + '/frozen/tools/clientbench/scripts'
CF = R + '/cluster-ssd/fdb.cluster'
LAYOUT = R + '/cluster-ssd/layout.json'
NODES = ('node1', 'node2', 'node3')
RECEIPT_FORMAT = 'fdb-bench-node-receipt/v1'
HASH_PROGRAM = '''# fdb-controller-hash-v1
import hashlib,json,pathlib,sys
expected=json.loads(sys.argv[1]); actual={}; errors=[]
for name,digest in expected.items():
 try:
  p=pathlib.Path(name)
  if not p.is_absolute() or not p.is_file(): raise ValueError('not an absolute regular file')
  with p.open('rb') as stream: h=hashlib.file_digest(stream,'sha256').hexdigest()
  actual[name]=h
  if h!=digest: errors.append(name+': hash mismatch')
 except Exception as exc: errors.append(name+': '+str(exc))
print(json.dumps({'actual':actual,'errors':errors},sort_keys=True))
sys.exit(2 if errors else 0)
'''
CGROUP_PROGRAM = '''# fdb-controller-cgroup-v1
import json,pathlib,sys
root=pathlib.Path('/sys/fs/cgroup')
assert (root/'cgroup.controllers').is_file(), 'cannot prove quiescence without cgroup v2'
p=root/sys.argv[1].lstrip('/')
r={'exists':p.exists(),'pids':[],'populated':False}
if p.exists():
 events=dict(line.split() for line in (p/'cgroup.events').read_text().splitlines())
 r['populated']=events['populated']=='1'
 r['pids']=sorted({int(v) for f in p.rglob('cgroup.procs') for v in f.read_text().split()})
print(json.dumps(r,sort_keys=True))
'''
CLIENT_DRIVER = '''import json,pathlib,subprocess,sys
sys.path.insert(0,sys.argv[1]);import seal_multinode as sm
manifest=json.loads(pathlib.Path(sys.argv[2]).read_text())
items=[c for c in manifest['campaigns'] if c['phase']==sys.argv[3]]
assert items
for c in items:
 print('RUN',c['phase'],c['workload'],flush=True)
 argv=['taskset','-c','0-1',*sm.render_run(c['run'],sys.argv[4],sys.argv[5])]
 rc=subprocess.run(argv).returncode
 print('RETURN',c['phase'],c['workload'],rc,flush=True)
 if rc:sys.exit(rc)
'''


class Refused(RuntimeError):
    pass


def need(condition, message):
    if not condition:
        raise Refused(message)


def q(value):
    return shlex.quote(str(value))


def sha(raw):
    return hashlib.sha256(raw).hexdigest()


def unit_name(kind, label):
    return 'fdb-mn-' + kind + '-' + label + '.service'


class Controller:
    def __init__(self, args, *, base=C, transport=None, clock=time.monotonic, sleep=time.sleep):
        self.a, self.base = args, Path(base)
        self.transport = transport or run
        self.clock, self.sleep = clock, sleep
        self.label = args.label or args.phase
        need(re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9_-]{0,79}', self.label) is not None, 'unsafe/fresh label required')
        self.folder = self.base / ('diagnostic-controller' if args.diagnostic else 'phase-controller') / self.label
        self.folder.mkdir(parents=True, exist_ok=False)
        self.telemetry = R + '/telemetry/' + self.label
        self.reconcile_dir = R + '/reconcile/' + self.label
        self.operations, self.errors, self.intents = [], [], []
        self.runner_unit = unit_name('runner', self.label)
        self.sampler_unit = unit_name('sampler', self.label)
        self.runner_intended = False
        self.runner_rc = self.reconcile_rc = None
        self.runner_status = self.runner_final_status = None
        self.runner_quiescent = True
        self.expected_cases = []
        self.sampler_hash = None
        self.phase_summary = None
        state = json.loads((self.base / 'state.json').read_text())
        self.hard_deadline = state['deadline_epoch']
        self.deadline = state.get('runner_lifetime_deadline_epoch', self.hard_deadline)

    def dump(self, name, value):
        with (self.folder / name).open('x') as stream:
            json.dump(value, stream, indent=2, sort_keys=True)
            stream.write('\n')

    def call(self, role, command, **kwargs):
        entry = dict(role=role, command=command, started_epoch=time.time())
        self.operations.append(entry)
        start = self.clock()
        try:
            result = self.transport(role, command, **kwargs)
            entry['returncode'] = result.returncode
            if isinstance(result.stdout, str):
                entry['stdout'] = result.stdout
            if isinstance(result.stderr, str):
                entry['stderr'] = result.stderr
            need(result.returncode != 255, role + ': SSH transport exit 255 (outcome unknown)')
            return result
        except Exception as exc:
            entry['error'] = repr(exc)
            entry['returncode'] = getattr(exc, 'returncode', None)
            for field in ('stdout', 'stderr'):
                value = getattr(exc, field, None)
                if isinstance(value, (str, bytes)):
                    entry[field] = value.decode(errors='replace') if isinstance(value, bytes) else value
            raise
        finally:
            entry['duration_seconds'] = self.clock() - start

    def text(self, role, command, timeout=60):
        return self.call(role, command, capture_output=True, text=True, check=True, timeout=timeout).stdout

    def read_json(self, role, path):
        return json.loads(self.text(role, 'cat ' + q(path)))

    def verify_hashes(self, role, expected, label):
        need(bool(expected), label + ': empty hash population')
        raw = self.text(role, shlex.join(['taskset', '-c', '0-1', 'python3', '-B', '-c',
                                         HASH_PROGRAM, json.dumps(expected, sort_keys=True)]), timeout=300)
        report = json.loads(raw)
        need(not report['errors'] and report['actual'] == expected, label + ': pinned input mismatch')
        self.dump(label + '.json', report)

    def unit_status(self, role, unit):
        props = 'LoadState,ActiveState,SubState,MainPID,ExecMainStatus,ExecMainCode,Result,ControlGroup'
        result = self.call(role, shlex.join(['systemctl', 'show', '--no-pager', '--property=' + props, unit]),
                           capture_output=True, text=True, timeout=30)
        fields = dict(line.split('=', 1) for line in result.stdout.splitlines() if '=' in line)
        need(fields.get('LoadState') in ('loaded', 'not-found'), role + ': unreadable exact unit state: ' + unit)
        if fields['LoadState'] != 'not-found':
            need(result.returncode == 0 and all(k in fields for k in
                 ('ActiveState', 'SubState', 'MainPID', 'ExecMainStatus', 'ExecMainCode', 'Result', 'ControlGroup')),
                 role + ': incomplete exact unit state: ' + unit)
        return fields

    def cgroup_empty(self, role, unit, status):
        expected = '/system.slice/' + unit
        group = status.get('ControlGroup') or expected
        need(group == expected, role + ': unexpected owned unit cgroup: ' + group)
        report = json.loads(self.text(role, shlex.join(['python3', '-B', '-c', CGROUP_PROGRAM, group]), timeout=30))
        need(type(report.get('exists')) is bool and type(report.get('populated')) is bool and
             isinstance(report.get('pids'), list), role + ': malformed cgroup proof')
        return not report['populated'] and report['pids'] == []

    def quiescent(self, role, unit):
        status = self.unit_status(role, unit)
        inactive = status['LoadState'] == 'not-found' or (
            status['ActiveState'] in ('inactive', 'failed') and status['MainPID'] == '0')
        return inactive and self.cgroup_empty(role, unit, status)

    def require_fresh_unit(self, role, unit):
        status = self.unit_status(role, unit)
        need(status['LoadState'] == 'not-found' and self.cgroup_empty(role, unit, status),
             role + ': unit/cgroup already exists; never adopt: ' + unit)

    def stop_and_prove(self, role, unit, seconds):
        end = self.clock() + seconds
        try:
            result = self.call(role, shlex.join(['systemctl', 'stop', '--no-block', unit]),
                               capture_output=True, text=True, timeout=30)
            if result.returncode:
                self.errors.append(role + ': stop returned ' + str(result.returncode) + ': ' + unit)
        except Exception as exc:
            self.errors.append(role + ': stop acknowledgement unknown: ' + repr(exc))
        last = None
        while self.clock() < end:
            try:
                if self.quiescent(role, unit):
                    return True
            except Exception as exc:
                last = repr(exc)
            self.sleep(min(10, max(0, end - self.clock())))
        self.errors.append(role + ': cannot prove unit/cgroup quiescence: ' + unit + ': ' + str(last))
        return False

    def register_intents(self, entries):
        self.intents.extend(dict(role=role, unit=unit) for role, unit in entries)
        # One exclusive file for each intent boundary, before the first dispatch.
        self.dump('intents-' + str(len(self.intents)) + '.json', self.intents)

    def validate_receipt(self, role, kind):
        path = self.telemetry + ('/client-' + kind + '.json' if role == 'client' else '/receipt-' + kind + '.json')
        receipt = self.read_json(role, path)
        need(receipt.get('format') == RECEIPT_FORMAT and receipt.get('host') == role and
             receipt.get('phase') == self.a.phase and receipt.get('kind') == kind,
             role + ': wrong receipt host/phase/kind: ' + kind)
        need(self.sampler_hash and receipt.get('sampler_sha256') == self.sampler_hash,
             role + ': receipt sampler hash differs from acknowledged seal: ' + kind)
        need(receipt.get('boot_id') and type(receipt.get('realtime_ns')) is int and receipt['realtime_ns'] > 0 and
             type(receipt.get('monotonic_ns')) is int and receipt['monotonic_ns'] > 0,
             role + ': empty receipt identity/clocks: ' + kind)
        if role != 'client':
            need(isinstance(receipt.get('units'), dict) and bool(receipt['units'].get('names')),
                 role + ': empty unit receipt: ' + kind)
            if kind == 'stop':
                need(type(receipt.get('samples')) is int and receipt['samples'] > 0 and
                     re.fullmatch('[0-9a-f]{64}', receipt.get('samples_sha256', '')) is not None,
                     role + ': empty/unsealed telemetry population')
        self.dump(role + '-receipt-' + kind + '.json', receipt)

    def client_receipt(self, kind):
        argv = ['taskset', '-c', '0-1', 'python3', '-B', T + '/node_sampler.py', 'receipt', '--host', 'client',
                '--phase', self.a.phase, '--kind', kind, '--no-units', '--cluster-file', CF,
                '--output', self.telemetry + '/client-' + kind + '.json']
        self.call('client', shlex.join(argv), check=True, timeout=60)
        self.validate_receipt('client', kind)

    def require_phase_budget(self, stage):
        need(type(self.cap) is int and self.cap > 0 and type(self.deadline) is int and
             type(self.hard_deadline) is int and self.deadline <= self.hard_deadline,
             'invalid phase cap/runner deadline/hard deadline')
        need(self.deadline - time.time() >= self.cap + 900,
             stage + ': insufficient lifetime: no partial phase')

    def preflight(self):
        a = self.a
        raw = self.text('client', 'cat ' + q(a.manifest)).encode()
        need(re.fullmatch('[0-9a-f]{64}', a.manifest_sha256) is not None and sha(raw) == a.manifest_sha256,
             'acknowledged manifest changed')
        self.manifest = json.loads(raw)
        self.pins = self.manifest.get('pinned_files')
        need(isinstance(self.pins, dict) and bool(self.pins) and all(
             Path(p).is_absolute() and isinstance(h, str) and re.fullmatch('[0-9a-f]{64}', h)
             for p, h in self.pins.items()), 'empty/malformed manifest pin population')
        self.verify_hashes('client', self.pins, 'client-pins-before-launch')
        need(a.protocol in self.pins and a.orders in self.pins, 'protocol/orders not pinned')
        protocol_raw = self.text('client', 'cat ' + q(a.protocol)).encode()
        need(sha(protocol_raw) == self.pins[a.protocol], 'protocol is not the sealed one')
        proto = json.loads(protocol_raw)
        self.phase = next(p for p in proto['phases'] if p['id'] == a.phase)
        need(self.phase['kind'] in ('client', 'testingb'), 'unknown phase kind')
        self.cap = self.phase['wall_cap_seconds']
        self.require_phase_budget('preflight')
        if self.phase['kind'] == 'client':
            self.expected_cases = [c['workload'] for c in self.manifest['campaigns'] if c['phase'] == a.phase]
        else:
            entry = next(p for p in self.manifest['phases'] if p['phase'] == a.phase)
            path = str(Path(a.manifest).parent / (a.phase + '.plan.json'))
            plan_raw = self.text('client', 'cat ' + q(path)).encode()
            need(sha(plan_raw) == entry['plan_sha256'], 'testing.B plan hash mismatch')
            self.expected_cases = [c['key'] for c in json.loads(plan_raw)['cases']]
        need(self.expected_cases and len(set(self.expected_cases)) == len(self.expected_cases), 'empty/duplicate case population')
        server_paths = (T + '/node_sampler.py', T + '/mn_common.py', CF, LAYOUT)
        need(all(p in self.pins for p in server_paths), 'missing required server code/cluster/layout pins')
        self.sampler_hash = self.pins[T + '/node_sampler.py']
        for node in NODES:
            self.verify_hashes(node, {p: self.pins[p] for p in server_paths}, node + '-pins-before-r3')
            self.require_fresh_unit(node, self.sampler_unit)
        self.require_fresh_unit('client', self.runner_unit)
        with (self.folder / 'manifest.json').open('xb') as stream:
            stream.write(raw)
        self.dump('inputs.json', vars(a) | {'deadline_epoch': self.deadline,
                  'runner_lifetime_deadline_epoch': self.deadline, 'hard_deadline_epoch': self.hard_deadline,
                  'expected_cases': self.expected_cases, 'controller_sha256': sha(Path(__file__).read_bytes())})
        self.require_phase_budget('preflight after pin/unit checks')
        self.call('client', 'test ! -e ' + q(self.telemetry) + ' && mkdir -p ' + q(self.telemetry), check=True, timeout=30)

    def start_samplers(self):
        self.register_intents([(node, self.sampler_unit) for node in NODES])
        for node in NODES:
            argv = ['systemd-run', '--unit=' + self.sampler_unit, '--property=Type=exec',
                    '--property=CPUAffinity=0-1', '--property=KillMode=control-group',
                    '--property=TimeoutStopSec=150s', '--property=RuntimeMaxSec=' + str(self.cap + 900) + 's',
                    'python3', '-B', T + '/node_sampler.py', 'run', '--host', node, '--phase', self.a.phase,
                    '--cluster-file', CF, '--output-dir', self.telemetry, '--max-seconds', str(self.cap + 900)]
            self.call(node, shlex.join(argv), check=True, timeout=60)
        for node in NODES:
            end = self.clock() + 90
            while True:
                result = self.call(node, 'test -f ' + q(self.telemetry + '/receipt-start.json'),
                                   capture_output=True, text=True, timeout=15)
                if result.returncode == 0:
                    self.validate_receipt(node, 'start')
                    state = self.unit_status(node, self.sampler_unit)
                    need(state.get('ActiveState') == 'active' and state.get('SubState') == 'running' and
                         state.get('MainPID', '0') != '0', node + ': sampler died after start receipt')
                    break
                need(self.clock() < end, node + ': sampler never became ready')
                self.sleep(.5)

    def runner_command(self):
        a = self.a
        if self.phase['kind'] == 'client':
            return shlex.join(['python3', '-u', '-B', '-c', CLIENT_DRIVER, T, a.manifest, a.phase,
                               a.manifest_sha256, str(self.deadline)])
        template_raw = self.text('client', 'cat ' + q(a.placement_template)).encode()
        need(a.placement_template in self.pins and sha(template_raw) == self.pins[a.placement_template],
             'placement template is not the acknowledged pinned file')
        return shlex.join(['taskset', '-c', '0-1', 'python3', '-B', T + '/bench_runner_mn.py', 'run',
            '--manifest', a.manifest, '--manifest-sha256', a.manifest_sha256, '--phase', a.phase,
            '--protocol', a.protocol, '--orders', a.orders, '--layout', LAYOUT, '--vpc-cidr', '10.89.20.0/24',
            '--scripts', S, '--source-receipt', R + '/provenance/source-receipt.json',
            '--placement-command', template_raw.decode().strip(), '--output', a.results,
            '--lifetime-deadline', str(self.deadline)])

    def execute_runner(self):
        command = self.runner_command()
        self.require_phase_budget('runner launch after sampler readiness')
        self.dump('runner-command.json', {'command': command})
        self.register_intents([('client', self.runner_unit)])
        self.runner_intended, self.runner_quiescent = True, False
        argv = ['systemd-run', '--unit=' + self.runner_unit, '--property=Type=exec',
                '--property=RemainAfterExit=yes', '--property=RuntimeMaxSec=' + str(self.cap + 300) + 's',
                '--property=KillMode=control-group', '--property=TimeoutStopSec=30s', '--property=CPUAffinity=0-1',
                '--property=StandardOutput=journal', '--property=StandardError=journal', '/bin/sh', '-c', command]
        self.call('client', shlex.join(argv), check=True, timeout=60)
        end = self.clock() + self.cap + 360
        while self.clock() < end:
            status = self.unit_status('client', self.runner_unit)
            need(status['LoadState'] != 'not-found', 'runner unit vanished without an exit receipt')
            self.runner_status = status
            terminal = status['ActiveState'] in ('inactive', 'failed') or (
                status['ActiveState'] == 'active' and status['SubState'] == 'exited')
            if terminal and status['MainPID'] == '0':
                code, value = status['ExecMainCode'], status['ExecMainStatus']
                need(code in ('1', '2', '3', 'exited', 'killed', 'dumped') and value.isdecimal(), 'runner exit status unknown')
                self.runner_rc = int(value) if code in ('1', 'exited') else 128 + int(value)
                need(status['Result'] == 'success' or self.runner_rc != 0, 'failed runner reported exit zero')
                self.dump('runner-exit.json', status | {'returncode': self.runner_rc})
                return
            self.sleep(60)
        raise Refused('runner exceeded bounded unit lifetime without a terminal status')

    def pull(self, role, remote_dir, local_dir):
        local_dir.mkdir(parents=True, exist_ok=False)
        data = self.call(role, 'tar -C ' + q(remote_dir) + ' -czf - .', capture_output=True, check=True, timeout=300).stdout
        with local_dir.with_name(local_dir.name + '.tar.gz').open('xb') as stream:
            stream.write(data)
        subprocess.run(['tar', '-C', str(local_dir), '-xzf', '-'], input=data, check=True, timeout=180)
        return data

    def finish_runner(self):
        if not self.runner_intended:
            return True
        self.runner_quiescent = self.stop_and_prove('client', self.runner_unit, 90)
        if self.runner_quiescent:
            try:
                self.runner_final_status = self.unit_status('client', self.runner_unit)
                with (self.folder / 'runner-journal.log').open('xb') as stream:
                    self.call('client', shlex.join(['journalctl', '--no-pager', '--unit=' + self.runner_unit,
                              '--output=short-iso-precise']), stdout=stream, stderr=stream, check=True, timeout=120)
            except Exception as exc:
                self.errors.append('runner terminal/journal collection: ' + repr(exc))
        return self.runner_quiescent

    def collect(self):
        try:
            self.client_receipt('after')
        except Exception as exc:
            self.errors.append('client-after: ' + repr(exc))
        for intent in self.intents:
            node = intent['role']
            if node not in NODES:
                continue
            if not self.stop_and_prove(node, self.sampler_unit, 180):
                continue
            try:
                self.validate_receipt(node, 'stop')
            except Exception as exc:
                self.errors.append(node + '-stop-receipt: ' + repr(exc))
            try:
                data = self.pull(node, self.telemetry, self.base / 'telemetry' / self.label / node)
                target = self.telemetry + '/' + node
                self.call('client', 'test ! -e ' + q(target) + ' && mkdir -p ' + q(target) +
                          ' && tar -C ' + q(target) + ' -xzf -', input=data, check=True, timeout=120)
            except Exception as exc:
                self.errors.append(node + '-collect: ' + repr(exc))
        for remote, local, label in ((self.telemetry, self.base / 'telemetry' / self.label / 'client', 'client-telemetry'),
                                      (self.a.results, self.base / 'results-raw' / self.label, 'results')):
            try:
                self.pull('client', remote, local)
            except Exception as exc:
                self.errors.append(label + ': ' + repr(exc))

    def reconcile(self):
        a = self.a
        argv = ['python3', '-B', T + '/build_reconcile_input.py', '--manifest', a.manifest,
                '--manifest-sha256', a.manifest_sha256, '--phase', a.phase, '--protocol', a.protocol,
                '--results', a.results, '--layout', LAYOUT, '--client-receipt', self.telemetry + '/client-before.json',
                '--client-receipt', self.telemetry + '/client-after.json']
        for node in NODES:
            argv += ['--node', node + '=' + self.telemetry + '/' + node]
        argv += ['--output', self.reconcile_dir + '/input.json']
        self.call('client', 'test ! -e ' + q(self.reconcile_dir) + ' && mkdir -p ' + q(self.reconcile_dir), check=True, timeout=30)
        # The R1/R2 checks ended before collection; R4 itself does not enforce its code pins.
        self.verify_hashes('client', self.pins, 'client-pins-before-r4')
        with (self.folder / 'reconcile.log').open('xb') as stream:
            self.call('client', shlex.join(argv), stdout=stream, stderr=stream, check=True, timeout=180)
            self.reconcile_rc = self.call('client', shlex.join(['python3', '-B', T + '/reconcile_telemetry.py',
                '--input', self.reconcile_dir + '/input.json', '--output', self.reconcile_dir + '/report.json']),
                stdout=stream, stderr=stream, timeout=180).returncode
        local = self.base / 'reconcile' / self.label
        self.pull('client', self.reconcile_dir, local)
        report = json.loads((local / 'report.json').read_text())
        need(report.get('reconciliation_complete') is True and report.get('phase') == a.phase and
             [c['case'] for c in report['cases']] == self.expected_cases, 'R4 did not reconcile the complete expected population')
        self.phase_summary = report['phase_summary']
        need(self.phase_summary['cases'] == len(self.expected_cases) and sum(self.phase_summary[k] for k in
             ('clean', 'contaminated', 'unverifiable')) == len(self.expected_cases), 'R4 case count disagreement')
        need(self.reconcile_rc == (0 if self.phase_summary['clean'] == len(self.expected_cases) else 3),
             'R4 exit/verdict disagreement')

    def execute(self):
        try:
            self.preflight()
            self.start_samplers()
            self.client_receipt('before')
            self.execute_runner()
        except BaseException as exc:
            self.errors.append('orchestration: ' + repr(exc))
        # A disconnected SSH client is not proof that the remote workload ended.
        if not self.finish_runner():
            return self.complete(2, 'runner-state-unknown-r3-left-bounded-armed')
        if self.intents:
            self.collect()
            try:
                self.reconcile()
            except Exception as exc:
                self.errors.append('R4: ' + repr(exc))
        if self.errors or self.reconcile_rc not in (0, 3) or self.runner_rc is None:
            return self.complete(2, 'controller-failure')
        if self.runner_rc != 0 or self.reconcile_rc == 3:
            return self.complete(1 if self.a.diagnostic else 3, 'known-adverse-or-omitted')
        return self.complete(0, 'clean')

    def complete(self, code, outcome):
        summary = dict(phase=self.a.phase, label=self.label, runner_rc=self.runner_rc,
            runner_status=self.runner_status, runner_final_status=self.runner_final_status,
            runner_quiescent=self.runner_quiescent, reconcile_rc=self.reconcile_rc,
            phase_summary=self.phase_summary, errors=self.errors, diagnostic=self.a.diagnostic,
            intents=self.intents, outcome=outcome, exit_status=code)
        self.dump('operations.json', self.operations)
        self.dump('complete.json', summary)
        print(json.dumps(summary, sort_keys=True), flush=True)
        return code


def main(argv=None):
    os.umask(0o077)
    ap = argparse.ArgumentParser(description=__doc__)
    for name in ('phase', 'manifest', 'manifest-sha256', 'protocol', 'orders', 'results'):
        ap.add_argument('--' + name, required=True)
    ap.add_argument('--label', help='fresh telemetry/controller label, defaults to phase')
    ap.add_argument('--placement-template', default=R + '/provenance/p0/placement-command.json')
    ap.add_argument('--diagnostic', action='store_true')
    args = ap.parse_args(argv)
    try:
        return Controller(args).execute()
    except (Refused, OSError, ValueError, KeyError) as exc:
        print('REFUSED/FAILED: ' + str(exc), file=sys.stderr)
        return 2


if __name__ == '__main__':
    sys.exit(main())
