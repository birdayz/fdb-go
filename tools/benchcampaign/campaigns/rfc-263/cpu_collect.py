#!/usr/bin/env python3
"""Preregistered RFC263 CPU NI collection: prepare a schedule, then collect one approved epoch.

No builds, containers, endpoint estimates, analysis, replacements or resume. Import
and --self-check need no environment variables or real benchmark inputs.
"""
import argparse
from collections import Counter
import ctypes
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
import re
import resource
import secrets
import shutil
import signal
import subprocess
import sys
import tarfile
import time
import traceback

sys.dont_write_bytecode = True
import cpu_a2_spec as a2
import cpu_a2_admission as admission

HERE = Path(__file__).resolve().parent
SEED = 263007
BASELINE = 'fa8725f1dcf6ff3ac66825ab6e3e819efbc0abcc'
BASELINE_BIN = '2dd943fdc9d58ff136449a181189ba5843465e86e6ae696d616a9b1bac4a439e'
HARNESS = 'cfc3a33191ec10b4e0955b3f0f192b1ba3be554104ea61016c2a6e60aabbf400'
CPUS = {0, 1, 6, 7, 11, 12, 13, 18, 19, 23}
AFF = '2,3,14,15'
ENV = {'GOMAXPROCS': '4', 'GOGC': '100', 'GOMEMLIMIT': 'off', 'GODEBUG': ''}
CELLS = tuple((w, n) for n in (8, 32) for w in ('grv', 'get100', 'set100', 'range100'))
CONTRASTS = ('AB', 'baselineAA', 'productionAA')
FORECAST = 25 * 60
DIAGNOSTIC_FORECAST = 8 * 60
CHILD_LIMIT = 120
RESERVE = 5
FORENSIC_LIMIT = 300
FORENSIC_RESERVE = 5


class ForensicTimeout(RuntimeError):
    pass


def forensic_time(deadline):
    if deadline is not None and time.monotonic() >= deadline:
        raise ForensicTimeout('five-minute forensic deadline budget exhausted')


def require(ok, message):
    if not ok:
        raise ValueError(message)


def sha(path, deadline=None):
    forensic_time(deadline)
    result = hashlib.sha256()
    with Path(path).open('rb') as stream:
        for block in iter(lambda: stream.read(1024 * 1024), b''):
            forensic_time(deadline)
            result.update(block)
    forensic_time(deadline)
    return result.hexdigest()


def save(path, data):
    Path(path).write_text(json.dumps(data, sort_keys=True, indent=2, allow_nan=False) + '\n')


def read_json(path):
    return json.loads(Path(path).read_text())


def utc():
    return dt.datetime.now(dt.timezone.utc).isoformat()


def digest(value):
    require(isinstance(value, str) and re.fullmatch('[0-9a-f]{64}', value), 'full SHA256 required')
    return value


def identities(production_tree, production_sha256):
    require(isinstance(production_tree, str) and re.fullmatch('[0-9a-f]{40}', production_tree),
            'production tree must be a full Git tree hash')
    return {'baseline': {'source': BASELINE, 'binary_sha256': BASELINE_BIN},
            'production': {'source': 'tree:' + production_tree, 'binary_sha256': digest(production_sha256)}}


def batch_prefix(prefix, namespace):
    if namespace == '':
        return prefix
    require(isinstance(namespace, str) and re.fullmatch('[0-9a-f]{32}', namespace), '32-hex batch prefix namespace required')
    return 'clientbench/rfc263/' + hashlib.sha256((namespace + '\0' + prefix).encode()).hexdigest()[:48] + '/'


def schedule(ids, prefix_namespace=''):
    rng = random.Random(SEED)
    cells = list(CELLS)
    rng.shuffle(cells)
    runs = []
    for epoch in (1, 2):
        # Eight visits per epoch. Every cell occurs once per visit, in a rotated order.
        # Each cell independently receives 3 AB, 3 BA, baselineAA and productionAA.
        kinds = {}
        for cell in cells:
            kinds[cell] = [('AB', 0)] * 3 + [('AB', 1)] * 3 + [('baselineAA', epoch - 1), ('productionAA', 2 - epoch)]
            rng.shuffle(kinds[cell])
        for visit in range(8):
            offset = ((epoch - 1) * 3 + visit) % 8
            rotated = cells[offset:] + cells[:offset]
            for position, (workload, workers) in enumerate(rotated):
                kind, order = kinds[(workload, workers)][visit]
                roles = [('A', 'baseline'), ('B', 'production')] if kind == 'AB' else [
                    ('A', 'baseline' if kind == 'baselineAA' else 'production'),
                    ('B', 'baseline' if kind == 'baselineAA' else 'production')]
                if order:
                    roles.reverse()
                cell = f'{workload}-w{workers}'
                pair = f'e{epoch}-v{visit}-{cell}'
                for member, (alias, binary) in enumerate(roles):
                    prefix = ('clientbench/rfc263/' + f'{rng.getrandbits(128):032x}').ljust(67, '0') + '/'
                    runs.append(dict(id=f'e{epoch}-r{len(runs) % 128:03d}', epoch=epoch,
                        visit=visit, position=position, block=(epoch - 1) * 8 + visit, cell=cell,
                        workload=workload, workers=workers, iterations=20000 if workers == 8 else 500000,
                        warmup=2000, pair=pair, contrast=kind, order=order, member=member,
                        alias=alias, binary=binary, prefix=batch_prefix(prefix, prefix_namespace), profile='none', **ids[binary]))
    validate_schedule(runs, ids)
    return runs


def validate_schedule(runs, ids):
    require(isinstance(runs, list) and len(runs) == 256, 'exactly256 processes required')
    require(len({r['id'] for r in runs}) == 256, 'duplicate run IDs')
    require(len({r['prefix'] for r in runs}) == 256, 'duplicate prefixes')
    require(all(len(r['prefix'].encode()) == 68 and r['prefix'].endswith('/') for r in runs), 'invalid prefix')
    require(set(ids) == {'baseline', 'production'} and ids == identities(ids['production']['source'].removeprefix('tree:'),
            ids['production']['binary_sha256']), 'invalid frozen identities')
    require([r['id'] for r in runs] == [f'e{e}-r{i:03d}' for e in (1, 2) for i in range(128)], 'run order mismatch')
    for epoch in (1, 2):
        rows = [r for r in runs if r['epoch'] == epoch]
        require(len(rows) == 128, 'epoch must contain128 processes')
        visit_cells = []
        for visit in range(8):
            rr = [r for r in rows if r['visit'] == visit]
            require(len(rr) == 16, 'visit must contain16 processes')
            require([(r['position'], r['member']) for r in rr] == [(p, m) for p in range(8) for m in (0, 1)],
                    'nonadjacent pair or visit order')
            cells = [(r['workload'], r['workers']) for r in rr if r['member'] == 0]
            require(set(cells) == set(CELLS), 'visit cell census mismatch')
            visit_cells.append(cells)
        for i in range(1, 8):
            require(visit_cells[i] == visit_cells[i-1][1:] + visit_cells[i-1][:1], 'cell visits not rotated')
        for workload, workers in CELLS:
            rr = [r for r in rows if (r['workload'], r['workers']) == (workload, workers)]
            require(len(rr) == 16, 'cell needs16 processes/epoch')
            pairs = [rr[i:i + 2] for i in range(0, len(rr), 2)]
            require(Counter(p[0]['contrast'] for p in pairs) == {'AB': 6, 'baselineAA': 1, 'productionAA': 1},
                    'cell contrast population mismatch')
            require(Counter(p[0]['order'] for p in pairs if p[0]['contrast'] == 'AB') == {0: 3, 1: 3}, 'AB order imbalance')
            for pair in pairs:
                first = pair[0]
                require(first['contrast'] in CONTRASTS and first['order'] in (0, 1), 'invalid contrast/order')
                require([r['member'] for r in pair] == [0, 1] and pair[0]['pair'] == pair[1]['pair'], 'pair identity mismatch')
                roles = [('A', 'baseline'), ('B', 'production')] if first['contrast'] == 'AB' else [
                    ('A', 'baseline' if first['contrast'] == 'baselineAA' else 'production'),
                    ('B', 'baseline' if first['contrast'] == 'baselineAA' else 'production')]
                if first['order']:
                    roles.reverse()
                require([(r['alias'], r['binary']) for r in pair] == roles, 'contrast role mismatch')
                for r in pair:
                    require(all(r[k] == first[k] for k in ('epoch', 'visit', 'position', 'block', 'cell',
                            'workload', 'workers', 'iterations', 'warmup', 'pair', 'contrast', 'order')), 'pair metadata mismatch')
                    require(r['cell'] == f'{workload}-w{workers}' and r['iterations'] == (20000 if workers == 8 else 500000)
                            and r['warmup'] == 2000 and r.get('profile') == 'none' and r['block'] == (epoch - 1) * 8 + r['visit'], 'cell/config mismatch')
                    require(all(r[k] == ids[r['binary']][k] for k in ('source', 'binary_sha256')), 'source/binary mapping mismatch')
    require(sum(r['iterations'] for r in runs) == 66560000 and sum(r['warmup'] for r in runs) == 512000,
            'total transaction census mismatch')
    for cell in {r['cell'] for r in runs}:
        for kind in ('baselineAA', 'productionAA'):
            require({r['order'] for r in runs if r['cell'] == cell and r['contrast'] == kind} == {0, 1}, 'AA order imbalance')


