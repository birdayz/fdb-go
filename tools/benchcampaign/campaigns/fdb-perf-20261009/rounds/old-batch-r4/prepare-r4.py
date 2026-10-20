#!/usr/bin/env python3
import copy
import json
from pathlib import Path
here = Path(__file__).resolve().parent
base = json.loads((here.parent / 'work-old-batch-r3/r3-timed-plan.json').read_text())
for mode in ['fp', 'dwarf']:
    p = copy.deepcopy(base)
    p['campaign'] = 'old-batch-r4-' + mode
    p['purpose'] = 'Perf-lead R4: whole-process native/Go CPU stack attribution with perf record; not a timed performance comparison. Existing binaries, no builds. Optional single CGo DWARF fallback only after evidence of broken FP callchains; retain initial FP artifacts.'
    p['seed'] = 703004 if mode == 'fp' else 703005
    p['blocks'] = 1
    p['instrumentation'] = 'perf-record'
    p['call_graph'] = mode
    p['sample_frequency_requested_hz'] = 1999
    p['diagnostic_only'] = True
    if mode == 'dwarf': p['arms'] = [a for a in p['arms'] if a['name'] == 'cgo']
    for a in p['arms']:
        cmd = a['command']
        code = 'r4fp' if mode == 'fp' else 'r4dw'
        cmd[cmd.index('--prefix')+1] = 'clientbench/' + code + '/xxxxxxxxxxxxxxxxxxxxxxxx/{workload:_<16}/b{block:03d}/' + a['name'] + '/'
        a['command'] = ['/usr/bin/perf', 'record', '-F', '1999', '-g', '--call-graph', mode, '-o', '{output}/perf.data', '--'] + cmd
    p['pinned_files'] += ['/opt/fdbperf/scripts/astra-perf-record-r4.py', '/opt/fdbperf/bin/candidate-hc03-base']
    p['interpretation'] = 'Attribution-only whole-process sampling, including seed,warmup,measurement,validation. One sample run/arm, not counterbalanced timing evidence. Leaf CPU-mode counts must distinguish user/kernel/other; sample percentages are not getrusage CPU percentages. Kernel or library symbol/unwind limitations must be reported.'
    (here / ('r4-' + mode + '-plan.json')).write_text(json.dumps(p, indent=2) + '\n')
