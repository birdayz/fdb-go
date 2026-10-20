#!/usr/bin/env python3
"""P0 only, on the dedicated campaign client. No provisioning or measurement.

Requires explicit --execute and literal F0 hashes. Outputs are exclusive/private.
Exit 0: integral and all cells process-disjoint; 3: integral partial attempt 3;
1: integrity/operation failure (stop); 2: refused before execution.
"""
import argparse
import hashlib
import importlib
import json
import math
import os
from pathlib import Path
import re
import subprocess
import sys
import time

STORES = ('small', 'split', 'planskip')
NEW = 'fa8725f1dcf6ff3ac66825ab6e3e819efbc0abcc'
OLD = '267a41434a81723b07e7150869d702636a1b91a9'
HARNESS = 'd94b4c61377d173b7c6884fde426a227dab0f0bb'
DEFECTS = {'delete-chunk': (2, 'canonical fixture key(s) missing'),
           'stray-suffix': (3, 'non-canonical key(s)'),
           'version-key': (-1, 'non-canonical key(s)')}


class Failure(RuntimeError):
    pass


def need(condition, message):
    if not condition:
        raise Failure(message)


def sha(path):
    return hashlib.sha256(Path(path).read_bytes()).hexdigest()


def safe(path):
    p = Path(os.path.abspath(path))
    for part in (p, *p.parents):
        need(not part.is_symlink(), f'symlink in path: {part}')
        need(not (part / '.git').exists(), f'Git worktree output/input root: {part}')
    return p


def mkdir(path):
    p = safe(path)
    p.mkdir(mode=0o700)
    return p


def dump(path, value):
    p = safe(path)
    with p.open('x', encoding='utf-8') as f:
        os.chmod(p, 0o600)
        json.dump(value, f, indent=2, sort_keys=True, allow_nan=False)
        f.write('\n')


def load(path):
    return json.loads(Path(path).read_text())


def fixture_from_log(text, name):
    audits = []
    for line in text.splitlines():
        if 'FDB_BENCH_AUDIT ' in line:
            audits.append(json.loads(line.split('FDB_BENCH_AUDIT ', 1)[1]))
    need(len(audits) == 1, f'{name}: expected exactly one fixture audit, got {len(audits)}')
    fx = audits[0]
    expected = {'case': 'BenchmarkPlanSkip', 'phase': 'fixture', 'rows': 1000, 'prefix_bytes': 22} if name == 'planskip' else {
        'case': f'BenchmarkIndexFetch/payload={name}', 'phase': 'fixture', 'rows': 1000, 'prefix_bytes': 19,
        'payload': name, 'payload_bytes': 100000 if name == 'split' else 0,
        'split': name == 'split', 'chunks': 2 if name == 'split' else 1}
    need(fx == expected, f'{name}: fixture audit differs from frozen contract: {fx!r}')
    need(not re.search(r'\bns/op\b|\b(?:B/op|allocs/op)\b', text), f'{name}: timed result in fixture-only log')
    need(re.search(r'^PASS\s*$', text, re.M), f'{name}: missing benchmark binary PASS')
    return fx


def seed_argv(binary, name):
    # Both parents load their fixture before calling b.Run on cells. No cell
    # matches this third (fetch) / second (planskip) path component.
    regex = '^BenchmarkPlanSkip$/^P0_FIXTURE_ONLY_NO_CELL$' if name == 'planskip' else (
        f'^BenchmarkIndexFetch$/^payload={name}$/^P0_FIXTURE_ONLY_NO_CELL$')
    return [str(binary), '-test.run=^$', '-test.bench=' + regex,
            '-test.benchtime=1x', '-test.count=1', '-test.cpu=8', '-test.v', '-test.timeout=10m']


def receipt_kind(rc, receipt):
    need(rc in (0, 3), f'receipt command exit {rc}; only integral partial exit 3 is acceptable')
    need(receipt.get('integrity_ok') is True and receipt.get('errors') == [] and receipt.get('error_count') == 0,
         f'receipt integrity failure: {receipt.get("errors")}')
    need(bool(receipt.get('cells')), 'empty receipt cell population')
    passed = all(c.get('passed') is True and c.get('server_disjoint_fraction') == 1.0 and
                 c.get('missing_ids') == 0 and c.get('rows', 0) > 0 for c in receipt['cells'].values())
    need(receipt.get('passed') is passed and rc == (0 if passed else 3), 'receipt exit/pass/cells disagreement')
    return passed