def diagnostic_schedule(ids, prefix_namespace=''):
    rng = random.Random(SEED + 1)
    workloads = ['grv', 'get100', 'set100', 'range100']
    rng.shuffle(workloads)
    rows = []
    for block, workload in enumerate(workloads):
        profiles = ('mutex', 'block') if block % 2 == 0 else ('block', 'mutex')
        for profile_index, profile in enumerate(profiles):
            roles = ('baseline', 'production') if (block + profile_index) % 2 == 0 else ('production', 'baseline')
            for member, role in enumerate(roles):
                rows.append(dict(id=f'd-r{len(rows):03d}', epoch=0, block=block * 2 + profile_index,
                    cell=f'{workload}-w32', workload=workload, workers=32, iterations=500000, warmup=2000,
                    profile=profile, contrast='diagnostic', pair=f'd-{workload}-{profile}', order=(block + profile_index) % 2,
                    member=member, alias='A' if role == 'baseline' else 'B', binary=role,
                    prefix=batch_prefix(('clientbench/rfc263/' + f'{rng.getrandbits(128):032x}').ljust(67, '0') + '/', prefix_namespace), **ids[role]))
    require(len(rows) == 16 and len({r['prefix'] for r in rows}) == 16, 'diagnostic census mismatch')
    return rows


def validate_prefix_population(timing, diagnostics):
    require(len(timing) == 256 and len(diagnostics) == 16, 'full timing/diagnostic prefix population required')
    prefixes = [r['prefix'] for r in timing + diagnostics]
    require(len(set(prefixes)) == 272 and all(len(p.encode()) == 68 for p in prefixes),
            'timing and diagnostic prefixes must be272 unique68-byte values')


def disjoint_prefix_population(timing, diagnostics, prior_timing, prior_diagnostics):
    validate_prefix_population(timing, diagnostics)
    validate_prefix_population(prior_timing, prior_diagnostics)
    require({r['prefix'] for r in timing + diagnostics}.isdisjoint(
            r['prefix'] for r in prior_timing + prior_diagnostics), 'prefix reused from prior batch')


def prior_prefix_schedules(evidence, frozen_sha256, copied_from=None, copy_prefix='prior-'):
    prefix = copy_prefix if copied_from is not None else ''
    original = Path(copied_from) if copied_from is not None else evidence
    frozen_path = evidence / (prefix + 'frozen.json')
    frozen = a2.checked_json(frozen_path, digest(frozen_sha256))
    rows = []
    for name in ('schedule.json', 'schedule-diagnostics.json'):
        path = evidence / (prefix + name)
        expected = frozen['hashes'].get(str((original / name).resolve()))
        rows.append(a2.checked_json(path, digest(expected)))
    validate_prefix_population(*rows)
    return rows


def timing_design(frozen):
    design = frozen.get('timing_design', 'a1')
    require(design in ('a1', a2.DESIGN), 'unknown frozen timing design')
    return design


def timing_schedule(ids, prefix_namespace='', design='a1'):
    require(design in ('a1', a2.DESIGN), 'unknown timing design')
    return schedule(ids, prefix_namespace) if design == 'a1' else a2.schedule(ids, prefix_namespace)


def validate_timing_schedule(rows, ids, design='a1'):
    require(design in ('a1', a2.DESIGN), 'unknown timing design')
    return validate_schedule(rows, ids) if design == 'a1' else a2.validate_schedule(rows, ids)


def timing_epoch_count(design='a1'):
    require(design in ('a1', a2.DESIGN), 'unknown timing design')
    return 128 if design == 'a1' else 88


def timing_count(design='a1'):
    return timing_epoch_count(design) * 2


def timing_seed(design='a1'):
    require(design in ('a1', a2.DESIGN), 'unknown timing design')
    return SEED if design == 'a1' else a2.SEED


def timing_forecast(design='a1'):
    require(design in ('a1', a2.DESIGN), 'unknown timing design')
    return FORECAST if design == 'a1' else a2.FORECAST


def a2_ancestry(prior_evidence, prior_frozen_sha256):
    require(prior_frozen_sha256 == a2.A1_FROZEN_SHA256, 'A2 must retain the actual preregistered A1 manifest')
    prior = a2.checked_json(prior_evidence / 'frozen.json', digest(prior_frozen_sha256))
    require(timing_design(prior) == 'a1' and prior['seed'] == SEED, 'A2 prior batch must be A1')
    a2.validate_identities(prior['identities'])
    ancestor = Path(prior['prior_evidence'])
    pin = digest(prior['prior_frozen_sha256'])
    live = prior_prefix_schedules(ancestor, pin)
    copied = prior_prefix_schedules(prior_evidence / 'inputs', pin, ancestor)
    require(live == copied, 'A1 original-invalid ancestry drift')
    return prior, ancestor, pin, live


def retained_a1(e, frozen):
    require(timing_design(frozen) == a2.DESIGN, 'retained A1 bounds require the A2 design')
    require(frozen['prior_frozen_sha256'] == a2.A1_FROZEN_SHA256, 'A2 retained prior manifest pin changed')
    meta = frozen['retained_a1']
    prior = a2.checked_json(e / 'inputs' / 'prior-frozen.json', frozen['prior_frozen_sha256'])
    copied = a2.retained_results(e / 'inputs' / 'retained-a1-results.json', e / 'inputs' / 'retained-a1-artifact-sha256.json',
        meta['results_sha256'], meta['index_sha256'], frozen['identities'], prior['identities'])
    live = a2.retained_results(Path(meta['results_path']), Path(meta['index_path']),
        meta['results_sha256'], meta['index_sha256'], frozen['identities'], prior['identities'])
    require(copied == live, 'retained A1 validation/copy race')
    return copied


def run_name(args):
    return 'diagnostics' if getattr(args, 'population', 'timing') == 'diagnostics' else f'epoch{args.epoch}'


def durable(path, root):
    return not any(Path(path).resolve().is_relative_to(p.resolve()) for p in (root, Path('/tmp'), Path('/run'), Path('/dev/shm')))


def source_tree(root):
    paths = subprocess.check_output(['git', 'ls-files', '-z', '--cached', '--others', '--exclude-standard'], cwd=root)
    files = {}
    for rel in sorted(set(paths.decode().split('\0')) - {''}):
        path = root / rel
        st = path.lstat()
        data = os.readlink(path).encode() if path.is_symlink() else path.read_bytes()
        files[rel] = {'sha256': hashlib.sha256(data).hexdigest(), 'mode': st.st_mode, 'symlink': path.is_symlink()}
    require(len(files) > 1000, 'vacuous source population')
    configs = {str(p): sha(p) if p.is_file() else None for p in
               (root / 'user.bazelrc', Path.home() / '.bazelrc', Path('/etc/bazel.bazelrc'))}
    return {'files': files, 'external_configs': configs}


