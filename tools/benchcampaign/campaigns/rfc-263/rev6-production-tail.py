#!/usr/bin/env python3
"""Bounded production-only RFC263 tail campaign; never builds or starts containers.

The parent supervises a separate process group, including benchmark children.
All inputs, failures, CSVs and traces remain in a new durable evidence directory.
--self-check exercises only pure guard/plan assertions; it launches no benchmark.
"""
import argparse
import datetime as dt
import fcntl
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import re
import shutil
import signal
import subprocess
import sys
import time
import traceback

ROOT = Path(__file__).resolve().parents[4]
HERE = Path(__file__).resolve().parent
CPUS = {0, 1, 6, 7, 11, 12, 13, 18, 19, 23}
AFF = '2,3,14,15'
H = 'cfc3a33191ec10b4e0955b3f0f192b1ba3be554104ea61016c2a6e60aabbf400'
PRIMARY = '14146a3faf65c5844926a563b9f7c03b98bee7f39f4aaa0dbf49804f0a246c25'
CLUSTER_SHA = 'fba1be7844c7c9f82471b6cb50b4b6a731ab08776c5bd502d593a2c827f1e62c'
BINARIES = {
    'baseline': '2dd943fdc9d58ff136449a181189ba5843465e86e6ae696d616a9b1bac4a439e',
    'rev4': '2f0993bd332a9d7085b1c1053659b68867286de95ad1cbe2bf91fe327f088e95',
}
SOURCES = {
    'baseline': 'fa8725f1dcf6ff3ac66825ab6e3e819efbc0abcc',
    'rev4': 'tree:61d2597e0231dfbedcd898a27c078c3412efaf6a',
}


def sha(path):
    h = hashlib.sha256()
    with Path(path).open('rb') as f:
        for block in iter(lambda: f.read(1024 * 1024), b''):
            h.update(block)
    return h.hexdigest()


def save(path, value):
    Path(path).write_text(json.dumps(value, indent=2, sort_keys=True) + '\n')


def utc():
    return dt.datetime.now(dt.timezone.utc).isoformat()


def bazel_action(argv):
    if not argv:
        return False
    name = Path(argv[0]).name
    # An idle Java A-server is allowed; clients, action wrappers and tools are not.
    if name.startswith('bazel(') or name == 'java':
        return False
    return (name in {'bazel', 'bazelisk', 'linux-sandbox', 'process-wrapper',
                     'compile', 'link', 'asm', 'cgo', 'gcc', 'g++', 'cc1', 'cc1plus', 'clang'}
            or name.startswith('bazel-')
            or ('bazel-out/' in argv[0] and ('_test' in name or name.endswith('.test')))
            or any('GoCompilePkg' in arg for arg in argv[1:]))


def forbidden_container(row):
    text = ' '.join(str(row.get(k, '')) for k in ('Names', 'Image', 'Labels')).lower()
    return any(token in text for token in ('testcontainers', 'ryuk', 'foundationdb'))


def plan(extra=False):
    rows = [('diagnostics', 'range100', 8, 20000, 0, arm, 'trace')
            for arm in ('baseline', 'rev4', 'production')]
    populations = [('range100', 8, 20000), ('range100', 32, 500000)]
    if extra:
        populations += [('get100', 8, 20000), ('set100', 8, 20000)]
    for workload, workers, iterations in populations:
        for block in range(6):
            order = ('baseline', 'production') if block % 2 == 0 else ('production', 'baseline')
            rows += [('timing', workload, workers, iterations, block, arm, 'none') for arm in order]
    return rows


def durable_evidence(path):
    return not any(path.is_relative_to(p) for p in (ROOT, Path('/tmp'), Path('/dev/shm'), Path('/run')))


def complete_population(completed, expected):
    return bool(expected) and len(completed) == len(expected) and set(completed) == set(expected)


