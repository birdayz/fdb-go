#!/usr/bin/env python3
"""Analyze only the complete, sealed two-epoch RFC263 confirmation population.

Primary: equal-weight epoch means of paired log p999 ratios, Welch-Satterthwaite
97.5% two-sided intervals. A/A means use 98.75% intervals. No exploratory sample
enters these estimates. Bootstrap and other endpoints are sensitivity/context only.
"""
from __future__ import annotations

import argparse
from collections import Counter, defaultdict
from concurrent.futures import ProcessPoolExecutor
import csv
import datetime as dt
import hashlib
import json
import math
import os
from pathlib import Path
import shutil
import sys
import unittest

import numpy as np
import scipy
from scipy import stats

CPUS = {0, 1, 6, 7, 11, 12, 13, 18, 19, 23}
SEED = 263006
BOOTSTRAPS = 20000
SCHEDULE_SHA = '26641bf7a50514e051c08c62859091df971f99b25ef8b91cd70e68045f327fe3'
SEALS = {1: 'a6d8c9165f656d8046708395180303676a25a84605225b2f9bf6dfa4f31334db',
         2: 'df0d17dd6be72df72d050e8e8b2b740d47ecaabc2ec28ffc318e1e753d95fd9b'}
BINS = {'master': '2dd943fdc9d58ff136449a181189ba5843465e86e6ae696d616a9b1bac4a439e',
        'production': 'ff3b447e7449a1e3dc38d04da15e4fdec15cc4e0840fa808e95fb6544b5f37de'}


def require(value, message):
    if not value:
        raise ValueError(message)


def sha(path):
    h = hashlib.sha256()
    with Path(path).open('rb') as stream:
        for part in iter(lambda: stream.read(1024 * 1024), b''):
            h.update(part)
    return h.hexdigest()


def save(path, value):
    Path(path).write_text(json.dumps(value, indent=2, sort_keys=True, allow_nan=False) + '\n')


def checked_json(path, digest):
    raw = path.read_bytes()
    require(hashlib.sha256(raw).hexdigest() == digest, 'changed sealed JSON: ' + str(path))
    return json.loads(raw)


def order_index(n, probability):
    require(isinstance(n, int) and not isinstance(n, bool) and n > 0 and 0 <= probability <= 1,
            'invalid quantile population/probability')
    return math.floor((n - 1) * probability)


def interval(epoch_values, confidence):
    require(len(epoch_values) == 2 and 0 < confidence < 1, 'two epochs required')
    values = [np.asarray(v, dtype=float) for v in epoch_values]
    require(all(v.ndim == 1 and len(v) >= 2 and np.isfinite(v).all() for v in values),
            'empty/invalid paired population')
    means = [math.fsum(v) / len(v) for v in values]
    variances = [float(np.var(v, ddof=1)) for v in values]
    terms = [v / len(x) for v, x in zip(variances, values)]
    mean = math.fsum(means) / 2
    se = math.sqrt(math.fsum(terms)) / 2
    if se == 0:
        df, critical = None, float(stats.norm.ppf((1 + confidence) / 2))
    else:
        df = math.fsum(terms) ** 2 / math.fsum(t * t / (len(v) - 1) for t, v in zip(terms, values))
        critical = float(stats.t.ppf((1 + confidence) / 2, df))
    return {'mean_log': mean, 'point_ratio': math.exp(mean), 'se_log': se, 'df': df,
            'confidence': confidence, 'critical': critical,
            'lower_ratio': math.exp(mean - critical * se), 'upper_ratio': math.exp(mean + critical * se),
            'epoch_mean_logs': means, 'epoch_variances_log': variances,
            'epoch_counts': [len(v) for v in values]}


def pair_log(rows, contrast):
    require(len(rows) == 2, 'pair must have exactly two runs')
    if contrast == 'AB':
        key = 'binary'
        numerator, denominator = 'production', 'master'
    else:
        require(contrast in ('masterAA', 'productionAA'), 'unknown contrast')
        key = 'alias'
        numerator, denominator = 'B', 'A'
        role = 'master' if contrast == 'masterAA' else 'production'
        require(all(r['binary'] == role for r in rows), 'A/A wrong binary role')
    require({r[key] for r in rows} == {numerator, denominator}, 'duplicate/missing pair member')
    a = next(r for r in rows if r[key] == numerator)
    b = next(r for r in rows if r[key] == denominator)
    require(a['p999_ns'] > 0 and b['p999_ns'] > 0, 'invalid paired percentile')
    return math.log(a['p999_ns'] / b['p999_ns'])


