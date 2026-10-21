"""Fixed A2 grv-w8 confirmation design and independently pinned A1 retention.

This module is import-pure. It neither runs a workload nor reads a path until a
caller explicitly supplies it. A1's schedule and validator remain separate.
"""
from collections import Counter
import hashlib
import json
import math
from pathlib import Path
import random
import re

DESIGN = 'a2-grv-w8'
SEED = 263107
EPOCH_PROCESSES = 88
PROCESSES = 176
AB_PER_EPOCH = 40
FORECAST = 15 * 60
TARGET = 'grv-w8'
BASELINE = 'fa8725f1dcf6ff3ac66825ab6e3e819efbc0abcc'
BASELINE_BIN = '2dd943fdc9d58ff136449a181189ba5843465e86e6ae696d616a9b1bac4a439e'
PRODUCTION_BIN = '02d86dcde2f70868c116aeed225c7ef0d8a626da9b8501fb0f06a2e668c41949'
RESULTS_SHA256 = '6c700352c97e190fbc1cadf7d0a597fa6eb3f16a10bf7fb06144c9a82377deeb'
INDEX_SHA256 = '6c4cd85cd82589cc11dcc6d11721d5511b0d649a59e0b26331a8cf1b0964d8c5'
A1_FROZEN_SHA256 = '154a783499a59a5e26e3bdffbf19631b99cd2a41b67261c34556eb187493dd62'
ENDPOINTS = ('window_cpu_us_per_tx', 'lifecycle_cpu_us_per_tx')
CELLS = {f'{workload}-w{workers}' for workload in ('grv', 'get100', 'set100', 'range100') for workers in (8, 32)}


def require(value, message):
    if not value:
        raise ValueError(message)


def validate_identities(ids):
    require(set(ids) == {'baseline', 'production'}, 'A2 requires both binary identities')
    require(ids['baseline'] == {'source': BASELINE, 'binary_sha256': BASELINE_BIN}, 'A2 baseline identity changed')
    require(ids['production']['binary_sha256'] == PRODUCTION_BIN and
            re.fullmatch('tree:[0-9a-f]{40}', ids['production']['source']), 'A2 production binary/tree identity changed')


def namespace_prefix(namespace, prefix):
    require(isinstance(namespace, str) and re.fullmatch('[0-9a-f]{32}', namespace), 'A2 requires a new32-hex namespace')
    return 'clientbench/rfc263/' + hashlib.sha256((namespace + '\0' + prefix).encode()).hexdigest()[:48] + '/'


def schedule(ids, prefix_namespace):
    validate_identities(ids)
    rng = random.Random(SEED)
    runs = []
    for epoch in (1, 2):
        contrasts = [('AB', 0)] * 20 + [('AB', 1)] * 20 + [
            ('baselineAA', 0), ('baselineAA', 1), ('productionAA', 0), ('productionAA', 1)]
        rng.shuffle(contrasts)
        for visit, (contrast, order) in enumerate(contrasts):
            roles = [('A', 'baseline'), ('B', 'production')] if contrast == 'AB' else [
                ('A', 'baseline' if contrast == 'baselineAA' else 'production'),
                ('B', 'baseline' if contrast == 'baselineAA' else 'production')]
            if order:
                roles.reverse()
            for member, (alias, binary) in enumerate(roles):
                raw_prefix = ('clientbench/rfc263/' + f'{rng.getrandbits(128):032x}').ljust(67, '0') + '/'
                runs.append(dict(id=f'e{epoch}-r{len(runs) % EPOCH_PROCESSES:03d}', epoch=epoch, visit=visit,
                    position=0, block=(epoch - 1) * 44 + visit, cell=TARGET, workload='grv', workers=8,
                    iterations=20000, warmup=2000, pair=f'e{epoch}-v{visit:02d}-{TARGET}', contrast=contrast,
                    order=order, member=member, alias=alias, binary=binary,
                    prefix=namespace_prefix(prefix_namespace, raw_prefix), profile='none', **ids[binary]))
    validate_schedule(runs, ids)
    return runs


