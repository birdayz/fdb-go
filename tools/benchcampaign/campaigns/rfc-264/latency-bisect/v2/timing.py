#!/usr/bin/env python3
"""Offline schedules/summaries; running requires an explicitly approved quiet slot."""
import argparse
import csv
import fcntl
import importlib.util
import json
import math
import os
from pathlib import Path
import signal
import shutil
import statistics
import subprocess
import time

from common import ROOT, PLAN, dump, load, path, rotate, schedule, sha, utc

BASE_PATH = '/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin'


def resolve_executable(name):
    resolved = shutil.which(name, path=BASE_PATH)
    if resolved is None:
        raise RuntimeError('required executable unavailable: ' + name)
    return resolved


def executable_preflight(p, identities):
    required = {'docker': resolve_executable('docker'), 'taskset': resolve_executable('taskset'),
                'fdbcli': str(path(p['fdbcli']))}
    required.update({'client/' + arm: record['binary'] for arm, record in identities.items()})
    for name, filename in required.items():
        if not Path(filename).is_file() or not os.access(filename, os.X_OK):
            raise RuntimeError('required executable not executable: ' + name + ' ' + filename)
    return required


def busy_reasons():
    bad = []
    forbidden = {'bazel', 'bazelisk', 'linux-sandbox', 'process-wrapper', 'compile', 'asm', 'link',
                 'gcc', 'g++', 'cc1', 'cc1plus', 'clang', 'clang++', 'ninja', 'make'}
    for proc in Path('/proc').iterdir():
        if not proc.name.isdecimal():
            continue
        try:
            argv = (proc / 'cmdline').read_bytes().split(b'\0')
            exe = Path(argv[0].decode(errors='replace')).name
        except (OSError, IndexError):
            continue
        if exe in forbidden or exe.startswith('bazelisk-'):
            bad.append('build/action: ' + proc.name + ' ' + b' '.join(argv[:4]).decode(errors='replace'))
    result = subprocess.run([resolve_executable('docker'), 'ps', '--format', '{{.Image}} {{.Names}} {{.Labels}}'],
                            capture_output=True, text=True, timeout=5)
    if result.returncode:
        bad.append('docker status unavailable: ' + result.stderr.strip())
    elif any('testcontainers' in line.lower() or 'reaper' in line.lower() for line in result.stdout.splitlines()):
        bad.append('testcontainers: ' + result.stdout.strip())
    return bad


def snapshot():
    return {'utc': utc(), 'loadavg': Path('/proc/loadavg').read_text().split()[:3]}


def server_affinities(status):
    expected = {'127.0.0.1:4500': {8, 20}, '127.0.0.1:4501': {9, 21}, '127.0.0.1:4502': {10, 22}}
    addresses = {server['address'] for server in status['cluster']['processes'].values()}
    if addresses != set(expected):
        raise RuntimeError('unexpected existing-cluster server addresses')
    found = {}
    for proc in Path('/proc').iterdir():
        if not proc.name.isdecimal():
            continue
        try:
            argv = (proc / 'cmdline').read_bytes().decode().strip('\0').split('\0')
            if Path(argv[0]).name != 'fdbserver' or '-p' not in argv:
                continue
            address = argv[argv.index('-p') + 1]
            if address not in addresses:
                continue
            affinity = os.sched_getaffinity(int(proc.name))
        except (OSError, UnicodeError, IndexError):
            continue
        if address in found or affinity != expected[address]:
            raise RuntimeError('unexpected server population/affinity at ' + address)
        found[address] = {'pid': int(proc.name), 'cpus': sorted(affinity), 'argv': argv}
    if set(found) != addresses:
        raise RuntimeError('could not inspect every existing-cluster server')
    return found


def binaries(p, slot):
    for pin in p['provenance']['pins']:
        if sha(path(pin['path'])) != pin['sha256']:
            raise RuntimeError('reused provenance changed: ' + pin['path'])
    manifest = json.loads(path(p['provenance']['build_manifest']).read_text())
    if (manifest['plan_sha256'] != p['provenance']['source_plan_sha256'] or
            not manifest['complete'] or len(manifest['builds']) != 10):
        raise RuntimeError('complete source build manifest required')
    records = {r['id']: r for r in manifest['builds']}
    out = {}
    for arm in p['arms']:
        if arm['id'] not in p['slots'][slot]['arms']:
            continue
        expected = arm['sha256']
        if arm.get('build'):
            record = records[arm['id']]
            if (record['source_revision'] != arm['revision'] or record['binary'] != str(path(arm['binary'])) or
                    record['sha256'] != expected):
                raise RuntimeError('build provenance mismatch')
            expected = record['sha256']
        binary = path(arm['binary'])
        if sha(binary) != expected:
            raise RuntimeError('binary changed: ' + arm['id'])
        out[arm['id']] = {'binary': str(binary), 'sha256': expected, 'revision': arm['revision']}
    return out


