#!/usr/bin/env python3
"""Preregistered RFC263 confirmation collection: integrity only, never analysis.

prepare freezes BOTH epochs before collection. run requires separate explicit owner
approval and an absolute <=60-minute deadline. No metrics, quantiles, sums, treatment
comparisons, replacement runs, decoding, builds or containers are launched here.
"""
import argparse
import csv
import datetime as dt
import fcntl
import hashlib
import importlib.util
import json
import math
import os
from pathlib import Path
import random
import resource
import shutil
import signal
import subprocess
import sys
import tarfile
import tempfile
import time
import traceback

sys.dont_write_bytecode = True
ROOT = Path(__file__).resolve().parents[4]
HERE = Path(__file__).resolve().parent
PREREG = '7bf58d7bf2bba737d8087d261b19f0e30bc274b5'
SEED = 263006
CPUS = {0, 1, 6, 7, 11, 12, 13, 18, 19, 23}
AFF = '2,3,14,15'
CLUSTER = None  # Operational paths are configured only by main(), after pure self-checks.
CLUSTER_SHA = 'fba1be7844c7c9f82471b6cb50b4b6a731ab08776c5bd502d593a2c827f1e62c'
LOCK = None
HARNESS = 'cfc3a33191ec10b4e0955b3f0f192b1ba3be554104ea61016c2a6e60aabbf400'
HELPER_SHA = '1bf7581971e612b27cdb3f91ad83b8057a3dc6a50a45990c73f87f6df33398e4'
SOURCES = {'master': 'fa8725f1dcf6ff3ac66825ab6e3e819efbc0abcc',
           'production': 'c2e52f46ab02df0295f272f421b87c3bdb3a3515'}
BINS = {'master': '2dd943fdc9d58ff136449a181189ba5843465e86e6ae696d616a9b1bac4a439e',
        'production': 'ff3b447e7449a1e3dc38d04da15e4fdec15cc4e0840fa808e95fb6544b5f37de'}
FORECAST_SECONDS = 58 * 60  # Upper end of preregistered 52-58min epoch estimate.


def sha(path):
    h = hashlib.sha256()
    with Path(path).open('rb') as f:
        for data in iter(lambda: f.read(1024 * 1024), b''):
            h.update(data)
    return h.hexdigest()


def save(path, data):
    Path(path).write_text(json.dumps(data, indent=2, sort_keys=True) + '\n')


def utc():
    return dt.datetime.now(dt.timezone.utc).isoformat()


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


def schedule():
    rng = random.Random(SEED)
    runs = []
    for group in range(48):
        contrasts = [(kind, order) for kind in ('AB', 'masterAA', 'productionAA') for order in (0, 1)]
        rng.shuffle(contrasts)
        firsts = ['get100'] * 3 + ['set100'] * 3
        rng.shuffle(firsts)
        for superblock, ((kind, order), first) in enumerate(zip(contrasts, firsts)):
            aliases = [('M', 'master'), ('P', 'production')] if kind == 'AB' else [
                ('A', 'master' if kind == 'masterAA' else 'production'),
                ('B', 'master' if kind == 'masterAA' else 'production')]
            if order:
                aliases.reverse()
            workloads = [first, 'set100' if first == 'get100' else 'get100']
            for workload in workloads:
                pair = f'g{group:02d}-s{superblock}-{workload}'
                for member, (alias, binary) in enumerate(aliases):
                    prefix = ('clientbench/rfc263/' + f'{rng.getrandbits(128):032x}').ljust(67, '0') + '/'
                    epoch = 1 + group // 24
                    runs.append({'id': f'e{epoch}-r{len(runs) % 576:04d}', 'epoch': epoch,
                                 'group': group, 'superblock': superblock, 'contrast': kind,
                                 'order': order, 'workload_first': first, 'pair': pair,
                                 'workload': workload, 'member': member, 'alias': alias,
                                 'binary': binary, 'binary_sha256': BINS[binary],
                                 'source': SOURCES[binary], 'prefix': prefix})
    validate_schedule(runs)
    return runs


