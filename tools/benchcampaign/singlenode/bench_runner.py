#!/usr/bin/env python3
"""Frozen testing.B campaign runner. Stdlib only; never builds or retries.

plan: --protocol FILE --orders FILE --phase B|C1|D1|C2|D2|C3
      --pins JSON --source-receipt FILE --output FILE [--network-receipt FILE]
      [--harness-commit SHA, required when protocol.json has no harness_revision]
Pins are {absolute_runtime_path: sha256}; include all ten binaries, this runner,
bench_analyze.py, protocol/orders, cluster, engine.txt, run.py and source receipt.
Source receipt JSON: harness_commit, production_revisions (protocol's map), and
nonempty source_artifacts_sha256 {archive_path: SHA256}; also pin those archives.
run: --plan FILE --output NEW_DIRECTORY --lifetime-deadline ISO8601_UTC
Runner must already be in the $FDB_BENCH_NETNS netns, pinned to the protocol's
housekeeping CPUs. Results include lifecycle CPU/wall, not measured-phase
CPU/op or p99. No auto-retries.
Environment: FDB_BENCH_ROOT (default /srv/fdb-bench), FDB_BENCH_NETNS (default
fdb-bench-client), FDB_BENCH_SERVER_ADDR (required for run: owned server host:port).
"""
import argparse
import csv
import datetime as dt
import hashlib
import importlib.util
import itertools
import json
import math
import os
from pathlib import Path
import re
import signal
import subprocess
import sys
import time

ROOT = Path(os.environ.get('FDB_BENCH_ROOT', '/srv/fdb-bench'))
NETNS = os.environ.get('FDB_BENCH_NETNS', 'fdb-bench-client')
CLUSTER = str(ROOT / 'cluster/fdb.cluster')
ENGINE = str(ROOT / 'cluster/engine.txt')
HELPER = str(ROOT / 'tools/clientbench/scripts/run.py')
BINARIES = ['rl-legacy', 'rl-before', 'rl-final', 'fetch-pr2', 'fetch-final',
            'sql-legacy', 'sql-before', 'sql-final', 'planskip-final', 'sqlfetch-final']
PHASES = ('B', 'C1', 'D1', 'C2', 'D2', 'C3')
METRICS = ('ns/op', 'B/op', 'allocs/op', 'rows/op', 'prefix-B')


class EnvironmentFailure(RuntimeError):
    pass


class CorrectnessFailure(ValueError):
    pass


def parse_cpus(text):
    """Parse a taskset CPU list such as '8-15' or '0,2-3' into sorted CPU numbers."""
    cpus = []
    for part in str(text).split(','):
        lo, dash, hi = part.partition('-')
        if not lo.isdecimal() or (dash and not hi.isdecimal()) or int(hi or lo) < int(lo):
            raise ValueError('malformed CPU list: '+str(text))
        cpus.extend(range(int(lo), int(hi or lo)+1))
    if len(set(cpus)) != len(cpus):
        raise ValueError('duplicate CPU in list: '+str(text))
    return sorted(cpus)


def cpu_set(plan, name):
    return next(s['cpus'] for s in plan['cpu_sets'] if s['name'] == name)


def server_addr():
    value = os.environ.get('FDB_BENCH_SERVER_ADDR', '')
    if not re.fullmatch(r'[^\s@:]+:[0-9]+', value):
        raise EnvironmentFailure('FDB_BENCH_SERVER_ADDR must name the owned server as host:port')
    return value


def sha(path):
    h = hashlib.sha256()
    with open(path, 'rb') as stream:
        for part in iter(lambda: stream.read(1048576), b''):
            h.update(part)
    return h.hexdigest()


def dump(path, value, exclusive=False):
    with open(path, 'x' if exclusive else 'w') as stream:
        json.dump(value, stream, indent=2, sort_keys=True, allow_nan=False)
        stream.write('\n')


def number(value, positive=False):
    if isinstance(value, bool) or not isinstance(value, (int, float)) or not math.isfinite(value):
        raise CorrectnessFailure('nonfinite/nonnumeric metric')
    if value < 0 or (positive and value <= 0):
        raise CorrectnessFailure('negative or nonpositive metric')
    return value


