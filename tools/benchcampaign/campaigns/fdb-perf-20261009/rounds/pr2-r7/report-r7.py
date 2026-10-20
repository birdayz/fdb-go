#!/usr/bin/env python3
"""R7: paired run-block estimates and descriptive top-tail index clustering.

No operation-level bootstrap. Exactly 10k shared resamples of the 18 complete
three-arm blocks, with a fixed seed, retaining all R6/R7 observations.
"""
import argparse
import csv
import hashlib
import itertools
import json
import math
from pathlib import Path
import random
import statistics

ARMS = ['cgo', 'old', 'new']
METRICS = ['p99_ns', 'p999_ns', 'p90_ns', 'p50_ns', 'process_cpu_ns_per_op',
           'mean_ns', 'user_cpu_ns_per_op', 'system_cpu_ns_per_op']
PAIRS = [('new', 'old'), ('new', 'cgo'), ('old', 'cgo')]
PINS = {
    'old': '4d236cbfc53cea32211b5042a86e0e14dc006f8896a1204b27aee3921fb17e71',
    'new': '276282e9bdd6aa9d5796afd785c57c0ee77a520dd203328f04f42e43ed1b91bf',
    'cgo': 'cd762ea429647784378270613f404573ba598a5049f422e055a887f7746ecbdb',
}
SEED = 787707
REPS = 10000


