#!/usr/bin/env python3
import csv
import json
import math
from pathlib import Path

out = Path('/opt/fdbperf/artifacts/pr4-r9-perf')
assert json.loads((out/'complete.json').read_text())['completed'] == 2
report, rows = {}, []
for arm in ['old', 'new']:
    folder = out/arm
    counters = {}
    for fields in csv.reader((folder/'perf-stat.csv').read_text().splitlines()):
        if len(fields) < 5 or fields[2].strip() not in ['context-switches', 'task-clock']:
            continue
        value, unit, event = float(fields[0]), fields[1].strip(), fields[2].strip()
        assert math.isfinite(value) and value >= 0 and event not in counters
        if event == 'context-switches':
            assert value == int(value)
        else:
            assert value > 0 and unit == 'msec'
        counters[event] = dict(value=value,unit=unit or 'count',runtime_ns=float(fields[3]),running_percent=float(fields[4]))
        assert 0 < counters[event]['running_percent'] <= 100
        rows.append(dict(arm=arm,event=event,**counters[event],whole_process_value_divided_by_measured_tx=value/5000))
    assert set(counters) == {'context-switches', 'task-clock'}
    summary = json.loads((folder/'summary.json').read_text())
    assert summary['success'] and summary['completed'] == 5000 and summary['warmup_completed'] == 200
    report[arm] = dict(counters=counters,measured_transactions=5000,warmup_transactions=200,
                       measured_callback_retries=summary['retries'],warmup_callback_retries=summary['warmup_retries'])
text = ['# R9 read1000 perf-stat diagnostics\n\nONE old then ONE new process,5000 measured+200warmup each. '
        'These counters cover the entire process, INCLUDING startup, executable hashing, setup, warmup, measurement, '
        'persisted validation and shutdown. Divide by5000 only for a whole-process-per-measured-transaction normalization; '
        'NOT measured-phase counters. perf task-clock is NOT interchangeable with measured-phase getrusage user+system CPU. '
        'No CGo control or counterbalanced diagnostic repeats; instrumented timings do not enter campaign conclusions.\n\n'
        '|Arm|Event|Whole-process value|Unit|Value / 5000|Running %|\n|---|---|---:|---|---:|---:|\n']
for r in rows:
    text.append(f'|{r["arm"]}|{r["event"]}|{r["value"]:.6f}|{r["unit"]}|{r["whole_process_value_divided_by_measured_tx"]:.6f}|{r["running_percent"]:.2f}|\n')
for event in ['context-switches', 'task-clock']:
    old,new = report['old']['counters'][event]['value'],report['new']['counters'][event]['value']
    assert old > 0
    report[event+'_new_old_ratio'] = new/old
    text.append(f'\nWhole-process {event} new/old ratio: {new/old:.6f}.\n')
for arm in ['old','new']:
    r=report[arm]
    text.append(f'\n{arm}: measured/warmup callback retries {r["measured_callback_retries"]}/{r["warmup_callback_retries"]}.\n')
(out/'perf-analysis.json').write_text(json.dumps(report,indent=2)+'\n')
(out/'PERF-TABLES.md').write_text(''.join(text))
with (out/'perf-per-arm.csv').open('w') as f:
    writer=csv.DictWriter(f,fieldnames=list(rows[0]));writer.writeheader();writer.writerows(rows)
print(''.join(text))