def cases_for(phase):
    def case(key, stem, binary_kind, audit, rows=None, write=False, **fields):
        return dict(key=key, stem=stem, binary_kind=binary_kind, audit=audit,
                    rows=rows, write=write, fields=fields)
    if phase == 'B':
        return [case(n, 'Benchmark'+n, 'rl', 'classic-write' if n.startswith('Save') else 'none',
                     write=n.startswith('Save')) for n in
                ['SaveRecord', 'LoadRecord', 'SaveRecordWithIndex', 'ScanIndexRecords/serial',
                 'ScanIndexRecords/pipelined', 'ScanRecords', 'ScanRecordsSplitVersioned', 'SaveSplitRecord']]
    if phase == 'D1':
        return [case(n, 'Benchmark'+n, 'sql', 'sql-write' if n.endswith('Insert') else 'sql-read',
                     rows=1000 if n.endswith('Insert') else (1 if n.endswith('Hit') else 50),
                     write=n.endswith('Insert')) for n in
                ['FDB_PlanCacheHit', 'FDB_IndexScanRange', 'FDB_TimestampInsert']]
    if phase == 'D2':
        return [case('rows'+str(n), 'BenchmarkSQLIndexFetch/query=rows'+str(n),
                     'sqlfetch', 'sql-fetch', rows=n) for n in (10, 1000)]
    if phase == 'C2':
        return [case('reverse-'+p+'-1000', 'BenchmarkIndexFetch/payload='+p+'/cell=rows1000/dir=reverse',
                     'fetch', 'fetch', rows=1000, payload=p, chunks=1 if p == 'small' else 2,
                     payload_bytes=0 if p == 'small' else 100000, reverse=True, skip=0, limit=0)
                for p in ('small', 'split')] + [
                case('executor-skip3-small-'+str(n), 'BenchmarkPlanSkip/cell=rows'+str(n)+'/dir=forward',
                     'planskip', 'planskip', rows=10 if n == 10 else 997,
                     payload='small', reverse=False, skip=3, limit=0) for n in (10, 1000)]
    if phase in ('C1', 'C3'):
        shapes = [('limit-1-broad', 'limit1', 1, 1), ('exact-10', 'rows10', 10, 0), ('all-1000', 'rows1000', 1000, 0)]
        if phase == 'C3':
            shapes = shapes[-1:]
        return [case(p+'-'+key, 'BenchmarkIndexFetch/payload='+p+'/cell='+cell+'/dir=forward',
                     'fetch', 'fetch', rows=rows, payload=p, chunks=1 if p == 'small' else 2,
                     payload_bytes=0 if p == 'small' else 100000, reverse=False, skip=0, limit=limit)
                for key, cell, rows, limit in shapes for p in ('small', 'split')]
    raise ValueError('unsupported phase')


def validate_orders(phase, orders):
    arms, blocks = phase['arms'], phase['blocks']
    if not arms or len(set(arms)) != len(arms) or len(orders) != blocks:
        raise ValueError('empty/duplicate arms or wrong block count')
    if any(not isinstance(o, list) or len(o) != len(arms) or set(o) != set(arms) for o in orders):
        raise ValueError('each nonempty block must contain every arm once')
    positions = {(a, p): sum(o[p] == a for o in orders) for a in arms for p in range(len(arms))}
    if len(set(positions.values())) != 1:
        raise ValueError('orders not position balanced')
    if phase['id'] == 'C1':
        pairs = {(a, b): sum(sum(x == a and y == b for x, y in zip(o, o[1:])) for o in orders)
                 for a in arms for b in arms if a != b}
        if blocks != 8 or len(arms) != 4 or set(pairs.values()) != {2}:
            raise ValueError('C1 requires eight complete Williams orders, each directed adjacency twice')
    elif len(arms) == 3 and set(map(tuple, orders)) != set(itertools.permutations(arms)):
        raise ValueError('three-arm orders must contain all six permutations')


def benchmark_name(case, arm):
    if case['audit'] in ('fetch', 'planskip', 'sql-fetch'):
        method = 'USE_REMOTE_FETCH' if arm.endswith('-remote') else 'SCAN_AND_FETCH'
        return case['stem']+'/method='+method
    return case['stem']


def binary_for(case, arm):
    kind = case['binary_kind']
    revision = 'pr2' if arm.startswith('pr2-') else ('final' if arm.startswith('final-') else arm)
    return str(ROOT / 'bin' / (kind+'-'+revision))


