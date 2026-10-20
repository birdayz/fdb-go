#!/usr/bin/env python3
"""Cross-check R7 intervals with the frozen analyzer and tail counts by heap."""
import csv
import heapq
import importlib.util
import json
import math
from pathlib import Path
import random
import statistics

root = Path('/opt/fdbperf/artifacts')
r = json.loads((root / 'pr2-r7-analysis/r7-analysis.json').read_text())
spec = importlib.util.spec_from_file_location('frozen', '/opt/fdbperf/scripts/astra-perf-analyze.py')
frozen = importlib.util.module_from_spec(spec)
spec.loader.exec_module(frozen)
by = {(x['global_block'], x['arm']): x for x in r['per_run']}
blocks = [c + '/b%03d' % i for c in ['R6', 'R7a', 'R7b'] for i in range(6)]
for c in r['comparisons']:
    pairs = [(by[b, c['numerator']][c['metric']], by[b, c['denominator']][c['metric']]) for b in blocks]
    check = frozen.bootstrap_log_ratio(pairs, random.Random(787707), 10000)
    for key, expected in [('paired_geomean_ratio', check['ratio']), ('ci95_low', check['ci95_low']), ('ci95_high', check['ci95_high'])]:
        assert math.isclose(c[key], expected, rel_tol=1e-12), (c, key, expected)
    assert math.isclose(c['paired_geomean_ratio'], statistics.geometric_mean(a/b for a,b in pairs), rel_tol=1e-12)
for x in r['per_run']:
    campaign = {'R6': 'pr2-r6-batchset50x1000', 'R7a': 'pr2-r7-a', 'R7b': 'pr2-r7-b'}[x['campaign']]
    with (root / campaign / x['run_id'] / 'samples.csv').open() as f:
        raw = list(csv.DictReader(f))
    top = heapq.nlargest(20, raw, key=lambda a: (int(a['latency_ns']), -int(a['index'])))
    indices = sorted(int(a['index']) for a in top)
    neighbors = sum((i > 0 and value - indices[i-1] <= 20) or (i+1 < 20 and indices[i+1] - value <= 20)
                    for i, value in enumerate(indices))
    assert neighbors == x['top_one_percent_neighbor_count']
    assert neighbors / 20 == x['top_one_percent_neighbor_fraction']
    assert x['p999_ns'] == sorted(int(a['latency_ns']) for a in raw)[1997]
print(f'PASS: {len(r["comparisons"])} paired estimates/95% CIs independently checked against frozen analyzer; {len(r["per_run"])} tail-neighbor counts checked by heap/adjacent-gap method; all p99.9 values checked at exact rank 1998.')
