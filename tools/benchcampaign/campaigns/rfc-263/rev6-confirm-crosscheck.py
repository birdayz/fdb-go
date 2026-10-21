#!/usr/bin/env python3
"""Independent RFC263 p999 crosscheck; writes only a new external review directory."""
import argparse
import csv
import hashlib
import io
import json
import math
from pathlib import Path
import shutil
import statistics
import sys

import scipy
from scipy.stats import t

N = 20000
RANK = math.floor((N - 1) * 0.999)
PREREG = '7bf58d7bf2bba737d8087d261b19f0e30bc274b5'
SCHEDULE_SHA = '26641bf7a50514e051c08c62859091df971f99b25ef8b91cd70e68045f327fe3'
SOURCES = {'master': 'fa8725f1dcf6ff3ac66825ab6e3e819efbc0abcc',
           'production': 'c2e52f46ab02df0295f272f421b87c3bdb3a3515'}
BINS = {'master': '2dd943fdc9d58ff136449a181189ba5843465e86e6ae696d616a9b1bac4a439e',
        'production': 'ff3b447e7449a1e3dc38d04da15e4fdec15cc4e0840fa808e95fb6544b5f37de'}


def require(ok, message):
    if not ok:
        raise RuntimeError(message)


def digest(data):
    return hashlib.sha256(data).hexdigest()


def load(path):
    return json.loads(path.read_text())


def order_statistic(values):
    require(len(values) == N and all(v > 0 for v in values), 'invalid percentile population')
    return sorted(values)[RANK]


def paired_log(rows):
    require(len(rows) == 2, 'pair needs two members')
    kind = rows[0]['contrast']
    aliases = {r['alias']: r for r in rows}
    numerator, denominator = ('P', 'M') if kind == 'AB' else ('B', 'A')
    require(set(aliases) == {numerator, denominator}, 'pair aliases')
    a, b = aliases[numerator], aliases[denominator]
    return math.log(a['p999_ns'] / b['p999_ns'])


def welch(epochs, confidence):
    require(len(epochs) == 2 and all(len(e) >= 2 for e in epochs), 'two nonvacuous epochs required')
    means = [statistics.mean(e) for e in epochs]
    variances = [statistics.variance(e) for e in epochs]
    components = [v / (4 * len(e)) for v, e in zip(variances, epochs)]
    variance = sum(components)
    require(variance > 0, 'degenerate interval variance')
    df = variance ** 2 / sum(c ** 2 / (len(e) - 1) for c, e in zip(components, epochs))
    mean = sum(means) / 2
    critical = float(t.ppf((1 + confidence) / 2, df))
    half_width = critical * math.sqrt(variance)
    combined = epochs[0] + epochs[1]
    return {'n': len(combined), 'n_per_epoch': [len(e) for e in epochs],
            'confidence': confidence, 'mean_log_ratio': mean, 'ratio': math.exp(mean),
            'log_ci': [mean - half_width, mean + half_width],
            'ratio_ci': [math.exp(mean - half_width), math.exp(mean + half_width)],
            'se': math.sqrt(variance), 'welch_df': df, 't_critical': critical,
            'sd_all_pairs': statistics.stdev(combined),
            'within_epoch_sd': math.sqrt(sum(variances) / 2),
            'epoch_mean_log_ratio': means, 'epoch_ratios': [math.exp(m) for m in means],
            'epoch_sd': [math.sqrt(v) for v in variances],
            'exceeds_0_15_planning_sd': statistics.stdev(combined) > 0.15}


def self_check():
    require(RANK == 19979 and order_statistic(list(range(N, 0, -1))) == 19980, 'rank positive control')
    for bad in ([], [1] * (N - 1), [0] + [1] * (N - 1)):
        try:
            order_statistic(bad)
        except RuntimeError:
            pass
        else:
            raise RuntimeError('invalid percentile population accepted')
    for kind, aliases in [('AB', ('P', 'M')), ('masterAA', ('B', 'A'))]:
        rows = [{'contrast': kind, 'alias': aliases[0], 'p999_ns': 110},
                {'contrast': kind, 'alias': aliases[1], 'p999_ns': 100}]
        require(paired_log(rows) == paired_log(rows[::-1]) == math.log(1.1), 'alias orientation control')
    got = welch([[-1, 0, 1], [-2, 0, 2]], .975)
    require(math.isclose(got['se'] ** 2, 5 / 12) and math.isclose(got['welch_df'], 50 / 17), 'Welch variance/df control')
    require(math.isclose(welch([[0, 1, 2], [10, 10, 10, 10]], .975)['mean_log_ratio'], 5.5), 'equal epoch weighting control')
    require(welch([[-1, 0, 1], [-2, 0, 2]], .9875)['t_critical'] > got['t_critical'], 'AA confidence control')
    print('SELF_CHECK_OK rank/population/orientation/equal-epoch/Welch/confidence', flush=True)