def decision(primary, controls_ok):
    require(set(primary) == {'get100', 'set100'}, 'both primary workloads required')
    if any(v['point_ratio'] >= 1.05 for v in primary.values()):
        point_rule = 'regression_design_must_change'
    elif all(v['upper_ratio'] < 1.05 for v in primary.values()):
        point_rule = 'p999_non_inferiority_gate_satisfied'
    else:
        point_rule = 'unresolved'
    return {'recorded_point_upper_rule': point_rule, 'a_a_controls_pass': controls_ok,
            'qualified_decision': point_rule if controls_ok else 'control_gate_failed_no_clearance'}


class Guards(unittest.TestCase):
    def test_quantile_rank_and_vacuity(self):
        self.assertEqual(order_index(20000, .999), 19979)
        self.assertEqual(order_index(5, .5), 2)
        for n in (0, -1, True):
            with self.assertRaises(ValueError): order_index(n, .999)

    def test_welch_values_and_empty(self):
        result = interval([[-.1, 0, .1], [-.2, 0, .2]], .975)
        self.assertAlmostEqual(result['mean_log'], 0)
        self.assertAlmostEqual(result['se_log'], math.sqrt(1 / 240))
        self.assertAlmostEqual(result['df'], 50 / 17)
        for values in ([], [[], []], [[0], [0]], [[math.nan, 0], [0, 0]]):
            with self.assertRaises(ValueError): interval(values, .975)

    def test_library_quantile_positive_controls(self):
        self.assertAlmostEqual(float(stats.t.ppf(.975, 10)), 2.2281388519649385, places=10)
        self.assertAlmostEqual(float(stats.norm.ppf(.975)), 1.959963984540054, places=10)

    def test_constant_intervals(self):
        result = interval([[0] * 48, [0] * 48], .975)
        self.assertEqual((result['point_ratio'], result['lower_ratio'], result['upper_ratio']), (1, 1, 1))
        self.assertIsNone(result['df'])

    def test_pairing_orientation(self):
        rows = [{'binary': 'production', 'alias': 'P', 'p999_ns': 200},
                {'binary': 'master', 'alias': 'M', 'p999_ns': 100}]
        self.assertAlmostEqual(pair_log(rows, 'AB'), math.log(2))
        self.assertEqual(pair_log(rows, 'AB'), pair_log(rows[::-1], 'AB'))
        aa = [{'binary': 'master', 'alias': 'B', 'p999_ns': 105},
              {'binary': 'master', 'alias': 'A', 'p999_ns': 100}]
        self.assertAlmostEqual(pair_log(aa, 'masterAA'), math.log(1.05))
        for invalid in ([], rows[:1], [rows[0], rows[0]]):
            with self.assertRaises(ValueError): pair_log(invalid, 'AB')
        with self.assertRaises(ValueError): pair_log(aa, 'productionAA')

    def test_exact_owner_boundaries_and_controls(self):
        passing = {'point_ratio': 1.0, 'upper_ratio': 1.049999}
        self.assertEqual(decision({'get100': passing, 'set100': passing}, True)['qualified_decision'],
                         'p999_non_inferiority_gate_satisfied')
        self.assertEqual(decision({'get100': passing, 'set100': dict(passing, upper_ratio=1.05)}, True)
                         ['qualified_decision'], 'unresolved')
        self.assertEqual(decision({'get100': passing, 'set100': {'point_ratio': 1.05, 'upper_ratio': 1.1}}, True)
                         ['qualified_decision'], 'regression_design_must_change')
        self.assertEqual(decision({'get100': passing, 'set100': passing}, False)['qualified_decision'],
                         'control_gate_failed_no_clearance')
        with self.assertRaises(ValueError): decision({}, True)


