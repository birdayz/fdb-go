#!/usr/bin/env python3
"""Read-only hcloud inventory check: exact pre-existing resource-kind/ID set.

No labels or mutable status fields determine success. Every kind must list
successfully, and every before/after ID set must be identical. JSON snapshots,
ID lists and unified diffs stay in an exclusive directory outside Git.
"""
import argparse
import difflib
import json
import os
from pathlib import Path
import subprocess
import sys

KINDS = ('server', 'network', 'firewall', 'ssh-key', 'volume', 'primary-ip',
         'floating-ip', 'load-balancer', 'placement-group', 'certificate')


def ids(value, kind):
    if not isinstance(value, list):
        raise ValueError(f'{kind}: expected a JSON list')
    result = []
    for item in value:
        rid = item.get('id')
        if not isinstance(rid, int):
            raise ValueError(f'{kind}: missing numeric ID')
        result.append(rid)
    if len(result) != len(set(result)):
        raise ValueError(f'{kind}: duplicate IDs')
    return sorted(result)


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--inventory', required=True, type=Path)
    p.add_argument('--output-dir', required=True, type=Path)
    p.add_argument('--hcloud', default='hcloud')
    args = p.parse_args()
    out = args.output_dir.resolve()
    for parent in (out, *out.parents):
        if (parent / '.git').exists():
            raise ValueError('output must be outside every Git worktree')
    os.umask(0o077)
    out.mkdir(mode=0o700, parents=True, exist_ok=False)
    failed = False
    summary = {'read_only': True, 'kinds': {}, 'passed': False}
    total = 0
    for kind in KINDS:
        try:
            before = ids(json.loads((args.inventory / f'before-{kind}.json').read_text()), kind)
            argv = [args.hcloud, kind, 'list', '-o', 'json']
            proc = subprocess.run(argv, text=True, capture_output=True, timeout=120)
            (out / f'after-{kind}.stderr').write_text(proc.stderr)
            if proc.returncode:
                raise ValueError(f'{kind}: list failed ({proc.returncode}): {proc.stderr.strip()}')
            (out / f'after-{kind}.json').write_text(proc.stdout)
            after = ids(json.loads(proc.stdout), kind)
            a = [f'{rid}\n' for rid in before]
            b = [f'{rid}\n' for rid in after]
            (out / f'before-{kind}.ids').write_text(''.join(a))
            (out / f'after-{kind}.ids').write_text(''.join(b))
            delta = ''.join(difflib.unified_diff(a, b, fromfile=f'before-{kind}', tofile=f'after-{kind}'))
            (out / f'{kind}.diff').write_text(delta)
            same = before == after
            summary['kinds'][kind] = {'before_ids': before, 'after_ids': after, 'passed': same,
                                      'missing': sorted(set(before) - set(after)),
                                      'unexpected': sorted(set(after) - set(before))}
            total += len(before)
            failed |= not same
            print(f'{"PASS" if same else "FAIL"} {kind}: before={len(before)} after={len(after)}')
            if delta:
                print(delta, file=sys.stderr)
        except Exception as exc:
            failed = True
            summary['kinds'][kind] = {'passed': False, 'error': str(exc)}
            print(f'FAIL LOUDLY: {exc}', file=sys.stderr)
    summary['passed'] = not failed
    summary['protected_resource_count'] = total
    (out / 'summary.json').write_text(json.dumps(summary, indent=2) + '\n')
    if failed:
        print('FAIL LOUDLY: project inventory differs or could not be read; do not declare teardown complete.', file=sys.stderr)
        return 1
    print(f'PASS: exactly the pre-existing set across all {len(KINDS)} resource kinds ({total} IDs).')
    return 0


if __name__ == '__main__':
    try:
        sys.exit(main())
    except Exception as exc:
        print(f'FAIL LOUDLY: {exc}', file=sys.stderr)
        sys.exit(2)