def validator(p):
    filename = Path(__file__).resolve().parents[1] / 'frozen_run.py'
    if sha(filename) != p['validator_sha256']:
        raise RuntimeError('frozen campaign validator changed')
    spec = importlib.util.spec_from_file_location('frozen_run', filename)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def command(p, job, identities, folder):
    w = job['workload']
    return [resolve_executable('taskset'), '-c', p['client_cpus'], identities[job['arm']]['binary'],
            '--cluster-file', str(path(p['cluster_file'])), '--workload', w['name'],
            '--iterations', str(w['iterations']), '--warmup', str(w['warmup']), '--workers', str(w['workers']),
            '--output', str(folder), '--run-id', job['run_id'], '--block', str(job['block']),
            '--source-revision', job['source_revision'], '--harness-id', p['harness_id'], '--allow-writes',
            '--transaction-timeout-ms', '30000', '--retry-limit', '10', '--prefix', job['prefix'], '--profile', 'none']


def validate(p, job, identities, folder, validate_module):
    validate_module.validate_raw_output(folder, job['workload'])
    frozen_plan = {'arms': [{'name': a, 'binary': r['binary'], 'source_revision': r['revision']} for a, r in identities.items()],
                   'source_commit': p['arms'][-1]['revision'], 'harness_id': p['harness_id'],
                   'go_version': '1.26.9', 'environment': p['environment']}
    entry = dict(job, command=command(p, job, identities, folder))
    validate_module.validate_identity(folder, entry, frozen_plan, {r['binary']: r['sha256'] for r in identities.values()})
    with open(folder / 'samples.csv', newline='') as stream:
        rows = list(csv.DictReader(stream))
    values = sorted(int(row['latency_ns']) for row in rows)
    # Empirical inverse CDF, with no interpolation and no cross-process pooling.
    n = len(values)
    return {'p50_ns': values[math.ceil(0.50 * n) - 1], 'p99_ns': values[math.ceil(0.99 * n) - 1],
            'operations': n, 'retries': sum(int(r['attempts']) - 1 for r in rows),
            'raw_sha256': {name: sha(folder / name) for name in ('samples.csv', 'warmup.csv', 'summary.json', 'manifest.json')}}


def stats(xs, blocks):
    if blocks < 1 or len(xs) != blocks or any(not math.isfinite(x) or x <= 0 for x in xs):
        raise RuntimeError('invalid/nonempty block population')
    return {'geometric_mean': math.exp(statistics.mean(math.log(x) for x in xs)), 'per_block': xs,
            'block_min': min(xs), 'block_max': max(xs), 'range_label': 'block min/max, not CI'}


def summarize(p, slot, out):
    verdict = json.loads((out / 'verdict.json').read_text())
    if not verdict['valid'] or sha(out / 'plan.json') != sha(PLAN):
        raise RuntimeError('invalid/incomplete run or changed plan; comparative summary prohibited')
    s = p['slots'][slot]
    results = {}
    for job in schedule(p, slot):
        record = json.loads((out / job['id'] / 'done.json').read_text())
        if record['returncode'] != 0:
            raise RuntimeError('nonzero process result')
        results[job['workload']['name'], job['workload']['workers'], job['block'], job['arm']] = record['statistics']
    arm_ids = s['arms']
    summary = {'slot': slot, 'blocks': s['blocks'], 'processes': len(results),
               'estimators': p['estimators'], 'cells': {}}
    for workload, workers in s['cells']:
        rows = {}
        for ai, arm in enumerate(arm_ids):
            row = {}
            for metric in ('p50_ns', 'p99_ns'):
                get = lambda block, a: results[workload, workers, block, a][metric]
                row[metric] = {'absolute_process_quantiles_ns': stats([get(b, arm) for b in range(s['blocks'])], s['blocks']),
                               'over_old': stats([get(b, arm) / get(b, 'old') for b in range(s['blocks'])], s['blocks'])}
                if ai:
                    previous = arm_ids[ai - 1]
                    row[metric]['over_previous'] = stats([get(b, arm) / get(b, previous) for b in range(s['blocks'])], s['blocks'])
            rows[arm] = row
        summary['cells'][f'{workload}/w{workers}'] = rows
    summary['cell_roles'] = p['cell_roles']
    primary = summary['cells']['get100/w8']
    summary['primary_endpoint'] = primary['master']['p50_ns']['over_old']
    summary['primary_adjacent_intervals'] = [
        {'earlier': earlier, 'later': later, 'ratios': primary[later]['p50_ns']['over_previous']}
        for earlier, later in zip(arm_ids, arm_ids[1:])]
    summary['primary_upward_in_all_four_blocks'] = [
        row for row in summary['primary_adjacent_intervals'] if row['ratios']['block_min'] > 1]
    summary['primary_largest_upward_point_estimate'] = max(
        (row for row in summary['primary_adjacent_intervals'] if row['ratios']['geometric_mean'] > 1),
        key=lambda row: row['ratios']['geometric_mean'], default=None)
    summary['interpretation'] = p['interpretation']
    dump(out / 'summary.json', summary)
    return summary


