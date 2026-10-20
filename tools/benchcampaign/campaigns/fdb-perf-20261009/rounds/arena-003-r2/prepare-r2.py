#!/usr/bin/env python3
import copy
import json
from pathlib import Path
here = Path(__file__).resolve().parent
original = here.parent / 'hillclimb-arena-003-recovery-001/artifacts/hillclimb-arena-003-build'
base = json.loads((original / 'read-cpu-plan.json').read_text())
for profile in ['cpu', 'heap']:
    p = copy.deepcopy(base)
    p['campaign'] = 'old-batch-attribution-r2-' + profile
    p['purpose'] = 'Attribution-only old affef937 Go profile, no performance comparison. Root#491 explicitly authorized omission of CGo for single-arm pprof attribution; timed comparisons still require native.'
    p['attribution_only'] = True
    p['authorization'] = 'root request#491; perf-lead R2'
    p['blocks'] = 1
    p['seed'] = 491001 if profile == 'cpu' else 491002
    p['arms'] = [next(a for a in p['arms'] if a['name'] == 'old')]
    command = p['arms'][0]['command']
    command[command.index('--profile')+1] = profile
    code = 'r2cp' if profile == 'cpu' else 'r2hp'
    command[command.index('--prefix')+1] = 'clientbench/' + code + '/xxxxxxxxxxxxxxxxxxxxxxxx/{workload:_<16}/b{block:03d}/old/'
    p['workloads'] = [{'name': 'batchset50', 'iterations': 3000, 'warmup': 100, 'workers': 1}]
    if profile == 'cpu':
        p['workloads'].append({'name': 'batchset50x1000', 'iterations': 2000, 'warmup': 100, 'workers': 1})
    p['interpretation'] = 'Single-arm/single-run attribution only, no old/new/CGo comparison or timing inference. Go pprof heap is coarse sampled lifetime including preparation; exact MemStats differences cover measured phase.'
    p['prefix_policy'] = 'Unique68B prefixes; same workload/key shape as003 campaigns.'
    p['pinned_files'] += ['/opt/fdbperf/scripts/astra-perf-attribution-r2.py']
    (here / ('r2-' + profile + '-plan.json')).write_text(json.dumps(p, indent=2) + '\n')