def make_plan(protocol, orders, phase_id, pins, paths, network_receipt=None, harness_commit=None):
    if protocol.get('version') not in (2, 3, 4) or phase_id not in PHASES:
        raise ValueError('sealed protocol v2/v3/v4 and supported phase required')
    phase = next(p for p in protocol['phases'] if p['id'] == phase_id)
    order = orders[phase['frozen_order_key']]
    if order['seed'] != phase['order_seed']:
        raise ValueError('order seed mismatch')
    validate_orders(phase, order['orders'])
    if (phase['repeats_per_arm_process'] != 3 or phase['read_benchtime'] != '3s' or
            phase['write_benchtime'] != '1000x' or phase['blocks'] != (8 if phase_id == 'C1' else 6)):
        raise ValueError('sample policy differs from frozen testing.B design')
    required = {str(ROOT/'bin'/b) for b in BINARIES} | {CLUSTER, ENGINE, HELPER, *paths.values()}
    if phase_id == 'C3':
        if not network_receipt:
            raise ValueError('C3 requires pinned actual netem/ping receipt')
        required.add(network_receipt)
    if not required.issubset(pins) or any(not str(Path(p)).startswith('/') or not re.fullmatch('[0-9a-f]{64}', h) for p, h in pins.items()):
        raise ValueError('complete absolute-path SHA256 pins required (binaries/config/source/runner/helpers)')
    expected_arms = (['legacy', 'before', 'final'] if phase_id in ('B', 'D1') else
                     ['pr2-ordinary', 'pr2-remote', 'final-ordinary', 'final-remote'] if phase_id == 'C1' else
                     ['final-ordinary', 'final-remote'])
    if phase['arms'] != expected_arms:
        raise ValueError('unknown phase arms/cases')
    if phase_id in ('B', 'D1') and phase['workloads'] != [c['key'] for c in cases_for(phase_id)]:
        raise ValueError('phase workload cases differ from sealed schedule')
    if phase_id == 'C2' and phase['cells'] != ['reverse-small-1000', 'reverse-split-1000', 'executor-skip3-small-10', 'executor-skip3-small-1000']:
        raise ValueError('unknown C2 case')
    if phase_id == 'D2' and phase['rows'] != [10, 1000]:
        raise ValueError('unknown D2 case')
    if phase_id == 'C1' and phase['cells'] != {'range': ['limit-1-broad', 'exact-10', 'all-1000'], 'record': ['small-unsplit', 'two-chunk-split']}:
        raise ValueError('unknown C1 cases')
    if phase_id == 'C3' and (phase['rows'] != 1000 or phase['records'] != ['small-unsplit', 'two-chunk-split']):
        raise ValueError('unknown C3 cases')
    runtime = protocol['runtime']
    groups = {name: parse_cpus(runtime[name+'_cpus']) for name in ('housekeeping', 'server', 'client')}
    population = [cpu for cpus in groups.values() for cpu in cpus]
    # GOMAXPROCS is the client CPU count, so it is also the benchmark name suffix.
    if (runtime['GOMAXPROCS'] != len(groups['client']) or runtime['GOGC'] != 100 or
            len(set(population)) != len(population)):
        raise ValueError('runtime CPU/GC policy mismatch')
    harness = protocol.get('harness_revision', harness_commit)
    if not harness:
        raise ValueError('harness commit required: protocol harness_revision or --harness-commit')
    thresholds = protocol['environment_policy']['pre_arm_thresholds']
    cpu_sets = [dict(name=name, cpus=groups[name], max_busy_fraction=thresholds[name+'_aggregate_busy'],
                     max_cpu_busy_fraction=thresholds[name+'_max_core_busy'])
                for name in ('housekeeping', 'server', 'client')]
    cases = cases_for(phase_id)
    jobs = []
    for ci, case in enumerate(cases):
        for block, arm_order in enumerate(order['orders']):
            for position, arm in enumerate(arm_order):
                name = benchmark_name(case, arm)
                # testing.B treats each slash-separated component as a regex.
                regex = '/'.join('^'+re.escape(part)+'$' for part in name.split('/'))
                command = ['taskset', '--cpu-list', runtime['client_cpus'], binary_for(case, arm),
                           '-test.run=^$', '-test.v', '-test.bench='+regex, '-test.count=3', '-test.benchmem',
                           '-test.benchtime='+('1000x' if case['write'] else '3s'), '-test.timeout=0']
                if case['binary_kind'] == 'rl':
                    command += ['-ginkgo.skip=.*']
                jobs.append(dict(id=f'{phase_id}-c{ci:02d}-b{block:02d}-p{position}-{arm}', case=case,
                                 block=block, position=position, arm=arm, name=name, command=command))
    if not jobs or len({j['id'] for j in jobs}) != len(jobs):
        raise ValueError('empty or duplicate schedule')
    return dict(version=1, campaign=protocol['campaign'], phase=phase, cases=cases, jobs=jobs,
                expected_sha256=pins, paths=paths, harness_commit=harness,
                production_revisions=protocol['production_revisions'], orders=order['orders'],
                network_receipt=network_receipt, phase_wall_cap_seconds=phase['wall_cap_seconds'],
                process_timeout_seconds=min(600, phase['wall_cap_seconds']),
                cooldown_seconds=protocol['sample_policy']['cooldown_seconds'],
                quiescence_seconds=protocol['sample_policy']['quiescence_seconds'], telemetry_seconds=1,
                max_busy_fraction=thresholds['all_cpu_aggregate_busy'], max_steal_fraction=0.01,
                cpu_sets=cpu_sets, analysis_seed=391706, bootstrap_resamples=20000,
                env=dict(GOMAXPROCS=str(runtime['GOMAXPROCS']), GOGC='100', GOMEMLIMIT='off', GODEBUG='', TZ='UTC', LC_ALL='C',
                         FDB_BENCH_CLUSTER_FILE=CLUSTER, FDB_CLUSTER_FILE=CLUSTER),
                interpretation='testing.B means are NOT p99; process wall/CPU are lifetime scopes. Legacy contains parity differences.')