def run_identity(row):
    population, workload, workers, iterations, block, arm, profile = row
    phase = 'tail-diag-trace-w8' if profile == 'trace' else f'production-{workload}-w{workers}'
    return phase, f'{phase}-{workload}-p{block}-{arm}'


def gate(evidence):
    processes, bad = [], []
    for path in Path('/proc').glob('[0-9]*/cmdline'):
        try:
            argv = path.read_bytes().decode(errors='replace').rstrip('\0').split('\0')
        except (FileNotFoundError, PermissionError, ProcessLookupError):
            continue
        if argv == ['']:
            continue
        processes.append({'pid': int(path.parent.name), 'argv': argv})
        if bazel_action(argv):
            bad.append(processes[-1])
    if len(processes) < 10:
        raise RuntimeError('vacuous /proc process population')
    docker = subprocess.run(['docker', 'ps', '--no-trunc', '--format', '{{json .}}'],
                            text=True, capture_output=True, timeout=10)
    if docker.returncode:
        raise RuntimeError('cannot verify Docker population: ' + docker.stderr)
    containers = [json.loads(line) for line in docker.stdout.splitlines() if line]
    load = Path('/proc/loadavg').read_text().strip()
    record = {'utc': utc(), 'loadavg': load, 'processes': processes, 'containers': containers,
              'bazel_actions': bad, 'forbidden_containers': [r for r in containers if forbidden_container(r)]}
    with (evidence / 'gates.jsonl').open('a') as out:
        out.write(json.dumps(record) + '\n')
    if bad or record['forbidden_containers']:
        raise RuntimeError('active Bazel action or FDB/testcontainers container; see gates.jsonl')
    return float(load.split()[0])


def remaining(args):
    return args.deadline - time.monotonic()


def wait_gate(args, settled=False):
    quiet_since = None
    while True:
        if remaining(args) < 280:
            raise RuntimeError('insufficient deadline margin for next invocation and preservation')
        load = gate(args.evidence)
        if load <= 7.5:
            quiet_since = time.monotonic() if quiet_since is None else quiet_since
            if not settled or time.monotonic() - quiet_since >= 60:
                return
        else:
            quiet_since = None
        time.sleep(5)


