#!/usr/bin/env python3
"""Analyze only a complete sealed RFC263 CPU NI population; never launch workloads.

Co-primary gates: measured-window (harness B→C) AND post-identity lifecycle
(harness E−A) process CPU/tx. Equal-epoch paired log means, Welch two-sided 95% CI.
A/A and allocations are diagnostic, never hard gates. No interim mode exists.
"""
import argparse
from collections import defaultdict
import fcntl
import importlib.metadata
import math
import os
from pathlib import Path
import statistics
import sys

sys.dont_write_bytecode = True
import cpu_collect as c

ENDPOINTS = ('window_cpu_us_per_tx', 'lifecycle_cpu_us_per_tx')
CELL_NAMES = {f'{workload}-w{workers}' for workload, workers in c.CELLS}


def interval(values, n=6):
    c.require(type(n) is int and n in (6, 40), 'only preregistered A1 n6 or A2 n40 supported')
    c.require(len(values) == 2 and all(len(v) == n for v in values), f'{n} ABpairs in each of two epochs required')
    c.require(all(type(x) in (int, float) and math.isfinite(x) for v in values for x in v), 'nonfinite paired log')
    means = [math.fsum(v) / n for v in values]
    variances = [statistics.variance(v) for v in values]
    terms = [v / n for v in variances]
    mean = math.fsum(means) / 2
    se = math.sqrt(math.fsum(terms)) / 2
    if se == 0:
        df, critical = None, 1.959963984540054
    else:
        from scipy import stats
        df = math.fsum(terms) ** 2 / math.fsum(t * t / (n - 1) for t in terms)
        critical = float(stats.t.ppf(.975, df))
    result = dict(mean_log=mean, point_ratio=math.exp(mean), lower_ratio=math.exp(mean - critical * se),
                  upper_ratio=math.exp(mean + critical * se), confidence=.95, se_log=se, df=df, critical=critical,
                  epoch_mean_logs=means, epoch_variances_log=variances, epoch_counts=[n, n])
    c.require(all(math.isfinite(result[k]) and result[k] > 0 for k in ('point_ratio', 'lower_ratio', 'upper_ratio')), 'invalid interval')
    return result


def pair_log(rows, contrast, metric):
    c.require(contrast in c.CONTRASTS and metric in ENDPOINTS, 'unknown contrast/endpoint')
    c.require(len(rows) == 2 and {r['alias'] for r in rows} == {'A', 'B'}, 'missing/duplicate pair members')
    c.require(all(r['contrast'] == contrast for r in rows), 'pair contrast mismatch')
    by_alias = {r['alias']: r for r in rows}
    roles = ('baseline', 'production') if contrast == 'AB' else (
        ('baseline', 'baseline') if contrast == 'baselineAA' else ('production', 'production'))
    c.require((by_alias['A']['binary'], by_alias['B']['binary']) == roles, 'wrong pair source role')
    a, b = by_alias['A'][metric], by_alias['B'][metric]
    c.require(type(a) in (int, float) and type(b) in (int, float) and math.isfinite(a) and math.isfinite(b) and a > 0 and b > 0,
              'nonpositive/nonfinite paired CPU endpoint')
    return math.log(b) - math.log(a)


def decision(reports):
    c.require(set(reports) == set(ENDPOINTS) and all(set(reports[k]) == CELL_NAMES for k in ENDPOINTS),
              'both endpoints and all eight cells required')
    all_results = [reports[metric][cell] for metric in ENDPOINTS for cell in sorted(CELL_NAMES)]
    for row in all_results:
        c.require(all(type(row[k]) in (int, float) and math.isfinite(row[k]) and row[k] > 0 for k in
                  ('point_ratio', 'lower_ratio', 'upper_ratio')) and row['lower_ratio'] <= row['point_ratio'] <= row['upper_ratio'], 'invalid decision interval')
    if any(row['point_ratio'] >= 1.02 for row in all_results):
        verdict = 'FAIL'
    elif all(row['upper_ratio'] <= 1.02 for row in all_results):
        verdict = 'PASS'
    else:
        verdict = 'unresolved'
    return dict(verdict=verdict, rule='FAIL if any point>=1.02; else PASS iff all16 upper95%<=1.02; else unresolved',
                improvement={metric: {cell: reports[metric][cell]['upper_ratio'] < 1.00 for cell in sorted(CELL_NAMES)} for metric in ENDPOINTS},
                aa_diagnostic_only=True)