def parse_output(text, job, repeats=3, *, gomaxprocs):
    # testing.B appends -GOMAXPROCS to result names unless it is 1.
    suffix = '' if int(gomaxprocs) == 1 else '-'+str(int(gomaxprocs))
    rows, audits = [], []
    for line in text.splitlines():
        if 'FDB_BENCH_AUDIT ' in line:
            try:
                audit = json.loads(line.split('FDB_BENCH_AUDIT ', 1)[1])
            except (ValueError, TypeError) as exc:
                raise CorrectnessFailure('malformed audit JSON') from exc
            if not isinstance(audit, dict) or not isinstance(audit.get('case'), str):
                raise CorrectnessFailure('audit lacks case')
            audits.append(audit)
        match = re.match(r'^(Benchmark\S+)\s+(\S+)\s+(.+)$', line.strip())
        if not match:
            continue
        # Bare benchmark progress labels are not result rows. Any line carrying
        # result units must be well formed, not silently discarded.
        name, n, rest = match.groups()
        if 'ns/op' not in rest and not n.isdecimal():
            continue
        if name != job['name']+suffix or not n.isdecimal() or int(n) <= 0:
            raise CorrectnessFailure('unexpected benchmark case, CPU suffix or N: '+line)
        parts = rest.split()
        if len(parts) % 2:
            raise CorrectnessFailure('odd/malformed metric fields')
        metrics = {}
        for value, unit in zip(parts[::2], parts[1::2]):
            if unit in metrics:
                raise CorrectnessFailure('duplicate metric '+unit)
            try:
                metrics[unit] = number(float(value), positive=unit in ('ns/op', 'rows/op', 'prefix-B'))
            except (ValueError, OverflowError) as exc:
                raise CorrectnessFailure('malformed metric '+value) from exc
        if not {'ns/op', 'B/op', 'allocs/op'}.issubset(metrics):
            raise CorrectnessFailure('missing required benchmark metrics')
        if job['case']['rows'] is not None and job['case']['audit'] in ('fetch', 'planskip', 'sql-fetch'):
            if metrics.get('rows/op') != job['case']['rows'] or 'prefix-B' not in metrics:
                raise CorrectnessFailure('incorrect rows/op or missing physical prefix metric')
        if job['case']['audit'] in ('sql-read', 'sql-write') and 'prefix-B' not in metrics:
            raise CorrectnessFailure('SQL physical prefix metric missing')
        if job['case']['write'] and int(n) != 1000:
            raise CorrectnessFailure('write benchmark did not use exactly 1000 iterations')
        rows.append(dict(repeat=len(rows), N=int(n), metrics=metrics))
    if len(rows) != repeats:
        raise CorrectnessFailure(f'expected exactly {repeats} result rows, got {len(rows)}')
    if re.search(r'(?m)^(?:FAIL|--- FAIL:|panic:|fatal error:)', text) or not re.search(r'(?m)^PASS\s*$', text):
        raise CorrectnessFailure('benchmark process did not report PASS')
    validate_audits(audits, job, rows)
    return rows, audits


