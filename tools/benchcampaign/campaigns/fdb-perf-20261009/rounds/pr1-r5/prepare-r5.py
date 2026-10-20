#!/usr/bin/env python3
import json
from pathlib import Path
here = Path(__file__).resolve().parent
base = here.parent / 'hillclimb-arena-003-recovery-001/artifacts/hillclimb-arena-003-build/read1000-plan.json'
p = json.loads(base.read_text())
p['campaign'] = 'pr1-r5-batchset50'
p['purpose'] = 'Perf-lead R5/root786 authorized PR1 keyRangeEqualsKeyAfter memcmp comparison; old exact9377c0863aef7ecdee250cdc323e40ebf69d5e46,new old+patch33f1ed2062a8f412f0ee291af16379f41e32aa7edee5b4274d1db1f7fc7b96ef,native immutableCGo. Main-module correctness/review are separate root-owned gates.'
p['source_commit'] = '9377c0863aef7ecdee250cdc323e40ebf69d5e46'
p['seed'] = 786005
p['workloads'] = [{'name':'batchset50','iterations':2500,'warmup':200,'workers':1}]
p['budget_rationale'] = 'Prespecified2500 before execution: observedR3 two-arm12runs122s predicts183s for18runs at3000, above180cap. Reduced per coordinator authorization, not after failed/favorable-result rerun.'
p['correctness_status_at_authorization'] = 'Root786 authorizes source-identifiable comparison; remote frozen standalone tests15/armpassed, not mainmodule/Bazel or finalreview approval.'
build = '/opt/fdbperf/artifacts/pr1-r5-build/'
p['pinned_files'] = ['/usr/lib/libfdb_c.so','/usr/sbin/fdbserver','/etc/foundationdb/fdb.cluster','/etc/foundationdb/foundationdb.conf','/opt/fdbperf/scripts/astra-perf-run.py','/opt/fdbperf/scripts/astra-perf-analyze.py','/opt/fdbperf/artifacts/ownership-001-build/harness-v6.tar.gz','/opt/fdbperf/artifacts/ownership-001-build/harness-v6.sha256',*[build+f for f in ['old-full.tar.gz','old-git-tree.txt','old-git-identity.txt','pr1-keyrange-memeq.patch','full-old.sha256','full-new.sha256','binaries.sha256','runtime.sha256']]]
for a in p['arms']:
    if a['name'] in ['old','new']:
        a['binary'] = '/opt/fdbperf/bin/candidate-pr1-r5-' + a['name']
        revision = p['source_commit'] + ('.pr1patch.33f1ed2062a8f412f0ee291af16379f41e32aa7edee5b4274d1db1f7fc7b96ef' if a['name']=='new' else '')
        a['source_revision'] = revision
        a['command'][a['command'].index('--source-revision')+1] = revision
    a['command'][a['command'].index('--prefix')+1] = 'clientbench/r5p1/xxxxxxxxxxxxxxxxxxxxxxxx/{workload:_<16}/b{block:03d}/'+a['name']+'/'
p['interpretation'] = 'Six counterbalanced blocks covering all6old/new/CGo orders,paired run-level geometric ratios,absolute medians of6run statistics. Exploratory oneVM/workload result,no global/native/capacity/noninferiority claim. Native countersunobserved;GoMemStats excludeC++heap.'
p['prefix_policy'] = 'All18 unique prefixes68bytes; identicalworkload/key/value/timeout/retry/frozenV6shape acrossarms.'
(here / 'r5-plan.json').write_text(json.dumps(p,indent=2)+'\n')
