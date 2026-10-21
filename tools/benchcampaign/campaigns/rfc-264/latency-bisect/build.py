#!/usr/bin/env python3
"""Build interiors sequentially in a new dedicated worktree, only with approval."""
import argparse
import os
from pathlib import Path
import shutil
import subprocess
import tarfile

from common import ROOT, PLAN, dump, load, path, sha, utc


def git(wt, *args):
    return subprocess.check_output(['git', '-C', str(wt), *args], text=True).rstrip('\n')


def main():
    if not __debug__:
        raise RuntimeError('Run without -O/PYTHONOPTIMIZE')
    parser = argparse.ArgumentParser()
    parser.add_argument('--approved-build-window', required=True, help='owner approval reference')
    args = parser.parse_args()
    p = load()
    if not args.approved_build_window.strip():
        parser.error('nonempty approval reference required')
    repo, wt = path(p['repository']), path(p['worktree'])
    if wt != ROOT.parent / 'worktrees/rfc-264-latency':
        raise RuntimeError('unexpected dedicated worktree path')
    if wt.exists() or (ROOT / 'build-manifest.json').exists():
        raise RuntimeError('fresh build required; preserve existing worktree/manifest, obtain new approval')
    ROOT.mkdir(parents=True, exist_ok=True)
    archive = path(p['harness_archive'])
    if sha(archive) != p['harness_id']:
        raise RuntimeError('frozen amended harness archive changed')
    # Build window is owner-coordinated; timing lock is not acquired.
    arms = [a for a in p['arms'] if a.get('build')]
    subprocess.run(['git', '-C', str(repo), 'worktree', 'add', '--detach', str(wt), arms[0]['revision']], check=True)
    (ROOT / 'bin').mkdir(exist_ok=True)
    (ROOT / 'build-logs').mkdir(exist_ok=True)
    manifest = {'plan_sha256': sha(PLAN), 'approval': args.approved_build_window,
                'harness_id': p['harness_id'], 'builds': [], 'complete': False}
    b = p['build']
    wrapper = ['nice', '-n', str(b['nice']), 'taskset', '-c', b['cpus']]
    startup = ['bazelisk', '--batch', '--output_user_root=' + str(ROOT / 'cache/bazel')]
    options = ['--repository_cache=' + str(path(b['repository_cache'])),
               '--disk_cache=' + str(path(b['disk_cache'])), '--lockfile_mode=error']
    previous_overlay = False
    for arm in arms:
        if previous_overlay:
            changed = git(wt, 'diff', '--name-only').splitlines()
            if any(not name.startswith('tools/clientbench/') for name in changed):
                raise RuntimeError('unexpected non-harness changes in dedicated worktree')
            subprocess.run(['git', '-C', str(wt), 'restore', '--worktree', '--source=HEAD', '--', 'tools/clientbench'], check=True)
            subprocess.run(['git', '-C', str(wt), 'clean', '-fd', '--', 'tools/clientbench'], check=True)
        if git(wt, 'status', '--porcelain', '--untracked-files=normal'):
            raise RuntimeError('dedicated worktree is not clean before checkout')
        subprocess.run(['git', '-C', str(wt), 'checkout', '--detach', arm['revision']], check=True)
        if git(wt, 'rev-parse', 'HEAD') != arm['revision']:
            raise RuntimeError('checkout revision mismatch')
        shutil.rmtree(wt / 'tools/clientbench')
        harness = {}
        with tarfile.open(archive, 'r:gz') as tar:
            for member in tar.getmembers():
                if member.isdir():
                    continue
                name = Path(member.name)
                if not member.isfile() or name.is_absolute() or '..' in name.parts or name.parts[:2] != ('tools', 'clientbench'):
                    raise RuntimeError('unsafe frozen archive member: ' + member.name)
                dest = wt / name
                dest.parent.mkdir(parents=True, exist_ok=True)
                dest.write_bytes(tar.extractfile(member).read())
                harness[str(name)] = sha(dest)
        if len(harness) != p['harness_regular_files']:
            raise RuntimeError('harness member count changed')
        def inventory():
            return {str(f.relative_to(wt)): sha(f) for f in (wt / 'tools/clientbench').rglob('*') if f.is_file()}
        if inventory() != harness:
            raise RuntimeError('frozen harness inventory differs')
        before_diff = git(wt, 'diff', '--binary')
        before_status = git(wt, 'status', '--porcelain', '--untracked-files=normal')
        if any(not line[3:].startswith('tools/clientbench/') for line in before_status.splitlines()):
            raise RuntimeError('overlay changed production files')
        started = utc()
        argv = wrapper + startup + ['build', *options, '--jobs=' + str(b['jobs']), '--local_resources=cpu=10', b['target']]
        with open(ROOT / 'build-logs' / (arm['id'] + '.log'), 'w') as log:
            subprocess.run(argv, cwd=wt, check=True, stdout=log, stderr=subprocess.STDOUT)
        with open(ROOT / 'build-logs' / (arm['id'] + '-cquery.log'), 'w') as log:
            query = wrapper + startup + ['cquery', *options, '--output=files', b['target']]
            output = subprocess.check_output(query, cwd=wt, stderr=log, text=True).strip()
        source, dest = wt / output, path(arm['binary'])
        if not source.is_file():
            raise RuntimeError('cquery did not return one executable: ' + output)
        subprocess.run(wrapper + ['install', '-m755', str(source), str(dest)], check=True)
        version = subprocess.check_output(wrapper + ['go', 'version', '-m', str(dest)], text=True).strip()
        if not version.splitlines()[0].endswith('go1.26.9'):
            raise RuntimeError('unexpected Go compiler: ' + version)
        if (sha(archive) != p['harness_id'] or inventory() != harness or git(wt, 'diff', '--binary') != before_diff or
                git(wt, 'status', '--porcelain', '--untracked-files=normal') != before_status):
            raise RuntimeError('sources changed during build')
        record = {'id': arm['id'], 'source_revision': arm['revision'], 'binary': str(dest), 'sha256': sha(dest),
                  'harness_files_sha256': harness, 'go_version_m': version, 'argv': argv, 'cquery_argv': query,
                  'started_utc': started, 'finished_utc': utc()}
        manifest['builds'].append(record)
        dump(ROOT / 'build-manifest.json', manifest)
        print(arm['id'], record['sha256'], flush=True)
        previous_overlay = True
    manifest['complete'] = len(manifest['builds']) == 10
    dump(ROOT / 'build-manifest.json', manifest)
    if not manifest['complete']:
        raise RuntimeError('incomplete interior build population')
    print('Ten interiors built; no tests or timings run. Worktree retained.')

if __name__ == '__main__':
    main()
