#!/usr/bin/env python3
"""Offline check/summary; timing requires a separately approved quiet slot."""
import argparse
import copy
import datetime
import fcntl
import hashlib
import json
import math
import os
from pathlib import Path
import re
import signal
import statistics
import subprocess
import tempfile
import time
import zlib

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
    with open(path, 'rb') as f:
        return hashlib.file_digest(f, 'sha256').hexdigest()


def dump(path, obj):
    Path(path).write_text(json.dumps(obj, indent=2) + '\n')


def snapshot():
    return {'utc': datetime.datetime.now(datetime.timezone.utc).isoformat(),
            'loadavg': Path('/proc/loadavg').read_text().split()[:3]}


def rotate(xs, n):
    n %= len(xs)
    return xs[n:] + xs[:n]


def schedule(p):
    out = []
    t = p['timing']
    for b in range(t['blocks']):
        for c, (direction, method) in enumerate(rotate(t['cells'], b)):
            arms = rotate(p['arms'], 2*b + 2*c)
            if (b+c) % 2:
                arms = list(reversed(arms))
            for pos, arm in enumerate(arms):
                binary = Path(arm['binary'])
                if not binary.is_absolute():
                    binary = ROOT / binary
                pattern = f'^BenchmarkPlanSkip$/^cell=rows10$/^dir={direction}$/^method={method}$'
                out.append({'id': f'b{b}/c{c}-{direction}-{method}/p{pos}-{arm["id"]}',
                            'block': b, 'cell_position': c, 'arm_position': pos, 'arm': arm['id'],
                            'source_revision': arm['revision'], 'direction': direction, 'method': method,
                            'argv': ['taskset', '-c', t['client_cpus'], str(binary), '-test.run=^$', '-test.v',
                                     '-test.bench=' + pattern, '-test.count=' + str(t['repeats']),
                                     '-test.benchmem', '-test.benchtime=' + t['benchtime'], '-test.timeout=0']})
    assert len(out) == t['total_processes'] == 144
    for a in p['arms']:
        for direction, method in t['cells']:
            js = [j for j in out if (j['arm'], j['direction'], j['method']) == (a['id'], direction, method)]
            assert sorted(j['block'] for j in js) == list(range(4))
    return out