def sha(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def quantile(values, p):
    assert values and 0 < p <= 1 and all(math.isfinite(x) for x in values)
    return sorted(values)[math.ceil(p * len(values)) - 1]


def gm(values):
    assert values and all(math.isfinite(x) and x > 0 for x in values)
    return math.exp(statistics.mean(math.log(x) for x in values))


def clustered(indices, radius=20):
    assert len(set(indices)) == len(indices)
    return sum(any(0 < abs(i - j) <= radius for j in indices) for i in indices)


def csvwrite(path, rows):
    with path.open('w', newline='') as f:
        writer = csv.DictWriter(f, fieldnames=list(rows[0]))
        writer.writeheader()
        writer.writerows(rows)


def expected_uniform_fraction(n, k, radius):
    # Conditional on one selected index, the other k-1 indices are a uniform
    # subset of the remaining n-1 positions; boundaries have fewer neighbors.
    return statistics.mean(1 - math.prod((n - 1 - min(radius, i) - min(radius, n - 1 - i) - j) /
                                        (n - 1 - j) for j in range(k - 1)) for i in range(n))


def main():
    ap = argparse.ArgumentParser()
    for name in ['r6', 'r7a', 'r7b', 'out']:
        ap.add_argument('--' + name, type=Path, required=True)
    args = ap.parse_args()
    args.out.mkdir(exist_ok=False)
    # Boundary/tie/nearest-rank checks independent of the measured result.
    assert clustered([0, 20, 100, 121]) == 2
    assert clustered([0, 21]) == 0
    assert clustered([0, 20, 40]) == 3
    assert quantile(list(range(1, 2001)), .99) == 1980
    assert quantile(list(range(1, 2001)), .999) == 1998
    assert gm([.5, 2]) == 1
    rows, hashes, tail_details, prefixes = [], {}, [], []
    for campaign, folder in [('R6', args.r6), ('R7a', args.r7a), ('R7b', args.r7b)]:
        plan = json.loads((folder / 'plan.json').read_text())
        schedule = json.loads((folder / 'schedule.json').read_text())
        completed = json.loads((folder / 'complete.json').read_text())
        validation = json.loads((folder / 'counter-resource-validation.json').read_text())
        analysis = json.loads((folder / 'analysis.json').read_text())
        assert analysis['valid_zero_error_campaign']
        assert completed['scheduled'] == completed['completed'] == len(schedule) == 18
        assert plan['blocks'] == 6 and plan['workloads'] == [dict(name='batchset50x1000', iterations=2000, warmup=200, workers=1)]
        assert {tuple(r['arm'] for r in schedule if r['block'] == b) for b in range(6)} == set(itertools.permutations(ARMS))
        assert all(a['all_raw_persistence_identity_and_quiescence_checks_passed'] for a in validation['workloads']['batchset50x1000']['arms'].values())
        for f in ['plan.json', 'schedule.json', 'complete.json', 'analysis.json', 'counter-resource-validation.json', 'binaries.json']:
            hashes[campaign + '/' + f] = sha(folder / f)
        analyzed = {r['run_id']: r for r in analysis['per_run']}
        for run in schedule:
            rid, arm = run['run_id'], run['arm']
            manifest = json.loads((folder / rid / 'manifest.json').read_text())
            summary = json.loads((folder / rid / 'summary.json').read_text())
            assert manifest['backend']['executable_sha256'] == PINS[arm]
            assert manifest['config']['profile'] == 'none' and manifest['gomaxprocs'] == 8
            assert manifest['config']['warmup'] == 200 and manifest['config']['iterations'] == 2000
            prefixes.append(manifest['config']['prefix'])
            with (folder / rid / 'samples.csv').open() as f:
                samples = list(csv.DictReader(f))
            assert len(samples) == 2000 and sorted(int(r['index']) for r in samples) == list(range(2000))
            assert all(r['phase'] == 'measure' and r['check_status'] == 'ok' and not r['error'] and int(r['attempts']) == 1 and int(r['worker']) == 0 for r in samples)
            latencies = [int(r['latency_ns']) for r in samples]
            assert min(latencies) > 0
            before, after = summary['stats_before'], summary['stats_after']
            r = dict(campaign=campaign, block=run['block'], global_block=campaign + '/b%03d' % run['block'],
                     position=run['position'], arm=arm, run_id=rid, observations=2000,
                     mean_ns=statistics.mean(latencies))
            for metric, prob in [('p50_ns', .5), ('p90_ns', .9), ('p99_ns', .99), ('p999_ns', .999)]:
                r[metric] = quantile(latencies, prob)
                if metric != 'p999_ns':
                    assert r[metric] == analyzed[rid][metric]
            for label in ['user', 'system']:
                r[label + '_cpu_ns_per_op'] = (after['process_' + label + '_cpu_us'] - before['process_' + label + '_cpu_us']) * 1000 / 2000
            r['process_cpu_ns_per_op'] = r['user_cpu_ns_per_op'] + r['system_cpu_ns_per_op']
            assert abs(r['process_cpu_ns_per_op'] - analyzed[rid]['process_cpu_ns_per_op']) < .000001
            for k in ['go_alloc_bytes_per_op', 'go_mallocs_per_op', 'go_gc_cycles']:
                r[k] = analyzed[rid][k]
            top = sorted(samples, key=lambda x: (-int(x['latency_ns']), int(x['index'])))[:20]
            indices = sorted(int(x['index']) for x in top)
            r['top_one_percent_samples'] = len(indices)
            r['top_one_percent_neighbor_count'] = clustered(indices)
            r['top_one_percent_neighbor_fraction'] = clustered(indices) / len(indices)
            tail_details.append(dict(campaign=campaign, run_id=rid, arm=arm, indices=indices,
                                     top_samples=[dict(index=int(x['index']), latency_ns=int(x['latency_ns'])) for x in top],
                                     threshold_tie_count=sum(v == int(top[-1]['latency_ns']) for v in latencies),
                                     neighbor_fraction=r['top_one_percent_neighbor_fraction']))
            rows.append(r)
            for f in ['manifest.json', 'summary.json', 'samples.csv', 'warmup.csv', 'result.json', 'quiescence.json']:
                hashes[campaign + '/' + rid + '/' + f] = sha(folder / rid / f)
    assert len(rows) == len(prefixes) == len(set(prefixes)) == 54
    assert {len(p.encode()) for p in prefixes} == {68}
    block_ids = [c + '/b%03d' % b for c in ['R6', 'R7a', 'R7b'] for b in range(6)]
    by = {(r['global_block'], r['arm']): r for r in rows}
    assert len(by) == 54
    rng = random.Random(SEED)
    resamples = [[rng.randrange(18) for _ in range(18)] for _ in range(REPS)]
    comparisons, block_rows, campaign_rows = [], [], []
    for num, den in PAIRS:
        for metric in METRICS:
            ratios = [by[b, num][metric] / by[b, den][metric] for b in block_ids]
            logs = list(map(math.log, ratios))
            boot = [math.exp(sum(logs[i] for i in indices) / 18) for indices in resamples]
            comparisons.append(dict(numerator=num, denominator=den, metric=metric, blocks=18,
                                    paired_geomean_ratio=gm(ratios), ci95_low=quantile(boot, .025), ci95_high=quantile(boot, .975),
                                    pairs_below_one=sum(r < 1 for r in ratios), per_block_ratios=ratios))
            for campaign in ['R6', 'R7a', 'R7b']:
                subset = [v for b, v in zip(block_ids, ratios) if b.startswith(campaign + '/')]
                campaign_rows.append(dict(campaign=campaign, numerator=num, denominator=den, metric=metric,
                                          paired_geomean_ratio=gm(subset), pairs_below_one=sum(v < 1 for v in subset)))
        for b in block_ids:
            block_rows.append(dict(global_block=b, numerator=num, denominator=den,
                                   **{m: by[b, num][m] / by[b, den][m] for m in METRICS}))
    absolute, clustering = {}, {}
    for arm in ARMS:
        rr = [r for r in rows if r['arm'] == arm]
        absolute[arm] = {m: statistics.median(r[m] for r in rr) for m in METRICS}
        clustering[arm] = dict(runs=18, numerator=sum(r['top_one_percent_neighbor_count'] for r in rr), denominator=360,
                               fraction=sum(r['top_one_percent_neighbor_count'] for r in rr) / 360,
                               median_run_fraction=statistics.median(r['top_one_percent_neighbor_fraction'] for r in rr),
                               min_run_fraction=min(r['top_one_percent_neighbor_fraction'] for r in rr),
                               max_run_fraction=max(r['top_one_percent_neighbor_fraction'] for r in rr))
    caveat = ('Nominal 95% percentile bootstrap over all 18 paired run blocks (10,000 replicates, seed 787707); '
              'not an operation bootstrap, not a ratio of pooled quantiles or medians. Same VM and continuously running server: '
              'block independence/CI coverage are not established; independent schedule seeds do not mean independent physical hosts. '
              'No multiplicity correction, prespecified noninferiority margin, or capacity/global-native claim. R6 prompted these two '
              'follow-ups; this is exploratory, not a fresh confirmatory test. Each run has only 2,000 observations, so p99.9 is highly '
              'variable (nearest rank 1998, only two observations above it). CPU is measured-phase process getrusage user+system, '
              'not perf task-clock or whole-process GNU time. Top-1% index clustering alone does not establish periodicity, stall cause, '
              'or that a p99 regression is harmless. Go MemStats excludes native C++ heap; native client internal counters are unobserved.')
    report = dict(bootstrap_seed=SEED, bootstrap_replicates=REPS, inference=caveat,
                  pins=PINS, raw_sha256=hashes, per_run=rows, comparisons=comparisons,
                  per_campaign=campaign_rows, absolute_median_run_ns=absolute, clustering=clustering,
                  uniform_random_top20_neighbor_fraction=expected_uniform_fraction(2000, 20, 20),
                  top_tail_details=tail_details)
    (args.out / 'r7-analysis.json').write_text(json.dumps(report, indent=2) + '\n')
    csvwrite(args.out / 'r7-per-run.csv', rows)
    csvwrite(args.out / 'r7-per-block.csv', block_rows)
    csvwrite(args.out / 'r7-per-campaign.csv', campaign_rows)
    csvwrite(args.out / 'r7-comparisons.csv', [{k: v for k, v in r.items() if k != 'per_block_ratios'} for r in comparisons])
    text = ['# R7: PR2 large-value follow-up\n\nAll 18 blocks: R6 + R7a + R7b; 54 runs, 2,000 measured + 200 warmup per run. '
            'Same exact R6 binaries; 50 distinct 1,000-byte Sets + commit/operation, one worker, no profiles/builds. '
            'Each campaign covers all six three-arm orders, with fresh equal-length prefixes.\n\n',
            '## Absolute medians of 18 run-level statistics (µs/transaction)\n\n',
            '|Arm|p99|p99.9|p90|p50|CPU|Mean|User CPU|System CPU|\n|---|---:|---:|---:|---:|---:|---:|---:|---:|\n']
    for arm in ARMS:
        text.append('|' + arm + '|' + '|'.join(f'{absolute[arm][m]/1000:.3f}' for m in METRICS) + '|\n')
    text.append('\n## Paired geometric-mean ratios and 95% block-bootstrap CIs\n\n|Ratio|Metric|GM|95% CI|Pairs <1|\n|---|---|---:|---|---:|\n')
    for r in comparisons:
        text.append(f'|{r["numerator"]}/{r["denominator"]}|{r["metric"]}|{r["paired_geomean_ratio"]:.6f}|[{r["ci95_low"]:.6f}, {r["ci95_high"]:.6f}]|{r["pairs_below_one"]}/18|\n')
    text.append('\n## Campaign point estimates (all blocks retained)\n\n|Campaign|Ratio|Metric|GM|Pairs <1|\n|---|---|---|---:|---:|\n')
    for r in campaign_rows:
        if r['metric'] not in ['p99_ns', 'process_cpu_ns_per_op']:
            continue
        text.append(f'|{r["campaign"]}|{r["numerator"]}/{r["denominator"]}|{r["metric"]}|{r["paired_geomean_ratio"]:.6f}|{r["pairs_below_one"]}/6|\n')
    text.append('\n## Per-block p99 and CPU ratios\n\n|Block|Ratio|p99|CPU|\n|---|---|---:|---:|\n')
    for r in block_rows:
        text.append(f'|{r["global_block"]}|{r["numerator"]}/{r["denominator"]}|{r["p99_ns"]:.6f}|{r["process_cpu_ns_per_op"]:.6f}|\n')
    text.append('\n## Top-1% clustering\n\nExactly 20 largest measured samples/run (ties broken by ascending operation index). '
                'An observation counts if another distinct top-20 index is within ±20, inclusive; no wraparound. '
                'Indices and latency values are in r7-analysis.json. All 54 per-run fractions follow.\n\n'
                '|Arm|Neighbor samples / top samples|Fraction|Median run fraction|Run range|\n|---|---:|---:|---:|---|\n')
    for arm in ARMS:
        r = clustering[arm]
        text.append(f'|{arm}|{r["numerator"]}/{r["denominator"]}|{r["fraction"]:.4f}|{r["median_run_fraction"]:.4f}|[{r["min_run_fraction"]:.4f}, {r["max_run_fraction"]:.4f}]|\n')
    text.append(f'\nFor context only, a uniformly random 20-position subset of 2,000 has expected fraction {report["uniform_random_top20_neighbor_fraction"]:.6f} under this same rule; this is not a causal model or a significance test.\n\n'
                '|Campaign/block|Arm|p99 µs|p99.9 µs|Top-20 neighbors|Fraction|\n|---|---|---:|---:|---:|---:|\n')
    for r in rows:
        text.append(f'|{r["global_block"]}|{r["arm"]}|{r["p99_ns"]/1000:.3f}|{r["p999_ns"]/1000:.3f}|{r["top_one_percent_neighbor_count"]}/20|{r["top_one_percent_neighbor_fraction"]:.4f}|\n')
    text.append('\n## Scope\n\n' + caveat + '\n')
    (args.out / 'R7-TABLES.md').write_text(''.join(text))
    print(''.join(text))


if __name__ == '__main__':
    main()
