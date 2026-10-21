#!/usr/bin/env python3
"""Analyze only both sealed A2 grv-w8 epochs; retain14 A1 bounds without pooling.

The176 fresh processes contain80 AB and8 AA pairs. Each target endpoint uses
40 ABpairs/epoch. The seven retained A1 cells still have12 ABpairs/cell.
"""
import argparse
from collections import defaultdict
from copy import deepcopy
import fcntl
import importlib.metadata
import math
import os
from pathlib import Path
import statistics
import sys

sys.dont_write_bytecode = True
import cpu_collect as c
import cpu_analyze as a
import cpu_a2_spec as spec


def combine_reports(retained, target):
    c.require(set(target) == set(a.ENDPOINTS), 'both A2 target endpoints required')
    combined = deepcopy(retained['primary_and_coprimary'])
    for metric in a.ENDPOINTS:
        c.require(target[metric]['epoch_counts'] == [40, 40] and all(type(n) is int for n in target[metric]['epoch_counts']) and target[metric]['confidence'] == .95,
                  'A2 target must use40 ABpairs in each epoch and95%CI')
        combined[metric][spec.TARGET] = deepcopy(target[metric])
    decision = a.decision(combined)
    decision['verdict'] = decision['verdict'].upper()
    decision['park'] = decision['verdict'] == 'UNRESOLVED'
    decision['scope'] = '14 retained A1 bounds +2 new A2 grv-w8 bounds; no pooled observations'
    return combined, decision