def cli(p, text, deadline):
    remaining = min(10, deadline - time.monotonic())
    if remaining < 1:
        raise TimeoutError('slot/cleanup deadline reached')
    result = subprocess.run([str(path(p['fdbcli'])), '-C', str(path(p['cluster_file'])), '--timeout',
                             str(math.floor(remaining)), '--exec', text], capture_output=True, text=True, timeout=remaining)
    if result.returncode:
        raise RuntimeError('fdbcli failed: ' + result.stdout + result.stderr)
    return result.stdout


def empty(p, deadline):
    text = cli(p, 'getrange ' + p['prefix_root'] + ' ' + p['prefix_end'] + ' 1', deadline)
    if ' is `' in text or 'Range limited to 1 keys' not in text:
        raise RuntimeError('owned range not verified empty: ' + text)
    return text


def stop(proc):
    if proc is None:
        return
    try:
        os.killpg(proc.pid, signal.SIGTERM)
        proc.wait(timeout=2)
    except ProcessLookupError:
        pass
    except subprocess.TimeoutExpired:
        os.killpg(proc.pid, signal.SIGKILL)
        proc.wait(timeout=2)
    try:
        os.killpg(proc.pid, signal.SIGKILL)
    except ProcessLookupError:
        pass


def execute(p, slot, out, approval):
    if not __debug__:
        raise RuntimeError('Run without -O/PYTHONOPTIMIZE')
    if out.exists():
        raise RuntimeError('fresh output directory required; no resume/retry')
    t0 = time.monotonic()
    deadline = t0 + p['slots'][slot]['wall_cap_seconds']
    measure_deadline = deadline - p['cleanup_reserve_seconds']
    out.mkdir(parents=True)
    (out / 'plan.json').write_bytes(PLAN.read_bytes())
    jobs = schedule(p, slot)
    dump(out / 'schedule.json', jobs)
    proc, touched, completed = None, False, 0
    verdict = {'valid': False, 'slot': slot, 'approval': approval, 'scheduled': len(jobs), 'completed': 0, 'started_utc': utc()}
    handlers = {sig: signal.signal(sig, lambda signum, _: (_ for _ in ()).throw(KeyboardInterrupt(str(signum))))
                for sig in (signal.SIGTERM, signal.SIGINT)}
    with open(path(p['lock']), 'a+') as lock:
        try:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
            identities = binaries(p, slot)
            dump(out / 'executables.json', executable_preflight(p, identities))
            if (sha(path(p['cluster_file'])) != p['cluster_file_sha256'] or
                    sha(path(p['fdbcli'])) != p['fdbcli_sha256'] or sha(path(p['harness_archive'])) != p['harness_id']):
                raise RuntimeError('cluster/harness input changed')
            v = validator(p)
            seal = {str(f): sha(f) for f in Path(__file__).resolve().parent.iterdir()
                    if f.is_file() and f.suffix in ('.py', '.json', '.md', '.tsv')}
            seal[str(Path(v.__file__))] = sha(v.__file__)
            dump(out / 'input-seal.json', {'files': seal, 'binaries': identities})
            gate = {'before': snapshot(), 'busy_before': busy_reasons(), 'rule': p['gate']}
            dump(out / 'gate.json', gate)
            if gate['busy_before']:
                raise RuntimeError('busy at initial gate: ' + repr(gate['busy_before']))
            settle_end = time.monotonic() + p['settle_seconds']
            while time.monotonic() < settle_end:
                time.sleep(min(1, settle_end - time.monotonic()))
                bad = busy_reasons()
                if bad:
                    gate['settle_busy'] = bad
                    dump(out / 'gate.json', gate)
                    raise RuntimeError('activity during settle')
            gate['after_settle'] = snapshot()
            gate['busy_after_settle'] = busy_reasons()
            gate['passed'] = not gate['busy_after_settle'] and float(gate['after_settle']['loadavg'][0]) <= p['load_gate']
            dump(out / 'gate.json', gate)
            if not gate['passed']:
                raise RuntimeError('quiet gate failed; no timing processes launched')
            status_text = cli(p, 'status json', measure_deadline)
            (out / 'status-before.json').write_text(status_text)
            status = json.loads(status_text)
            storage = [role for server in status['cluster']['processes'].values() for role in server['roles'] if role['role'] == 'storage']
            if not status['client']['database_status']['available'] or len(storage) != 1:
                raise RuntimeError('expected available single-storage-server local cluster')
            dump(out / 'server-affinities.json', server_affinities(status))
            (out / 'range-before.txt').write_text(empty(p, measure_deadline))
            for job in jobs:
                if time.monotonic() >= measure_deadline:
                    raise TimeoutError('hard cap reached before next process')
                bad = busy_reasons()
                if bad:
                    raise RuntimeError('quiet violation: ' + repr(bad))
                folder = out / job['id']
                folder.mkdir()
                raw = folder / 'run'
                argv = command(p, job, identities, raw)
                before, start = snapshot(), time.monotonic()
                dump(folder / 'invocation.json', dict(job, argv=argv, environment=p['environment'], before=before))
                touched = True
                with open(folder / 'stdout.txt', 'w') as stdout, open(folder / 'stderr.txt', 'w') as stderr, open(folder / 'telemetry.jsonl', 'w') as telemetry:
                    env = {'PATH': BASE_PATH,
                           'HOME': str(Path.home()), **p['environment']}
                    proc = subprocess.Popen(argv,
                                            env=env, stdout=stdout, stderr=stderr, start_new_session=True)
                    process_deadline = min(measure_deadline, start + p['process_timeout_seconds'])
                    while True:
                        telemetry.write(json.dumps(snapshot()) + '\n')
                        telemetry.flush()
                        bad = busy_reasons()
                        if bad:
                            raise RuntimeError('quiet violation during process: ' + repr(bad))
                        remaining = process_deadline - time.monotonic()
                        if remaining <= 0:
                            raise TimeoutError('process/slot deadline: ' + job['id'])
                        try:
                            proc.wait(timeout=min(1, remaining))
                            break
                        except subprocess.TimeoutExpired:
                            pass
                returncode = proc.returncode
                stop(proc)
                proc = None
                record = {'returncode': returncode, 'wall_seconds': time.monotonic() - start, 'before': before, 'after': snapshot()}
                dump(folder / 'done.json', record)
                if returncode:
                    raise RuntimeError('benchmark failed: ' + job['id'])
                record['statistics'] = validate(p, job, identities, raw, v)
                dump(folder / 'done.json', record)
                completed += 1
                print(f'{completed}/{len(jobs)} {job["id"]}', flush=True)
            if binaries(p, slot) != identities or any(sha(f) != digest for f, digest in seal.items()):
                raise RuntimeError('inputs changed during slot')
            if time.monotonic() >= measure_deadline:
                raise TimeoutError('hard cap reached after timing')
            verdict['timings_complete'] = True
        except BaseException as exc:
            verdict['error'] = repr(exc)
        finally:
            stop(proc)
            try:
                if touched:
                    (out / 'cleanup.txt').write_text(cli(p, 'writemode on; clearrange ' + p['prefix_root'] + ' ' + p['prefix_end'], deadline))
                    (out / 'range-after.txt').write_text(empty(p, deadline))
                    verdict['cleanup_verified'] = True
                else:
                    verdict['cleanup_verified'] = 'not needed; zero timing processes'
            except BaseException as exc:
                verdict['cleanup_error'] = repr(exc)
            verdict.update(completed=completed, finished_utc=utc(), wall_seconds=time.monotonic() - t0)
            verdict['valid'] = (verdict.get('timings_complete') is True and verdict.get('cleanup_verified') is True and
                                completed == len(jobs) and 'error' not in verdict and 'cleanup_error' not in verdict and
                                time.monotonic() < deadline)
            dump(out / 'verdict.json', verdict)
            for sig, handler in handlers.items():
                signal.signal(sig, handler)
    if not verdict['valid']:
        raise RuntimeError('slot invalid: ' + repr(verdict))
    return summarize(p, slot, out)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('action', choices=['schedule', 'run', 'summary'])
    parser.add_argument('--slot', choices=['narrow'], required=True)
    parser.add_argument('--output', type=Path)
    parser.add_argument('--approved-quiet-slot')
    args = parser.parse_args()
    p = load()
    if args.action == 'schedule':
        print(json.dumps(schedule(p, args.slot), indent=2))
        return
    if args.output is None:
        parser.error('--output is required')
    out = args.output.resolve()
    if out.parent != ROOT:
        parser.error('output must be a fresh direct child of the campaign root')
    if args.action == 'summary':
        print(json.dumps(summarize(p, args.slot, out), indent=2))
    else:
        if not args.approved_quiet_slot or not args.approved_quiet_slot.strip():
            parser.error('explicit --approved-quiet-slot reference required')
        result = execute(p, args.slot, out, args.approved_quiet_slot)
        print(json.dumps(result.get('reproduction', {'slot': args.slot, 'complete': True}), indent=2))


if __name__ == '__main__':
    main()
