#!/usr/bin/env python3
import csv
import json
from pathlib import Path

out = Path('/opt/fdbperf/artifacts/pr3-r8-strace')
assert json.loads((out/'complete.json').read_text())['completed'] == 2
report, rows = {}, []
for arm in ['old','new']:
    folder = out/arm
    calls = {}
    total = None
    for line in (folder/'strace-summary.txt').read_text().splitlines():
        fields = line.split()
        if len(fields) not in (5,6):
            continue
        try:
            float(fields[0]); float(fields[1]); int(fields[2])
            count = int(fields[3]); errors = int(fields[4]) if len(fields)==6 else 0
        except ValueError:
            continue
        if fields[-1]=='total':
            total = dict(calls=count,errors=errors)
        else:
            assert fields[-1] not in calls
            calls[fields[-1]] = dict(calls=count,errors=errors)
    assert total and sum(v['calls'] for v in calls.values()) == total['calls']
    assert sum(v['errors'] for v in calls.values()) == total['errors']
    assert 'read' in calls
    summary = json.loads((folder/'summary.json').read_text())
    assert summary['success'] and summary['completed']==5000 and summary['warmup_completed']==200
    report[arm] = dict(syscalls=calls,total=total,measured_transactions=5000,warmup_transactions=200,
                       measured_callback_retries=summary['retries'],warmup_callback_retries=summary['warmup_retries'])
    for name in ['read','recvfrom','readv','recvmsg','recvmmsg']:
        counts = calls.get(name,dict(calls=0,errors=0))
        rows.append(dict(arm=arm,syscall=name,**counts,calls_divided_by_measured_tx=counts['calls']/5000,
                         errors_divided_by_measured_tx=counts['errors']/5000))
text = ['# R8 read1000 strace diagnostics\n\nONE old and ONE new process, old then new, 5,000 measured +200 warmup each. '
        'All syscall counts are process-lifetime, INCLUDING startup, executable hashing, setup, warmup and persisted validation. '
        'Normalization divides this whole-process population by 5,000 measured transactions; these are NOT measured-phase counts. '
        'Instrumented latency/CPU is not used as a timed comparison. No CGo syscall control was requested for this attribution step.\n\n'
        '|Arm|Syscall|Calls|Errors|Calls / 5000|Errors / 5000|\n|---|---|---:|---:|---:|---:|\n']
for r in rows:
    text.append(f'|{r["arm"]}|{r["syscall"]}|{r["calls"]}|{r["errors"]}|{r["calls_divided_by_measured_tx"]:.6f}|{r["errors_divided_by_measured_tx"]:.6f}|\n')
for arm in ['old','new']:
    r=report[arm]
    text.append(f'\n{arm}: measured/warmup callback retries {r["measured_callback_retries"]}/{r["warmup_callback_retries"]}; all {len(r["syscalls"])} syscall rows sum exactly to strace total {r["total"]}.\n')
old=report['old']['syscalls']['read']['calls'];new=report['new']['syscalls']['read']['calls']
report['read_new_old_ratio']=new/old
text.append(f'\nWhole-process read() new/old count ratio: {new/old:.6f}.\n')
(out/'strace-analysis.json').write_text(json.dumps(report,indent=2)+'\n')
(out/'STRACE-TABLES.md').write_text(''.join(text))
with (out/'strace-per-arm.csv').open('w') as f:
    w=csv.DictWriter(f,fieldnames=list(rows[0]));w.writeheader();w.writerows(rows)
print(''.join(text))