def validate_schedule(runs):
    require(len(runs) == 1152, 'schedule must contain exactly1152 runs')
    require(len({r['id'] for r in runs}) == 1152, 'duplicate run IDs')
    require(len({r['prefix'] for r in runs}) == 1152, 'duplicate prefixes')
    require(all(len(r['prefix'].encode()) == 68 for r in runs), 'prefix must be68 bytes')
    for epoch in (1, 2):
        rr = [r for r in runs if r['epoch'] == epoch]
        require(len(rr) == 576, 'epoch must contain576 runs')
        for workload in ('get100', 'set100'):
            for kind in ('AB', 'masterAA', 'productionAA'):
                pairs = {}
                for r in rr:
                    if r['workload'] == workload and r['contrast'] == kind:
                        pairs.setdefault(r['pair'], []).append(r)
                require(len(pairs) == 48, 'each epoch/type/workload needs48 pairs')
                require([p[0]['order'] for p in pairs.values()].count(0) == 24, 'first order imbalance')
                for pair in pairs.values():
                    require(len(pair) == 2 and [r['member'] for r in pair] == [0, 1], 'pair integrity')
                    expected_roles = ['master', 'production'] if kind == 'AB' else [
                        'master' if kind == 'masterAA' else 'production'] * 2
                    if pair[0]['order']:
                        expected_roles.reverse()
                    require([r['binary'] for r in pair] == expected_roles, 'contrast role mismatch')
                    if kind != 'AB':
                        require(pair[0]['binary_sha256'] == pair[1]['binary_sha256'] and
                                pair[0]['source'] == pair[1]['source'], 'AA binary/source mismatch')
                    for r in pair:
                        require(r['binary_sha256'] == BINS[r['binary']] and r['source'] == SOURCES[r['binary']],
                                'label/binary mapping mismatch')
    for group in range(48):
        firsts = [r for r in runs if r['group'] == group and r['member'] == 0 and r['workload'] == r['workload_first']]
        require(len(firsts) == 6 and [r['workload_first'] for r in firsts].count('get100') == 3,
                'group must contain three get-first and three set-first superblocks')
        require(all([r['contrast'] for r in firsts].count(k) == 2 for k in ('AB', 'masterAA', 'productionAA')),
                'group contrast imbalance')


def source_tree():
    paths = subprocess.check_output(['git', 'ls-files', '-z', '--cached', '--others', '--exclude-standard'], cwd=ROOT)
    files = {}
    for rel in sorted(set(paths.decode().split('\0')) - {''}):
        p = ROOT / rel
        info = p.lstat()
        data = os.readlink(p).encode() if p.is_symlink() else p.read_bytes()
        files[rel] = {'sha256': hashlib.sha256(data).hexdigest(), 'mode': info.st_mode,
                      'symlink': p.is_symlink()}
    require(len(files) > 1000, 'vacuous input source population')
    configs = {}
    for p in (ROOT / 'user.bazelrc', Path.home() / '.bazelrc', Path('/etc/bazel.bazelrc')):
        configs[str(p)] = sha(p) if p.is_file() else None
    return {'files': files, 'external_configs': configs}


def verify_inputs(evidence, expected):
    require(source_tree() == expected['source_tree'], 'tracked/untracked source/config drift')
    for path, digest in expected['hashes'].items():
        require(sha(path) == digest, 'frozen input drift: ' + path)