def validate_audits(audits, job, rows):
    c, kind = job['case'], job['case']['audit']
    matching = [a for a in audits if a['case'] == job['name']]
    if kind == 'none':
        return  # Classic read benchmarks intentionally emit no fake receipts.
    for a in matching:
        number(a.get('prefix_bytes'), True)
    if kind == 'classic-write':
        selected = [a for a in matching if a.get('n') == 1000]
        if len(selected) != 3 or any(a.get('count') != 1000 or a.get('first_id') != 0 or a.get('present') is not True for a in selected):
            raise CorrectnessFailure('classic write exact-population receipt missing')
        return
    if kind in ('sql-read', 'sql-write'):
        if len(matching) < 3:
            raise CorrectnessFailure('missing SQL receipts')
        selected = [a for a in matching if a.get('rows') == c['rows']]
        if len(selected) < 3:
            raise CorrectnessFailure('wrong SQL audited population')
        if kind == 'sql-write' and any(a.get('first_id') != 1 or a.get('last_id') != 1000 for a in selected):
            raise CorrectnessFailure('wrong SQL write endpoints')
        if any(r['metrics']['prefix-B'] != a['prefix_bytes'] for r in rows for a in matching):
            raise CorrectnessFailure('SQL physical prefix differs between metrics and audits')
        if kind == 'sql-read':
            for a in matching:
                number(a.get('checksum'))
                if not isinstance(a.get('query'), str) or not a['query']:
                    raise CorrectnessFailure('missing SQL query')
        return
    paired = [a for a in matching if a.get('phase') in ('before', 'after')]
    if len(paired) < 6 or len(paired) % 2:
        raise CorrectnessFailure('missing before/after audit pairs')
    method = 'USE_REMOTE_FETCH' if job['arm'].endswith('-remote') else 'SCAN_AND_FETCH'
    for before, after in zip(paired[::2], paired[1::2]):
        if before.get('phase') != 'before' or after.get('phase') != 'after':
            raise CorrectnessFailure('unordered audit pair')
        for a in (before, after):
            if a.get('method') != method or a.get('rows') != c['rows']:
                raise CorrectnessFailure('wrong method or audit population')
            for field, value in c['fields'].items():
                if a.get(field) != value:
                    raise CorrectnessFailure('wrong audit field '+field)
            for field in ('remote_openings', 'remote_entries', 'index_scans', 'record_loads', 'checksum'):
                number(a.get(field))
            if kind == 'sql-fetch' and (not isinstance(a.get('query'), str) or not a['query']):
                raise CorrectnessFailure('missing audited SQL query')
            if method == 'USE_REMOTE_FETCH':
                if a['remote_openings'] < 1 or a['remote_entries'] < c['rows'] or a['index_scans'] != 0 or a['record_loads'] != 0:
                    raise CorrectnessFailure('remote path audit failed')
                if kind == 'planskip' and (a['remote_openings'] != 1 or a['remote_entries'] != c['rows']+4):
                    raise CorrectnessFailure('Skip is not above one mapped read')
            elif a['remote_openings'] != 0 or a['remote_entries'] != 0 or a['index_scans'] < 1 or a['record_loads'] < c['rows']:
                raise CorrectnessFailure('ordinary path audit failed')
            if kind == 'planskip' and method == 'SCAN_AND_FETCH' and a['record_loads'] != c['rows']:
                raise CorrectnessFailure('ordinary Skip loaded discarded records')
            if any(r['metrics']['prefix-B'] != a['prefix_bytes'] for r in rows):
                raise CorrectnessFailure('physical prefix metric differs from audit')
        if before['checksum'] != after['checksum']:
            raise CorrectnessFailure('before/after checksum mismatch')
    fixtures = [a for a in audits if a.get('phase') == 'fixture']
    if not fixtures or any(a.get('rows') != 1000 for a in fixtures):
        raise CorrectnessFailure('missing/nonempty fixture receipt')
    if kind == 'fetch':
        if not any(all(a.get(k) == v for k, v in c['fields'].items() if k in ('payload', 'payload_bytes', 'chunks'))
                   and a.get('split') == (c['fields']['payload'] == 'split') for a in fixtures):
            raise CorrectnessFailure('payload/chunk fixture mismatch')