def controls_ok(report, pr):
    need(report.get('ok') is True and report.get('original_integrity_ok') is True and
         report.get('original_errors') == [], 'offline controls did not qualify original')
    groups = {'mutations': {'duplicate-key', 'zero-read-version', 'truncated-boundaries', 'incomplete-key-scan',
                            'foreign-address', 'omitted-chunk', 'stray-suffix', 'version-key'},
              'provenance_mutations': {'diagnostic-addresses', 'addresses-at-other-version', 'dd-not-required-off',
                                      'dd-on-at-v1', 'dd-on-at-v2', 'dd-key-absent-at-v1', 'recheck-before-v1',
                                      'boundaries-not-rechecked', 'lock-owner-taken', 'lock-owner-missing',
                                      'lock-owner-not-rechecked', 'lock-token-changed', 'key-servers-changed',
                                      'fixture-sha-mismatch', 'cluster-mismatch'} |
                                     {'missing-' + f for f in pr.PROVENANCE_FIELDS}}
    reasons = {'duplicate-key': 'listed more than once', 'zero-read-version': 'read_version 0 is not a positive integer',
        'truncated-boundaries': 'boundaries_complete is not true', 'incomplete-key-scan': 'keys_complete is not true',
        'foreign-address': 'not a storage server', 'omitted-chunk': 'canonical fixture key(s) missing',
        'stray-suffix': 'non-canonical key(s)', 'version-key': 'non-canonical key(s)',
        'diagnostic-addresses': 'is not transactional', 'addresses-at-other-version': 'addresses were not read',
        'dd-not-required-off': 'did not require data distribution off', 'dd-on-at-v1': 'at_v1',
        'dd-on-at-v2': 'at_v2', 'dd-key-absent-at-v1': 'at_v1', 'recheck-before-v1': 'recheck read version',
        'boundaries-not-rechecked': 'boundaries were not rechecked', 'lock-owner-taken': 'moveKeysLock owner',
        'lock-owner-missing': 'moveKeysLock owner', 'lock-owner-not-rechecked': 'moveKeysLock owner',
        'lock-token-changed': 'moveKeysLock write token', 'key-servers-changed': 'keyServers digest',
        'fixture-sha-mismatch': 'fixture_receipt_sha256 differs', 'cluster-mismatch': 'cluster identity differs',
        'missing-addresses_basis': 'is not transactional', 'missing-cluster': 'cluster identity differs',
        'missing-dd_mode': 'did not require data distribution off',
        'missing-fixture_receipt_sha256': 'fixture_receipt_sha256 differs', 'missing-key_servers': 'keyServers digest',
        'missing-move_keys_lock': 'moveKeysLock owner', 'missing-recheck': 'recheck read version'}
    for group, expected in groups.items():
        need(set(report.get(group, {})) == expected, f'offline {group} population changed')
        for name, result in report[group].items():
            need(result.get('rejected') is True and any(reasons[name] in e for e in result.get('errors', [])),
                 f'offline {name} was not rejected for its intended reason')
    need(set(report.get('positive_controls', {})) == {'stable-random-owner'}, 'missing positive owner control')
    need(report['positive_controls']['stable-random-owner'] == {'accepted': True, 'errors': []},
         'stable random owner was rejected')


def mutation_ok(before, after, receipt, defect, pr):
    suffix, reason = DEFECTS[defect]
    key = (bytes.fromhex(before['store_prefix_hex']) + pr.pack([1, 7, suffix])).hex()
    a = {k['key_hex'] for k in before['keys']}
    b = {k['key_hex'] for k in after['keys']}
    need(len(a) == len(before['keys']) and len(b) == len(after['keys']), 'mutation introduced duplicate scan keys')
    need((a - b, b - a) == (({key}, set()) if defect == 'delete-chunk' else (set(), {key})),
         f'{defect}: physical mutation did not apply exactly to ID 7 suffix {suffix}')
    need(receipt.get('integrity_ok') is False and receipt.get('passed') is False and receipt.get('error_count', 0) > 0,
         f'{defect}: defective fixture accepted')
    need(any(reason in e and key in e for e in receipt.get('errors', [])),
         f'{defect}: rejection did not name intended physical defect: {receipt.get("errors")}')