def verify_inputs(e, frozen):
    if timing_design(frozen) == a2.DESIGN:
        require(frozen['prior_frozen_sha256'] == a2.A1_FROZEN_SHA256, 'A2 prior manifest is not the actual A1 batch')
    require(source_tree(Path(frozen['source_root'])) == frozen['source_tree'], 'source/config drift')
    require(bool(frozen['hashes']), 'empty frozen input hashes')
    for path, expected in frozen['hashes'].items():
        require(sha(path) == expected, 'frozen input drift: ' + path)
    timing, diagnostics = read_json(e / 'schedule.json'), read_json(e / 'schedule-diagnostics.json')
    require(isinstance(frozen.get('prefix_namespace'), str) and re.fullmatch('[0-9a-f]{32}', frozen['prefix_namespace']),
            '32-hex frozen batch prefix namespace required')
    design = timing_design(frozen)
    validate_timing_schedule(timing, frozen['identities'], design)
    require(timing == timing_schedule(frozen['identities'], frozen['prefix_namespace'], design), 'schedule not canonical for frozen timing design')
    require(diagnostics == diagnostic_schedule(frozen['identities'], frozen['prefix_namespace']), 'diagnostic schedule changed')
    prior = prior_prefix_schedules(e / 'inputs', frozen['prior_frozen_sha256'], frozen['prior_evidence'])
    require(prior == prior_prefix_schedules(Path(frozen['prior_evidence']), frozen['prior_frozen_sha256']),
            'copied prior prefix schedules differ from the original')
    if design == 'a1':
        disjoint_prefix_population(timing, diagnostics, *prior)
    else:
        prior_meta, ancestor, pin, live = a2_ancestry(Path(frozen['prior_evidence']), frozen['prior_frozen_sha256'])
        require(frozen['ancestor_evidence'] == str(ancestor.resolve()) and frozen['ancestor_frozen_sha256'] == pin,
                'A2 original-invalid ancestry identity changed')
        copied = prior_prefix_schedules(e / 'inputs', pin, ancestor, copy_prefix='ancestor-')
        require(copied == live, 'A2 copied original-invalid ancestry drift')
        require(frozen['prefix_namespace'] != prior_meta['prefix_namespace'], 'A2 namespace reused from A1')
        admission.historical_inputs(admission.historical_pins(frozen), prior_meta)
        proof = admission.source_equivalence(frozen['source_tree'], prior_meta['source_tree'])
        require(frozen['source_equivalence'] == proof and read_json(e / 'inputs' / 'source-equivalence.json') == proof,
                'A2 source equivalence receipt changed')
        a2.validate_prefix_population(timing, diagnostics, (prior[0] + prior[1], live[0] + live[1]))
        retained_a1(e, frozen)


def load_module(path, name):
    spec = importlib.util.spec_from_file_location(name, path)
    module = importlib.util.module_from_spec(spec)
    sys.modules[name] = module
    spec.loader.exec_module(module)
    return module


def helper_chain(helper_dir, pins):
    require(set(pins) == {'rev5-primary.py', 'rev6-production-tail.py', 'rev6-confirm.py'}, 'helper census mismatch')
    for name, expected in pins.items():
        require(sha(helper_dir / name) == digest(expected), 'helper changed: ' + name)
    # Do not import the primary, which historically had import-time evidence reads.
    tail = load_module(helper_dir / 'rev6-production-tail.py', 'cpu_tail_guard')
    confirm = load_module(helper_dir / 'rev6-confirm.py', 'cpu_confirm_guard')
    require(tail.PRIMARY == pins['rev5-primary.py'] and confirm.HELPER_SHA == pins['rev6-production-tail.py'],
            'primary→tail→confirm hash chain mismatch')
    require(tail.H == HARNESS and confirm.HARNESS == HARNESS and callable(tail.gate), 'helper harness/API mismatch')
    return tail


def deadline_seconds(value, now=None):
    now = now or dt.datetime.now(dt.timezone.utc)
    stop = dt.datetime.fromisoformat(value.replace('Z', '+00:00'))
    require(stop.tzinfo is not None, 'deadline needs timezone')
    seconds = (stop - now).total_seconds()
    require(0 < seconds <= 1800, 'owner-authorized deadline must be future and at most30min away')
    return seconds


def child_timeout(deadline, now=None):
    remaining = deadline - (time.monotonic() if now is None else now) - RESERVE
    require(remaining > 0, 'deadline before child launch; no replacement')
    return min(CHILD_LIMIT, remaining)


def csv_integrity(path, n, workers, phase):
    require(type(n) is int and n > 0 and type(workers) is int and 0 < workers <= n, 'nonempty CSV population required')
    require(phase in ('measure', 'warmup'), 'invalid CSV phase')
    seen = bytearray(n)
    count = 0
    with Path(path).open(newline='') as stream:
        reader = csv.DictReader(stream)
        fields = reader.fieldnames or []
        require(len(fields) == len(set(fields)) and {'index', 'worker', 'phase', 'error', 'check_status', 'latency_ns', 'attempts'} <= set(fields), 'CSV schema mismatch')
        for row in reader:
            require(None not in row and all(v is not None for v in row.values()), 'malformed CSV row')
            index = int(row['index'])
            require(0 <= index < n and not seen[index], 'duplicate/out-of-range index')
            require(int(row['worker']) == index % workers, 'worker mismatch')
            require(row['phase'] == phase and row['error'] == '' and row['check_status'] == 'ok', 'CSV health failure')
            require(int(row['latency_ns']) > 0 and int(row['attempts']) >= 1, 'invalid sample counters')
            seen[index] = 1
            count += 1
    require(count == n, 'empty/incomplete CSV')


def expected_config(row, out, frozen):
    return dict(cluster_file=frozen['cluster'], workload=row['workload'], workers=row['workers'], iterations=row['iterations'],
        warmup=row['warmup'], transaction_timeout_ms=30000, retry_limit=10, allow_writes=True, prefix=row['prefix'],
        profile=row.get('profile', 'none'), source_revision=row['source'], harness_id=HARNESS, run_id=row['id'], output=str(out), block=row['block'])


def check_manifest(manifest, row, out, frozen):
    require(all(manifest['config'].get(k) == v for k, v in expected_config(row, out, frozen).items()), 'manifest configuration mismatch')
    require(manifest['go_version'] == 'go1.26.9' and manifest['gomaxprocs'] == 4 and manifest['environment'] == ENV, 'compiler/runtime mismatch')
    backend = manifest['backend']
    require(backend['executable_sha256'] == row['binary_sha256'] and backend['backend'] == 'go' and backend['native_library'] is False, 'backend mismatch')


def check_summary(summary, row, manifest):
    require(summary['config'] == manifest['config'] and summary['backend'] == manifest['backend'], 'summary/manifest identity mismatch')
    require(summary['success'] is True and summary['failure'] == '' and summary['profile'] == row.get('profile', 'none') and summary['persisted_validation'] == 'ok', 'unhealthy summary')
    require(summary['requested'] == summary['completed'] == row['iterations'] and summary['errors'] == 0 and
            summary['warmup_requested'] == summary['warmup_completed'] == row['warmup'] and summary['warmup_errors'] == 0,
            'measured/warmup population failure')
    require(type(summary['measured_wall_ns']) is int and summary['measured_wall_ns'] > 0, 'invalid measurement wall clock')
    for key in ('retries', 'warmup_retries'):
        require(type(summary[key]) is int and summary[key] >= 0, 'invalid retry counter')
    # Integrity only: never report endpoint values during collection.
    endpoint(summary)


def validate_lifecycle(summary, frozen, evidence):
    helper = evidence / 'inputs' / 'lifecycle_helper.py'
    require(sha(helper) == frozen['lifecycle_helper_sha256'], 'lifecycle helper changed')
    load_module(helper, 'cpu_lifecycle_guard').validate_lifecycle(summary)


