#!/usr/bin/env python3
import copy
import json
from pathlib import Path
here = Path(__file__).resolve().parent
original = here.parent / 'hillclimb-arena-003-recovery-001/artifacts/hillclimb-arena-003-build'
p = json.loads((original / 'read1000-plan.json').read_text())
p['campaign'] = 'hillclimb-arena-003-read-aa-r1'
p['purpose'] = 'Perf-lead R1 prespecified four-arm read1000 variability diagnostic: exact old binary repeated as old-aa alongside old/new/native CGo. Test variability at 5000 operations; not a rerun replacing original adverse 1000-op result, and not a noninferiority/no-regression proof.'
p['seed'] = 477001
p['workloads'] = [{'name': 'read1000', 'iterations': 5000, 'warmup': 200, 'workers': 1}]
old = next(a for a in p['arms'] if a['name'] == 'old')
aa = copy.deepcopy(old)
aa['name'] = 'old-aa'
p['arms'].append(aa)
for a in p['arms']:
    prefix = 'clientbench/a3r1/xxxxxxxxxxxxxxxxxxxxxxxx/{workload:_<16}/b{block:03d}/' + {'old': 'old', 'old-aa': 'aaa', 'new': 'new', 'cgo': 'cgo'}[a['name']] + '/'
    a['command'][a['command'].index('--prefix') + 1] = prefix
p['explicit_orders'] = [
    ['cgo', 'old', 'old-aa', 'new'],
    ['new', 'old-aa', 'old', 'cgo'],
    ['old', 'old-aa', 'new', 'cgo'],
    ['cgo', 'new', 'old-aa', 'old'],
    ['old-aa', 'cgo', 'new', 'old'],
    ['old', 'new', 'cgo', 'old-aa'],
]
p['interpretation'] = 'Six partially counterbalanced blocks, NOT all24 four-arm orders: three reversal pairs, every pairwise precedence3/3, each arm1or2 times/position. Fixed-seed shuffled orders. Paired run-level geometric ratios; A/A measures observed variation, not proof patch cost is zero. Profile-free exploratory diagnostic. Native internal counters unobserved. No capacity/global-native-win/noninferiority claims.'
p['prefix_policy'] = 'All24 unique prefixes are68bytes. old-aa arm uses aaa suffix, preserving79B measured key and159B conflicts. old and old-aa share exact binary and source_revision; only run/output/prefix/block placement differs.'
p['hypothesis'] = 'H0 proposed by coordinator: original read CPU+11% may reflect short-run variability. Alternative: patch/source/fixture-induced cost persists. Data will be reported regardless of direction; no repeat-until-green. The owner adds8B and fixture slab/pool behavior differs, so no-zero-code-cost assumption.'
p['campaign_wall_cap_seconds'] = 180
p['pinned_files'] += ['/opt/fdbperf/scripts/astra-perf-run-r1.py']
(here / 'r1-plan.json').write_text(json.dumps(p, indent=2) + '\n')