def extract(evidence, row, frozen):
    out = evidence / f'epoch{row["epoch"]}' / 'runs' / row['id']
    manifest, summary = c.read_json(out / 'manifest.json'), c.read_json(out / 'summary.json')
    c.check_manifest(manifest, row, out, frozen)
    c.check_summary(summary, row, manifest)
    c.validate_lifecycle(summary, frozen, evidence)
    c.csv_integrity(out / 'samples.csv', row['iterations'], row['workers'], 'measure')
    c.csv_integrity(out / 'warmup.csv', row['warmup'], row['workers'], 'warmup')
    return dict(row, **c.endpoint(summary), retries=summary['retries'], warmup_retries=summary['warmup_retries'])


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
    c.require(c.timing_design(frozen) == spec.DESIGN and frozen['seed'] == spec.SEED, 'A2 timing design/seed required')
    c.verify_inputs(e, frozen)
    rows = c.read_json(e / 'schedule.json')
    c.validate_timing_schedule(rows, frozen['identities'], spec.DESIGN)
    c.require(rows == c.timing_schedule(frozen['identities'], frozen['prefix_namespace'], spec.DESIGN), 'noncanonical A2 schedule')
    # No fresh CPU estimate is extracted until BOTH complete88-process seals pass.
    for epoch in (1, 2):
        a.checked_epoch(e, epoch, getattr(args, f'seal{epoch}'), [r for r in rows if r['epoch'] == epoch], expected_count=88)
    retained = c.retained_a1(e, frozen)
    c.helper_chain(e / 'inputs', frozen['helper_hashes'])
    runs = [extract(e, row, frozen) for row in rows]
    c.require(len(runs) == 176, 'full176 fresh endpoint population required')
    grouped = defaultdict(list)
    for row in runs:
        grouped[(row['epoch'], row['contrast'], row['pair'])].append(row)
    c.require(len(grouped) == 88, 'full88 fresh pair population required')
    pairs = []
    for (epoch, contrast, pair), members in grouped.items():
        pairs.append(dict(epoch=epoch, contrast=contrast, pair=pair, order=members[0]['order'],
            logs={metric: a.pair_log(members, contrast, metric) for metric in a.ENDPOINTS}, members=[r['id'] for r in members]))
    c.require(sum(p['contrast'] == 'AB' for p in pairs) == 80 and sum(p['contrast'] != 'AB' for p in pairs) == 8,
              'A2 requires80 AB+8 AA pairs')
    target, aa = {}, {}
    for metric in a.ENDPOINTS:
        ab = [p for p in pairs if p['contrast'] == 'AB']
        values = [[p['logs'][metric] for p in ab if p['epoch'] == epoch] for epoch in (1, 2)]
        report = a.interval(values, n=40)
        report['epoch_ratios'] = [math.exp(x) for x in report['epoch_mean_logs']]
        report['order_ratios_diagnostic'] = {str(order): math.exp(statistics.mean(p['logs'][metric] for p in ab if p['order'] == order)) for order in (0, 1)}
        target[metric] = report
        aa[metric] = {}
        for contrast in ('baselineAA', 'productionAA'):
            selected = [p for p in pairs if p['contrast'] == contrast]
            epoch_values = [[p['logs'][metric] for p in selected if p['epoch'] == epoch] for epoch in (1, 2)]
            c.require(len(selected) == 4 and all(len(v) == 2 for v in epoch_values), 'A2 AA role needs2 pairs/epoch,4 overall')
            aa[metric][contrast] = dict(pairs=4, epoch_pair_counts=[2, 2], interval=None,
                epoch_geometric_mean_ratios=[math.exp(statistics.mean(v)) for v in epoch_values],
                geometric_mean_ratio=math.exp(statistics.mean(p['logs'][metric] for p in selected)),
                reason='descriptive AA only; no CI and no hard gate')
    combined, decision = combine_reports(retained, target)
    allocations = {}
    for role in ('baseline', 'production'):
        selected = [r for r in runs if r['contrast'] == 'AB' and r['binary'] == role]
        c.require(len(selected) == 80, 'A2 allocation context requires80 AB runs per arm')
        allocations[role] = {key: statistics.mean(statistics.mean(r[key] for r in selected if r['epoch'] == epoch) for epoch in (1, 2))
            for key in ('allocations_per_tx', 'allocated_bytes_per_tx', 'gc_cycles', 'retries', 'warmup_retries')}
    result = dict(timing_design=spec.DESIGN, seed=spec.SEED, frozen_sha256=args.frozen_sha256,
        epoch_seals={'1': args.seal1, '2': args.seal2}, processes=176, pairs=88, fresh_ab_pairs=80, fresh_aa_pairs=8,
        measured_transactions=3520000, warmup_transactions=352000, identities=frozen['identities'], target_cell=spec.TARGET,
        target_ab_pairs=80, target_ab_pairs_per_epoch=[40, 40], aa_pairs_per_role=4, aa_diagnostics=aa,
        target_primary_and_coprimary=target, primary_and_coprimary=combined, decision=decision,
        target_decision=dict(verdict=decision['verdict'], park=decision['park'], rule='both target upper95%<=1.02, with any point>=1.02 FAIL precedence'),
        bounds_origin={cell: ('A2 fresh;80 ABpairs' if cell == spec.TARGET else 'A1 retained;12 ABpairs') for cell in sorted(a.CELL_NAMES)},
        ab_pairs_by_cell={cell: (80 if cell == spec.TARGET else 12) for cell in sorted(a.CELL_NAMES)},
        retained_a1=dict(frozen_sha256=spec.A1_FROZEN_SHA256, results_sha256=spec.RESULTS_SHA256, index_sha256=spec.INDEX_SHA256,
                         bounds=14, cells=7, identities=retained['identities']),
        superseded_a1_target_context_not_pooled={metric: retained['primary_and_coprimary'][metric][spec.TARGET] for metric in a.ENDPOINTS},
        allocation_context=allocations, future_diagnostics=dict(frozen_processes=16, pooled=False, collected_by_this_analysis=False),
        endpoint_definitions={
            'window_cpu_us_per_tx': '(stats_after−stats_before).(process_user_cpu_us+process_system_cpu_us)/completed; harness B→C',
            'lifecycle_cpu_us_per_tx': '(lifecycle.stats_after_shutdown−stats_after_identity).(process_user_cpu_us+process_system_cpu_us)/completed; harness E−A'},
        caveats=['A2 grv-w8 supersedes both A1 grv-w8 bounds; no A1 observation enters the fresh estimate.',
                 'The combined matrix has heterogeneous populations:80 ABpairs for grv-w8,12 for each retained A1 cell.',
                 'Both target endpoints must pass; any target point>=1.02 takes precedence. Otherwise both upper95%<=1.02 pass; else UNRESOLVED and park.',
                 'Intervals are marginal two-sided95%, not a simultaneous coverage or power guarantee.',
                 'No extensions, interim estimates, exclusions, resume or replacements; AA remains diagnostic only.',
                 'Overall CPU NI PASS does not clear queue-wait, correctness, throughput, tail latency or merge gates.'])
    c.verify_inputs(e, frozen)
    for epoch in (1, 2):
        a.checked_epoch(e, epoch, getattr(args, f'seal{epoch}'), [r for r in rows if r['epoch'] == epoch], expected_count=88)
    output = args.output.resolve()
    c.require(c.durable(output, Path(frozen['source_root'])) and not output.is_relative_to(e), 'analysis output must be durable/off-tree and outside collection evidence')
    output.mkdir(parents=True, exist_ok=False)
    c.save(output / 'results.json', result)
    c.save(output / 'run-rows.json', runs)
    c.save(output / 'pair-rows.json', pairs)
    c.save(output / 'analysis-inputs.json', dict(frozen_sha256=args.frozen_sha256, epoch_seals=[args.seal1, args.seal2],
        retained_results_sha256=spec.RESULTS_SHA256, retained_index_sha256=spec.INDEX_SHA256,
        collector_sha256=c.sha(Path(c.__file__)), analyzer_sha256=c.sha(Path(__file__)), spec_sha256=c.sha(Path(spec.__file__)),
        python=sys.version, scipy_version=importlib.metadata.version('scipy')))
    c.save(output / 'artifact-sha256.json', {p.name: c.sha(p) for p in sorted(output.iterdir()) if p.is_file()})
    print('CPU_A2_TARGET_AND_OVERALL_NI_GATE', decision['verdict'], 'PARK' if decision['park'] else '')
    for metric, report in target.items():
        print(metric, spec.TARGET, 'point', report['point_ratio'], 'CI95', report['lower_ratio'], report['upper_ratio'])


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--self-check', action='store_true')
    parser.add_argument('--evidence', type=Path)
    parser.add_argument('--output', type=Path)
    for name in ('frozen-sha256', 'seal1', 'seal2'):
        parser.add_argument('--' + name)
    args = parser.parse_args()
    if args.self_check:
        c.load_module(Path(__file__).with_name('cpu_a2_guard_test.py'), 'cpu_a2_guard_tests').run_tests()
        return
    c.require(all(getattr(args, name) is not None for name in ('evidence', 'output', 'frozen_sha256', 'seal1', 'seal2')),
              'A2 requires complete frozen/sealed evidence and output')
    analyze(args)


if __name__ == '__main__':
    main()