def worker(args):
    evidence = args.evidence
    evidence.mkdir(parents=True, exist_ok=False)
    inputs = evidence / 'inputs'
    inputs.mkdir()
    status = {'started_utc': utc(), 'completed': [], 'planned_runs': len(plan(args.include_get_set)),
              'mandatory_complete': False, 'complete': False, 'analysis_complete': False}
    save(evidence / 'status.json', status)
    frozen = {}
    try:
        if os.getpriority(os.PRIO_PROCESS, 0) != 19 or set(os.sched_getaffinity(0)) != CPUS:
            raise RuntimeError('controller requires nice19 and reserved CPU mask')
        if not re.fullmatch('[0-9a-f]{40}', args.production_sha):
            raise RuntimeError('production-sha must be a full commit SHA')
        subprocess.run(['git', 'cat-file', '-e', args.production_sha + '^{commit}'], cwd=ROOT, check=True)
        subprocess.run(['git', 'diff', '--exit-code', args.production_sha, '--', 'pkg/fdbgo/client',
                        'tools/clientbench', 'go.mod', 'go.sum', 'MODULE.bazel', '.bazelrc'],
                       cwd=ROOT, check=True, stdout=(inputs / 'production-input-diff.txt').open('w'))
        if any(k.startswith('FDB_NETWORK_OPTION_') for k in os.environ):
            raise RuntimeError('forbidden inherited FDB network option')
        scripts = [Path(__file__).resolve(), HERE / 'rev5-primary.py', HERE / 'rev5-tail-analyze.py']
        if sha(scripts[1]) != PRIMARY or sha(args.phase1_evidence / 'harness.tar.gz') != H:
            raise RuntimeError('frozen helper/harness mismatch')
        if sha(args.cluster_file) != CLUSTER_SHA or sha(args.cluster_reference) != CLUSTER_SHA:
            raise RuntimeError('native cluster identity changed')
        for script in scripts:
            shutil.copy2(script, inputs / script.name)
        # Bootstrap the helper exclusively in NEW evidence, never its old default path.
        shutil.copy2(args.phase1_evidence / 'harness.tar.gz', evidence / 'harness.tar.gz')
        (evidence / 'baseline-revision.txt').write_text(SOURCES['baseline'] + '\n')
        tree = subprocess.check_output(['git', 'rev-parse', args.production_sha + '^{tree}'], cwd=ROOT, text=True).strip()
        (evidence / 'candidate-tree.txt').write_text(tree + '\n')
        (evidence / 'bin').mkdir()
        expected = dict(BINARIES, production=sha(args.production_binary))
        origins = {arm: args.prior_tail_evidence / 'bin' / arm for arm in BINARIES}
        origins['production'] = args.production_binary
        for arm, origin in origins.items():
            if sha(origin) != expected[arm]:
                raise RuntimeError('binary identity mismatch: ' + str(origin))
            dest = evidence / 'bin' / arm
            shutil.copy2(origin, dest)
            dest.chmod(0o755)
            if sha(dest) != expected[arm]:
                raise RuntimeError('binary copy mismatch')
        for path in [*scripts, *origins.values(), *list(inputs.glob('*.py')),
                     *list((evidence / 'bin').iterdir()), evidence / 'harness.tar.gz',
                     args.cluster_file, args.cluster_reference, ROOT / 'go.mod', ROOT / '.bazelrc']:
            frozen[str(path.resolve())] = sha(path)
        for path in [ROOT / 'user.bazelrc', Path.home() / '.bazelrc', Path('/etc/bazel.bazelrc')]:
            frozen[str(path)] = sha(path) if path.is_file() else None
        save(inputs / 'manifest.json', {'hashes': frozen, 'sources': dict(SOURCES, production=args.production_sha),
             'binary_sha256': expected, 'harness_id': H, 'cluster': str(args.cluster_file), 'affinity': AFF,
             'controller_affinity': sorted(CPUS), 'nice': 19, 'deadline_monotonic': args.deadline,
             'plan': plan(args.include_get_set), 'production_binary_origin': str(args.production_binary),
             'git_head': subprocess.check_output(['git', 'rev-parse', 'HEAD'], cwd=ROOT, text=True).strip(),
             'git_status': subprocess.check_output(['git', 'status', '--porcelain=v1'], cwd=ROOT, text=True)})
        os.environ['RFC263_PHASE1_EVIDENCE'] = str(evidence)
        spec = importlib.util.spec_from_file_location('frozen_primary', inputs / 'rev5-primary.py')
        c = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(c)
        c.E, c.S, c.AFF = evidence, dict(SOURCES, production=args.production_sha), AFF
        c.ENV = dict(os.environ, GOMAXPROCS='4', GOGC='100', GOMEMLIMIT='off', GODEBUG='')
        c.CLUSTER, c.H = str(args.cluster_file), H
        save(inputs / 'runtime.json', {'env': {k: c.ENV[k] for k in ('GOMAXPROCS', 'GOGC', 'GOMEMLIMIT', 'GODEBUG')},
                                     'python': sys.version, 'uname': list(os.uname()), 'started_utc': utc()})
        rows = {'diagnostics': [], 'timing': []}
        for population in rows:
            (evidence / population / 'runs').mkdir(parents=True)
            # one() resolves E/bin, so use NEW population-local symlinks to frozen bins.
            (evidence / population / 'bin').symlink_to(evidence / 'bin', target_is_directory=True)
        with args.lock_file.open('a') as lock:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
            wait_gate(args, settled=True)
            for population, workload, workers, iterations, block, arm, profile in plan(args.include_get_set):
                if remaining(args) < 280:
                    status['stopped_before'] = [population, workload, workers, block, arm]
                    break
                wait_gate(args)
                c.E = evidence / population
                c.RUNS = c.E / 'runs'
                phase, name = run_identity((population, workload, workers, iterations, block, arm, profile))
                subprocess.run(['uptime'], stdout=(c.RUNS / (name + '.uptime-before.txt')).open('w'), check=True)
                try:
                    row = c.one(phase, workload, block, arm, workers=workers, iterations=iterations, profile=profile)
                    summary = json.loads((c.RUNS / row['run'] / 'summary.json').read_text())
                    if summary['backend']['backend'] != 'go' or summary['backend']['native_library']:
                        raise RuntimeError('production campaign requires the pure-Go backend')
                    if summary['warmup_completed'] != 2000 or summary['warmup_errors'] != 0:
                        raise RuntimeError('incomplete warmup population')
                    rows[population].append(row)
                    status['completed'].append(row['run'])
                    status['mandatory_complete'] = len(status['completed']) >= 27
                finally:
                    subprocess.run(['uptime'], stdout=(c.RUNS / (name + '.uptime-after.txt')).open('w'), check=True)
                    gate(evidence)
                    save(evidence / 'status.json', status)
            status['complete'] = complete_population(status['completed'], [run_identity(r)[1] for r in plan(args.include_get_set)])
            status['timing_released_utc'] = utc()
        if not status['complete']:
            raise RuntimeError('requested campaign incomplete; partial artifacts retained, no green verdict')
        # No decoder builds. Quantiles/full histograms are derived only AFTER timing ownership.
        for population in rows:
            root = evidence / population
            if rows[population]:
                with (root / 'analysis.stdout.log').open('w') as out:
                    subprocess.run([sys.executable, str(inputs / 'rev5-tail-analyze.py'), str(root),
                                    '--output-prefix', population], stdout=out, stderr=subprocess.STDOUT,
                                   check=True, timeout=max(1, min(120, remaining(args) - 10)))
        status['analysis_complete'] = True
        for path, digest in frozen.items():
            actual = sha(path) if Path(path).is_file() else None
            if actual != digest:
                raise RuntimeError('frozen input drift: ' + path)
        status['hashes_verified_after'] = True
        if not status['mandatory_complete']:
            raise RuntimeError('mandatory 3 diagnostic + 24 range population incomplete')
    except BaseException as exc:
        status['failure'] = repr(exc)
        (evidence / 'failure.txt').write_text(traceback.format_exc())
        raise
    finally:
        status['finished_utc'] = utc()
        save(evidence / 'status.json', status)


