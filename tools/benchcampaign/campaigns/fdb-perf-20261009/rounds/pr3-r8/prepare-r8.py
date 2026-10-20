#!/usr/bin/env python3
import copy
import json
from pathlib import Path
here = Path(__file__).resolve().parent
base = json.loads((here.parent / 'work-pr2-r6/r6-batchset50x1000-plan.json').read_text())
for work, n, seed, code in [('read1000', 5000, 788106, 'r8r1'), ('batchset50', 2500, 788206, 'r8b1')]:
    p = copy.deepcopy(base)
    p['campaign'] = 'pr3-r8-' + work
    p['seed'] = seed
    p['source_commit'] = '590145e3161c8dc5cda26b62a95d578b3bdafd3b'
    p['purpose'] = 'Perf-lead/root846 R8 PR3 16KiB buffered readLoop. Old binary compiled9377 reused after exact pkg/fdbgo+tools+dependency/build-pin equality to master590145. New built from exact590145+PR3; PR1/PR2 NOT stacked. Fixed budgets before first run.'
    p['workloads'] = [dict(name=work, iterations=n, warmup=200, workers=1)]
    p['budget_rationale'] = 'Read5000/200 and batch2500/200 fixed before measurement, separate180s caps. Prior R1 read5000 had max3.040365898s/process, so18*(max+3s gates)=108.727s. No adaptive repeat-until-fits/green.'
    p['analysis'] = dict(seed=788707, resamples=10000, method='common whole-block resamples across metrics/comparisons; percentile95%CI of geometric mean paired ratios', metrics=['mean','p99','CPU','user','system'])
    build = '/opt/fdbperf/artifacts/pr3-r8-build/'
    p['pinned_files'] = ['/usr/lib/libfdb_c.so', '/usr/sbin/fdbserver', '/etc/foundationdb/fdb.cluster', '/etc/foundationdb/foundationdb.conf', '/opt/fdbperf/scripts/astra-perf-run.py', '/opt/fdbperf/scripts/astra-perf-analyze.py', '/opt/fdbperf/artifacts/ownership-001-build/harness-v6.tar.gz', '/opt/fdbperf/artifacts/ownership-001-build/harness-v6.sha256', *[build+f for f in ['parent-full.tar.gz', 'parent-git-tree.txt', 'old-reuse-equivalence.txt', 'pr3-readloop-buffered.patch', 'full-old.sha256', 'full-parent.sha256', 'full-new.sha256', 'binaries.sha256', 'runtime.sha256']]]
    p['correctness_status_at_authorization'] = 'Perf-lead authorizes R8; only frozen standalone-harness tests/build run remotely. Parent correctness/concurrency/Bazel/review approvals are root-owned and not implied by benchmark raw/persistence checks.'
    for arm in p['arms']:
        if arm['name'] == 'new':
            arm['binary'] = '/opt/fdbperf/bin/candidate-pr3-r8-new'
            arm['source_revision'] = p['source_commit'] + '.pr3patch.f02c03072178a7a1bfd4703e58a7401afdfcba1ccee3273ed5f721306d0c2128'
            arm['command'][arm['command'].index('--source-revision')+1] = arm['source_revision']
        arm['command'][arm['command'].index('--prefix')+1] = 'clientbench/'+code+'/xxxxxxxxxxxxxxxxxxxxxxxx/{workload:_<16}/b{block:03d}/'+arm['name']+'/'
    p['prefix_policy'] = 'Fresh equal68-byte prefixes in each campaign; read1000 exact1000B Get via ReadTransact(no commit), batchset50 50distinct100B Sets+commit.'
    (here / ('r8-' + work + '-plan.json')).write_text(json.dumps(p, indent=2) + '\n')