def prepare(args):
    e = args.evidence
    e.mkdir(parents=True, exist_ok=False)
    (e / 'inputs').mkdir()
    (e / 'bin').mkdir()
    runs = schedule()
    save(e / 'schedule.json', runs)
    for epoch in (1, 2):
        save(e / f'schedule-epoch{epoch}.json', [r for r in runs if r['epoch'] == epoch])
    helper = HERE / 'rev6-production-tail.py'
    require(args.provenance_ack.is_file() and args.provenance_diff.is_file(), 'provenance ACK/diff receipts required')
    ack = json.loads(args.provenance_ack.read_text())
    require(isinstance(ack, dict) and bool(ack), 'empty provenance ACK')
    shutil.copy2(args.provenance_ack, e / 'inputs' / 'provenance-ack.json')
    shutil.copy2(args.provenance_diff, e / 'inputs' / 'provenance-code-diff.stat')
    require(sha(helper) == HELPER_SHA, 'guard helper changed')
    for script in (Path(__file__).resolve(), helper):
        shutil.copy2(script, e / 'inputs' / script.name)
    for binary, origin in [('master', args.quiet_a / 'bin' / 'baseline'),
                           ('production', args.quiet_a / 'bin' / 'production')]:
        require(sha(origin) == BINS[binary], 'qualified executable mismatch')
        shutil.copy2(origin, e / 'bin' / binary)
        (e / 'bin' / binary).chmod(0o555)
        require(sha(e / 'bin' / binary) == BINS[binary], 'copied executable mismatch')
    require(sha(args.quiet_a / 'harness.tar.gz') == HARNESS, 'harness mismatch')
    shutil.copy2(args.quiet_a / 'harness.tar.gz', e / 'inputs' / 'harness.tar.gz')
    require(sha(CLUSTER) == CLUSTER_SHA and sha(args.cluster_reference) == CLUSTER_SHA, 'cluster changed')
    tree = source_tree()
    save(e / 'inputs' / 'source-tree.json', tree)
    with tarfile.open(e / 'inputs' / 'source.tar.gz', 'w:gz', compresslevel=1) as archive:
        for rel in tree['files']:
            archive.add(ROOT / rel, arcname=rel, recursive=False)
    hashes = {}
    for p in [*list((e / 'inputs').iterdir()), *list((e / 'bin').iterdir()),
              e / 'schedule.json', e / 'schedule-epoch1.json', e / 'schedule-epoch2.json',
              Path(__file__).resolve(), helper, CLUSTER, args.cluster_reference]:
        hashes[str(p.resolve())] = sha(p)
    frozen = {'prepared_utc': utc(), 'preregistration': PREREG, 'seed': SEED, 'hashes': hashes,
              'source_tree': tree, 'sources': SOURCES, 'binaries': BINS, 'harness': HARNESS,
              'source_labels': {'master': 'frozen baseline fa8725f1d', 'production': 'selected production c2e52f46a'},
              'provenance_ack_origin': str(args.provenance_ack.resolve()),
              'provenance_diff_origin': str(args.provenance_diff.resolve()),
              'forecast_seconds_per_epoch': FORECAST_SECONDS, 'forecast_basis': 'preregistered upper58min estimate',
              'git_head': subprocess.check_output(['git', 'rev-parse', 'HEAD'], cwd=ROOT, text=True).strip()}
    save(e / 'frozen.json', frozen)
    verify_inputs(e, frozen)
    print('PREPARED:1152 raw processes;576 per epoch;no measurements launched', flush=True)


def load_helper(e):
    spec = importlib.util.spec_from_file_location('confirm_guard', e / 'inputs' / 'rev6-production-tail.py')
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def deadline_seconds(value, now=None):
    now = now or dt.datetime.now(dt.timezone.utc)
    stop = dt.datetime.fromisoformat(value.replace('Z', '+00:00'))
    require(stop.tzinfo is not None, 'deadline must contain timezone')
    seconds = (stop - now).total_seconds()
    require(0 < seconds <= 3600, 'explicit deadline must be future and at most60min away')
    return seconds


def stat():
    return Path('/proc/stat').read_text().splitlines()[0]


def csv_integrity(path, n):
    require(isinstance(n, int) and not isinstance(n, bool) and n > 0, 'CSV population must be positive')
    seen = set()
    with path.open() as f:
        for row in csv.DictReader(f):
            index = int(row['index'])
            require(0 <= index < n and index not in seen, 'duplicate/out-of-range sample index')
            require(int(row['worker']) == index % 8, 'worker assignment mismatch')
            require(row['phase'] == 'measure' and row['error'] == '' and row['check_status'] == 'ok', 'sample health failure')
            require(int(row['latency_ns']) > 0, 'nonpositive latency')
            seen.add(index)
    require(len(seen) == n, 'empty/incomplete CSV population')