def parse_output(text, job):
    case = f'BenchmarkPlanSkip/cell=rows10/dir={job["direction"]}/method={job["method"]}'
    result_pattern = re.compile(r'^' + re.escape(case) + r'-8\s+(\d+)\s+([\d.]+) ns/op\s+([\d.]+) prefix-B\s+([\d.]+) rows/op\s+(\d+) B/op\s+(\d+) allocs/op$')
    results, audits = [], []
    for line in text.splitlines():
        if 'FDB_BENCH_AUDIT ' in line:
            audits.append(json.loads(line.split('FDB_BENCH_AUDIT ', 1)[1]))
        if line.startswith('BenchmarkPlanSkip/') and ' ns/op' in line:
            m = result_pattern.fullmatch(line.strip())
            if m is None:
                raise RuntimeError('unexpected benchmark result: ' + line)
            n, ns, prefix, rows, size, allocs = map(float, m.groups())
            assert n > 0 and ns > 0 and prefix == 22 and rows == 10
            results.append({'iterations': int(n), 'ns_op': ns, 'B_op': int(size), 'allocs_op': int(allocs)})
    assert len(results) == 3, f'expected three result lines, got {len(results)}'
    assert text.splitlines()[-1] == 'PASS', 'missing terminal PASS'
    fixtures = [a for a in audits if a['phase'] == 'fixture']
    assert len(fixtures) == 1 and fixtures[0]['rows'] == 1000 and fixtures[0]['prefix_bytes'] == 22
    checks = [a for a in audits if a['phase'] != 'fixture']
    assert len(checks) >= 6 and len(checks) % 2 == 0, 'missing calibration/repeat audit pairs'
    ids = range(103, 113) if job['direction'] == 'forward' else range(109, 99, -1)
    checksum = zlib.crc32(''.join(f'{i}\n' for i in ids).encode())
    for i, a in enumerate(checks):
        assert a['phase'] == ('before' if i % 2 == 0 else 'after')
        assert a['case'] == case and a['method'] == job['method']
        assert a['reverse'] == (job['direction'] == 'reverse')
        assert a['rows'] == 10 and a['skip'] == 3 and a['limit'] == 0 and a['prefix_bytes'] == 22
        assert a['checksum'] == checksum
        if job['method'] == 'USE_REMOTE_FETCH':
            assert (a['remote_openings'], a['remote_entries'], a['index_scans'], a['record_loads']) == (1, 14, 0, 0)
        else:
            assert a['remote_openings'] == a['remote_entries'] == 0
            assert a['index_scans'] >= 1 and a['record_loads'] == 10
    return {'repeats': results, 'median_ns_op': statistics.median(x['ns_op'] for x in results),
            'audit_pairs': len(checks)//2}


def busy_reasons():
    bad = []
    for proc in Path('/proc').iterdir():
        if not proc.name.isdecimal():
            continue
        try:
            argv = (proc / 'cmdline').read_bytes().split(b'\0')
            exe = Path(argv[0].decode(errors='replace')).name
        except (OSError, IndexError):
            continue
        if exe in ('bazel', 'bazelisk', 'linux-sandbox', 'process-wrapper') or exe.startswith('bazelisk-'):
            bad.append('active build: ' + proc.name + ' ' + b' '.join(argv[:4]).decode(errors='replace'))
    r = subprocess.run(['docker', 'ps', '--format', '{{.Image}} {{.Names}} {{.Labels}}'], capture_output=True, text=True, timeout=5)
    if r.returncode:
        bad.append('docker status unavailable: ' + r.stderr.strip())
    elif any('testcontainers' in x.lower() or 'reaper' in x.lower() for x in r.stdout.splitlines()):
        bad.append('testcontainer running: ' + r.stdout.strip())
    return bad


def cli(t, command, deadline):
    remaining = deadline-time.monotonic()
    if remaining <= 0:
        raise RuntimeError('hard slot deadline reached')
    timeout = min(10, remaining)
    r = subprocess.run([t['fdbcli'], '-C', t['cluster_file'], '--timeout', str(max(1, math.floor(timeout))), '--exec', command],
                       capture_output=True, text=True, timeout=timeout)
    if r.returncode:
        raise RuntimeError('fdbcli failed: ' + r.stdout + r.stderr)
    return r.stdout


def binaries(p):
    manifest = json.loads((ROOT / 'build-manifest.json').read_text())
    assert manifest['plan_sha256'] == sha(PLAN), 'plan changed after build'
    records = {r['id']: r for r in manifest['builds']}
    assert len(records) == 7, 'interior builds incomplete'
    out = {}
    for a in p['arms']:
        path = Path(a['binary'])
        if not path.is_absolute():
            path = ROOT / path
        if a.get('build'):
            r = records[a['id']]
            assert r['source_revision'] == a['revision'] and r['binary'] == str(path)
            expected = r['sha256']
        else:
            expected = a['sha256']
        assert sha(path) == expected, 'binary hash mismatch: ' + a['id']
        out[a['id']] = {'path': str(path), 'sha256': expected, 'source_revision': a['revision']}
    return out


def summarize(p, out):
    verdict = json.loads((out / 'verdict.json').read_text())
    assert verdict['valid'], 'incomplete/invalid timing run: comparative estimators prohibited'
    assert sha(out / 'plan.json') == sha(PLAN), 'current plan differs from run plan'
    med = {}
    for j in schedule(p):
        r = json.loads((out / j['id'] / 'done.json').read_text())
        assert r['rc'] == 0
        parsed = parse_output((out / j['id'] / 'stdout.txt').read_text(), j)
        med[j['direction'], j['block'], j['arm'], j['method']] = parsed['median_ns_op']
    def stats(xs):
        assert len(xs) == 4 and all(x > 0 for x in xs)
        return {'geometric_mean': math.exp(statistics.mean(math.log(x) for x in xs)),
                'per_block': xs, 'min': min(xs), 'max': max(xs)}
    result = {'range_note': 'min/max across four blocks, not confidence intervals', 'directions': {}}
    for direction in ('reverse', 'forward'):
        rows = {}
        for ai, a in enumerate(p['arms']):
            arm = a['id']
            get = lambda b, x, m: med[direction, b, x, m]
            remote = 'USE_REMOTE_FETCH'
            ordinary = 'SCAN_AND_FETCH'
            r = {'remote_over_old': stats([get(b, arm, remote)/get(b, 'old', remote) for b in range(4)]),
                 'ordinary_over_old': stats([get(b, arm, ordinary)/get(b, 'old', ordinary) for b in range(4)]),
                 'remote_over_ordinary': stats([get(b, arm, remote)/get(b, arm, ordinary) for b in range(4)]),
                 'difference_in_differences': stats([(get(b, arm, remote)/get(b, 'old', remote))/(get(b, arm, ordinary)/get(b, 'old', ordinary)) for b in range(4)])}
            if ai:
                previous = p['arms'][ai-1]['id']
                r['over_previous'] = {m: stats([get(b, arm, m)/get(b, previous, m) for b in range(4)]) for m in (remote, ordinary)}
            rows[arm] = r
        result['directions'][direction] = rows
    dump(out / 'summary.json', result)
    print(json.dumps(result, indent=2))


def execute(p, out):
    if out.exists():
        raise RuntimeError('fresh output directory required; no resuming')
    out.mkdir(parents=True)
    # Copy exact plan bytes: their hash is the pre-timing estimator seal.
    (out / 'plan.json').write_bytes(PLAN.read_bytes())
    t = p['timing']
    with open(t['lock'], 'a+') as lock:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        started = time.monotonic()
        deadline = started + t['hard_limit_seconds']
        completed, owned, error = 0, False, None
        receipts = binaries(p)
        input_files = {'plan.json': PLAN, 'build.py': CODE_ROOT / 'build.py',
                       'timing.py': CODE_ROOT / 'timing.py', 'build-manifest.json': ROOT / 'build-manifest.json'}
        input_hashes = {f: sha(path) for f, path in input_files.items()}
        dump(out / 'input-seal.json', {'files_sha256': input_hashes, 'binaries': receipts})
        jobs = schedule(p)
        dump(out / 'jobs.json', jobs)
        try:
            gate = {'before': snapshot(), 'busy_before': busy_reasons()}
            dump(out / 'gate.json', gate)
            assert not gate['busy_before'], gate['busy_before']
            time.sleep(t['settle_seconds'])
            gate.update({'after_settle': snapshot(), 'busy_after': busy_reasons()})
            dump(out / 'gate.json', gate)
            assert not gate['busy_after'], gate['busy_after']
            assert float(gate['after_settle']['loadavg'][0]) <= t['load1_gate'], 'load gate failed'
            status = cli(t, 'status json', deadline)
            (out / 'status-before.json').write_text(status)
            processes = json.loads(status)['cluster']['processes'].values()
            assert sum(r['role'] == 'storage' for proc in processes for r in proc['roles']) == 1, 'expected one storage server'
            a, b = t['owned_range']
            text = cli(t, f'getrange {a} {b} 1', deadline)
            (out / 'range-before.txt').write_text(text)
            assert 'Range limited to 1 keys' in text, 'missing getrange response'
            assert ' is `' not in text, 'owned benchmark range is not empty'
            owned = True
            env = dict(t['environment'], FDB_BENCH_CLUSTER_FILE=t['cluster_file'], FDB_CLUSTER_FILE=t['cluster_file'])
            for j in jobs:
                assert sha(PLAN) == input_hashes['plan.json'], 'plan changed during timing'
                assert time.monotonic() < deadline-t['cleanup_reserve_seconds'], 'deadline reserve reached'
                assert not busy_reasons(), 'new build/testcontainer activity'
                assert sha(receipts[j['arm']]['path']) == receipts[j['arm']]['sha256'], 'binary changed'
                d = out / j['id']
                d.mkdir(parents=True)
                meta = {'job': j, 'before': snapshot(), 'env': env}
                job_start = time.monotonic()
                proc = None
                with open(d / 'stdout.txt', 'w') as so, open(d / 'stderr.txt', 'w') as se:
                    try:
                        proc = subprocess.Popen(j['argv'], env=env, cwd=d, stdout=so, stderr=se)
                        while proc.poll() is None:
                            assert time.monotonic() < deadline-t['cleanup_reserve_seconds'], 'slot deadline reserve reached'
                            assert time.monotonic()-job_start < 90, 'process exceeded fixed 90-second budget'
                            assert not busy_reasons(), 'new build/testcontainer activity'
                            try:
                                proc.wait(timeout=1)
                            except subprocess.TimeoutExpired:
                                pass
                    except BaseException as exc:
                        meta['error'] = f'{type(exc).__name__}: {exc}'
                        raise
                    finally:
                        if proc is not None and proc.poll() is None:
                            proc.terminate()
                            try:
                                proc.wait(timeout=1)
                            except subprocess.TimeoutExpired:
                                proc.kill()
                                proc.wait()
                        meta.update({'rc': proc.returncode if proc is not None else None, 'after': snapshot(),
                                     'wall_seconds': time.monotonic()-job_start})
                        dump(d / 'done.json', meta)
                assert proc.returncode == 0, f'process failed: {j["id"]}, rc={proc.returncode}'
                meta['parsed'] = parse_output((d / 'stdout.txt').read_text(), j)
                dump(d / 'done.json', meta)
                completed += 1
                print(completed, '/', len(jobs), j['id'], meta['parsed']['median_ns_op'], flush=True)
            assert not busy_reasons(), 'activity at end of timing'
            assert binaries(p) == receipts, 'binary provenance changed'
            assert all(sha(input_files[f]) == h for f, h in input_hashes.items()), 'inputs changed during timing'
        except BaseException as exc:
            error = f'{type(exc).__name__}: {exc}'
        finally:
            if owned:
                try:
                    a, b = t['owned_range']
                    text = cli(t, f'writemode on; clearrange {a} {b}; getrange {a} {b} 1', deadline)
                    (out / 'range-after.txt').write_text(text)
                    assert 'Range limited to 1 keys' in text, 'missing cleanup getrange response'
                    assert ' is `' not in text, 'owned range not empty after cleanup'
                except BaseException as exc:
                    error = (error or '') + '; cleanup: ' + str(exc)
            elapsed = time.monotonic()-started
            if elapsed >= t['hard_limit_seconds']:
                error = (error or '') + '; hard slot limit exceeded'
            verdict = {'valid': error is None and completed == 144, 'completed_processes': completed,
                       'expected_processes': 144, 'elapsed_seconds': elapsed, 'error': error}
            dump(out / 'verdict.json', verdict)
            print(json.dumps(verdict), flush=True)
        if not verdict['valid']:
            raise RuntimeError('timing invalid; preserve raw data, no comparative summary')
        summarize(p, out)


def terminated(signum, frame):
    raise SystemExit(f'received signal {signum}')


def check_abort(plan):
    # Fake child and cluster; private lock. No executable or network access.
    saved = {name: globals()[name] for name in ('busy_reasons', 'cli', 'binaries', 'sha', 'snapshot')}
    old_popen = subprocess.Popen
    children, calls = [], {'busy': 0, 'cleanup': 0}
    with tempfile.TemporaryDirectory(prefix='mapped-bisect-offline-') as temp:
        p = copy.deepcopy(plan)
        p['timing']['lock'] = str(Path(temp) / 'private.lock')
        p['timing']['settle_seconds'] = 0
        out = Path(temp) / 'output'

        def busy():
            calls['busy'] += 1
            if calls['busy'] == 4:
                terminated(signal.SIGTERM, None)
            return []

        def fake_cli(t, command, deadline):
            if command == 'status json':
                return json.dumps({'cluster': {'processes': {'p': {'roles': [{'role': 'storage'}]}}}})
            if command.startswith('writemode on;'):
                calls['cleanup'] += 1
            return '\nRange limited to 1 keys\n\n'

        class Child:
            def __init__(self, *args, **kwargs):
                self.returncode = None
                children.append(self)
            def poll(self):
                return self.returncode
            def terminate(self):
                self.returncode = -15
            def kill(self):
                self.returncode = -9
            def wait(self, timeout=None):
                return self.returncode

        try:
            globals().update(busy_reasons=busy, cli=fake_cli,
                             binaries=lambda p: {'old': {'path': '/fake/binary', 'sha256': 'offline'}},
                             sha=lambda path: 'offline', snapshot=lambda: {'utc': 'offline', 'loadavg': ['0', '0', '0']})
            subprocess.Popen = Child
            try:
                execute(p, out)
            except RuntimeError as exc:
                assert 'timing invalid' in str(exc)
            else:
                raise AssertionError('interrupted run was accepted')
            receipt = json.loads((out / schedule(p)[0]['id'] / 'done.json').read_text())
            verdict = json.loads((out / 'verdict.json').read_text())
            assert len(children) == 1 and children[0].returncode == -15
            assert receipt['rc'] == -15 and 'SystemExit' in receipt['error']
            assert 'after' in receipt and receipt['wall_seconds'] >= 0
            assert calls['cleanup'] == 1 and verdict['completed_processes'] == 0 and not verdict['valid']
            assert not (out / 'summary.json').exists()
        finally:
            globals().update(saved)
            subprocess.Popen = old_popen
    print('Offline SIGTERM check passed: child reaped, failed receipt saved, cleanup called, summary refused.')


def main():
    global ROOT
    signal.signal(signal.SIGTERM, terminated)
    if not __debug__:
        raise RuntimeError('Python optimization disables safety checks; run without -O/PYTHONOPTIMIZE')
    ap = argparse.ArgumentParser()
    ap.add_argument('command', choices=['check', 'run', 'summary'])
    ap.add_argument('--approved-quiet-slot', action='store_true')
    ap.add_argument('--out', type=Path)
    args = ap.parse_args()
    p = load_plan()
    ROOT = Path(p['root'])
    if not ROOT.is_absolute():
        raise RuntimeError('artifact root must be absolute')
    if args.command == 'check':
        jobs = schedule(p)
        # Offline parser checks against already-recorded A2 outputs; no executable or cluster access.
        source = ROOT.parent / 'profiles/reference-client/reference-output'
        for direction in ('forward', 'reverse'):
            for method, suffix in [('SCAN_AND_FETCH', 'ordinary'), ('USE_REMOTE_FETCH', 'remote')]:
                matches = list((source / direction / 'b0').glob('p*-old-' + suffix + '/stdout.txt'))
                assert len(matches) == 1
                text = matches[0].read_text()
                job = {'direction': direction, 'method': method}
                parse_output(text, job)
                negatives = [text.replace(' ns/op', ' wrong-unit'),
                             text.replace('"phase":"fixture"', '"phase":"unknown"'),
                             text.replace('"rows":10,', '"rows":9,')]
                for bad in negatives:
                    try:
                        parse_output(bad, job)
                    except (AssertionError, RuntimeError):
                        continue
                    raise AssertionError('invalid parser fixture accepted')
        assert len({j['id'] for j in jobs}) == 144
        check_abort(p)
        print('Offline check passed: 144 unique jobs; four blocks per arm/cell; four recorded parser fixtures and 12 negative controls passed.')
    elif args.command == 'run':
        if not args.approved_quiet_slot or args.out is None:
            ap.error('run requires --approved-quiet-slot and a fresh --out directory')
        execute(p, args.out.resolve())
    else:
        if args.out is None:
            ap.error('summary requires --out')
        summarize(p, args.out.resolve())


if __name__ == '__main__':
    main()