def settle_errors(errors):
    transient_prefixes = ('storage data lag ', 'storage durability lag ', 'log queue ',
        'moving_data.in_flight_bytes missing or not a number:',
        'moving_data.in_queue_bytes missing or not a number:')
    transient = [e for e in errors if e.startswith(transient_prefixes)]
    return [e for e in errors if e not in transient], transient


def same_epoch(receipts):
    need(len(receipts) >= 3, 'need nonempty all-store receipt population')
    owners = {(r['move_keys_owner_hex'], r['move_keys_write_token_hex']) for r in receipts}
    need(len(owners) == 1, 'DD lock owner/write token changed across store probe batch')


class P0:
    def __init__(self, args):
        self.a = args
        self.root = safe(args.root)
        self.out = safe(args.output)
        need(self.out.is_absolute() and not os.path.lexists(self.out), 'output must be a new absolute directory')
        self.tools = self.root / 'tooling/tools/benchcampaign/multinode'
        self.cluster = self.root / 'cluster-ssd/fdb.cluster'
        self.pp = self.root / 'bin/placementprobe-cgo'
        self.fetch = self.root / 'bin/fetch-master'
        self.skip = self.root / 'bin/planskip-master'
        self.cli = Path(args.fdbcli).absolute()
        self.protocol_path = self.root / 'protocol.json'
        self.layout = load(self.root / 'cluster-ssd/layout.json')
        protocol = load(self.protocol_path)
        need(protocol['frozen_harness_revision'] == HARNESS, 'harness revision mismatch')
        need(protocol['production_revisions'] == {'before_allocation_work_after_read_parity': OLD, 'final': NEW},
             'production revisions mismatch')
        sys.path.insert(0, str(self.tools))
        self.pr = importlib.import_module('placement_receipt')
        self.cg = importlib.import_module('cluster_gate')
        self.identity = self.pr.cluster_identity(self.cluster.read_bytes())
        self.pins = {str(p): sha(p) for p in [self.cluster, self.pp, self.fetch, self.skip, self.cli,
                    self.protocol_path, self.root / 'cluster-ssd/layout.json', Path(sys.executable).resolve(), Path(__file__).absolute(),
                    Path(__file__).absolute().with_name('placement_checked.py')]}
        for folder in (self.tools, self.tools.parent / 'singlenode'):
            for p in sorted(folder.rglob('*.py')):
                self.pins[str(p)] = sha(p)
        for p, expected in ((self.cluster, args.expect_cluster_sha256), (self.pp, args.expect_placementprobe_sha256),
                            (self.fetch, args.expect_fetch_sha256), (self.skip, args.expect_planskip_sha256)):
            need(re.fullmatch('[0-9a-f]{64}', expected) is not None and sha(p) == expected, f'F0 hash mismatch: {p}')
        self.check_pins()
        self.cg.cluster_file_guard(self.cluster, self.layout, protocol['topology']['network']['ip_range'])
        need(self.layout['engine'] == 'ssd-2', 'P0 requires frozen SSD layout')

    def check_pins(self):
        for p, expected in self.pins.items():
            need(sha(p) == expected, f'input changed during P0: {p}')

    def run(self, folder, label, argv, *, allowed=(0,), env=None, timeout=600):
        self.check_pins()
        work = mkdir(folder / label)
        argv = [str(a) for a in argv]
        started, mono = time.time(), time.monotonic()
        rc, timed_out = None, False
        with (work / 'stdout').open('xb') as out, (work / 'stderr').open('xb') as err:
            try:
                proc = subprocess.run(argv, stdout=out, stderr=err, timeout=timeout,
                                      env=dict(os.environ, **(env or {})))
                rc = proc.returncode
            except subprocess.TimeoutExpired:
                timed_out = True
        dump(work / 'command.json', dict(argv=argv, explicit_environment=env or {}, returncode=rc, timeout=timed_out,
            started_epoch=started, duration_seconds=time.monotonic() - mono,
            cluster_file_sha256=self.a.expect_cluster_sha256,
            placementprobe_sha256=self.a.expect_placementprobe_sha256, pinned_hashes=self.pins))
        self.check_pins()
        need(not timed_out and rc in allowed, f'{label}: exit={rc}, timeout={timed_out}; see {work}')
        return rc, (work / 'stdout').read_text(), (work / 'stderr').read_text()

    def cli_exec(self, folder, label, command):
        return self.run(folder, label, [self.cli, '-C', self.cluster, '--exec', command], timeout=240)[1]

    def status(self, folder, label, mode, *, settling=False):
        status = json.loads(self.cli_exec(folder, label, 'status json'))
        errors, movement, roles = self.cg.status_verdict(status, self.layout, 'ssd-2', mode)
        dump(folder / (label + '.json'), status)
        dump(folder / (label + '-verdict.json'), dict(errors=errors, movement=movement, roles=roles))
        hard, pending = settle_errors(errors) if settling else (errors, [])
        need(not hard, f'{label}: status invalid: {hard}')
        return status, movement + pending

    def pp_cmd(self, folder, label, cmd, store, fixture=None, **flags):
        output = folder / (label + '.json')
        argv = [self.pp, cmd, '--cluster-file', self.cluster, '--store', store, '--output', output]
        if fixture is not None:
            argv += ['--fixture-receipt', fixture]
        for k, value in flags.items():
            argv += ['--' + k.replace('_', '-'), str(value)]
        self.run(folder, label, argv)
        result = load(output)
        need(result.get('cluster') == self.identity, f'{label}: H5/H6 cluster mismatch')
        need(result.get('store') == store, f'{label}: store mismatch')
        suffix = store.rsplit('/', 1)[-1]
        tup = ['benchfetch', 'control', suffix.removeprefix('control-')] if suffix.startswith('control-') else ['benchfetch', suffix]
        prefix = self.pr.pack(tup)
        need(result.get('store_prefix_hex') == prefix.hex(), f'{label}: non-owned prefix')
        if cmd != 'probe':
            need(result.get('action') == cmd, f'{label}: H6 action mismatch')
            if cmd.startswith('ballast-'):
                begin, end = prefix + self.pr.pack([1, 1000000]), self.pr.strinc(prefix + self.pr.pack([1]))
            elif cmd == 'control-mutate':
                begin = end = prefix + self.pr.pack([1, 7, DEFECTS[flags['defect']][0]])
                need(result.get('defect') == flags['defect'], 'mutation receipt defect mismatch')
            else:
                begin, end = prefix, self.pr.strinc(prefix)
            need(result.get('range_hex') == [begin.hex(), end.hex()], f'{label}: H6 touched wrong range')
            if cmd not in ('ballast-verify-empty',):
                versions = result.get('committed_versions', [])
                need(bool(versions) and all(type(v) is int and v > 0 for v in versions) and
                     result.get('transactions') == len(versions), f'{label}: missing committed versions')
            if cmd == 'ballast-clear':
                need(result.get('dd_mode_required') == 'off', 'ballast clear not guarded by DD-off')
            if cmd == 'ballast-write':
                need(result.get('dd_mode_required') == 'on', 'ballast write not guarded by DD-on')
        return output, result

    def cleared(self, receipt):
        need(receipt.get('empty') is True and receipt.get('keys_found') == 0 and
             type(receipt.get('read_version')) is int and receipt['read_version'] > 0, 'empty range not verified at positive RV')

    def probe(self, folder, name, store, fixture, *, scratch=False, defective=False):
        probe_path, probe = self.pp_cmd(folder, name + '-probe', 'probe', store, fixture)
        status, movement = self.status(folder, name + '-status', 'disabled')
        need(not movement, f'{name}: movement while frozen')
        suffix = store.rsplit('/', 1)[-1]
        fmt = 'split' if suffix in ('split', 'control-split') else 'unsplit'
        preset = 'planskip' if suffix == 'planskip' else 'benchfetch'
        tup = ['benchfetch', 'control', suffix.removeprefix('control-')] if scratch else ['benchfetch', suffix]
        receipt_path = folder / (name + '-receipt.json')
        common = ['--status', folder / (name + '-status.json'), '--probe', probe_path, '--fixture-receipt', fixture,
                  '--cluster-file', self.cluster, '--preset', preset, '--layout', 'recordlayer',
                  '--record-format', fmt, '--rows', '1000', '--expect-prefix-tuple', ','.join(tup), '--replicas', '2']
        rc, _, _ = self.run(folder, name + '-receipt-command',
            [sys.executable, '-B', self.tools / 'placement_receipt.py', 'receipt', *common, '--output', receipt_path],
            allowed=(1,) if defective else (0, 3))
        receipt = load(receipt_path)
        if not defective:
            receipt_kind(rc, receipt)
            need(not self.pr.sealed_errors(receipt, self.identity), f'{name}: unsealable provenance')
            controls = folder / (name + '-controls.json')
            self.run(folder, name + '-controls-command', [sys.executable, '-B', self.tools / 'placement_receipt.py',
                     'controls', *common, '--output', controls])
            controls_ok(load(controls), self.pr)
        return receipt_path, receipt, probe, self.cg.role_identity(status)

    def qualify(self):
        q = mkdir(self.out / 'qualification')
        self.run(q, 'refuse-noargs', [self.pp], allowed=(2,))
        absent = q / 'unowned-must-not-exist.json'
        self.run(q, 'refuse-unowned', [self.pp, 'probe', '--cluster-file', self.cluster, '--store', 'unowned/forbidden',
                 '--fixture-receipt', self.protocol_path, '--output', absent], allowed=(2,))
        need(not os.path.lexists(absent), 'unowned refusal created a probe output')
        self.cli_exec(q, 'dd-off', 'datadistribution off')
        self.status(q, 'dd-off-status', 'disabled')
        for name in ('small', 'split'):
            folder = mkdir(q / ('original-' + name))
            store = 'benchfetch/control-' + name
            _, clear = self.pp_cmd(folder, 'clear-before', 'control-clear', store)
            self.cleared(clear)
            fixture = folder / 'fixture.json'
            self.pp_cmd(folder, 'write', 'control-write', store, fixture_receipt_out=fixture, rows=1000)
            self.probe(folder, 'original', store, fixture, scratch=True)
            _, clear = self.pp_cmd(folder, 'clear-after', 'control-clear', store)
            self.cleared(clear)
        for defect in DEFECTS:
            folder = mkdir(q / defect)
            store = 'benchfetch/control-split'
            _, clear = self.pp_cmd(folder, 'clear-before', 'control-clear', store)
            self.cleared(clear)
            fixture = folder / 'fixture.json'
            self.pp_cmd(folder, 'write', 'control-write', store, fixture_receipt_out=fixture, rows=1000)
            _, _, original, _ = self.probe(folder, 'original', store, fixture, scratch=True)
            self.pp_cmd(folder, 'mutate', 'control-mutate', store, defect=defect)
            _, receipt, mutated, _ = self.probe(folder, 'defective', store, fixture, scratch=True, defective=True)
            mutation_ok(original, mutated, receipt, defect, self.pr)
            dump(folder / 'verified-defect.json', dict(defect=defect, intended_key_rejected=True))
            _, clear = self.pp_cmd(folder, 'clear-after', 'control-clear', store)
            self.cleared(clear)
        dump(q / 'qualified.json', dict(ok=True, originals=['small', 'split'], defects=list(DEFECTS)))

    def settle(self, folder, *, deadline_epoch=None):
        start, zero_start, poll = time.monotonic(), None, 0
        if deadline_epoch is None:
            deadline_epoch = time.time() + self.a.settle_timeout_seconds
        end = start + max(0, deadline_epoch - time.time())
        initial_roles = getattr(self, 'settle_expected_roles', None)
        while time.monotonic() < end:
            status, movement = self.status(folder, f'settle-{poll:04d}', 'enabled', settling=True)
            now = time.monotonic()
            need(now <= end, 'original settle deadline expired during status poll')
            roles = self.cg.role_identity(status)
            if initial_roles is None:
                initial_roles = roles
            need(roles == initial_roles, 'roles/recovery changed during settling')
            if movement:
                zero_start = None
            elif zero_start is None:
                zero_start = now
            elif now - zero_start >= 120:
                dump(folder / 'settled.json', dict(zero_movement_seconds=now - zero_start,
                     polls=poll + 1, period_seconds=5, deadline_epoch=deadline_epoch))
                return
            poll += 1
            time.sleep(min(5, max(0, end - time.monotonic())))
        raise Failure('movement/health failed to remain valid and zero for 120s within original settle budget')

    def attempt(self, k, mib):
        a = mkdir(self.out / f'attempt-{k}')
        self.cli_exec(a, 'dd-on', 'datadistribution on')
        fixtures = {}
        for name in STORES:
            b = mkdir(a / name)
            binary = self.skip if name == 'planskip' else self.fetch
            _, text, err = self.run(b, 'fixture-only', seed_argv(binary, name),
                env={'FDB_BENCH_CLUSTER_FILE': str(self.cluster), 'GOMAXPROCS': '8'})
            need(not err.strip(), f'{name}: unexpected fixture stderr; inspect before proceeding')
            fixture = b / 'fixture.json'
            dump(fixture, fixture_from_log(text, name))
            fixtures[name] = fixture
            _, receipt = self.pp_cmd(b, 'ballast-write', 'ballast-write', 'benchfetch/' + name, fixture, ballast_mib=mib)
            count = math.ceil((mib * 1024 * 1024) / 100000)
            need(receipt.get('keys_written') == count and receipt.get('total_bytes') == count * 100000 and
                 receipt.get('value_bytes') == 100000, 'ballast size differs from protocol')
        return self.complete_attempt(k, mib, a, fixtures)

    def complete_attempt(self, k, mib, a, fixtures, *, deadline_epoch=None, before_clear=None, already_frozen=False):
        if not already_frozen:
            if deadline_epoch is None:
                self.settle(a)
            else:
                self.settle(a, deadline_epoch=deadline_epoch)
            self.cli_exec(a, 'dd-off', 'datadistribution off')
        self.status(a, 'dd-off-status', 'disabled')
        if before_clear is not None:
            before_clear(a, fixtures)
        for name, fixture in fixtures.items():
            b = a / name
            self.pp_cmd(b, 'ballast-clear', 'ballast-clear', 'benchfetch/' + name, fixture)
            _, receipt = self.pp_cmd(b, 'ballast-empty', 'ballast-verify-empty', 'benchfetch/' + name, fixture)
            self.cleared(receipt)
        receipts, roles = {}, []
        for name, fixture in fixtures.items():
            path, receipt, _, role = self.probe(a / name, 'canonical', 'benchfetch/' + name, fixture)
            receipts[name] = (path, receipt)
            roles.append(role)
        same_epoch([r for _, r in receipts.values()])
        need(all(r == roles[0] for r in roles), 'roles/recovery changed across all-store canonical sweep')
        passed = all(r['passed'] for _, r in receipts.values())
        dump(a / 'attempt.json', dict(attempt=k, ballast_mib_per_store=mib, all_cells_pass=passed,
             receipts={n: str(p) for n, (p, _) in receipts.items()}, fixtures={n: str(p) for n, p in fixtures.items()}))
        return passed, a, fixtures, receipts, roles[0]

    def finish(self, k, a, fixtures, receipts, role):
        final = mkdir(self.out / 'selected')
        selected, final_receipts = {}, []
        for name in STORES:
            b = mkdir(final / name)
            path, r, _, current_role = self.probe(b, 'final', 'benchfetch/' + name, fixtures[name])
            comparison = self.pr.compare(receipts[name][1], r)
            dump(b / 'compare.json', comparison)
            need(comparison['equal'], f'{name}: changed after selected attempt: {comparison["differences"]}')
            need(current_role == role, 'roles/recovery changed before final selection')
            selected['benchfetch/' + name] = str(path)
            final_receipts.append(r)
        same_epoch([r for _, r in receipts.values()] + final_receipts)
        status, movement = self.status(final, 'frozen-final-status', 'disabled')
        need(not movement and self.cg.role_identity(status) == role, 'final status not stable')
        fixture_map = {'benchfetch/' + n: str(p) for n, p in fixtures.items()}
        dump(self.out / 'fixture-receipts.json', fixture_map)
        dump(self.out / 'selected-receipts.json', selected)
        # R2 delegates to the frozen placement command. The external wrapper
        # additionally checks PP's literal hash and archives command receipts.
        command = [sys.executable, '-B', str(Path(__file__).absolute().with_name('placement_checked.py')),
            '--expect-placementprobe-sha256', self.a.expect_placementprobe_sha256,
            '--tooling', str(self.tools), '--store', '{store}', '--case', '{case}', '--when', '{when}',
            '--preset', '{preset}', '--layout', '{layout}', '--record-format', '{record_format}',
            '--cluster-file', '{cluster_file}', '--expect-cluster-sha256', self.a.expect_cluster_sha256,
            '--output', '{output}', '--placementprobe', str(self.pp), '--fdbcli', str(self.cli),
            '--fixture-receipts', str(self.out / 'fixture-receipts.json'), '--work-dir', str(self.out / 'r2-placement-work')]
        dump(self.out / 'placement-command.json', command)
        seal_paths = [self.out / 'fixture-receipts.json', self.out / 'selected-receipts.json',
                      self.out / 'placement-command.json', *fixtures.values(), *map(Path, selected.values()),
                      Path(sys.executable).resolve(), Path(__file__).absolute(),
                      Path(__file__).absolute().with_name('placement_checked.py')]
        seal_paths += list(getattr(self, 'extra_seal_paths', []))
        dump(self.out / 'seal-pins.json', {str(p): sha(p) for p in seal_paths})
        self.check_pins()
        passed = all(r['passed'] for r in final_receipts)
        dump(self.out / 'complete.json', dict(ok=True, integral=True, exit_status=0 if passed else 3,
             selected_attempt=k, all_cells_pass=passed, dd_left_off=True, timed_measurements=0,
             fixture_receipts=str(self.out / 'fixture-receipts.json'), selected_receipts=selected,
             placement_command_sha256=sha(self.out / 'placement-command.json'),
             cluster_file_sha256=self.a.expect_cluster_sha256, placementprobe_sha256=self.a.expect_placementprobe_sha256,
             cells={r['store']: r['cells'] for r in final_receipts}, pinned_hashes=self.pins))
        return 0 if passed else 3

    def execute(self):
        mkdir(self.out)
        dump(self.out / 'invocation.json', dict(argv=sys.argv, inputs=self.pins, revisions=dict(new=NEW, old=OLD, harness=HARNESS),
             fixture_quantity={'small': 'id%7+1', 'split': 'id%7+1', 'planskip': 1},
             selection='first all-store/all-cell pass; otherwise attempt 3', no_measurement=True))
        self.qualify()
        for k, mib in enumerate((256, 512, 1024), 1):
            passed, a, fixtures, receipts, role = self.attempt(k, mib)
            if passed or k == 3:
                return self.finish(k, a, fixtures, receipts, role)
        raise AssertionError('unreachable')