def check_manifest(manifest, row, out):
    config = manifest['config']
    required = {'cluster_file': str(CLUSTER), 'workload': row['workload'], 'workers': 8,
                'iterations': 20000, 'warmup': 2000, 'transaction_timeout_ms': 30000,
                'retry_limit': 10, 'allow_writes': True, 'prefix': row['prefix'], 'profile': 'none',
                'source_revision': row['source'], 'harness_id': HARNESS, 'run_id': row['id'],
                'output': str(out), 'block': row['group']}
    require(all(config.get(k) == v for k, v in required.items()), 'manifest configuration mismatch')
    require(manifest['go_version'] == 'go1.26.9', 'compiler mismatch')
    require(manifest['backend']['executable_sha256'] == row['binary_sha256'] and
            manifest['backend']['backend'] == 'go' and not manifest['backend']['native_library'], 'backend mismatch')


def collect(args):
    e = args.evidence
    epoch = e / f'epoch{args.epoch}'
    epoch.mkdir(exist_ok=False)
    (epoch / 'runs').mkdir()
    health = {'started_utc': utc(), 'epoch': args.epoch, 'completed': [], 'complete': False,
              'authorization': args.authorization, 'deadline_utc': args.deadline_utc}
    save(epoch / 'status.json', health)
    try:
        require(os.getpriority(os.PRIO_PROCESS, 0) == 19 and set(os.sched_getaffinity(0)) == CPUS,
                'controller must use nice19 and reserved CPU mask')
        require(not any(k.startswith('FDB_NETWORK_OPTION_') for k in os.environ), 'inherited network option')
        frozen = json.loads((e / 'frozen.json').read_text())
        require(frozen['preregistration'] == PREREG and frozen['sources'] == SOURCES and frozen['binaries'] == BINS,
                'preregistered identity mismatch')
        require(frozen['hashes'].get(str(CLUSTER.resolve())) == CLUSTER_SHA,
                'cluster path differs from prepared input')
        verify_inputs(e, frozen)
        all_runs = json.loads((e / 'schedule.json').read_text())
        validate_schedule(all_runs)
        rows = json.loads((e / f'schedule-epoch{args.epoch}.json').read_text())
        require(rows == [r for r in all_runs if r['epoch'] == args.epoch] and len(rows) == 576, 'epoch schedule mismatch')
        if args.epoch == 2:
            prior = json.loads((e / 'epoch1' / 'status.json').read_text())
            require(prior['complete'] and len(prior['completed']) == 576, 'epoch1 incomplete')
        helper = load_helper(e)
        env = dict(os.environ, GOMAXPROCS='4', GOGC='100', GOMEMLIMIT='off', GODEBUG='')
        save(epoch / 'runtime.json', {'utc': utc(), 'python': sys.version, 'uname': list(os.uname()),
             'env': {k: env[k] for k in ('GOMAXPROCS', 'GOGC', 'GOMEMLIMIT', 'GODEBUG')},
             'nice': 19, 'controller_affinity': sorted(CPUS), 'benchmark_affinity': AFF})
        with LOCK.open('a') as lock:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
            require(args.deadline - time.monotonic() >= FORECAST_SECONDS,
                    'full576-run conservative58min forecast does not fit; no process launched')
            quiet_since = None
            while quiet_since is None or time.monotonic() - quiet_since < 60:
                require(time.monotonic() < args.deadline - 30, 'deadline during settle')
                load = helper.gate(epoch)
                quiet_since = (quiet_since or time.monotonic()) if load <= 7.5 else None
                time.sleep(5)
            for row in rows:
                while True:
                    while helper.gate(epoch) > 7.5:
                        require(time.monotonic() < args.deadline - 30, 'deadline waiting for load gate')
                        time.sleep(5)
                    require(time.monotonic() < args.deadline - 30, 'incomplete epoch at admission cutoff')
                    time.sleep(2)
                    before_stat = stat()
                    time.sleep(1)
                    after_stat = stat()
                    accepted_load = helper.gate(epoch)
                    if accepted_load <= 7.5:
                        accepted_gate_utc = utc()
                        break
                require(time.monotonic() < args.deadline - 25, 'deadline immediately before launch')
                out = epoch / 'runs' / row['id']
                require(not out.exists(), 'refuse replacement run')
                exe = e / 'bin' / row['binary']
                cmd = ['taskset', '-c', AFF, str(exe), '--cluster-file', str(CLUSTER),
                       '--workload', row['workload'], '--workers', '8', '--iterations', '20000',
                       '--warmup', '2000', '--transaction-timeout-ms', '30000', '--retry-limit', '10',
                       '--allow-writes', '--prefix', row['prefix'], '--output', str(out),
                       '--run-id', row['id'], '--block', str(row['group']), '--source-revision', row['source'],
                       '--harness-id', HARNESS, '--profile', 'none']
                meta = {'schedule': row, 'argv': cmd, 'env': {k: env[k] for k in ('GOMAXPROCS', 'GOGC', 'GOMEMLIMIT', 'GODEBUG')},
                        'started_utc': utc(), 'system_stat_before': before_stat, 'system_stat_after': after_stat,
                        'accepted_gate_utc': accepted_gate_utc, 'accepted_load': accepted_load,
                        'usage_before': list(resource.getrusage(resource.RUSAGE_CHILDREN))}
                receipt = epoch / 'runs' / (row['id'] + '.command.json')
                save(receipt, meta)
                started = time.monotonic_ns()
                try:
                    with (epoch / 'runs' / (row['id'] + '.stdout.log')).open('w') as log:
                        result = subprocess.run(cmd, env=env, stdout=log, stderr=subprocess.STDOUT, timeout=20)
                    meta['returncode'] = result.returncode
                    require(result.returncode == 0, 'benchmark technical failure; no replacement')
                finally:
                    meta['finished_utc'] = utc()
                    meta['duration_upper_ns'] = time.monotonic_ns() - started + math.ceil(2e9 * time.get_clock_info('monotonic').resolution)
                    meta['usage_after'] = list(resource.getrusage(resource.RUSAGE_CHILDREN))
                    save(receipt, meta)
                require(meta['duration_upper_ns'] < 10_000_000_000, 'process crossed10s exposure bound; no replacement')
                summary = json.loads((out / 'summary.json').read_text())
                require(summary['success'] is True and summary['completed'] == 20000 and summary['errors'] == 0,
                        'summary technical population failure')
                check_manifest(json.loads((out / 'manifest.json').read_text()), row, out)
                csv_integrity(out / 'samples.csv', 20000)
                artifacts = [p for p in out.iterdir() if p.is_file()] + [receipt, epoch / 'runs' / (row['id'] + '.stdout.log')]
                save(epoch / 'runs' / (row['id'] + '.sha256.json'),
                     {str(p.relative_to(epoch)): sha(p) for p in artifacts})
                helper.gate(epoch)
                health['completed'].append(row['id'])
                save(epoch / 'status.json', health)
                print(f'HEALTH epoch={args.epoch} completed={len(health["completed"])}/576', flush=True)
            require(health['completed'] == [r['id'] for r in rows] and len(health['completed']) == 576,
                    'nonempty full576-run population guard failed')
            verify_inputs(e, frozen)
            health['complete'] = True
            health['input_hashes_verified_after'] = True
    except BaseException as exc:
        health['failure'] = repr(exc)
        (epoch / 'failure.txt').write_text(traceback.format_exc())
        raise
    finally:
        health['finished_utc'] = utc()
        save(epoch / 'status.json', health)