def endpoint(summary):
    n = summary['completed']
    require(type(n) is int and n > 0, 'nonempty endpoint denominator required')
    before, after = summary['stats_before'], summary['stats_after']
    identity, shutdown = summary['stats_after_identity'], summary['lifecycle']['stats_after_shutdown']
    drain = summary['lifecycle']['stats_after_measurement_drain']
    require(summary['lifecycle']['complete'] is True, 'incomplete lifecycle')
    keys = ('process_user_cpu_us', 'process_system_cpu_us', 'go_mallocs', 'go_total_alloc_bytes', 'go_gc_cycles')
    snapshots = (identity, before, after, drain, shutdown)
    for k in keys:
        require(all(type(s[k]) is int and s[k] >= 0 for s in snapshots) and
                all(a[k] <= b[k] for a, b in zip(snapshots, snapshots[1:])), 'invalid/decreasing endpoint counter: ' + k)
    cpu = sum(after[k] - before[k] for k in keys[:2])
    lifecycle_cpu = sum(shutdown[k] - identity[k] for k in keys[:2])
    require(cpu > 0 and lifecycle_cpu > 0, 'nonpositive process CPU endpoint')
    return dict(window_cpu_us_per_tx=cpu / n, lifecycle_cpu_us_per_tx=lifecycle_cpu / n,
                allocations_per_tx=(after['go_mallocs'] - before['go_mallocs']) / n,
                allocated_bytes_per_tx=(after['go_total_alloc_bytes'] - before['go_total_alloc_bytes']) / n,
                gc_cycles=after['go_gc_cycles'] - before['go_gc_cycles'])


def prepare(args):
    root, e = args.source_root.resolve(), args.evidence.resolve()
    require(durable(e, root), 'evidence must be durable and off-tree')
    ids = identities(args.production_tree, args.production_sha256)
    design = getattr(args, 'timing_design', 'a1')
    require(design in ('a1', a2.DESIGN), 'unknown timing design')
    a2_metadata = {}
    if design == a2.DESIGN:
        require(args.prior_frozen_sha256 == a2.A1_FROZEN_SHA256, 'A2 requires the actual A1 manifest pin')
    require(subprocess.check_output(['git', 'cat-file', '-t', args.production_tree], cwd=root, text=True).strip() == 'tree',
            'production-tree object itself must be a tree, not a commit')
    require(not subprocess.check_output(['git', 'status', '--porcelain=v1', '--untracked-files=all'], cwd=root, text=True),
            'sealed tree requires clean tracked/staged source and zero nonignored untracked inputs')
    require(subprocess.check_output(['git', 'rev-parse', 'HEAD^{tree}'], cwd=root, text=True).strip() == args.production_tree,
            'HEAD tree is not the sealed production input tree')
    subprocess.run(['git', 'diff', '--exit-code', args.production_tree, '--'], cwd=root, check=True)
    require(isinstance(args.prefix_namespace, str) and re.fullmatch('[0-9a-f]{32}', args.prefix_namespace),
            '32-hex batch prefix namespace required')
    prior_timing, prior_diagnostics = prior_prefix_schedules(args.prior_evidence, args.prior_frozen_sha256)
    if design == a2.DESIGN:
        a2.validate_identities(ids)
        prior_meta, ancestor, ancestor_pin, ancestor_rows = a2_ancestry(args.prior_evidence, args.prior_frozen_sha256)
        require(args.prefix_namespace != prior_meta['prefix_namespace'], 'A2 requires a new namespace, not A1 reuse')
        retained = a2.retained_results(args.retained_results, args.retained_index, args.retained_results_sha256,
                                      args.retained_index_sha256, ids, prior_meta['identities'])
        a2_metadata = dict(ancestor_evidence=str(ancestor.resolve()), ancestor_frozen_sha256=ancestor_pin,
            retained_a1=dict(results_path=str(args.retained_results.resolve()), index_path=str(args.retained_index.resolve()),
                             results_sha256=args.retained_results_sha256, index_sha256=args.retained_index_sha256))
    pins = dict(zip(('rev5-primary.py', 'rev6-production-tail.py', 'rev6-confirm.py'),
                    (args.primary_sha256, args.tail_sha256, args.confirm_sha256)))
    if design == a2.DESIGN:
        admission.historical_inputs(dict(helper_hashes=pins, lifecycle_helper_sha256=args.lifecycle_sha256,
            pprof_sha256=args.pprof_sha256, cluster_sha256=args.cluster_sha256, baseline_source_sha256=args.baseline_source_sha256), prior_meta)
    helper_chain(args.helper_dir, pins)
    require(sha(args.lifecycle_helper) == digest(args.lifecycle_sha256), 'lifecycle helper mismatch')
    require(sha(args.pprof_binary) == digest(args.pprof_sha256), 'prebuilt pprof mismatch')
    require(sha(args.plan_file) == digest(args.plan_sha256), 'approved plan mismatch')
    require(sha(args.baseline_source_archive) == digest(args.baseline_source_sha256), 'baseline source archive mismatch')
    require(callable(load_module(args.lifecycle_helper, 'cpu_lifecycle_prepare').validate_lifecycle), 'lifecycle helper API mismatch')
    require(sha(args.baseline_binary) == BASELINE_BIN and sha(args.production_binary) == args.production_sha256, 'executable mismatch')
    require(sha(args.harness) == HARNESS and sha(args.cluster) == digest(args.cluster_sha256), 'harness/cluster mismatch')
    e.mkdir(parents=True, exist_ok=False)
    (e / 'inputs').mkdir()
    (e / 'bin').mkdir()
    for script in (HERE / 'cpu_collect.py', HERE / 'cpu_analyze.py', HERE / 'rev6-cpu-queuewait.py', HERE / 'cpu_queuewait_guard_test.py', HERE / 'cpu_guard_test.py', HERE / 'cpu_a2_spec.py', HERE / 'cpu_a2_analyze.py', HERE / 'cpu_a2_guard_test.py', HERE / 'cpu_a2_power.py', HERE / 'cpu_a2_admission.py'):
        shutil.copy2(script, e / 'inputs' / script.name)
    for name in pins:
        shutil.copy2(args.helper_dir / name, e / 'inputs' / name)
    shutil.copy2(args.harness, e / 'inputs' / 'harness.tar.gz')
    shutil.copy2(args.plan_file, e / 'inputs' / 'rev6-cpu-plan.md')
    shutil.copy2(args.lifecycle_helper, e / 'inputs' / 'lifecycle_helper.py')
    shutil.copy2(args.prior_evidence / 'frozen.json', e / 'inputs' / 'prior-frozen.json')
    shutil.copy2(args.prior_evidence / 'schedule.json', e / 'inputs' / 'prior-schedule.json')
    shutil.copy2(args.prior_evidence / 'schedule-diagnostics.json', e / 'inputs' / 'prior-schedule-diagnostics.json')
    require(prior_prefix_schedules(e / 'inputs', args.prior_frozen_sha256, args.prior_evidence) ==
            [prior_timing, prior_diagnostics], 'prior prefix validation/copy race')
    if design == a2.DESIGN:
        for name in ('frozen.json', 'schedule.json', 'schedule-diagnostics.json'):
            shutil.copy2(ancestor / name, e / 'inputs' / ('ancestor-' + name))
        require(prior_prefix_schedules(e / 'inputs', ancestor_pin, ancestor, copy_prefix='ancestor-') == ancestor_rows,
                'original-invalid ancestry validation/copy race')
        shutil.copy2(args.retained_results, e / 'inputs' / 'retained-a1-results.json')
        shutil.copy2(args.retained_index, e / 'inputs' / 'retained-a1-artifact-sha256.json')
        require(a2.retained_results(e / 'inputs' / 'retained-a1-results.json', e / 'inputs' / 'retained-a1-artifact-sha256.json',
                    args.retained_results_sha256, args.retained_index_sha256, ids, prior_meta['identities']) == retained and
                a2.retained_results(args.retained_results, args.retained_index, args.retained_results_sha256,
                    args.retained_index_sha256, ids, prior_meta['identities']) == retained, 'retained A1 validation/copy race')
    shutil.copy2(args.baseline_source_archive, e / 'inputs' / 'baseline-source.tar.gz')
    shutil.copy2(args.pprof_binary, e / 'bin' / 'pprof')
    (e / 'bin' / 'pprof').chmod(0o555)
    require(args.provenance.is_file(), 'provenance receipt required')
    provenance = read_json(args.provenance)
    require(provenance.get('identities') == ids and provenance.get('sealed_production_tree') == args.production_tree and
            provenance.get('baseline_source_archive') == {'source': BASELINE, 'sha256': args.baseline_source_sha256} and
            bool(provenance.get('owner_ack')), 'provenance must bind owner ACK to sealed input tree, binaries and baseline source archive')
    shutil.copy2(args.provenance, e / 'inputs' / 'provenance.json')
    for role, origin in (('baseline', args.baseline_binary), ('production', args.production_binary)):
        shutil.copy2(origin, e / 'bin' / role)
        (e / 'bin' / role).chmod(0o555)
        require(sha(e / 'bin' / role) == ids[role]['binary_sha256'], 'binary copy mismatch')
    runs, diagnostics = timing_schedule(ids, args.prefix_namespace, design), diagnostic_schedule(ids, args.prefix_namespace)
    if design == 'a1':
        disjoint_prefix_population(runs, diagnostics, prior_timing, prior_diagnostics)
    else:
        a2.validate_prefix_population(runs, diagnostics, (prior_timing + prior_diagnostics, ancestor_rows[0] + ancestor_rows[1]))
    save(e / 'schedule.json', runs)
    save(e / 'schedule-diagnostics.json', diagnostics)
    for epoch in (1, 2):
        save(e / f'schedule-epoch{epoch}.json', [r for r in runs if r['epoch'] == epoch])
    tree = source_tree(root)
    if design == a2.DESIGN:
        proof = admission.source_equivalence(tree, prior_meta['source_tree'])
        a2_metadata['source_equivalence'] = proof
        save(e / 'inputs' / 'source-equivalence.json', proof)
    save(e / 'inputs' / 'source-tree.json', tree)
    with tarfile.open(e / 'inputs' / 'source.tar.gz', 'w:gz', compresslevel=1) as archive:
        for rel in tree['files']:
            archive.add(root / rel, arcname=rel, recursive=False)
    paths = list((e / 'inputs').iterdir()) + list((e / 'bin').iterdir()) + list(e.glob('schedule*.json'))
    paths += [args.cluster, args.baseline_binary, args.production_binary, args.harness, args.provenance, args.lifecycle_helper,
              args.pprof_binary, args.baseline_source_archive, args.plan_file]
    paths += [args.prior_evidence / name for name in ('frozen.json', 'schedule.json', 'schedule-diagnostics.json')]
    if design == a2.DESIGN:
        paths += [ancestor / name for name in ('frozen.json', 'schedule.json', 'schedule-diagnostics.json')]
        paths += [args.retained_results, args.retained_index]
    paths += [args.helper_dir / name for name in pins] + [HERE / name for name in ('cpu_collect.py', 'cpu_analyze.py', 'rev6-cpu-queuewait.py', 'cpu_queuewait_guard_test.py', 'cpu_guard_test.py', 'cpu_a2_spec.py', 'cpu_a2_analyze.py', 'cpu_a2_guard_test.py', 'cpu_a2_power.py', 'cpu_a2_admission.py')]
    frozen = dict(seed=timing_seed(design), timing_design=design, prefix_namespace=args.prefix_namespace, prior_evidence=str(args.prior_evidence.resolve()),
        prior_frozen_sha256=args.prior_frozen_sha256, prepared_utc=utc(), identities=ids, source_root=str(root), source_tree=tree,
        sealed_production_tree=args.production_tree, cluster=str(args.cluster.resolve()), lock=str(args.lock.resolve()),
        helper_hashes=pins, lifecycle_helper_sha256=args.lifecycle_sha256, pprof_sha256=args.pprof_sha256,
        source_archives={'baseline': {'path': str(e / 'inputs' / 'baseline-source.tar.gz'), 'sha256': args.baseline_source_sha256, 'source': BASELINE},
                         'production': {'path': str(e / 'inputs' / 'source.tar.gz'), 'sha256': sha(e / 'inputs' / 'source.tar.gz'), 'source': ids['production']['source']}},
        forecast_seconds_per_epoch=timing_forecast(design), hashes={str(p.resolve()): sha(p) for p in paths})
    frozen.update(a2_metadata)
    save(e / 'frozen.json', frozen)
    verify_inputs(e, frozen)
    print(f'PREPARED design={design};{len(runs)} processes;{timing_epoch_count(design)}/epoch;'
          f'{sum(r["iterations"] for r in runs)} measured;{sum(r["warmup"] for r in runs)} warmup; no measurements launched')
    print('FROZEN_SHA256=' + sha(e / 'frozen.json'))


