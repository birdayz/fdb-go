#!/usr/bin/env python3
import copy
import json
from pathlib import Path
here = Path(__file__).resolve().parent
base = json.loads((here.parent / 'work-pr2-r6/r6-batchset50x1000-plan.json').read_text())
for work, n, seed, code in [('read1000', 5000, 789106, 'r9r1'), ('batchset50', 2500, 789206, 'r9b1')]:
    p = copy.deepcopy(base)
    p['campaign'] = 'pr4-r9-' + work
    p['seed'] = seed
    p['source_commit'] = '590145e3161c8dc5cda26b62a95d578b3bdafd3b'
    p['purpose'] = 'Perf-lead R9 PR4 inline lone GRV. Old binary compiled9377 reused after exact pkg/fdbgo+tools+dependency/build-pin equality to master590145. New built from exact590145+PR4; PR1/PR2/PR3 NOT stacked. Fixed budgets before first run.'
    p['workloads'] = [dict(name=work, iterations=n, warmup=200, workers=1)]
    p['budget_rationale'] = 'Read5000/200 and batch2500/200 fixed before measurement, separate180s caps. Prior R1 read5000 had max3.040365898s/process, so18*(max+3s gates)=108.727s. No adaptive repeat-until-fits/green.'
    p['analysis'] = dict(seed=789707, resamples=10000, method='common whole-block resamples across metrics/comparisons; percentile95%CI of geometric mean paired ratios', metrics=['mean','p99','CPU','user','system'])
    build = '/opt/fdbperf/artifacts/pr4-r9-build/'
    p['pinned_files'] = ['/usr/lib/libfdb_c.so', '/usr/sbin/fdbserver', '/etc/foundationdb/fdb.cluster', '/etc/foundationdb/foundationdb.conf', '/opt/fdbperf/scripts/astra-perf-run.py', '/opt/fdbperf/scripts/astra-perf-analyze.py', '/opt/fdbperf/artifacts/ownership-001-build/harness-v6.tar.gz', '/opt/fdbperf/artifacts/ownership-001-build/harness-v6.sha256', *[build+f for f in ['parent-full.tar.gz', 'parent-git-tree.txt', 'old-reuse-equivalence.txt', 'pr4-grv-inline.patch', 'full-old.sha256', 'full-parent.sha256', 'full-new.sha256', 'binaries.sha256', 'runtime.sha256']]]
    p['correctness_status_at_authorization'] = 'Perf-lead authorizes R9; only frozen standalone-harness tests/build run remotely. Parent correctness/concurrency/Bazel/review approvals are root-owned and not implied by benchmark raw/persistence checks.'
    for arm in p['arms']:
        if arm['name'] == 'new':
            arm['binary'] = '/opt/fdbperf/bin/candidate-pr4-r9-new'
            arm['source_revision'] = p['source_commit'] + '.pr4patch.2414b55ab15d20c8597ebc54917b67c24320c8f1b02e8ab6a412a0c1de3be014'
            arm['command'][arm['command'].index('--source-revision')+1] = arm['source_revision']
        arm['command'][arm['command'].index('--prefix')+1] = 'clientbench/'+code+'/xxxxxxxxxxxxxxxxxxxxxxxx/{workload:_<16}/b{block:03d}/'+arm['name']+'/'
    p['prefix_policy'] = 'Fresh equal68-byte prefixes in each campaign; read1000 exact1000B Get via ReadTransact(no commit), batchset50 50distinct100B Sets+commit.'
    (here / ('r9-' + work + '-plan.json')).write_text(json.dumps(p, indent=2) + '\n')