def checked_epoch(evidence, epoch, seal, rows, expected_count=128):
    directory = evidence / f'epoch{epoch}'
    c.require(c.sha(directory / 'artifact-sha256.json') == c.digest(seal), 'epoch seal changed')
    index = c.read_json(directory / 'artifact-sha256.json')
    c.require(bool(index) and 'status.json' in index, 'empty/missing artifact census')
    actual = {str(p.relative_to(directory)) for p in directory.rglob('*') if p.is_file() and p.name != 'artifact-sha256.json'}
    c.require(actual == set(index), 'extra/missing sealed artifacts')
    for rel, expected in index.items():
        path = directory / rel
        c.require(not Path(rel).is_absolute() and '..' not in Path(rel).parts and not path.is_symlink(), 'unsafe artifact path')
        c.require(c.sha(path) == c.digest(expected), 'sealed artifact changed: ' + rel)
    status = c.read_json(directory / 'status.json')
    c.require(status['epoch'] == epoch, 'wrong epoch status')
    c.check_status(status, rows, expected_count)
    for row in rows:
        run = directory / 'runs' / row['id']
        for name in ('summary.json', 'manifest.json', 'samples.csv', 'warmup.csv'):
            c.require(str((run / name).relative_to(directory)) in index, 'missing required run artifact')
        receipt = c.read_json(directory / 'runs' / (row['id'] + '.command.json'))
        c.require(receipt['schedule'] == row and receipt['returncode'] == 0 and receipt['env'] == c.ENV and
                  0 < receipt['timeout_seconds'] <= c.CHILD_LIMIT and receipt['duration_ns'] > 0, 'command receipt mismatch')
    return index


def analyze(args):
    c.require(os.getpriority(os.PRIO_PROCESS, 0) == 19 and set(os.sched_getaffinity(0)) == c.CPUS,
              'analysis requires nice19 and reserved controller affinity')
    e = args.evidence.resolve()
    c.require(c.sha(e / 'frozen.json') == c.digest(args.frozen_sha256), 'frozen manifest changed')
    frozen = c.read_json(e / 'frozen.json')
    with Path(frozen['lock']).open('a') as lock:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        analyze_locked(args)