def frozen_inputs(args):
    require(sha(args.evidence / 'frozen.json') == digest(args.frozen_sha256), 'frozen manifest changed')
    frozen = read_json(args.evidence / 'frozen.json')
    require(frozen['seed'] == timing_seed(timing_design(frozen)), 'seed mismatch')
    verify_inputs(args.evidence, frozen)
    return frozen


def check_status(status, rows, expected_count=128):
    require(type(expected_count) is int and expected_count in (128, 88), 'unsupported epoch population')
    require(len(rows) == expected_count and status['complete'] is True and status['input_hashes_verified_after'] is True and
            status['completed'] == [r['id'] for r in rows], 'incomplete epoch population')


def confirmation_epoch_seals(e, frozen):
    guard_path = (e / 'inputs' / 'cpu_analyze.py').resolve()
    require(str(guard_path) in frozen['hashes'] and sha(guard_path) == frozen['hashes'][str(guard_path)],
            'confirmation epoch guard is not frozen')
    guard = load_module(guard_path, 'cpu_a2_confirmation_epoch_guard')
    rows = read_json(e / 'schedule.json')
    validate_timing_schedule(rows, frozen['identities'], a2.DESIGN)
    seals = {}
    for epoch in (1, 2):
        seals[str(epoch)] = sha(e / f'epoch{epoch}' / 'artifact-sha256.json')
        guard.checked_epoch(e, epoch, seals[str(epoch)], [r for r in rows if r['epoch'] == epoch], expected_count=88)
    return seals


def admit_a2_confirmation(args, e, frozen, directory):
    pin = digest(args.confirmation_results_sha256)
    require(sha(e / 'frozen.json') == digest(args.frozen_sha256), 'A2 frozen manifest drift before diagnostic admission')
    seals = confirmation_epoch_seals(e, frozen)
    result = a2.checked_json(args.confirmation_results, pin)
    retained = retained_a1(e, frozen)
    admission.confirmation(result, frozen, args.frozen_sha256, seals, retained)
    copied = directory / 'confirmation-results.json'
    require(not copied.exists(), 'refuse confirmation receipt replacement')
    shutil.copy2(args.confirmation_results, copied)
    require(a2.checked_json(copied, pin) == result and a2.checked_json(args.confirmation_results, pin) == result,
            'A2 confirmation validation/copy race')
    require(sha(e / 'frozen.json') == args.frozen_sha256 and confirmation_epoch_seals(e, frozen) == seals,
            'A2 frozen/epoch inputs changed during confirmation copy')
    receipt = dict(scope='rfc263-a2-diagnostic-admission', admitted_utc=utc(), confirmation_verdict='PASS',
        results_origin=str(args.confirmation_results.resolve()), results_sha256=pin, frozen_sha256=args.frozen_sha256,
        epoch_seals=seals, identities=frozen['identities'])
    save(directory / 'confirmation-admission.json', receipt)


def recheck_a2_confirmation(args, e, frozen, directory):
    receipt = read_json(directory / 'confirmation-admission.json')
    pin = digest(args.confirmation_results_sha256)
    require(receipt['scope'] == 'rfc263-a2-diagnostic-admission' and receipt['results_sha256'] == pin and
            receipt['frozen_sha256'] == args.frozen_sha256 and receipt['identities'] == frozen['identities'] and
            receipt['confirmation_verdict'] == 'PASS', 'A2 confirmation admission drift')
    seals = confirmation_epoch_seals(e, frozen)
    require(receipt['epoch_seals'] == seals and sha(e / 'frozen.json') == args.frozen_sha256, 'A2 confirmation epoch/frozen drift')
    result = a2.checked_json(directory / 'confirmation-results.json', pin)
    require(a2.checked_json(args.confirmation_results, pin) == result, 'A2 live confirmation receipt drift')
    admission.confirmation(result, frozen, args.frozen_sha256, seals, retained_a1(e, frozen))


