#!/usr/bin/env python3
"""Explicit same-attempt-1 settle continuation. No qualification/seed/write replay.

Original input/output bytes remain pinned/read-only. Original 1200s settle
clock is preserved. A frozen read-only physical key census precedes any clear.
"""
import argparse
import copy
import math
import os
from pathlib import Path
import re
import sys
import time

import p0
from p0 import Failure, dump, load, mkdir, need, safe, sha


def files(root):
    result = {}
    for path in sorted(root.rglob('*')):
        safe(path)
        if path.is_file():
            result[str(path)] = sha(path)
    return result


def flag(argv, name, default=None):
    found = [i for i, x in enumerate(argv) if x == name or x.startswith(name + '=')]
    need(len(found) <= 1, 'duplicate original option: ' + name)
    if not found:
        return default
    i = found[0]
    return argv[i].split('=', 1)[1] if '=' in argv[i] else argv[i + 1]


def expected_ballast(pr, prefix, mib=256):
    count = math.ceil(mib * 1024 * 1024 / 100000)
    return {prefix + pr.pack([1, 1000000 + i]): ('record', 1000000 + i) for i in range(count)}


def check_ballast(receipt, store, identity, pr, mib=256):
    prefix = pr.pack(['benchfetch', store])
    count = math.ceil(mib * 1024 * 1024 / 100000)
    versions = receipt.get('committed_versions', [])
    need(receipt.get('action') == 'ballast-write' and receipt.get('cluster') == identity and
         receipt.get('store') == 'benchfetch/' + store and receipt.get('store_prefix_hex') == prefix.hex() and
         receipt.get('dd_mode_required') == 'on' and receipt.get('keys_written') == count and
         receipt.get('value_bytes') == 100000 and receipt.get('total_bytes') == count * 100000 and
         receipt.get('range_hex') == [(prefix + pr.pack([1, 1000000])).hex(), pr.strinc(prefix + pr.pack([1])).hex()] and
         len(versions) == math.ceil(count / 50) and receipt.get('transactions') == len(versions) and
         all(type(v) is int and v > 0 for v in versions), 'original ballast receipt is not complete exact256MiB: ' + store)


def verify_ballasted_probe(probe, status, fixture, fixture_sha, identity, pr, store):
    prefix, boundaries, brange, keys = pr.load_probe(probe)
    expected_prefix = pr.pack(['benchfetch', store])
    need(prefix == expected_prefix and probe.get('store') == 'benchfetch/' + store, 'resume data probe wrong owned store')
    fmt = 'split' if store == 'split' else 'unsplit'
    expected = pr.canonical_keys(prefix, 1000, 'price_idx', fmt)
    ballast = expected_ballast(pr, prefix)
    expected.update(ballast)
    actual = {k['key']: (k['kind'], k['id']) for k in keys}
    need(len(actual) == len(keys) and actual == expected, 'resume physical fixture/ballast key census differs: ' + store)
    errors = []
    pr.probe_integrity(probe, prefix, boundaries, brange, errors)
    errors += pr.provenance_errors(probe, prefix, fixture_sha, identity)
    pr.fixture_integrity(fixture, prefix, 'recordlayer', 1000, fmt, errors)
    servers = pr.storage_map(status)
    for k in keys:
        if len(k['addresses']) != 2 or any(a not in servers for a in k['addresses']):
            errors.append('physical key lacks exactly two known storage-role addresses')
    need(not errors, 'resume data census provenance/integrity failed: ' + '; '.join(errors))
    return dict(store=probe['store'], fixture_keys=len(expected) - len(ballast), ballast_keys=len(ballast),
        read_version=probe['read_version'], owner=probe['move_keys_lock']['owner_hex_at_v1'],
        token=probe['move_keys_lock']['write_token_hex_at_v1'], digest=probe['key_servers']['sha256_at_v1'])


