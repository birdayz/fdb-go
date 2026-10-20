#!/usr/bin/env python3
import csv
import json
import math
from pathlib import Path
import sys
root = Path('/opt/fdbperf/artifacts')
out = Path(sys.argv[1])
out.mkdir(exist_ok=False)
text = ['# R3 instrumentation — separate diagnostic runs\n\nOne extra old run and one extra CGo run for each instrument,3000 measured transactions+200 warmup,one worker. Counts cover the ENTIRE child process (setup/seed,warmup,measurement,validation/cleanup), not the measured phase. Counts/3000 below are whole-process counts normalized by measured transactions, NOT exact measured-phase event rates. Instrumentation overhead changes behavior; these runs are not unprofiled timing results or a counterbalanced performance comparison.\n']
report = {}
perf_names = ['task-clock', 'cycles', 'instructions', 'context-switches', 'cpu-migrations', 'page-faults']
for kind in ['perf', 'strace']:
    p = root / ('old-batch-r3-' + kind)
    validation = json.loads((p / 'r3-validation.json').read_text())
    assert validation['valid'] and len(validation['per_run']) == 2
    measurements = {}
    for run in validation['per_run']:
        arm = run['arm'];d = p / run['run_id'];n = run['measured_tx']
        assert n == 3000
        if kind == 'perf':
            events = {}
            with (d / 'perf-stat.csv').open() as f:
                for fields in csv.reader(f):
                    if not fields or fields[0].startswith('#') or len(fields) < 3: continue
                    value, unit, event = fields[:3]
                    if event not in perf_names: continue
                    assert event not in events and not value.startswith('<'), (arm, fields)
                    count = float(value);assert math.isfinite(count) and count >= 0
                    events[event] = {'count': count, 'unit': unit, 'per_measured_tx_whole_process': count/n, 'raw_columns': fields}
            assert set(events) == set(perf_names), (arm, events)
            measurements[arm] = events
        else:
            calls = {}
            for line in (d / 'strace-summary.txt').read_text().splitlines():
                parts = line.split()
                if len(parts) not in [5, 6] or parts[-1] == 'total': continue
                try: percentage, seconds, count = float(parts[0]), float(parts[1]), int(parts[3])
                except ValueError: continue
                syscall = parts[-1]
                assert syscall not in calls
                calls[syscall] = {'calls': count, 'errors': int(parts[4]) if len(parts) == 6 else 0, 'seconds': seconds, 'time_percent': percentage, 'calls_per_measured_tx_whole_process': count/n}
            assert calls, arm
            measurements[arm] = calls
    assert set(measurements) == {'old', 'cgo'}
    report[kind] = measurements
    if kind == 'perf':
        text += ['\n## perf stat\n\n|Event|Unit|Old whole-process|CGo whole-process|Old /3000|CGo /3000|\n|---|---|---:|---:|---:|---:|\n']
        for name in perf_names:
            old, cgo = measurements['old'][name], measurements['cgo'][name]
            assert old['unit'] == cgo['unit']
            text.append(f'|{name}|{old["unit"] or "count"}|{old["count"]:.6f}|{cgo["count"]:.6f}|{old["per_measured_tx_whole_process"]:.6f}|{cgo["per_measured_tx_whole_process"]:.6f}|\n')
        text.append('\nRaw event runtime/scaling columns are retained in perf-stat.csv and instruments.json. CPU events are guest-process measurements, not physical-host isolation evidence.\n')
    else:
        requested = ['write', 'writev', 'sendmsg', 'sendto', 'read', 'readv', 'recvfrom', 'recvmsg', 'epoll_wait', 'epoll_pwait', 'epoll_pwait2', 'futex', 'futex_waitv', 'nanosleep', 'clock_nanosleep']
        names = requested + sorted((set(measurements['old']) | set(measurements['cgo'])) - set(requested))
        text += ['\n## strace -f -c syscall census\n\nZero means the tracer reported no call of that syscall in this process, not an unobserved client-internal counter. Syscall elapsed time is tracer-distorted and is not presented as client CPU attribution.\n\n|Syscall|Old calls|CGo calls|Old calls/3000|CGo calls/3000|Old errors|CGo errors|\n|---|---:|---:|---:|---:|---:|---:|\n']
        flat = []
        for name in names:
            old = measurements['old'].get(name, {'calls': 0, 'errors': 0})
            cgo = measurements['cgo'].get(name, {'calls': 0, 'errors': 0})
            text.append(f'|{name}|{old["calls"]}|{cgo["calls"]}|{old["calls"]/3000:.6f}|{cgo["calls"]/3000:.6f}|{old["errors"]}|{cgo["errors"]}|\n')
            flat.append({'syscall': name, 'old_calls': old['calls'], 'cgo_calls': cgo['calls'], 'old_calls_per_measured_tx_whole_process': old['calls']/3000, 'cgo_calls_per_measured_tx_whole_process': cgo['calls']/3000, 'old_errors': old['errors'], 'cgo_errors': cgo['errors']})
        with (out / 'syscall-counts.csv').open('w') as f:
            writer = csv.DictWriter(f, fieldnames=list(flat[0]));writer.writeheader();writer.writerows(flat)
(out / 'instruments.json').write_text(json.dumps(report, indent=2) + '\n')
(out / 'INSTRUMENTS.md').write_text(''.join(text))
print(''.join(text))