def verify_a2_confirmation_admission(e, frozen, frozen_sha256, epoch_seals, diagnostic_seal):
    directory = e / 'diagnostics'
    index = a2.checked_json(directory / 'artifact-sha256.json', digest(diagnostic_seal))
    receipt = a2.checked_json(directory / 'confirmation-admission.json', digest(index['confirmation-admission.json']))
    require(receipt['scope'] == 'rfc263-a2-diagnostic-admission' and receipt['confirmation_verdict'] == 'PASS' and
            receipt['frozen_sha256'] == frozen_sha256 and receipt['epoch_seals'] == epoch_seals and
            receipt['identities'] == frozen['identities'], 'sealed A2 diagnostic admission binding mismatch')
    pin = digest(receipt['results_sha256'])
    require(index['confirmation-results.json'] == pin, 'sealed confirmation result pin mismatch')
    result = a2.checked_json(directory / 'confirmation-results.json', pin)
    admission.confirmation(result, frozen, frozen_sha256, epoch_seals, retained_a1(e, frozen))
    return receipt


def collect(args):
    e = args.evidence
    diagnostics = getattr(args, 'population', 'timing') == 'diagnostics'
    epoch = e / run_name(args)
    epoch.mkdir(exist_ok=False)
    (epoch / 'runs').mkdir()
    status = dict(epoch=args.epoch, started_utc=utc(), complete=False, completed=[], authorization=args.authorization,
                  deadline_utc=args.deadline_utc, input_hashes_verified_after=False)
    save(epoch / 'status.json', status)
    try:
        require(os.getpriority(os.PRIO_PROCESS, 0) == 19 and set(os.sched_getaffinity(0)) == CPUS, 'controller needs nice19/reserved affinity')
        require(not any(k.startswith('FDB_NETWORK_OPTION_') for k in os.environ), 'inherited network option')
        frozen = frozen_inputs(args)
        helper = helper_chain(e / 'inputs', frozen['helper_hashes'])
        design = timing_design(frozen)
        all_rows = read_json(e / 'schedule.json')
        validate_timing_schedule(all_rows, frozen['identities'], design)
        rows = read_json(e / ('schedule-diagnostics.json' if diagnostics else f'schedule-epoch{args.epoch}.json'))
        require(rows == (diagnostic_schedule(frozen['identities'], frozen['prefix_namespace']) if diagnostics else [r for r in all_rows if r['epoch'] == args.epoch]), 'population schedule mismatch')
        if diagnostics:
            for prior_epoch in (1, 2):
                check_status(read_json(e / f'epoch{prior_epoch}' / 'status.json'), [r for r in all_rows if r['epoch'] == prior_epoch], timing_epoch_count(design))
        if args.epoch == 2:
            check_status(read_json(e / 'epoch1' / 'status.json'), [r for r in all_rows if r['epoch'] == 1], timing_epoch_count(design))
        save(epoch / 'runtime.json', dict(utc=utc(), python=sys.version, uname=list(os.uname()), env=ENV,
             nice=19, controller_affinity=sorted(CPUS), benchmark_affinity=AFF))
        env = dict(os.environ, **ENV)
        with collection_lock(frozen) as lock:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
            require(args.deadline - time.monotonic() >= (DIAGNOSTIC_FORECAST if diagnostics else timing_forecast(design)), 'planning forecast does not fit; no child launched')
            if diagnostics and design == a2.DESIGN:
                admit_a2_confirmation(args, e, frozen, epoch)
            quiet_since = None
            while quiet_since is None or time.monotonic() - quiet_since < 60:
                child_timeout(args.deadline)
                load = helper.gate(epoch)
                quiet_since = (quiet_since or time.monotonic()) if load <= 7.5 else None
                time.sleep(min(5, child_timeout(args.deadline)))
            for row in rows:
                while True:
                    child_timeout(args.deadline)
                    if helper.gate(epoch) <= 7.5:
                        time.sleep(min(2, child_timeout(args.deadline)))
                        if helper.gate(epoch) <= 7.5:
                            break
                    time.sleep(min(5, child_timeout(args.deadline)))
                timeout = child_timeout(args.deadline)
                out = epoch / 'runs' / row['id']
                require(not out.exists(), 'refuse replacement run')
                cmd = ['taskset', '-c', AFF, str(e / 'bin' / row['binary'])]
                for key, value in expected_config(row, out, frozen).items():
                    if key == 'allow_writes':
                        cmd.append('--allow-writes')
                    else:
                        cmd += ['--' + key.replace('_', '-'), str(value)]
                receipt = epoch / 'runs' / (row['id'] + '.command.json')
                meta = dict(schedule=row, argv=cmd, env=ENV, started_utc=utc(), timeout_seconds=timeout,
                            usage_before=list(resource.getrusage(resource.RUSAGE_CHILDREN)))
                save(receipt, meta)
                started = time.monotonic_ns()
                try:
                    with (epoch / 'runs' / (row['id'] + '.stdout.log')).open('w') as log:
                        result = subprocess.run(cmd, env=env, stdout=log, stderr=subprocess.STDOUT, timeout=timeout)
                    meta['returncode'] = result.returncode
                    require(result.returncode == 0, 'benchmark technical failure; no replacement')
                finally:
                    meta.update(finished_utc=utc(), duration_ns=time.monotonic_ns() - started,
                                usage_after=list(resource.getrusage(resource.RUSAGE_CHILDREN)))
                    save(receipt, meta)
                # Deliberately no 10-second duration exclusion, at either worker count.
                manifest, summary = read_json(out / 'manifest.json'), read_json(out / 'summary.json')
                check_manifest(manifest, row, out, frozen)
                check_summary(summary, row, manifest)
                validate_lifecycle(summary, frozen, e)
                csv_integrity(out / 'samples.csv', row['iterations'], row['workers'], 'measure')
                csv_integrity(out / 'warmup.csv', row['warmup'], row['workers'], 'warmup')
                if diagnostics:
                    require((out / (row['profile'] + '.pprof')).stat().st_size > 0, 'empty/missing diagnostic profile')
                artifacts = [p for p in out.iterdir() if p.is_file()] + [receipt, epoch / 'runs' / (row['id'] + '.stdout.log')]
                save(epoch / 'runs' / (row['id'] + '.sha256.json'), {str(p.relative_to(epoch)): sha(p) for p in artifacts})
                helper.gate(epoch)
                status['completed'].append(row['id'])
                save(epoch / 'status.json', status)
                print(f'HEALTH population={run_name(args)} completed={len(status["completed"])}/{len(rows)}', flush=True)
            if diagnostics and design == a2.DESIGN:
                recheck_a2_confirmation(args, e, frozen, epoch)
            verify_inputs(e, frozen)
            status.update(complete=True, input_hashes_verified_after=True)
            if diagnostics:
                require(len(rows) == 16 and status['completed'] == [r['id'] for r in rows], 'full16 diagnostic population required')
            else:
                check_status(status, rows, timing_epoch_count(design))
    except BaseException as exc:
        status['failure'] = repr(exc)
        (epoch / 'failure.txt').write_text(traceback.format_exc())
        raise
    finally:
        status['finished_utc'] = utc()
        save(epoch / 'status.json', status)
        # Successful or not, seal all available artifacts. Only full epochs are analyzable.
        # Only the supervisor hashes/seals after proving the sampling group gone.


def seal_population(directory, deadline=None, forensic_log=None):
    forensic_time(deadline)
    index = {}
    for path in sorted(directory.rglob('*')):
        forensic_time(deadline)
        if path.is_file() and path.name != 'artifact-sha256.json':
            index[str(path.relative_to(directory))] = sha(path, deadline)
    if forensic_log is not None:
        forensic_log.update(state='complete', finished_utc=utc())
        save(directory / 'forensic-status.json', forensic_log)
        index['forensic-status.json'] = sha(directory / 'forensic-status.json', deadline)
    forensic_time(deadline)
    save(directory / 'artifact-sha256.json', index)
    return sha(directory / 'artifact-sha256.json', deadline)


def collection_lock(frozen):
    # The supervisor retains this same open-file-description lock if the worker
    # exits first. A stubborn benchmark descendant cannot outlive the lane lock.
    inherited = os.environ.get('RFC263_CPU_LOCK_FD')
    if inherited is None:
        return Path(frozen['lock']).open('a')
    fd = int(inherited)
    opened, named = os.fstat(fd), Path(frozen['lock']).stat()
    require((opened.st_dev, opened.st_ino) == (named.st_dev, named.st_ino), 'inherited lane lock identity mismatch')
    return os.fdopen(os.dup(fd), 'a')


