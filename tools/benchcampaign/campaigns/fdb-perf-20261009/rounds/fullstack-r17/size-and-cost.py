#!/usr/bin/env python3
"""Offline pilot-only arithmetic for R17; never reads R17 results or a VM."""
import collections
import csv
import datetime
import hashlib
import json
import math
from pathlib import Path
import statistics

root = Path(__file__).resolve().parent.parent
pilot = root / 'fullstack-r16-extracted' / 'artifacts'
paired = pilot / 'fullstack-r16-read1000' / 'r16-per-block.csv'
with paired.open() as stream:
    ratios = [float(row['ratio']) for row in csv.DictReader(stream)
              if row['numerator'] == 'new' and row['denominator'] == 'cgo'
              and row['metric'] == 'shutdown_process_cpu_ns_per_op']
assert len(ratios) == 6
sigma = statistics.stdev(map(math.log, ratios))
delta = abs(math.log(.93))
normal = statistics.NormalDist()
z_ci, z_power = normal.inv_cdf(.995), normal.inv_cdf(.90)
sizing = []
for factor in [1, 1.25, 1.5]:
    count = math.ceil(((z_ci + z_power) * sigma * factor / delta) ** 2)
    sizing.append(dict(sigma_factor=factor, normal_approx_pairs=count,
                       rounded_multiple_8=8 * math.ceil(count / 8)))
report = dict(pilot_csv=str(paired), pilot_csv_sha256=hashlib.sha256(paired.read_bytes()).hexdigest(),
              pilot_ratios=ratios, sample_log_sd=sigma, target_ratio=.93,
              absolute_log_effect=delta, two_sided_confidence=.99, target_power=.90,
              z_ci=z_ci, z_power=z_power, sizing=sizing, workloads={})
for work in ['read1000', 'batchset50']:
    by_block = collections.defaultdict(dict)
    runs = []
    for folder in sorted((pilot / ('fullstack-r16-' + work)).glob('b*')):
        if not folder.is_dir():
            continue
        invocation = json.loads((folder / 'invocation.json').read_text())
        result = json.loads((folder / 'result.json').read_text())
        assert result['returncode'] == 0
        run = dict(invocation, **result)
        by_block[run['block']][run['arm']] = run
        runs.append(run)
    assert len(by_block) == 6 and len(runs) == 18
    pair_seconds = [(arms['new']['process_wall_ns'] + arms['cgo']['process_wall_ns']) / 1e9
                    for _, arms in sorted(by_block.items())]
    gap_seconds = [(datetime.datetime.fromisoformat(b['started_utc']) -
                    datetime.datetime.fromisoformat(a['ended_utc'])).total_seconds()
                   for a, b in zip(runs, runs[1:])]
    projected_pair = statistics.mean(pair_seconds) + 2 * statistics.mean(gap_seconds)
    report['workloads'][work] = dict(
        process_pair_seconds=pair_seconds,
        mean_process_pair_seconds=statistics.mean(pair_seconds),
        mean_interrun_gap_seconds=statistics.mean(gap_seconds),
        interrun_gaps_seconds=gap_seconds,
        projected_pair_including_two_gaps_seconds=projected_pair,
        projected_160_pairs_minutes=160 * projected_pair / 60)
report['projected_total_minutes'] = sum(w['projected_160_pairs_minutes'] for w in report['workloads'].values())
report['scope'] = ('R16 was three-arm: pair process sums are measured components, not always adjacent spans. '
                   'R17 runtime is projected by adding two measured average orchestration gaps per pair. '
                   'Normal-approximate power is a planning calculation, not guaranteed bootstrap power.')
print(json.dumps(report, indent=2, sort_keys=True))