class Resume(p0.P0):
    def __init__(self, args):
        super().__init__(args)
        self.frozen = args.frozen_after_preclear_1007
        self.prior = safe(args.prior_output)
        need(self.prior.is_dir() and not self.out.is_relative_to(self.prior) and not self.prior.is_relative_to(self.out),
             'continuation output must be a fresh sibling, never inside or enclosing the original output')
        need(sha(Path(p0.__file__)) == args.ack_current_p0_sha256, 'current p0.py SHA was not acknowledged literally')
        need(sha(self.prior / 'invocation.json') == args.expect_prior_invocation_sha256, 'original invocation SHA mismatch')
        invocation = load(self.prior / 'invocation.json')
        original = invocation['inputs']
        script = str(Path(p0.__file__).absolute())
        need(original.get(script) == args.ack_prior_p0_sha256, 'original p0.py SHA acknowledgement differs from invocation')
        expected = dict(original)
        expected[script] = args.ack_current_p0_sha256
        need(expected == self.pins, 'non-p0.py input set/hash changed since original invocation')
        argv = invocation['argv']
        need('--execute' in argv and flag(argv, '--root', '/srv/fdb-bench') == str(self.root) and
             flag(argv, '--output') == str(self.prior), 'original invocation root/output/execution differs')
        for name in ('cluster', 'placementprobe', 'fetch', 'planskip'):
            need(flag(argv, '--expect-' + name + '-sha256') == getattr(args, 'expect_' + name + '_sha256'),
                 'original literal input differs: ' + name)
        self.original_timeout = int(flag(argv, '--settle-timeout-seconds', '1200'))
        need(self.original_timeout == args.settle_timeout_seconds == 1200, 'original1200s settle budget may not change')
        self.validate_boundary(original)
        self.prior_hashes = files(self.prior)
        self.pins.update(self.prior_hashes)
        self.pins[str(Path(__file__).absolute())] = sha(__file__)
        self.preclear = {}
        self.frozen_epoch = None
        if self.frozen:
            self.validate_frozen_boundary()
        self.check_pins()

    def check_pins(self):
        super().check_pins()
        if hasattr(self, 'prior_hashes'):
            need(files(self.prior) == self.prior_hashes, 'original output tree changed during continuation')
        if hasattr(self, 'prior_continuation_hashes'):
            need(files(self.prior_continuation) == self.prior_continuation_hashes, 'first continuation tree changed')

    def command(self, path, original, *, argv=None, rc=(0,)):
        c = load(path / 'command.json')
        need(c.get('pinned_hashes') == original and c.get('timeout') is False and c.get('returncode') in rc and
             c.get('cluster_file_sha256') == self.a.expect_cluster_sha256 and
             c.get('placementprobe_sha256') == self.a.expect_placementprobe_sha256,
             'original command receipt pins/exit invalid: ' + str(path))
        if argv is not None:
            need(c['argv'] == [str(v) for v in argv], 'original command argv differs: ' + str(path))
        return c

    def validate_boundary(self, original):
        need({x.name for x in self.prior.iterdir()} == {'invocation.json', 'failure.json', 'qualification', 'attempt-1'},
             'original root is not exact initial-attempt1 failure boundary')
        a = self.prior / 'attempt-1'
        need({x.name for x in a.iterdir()} == {'dd-on', 'small', 'split', 'planskip', 'settle-0000',
             'settle-0000.json', 'settle-0000-verdict.json'}, 'original attempt contains unexpected/further operations')
        q = self.prior / 'qualification'
        need(load(q / 'qualified.json') == dict(ok=True, originals=['small', 'split'], defects=list(p0.DEFECTS)),
             'original complete qualification missing')
        need(not (q / 'unowned-must-not-exist.json').exists(), 'original unowned refusal wrote output')
        controls = sorted(q.rglob('*-controls.json'))
        need(len(controls) == 5, 'original qualification controls population differs')
        for path in controls:
            p0.controls_ok(load(path), self.pr)
        for name in ('small', 'split'):
            b = q / ('original-' + name)
            need(not self.pr.sealed_errors(load(b / 'original-receipt.json'), self.identity), 'original scratch lacked integrity')
            self.cleared(load(b / 'clear-after.json'))
        for defect in p0.DEFECTS:
            b = q / defect
            need(load(b / 'verified-defect.json') == dict(defect=defect, intended_key_rejected=True), 'unverified old defect')
            p0.mutation_ok(load(b / 'original-probe.json'), load(b / 'defective-probe.json'),
                          load(b / 'defective-receipt.json'), defect, self.pr)
            self.cleared(load(b / 'clear-after.json'))
        for path in q.rglob('command.json'):
            rc = (2,) if path.parent.name.startswith('refuse-') else ((1,) if path.parent.name == 'defective-receipt-command' else (0, 3))
            self.command(path.parent, original, rc=rc)
        self.command(a / 'dd-on', original, argv=[self.cli, '-C', self.cluster, '--exec', 'datadistribution on'])
        self.fixtures = {}
        for name in p0.STORES:
            b = a / name
            need({x.name for x in b.iterdir()} == {'fixture-only', 'fixture.json', 'ballast-write', 'ballast-write.json'},
                 'original store contains later/unexpected operation: ' + name)
            binary = self.skip if name == 'planskip' else self.fetch
            c = self.command(b / 'fixture-only', original, argv=p0.seed_argv(binary, name))
            need(c['explicit_environment'] == {'FDB_BENCH_CLUSTER_FILE': str(self.cluster), 'GOMAXPROCS': '8'}, 'old fixture environment differs')
            need(not (b / 'fixture-only/stderr').read_text().strip(), 'old fixture stderr nonempty')
            need(p0.fixture_from_log((b / 'fixture-only/stdout').read_text(), name) == load(b / 'fixture.json'), 'old fixture audit differs')
            fixture = b / 'fixture.json'
            self.fixtures[name] = fixture
            self.command(b / 'ballast-write', original, argv=[self.pp, 'ballast-write', '--cluster-file', self.cluster,
                '--store', 'benchfetch/' + name, '--output', b / 'ballast-write.json', '--fixture-receipt', fixture, '--ballast-mib', '256'])
            check_ballast(load(b / 'ballast-write.json'), name, self.identity, self.pr)
        c = self.command(a / 'settle-0000', original, argv=[self.cli, '-C', self.cluster, '--exec', 'status json'])
        status = load(a / 'settle-0000.json')
        need(status == load(a / 'settle-0000/stdout'), 'old failed status output differs')
        errors, movement, roles = self.cg.status_verdict(status, self.layout, 'ssd-2', 'enabled')
        hard, pending = p0.settle_errors(errors)
        need(not hard and bool(pending) and load(a / 'settle-0000-verdict.json') == dict(errors=errors, movement=movement, roles=roles),
             'old failure was not solely the documented transient initial settle poll')
        failure = load(self.prior / 'failure.json')
        need(failure.get('error') == 'settle-0000: status invalid: ' + str(errors) and failure.get('continue_to_timing') is False,
             'old failure record differs from documented boundary')
        need(isinstance(c.get('started_epoch'), (float, int)) and c['started_epoch'] > 0, 'missing original settle start')
        self.settle_deadline = c['started_epoch'] + self.original_timeout
        if not self.frozen:
            need(self.settle_deadline - time.time() >= 120, 'original settle budget has insufficient time for120s: no extension/retry')
        self.settle_expected_roles = self.cg.role_identity(status)

    def validate_frozen_boundary(self):
        args = self.a
        need(args.prior_continuation and args.expect_prior_continuity_sha256 and
             args.ack_failed_continuation_p0_sha256 and args.ack_failed_continuation_resume_sha256,
             'frozen1007 entry requires prior continuation and all literal continuity/source ACKs')
        root = safe(args.prior_continuation)
        need(not self.out.is_relative_to(root) and not root.is_relative_to(self.out) and root != self.prior,
             'new frozen output must preserve both old sibling trees')
        need(sha(root / 'continuity.json') == args.expect_prior_continuity_sha256, 'prior continuity hash differs')
        continuity = load(root / 'continuity.json')
        need(continuity['same_attempt'] == 1 and continuity['original_output'] == str(self.prior) and
             continuity['original_files_sha256'] == self.prior_hashes and
             continuity['original_settle_deadline_epoch'] == self.settle_deadline and
             continuity['original_invocation_sha256'] == args.expect_prior_invocation_sha256 and
             continuity['prior_p0_sha256'] == args.ack_prior_p0_sha256 and
             continuity['current_p0_sha256'] == args.ack_failed_continuation_p0_sha256,
             'prior same-attempt continuity lineage differs')
        expected = dict(self.pins)
        expected[str(Path(p0.__file__).absolute())] = args.ack_failed_continuation_p0_sha256
        expected[str(Path(__file__).absolute())] = args.ack_failed_continuation_resume_sha256
        expected[str(root / 'continuity.json')] = sha(root / 'continuity.json')
        need({p.name for p in root.iterdir()} == {'continuity.json', 'attempt-1', 'failure.json'},
             'prior continuation has unexpected later output')
        a = root / 'attempt-1'
        allowed = {'small', 'split', 'planskip', 'settled.json', 'dd-off', 'dd-off-status',
                   'dd-off-status.json', 'dd-off-status-verdict.json'}
        need(all(p.name in allowed or re.fullmatch(r'settle-[0-9]{4}(?:-verdict)?(?:\.json)?', p.name)
                 for p in a.iterdir()), 'prior frozen attempt has clear/canonical/later operations')
        settled = load(a / 'settled.json')
        need(settled['deadline_epoch'] == self.settle_deadline and settled['zero_movement_seconds'] >= 120 and
             type(settled['polls']) is int and settled['polls'] >= 2, 'prior healthy-zero interval not complete')
        for i in range(settled['polls']):
            label = f'settle-{i:04d}'
            c = self.command(a / label, expected, argv=[self.cli, '-C', self.cluster, '--exec', 'status json'])
            need(c['started_epoch'] + c['duration_seconds'] <= self.settle_deadline, 'prior settle exceeded original deadline')
            status = load(a / (label + '.json'))
            need(status == load(a / label / 'stdout'), 'prior settle status stdout differs')
            errors, movement, roles = self.cg.status_verdict(status, self.layout, 'ssd-2', 'enabled')
            hard, _ = p0.settle_errors(errors)
            need(not hard and self.cg.role_identity(status) == self.settle_expected_roles and
                 load(a / (label + '-verdict.json')) == dict(errors=errors, movement=movement, roles=roles),
                 'prior settle hard error/role/history differs')
            if i == settled['polls'] - 1:
                need(not errors and not movement, 'last settled poll was not healthy-zero')
        self.command(a / 'dd-off', expected, argv=[self.cli, '-C', self.cluster, '--exec', 'datadistribution off'])
        self.command(a / 'dd-off-status', expected, argv=[self.cli, '-C', self.cluster, '--exec', 'status json'])
        frozen = load(a / 'dd-off-status.json')
        errors, movement, _ = self.cg.status_verdict(frozen, self.layout, 'ssd-2', 'disabled')
        need(not errors and not movement and self.cg.role_identity(frozen) == self.settle_expected_roles,
             'prior DD-off health/roles invalid')
        small, split = a / 'small', a / 'split'
        need({p.name for p in small.iterdir()} == {'resume-ballasted-data', 'resume-ballasted-data.json',
            'resume-ballasted-status', 'resume-ballasted-status.json', 'resume-ballasted-status-verdict.json',
            'resume-data-verified.json'} and {p.name for p in split.iterdir()} == {'resume-ballasted-data'} and
             not list((a / 'planskip').iterdir()), 'prior census boundary contains clear/later work')
        for path in a.rglob('command.json'):
            self.command(path.parent, expected, rc=(1,) if path.parent == split / 'resume-ballasted-data' else (0,))
        failed = self.command(split / 'resume-ballasted-data', expected, rc=(1,))
        need(failed['argv'] == [str(x) for x in [self.pp, 'probe', '--cluster-file', self.cluster, '--store',
             'benchfetch/split', '--output', split / 'resume-ballasted-data.json', '--fixture-receipt', self.fixtures['split']]],
             'failed prior operation was not exact split read-only census')
        stderr = (split / 'resume-ballasted-data/stderr').read_text()
        need('scan data subspace' in stderr and ('1007' in stderr or 'transaction_too_old' in stderr),
             'frozen boundary permits only scan transaction_too_old1007')
        failure = load(root / 'failure.json')
        need(failure.get('error') == f'resume-ballasted-data: exit=1, timeout=False; see {split / "resume-ballasted-data"}' and
             failure.get('continue_to_timing') is False, 'prior failure boundary differs')
        probe = load(small / 'resume-ballasted-data.json')
        status = load(small / 'resume-ballasted-status.json')
        errors, movement, _ = self.cg.status_verdict(status, self.layout, 'ssd-2', 'disabled')
        need(not errors and not movement and self.cg.role_identity(status) == self.settle_expected_roles, 'small census role/health invalid')
        verified = verify_ballasted_probe(probe, status, load(self.fixtures['small']), sha(self.fixtures['small']),
                                          self.identity, self.pr, 'small')
        need(verified == load(small / 'resume-data-verified.json'), 'small census proof differs')
        self.preclear['small'] = verified
        self.frozen_version_floor = probe['recheck']['read_version']
        self.prior_continuation, self.prior_continuation_hashes = root, files(root)
        self.pins.update(self.prior_continuation_hashes)

    def frozen_snapshot(self, a, fixtures):
        # Empty scratch probe is DD/provenance ONLY, never a placement receipt.
        fixture = self.prior / 'qualification/original-small/fixture.json'
        _, probe = self.pp_cmd(a, 'frozen-dd-snapshot', 'probe', 'benchfetch/control-small', fixture)
        need(probe.get('format') == self.pr.PROBE_FORMAT and probe.get('keys') == [], 'DD snapshot scratch range is not empty')
        prefix = bytes.fromhex(probe['store_prefix_hex'])
        boundaries = [bytes.fromhex(x) for x in probe['boundaries_hex']]
        brange = [bytes.fromhex(x) for x in probe['boundary_range_hex']]
        keys = probe['keys']
        errors = []
        self.pr.probe_integrity(probe, prefix, boundaries, brange, errors)
        errors += self.pr.provenance_errors(probe, prefix, sha(fixture), self.identity)
        small = self.preclear['small']
        need(not errors and not keys and prefix == self.pr.pack(['benchfetch', 'control', 'small']) and
             probe['read_version'] >= self.frozen_version_floor and
             (probe['move_keys_lock']['owner_hex_at_v1'], probe['move_keys_lock']['write_token_hex_at_v1']) ==
             (small['owner'], small['token']), 'fresh empty-scratch DD epoch differs from valid small census')
        status, movement = self.status(a, 'frozen-dd-snapshot-status', 'disabled')
        need(not movement and self.cg.role_identity(status) == self.settle_expected_roles, 'frozen DD snapshot roles/recovery changed')
        self.frozen_epoch = (small['owner'], small['token'])
        self.frozen_version_floor = probe['recheck']['read_version']
        dump(a / 'frozen-dd-snapshot-verified.json', dict(dd_provenance_only=True, not_placement=True,
             read_version=probe['read_version'], recheck_read_version=self.frozen_version_floor, owner=small['owner'], token=small['token']))

    def before_clear(self, a, fixtures):
        # Census every store before clearing any ballast. These are continuity
        # diagnostics with ballast present, never placement/selection receipts.
        for name, fixture in fixtures.items():
            b = a / name
            _, probe = self.pp_cmd(b, 'resume-ballasted-data', 'probe', 'benchfetch/' + name, fixture)
            status, movement = self.status(b, 'resume-ballasted-status', 'disabled')
            need(not movement and self.cg.role_identity(status) == self.settle_expected_roles, 'roles/recovery changed before resume data census')
            self.preclear[name] = verify_ballasted_probe(probe, status, load(fixture), sha(fixture), self.identity, self.pr, name)
            dump(b / 'resume-data-verified.json', self.preclear[name])
        need(len({(r['owner'], r['token']) for r in self.preclear.values()}) == 1, 'DD epoch changed across resume preclear census')

    def probe(self, folder, name, store, fixture, **kwargs):
        result = super().probe(folder, name, store, fixture, **kwargs)
        if self.preclear:
            raw = next(iter(self.preclear.values()))
            receipt = result[1]
            need((receipt['move_keys_owner_hex'], receipt['move_keys_write_token_hex']) ==
                 (raw['owner'], raw['token']), 'DD epoch changed between preclear and canonical probe')
            same_store = self.preclear.get(store.rsplit('/', 1)[-1])
            if same_store is not None:
                need(receipt['key_servers_sha256'] == same_store['digest'], 'same-store keyServers changed after census')
            if getattr(self, 'frozen_epoch', None) is not None:
                need(receipt['read_version'] >= self.frozen_version_floor, 'frozen continuation read versions went backwards')
                self.frozen_version_floor = receipt['provenance']['recheck']['read_version']
            need(result[3] == self.settle_expected_roles, 'roles/recovery changed between preclear and canonical probe')
        return result

    def execute(self):
        if not self.frozen:
            need(self.settle_deadline - time.time() >= 120, 'original settle timeout expired before continuation start')
        mkdir(self.out)
        continuity = self.out / 'continuity.json'
        dump(continuity, dict(same_attempt=1, ballast_mib_per_store=256, no_seed_write_or_qualification_replay=True,
            original_output=str(self.prior), original_files_sha256=self.prior_hashes,
            original_invocation_sha256=self.a.expect_prior_invocation_sha256,
            prior_p0_sha256=self.a.ack_prior_p0_sha256, current_p0_sha256=self.a.ack_current_p0_sha256,
            original_settle_deadline_epoch=self.settle_deadline, original_timeout_seconds=self.original_timeout,
            correction='QoS backlog/missing moving_data are pending; healthy zero must hold120s before original deadline',
            frozen_after_preclear_1007=self.frozen,
            dropped_extra_preclear_censuses=['split', 'planskip'] if self.frozen else [],
            prior_continuation=str(getattr(self, 'prior_continuation', '')),
            prior_continuation_files_sha256=getattr(self, 'prior_continuation_hashes', {}), argv=sys.argv))
        self.pins[str(continuity)] = sha(continuity)
        self.extra_seal_paths = [continuity, Path(__file__).absolute(), *map(Path, self.prior_hashes),
                                 *map(Path, getattr(self, 'prior_continuation_hashes', {}))]
        a = mkdir(self.out / 'attempt-1')
        for name in p0.STORES:
            mkdir(a / name)
        passed, a, fixtures, receipts, role = self.complete_attempt(1, 256, a, self.fixtures,
            deadline_epoch=self.settle_deadline, already_frozen=self.frozen,
            before_clear=self.frozen_snapshot if self.frozen else self.before_clear)
        if passed:
            return self.finish(1, a, fixtures, receipts, role)
        # Only the interrupted attempt is resumed without writes. The original
        # conditional attempts2/3 remain exactly the preregistered sequence.
        self.preclear = {}
        self.frozen_epoch = None
        del self.settle_expected_roles
        for k, mib in ((2, 512), (3, 1024)):
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
    ap.add_argument('--prior-output', required=True)
    ap.add_argument('--fdbcli', default='/usr/bin/fdbcli')
    ap.add_argument('--frozen-after-preclear-1007', action='store_true')
    ap.add_argument('--prior-continuation')
    ap.add_argument('--expect-prior-continuity-sha256')
    ap.add_argument('--ack-failed-continuation-p0-sha256')
    ap.add_argument('--ack-failed-continuation-resume-sha256')
    ap.add_argument('--settle-timeout-seconds', type=int, default=1200)
    for name in ('cluster', 'placementprobe', 'fetch', 'planskip'):
        ap.add_argument('--expect-' + name + '-sha256', required=True)
    for name in ('ack-prior-p0-sha256', 'ack-current-p0-sha256', 'expect-prior-invocation-sha256'):
        ap.add_argument('--' + name, required=True)
    args = ap.parse_args(argv)
    operation = None
    try:
        need(args.execute, '--execute required')
        for name in ('ack_prior_p0_sha256', 'ack_current_p0_sha256', 'expect_prior_invocation_sha256'):
            need(re.fullmatch('[0-9a-f]{64}', getattr(args, name)) is not None, 'invalid literal SHA: ' + name)
        operation = Resume(args)
        return operation.execute()
    except (Failure, OSError, ValueError, KeyError, ImportError) as exc:
        print('STOP: ' + str(exc), file=sys.stderr)
        if operation is not None and operation.out.exists():
            dump(operation.out / 'failure.json', dict(error=str(exc), continue_to_timing=False, same_attempt=1,
                no_deadline_extension=True, no_seed_or_ballast_write_replay=True))
        return 2


if __name__ == '__main__':
    sys.exit(main())
