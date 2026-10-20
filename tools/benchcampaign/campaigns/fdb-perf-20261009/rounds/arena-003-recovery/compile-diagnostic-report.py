#!/usr/bin/env python3
"""Reproduce tables from completed 003 campaigns; never execute a benchmark."""
import csv
import json
import math
from pathlib import Path
import shutil
import statistics

root = Path('/opt/fdbperf/artifacts')
out = root / 'hillclimb-arena-003-diagnostic-report'
out.mkdir(exist_ok=False)
base = 'hillclimb-arena-003'
names = [base, *[base + '-' + x for x in ['read1000', 'get1000', 'range100', 'set100', 'read-cpu', 'read-heap']]]
metrics = [('mean_ns', 'Mean'), ('p99_ns', 'p99'), ('process_cpu_ns_per_op', 'CPU/op')]
comparisons = [('new', 'old'), ('new', 'cgo'), ('old', 'cgo')]
intro = '''# Arena003 completed campaigns

Old = published affef93729368a3fb183df90c5332b5bd2289b25; new = that source plus arena003 scratch patch; CGo = immutable native 7.3.77/API730. CPU is measured-process user+system time, not wall time. Absolute values are medians of six run-level statistics. Ratios are geometric means of six within-block ratios, NOT ratios of the medians. All workloads use one worker and 40 warmup transactions. Batch workloads use 100 measured transactions/arm; unprofiled controls use 1000; separate profiling campaigns use 3000.

Each campaign contains all six orders of old/new/CGo. Results are exploratory, not confirmatory/noninferiority/global-native-win/capacity evidence. Profiled campaigns are separate diagnostics, NOT replacements for unprofiled results. Native internal counters remain unobserved; Go allocation statistics exclude C++ heap. Pooled tails are descriptive. Source/binary checks and benchmark checks are not main-module correctness approval.

'''
text = [intro]
all_rows = []
split_rows = []
split_text = ['# Read CPU accounting from existing raw summaries\n\nNo new workloads. Measured user/system deltas come from getrusage snapshots; CPU = user+system. No context-switch count was captured in these snapshots. Profiled campaigns stay separate. All values are microseconds/transaction.\n']
for name in names:
    source = root / name
    rows = json.loads((source / 'analysis.json').read_text())['per_run']
    expected = 36 if name == base else 18
    assert len(rows) == expected, (name, len(rows))
    copy = out / 'inputs' / name
    copy.mkdir(parents=True)
    for f in ['analysis.json', 'per-run.csv', 'counter-resource-validation.json', 'TABLES.md']:
        shutil.copy2(source / f, copy / f)
    text += ['\n## ' + name + (' — PROFILED DIAGNOSTIC' if name.endswith(('-cpu', '-heap')) else ' — UNPROFILED') + '\n', '|Workload|Arm|Mean µs/op|p99 µs/op|CPU µs/op|\n|---|---|---:|---:|---:|\n']
    workloads = sorted({r['workload'] for r in rows})
    for work in workloads:
        for arm in ['cgo', 'old', 'new']:
            rr = [r for r in rows if r['workload'] == work and r['arm'] == arm]
            assert len(rr) == 6 and {r['block'] for r in rr} == set(range(6))
            text.append('|%s|%s|%s|\n' % (work, arm, '|'.join(f'{statistics.median(r[k] for r in rr)/1000:.3f}' for k, _ in metrics)))
    text += ['\n|Workload|Ratio|Mean|p99|CPU/op|\n|---|---|---:|---:|---:|\n']
    for work in workloads:
        block = {(r['block'], r['arm']): r for r in rows if r['workload'] == work}
        for n, d in comparisons:
            ratios = [math.exp(statistics.mean(math.log(block[b, n][k] / block[b, d][k]) for b in range(6))) for k, _ in metrics]
            text.append(f'|{work}|{n}/{d}|' + '|'.join(f'{v:.6f}' for v in ratios) + '|\n')
    for row in rows:
        all_rows.append({'campaign': name, **row})
    if name not in [base + '-read1000', base + '-read-cpu', base + '-read-heap']:
        continue
    sr = []
    for row in rows:
        s = json.loads((source / row['run_id'] / 'summary.json').read_text())
        before, after = s['stats_before'], s['stats_after']
        n = row['observations']
        user = after['process_user_cpu_us'] - before['process_user_cpu_us']
        system = after['process_system_cpu_us'] - before['process_system_cpu_us']
        assert (user + system) * 1000 == row['process_cpu_ns']
        r = {'campaign': name, 'run_id': row['run_id'], 'block': row['block'], 'position': row['position'], 'arm': row['arm'], 'observations': n, 'user_us': user, 'system_us': system, 'user_us_per_op': user/n, 'system_us_per_op': system/n, 'cpu_us_per_op': (user+system)/n}
        sr.append(r)
        split_rows.append(r)
    split_text += [f'\n## {name}\n', '|Arm|User median µs/op|System median µs/op|CPU median µs/op|User total s|System total s|\n|---|---:|---:|---:|---:|---:|\n']
    for arm in ['cgo', 'old', 'new']:
        rr = [r for r in sr if r['arm'] == arm]
        split_text.append(f'|{arm}|' + '|'.join(f'{statistics.median(r[k] for r in rr):.3f}' for k in ['user_us_per_op', 'system_us_per_op', 'cpu_us_per_op']) + f'|{sum(r["user_us"] for r in rr)/1e6:.6f}|{sum(r["system_us"] for r in rr)/1e6:.6f}|\n')
    split_text += ['\n|Ratio|User paired GM|System paired GM|CPU paired GM|\n|---|---:|---:|---:|\n']
    block = {(r['block'], r['arm']): r for r in sr}
    for n, d in comparisons:
        ratios = [math.exp(statistics.mean(math.log(block[b, n][k] / block[b, d][k]) for b in range(6))) for k in ['user_us_per_op', 'system_us_per_op', 'cpu_us_per_op']]
        split_text.append(f'|{n}/{d}|' + '|'.join(f'{v:.6f}' for v in ratios) + '|\n')
(out / 'ALL-TABLES.md').write_text(''.join(text))
(out / 'READ-CPU-SPLIT.md').write_text(''.join(split_text))
for name, rows in [('PER-BLOCK.csv', all_rows), ('READ-CPU-SPLIT.csv', split_rows)]:
    with (out / name).open('w') as f:
        writer = csv.DictWriter(f, fieldnames=list(rows[0]))
        writer.writeheader()
        writer.writerows(rows)
assert len(all_rows) == 144 and len(split_rows) == 54
print(f'{len(all_rows)} rows total (108 unprofiled + 36 profiled); {len(split_rows)} read accounting rows; all CPU deltas exact')
print((out / 'READ-CPU-SPLIT.md').read_text())