def load_helper(path):
    spec = importlib.util.spec_from_file_location('frozen_client_runner_helpers', path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def validate_source_receipt(receipt, plan):
    if receipt.get('harness_commit') != plan['harness_commit'] or receipt.get('production_revisions') != plan['production_revisions']:
        raise EnvironmentFailure('source receipt does not identify frozen harness/production trees')
    sources = receipt.get('source_artifacts_sha256')
    if not isinstance(sources, dict) or not sources or any(plan['expected_sha256'].get(p) != h for p, h in sources.items()):
        raise EnvironmentFailure('source artifact receipts must be nonempty and included in SHA pins')


def retain_raw_audits(folder):
    values = []
    for name in ('stdout.txt', 'stderr.txt'):
        if not (folder/name).exists():
            continue
        for line in (folder/name).read_text().splitlines():
            if 'FDB_BENCH_AUDIT ' in line:
                raw = line.split('FDB_BENCH_AUDIT ', 1)[1]
                try:
                    values.append(json.loads(raw))
                except ValueError:
                    values.append(dict(malformed_raw=raw))
    dump(folder/'audits.raw.json', values)


def verify_pins(plan):
    actual = {p: sha(p) for p in plan['expected_sha256']}
    if actual != plan['expected_sha256']:
        raise EnvironmentFailure('pinned binary/config/source/runner inputs changed')
    return actual


def cluster_guard(plan):
    text = Path(CLUSTER).read_text().strip()
    if not re.fullmatch(r'[^\s@]+@'+re.escape(server_addr()), text):
        raise EnvironmentFailure('refusing clear: not the private owned single-coordinator cluster')
    if Path(ENGINE).read_text().strip() != 'memory':
        raise EnvironmentFailure('testing.B phases require the owned memory-engine cluster')
    if os.stat('/proc/self/ns/net').st_ino != os.stat('/var/run/netns/'+NETNS).st_ino:
        raise EnvironmentFailure('runner is not in the private client namespace')
    if sorted(os.sched_getaffinity(0)) != cpu_set(plan, 'housekeeping'):
        raise EnvironmentFailure('runner/telemetry must be pinned to the housekeeping CPUs')


def controlled_command(command, folder, name, helper, env, timeout=30):
    process = None
    try:
        with open(folder/(name+'.stdout'), 'x') as out, open(folder/(name+'.stderr'), 'x') as err:
            process = subprocess.Popen(command, stdout=out, stderr=err, env=env, start_new_session=True)
            process.wait(timeout=timeout)
        if process.returncode:
            raise EnvironmentFailure(name+' failed; retain raw log')
        return (folder/(name+'.stdout')).read_text()
    except subprocess.TimeoutExpired as exc:
        raise EnvironmentFailure(name+' timeout') from exc
    finally:
        helper.stop_process_group(process)


def health(folder, label, helper, env):
    text = controlled_command(['fdbcli', '-C', CLUSTER, '--exec', 'status json'], folder, label, helper, env)
    try:
        value = json.loads(text)
        if value['cluster']['database_available'] is not True:
            raise ValueError('unavailable')
    except (ValueError, KeyError, TypeError) as exc:
        raise EnvironmentFailure('missing/unhealthy FDB status receipt') from exc


def ensure_idle(helper, owned=()):
    helper.check_builds()
    result = subprocess.run(['ps', '-eo', 'pid=,comm='], capture_output=True, text=True, check=True)
    forbidden = set(BINARIES)
    for line in result.stdout.splitlines():
        pid, name = line.split(maxsplit=1)
        if name in forbidden and int(pid) not in owned:
            raise EnvironmentFailure('another benchmark process is active: '+line)


def telemetry_guard(samples, plan):
    if len(samples) < 2:
        raise EnvironmentFailure('missing telemetry endpoints')
    def counters(sample):
        return {f[0]: list(map(int, f[1:9])) for line in sample['/proc/stat'].splitlines()
                if (f := line.split()) and re.fullmatch('cpu[0-9]+', f[0])}
    servers = [{p['pid'] for p in s.get('processes', []) if p.get('comm') == 'fdbserver'} for s in samples]
    if any(len(pids) != 1 for pids in servers) or any(pids != servers[0] for pids in servers):
        raise EnvironmentFailure('owned FDB server missing/restarted during arm')
    for a, b in zip(samples, samples[1:]):
        seconds = (b['monotonic_ns']-a['monotonic_ns'])/1e9
        if seconds > 2 or seconds <= 0:
            raise EnvironmentFailure('missing/nonmonotonic 1Hz telemetry interval')
        if seconds < 0.95:
            continue
        ac, bc = counters(a), counters(b)
        measured = cpu_set(plan, 'server')+cpu_set(plan, 'client')
        if not all('cpu'+str(i) in ac and 'cpu'+str(i) in bc for i in measured):
            raise EnvironmentFailure('missing CPU telemetry')
        for i in measured:
            delta = [y-x for x, y in zip(ac['cpu'+str(i)], bc['cpu'+str(i)])]
            if min(delta) < 0 or sum(delta) <= 0 or delta[7]/sum(delta) > plan['max_steal_fraction']:
                raise EnvironmentFailure('contaminated block: CPU steal or invalid counters')


def write_metrics(path, samples):
    with open(path, 'x', newline='') as stream:
        writer = csv.DictWriter(stream, fieldnames=['phase', 'case', 'block', 'position', 'arm', 'repeat', 'N', *METRICS])
        writer.writeheader()
        for s in samples:
            writer.writerow({k: s.get(k, '') for k in writer.fieldnames})


def run(plan_path, output, lifetime_deadline):
    plan_path, output = Path(plan_path), Path(output)
    plan_hash = sha(plan_path)
    if Path(str(plan_path)+'.sha256').read_text().strip() != plan_hash:
        raise EnvironmentFailure('plan differs from sealed SHA256 sidecar')
    plan = json.loads(plan_path.read_text())
    # Rebuild the complete schedule from pinned original inputs; do not trust edited argv/cases.
    original = make_plan(json.loads(Path(plan['paths']['protocol']).read_text()),
                         json.loads(Path(plan['paths']['orders']).read_text()), plan['phase']['id'],
                         plan['expected_sha256'], plan['paths'], plan.get('network_receipt'),
                         harness_commit=plan['harness_commit'])
    if original != plan:
        raise EnvironmentFailure('plan differs from sealed protocol/orders')
    output.mkdir(parents=True, exist_ok=False)
    helper = None
    active = None
    samples, results, omitted = [], [], {}
    deadline = time.monotonic()+plan['phase_wall_cap_seconds']
    env = {'PATH': '/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin', 'HOME': str(Path.home()), **plan['env']}
    handlers = {}
    try:
        left = (dt.datetime.fromisoformat(lifetime_deadline.replace('Z', '+00:00'))-dt.datetime.now(dt.timezone.utc)).total_seconds()
        if left < plan['phase_wall_cap_seconds']+600:
            dump(output/'UNEXECUTED.json', dict(reason='budget', remaining_seconds=left, required_seconds=plan['phase_wall_cap_seconds']+600))
            raise EnvironmentFailure('insufficient lifetime for full phase plus artifact collection')
        pins = verify_pins(plan)
        validate_source_receipt(json.loads(Path(plan['paths']['source_receipt']).read_text()), plan)
        for binary in BINARIES:
            path = ROOT/'bin'/binary
            with open(path, 'rb') as stream:
                if stream.read(4) != b'\x7fELF' or not os.access(path, os.X_OK):
                    raise EnvironmentFailure('required compiled executable missing: '+str(path))
        dump(output/'pins.before.json', pins)
        dump(output/'plan.json', plan)
        dump(output/'controller.json', dict(plan_sha256=plan_hash, python=sys.version, argv=sys.argv,
                                          lifetime_deadline=lifetime_deadline, environment=env))
        cluster_guard(plan)
        if plan['phase']['id'] == 'C3':
            receipt = json.loads(Path(plan['network_receipt']).read_text())
            if not (receipt.get('verified') is True and receipt.get('one_way_delay_us') == 250 and
                    receipt.get('ping_packets') == 100 and receipt.get('qdisc_drops') == 0 and
                    all(receipt.get(k) == 0 for k in ('loss_percent', 'jitter_us', 'reorder_percent'))):
                raise EnvironmentFailure('invalid netem/ping verification receipt')
            number(receipt.get('rtt_p50_ms'), True)
            number(receipt.get('rtt_p99_ms'), True)
            dump(output/'network-receipt.json', receipt)
        helper = load_helper(HELPER)
        def interrupted(sig, frame):
            raise KeyboardInterrupt('signal '+str(sig))
        handlers = {sig: signal.signal(sig, interrupted) for sig in (signal.SIGINT, signal.SIGTERM)}
        for job in plan['jobs']:
            key = job['case']['key']
            if key in omitted:
                continue
            folder = output/job['id']
            folder.mkdir()
            dump(folder/'invocation.json', job)
            result = dict(id=job['id'], case=key, block=job['block'], position=job['position'], arm=job['arm'], status='started')
            results.append(result)
            try:
                if time.monotonic()+3 >= deadline:
                    raise EnvironmentFailure('phase wall cap before next arm')
                verify_pins(plan)
                cluster_guard(plan)
                ensure_idle(helper)
                health(folder, 'status-before-clear', helper, env)
                cleared = controlled_command(['fdbcli', '-C', CLUSTER, '--exec',
                                              'writemode on; clearrange "" "\\xff"'],
                                             folder, 'clear-user-keys', helper, env)
                if 'Committed' not in cleared:
                    raise EnvironmentFailure('no committed user-key clear receipt')
                time.sleep(plan['cooldown_seconds'])
                gate = helper.quiescence(plan)
                dump(folder/'quiescence.json', gate)
                if not gate['passed']:
                    raise EnvironmentFailure('pre-arm CPU/steal gate failed')
                ensure_idle(helper)
                helper.snapshot(folder/'before')
                started = time.monotonic_ns()
                telemetry = []
                with open(folder/'stdout.txt', 'x') as out, open(folder/'stderr.txt', 'x') as err, open(folder/'telemetry.jsonl', 'x') as stream:
                    active = subprocess.Popen(['/usr/bin/time', '-v', '-o', str(folder/'process-time.txt'), '--', *job['command']],
                                              stdout=out, stderr=err, env=env, start_new_session=True)
                    process_deadline = min(deadline, time.monotonic()+plan['process_timeout_seconds'])
                    while True:
                        entry = helper.telemetry(active.pid)
                        telemetry.append(entry)
                        stream.write(json.dumps(entry)+'\n')
                        stream.flush()
                        if time.monotonic() >= process_deadline:
                            raise EnvironmentFailure('benchmark process timeout; no retry')
                        ensure_idle(helper, helper.descendants(active.pid))
                        remaining = process_deadline-time.monotonic()
                        if remaining <= 0:
                            raise EnvironmentFailure('process timeout after telemetry')
                        try:
                            active.wait(timeout=min(1, remaining))
                            break
                        except subprocess.TimeoutExpired:
                            pass
                    telemetry.append(helper.telemetry(active.pid))
                    stream.write(json.dumps(telemetry[-1])+'\n')
                rc = active.returncode
                helper.stop_process_group(active)
                active = None
                time_text = (folder/'process-time.txt').read_text()
                cpu = {}
                for field, key in [('User time (seconds)', 'process_user_cpu_seconds'), ('System time (seconds)', 'process_system_cpu_seconds')]:
                    match = re.search(re.escape(field)+r':\s*(\S+)', time_text)
                    if not match:
                        raise EnvironmentFailure('missing GNU time lifetime CPU receipt')
                    cpu[key] = number(float(match.group(1)))
                result.update(cpu)
                result.update(returncode=rc, process_wall_ns=time.monotonic_ns()-started,
                              cpu_scope='GNU time user/system CPU includes fixtures/calibration/audits/shutdown; NOT CPU/op')
                helper.snapshot(folder/'after')
                telemetry_guard(telemetry, plan)
                before_vm = (folder/'before/proc_vmstat').read_text()
                after_vm = (folder/'after/proc_vmstat').read_text()
                if re.search(r'^oom_kill (\d+)$', before_vm, re.M).group(1) != re.search(r'^oom_kill (\d+)$', after_vm, re.M).group(1):
                    raise EnvironmentFailure('OOM during arm')
                health(folder, 'status-after', helper, env)
                verify_pins(plan)
                if rc:
                    stderr = (folder/'stderr.txt').read_text()
                    if rc < 0 or 'signal' in stderr or 'timeout' in stderr.lower():
                        raise EnvironmentFailure('benchmark signalled/environmental failure')
                    raise CorrectnessFailure('benchmark nonzero exit; raw logs retained')
                rows, audits = parse_output((folder/'stdout.txt').read_text()+'\n'+(folder/'stderr.txt').read_text(), job,
                                            gomaxprocs=plan['env']['GOMAXPROCS'])
                dump(folder/'audits.json', audits)
                for row in rows:
                    samples.append(dict(phase=plan['phase']['id'], case=key, arm=job['arm'], block=job['block'],
                                        position=job['position'], repeat=row['repeat'], N=row['N'], **row['metrics']))
                result['status'] = 'ok'
                dump(folder/'parsed.json', rows)
            except EnvironmentFailure as exc:
                if helper:
                    helper.stop_process_group(active)
                active = None
                result.update(status='environment-aborted', error=str(exc), contaminated_block=job['block'])
                omitted[key] = dict(reason='environment-aborted; no ratios/no automatic retries', at=job['id'])
            except CorrectnessFailure as exc:
                result.update(status='correctness-failed', error=str(exc))
                is_final = job['arm'].startswith('final')
                omitted[key] = dict(reason='final correctness-failed; phase stopped' if is_final else 'historical correctness-failed/not-comparable', at=job['id'])
                if is_final:
                    raise
            finally:
                retain_raw_audits(folder)
                dump(folder/'result.json', result)
                dump(output/'progress.json', results)
                dump(output/'omitted.json', omitted)
            print(job['id']+' '+result['status'], flush=True)
        after = verify_pins(plan)
        dump(output/'pins.after.json', after)
        if sha(plan_path) != plan_hash:
            raise EnvironmentFailure('plan changed during execution')
        write_metrics(output/'metrics.csv', samples)
        if time.monotonic() >= deadline:
            raise EnvironmentFailure('phase wall cap exceeded')
        expected = len(plan['jobs'])*3
        if omitted:
            dump(output/'INCOMPLETE.json', dict(reason='retained adverse cases; NOT a complete measurement population', omitted=omitted,
                                               scheduled=len(plan['jobs']), finished=len(results), raw_samples=len(samples)))
            return 2
        if len(samples) != expected or len(results) != len(plan['jobs']) or any(r['status'] != 'ok' for r in results):
            raise CorrectnessFailure('incomplete/nonempty schedule population')
        dump(output/'complete.json', dict(scheduled=len(plan['jobs']), completed=len(results), samples=len(samples),
                                         pins_unchanged=True, plan_sha256=plan_hash))
        return 0
    except BaseException as exc:
        dump(output/'ABORTED.json', dict(error=repr(exc), results=results, omitted=omitted))
        if not (output/'metrics.csv').exists():
            write_metrics(output/'metrics.csv', samples)
        raise
    finally:
        if helper:
            helper.stop_process_group(active)
        for sig, handler in handlers.items():
            signal.signal(sig, handler)
        try:
            dump(output/'pins.finally.json', {p: sha(p) if Path(p).is_file() else None for p in plan['expected_sha256']})
        except OSError as exc:
            dump(output/'pins-finally-error.json', dict(error=str(exc)))


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = parser.add_subparsers(dest='command', required=True)
    p = sub.add_parser('plan')
    for flag in ('protocol', 'orders', 'phase', 'pins', 'source-receipt', 'output'):
        p.add_argument('--'+flag, required=True)
    p.add_argument('--network-receipt')
    p.add_argument('--harness-commit', help='harness commit when protocol.json has no harness_revision')
    p = sub.add_parser('run')
    for flag in ('plan', 'output', 'lifetime-deadline'):
        p.add_argument('--'+flag, required=True)
    args = parser.parse_args()
    if args.command == 'run':
        return run(args.plan, args.output, args.lifetime_deadline)
    paths = dict(protocol=str(Path(args.protocol).resolve()), orders=str(Path(args.orders).resolve()),
                 runner=str(Path(__file__).resolve()), analyzer=str(Path(__file__).with_name('bench_analyze.py').resolve()),
                 source_receipt=str(Path(args.source_receipt).resolve()))
    plan = make_plan(json.loads(Path(args.protocol).read_text()), json.loads(Path(args.orders).read_text()), args.phase,
                     json.loads(Path(args.pins).read_text()), paths, args.network_receipt,
                     harness_commit=args.harness_commit)
    out = Path(args.output)
    if out.exists() or Path(str(out)+'.sha256').exists():
        raise FileExistsError('refusing plan/sidecar overwrite')
    dump(out, plan, exclusive=True)
    with open(str(out)+'.sha256', 'x') as stream:
        stream.write(sha(out)+'\n')
    print(json.dumps(dict(plan=str(out), jobs=len(plan['jobs']), samples=len(plan['jobs'])*3, sha256=sha(out))))
    return 0


if __name__ == '__main__':
    sys.exit(main())