def self_check():
    runs = schedule()
    require(runs == schedule(), 'seed reproducibility')
    require(not any(r['binary'] == 'production' for r in runs if r['contrast'] == 'masterAA'), 'AA label mapping')
    for broken in ([], runs[:-1], runs[:-1] + [runs[0]]):
        try:
            validate_schedule(broken)
        except RuntimeError:
            pass
        else:
            raise RuntimeError('empty/partial/duplicate schedule accepted')
    bad = [dict(r) for r in runs]
    target = next(r for r in bad if r['contrast'] == 'masterAA')
    target['binary_sha256'] = BINS['production']
    try:
        validate_schedule(bad)
    except RuntimeError:
        pass
    else:
        raise RuntimeError('AA mismatch accepted')
    bad = [dict(r) for r in runs]
    pair_id = next(r['pair'] for r in bad if r['contrast'] == 'masterAA')
    for r in bad:
        if r['pair'] == pair_id:
            r.update(binary='production', binary_sha256=BINS['production'], source=SOURCES['production'])
    try:
        validate_schedule(bad)
    except RuntimeError:
        pass
    else:
        raise RuntimeError('wrong AA source role accepted')
    now = dt.datetime(2026, 1, 1, tzinfo=dt.timezone.utc)
    require(deadline_seconds('2026-01-01T01:00:00Z', now) == 3600, 'date positive control')
    for value in ('2026-01-01T00:00:00Z', '2026-01-01T02:00:00Z', '2026-01-01T00:10:00'):
        try:
            deadline_seconds(value, now)
        except RuntimeError:
            pass
        else:
            raise RuntimeError('invalid deadline accepted')
    require(not durable(ROOT) and not durable(ROOT / 'evidence') and not durable(Path('/tmp/x')), 'path guard')
    with tempfile.TemporaryDirectory(prefix='rfc263-confirm-guard-') as directory:
        path = Path(directory) / 'samples.csv'
        fields = ['index', 'worker', 'phase', 'error', 'check_status', 'latency_ns']
        valid = [dict(index=i, worker=i, phase='measure', error='', check_status='ok', latency_ns=100) for i in range(2)]
        def fixture(rows):
            with path.open('w') as stream:
                writer = csv.DictWriter(stream, fields)
                writer.writeheader()
                writer.writerows(rows)
        fixture(valid)
        csv_integrity(path, 2)
        for n in (0, -1, True):
            try:
                csv_integrity(path, n)
            except RuntimeError:
                pass
            else:
                raise RuntimeError('vacuous CSV population accepted')
        malformed = [[], valid[:1], [valid[0], valid[0]], [dict(valid[0], index=2), valid[1]]]
        malformed += [[dict(valid[0], **{key: value}), valid[1]] for key, value in
                      [('worker', 7), ('phase', 'warmup'), ('error', 'failed'), ('check_status', 'failed'), ('latency_ns', 0)]]
        for rows in malformed:
            fixture(rows)
            try:
                csv_integrity(path, 2)
            except RuntimeError:
                pass
            else:
                raise RuntimeError('invalid CSV population accepted')
    row = runs[0]
    out = Path('/not-a-real-run')
    config = {'cluster_file': str(CLUSTER), 'workload': row['workload'], 'workers': 8,
              'iterations': 20000, 'warmup': 2000, 'transaction_timeout_ms': 30000,
              'retry_limit': 10, 'allow_writes': True, 'prefix': row['prefix'], 'profile': 'none',
              'source_revision': row['source'], 'harness_id': HARNESS, 'run_id': row['id'],
              'output': str(out), 'block': row['group']}
    manifest = {'config': config, 'go_version': 'go1.26.9', 'backend':
                {'executable_sha256': row['binary_sha256'], 'backend': 'go', 'native_library': False}}
    check_manifest(manifest, row, out)
    malformed = [dict(manifest, config={}), dict(manifest, go_version='go1.27.1'),
                 dict(manifest, config=dict(config, iterations=0)), dict(manifest, config=dict(config, profile='trace')),
                 dict(manifest, backend=dict(manifest['backend'], executable_sha256='wrong')),
                 dict(manifest, backend=dict(manifest['backend'], backend='cgo')),
                 dict(manifest, backend=dict(manifest['backend'], native_library=True))]
    for value in malformed:
        try:
            check_manifest(value, row, out)
        except RuntimeError:
            pass
        else:
            raise RuntimeError('invalid manifest accepted')
    print('SELF_CHECK_OK: nonempty/full/duplicate/AA/seed/balance/deadline/path/CSV/manifest controls', flush=True)