def main(argv=None):
    os.umask(0o077)
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument('--execute', action='store_true')
    ap.add_argument('--root', default='/srv/fdb-bench')
    ap.add_argument('--output', required=True)
    ap.add_argument('--fdbcli', default='/usr/bin/fdbcli')
    for name in ('cluster', 'placementprobe', 'fetch', 'planskip'):
        ap.add_argument('--expect-' + name + '-sha256', required=True)
    ap.add_argument('--settle-timeout-seconds', type=int, default=1200)
    args = ap.parse_args(argv)
    if not args.execute:
        print('REFUSED: --execute required; this script mutates only the dedicated campaign stores/DD', file=sys.stderr)
        return 2
    p0 = None
    try:
        need(Path(args.output).is_absolute() and Path(args.root).is_absolute(), 'root/output must be absolute')
        need(args.settle_timeout_seconds >= 120, 'settle budget cannot be below 120s')
        p0 = P0(args)
        return p0.execute()
    except (Failure, OSError, ValueError, KeyError, ImportError) as exc:
        print('STOP: ' + str(exc), file=sys.stderr)
        if p0 is not None and p0.out.exists():
            dump(p0.out / 'failure.json', dict(error=str(exc), continue_to_timing=False,
                 dd_not_reenabled_by_cleanup=True, timed_measurements=0))
        return 1 if p0 is not None and p0.out.exists() else 2


if __name__ == '__main__':
    sys.exit(main())
