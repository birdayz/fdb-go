#!/usr/bin/env python3
import csv
import json
import math
from pathlib import Path
import re
import statistics
import sys

p = Path(sys.argv[1])
a = json.loads((p / 'analysis.json').read_text())
plan = json.loads((p / 'plan.json').read_text())
rows = a['per_run']
assert len(rows) == 24
arms = ['cgo', 'old', 'old-aa', 'new']
by_arm = {x['name']: x for x in plan['arms']}
assert by_arm['old']['binary'] == by_arm['old-aa']['binary']
assert by_arm['old']['source_revision'] == by_arm['old-aa']['source_revision']
comparisons = [('new', 'old'), ('old-aa', 'old'), ('old', 'cgo'), ('new', 'cgo'), ('old-aa', 'cgo')]
metrics = ['mean_ns', 'p99_ns', 'process_cpu_ns_per_op', 'user_cpu_ns_per_op', 'system_cpu_ns_per_op']
for r in rows:
    s = json.loads((p / r['run_id'] / 'summary.json').read_text())
    before, after = s['stats_before'], s['stats_after']
    n = r['observations']
    for label in ['user', 'system']:
        r[label + '_cpu_ns_per_op'] = (after['process_' + label + '_cpu_us'] - before['process_' + label + '_cpu_us']) * 1000 / n
    assert abs(r['user_cpu_ns_per_op'] + r['system_cpu_ns_per_op'] - r['process_cpu_ns_per_op']) < .0001
    timing = (p / r['run_id'] / 'process-time.txt').read_text()
    for label, key in [('Voluntary context switches', 'whole_process_voluntary_context_switches'), ('Involuntary context switches', 'whole_process_involuntary_context_switches')]:
        matches = re.findall(r'^\s*' + re.escape(label) + r':\s*(\d+)\s*$', timing, flags=re.MULTILINE)
        assert len(matches) == 1
        r[key] = int(matches[0])
    assert re.search(r'^\s*Exit status: 0\s*$', timing, flags=re.MULTILINE)
by = {(r['block'], r['arm']): r for r in rows}
assert len(by) == 24
text = ['# R1 read1000 A/A variability diagnostic\n\n5000 measured transactions, 200 warmup, one worker, no profiling; existing immutable three binaries, old-aa reuses exact old binary. Six partial-counterbalanced blocks, not all24 orders: three reversal pairs, pairwise precedence3/3 and per-arm positions1or2 appearances. Fixed-seed shuffled schedule.\n\nAbsolute values are medians of six run statistics; ratios are geometric means of six within-block ratios, NOT ratios of medians. Latency and CPU columns are µs/transaction. User+system CPU are measured-phase getrusage deltas. Context-switch counts are **whole process**, including setup/fixture/warmup/validation, NOT measured-phase counts. Their per-run totals must not be equated to scheduler work during measurement.\n\n|Arm|Mean|p99|CPU|User CPU|System CPU|CPU/wall|Voluntary ctx/run|Involuntary ctx/run|\n|---|---:|---:|---:|---:|---:|---:|---:|---:|\n']
for arm in arms:
    rr = [r for r in rows if r['arm'] == arm]
    assert len(rr) == 6
    values = [statistics.median(r[k] for r in rr) / 1000 for k in metrics]
    text.append(f'|{arm}|' + '|'.join(f'{x:.3f}' for x in values) + f'|{statistics.median(r["process_cpu_over_wall"] for r in rr):.4f}|{statistics.median(r["whole_process_voluntary_context_switches"] for r in rr):.1f}|{statistics.median(r["whole_process_involuntary_context_switches"] for r in rr):.1f}|\n')
text += ['\n|Ratio|Mean|p99|CPU|User CPU|System CPU|CPU pairs <1|\n|---|---:|---:|---:|---:|---:|---:|\n']
paired = []
per_block = []
for numerator, denominator in comparisons:
    ratios = {k: [by[b, numerator][k] / by[b, denominator][k] for b in range(6)] for k in metrics}
    gm = {k: math.exp(statistics.mean(math.log(x) for x in rr)) for k, rr in ratios.items()}
    text.append(f'|{numerator}/{denominator}|' + '|'.join(f'{gm[k]:.6f}' for k in metrics) + f'|{sum(x < 1 for x in ratios["process_cpu_ns_per_op"])}/6|\n')
    paired.append({'numerator': numerator, 'denominator': denominator, 'paired_geomean': gm, 'ratios': ratios})
    for block in range(6):
        per_block.append({'block': block, 'numerator': numerator, 'denominator': denominator, **{k: ratios[k][block] for k in metrics}})
text += ['\n## Per-block ratios\n\n|Block|Ratio|Mean|p99|CPU|User CPU|System CPU|\n|---|---|---:|---:|---:|---:|---:|\n']
for r in per_block:
    text.append(f'|{r["block"]}|{r["numerator"]}/{r["denominator"]}|' + '|'.join(f'{r[k]:.6f}' for k in metrics) + '|\n')
text += ['\n## Per-run CPU and whole-process switches\n\n|Run|Mean µs|CPU µs|User µs|System µs|CPU/wall|Voluntary ctx|Involuntary ctx|\n|---|---:|---:|---:|---:|---:|---:|---:|\n']
for r in rows:
    text.append(f'|{r["run_id"]}|' + '|'.join(f'{r[k]/1000:.3f}' for k in ['mean_ns', 'process_cpu_ns_per_op', 'user_cpu_ns_per_op', 'system_cpu_ns_per_op']) + f'|{r["process_cpu_over_wall"]:.4f}|{r["whole_process_voluntary_context_switches"]}|{r["whole_process_involuntary_context_switches"]}|\n')
text += ['\n## Scope\n\nThis adaptive six-block A/A experiment characterizes observed variability at5000 ops/run; it does not prove zero patch cost, noninferiority, or explain away the original1000-op new/old CPU1.110292 result. The patch changes an owner24→32B and fixture slab/pool state even though each measured read fits a512B slab and never resets it. More operations and warmup change GC/lifetime conditions. No experiment was repeated to obtain a favorable result. Old/new/CGo native comparisons remain scoped to this VM/workload; native internal counters are unobserved, and GoMemStats excludes native heap. Raw/persistence/counter checks do not constitute main-module correctness approval.\n']
(p / 'R1-TABLES.md').write_text(''.join(text))
(p / 'r1-detailed.json').write_text(json.dumps({'per_run': rows, 'paired': paired}, indent=2) + '\n')
for filename, data in [('r1-per-run.csv', rows), ('r1-per-block.csv', per_block)]:
    with (p / filename).open('w') as f:
        writer = csv.DictWriter(f, fieldnames=list(data[0]))
        writer.writeheader()
        writer.writerows(data)
print(''.join(text))