def validate_schedule(runs, ids):
    validate_identities(ids)
    require(isinstance(runs, list) and len(runs) == 176, 'A2 requires exactly176 timing processes')
    require([r['id'] for r in runs] == [f'e{epoch}-r{i:03d}' for epoch in (1, 2) for i in range(88)], 'A2 run order/IDs changed')
    require(len({r['prefix'] for r in runs}) == 176 and all(
        isinstance(r['prefix'], str) and re.fullmatch('clientbench/rfc263/[0-9a-f]{48}/', r['prefix']) for r in runs), 'A2 prefix population invalid')
    require(len({r['pair'] for r in runs}) == 88, 'A2 pair census changed')
    for epoch in (1, 2):
        rows = [r for r in runs if r['epoch'] == epoch]
        require(len(rows) == 88, 'A2 requires exactly88 processes per epoch')
        require([(r['visit'], r['member']) for r in rows] == [(visit, member) for visit in range(44) for member in (0, 1)],
                'A2 pair adjacency/visit order changed')
        pairs = [rows[i:i + 2] for i in range(0, 88, 2)]
        require(Counter(p[0]['contrast'] for p in pairs) == {'AB': 40, 'baselineAA': 2, 'productionAA': 2}, 'A2 contrast census changed')
        for contrast, counts in (('AB', {0: 20, 1: 20}), ('baselineAA', {0: 1, 1: 1}), ('productionAA', {0: 1, 1: 1})):
            require(Counter(p[0]['order'] for p in pairs if p[0]['contrast'] == contrast) == counts, 'A2 execution-order balance changed')
        for pair in pairs:
            first = pair[0]
            roles = [('A', 'baseline'), ('B', 'production')] if first['contrast'] == 'AB' else [
                ('A', 'baseline' if first['contrast'] == 'baselineAA' else 'production'),
                ('B', 'baseline' if first['contrast'] == 'baselineAA' else 'production')]
            if first['order']:
                roles.reverse()
            require([(r['alias'], r['binary']) for r in pair] == roles, 'A2 pair roles changed')
            for r in pair:
                require(all(type(r[k]) is int for k in ('epoch', 'visit', 'position', 'block', 'workers', 'iterations', 'warmup', 'order', 'member')),
                        'A2 integer schedule field invalid')
                require(all(r[k] == first[k] for k in ('epoch', 'visit', 'position', 'block', 'cell', 'workload', 'workers',
                            'iterations', 'warmup', 'pair', 'contrast', 'order')), 'A2 pair metadata mismatch')
                require(r['cell'] == TARGET and r['workload'] == 'grv' and r['workers'] == 8 and r['iterations'] == 20000 and
                        r['warmup'] == 2000 and r['profile'] == 'none' and r['position'] == 0 and
                        r['block'] == (epoch - 1) * 44 + r['visit'] and r['pair'] == f'e{epoch}-v{r["visit"]:02d}-{TARGET}', 'A2 workload/config changed')
                require(all(r[k] == ids[r['binary']][k] for k in ('source', 'binary_sha256')), 'A2 source/binary mapping changed')
    require(sum(r['iterations'] for r in runs) == 3520000 and sum(r['warmup'] for r in runs) == 352000, 'A2 transaction census changed')


def validate_prefix_population(timing, diagnostics, prior_populations=()):
    require(len(timing) == 176 and len(diagnostics) == 16, 'A2 requires176 timing+16 diagnostic prefixes')
    current = [r['prefix'] for r in timing + diagnostics]
    require(len(set(current)) == 192 and all(len(p.encode()) == 68 for p in current), 'A2 requires192 unique68-byte prefixes')
    for prior in prior_populations:
        require(len(prior) == 272 and len({r['prefix'] for r in prior}) == 272, 'prior A1/invalid prefix population incomplete')
        require(set(current).isdisjoint(r['prefix'] for r in prior), 'A2 prefix reused from A1 or original invalid population')


def checked_json(path, expected):
    require(isinstance(expected, str) and re.fullmatch('[0-9a-f]{64}', expected), 'independent full SHA256 pin required')
    raw = Path(path).read_bytes()
    require(hashlib.sha256(raw).hexdigest() == expected, 'pinned JSON drift: ' + str(path))
    return json.loads(raw)


def validate_retained(result, index, ids, prior_ids=None):
    validate_identities(ids)
    require(isinstance(index, dict) and bool(index) and index.get('results.json') == RESULTS_SHA256, 'A1 result/index binding missing')
    require(result['seed'] == 263007 and result['processes'] == 256 and result['pairs'] == 128 and
            result['ab_pairs_per_cell'] == 12 and result['aa_pairs_per_cell'] == 4 and
            result['measured_transactions'] == 66560000 and result['warmup_transactions'] == 512000, 'retained A1 population changed')
    old_ids = result['identities']
    validate_identities(old_ids)
    require(all(old_ids[role]['binary_sha256'] == ids[role]['binary_sha256'] for role in ids), 'A1/A2 binary mismatch')
    if prior_ids is not None:
        require(old_ids == prior_ids, 'retained A1 identities do not match the pinned prior batch')
    reports = result['primary_and_coprimary']
    require(set(reports) == set(ENDPOINTS) and all(set(reports[metric]) == CELLS for metric in ENDPOINTS), 'retained A1 bound population incomplete')
    retained = 0
    for metric in ENDPOINTS:
        for cell in CELLS:
            row = reports[metric][cell]
            require(all(type(row[k]) in (int, float) and math.isfinite(row[k]) and row[k] > 0 for k in ('point_ratio', 'lower_ratio', 'upper_ratio')) and
                    row['lower_ratio'] <= row['point_ratio'] <= row['upper_ratio'] and row['confidence'] == .95 and row['epoch_counts'] == [6, 6],
                    'invalid retained A1 interval')
            if cell != TARGET:
                require(row['point_ratio'] < 1.02 and row['upper_ratio'] <= 1.02, 'one of14 retained A1 bounds is not NI')
                retained += 1
    require(retained == 14, 'exactly14 A1 bounds must be retained')
    return result


def retained_results(results_path, index_path, results_sha256, index_sha256, ids, prior_ids=None):
    require(results_sha256 == RESULTS_SHA256 and index_sha256 == INDEX_SHA256, 'A2 retained A1 pins differ from preregistration')
    result = checked_json(results_path, RESULTS_SHA256)
    index = checked_json(index_path, INDEX_SHA256)
    return validate_retained(result, index, ids, prior_ids)
