#!/usr/bin/env python3
import copy
import json
from pathlib import Path
here = Path(__file__).resolve().parent
original = here.parent / 'hillclimb-arena-003-recovery-001/artifacts/hillclimb-arena-003-build/read1000-plan.json'
base = json.loads(original.read_text())
for kind in ['timed', 'perf', 'strace']:
    p = copy.deepcopy(base)
    p['campaign'] = 'old-batch-r3-' + kind
    p['purpose'] = 'Perf-lead R3: longer batchset50 old/CGo comparison plus separately instrumented whole-process attribution. Existing binaries only, no builds. No claim that GC/warmup explains earlier100tx results without causal isolation.'
    p['seed'] = {'timed': 641001, 'perf': 641002, 'strace': 641003}[kind]
    p['arms'] = [a for a in p['arms'] if a['name'] in ['old', 'cgo']]
    p['workloads'] = [{'name': 'batchset50', 'iterations': 3000, 'warmup': 200, 'workers': 1}]
    p['blocks'] = 6 if kind == 'timed' else 1
    p['instrumentation'] = 'none' if kind == 'timed' else kind
    p['diagnostic_only'] = kind != 'timed'
    p['campaign_wall_cap_seconds'] = 180
    for a in p['arms']:
        code = {'timed': 'r3tm', 'perf': 'r3pf', 'strace': 'r3st'}[kind]
        cmd = a['command']
        cmd[cmd.index('--prefix')+1] = 'clientbench/' + code + '/xxxxxxxxxxxxxxxxxxxxxxxx/{workload:_<16}/b{block:03d}/' + a['name'] + '/'
        if kind == 'perf':
            a['command'] = ['/usr/bin/perf', 'stat', '-x,', '-o', '{output}/perf-stat.csv', '-e', 'task-clock,cycles,instructions,context-switches,cpu-migrations,page-faults', '--'] + cmd
        if kind == 'strace':
            a['command'] = ['/usr/bin/strace', '-f', '-c', '-o', '{output}/strace-summary.txt', '--'] + cmd
    p['pinned_files'] += ['/usr/bin/perf', '/usr/bin/strace']
    if kind != 'timed':
        p['pinned_files'] += ['/opt/fdbperf/scripts/astra-perf-diagnostic-r3.py']
    p['prefix_policy'] = 'Unique68B prefixes; identical key/value shape across old/CGo;3000measure/200warm;no profiler in timed campaign.'
    p['interpretation'] = ('Six paired exploratory blocks: each of the two orders appears three times. Absolute median-of-run statistics and within-block geometric ratios. No global speed/capacity/noninferiority claims.' if kind == 'timed' else 'One extra run per arm, diagnostic only, not counterbalanced or an unprofiled speed comparison. Instrumentation counts cover entire child process including fixture,warmup,validation. Dividing by3000 is normalization, not exact measured-phase event rate.')
    (here / ('r3-' + kind + '-plan.json')).write_text(json.dumps(p, indent=2) + '\n')