def self_check():
    assert bazel_action(['/usr/bin/bazelisk', 'test', '//...'])
    assert bazel_action(['/tmp/compile', '-o', 'x'])
    assert not bazel_action(['bazel(fdb-go-perf)', '-jar', 'A-server.jar'])
    assert not bazel_action(['/usr/bin/earlyoom', '--prefer', 'bazel|compile'])
    assert forbidden_container({'Image': 'testcontainers/ryuk:latest'})
    assert forbidden_container({'Labels': 'org.testcontainers=true'})
    assert forbidden_container({'Image': 'foundationdb/foundationdb:7.3'})
    assert not forbidden_container({'Names': 'bruh-substrate-control-plane', 'Image': 'kindest/node'})
    assert len(plan()) == 27 and len(plan(True)) == 51
    assert not durable_evidence(ROOT) and not durable_evidence(ROOT / 'evidence')
    assert not durable_evidence(Path('/tmp/rfc263-evidence'))
    assert durable_evidence(Path('/evidence/rfc263-control'))
    expected = [run_identity(r)[1] for r in plan()]
    assert len(set(expected)) == 27
    assert complete_population(expected, expected)
    assert not complete_population([], [])
    assert not complete_population(expected[:-1], expected)
    assert not complete_population(expected[:-1] + [expected[0]], expected)
    for workload, workers in [('range100', 8), ('range100', 32), ('get100', 8), ('set100', 8)]:
        rr = [r for r in plan(True) if r[0] == 'timing' and r[1:3] == (workload, workers)]
        assert len(rr) == 12
        assert [r[5] for r in rr[::2]].count('baseline') == 3
    print('SELF_CHECK_OK: guard positive/negative controls; 3 traces, 24 core and 24 optional timing runs')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--evidence', type=Path)
    parser.add_argument('--production-sha')
    parser.add_argument('--production-binary', type=Path)
    parser.add_argument('--phase1-evidence', type=Path, default=os.environ.get('RFC263_PHASE1_EVIDENCE') or None)
    parser.add_argument('--prior-tail-evidence', type=Path, default=os.environ.get('RFC263_PRIOR_TAIL_EVIDENCE') or None)
    parser.add_argument('--cluster-file', type=Path, default=os.environ.get('RFC263_CLUSTER_FILE') or None)
    parser.add_argument('--cluster-reference', type=Path, default=os.environ.get('RFC263_CLUSTER_REFERENCE') or None,
                        help='independent native cluster file; both identities must match the frozen SHA')
    parser.add_argument('--lock-file', type=Path, default=os.environ.get('RFC263_FAST_LOCK') or None)
    parser.add_argument('--minutes', type=int, default=30, choices=range(1, 31))
    parser.add_argument('--deadline-utc', help='absolute ISO8601 deadline including earlier build/shutdown time')
    parser.add_argument('--include-get-set', action='store_true')
    parser.add_argument('--self-check', action='store_true')
    parser.add_argument('--worker', action='store_true', help=argparse.SUPPRESS)
    args = parser.parse_args()
    if args.self_check:
        self_check()
        return
    required = ('evidence', 'production_sha', 'production_binary', 'phase1_evidence',
                'prior_tail_evidence', 'cluster_file', 'cluster_reference', 'lock_file')
    missing = ['--' + name.replace('_', '-') for name in required if getattr(args, name) is None]
    if missing:
        parser.error('required explicit paths/settings: ' + ', '.join(missing))
    for name in required:
        if name != 'production_sha':
            setattr(args, name, getattr(args, name).resolve())
    if not durable_evidence(args.evidence):
        parser.error('evidence must be durable and outside the repository, /tmp, /run and /dev/shm')
    if args.worker:
        args.deadline = float(os.environ['RFC263_TAIL_DEADLINE'])
        worker(args)
        return
    if args.evidence.exists():
        parser.error('refusing existing evidence directory')
    # The supervisor kills the entire group, including a benchmark inside one().
    seconds = args.minutes * 60
    if args.deadline_utc:
        stop = dt.datetime.fromisoformat(args.deadline_utc.replace('Z', '+00:00'))
        if stop.tzinfo is None:
            parser.error('--deadline-utc must include timezone')
        seconds = min(seconds, (stop - dt.datetime.now(dt.timezone.utc)).total_seconds())
    if seconds <= 5:
        parser.error('deadline has no remaining supervision margin')
    env = dict(os.environ, RFC263_TAIL_DEADLINE=str(time.monotonic() + seconds - 5))
    child = subprocess.Popen([sys.executable, str(Path(__file__).resolve()), *sys.argv[1:], '--worker'],
                             env=env, process_group=0)
    def stop_requested(signum, frame):
        raise KeyboardInterrupt('supervisor received signal ' + str(signum))
    signal.signal(signal.SIGTERM, stop_requested)
    try:
        code = child.wait(timeout=seconds - 5)
    except (subprocess.TimeoutExpired, KeyboardInterrupt) as exc:
        os.killpg(child.pid, signal.SIGTERM)
        try:
            child.wait(timeout=2)
        except subprocess.TimeoutExpired:
            os.killpg(child.pid, signal.SIGKILL)
            child.wait(timeout=2)
        if args.evidence.is_dir():
            save(args.evidence / 'supervisor-stop.json', {'utc': utc(), 'failure': repr(exc), 'process_group': child.pid})
        raise SystemExit(124)
    raise SystemExit(code)


if __name__ == '__main__':
    main()
