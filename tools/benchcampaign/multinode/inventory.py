#!/usr/bin/env python3
"""Read-only hcloud project inventory: snapshot before a campaign, verify after it.

snapshot --output-dir DIR
    Lists every resource kind and writes before-<kind>.json plus protect.tsv
    (`kind<TAB>id<TAB>name[<TAB>cidr]`, the network's range as cidr). protect.tsv
    is the private input of provision.py and teardown_guard.py --protect-file.
verify --inventory DIR --output-dir DIR
    Lists every kind again and passes only if each kind's ID set equals the
    snapshot's exactly. Labels and mutable fields do not count. Writes the fresh
    lists, ID lists, unified diffs and summary.json.

Both directories name the project's other resources and must lie outside every
Git work tree; outputs are new 0700 directories with 0600 files. Exit 0 pass,
1 inventory differs or a kind could not be listed, 2 refused.
"""
import argparse
import difflib
import json
from pathlib import Path
import sys

import mn_common as mc

KINDS = mc.PROTECT_KINDS


def new_private_dir(path):
    mc.require_outside_repo(path, 'output dir')
    p = Path(path)
    p.mkdir(mode=0o700, parents=True, exist_ok=False)
    return p


def list_kind(hcloud, kind, runner):
    argv = [hcloud, kind, 'list', '-o', 'json']
    rc, out, err = runner(argv)
    if rc:
        raise mc.GuardError(f'{kind}: list failed ({rc}): {err.strip()}')
    value = mc.parse_json_list(out, f'{kind} list')
    ids(value, kind)
    return value


def ids(value, kind):
    if not isinstance(value, list):
        raise mc.GuardError(f'{kind}: expected a JSON list')
    result = []
    for item in value:
        rid = item.get('id') if isinstance(item, dict) else None
        if not isinstance(rid, int) or isinstance(rid, bool):
            raise mc.GuardError(f'{kind}: missing numeric ID')
        result.append(rid)
    if len(result) != len(set(result)):
        raise mc.GuardError(f'{kind}: duplicate IDs')
    return sorted(result)


def protect_lines(kind, items):
    lines = []
    for item in sorted(items, key=lambda x: x['id']):
        name = item.get('name') or ''
        if '\t' in name or '\n' in name:
            raise mc.GuardError(f'{kind} {item["id"]}: name contains a tab or newline')
        fields = [kind, str(item['id']), name]
        if kind == 'network' and item.get('ip_range'):
            fields.append(item['ip_range'])
        lines.append('\t'.join(fields) + '\n')
    return lines


def snapshot(args, runner=mc.default_runner, out=sys.stdout):
    listed = {kind: list_kind(args.hcloud, kind, runner) for kind in KINDS}
    d = new_private_dir(args.output_dir)
    lines = []
    for kind in KINDS:
        mc.write_private(d / f'before-{kind}.json', json.dumps(listed[kind], indent=2) + '\n')
        lines += protect_lines(kind, listed[kind])
    mc.write_private(d / 'protect.tsv', ''.join(lines))
    print(f'snapshot: {len(lines)} existing resources across {len(KINDS)} kinds', file=out)
    return 0


def verify(args, runner=mc.default_runner, out=sys.stdout, err=sys.stderr):
    mc.require_outside_repo(args.inventory, 'inventory dir')
    d = new_private_dir(args.output_dir)
    summary = dict(read_only=True, kinds={}, passed=False)
    failed, total = False, 0
    for kind in KINDS:
        try:
            baseline = (Path(args.inventory) / f'before-{kind}.json').read_text()
            before = ids(mc.parse_json_list(baseline, f'{kind} baseline'), kind)
            fresh = list_kind(args.hcloud, kind, runner)
            mc.write_private(d / f'after-{kind}.json', json.dumps(fresh, indent=2) + '\n')
            after = ids(fresh, kind)
            a, b = [f'{i}\n' for i in before], [f'{i}\n' for i in after]
            mc.write_private(d / f'before-{kind}.ids', ''.join(a))
            mc.write_private(d / f'after-{kind}.ids', ''.join(b))
            delta = ''.join(difflib.unified_diff(a, b, fromfile=f'before-{kind}', tofile=f'after-{kind}'))
            mc.write_private(d / f'{kind}.diff', delta)
            same = before == after
            summary['kinds'][kind] = dict(passed=same, before=len(before), after=len(after),
                                          missing=sorted(set(before) - set(after)),
                                          unexpected=sorted(set(after) - set(before)))
            total += len(before)
            failed |= not same
            print(f'{"PASS" if same else "FAIL"} {kind}: before={len(before)} after={len(after)}', file=out)
        except (OSError, ValueError, mc.GuardError) as exc:
            failed = True
            summary['kinds'][kind] = dict(passed=False, error=str(exc))
            print(f'FAIL {kind}: {exc}', file=err)
    summary['passed'] = not failed
    summary['protected_resource_count'] = total
    mc.write_private(d / 'summary.json', json.dumps(summary, indent=2) + '\n')
    if failed:
        print('FAIL: the project inventory differs or could not be read; teardown is not complete.', file=err)
        return 1
    print(f'PASS: exactly the pre-existing set across {len(KINDS)} kinds ({total} IDs).', file=out)
    return 0


def parse_args(argv=None):
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = ap.add_subparsers(dest='command', required=True)
    s = sub.add_parser('snapshot')
    s.add_argument('--output-dir', required=True)
    s.add_argument('--hcloud', default='hcloud')
    v = sub.add_parser('verify')
    v.add_argument('--inventory', required=True, help='a snapshot output directory')
    v.add_argument('--output-dir', required=True)
    v.add_argument('--hcloud', default='hcloud')
    return ap.parse_args(argv)


def main(argv=None, runner=mc.default_runner):
    args = parse_args(argv)
    try:
        return (snapshot if args.command == 'snapshot' else verify)(args, runner)
    except (OSError, ValueError, mc.GuardError) as exc:
        print(f'REFUSED: {exc}', file=sys.stderr)
        return 2


if __name__ == '__main__':
    sys.exit(main())
