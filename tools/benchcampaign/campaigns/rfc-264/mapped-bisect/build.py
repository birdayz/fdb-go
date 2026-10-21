#!/usr/bin/env python3
"""Prepare seven interior binaries only after an explicit build-window approval."""
import argparse
import datetime
import fcntl
import hashlib
import json
import os
import re
from pathlib import Path
import subprocess

CODE_ROOT = Path(__file__).resolve().parent
PLAN = CODE_ROOT / 'plan.json'
ROOT = None


def load_plan():
    def expand(value):
        if isinstance(value, str):
            return re.sub(r'\$\{([A-Z][A-Z0-9_]*)\}', lambda m: os.environ[m[1]], value)
        if isinstance(value, list):
            return [expand(v) for v in value]
        if isinstance(value, dict):
            return {k: expand(v) for k, v in value.items()}
        return value
    return expand(json.loads(PLAN.read_text()))


def sha(path):
    return hashlib.file_digest(open(path, 'rb'), 'sha256').hexdigest()


def run(argv, **kwargs):
    return subprocess.run(argv, check=True, **kwargs)


def main():
    global ROOT
    if not __debug__:
        raise RuntimeError('Python optimization disables safety checks; run without -O/PYTHONOPTIMIZE')
    ap = argparse.ArgumentParser()
    ap.add_argument('--approved-build-window', action='store_true', required=True)
    args = ap.parse_args()
    if not args.approved_build_window:
        ap.error('build window required')
    p = load_plan()
    ROOT = Path(p['root'])
    repo, wt = Path(p['repository']), Path(p['worktree'])
    if not ROOT.is_absolute() or not repo.is_absolute() or not wt.is_absolute():
        raise RuntimeError('artifact root, repository and worktree must be absolute paths')
    if wt.resolve() == repo.resolve() or wt.is_symlink():
        raise RuntimeError('dedicated worktree must not be the source checkout or a symlink')
    ROOT.mkdir(parents=True, exist_ok=True)
    wt.parent.mkdir(parents=True, exist_ok=True)
    Path(p['timing']['lock']).parent.mkdir(parents=True, exist_ok=True)
    if (ROOT / 'build-manifest.json').exists():
        raise RuntimeError('manifest already exists; preserve it and obtain approval before another build')
    lock = open(p['timing']['lock'], 'a+')
    fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
    commits = [a for a in p['arms'] if a.get('build')]
    if wt.exists():
        # The owner authorized this exact detached starting checkout during preparation.
        head = subprocess.check_output(['git', '-C', str(wt), 'rev-parse', 'HEAD'], text=True).strip()
        status = subprocess.check_output(['git', '-C', str(wt), 'status', '--porcelain'], text=True).strip()
        branch = subprocess.run(['git', '-C', str(wt), 'symbolic-ref', '-q', 'HEAD'], capture_output=True)
        if head != p['arms'][-1]['revision'] or status or branch.returncode != 1:
            raise RuntimeError('dedicated worktree is not the clean, detached approved starting checkout')
    else:
        run(['git', '-C', str(repo), 'worktree', 'add', '--detach', str(wt), commits[0]['revision']])
    (ROOT / 'bin').mkdir(exist_ok=True)
    (ROOT / 'build-logs').mkdir(exist_ok=True)
    b = p['build']
    wrapper = ['nice', '-n', str(b['nice']), 'taskset', '-c', ','.join(map(str, b['cpus']))]
    bz = wrapper + ['bazelisk', '--batch']
    manifest = {'plan_sha256': sha(PLAN), 'harness_revision': p['harness_revision'], 'builds': []}
    for arm in commits:
        if subprocess.check_output(['git', '-C', str(wt), 'status', '--porcelain', '--untracked-files=no'], text=True).strip():
            raise RuntimeError('tracked files changed in dedicated worktree; refusing checkout')
        run(['git', '-C', str(wt), 'checkout', '--detach', arm['revision']])
        head = subprocess.check_output(['git', '-C', str(wt), 'rev-parse', 'HEAD'], text=True).strip()
        assert head == arm['revision']
        harness = {}
        for name in p['harness_paths']:
            frozen = subprocess.check_output(['git', '-C', str(repo), 'show', p['harness_revision'] + ':' + name])
            expected = hashlib.sha256(frozen).hexdigest()
            assert sha(wt / name) == expected, f'frozen planskip harness mismatch: {name}'
            harness[name] = expected
        start = datetime.datetime.now(datetime.timezone.utc).isoformat()
        argv = bz + ['build', '--jobs=' + str(b['jobs']), b['target']]
        with open(ROOT / 'build-logs' / (arm['id'] + '.log'), 'w') as log:
            run(argv, cwd=wt, stdout=log, stderr=subprocess.STDOUT)
        with open(ROOT / 'build-logs' / (arm['id'] + '-cquery.log'), 'w') as log:
            path = subprocess.check_output(bz + ['cquery', '--output=files', b['target']], cwd=wt, stderr=log, text=True).strip()
        source = wt / path
        if not source.is_file():
            raise RuntimeError(f'cquery did not return one executable: {path!r}')
        dest = ROOT / arm['binary']
        run(wrapper + ['install', '-m755', str(source), str(dest)])
        version = subprocess.check_output(['go', 'version', '-m', str(dest)], text=True).strip()
        if not version.splitlines()[0].endswith('go1.26.9'):
            raise RuntimeError('unexpected Go SDK: ' + version)
        if subprocess.check_output(['git', '-C', str(wt), 'status', '--porcelain', '--untracked-files=no'], text=True).strip():
            raise RuntimeError('build changed tracked sources')
        record = {'id': arm['id'], 'source_revision': head, 'binary': str(dest), 'sha256': sha(dest),
                  'harness_files_sha256': harness, 'go_version_m': version, 'argv': argv,
                  'started_utc': start, 'finished_utc': datetime.datetime.now(datetime.timezone.utc).isoformat()}
        manifest['builds'].append(record)
        tmp = ROOT / 'build-manifest.json.tmp'
        tmp.write_text(json.dumps(manifest, indent=2) + '\n')
        tmp.replace(ROOT / 'build-manifest.json')
        print(arm['id'], record['sha256'], flush=True)
    assert len(manifest['builds']) == 7
    print('Built seven interiors; no timings run. Dedicated worktree retained for inspection.')


if __name__ == '__main__':
    main()