def extract(payload):
    evidence, output, row, index = payload
    epoch = Path(evidence) / ('epoch' + str(row['epoch']))
    run = epoch / 'runs' / row['id']
    def read(name):
        rel = 'runs/' + row['id'] + '/' + name
        return checked_json(run / name, index[rel])
    manifest, summary = read('manifest.json'), read('summary.json')
    require(summary['success'] is True and summary['completed'] == 20000 and summary['errors'] == 0,
            'unhealthy native summary')
    cfg = manifest['config']
    require(cfg['run_id'] == row['id'] and cfg['workload'] == row['workload'] and cfg['profile'] == 'none'
            and cfg['iterations'] == 20000 and cfg['workers'] == 8, 'configuration mismatch')
    require(manifest['backend']['executable_sha256'] == BINS[row['binary']], 'binary mismatch')
    path = run / 'samples.csv'
    digest = hashlib.sha256()
    values, seen = [], set()
    with path.open('rb') as stream:
        def lines():
            for raw in stream:
                digest.update(raw)
                yield raw.decode('utf-8')
        reader = csv.DictReader(lines())
        require(reader.fieldnames and len(reader.fieldnames) == len(set(reader.fieldnames)), 'bad CSV header')
        for sample in reader:
            i = int(sample['index'])
            require(0 <= i < 20000 and i not in seen and int(sample['worker']) == i % 8,
                    'CSV population/worker mismatch')
            require(sample['phase'] == 'measure' and not sample['error'] and sample['check_status'] == 'ok',
                    'CSV health failure')
            ns = int(sample['latency_ns'])
            require(ns > 0, 'invalid latency')
            values.append(ns)
            seen.add(i)
    require(len(values) == 20000 and digest.hexdigest() == index['runs/' + row['id'] + '/samples.csv'],
            'incomplete/changed CSV')
    data = np.asarray(values, dtype=np.int64)
    ranks = [order_index(len(values), p) for p in (.5, .99, .999)]
    selected = np.partition(data, ranks)
    bins, counts = np.unique(data // 1000, return_counts=True)
    histogram = Path(output) / 'histograms' / (row['id'] + '.json')
    save(histogram, {'count': 20000, 'bin_width_ns': 1000, 'bounds': 'lower inclusive; no overflow bucket',
                     'bins': [[int(b) * 1000, int(c)] for b, c in zip(bins, counts)]})
    before, after = summary['stats_before'], summary['stats_after']
    cpu = ((after['process_user_cpu_us'] - before['process_user_cpu_us']) +
           (after['process_system_cpu_us'] - before['process_system_cpu_us'])) / 20000
    return dict(row, p50_ns=int(selected[ranks[0]]), p99_ns=int(selected[ranks[1]]),
                p999_ns=int(selected[ranks[2]]), mean_ns=math.fsum(values) / 20000,
                cpu_us=cpu, allocs=(after['go_mallocs'] - before['go_mallocs']) / 20000,
                allocated_bytes=(after['go_total_alloc_bytes'] - before['go_total_alloc_bytes']) / 20000,
                gc_cycles=after['go_gc_cycles'] - before['go_gc_cycles'],
                samples_sha256=digest.hexdigest(), histogram_sha256=sha(histogram))


def diagnostics(values):
    all_values = np.concatenate(values)
    n = len(all_values)
    sd = float(np.std(all_values, ddof=1))
    lower = sd * math.sqrt((n - 1) / float(stats.chi2.ppf(.975, n - 1)))
    upper = sd * math.sqrt((n - 1) / float(stats.chi2.ppf(.025, n - 1)))
    absolute = np.abs(np.expm1(all_values)) * 100
    return {'pooled_sd_log': sd, 'pooled_sd_log_normal_model_ci95': [lower, upper],
            'epoch_sd_log': [float(np.std(v, ddof=1)) for v in values],
            'epoch_lag1_correlation': [float(np.corrcoef(v[:-1], v[1:])[0, 1]) if np.std(v) else None
                                       for v in values],
            'absolute_paired_change_percent': {str(p): float(np.quantile(absolute, p, method='lower'))
                                               for p in (.5, .9, .95, .99, 1.0)}}


def plot(path, reports):
    panels = []
    width, panel_height = 1000, 180
    for k, (label, report) in enumerate(reports.items()):
        values = [100 * math.expm1(r['log_ratio']) for r in report['pairs']]
        low, high = min(-5, min(values)), max(5, max(values))
        pad = max(1, (high - low) * .05)
        low, high = low - pad, high + pad
        y0 = 30 + k * panel_height
        def x(i): return 65 + i * 880 / 95
        def y(v): return y0 + 130 - (v - low) * 110 / (high - low)
        points = ' '.join(f'{x(i):.2f},{y(v):.2f}' for i, v in enumerate(values))
        panels += [f'<text x="65" y="{y0}">{label}: paired p999 change %; epoch boundary dashed</text>',
                   f'<line x1="65" x2="945" y1="{y(0):.2f}" y2="{y(0):.2f}" stroke="#888"/>',
                   f'<line x1="{x(47.5):.2f}" x2="{x(47.5):.2f}" y1="{y0+10}" y2="{y0+140}" stroke="#888" stroke-dasharray="4"/>',
                   f'<polyline points="{points}" fill="none" stroke="#1263a5"/>',
                   f'<text x="5" y="{y0+30}">{high:.1f}</text><text x="5" y="{y0+130}">{low:.1f}</text>']
    path.write_text(f'<svg xmlns="http://www.w3.org/2000/svg" width="{width}" height="{panel_height*len(reports)+30}" font-family="sans-serif" font-size="12">' + ''.join(panels) + '</svg>\n')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--evidence', required=True, type=Path)
    parser.add_argument('--output', required=True, type=Path)
    parser.add_argument('--self-check', action='store_true')
    args = parser.parse_args()
    os.sched_setaffinity(0, CPUS)
    os.setpriority(os.PRIO_PROCESS, 0, 19)
    args.output.mkdir(parents=True, exist_ok=False)
    with (args.output / 'guard-tests.log').open('w') as log:
        result = unittest.TextTestRunner(stream=log, verbosity=2).run(unittest.defaultTestLoader.loadTestsFromTestCase(Guards))
    require(result.wasSuccessful() and result.testsRun == 6, 'nonempty statistical guards failed')
    if args.self_check:
        print('STATISTICAL_GUARDS_OK 6 groups')
        return
    root = args.evidence.resolve()
    require(sha(root / 'schedule.json') == SCHEDULE_SHA, 'changed preregistered schedule')
    schedule = json.loads((root / 'schedule.json').read_text())
    require(len(schedule) == 1152 and len({r['id'] for r in schedule}) == 1152, 'full fixed population required')
    indexes = {}
    for e in (1, 2):
        epoch = root / ('epoch' + str(e))
        indexes[e] = checked_json(epoch / 'artifact-sha256.json', SEALS[e])
        health = checked_json(epoch / 'status.json', indexes[e]['status.json'])
        require(health['complete'] and health['input_hashes_verified_after'] and len(health['completed']) == 576,
                'both complete integrity-qualified epochs required')
        require(health['completed'] == [r['id'] for r in schedule if r['epoch'] == e], 'completion order mismatch')
    metadata = {'analysis_started_utc': dt.datetime.now(dt.timezone.utc).isoformat(), 'script_sha256': sha(Path(__file__)),
                'python': sys.version, 'numpy': np.__version__, 'scipy': scipy.__version__, 'schedule_sha256': SCHEDULE_SHA,
                'epoch_seals': SEALS, 'bootstrap_replicates': BOOTSTRAPS, 'bootstrap_seed': SEED,
                'bootstrap_method': 'percentile; resample whole paired superblocks within each epoch, shared across workloads',
                'primary': 'Welch-Satterthwaite on equal-weight epoch means of paired log ratios; two-sided97.5%',
                'aa': 'same epoch-stratified method; two-sided98.75%; SD CI95 uses normal model',
                'threshold': 1.05, 'other_endpoints': 'context only; never select primary method by their results'}
    save(args.output / 'analysis-config.json', metadata)
    shutil.copy2(Path(__file__), args.output / 'analyzer-source.py')
    (args.output / 'histograms').mkdir()
    with ProcessPoolExecutor(max_workers=4) as pool:
        runs = list(pool.map(extract, [(str(root), str(args.output), r, indexes[r['epoch']]) for r in schedule]))
    save(args.output / 'run-rows.json', runs)
    grouped = defaultdict(list)
    for r in runs:
        grouped[(r['epoch'], r['workload'], r['contrast'], r['pair'])].append(r)
    require(len(grouped) == 576 and all(len(v) == 2 for v in grouped.values()), 'exact paired population required')
    pair_rows = []
    for (epoch, workload, contrast, pair), rows in grouped.items():
        d = pair_log(rows, contrast)
        pair_rows.append({'epoch': epoch, 'workload': workload, 'contrast': contrast, 'pair': pair,
                          'group': rows[0]['group'], 'superblock': rows[0]['superblock'],
                          'order': rows[0]['order'], 'log_ratio': d, 'ratio': math.exp(d),
                          'members': [{'id': r['id'], 'alias': r['alias'], 'binary': r['binary'], 'p999_ns': r['p999_ns']} for r in rows]})
    save(args.output / 'pair-rows.json', pair_rows)
    reports, primary, controls = {}, {}, {}
    for kind_index, contrast in enumerate(('AB', 'masterAA', 'productionAA')):
        rng = np.random.default_rng(SEED + kind_index)
        bootstrap_indices = [rng.integers(0, 48, size=(BOOTSTRAPS, 48)) for _ in (1, 2)]
        for workload in ('get100', 'set100'):
            pairs = sorted([r for r in pair_rows if r['contrast'] == contrast and r['workload'] == workload],
                           key=lambda r: (r['epoch'], r['group'], r['superblock']))
            require(len(pairs) == 96, '96 pairs per workload/contrast required')
            arrays = [np.asarray([r['log_ratio'] for r in pairs if r['epoch'] == e]) for e in (1, 2)]
            require(all(len(v) == 48 for v in arrays), '48 pairs per epoch required')
            confidence = .975 if contrast == 'AB' else .9875
            report = interval(arrays, confidence)
            report.update(diagnostics(arrays))
            boot = (arrays[0][bootstrap_indices[0]].mean(axis=1) + arrays[1][bootstrap_indices[1]].mean(axis=1)) / 2
            alpha = (1 - confidence) / 2
            report['bootstrap_percentile_ratio_interval_sensitivity_only'] = np.exp(np.quantile(boot, [alpha, 1-alpha])).tolist()
            report['pairs'] = pairs
            report['order_geomean_ratios_context'] = {str(order): math.exp(math.fsum(r['log_ratio'] for r in pairs if r['order'] == order) / 48) for order in (0, 1)}
            report['epoch_geomean_ratios'] = [math.exp(x) for x in report['epoch_mean_logs']]
            reports[workload + ':' + contrast] = report
            if contrast == 'AB': primary[workload] = report
            else: controls[workload + ':' + contrast] = report
    controls_ok = all(v['lower_ratio'] <= 1 <= v['upper_ratio'] for v in controls.values())
    result = {'analysis_config': metadata, 'primary': primary, 'aa_controls': controls,
              'decision': decision(primary, controls_ok), 'runs': 1152, 'pairs': 576,
              'caveats': ['80% planning power is per-workload at true zero, not joint80%',
                          'A/A non-significance is not proof of zero bias/equivalence',
                          'bootstrap/order/temporal/other endpoints do not replace the primary interval',
                          'passing p999 bounds does not replace other owner/full-suite/review gates']}
    save(args.output / 'results.json', result)
    plot(args.output / 'paired-order.svg', reports)
    context = {}
    for workload in ('get100', 'set100'):
        context[workload] = {}
        for binary in ('master', 'production'):
            selected = [r for r in runs if r['workload'] == workload and r['contrast'] == 'AB' and r['binary'] == binary]
            context[workload][binary] = {metric: float(np.median([r[metric] for r in selected]))
                                       for metric in ('cpu_us', 'mean_ns', 'p50_ns', 'p99_ns', 'p999_ns', 'allocs', 'allocated_bytes', 'gc_cycles')}
    save(args.output / 'context-not-primary.json', context)
    save(args.output / 'artifact-sha256.json', {str(p.relative_to(args.output)): sha(p) for p in sorted(args.output.rglob('*'))
                                               if p.is_file() and p.name != 'artifact-sha256.json'})
    print('PREREGISTERED_DECISION', result['decision'])
    for name, r in primary.items():
        print('PRIMARY', name, 'point', r['point_ratio'], 'CI97.5', r['lower_ratio'], r['upper_ratio'], 'SD', r['pooled_sd_log'])
    for name, r in controls.items():
        print('AA', name, 'point', r['point_ratio'], 'CI98.75', r['lower_ratio'], r['upper_ratio'], 'SD', r['pooled_sd_log'])


if __name__ == '__main__':
    main()