def analyze(evidence, output):
    require(scipy.__version__ == '1.16.3', 'SciPy must be pinned to1.16.3')
    evidence = evidence.resolve()
    output = output.resolve()
    require(not output.is_relative_to(evidence), 'review output must be outside closed evidence')
    require(not output.exists(), 'refuse replacing a review')
    frozen = load(evidence / 'frozen.json')
    require(frozen['preregistration'] == PREREG and frozen['sources'] == SOURCES and frozen['binaries'] == BINS,
            'frozen identity mismatch')
    schedule_bytes = (evidence / 'schedule.json').read_bytes()
    require(digest(schedule_bytes) == SCHEDULE_SHA, 'schedule hash mismatch')
    schedule = json.loads(schedule_bytes)
    require(len(schedule) == 1152 and len({r['id'] for r in schedule}) == 1152, 'full1152 schedule required')
    require(len({r['prefix'] for r in schedule}) == 1152, 'unique prefixes required')
    output.mkdir(parents=True)
    source = Path(__file__).resolve()
    shutil.copyfile(source, output / source.name)
    run_results = []
    pairs = {}
    total_rows = 0
    for epoch in (1, 2):
        rows = [r for r in schedule if r['epoch'] == epoch]
        status = load(evidence / f'epoch{epoch}/status.json')
        require(len(rows) == 576 and status['complete'] is True and status['input_hashes_verified_after'] is True and
                status['completed'] == [r['id'] for r in rows] and 'failure' not in status, 'incomplete epoch')
        runs = evidence / f'epoch{epoch}/runs'
        require({p.name for p in runs.iterdir() if p.is_dir()} == {r['id'] for r in rows}, 'output population mismatch')
        for r in rows:
            require(r['source'] == SOURCES[r['binary']] and r['binary_sha256'] == BINS[r['binary']], 'run identity mismatch')
            out = runs / r['id']
            command = load(runs / (r['id'] + '.command.json'))
            require(command['schedule'] == r and command['returncode'] == 0 and
                    0 < command['duration_upper_ns'] < 10_000_000_000 and command['accepted_load'] <= 7.5,
                    'command/admission failure')
            manifest = load(out / 'manifest.json')
            config, backend = manifest['config'], manifest['backend']
            require(config['profile'] == 'none' and config['iterations'] == N and config['workers'] == 8 and
                    config['warmup'] == 2000 and config['source_revision'] == r['source'] and
                    config['workload'] == r['workload'] and config['prefix'] == r['prefix'] and
                    backend['executable_sha256'] == r['binary_sha256'] and backend['backend'] == 'go' and
                    backend['native_library'] is False and manifest['go_version'] == 'go1.26.9', 'manifest mismatch')
            data = (out / 'samples.csv').read_bytes()
            csv_sha = digest(data)
            hashes = load(runs / (r['id'] + '.sha256.json'))
            require(hashes[f"runs/{r['id']}/samples.csv"] == csv_sha, 'raw CSV seal mismatch')
            seen = bytearray(N)
            latencies = []
            for sample in csv.DictReader(io.StringIO(data.decode())):
                index = int(sample['index'])
                require(0 <= index < N and not seen[index], 'duplicate/out-of-range sample')
                seen[index] = 1
                require(int(sample['worker']) == index % 8 and sample['phase'] == 'measure' and
                        sample['error'] == '' and sample['check_status'] == 'ok' and
                        int(sample['attempts']) > 0, 'sample health mismatch')
                latencies.append(int(sample['latency_ns']))
            require(len(latencies) == N and all(seen), 'missing sample')
            total_rows += len(latencies)
            result = dict(r, p999_ns=order_statistic(latencies), csv_sha256=csv_sha)
            run_results.append(result)
            pairs.setdefault((epoch, r['workload'], r['contrast'], r['pair']), []).append((result, config))
        print(f'RAW_EPOCH_OK epoch={epoch} processes=576 samples={576 * N}', flush=True)
    populations = {}
    for workload in ('get100', 'set100'):
        for kind in ('AB', 'masterAA', 'productionAA'):
            records, epochs = [], []
            for epoch in (1, 2):
                logs = []
                selected = [(key, pair) for key, pair in pairs.items() if key[:3] == (epoch, workload, kind)]
                require(len(selected) == 48, 'need48 pairs per population/epoch')
                require(sum(pair[0][0]['order'] == 0 for _, pair in selected) == 24, 'order imbalance')
                for key, pair in selected:
                    require(len(pair) == 2 and [r['member'] for r, c in pair] == [0, 1], 'pair membership')
                    rs = [r for r, c in pair]
                    expected = ['master', 'production'] if kind == 'AB' else ['master' if kind == 'masterAA' else 'production'] * 2
                    if rs[0]['order']:
                        expected.reverse()
                    require([r['binary'] for r in rs] == expected, 'pair role mismatch')
                    if kind != 'AB':
                        configs = [{k: v for k, v in c.items() if k not in ('run_id', 'prefix', 'output')} for r, c in pair]
                        require(configs[0] == configs[1], 'AA configuration mismatch')
                    value = paired_log(rs)
                    logs.append(value)
                    records.append({'epoch': epoch, 'pair': key[3], 'order': rs[0]['order'],
                                    'run_ids': [r['id'] for r in rs], 'aliases': [r['alias'] for r in rs],
                                    'p999_ns_in_execution_order': [r['p999_ns'] for r in rs],
                                    'log_ratio': value, 'ratio': math.exp(value)})
                epochs.append(logs)
            result = welch(epochs, .975 if kind == 'AB' else .9875)
            result['pairs'] = records
            if kind == 'AB':
                result['engineering_decision'] = ('regression/design-must-change' if result['mean_log_ratio'] >= math.log(1.05)
                                                  else 'unresolved' if result['log_ci'][1] >= math.log(1.05) else 'clears-upper-bound')
            else:
                result['aa_mean_interval_contains_one'] = result['log_ci'][0] <= 0 <= result['log_ci'][1]
            populations[f'{workload}/{kind}'] = result
    aa_pass = all(v['aa_mean_interval_contains_one'] for k, v in populations.items() if not k.endswith('/AB'))
    primary_pass = all(populations[f'{w}/AB']['log_ci'][1] < math.log(1.05) for w in ('get100', 'set100'))
    report = {'method': 'raw per-process lower p999; P/M treatment and B/A controls; equal epoch means; within-epoch Welch-Satterthwaite t intervals',
              'preregistration': PREREG, 'schedule_sha256': SCHEDULE_SHA,
              'source_sha256': digest(source.read_bytes()), 'source_archive': source.name,
              'python': sys.version, 'scipy': scipy.__version__, 'process_count': len(run_results),
              'measured_row_count': total_rows, 'pair_count': len(pairs), 'p999_zero_based_rank': RANK,
              'aa_gate_pass': aa_pass, 'both_primary_upper_bounds_below_1_05': primary_pass,
              'p999_blocker_clears': aa_pass and primary_pass, 'populations': populations, 'runs': run_results}
    require(len(run_results) == 1152 and total_rows == 23_040_000 and len(pairs) == 576, 'final population guard')
    destination = output / 'independent-review.json'
    destination.write_text(json.dumps(report, indent=2, sort_keys=True, allow_nan=False) + '\n')
    print('REVIEW_JSON', destination, 'SHA256', digest(destination.read_bytes()), flush=True)
    for key, result in populations.items():
        print(key, json.dumps({k: v for k, v in result.items() if k != 'pairs'}, sort_keys=True), flush=True)
    print('AA_GATE_PASS', aa_pass, 'PRIMARY_UPPER_BOUNDS_PASS', primary_pass, 'BLOCKER_CLEARS', aa_pass and primary_pass, flush=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--self-check', action='store_true')
    parser.add_argument('--evidence', type=Path)
    parser.add_argument('--output-dir', type=Path)
    args = parser.parse_args()
    self_check()
    if not args.self_check:
        require(args.evidence is not None and args.output_dir is not None, 'evidence/output-dir required')
        analyze(args.evidence, args.output_dir)


if __name__ == '__main__':
    main()