def durable(path):
    return not any(path.is_relative_to(p) for p in (ROOT, Path('/tmp'), Path('/run'), Path('/dev/shm')))


def main():
    global CLUSTER, LOCK
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('command', nargs='?', choices=['prepare', 'run', 'self-check'])
    parser.add_argument('--self-check', action='store_true')
    parser.add_argument('--evidence', type=Path)
    parser.add_argument('--quiet-a', type=Path, default=os.environ.get('RFC263_QUIET_A') or None)
    parser.add_argument('--cluster-file', type=Path, default=os.environ.get('RFC263_CLUSTER_FILE') or None)
    parser.add_argument('--cluster-reference', type=Path, default=os.environ.get('RFC263_CLUSTER_REFERENCE') or None,
                        help='independent native cluster file used by prepare; identities must match frozen SHA')
    parser.add_argument('--lock-file', type=Path, default=os.environ.get('RFC263_FAST_LOCK') or None)
    parser.add_argument('--provenance-ack', type=Path)
    parser.add_argument('--provenance-diff', type=Path)
    parser.add_argument('--epoch', type=int, choices=[1, 2])
    parser.add_argument('--deadline-utc')
    parser.add_argument('--authorization', help='owner approval receipt; must explicitly cover frozen fa/c2 identities')
    parser.add_argument('--worker', action='store_true', help=argparse.SUPPRESS)
    args = parser.parse_args()
    if args.self_check or args.command == 'self-check':
        self_check()
        return
    require(args.command in ('prepare', 'run'), 'prepare or run command required')
    require(args.evidence is not None, '--evidence required')
    require(args.cluster_file is not None, '--cluster-file or RFC263_CLUSTER_FILE required')
    CLUSTER = args.cluster_file.resolve()
    if args.lock_file is not None:
        LOCK = args.lock_file.resolve()
    args.evidence = args.evidence.resolve()
    require(durable(args.evidence), 'evidence must be durable and outside repository')
    if args.command == 'prepare':
        require(args.quiet_a is not None and args.cluster_reference is not None,
                'prepare requires --quiet-a and --cluster-reference (or their RFC263 environment variables)')
        args.quiet_a, args.cluster_reference = args.quiet_a.resolve(), args.cluster_reference.resolve()
        require(args.provenance_ack is not None and args.provenance_diff is not None,
                'prepare requires --provenance-ack and --provenance-diff')
        prepare(args)
        return
    require(LOCK is not None, 'run requires --lock-file or RFC263_FAST_LOCK')
    require(args.epoch and args.deadline_utc and args.authorization, 'run requires epoch/deadline/explicit owner authorization')
    seconds = deadline_seconds(args.deadline_utc)
    if args.worker:
        args.deadline = float(os.environ['RFC263_CONFIRM_DEADLINE'])
        collect(args)
        return
    require(not (args.evidence / f'epoch{args.epoch}').exists(), 'no replacement or resume allowed')
    require(seconds >= FORECAST_SECONDS + 5, 'full frozen epoch forecast does not fit deadline')
    env = dict(os.environ, RFC263_CONFIRM_DEADLINE=str(time.monotonic() + seconds - 5))
    child = subprocess.Popen([sys.executable, str(Path(__file__).resolve()), *sys.argv[1:], '--worker'],
                             env=env, process_group=0)
    def stop_requested(signum, frame):
        raise KeyboardInterrupt('supervisor signal ' + str(signum))
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
        save(args.evidence / f'epoch{args.epoch}-supervisor-stop.json', {'utc': utc(), 'failure': repr(exc)})
        raise SystemExit(124)
    raise SystemExit(code)


if __name__ == '__main__':
    main()