def analyze_locked(args):
    e = args.evidence.resolve()
    c.require(c.sha(e / 'frozen.json') == c.digest(args.frozen_sha256), 'frozen manifest changed')
    frozen = c.read_json(e / 'frozen.json')
    c.require(frozen['seed'] == c.SEED, 'seed mismatch')
    c.verify_inputs(e, frozen)
    rows = c.read_json(e / 'schedule.json')
    c.validate_schedule(rows, frozen['identities'])
    c.require(rows == c.schedule(frozen['identities'], frozen['prefix_namespace']), 'noncanonical frozen schedule')
    # Verify BOTH complete populations before extracting or reporting any endpoint.
    for epoch in (1, 2):
        checked_epoch(e, epoch, getattr(args, f'seal{epoch}'), [r for r in rows if r['epoch'] == epoch])
    c.helper_chain(e / 'inputs', frozen['helper_hashes'])
    runs = []
    for row in rows:
        out = e / f'epoch{row["epoch"]}' / 'runs' / row['id']
        manifest, summary = c.read_json(out / 'manifest.json'), c.read_json(out / 'summary.json')
        c.check_manifest(manifest, row, out, frozen)
        c.check_summary(summary, row, manifest)
        c.validate_lifecycle(summary, frozen, e)
        c.csv_integrity(out / 'samples.csv', row['iterations'], row['workers'], 'measure')
        c.csv_integrity(out / 'warmup.csv', row['warmup'], row['workers'], 'warmup')
        runs.append(dict(row, **c.endpoint(summary), retries=summary['retries'], warmup_retries=summary['warmup_retries']))
    c.require(len(runs) == 256, 'full256 endpoint population required')
    grouped = defaultdict(list)
    for row in runs:
        grouped[(row['epoch'], row['cell'], row['contrast'], row['pair'])].append(row)
    c.require(len(grouped) == 128, 'full128 pair population required')
    pairs = []
    for (epoch, cell, contrast, pair), members in grouped.items():
        pairs.append(dict(epoch=epoch, cell=cell, contrast=contrast, pair=pair, order=members[0]['order'],
            logs={metric: pair_log(members, contrast, metric) for metric in ENDPOINTS}, members=[r['id'] for r in members]))
    reports, diagnostics = {metric: {} for metric in ENDPOINTS}, {metric: {} for metric in ENDPOINTS}
    for metric in ENDPOINTS:
        for cell in sorted(CELL_NAMES):
            selected = [p for p in pairs if p['cell'] == cell and p['contrast'] == 'AB']
            arrays = [[p['logs'][metric] for p in selected if p['epoch'] == epoch] for epoch in (1, 2)]
            report = interval(arrays)
            report['epoch_ratios'] = [math.exp(x) for x in report['epoch_mean_logs']]
            report['order_ratios_diagnostic'] = {str(order): math.exp(statistics.mean(p['logs'][metric] for p in selected if p['order'] == order)) for order in (0, 1)}
            reports[metric][cell] = report
            diagnostics[metric][cell] = {}
            for kind in ('baselineAA', 'productionAA'):
                aa = [p for p in pairs if p['cell'] == cell and p['contrast'] == kind]
                c.require(len(aa) == 2 and {p['epoch'] for p in aa} == {1, 2}, 'nonempty AA diagnostic population required')
                diagnostics[metric][cell][kind] = dict(epoch_ratios=[math.exp(next(p['logs'][metric] for p in aa if p['epoch'] == epoch)) for epoch in (1, 2)],
                    geometric_mean_ratio=math.exp(statistics.mean(p['logs'][metric] for p in aa)),
                    pairs=2, interval=None, reason='one AA pair per epoch; descriptive only, no hard gate')
    allocations = {}
    for cell in sorted(CELL_NAMES):
        allocations[cell] = {}
        for role in ('baseline', 'production'):
            rr = [r for r in runs if r['cell'] == cell and r['contrast'] == 'AB' and r['binary'] == role]
            c.require(len(rr) == 12, 'allocation population missing')
            allocations[cell][role] = {key: statistics.mean(statistics.mean(r[key] for r in rr if r['epoch'] == epoch) for epoch in (1, 2))
                for key in ('allocations_per_tx', 'allocated_bytes_per_tx', 'gc_cycles', 'retries', 'warmup_retries')}
    result = dict(seed=c.SEED, processes=256, pairs=128, ab_pairs_per_cell=12, aa_pairs_per_cell=4,
        measured_transactions=66560000, warmup_transactions=512000, identities=frozen['identities'],
        endpoint_definitions={
            'window_cpu_us_per_tx': '(stats_after−stats_before).(process_user_cpu_us+process_system_cpu_us)/completed; harness B→C',
            'lifecycle_cpu_us_per_tx': '(lifecycle.stats_after_shutdown−stats_after_identity).(process_user_cpu_us+process_system_cpu_us)/completed; harness E−A'},
        primary_and_coprimary=reports, aa_diagnostics=diagnostics, allocation_context=allocations, decision=decision(reports),
        caveats=['No simultaneous confidence claim: each endpoint/cell interval is two-sided95%; PASS requires all16 bounds.',
                 'Lifecycle includes startup, fixture, warmup, validation, CSV and deferred cleanup/drain work; not steady-state CPU.',
                 'A/A has no hard gate; low AA counts cannot establish equivalence.', 'No replacements, exclusions, retries of epochs or optional stopping.',
                 'PASS is CPU NI evidence only, not correctness, throughput, tail latency or merge authorization.'])
    output = args.output.resolve()
    c.require(c.durable(output, Path(frozen['source_root'])) and not output.is_relative_to(e), 'analysis output must be durable/off-tree and outside collection evidence')
    output.mkdir(parents=True, exist_ok=False)
    c.save(output / 'results.json', result)
    c.save(output / 'run-rows.json', runs)
    c.save(output / 'pair-rows.json', pairs)
    c.save(output / 'analysis-inputs.json', dict(frozen_sha256=args.frozen_sha256, epoch_seals=[args.seal1, args.seal2],
        collector_sha256=c.sha(Path(c.__file__)), analyzer_sha256=c.sha(Path(__file__)), python=sys.version,
        scipy_version=importlib.metadata.version('scipy')))
    c.save(output / 'artifact-sha256.json', {p.name: c.sha(p) for p in sorted(output.iterdir()) if p.is_file()})
    print('CPU_NI_DECISION', result['decision']['verdict'])
    for metric in ENDPOINTS:
        for cell in sorted(CELL_NAMES):
            row = reports[metric][cell]
            print(metric, cell, 'point', row['point_ratio'], 'CI95', row['lower_ratio'], row['upper_ratio'])


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--self-check', action='store_true')
    parser.add_argument('--evidence', type=Path)
    parser.add_argument('--output', type=Path)
    for key in ('frozen-sha256', 'seal1', 'seal2'):
        parser.add_argument('--' + key)
    args = parser.parse_args()
    if args.self_check:
        c.load_module(Path(__file__).with_name('cpu_guard_test.py'), 'cpu_guard_tests').run_tests()
        return
    c.require(all(getattr(args, key) is not None for key in ('evidence', 'output', 'frozen_sha256', 'seal1', 'seal2')), 'complete frozen/sealed evidence and output required')
    analyze(args)


if __name__ == '__main__':
    main()