def require_supervised_worker(frozen):
    require(os.environ.get('RFC263_CPU_SUPERVISOR_PID') == str(os.getppid()) and os.environ.get('RFC263_CPU_LOCK_FD'),
            'direct worker invocation forbidden; validated supervisor PID and inherited lock FD required')
    with collection_lock(frozen) as lock:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)


def enable_subreaper():
    # Linux affinity/proc collection already requires Linux. Adopt orphaned
    # benchmark descendants so they can be reaped before the lane lock closes.
    libc = ctypes.CDLL(None, use_errno=True)
    require(libc.prctl(36, 1, 0, 0, 0) == 0, 'cannot enable child subreaper: errno=' + str(ctypes.get_errno()))


def stop_requested(signum, frame):
    raise KeyboardInterrupt('collection signal ' + str(signum))


def stop_process_group(child, deadline=None):
    def wait_budget(seconds):
        return seconds if deadline is None else max(0, min(seconds, deadline - time.monotonic()))
    grace_end = time.monotonic() + wait_budget(2)
    try:
        try:
            os.killpg(child.pid, signal.SIGTERM)
        except ProcessLookupError:
            pass
        try:
            child.wait(timeout=wait_budget(2))
        except subprocess.TimeoutExpired:
            pass
        remaining = grace_end - time.monotonic()
        if remaining > 0:
            time.sleep(remaining)
    finally:
        # KILL is unconditional, even if TERM/wait failed or the leader exited.
        try:
            os.killpg(child.pid, signal.SIGKILL)
        except ProcessLookupError:
            pass
    child.wait(timeout=wait_budget(2))
    reap_end = time.monotonic() + wait_budget(2)
    while True:
        try:
            pid, _ = os.waitpid(-child.pid, os.WNOHANG)
        except ChildProcessError:
            pid = 0
        if pid:
            continue
        try:
            os.killpg(child.pid, 0)
        except ProcessLookupError:
            require(deadline is None or time.monotonic() <= deadline, 'group-gone proof crossed hard sampling deadline')
            return
        require(time.monotonic() < reap_end, 'process group did not disappear after KILL/reap')
        time.sleep(.01)


def terminal_failure(args, reason):
    directory = args.evidence / run_name(args)
    directory.mkdir(parents=True, exist_ok=True)
    path = directory / 'status.json'
    status = dict(epoch=args.epoch, completed=[], authorization=args.authorization, deadline_utc=args.deadline_utc)
    if path.exists():
        try:
            prior = read_json(path)
            require(isinstance(prior, dict), 'invalid interrupted status')
            status.update(prior)
        except (ValueError, TypeError):
            shutil.copy2(path, directory / 'interrupted-status.json')
    status.update(complete=False, input_hashes_verified_after=False, supervisor_failure=reason, finished_utc=utc())
    save(path, status)
    receipt = dict(utc=utc(), failure=reason)
    save(directory / 'supervisor-stop.json', receipt)
    save(args.evidence / (run_name(args) + '-supervisor-stop.json'), receipt)
    # The supervisor seals and publishes once, after all descendants are gone.


def forensic_seal(args, sampling_stopped_utc, started_monotonic=None):
    directory = args.evidence / run_name(args)
    start = dt.datetime.fromisoformat(sampling_stopped_utc)
    end = (time.monotonic() if started_monotonic is None else started_monotonic) + FORENSIC_LIMIT
    forensic_time(end)
    work_end = end - FORENSIC_RESERVE
    log = dict(state='running', sampling_stopped_utc=sampling_stopped_utc, started_utc=start.isoformat(), entered_utc=utc(),
               deadline_utc=(start + dt.timedelta(seconds=FORENSIC_LIMIT)).isoformat(),
               hash_deadline_utc=(start + dt.timedelta(seconds=FORENSIC_LIMIT - FORENSIC_RESERVE)).isoformat())
    save(directory / 'forensic-status.json', log)
    print('FORENSIC_STARTED', run_name(args), log['started_utc'], 'deadline_utc=' + log['deadline_utc'], flush=True)
    try:
        final_seal = seal_population(directory, work_end, log)
        terminal = dict(log, finished_utc=utc(), final_seal_sha256=final_seal)
        forensic_time(work_end)
        save(args.evidence / (run_name(args) + '-forensic-terminal.json'), terminal)
        forensic_time(end)
    except BaseException as exc:
        # Hashing stops five seconds before the tail cap, leaving only small
        # status writes. Never publish a seal for an incomplete forensic tail.
        forensic_time(end)
        terminal_failure(args, 'incomplete forensic tail: ' + repr(exc))
        log.update(state='incomplete', finished_utc=utc(), failure=repr(exc))
        forensic_time(end)
        save(directory / 'forensic-status.json', log)
        save(args.evidence / (run_name(args) + '-forensic-terminal.json'), log)
        print('FORENSIC_INCOMPLETE', run_name(args), log['finished_utc'], repr(exc), flush=True)
        return False
    print('FORENSIC_FINISHED', run_name(args), terminal['finished_utc'], flush=True)
    print(run_name(args).upper() + '_SEAL_SHA256=' + final_seal, flush=True)
    return True


def group_members(pgid):
    rows = []
    for path in Path('/proc').glob('[0-9]*/stat'):
        try:
            fields = path.read_text().rsplit(')', 1)[1].split()
            if int(fields[2]) == pgid:
                rows.append(dict(pid=int(path.parent.name), state=fields[0], ppid=int(fields[1]), pgid=pgid))
        except (FileNotFoundError, ProcessLookupError, PermissionError):
            continue
    return rows


def group_gone(pgid):
    while True:
        try:
            pid, _ = os.waitpid(-pgid, os.WNOHANG)
        except ChildProcessError:
            pid = 0
        if not pid:
            break
    try:
        os.killpg(pgid, 0)
    except ProcessLookupError:
        return True
    except PermissionError:
        return False
    return False


def release_ack(path, args, token, pgid):
    try:
        require(path.is_file() and not path.is_symlink() and path.stat().st_uid == os.getuid() and path.stat().st_size <= 4096,
                'invalid owner release file')
        ack = read_json(path)
        expected = dict(action='release_quarantine', scope='rfc263-cpu-quarantine', token=token,
                        supervisor_pid=os.getpid(), pgid=pgid, population=run_name(args), evidence=str(args.evidence.resolve()))
        require(all(ack.get(k) == v for k, v in expected.items()) and
                isinstance(ack.get('owner_ack'), str) and bool(ack['owner_ack'].strip()), 'owner release identity mismatch')
    except Exception:
        return False
    try:
        return group_gone(pgid)
    except Exception:
        return False


def quarantine(args, child, reason, release_file, token):
    # This is an owner-authorized emergency exception to the five-minute admin
    # cap. No hash, seal or new work is allowed; only the release file is polled.
    receipt_errors = []
    try:
        terminal_failure(args, 'QUARANTINE: ' + reason)
    except Exception as exc:
        receipt_errors.append(repr(exc))
    try:
        members = group_members(child.pid)
        census_error = None
    except BaseException as exc:
        members, census_error = [], repr(exc)
    started = dt.datetime.now(dt.timezone.utc)
    receipt = dict(scope='rfc263-cpu-quarantine', unsealed=True, started_utc=started.isoformat(),
        administrative_deadline_utc=(started + dt.timedelta(seconds=FORENSIC_LIMIT)).isoformat(), reason=reason,
        supervisor_pid=os.getpid(), pgid=child.pid, population=run_name(args), evidence=str(args.evidence.resolve()),
        token=token, release_file=str(release_file), members=members, census_error=census_error,
        state='QUARANTINE_OWNER_RELEASE_REQUIRED', passive_hold_may_exceed_five_minutes=True, receipt_errors=receipt_errors)
    try:
        save(args.evidence / run_name(args) / 'quarantine-unsealed.json', receipt)
    except Exception as exc:
        receipt_errors.append(repr(exc))
    try:
        print('QUARANTINE_OWNER_RELEASE_REQUIRED', json.dumps(receipt, sort_keys=True), flush=True)
    except Exception:
        pass  # A broken output stream must not release the lane lock.
    while True:
        try:
            approved = release_file.exists() and release_ack(release_file, args, token, child.pid)
        except OSError:
            approved = False
        if approved:
            try:
                print('QUARANTINE_OWNER_RELEASED', run_name(args), utc(), 'NO_AUTHORITATIVE_SEAL', flush=True)
            except OSError:
                pass
            return
        time.sleep(5)


