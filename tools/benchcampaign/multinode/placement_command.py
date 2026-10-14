#!/usr/bin/env python3
"""Placement command for bench_runner_mn.py (R2): one fresh placement receipt for one store.

R2 runs this before the first and after the last process of every M2 case through
its --placement-command JSON argv template, for example:

  ["python3", "-B", "/abs/placement_command.py",
   "--store", "{store}", "--case", "{case}", "--when", "{when}", "--preset", "{preset}",
   "--layout", "{layout}", "--record-format", "{record_format}",
   "--cluster-file", "{cluster_file}", "--expect-cluster-sha256", "<sha256 of the generated cluster file>",
   "--output", "{output}",
   "--placementprobe", "/abs/placementprobe", "--fdbcli", "/usr/bin/fdbcli",
   "--fixture-receipts", "/abs/fixture-receipts.json", "--work-dir", "/abs/placement-work"]

Steps, all outside timing:
  1. `placementprobe probe` (H5) for the store, with the store's fixture receipt
     from --fixture-receipts (a JSON map store -> fixture receipt JSON file);
  2. `fdbcli -C CLUSTER --exec 'status json'`;
  3. placement_receipt.build_receipt with the sealed preset, layout and record
     format (checked against the store: benchfetch/small recordlayer/unsplit,
     benchfetch/split recordlayer/split, benchfetch/planskip recordlayer/unsplit,
     sql/IDX_A opaque/none), the store's expected tuple prefix, the fixture
     receipt's rows, and the preset's cells; written to --output.
The cluster file must hash to --expect-cluster-sha256 (sealed literally in the
template, the generated cluster file pinned in the manifest); the receipt then
verifies the probe's recorded cluster identity and fixture receipt hash against
these files, and placement_receipt.sealed_errors must be empty for exit 0.
Intermediate files go to WORK_DIR/<case>/<when>/; no component of that path
may be a symlink, and its resolved location must lie outside any Git work tree;
every path is created exclusively. Exit 0: receipt written with
integrity; 1: receipt written without integrity, or a step failed after start;
2: refused before running anything. R2 itself compares the receipt with the
sealed one; this command never judges placement acceptance.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import mn_common as mc  # noqa: E402
import placement_receipt as pr  # noqa: E402

# store -> (expected tuple prefix or None, layout, record_format or None)
STORES = {
    'benchfetch/small': (('benchfetch', 'small'), 'recordlayer', 'unsplit'),
    'benchfetch/split': (('benchfetch', 'split'), 'recordlayer', 'split'),
    'benchfetch/planskip': (('benchfetch', 'planskip'), 'recordlayer', 'unsplit'),
    'sql/IDX_A': (None, 'opaque', None),
}
PRESET_OF = {'benchfetch/small': 'benchfetch', 'benchfetch/split': 'benchfetch',
             'benchfetch/planskip': 'planskip', 'sql/IDX_A': 'sql'}
STEP_TIMEOUT_SECONDS = 240


class Refused(ValueError):
    pass


def parse_args(argv=None):
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    for name in ('store', 'case', 'when', 'preset', 'layout', 'cluster-file', 'output', 'placementprobe',
                 'fixture-receipts', 'work-dir'):
        ap.add_argument('--' + name, required=True)
    ap.add_argument('--record-format', required=True, help="unsplit, split, or '' for SQL")
    ap.add_argument('--expect-cluster-sha256', required=True, help='SHA-256 of the pinned generated cluster file')
    ap.add_argument('--fdbcli', default='fdbcli')
    ap.add_argument('--replicas', type=int, default=2)
    return ap.parse_args(argv)


def validate(args):
    if args.store not in STORES:
        raise Refused(f'unknown store {args.store!r}')
    tup, layout, fmt = STORES[args.store]
    if args.layout != layout or (args.record_format or None) != fmt or args.preset != PRESET_OF[args.store]:
        raise Refused(f'{args.store} needs preset {PRESET_OF[args.store]}, layout {layout}, record format {fmt}; got '
                      f'{args.preset}, {args.layout}, {args.record_format or None}')
    if args.when not in ('before', 'after'):
        raise Refused('--when must be before or after')
    if not re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9._-]{0,127}', args.case):
        raise Refused(f'unsafe case name {args.case!r}')
    for path, what in ((args.output, 'output'), (args.work_dir, 'work dir')):
        mc.require_outside_repo(path, what)
    if not re.fullmatch(r'[0-9a-f]{64}', args.expect_cluster_sha256):
        raise Refused('--expect-cluster-sha256 must be 64 lowercase hex digits')
    cluster_bytes = Path(args.cluster_file).read_bytes()
    if hashlib.sha256(cluster_bytes).hexdigest() != args.expect_cluster_sha256:
        raise Refused(f'{args.cluster_file} does not hash to the pinned --expect-cluster-sha256')
    try:
        identity = pr.cluster_identity(cluster_bytes)
    except pr.InputError as exc:
        raise Refused(f'{args.cluster_file}: {exc}') from exc
    if os.path.lexists(args.output):
        raise Refused(f'{args.output} exists; receipts are never overwritten')
    receipts = json.loads(Path(args.fixture_receipts).read_text())
    if not isinstance(receipts, dict) or args.store not in receipts:
        raise Refused(f'--fixture-receipts has no entry for {args.store}')
    fixture_path = Path(receipts[args.store])
    if not fixture_path.is_absolute():
        raise Refused('fixture receipt paths must be absolute (they are pinned by the sealing manifest)')
    fixture = json.loads(fixture_path.read_text())
    if not isinstance(fixture, dict) or fixture.get('phase') != 'fixture' or not isinstance(fixture.get('rows'), int):
        raise Refused(f'{fixture_path} is not a fixture receipt JSON object with rows')
    root = Path(args.work_dir)
    work = root / args.case / args.when
    for part in (root, root / args.case, work):
        if part.is_symlink():
            raise Refused(f'{part} is a symlink; placement work directories must be real directories')
    mc.require_outside_repo(work.resolve(), 'resolved work dir')
    if os.path.lexists(work):
        raise Refused(f'{work} exists; every placement check gets a fresh work directory')
    return tup, fixture_path, fixture, work, identity


def sha(path):
    return hashlib.sha256(Path(path).read_bytes()).hexdigest()


def run(args, runner=mc.default_runner):
    """Returns the receipt (also written to args.output)."""
    tup, fixture_path, fixture, work, identity = validate(args)
    work.mkdir(parents=True)
    os.chmod(work, 0o700)
    mc.require_outside_repo(work.resolve(), 'created work dir')
    probe_path, status_path = work / 'probe.json', work / 'status.json'
    probe_argv = [args.placementprobe, 'probe', '--cluster-file', args.cluster_file, '--store', args.store,
                  '--fixture-receipt', str(fixture_path), '--output', str(probe_path)]
    rc, out, err = runner(probe_argv, timeout=STEP_TIMEOUT_SECONDS)
    mc.dump_json(work / 'probe.argv.json', dict(argv=probe_argv, returncode=rc, stdout=out, stderr=err))
    if rc != 0:
        raise RuntimeError(f'placementprobe probe failed ({rc}): {err.strip()[:500]}')
    status_argv = [args.fdbcli, '-C', args.cluster_file, '--exec', 'status json']
    rc, out, err = runner(status_argv, timeout=STEP_TIMEOUT_SECONDS)
    mc.dump_json(work / 'status.argv.json', dict(argv=status_argv, returncode=rc, stderr=err))
    if rc != 0:
        raise RuntimeError(f'fdbcli status json failed ({rc}): {err.strip()[:500]}')
    status = json.loads(out)
    with open(status_path, 'x') as stream:
        stream.write(out)
    probe = json.loads(probe_path.read_text())
    fmt = args.record_format or None
    receipt = pr.build_receipt(
        status, probe, dict(pr.PRESETS[args.preset]), layout=args.layout, rows=fixture['rows'], record_format=fmt,
        expect_prefix=pr.pack(list(tup)) if tup else None, fixture=fixture, replicas=args.replicas,
        records_per_id=fixture.get('chunks') if args.layout == 'opaque' else None, preset=args.preset,
        fixture_sha256=sha(fixture_path), expect_cluster=identity)
    receipt['inputs'] = dict(status_sha256=sha(status_path), probe_sha256=sha(probe_path),
                             fixture_receipt_sha256=sha(fixture_path), cluster_file_sha256=identity['cluster_file_sha256'])
    receipt['sealed_errors'] = pr.sealed_errors(receipt, identity)
    receipt['placement_command'] = dict(store=args.store, case=args.case, when=args.when, work_dir=str(work))
    mc.dump_json(args.output, receipt)
    return receipt


def main(argv=None, runner=mc.default_runner):
    args = parse_args(argv)
    try:
        receipt = run(args, runner)
    except (Refused, mc.GuardError, OSError, ValueError) as exc:
        if isinstance(exc, (Refused, mc.GuardError)):
            print('REFUSED: ' + str(exc), file=sys.stderr)
            return 2
        print('FAILED: ' + str(exc), file=sys.stderr)
        return 1
    except RuntimeError as exc:
        print('FAILED: ' + str(exc), file=sys.stderr)
        return 1
    print(json.dumps({k: receipt[k] for k in ('integrity_ok', 'passed', 'error_count', 'shared_shards')}, sort_keys=True))
    return 0 if receipt['integrity_ok'] and not receipt['sealed_errors'] else 1


if __name__ == '__main__':
    sys.exit(main())