def supervise(args):
    seconds = deadline_seconds(args.deadline_utc)
    stop_at = time.monotonic() + seconds
    forecast = DIAGNOSTIC_FORECAST if getattr(args, 'population', 'timing') == 'diagnostics' else timing_forecast(getattr(args, 'timing_design', 'a1'))
    require(seconds >= forecast + 10, 'planning forecast does not fit authorized deadline')
    require(not (args.evidence / run_name(args)).exists(), 'no replacement/resume')
    frozen = frozen_inputs(args)
    release_file = (getattr(args, 'quarantine_release_file', None) or (args.evidence / (run_name(args) + '-owner-release.json'))).resolve()
    require(not release_file.exists(), 'owner release file must not preexist this slot')
    token = secrets.token_hex(16)
    with Path(frozen['lock']).open('a') as lock:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        enable_subreaper()
        env = dict(os.environ, RFC263_CPU_DEADLINE=str(stop_at - RESERVE),
                   RFC263_CPU_LOCK_FD=str(lock.fileno()), RFC263_CPU_SUPERVISOR_PID=str(os.getpid()))
        child, failure, code, quarantined = None, None, 1, False
        tail_clock, sampling_stopped_utc = None, None
        previous_term = signal.signal(signal.SIGTERM, stop_requested)
        previous_int = signal.getsignal(signal.SIGINT)
        try:
            child = subprocess.Popen([sys.executable, str(Path(__file__).resolve()), *sys.argv[1:], '--worker'],
                                     env=env, process_group=0, pass_fds=(lock.fileno(),))
            code = child.wait(timeout=max(0, stop_at - time.monotonic() - RESERVE))
            if code != 0:
                failure = 'worker exited with status ' + str(code)
        except (subprocess.TimeoutExpired, KeyboardInterrupt) as exc:
            failure, code = repr(exc), 124
        except BaseException as exc:
            failure, code = repr(exc), 1
        finally:
            signal.signal(signal.SIGTERM, signal.SIG_IGN)
            signal.signal(signal.SIGINT, signal.SIG_IGN)
            try:
                if child is not None:
                    try:
                        stop_process_group(child, stop_at)
                        tail_clock, sampling_stopped_utc = time.monotonic(), utc()
                    except BaseException as exc:
                        failure = (failure or '') + '; process-group cleanup: ' + repr(exc)
                        quarantine(args, child, failure, release_file, token)
                        code, quarantined = 125, True
                if not quarantined and tail_clock is None:
                    tail_clock, sampling_stopped_utc = time.monotonic(), utc()
                if not quarantined and failure is None:
                    try:
                        directory = args.evidence / run_name(args)
                        status = read_json(directory / 'status.json')
                        rows = read_json(args.evidence / ('schedule-diagnostics.json' if args.epoch == 0 else f'schedule-epoch{args.epoch}.json'))
                        if args.epoch == 0:
                            require(status['complete'] is True and status['input_hashes_verified_after'] is True and len(rows) == 16 and
                                    status['completed'] == [r['id'] for r in rows], 'incomplete diagnostic worker exit')
                        else:
                            check_status(status, rows, timing_epoch_count(timing_design(frozen)))
                    except BaseException as exc:
                        failure, code = 'invalid terminal worker status: ' + repr(exc), 1
                if not quarantined:
                    forensic_time(tail_clock + FORENSIC_LIMIT)
                    if failure is not None:
                        terminal_failure(args, failure)
                    if not forensic_seal(args, sampling_stopped_utc, tail_clock):
                        code = code or 126
            finally:
                signal.signal(signal.SIGTERM, previous_term)
                signal.signal(signal.SIGINT, previous_int)
    raise SystemExit(code)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('command', choices=('prepare', 'run', 'diagnostics', 'self-check'))
    parser.add_argument('--evidence', type=Path)
    parser.add_argument('--source-root', type=Path)
    parser.add_argument('--helper-dir', type=Path)
    for name in ('baseline-binary', 'production-binary', 'harness', 'cluster', 'lock', 'provenance', 'lifecycle-helper', 'pprof-binary', 'baseline-source-archive', 'plan-file', 'prior-evidence', 'retained-results', 'retained-index', 'confirmation-results'):
        parser.add_argument('--' + name, type=Path)
    for name in ('production-tree', 'production-sha256', 'cluster-sha256', 'primary-sha256', 'tail-sha256', 'confirm-sha256', 'lifecycle-sha256', 'pprof-sha256', 'baseline-source-sha256', 'plan-sha256', 'frozen-sha256', 'deadline-utc', 'authorization', 'prefix-namespace', 'prior-frozen-sha256', 'retained-results-sha256', 'retained-index-sha256', 'confirmation-results-sha256'):
        parser.add_argument('--' + name)
    parser.add_argument('--timing-design', choices=('a1', a2.DESIGN), default='a1')
    parser.add_argument('--epoch', type=int, choices=(1, 2))
    parser.add_argument('--quarantine-release-file', type=Path)
    parser.add_argument('--worker', action='store_true', help=argparse.SUPPRESS)
    args = parser.parse_args()
    if args.command == 'self-check':
        tests = load_module(HERE / 'cpu_guard_test.py', 'cpu_guard_tests')
        tests.run_tests()
        return
    require(args.evidence is not None, '--evidence required')
    args.evidence = args.evidence.resolve()
    if args.command == 'prepare':
        # Path environment variables are optional conveniences, never import prerequisites.
        for key, env in (('source_root', 'RFC263_SOURCE_ROOT'), ('helper_dir', 'RFC263_HELPER_DIR'),
                         ('cluster', 'RFC263_CLUSTER'), ('lock', 'RFC263_LOCK')):
            if getattr(args, key) is None and os.environ.get(env):
                setattr(args, key, Path(os.environ[env]))
        for key in ('source_root', 'helper_dir', 'baseline_binary', 'production_binary', 'harness', 'cluster', 'lock',
                    'provenance', 'production_tree', 'production_sha256', 'cluster_sha256', 'primary_sha256', 'tail_sha256', 'confirm_sha256',
                    'lifecycle_helper', 'lifecycle_sha256', 'pprof_binary', 'pprof_sha256', 'baseline_source_archive', 'baseline_source_sha256',
                    'plan_file', 'plan_sha256', 'prefix_namespace', 'prior_evidence', 'prior_frozen_sha256'):
            require(getattr(args, key) is not None, 'prepare requires --' + key.replace('_', '-'))
        if args.timing_design == a2.DESIGN:
            for key in ('retained_results', 'retained_index', 'retained_results_sha256', 'retained_index_sha256'):
                require(getattr(args, key) is not None, 'A2 prepare requires --' + key.replace('_', '-'))
        prepare(args)
        return
    args.population = 'diagnostics' if args.command == 'diagnostics' else 'timing'
    if args.population == 'diagnostics':
        args.epoch = 0
    require((args.epoch in (0, 1, 2)) and args.deadline_utc and args.authorization and args.frozen_sha256,
            'run requires epoch/deadline/owner-authorization/frozen-sha256')
    frozen = frozen_inputs(args)
    args.timing_design = timing_design(frozen)
    if args.population == 'diagnostics' and args.timing_design == a2.DESIGN:
        require(args.confirmation_results is not None and args.confirmation_results_sha256 is not None,
                'A2 diagnostics require --confirmation-results and --confirmation-results-sha256')
    require(durable(args.evidence, Path(frozen['source_root'])), 'evidence path not durable/off-tree')
    if args.worker:
        require_supervised_worker(frozen)
        signal.signal(signal.SIGTERM, stop_requested)
        seconds = deadline_seconds(args.deadline_utc)
        args.deadline = float(os.environ['RFC263_CPU_DEADLINE'])
        require(math.isfinite(args.deadline) and time.monotonic() < args.deadline <= time.monotonic() + seconds,
                'worker deadline missing/extended beyond owner absolute deadline')
        collect(args)
    else:
        supervise(args)


if __name__ == '__main__':
    main()
