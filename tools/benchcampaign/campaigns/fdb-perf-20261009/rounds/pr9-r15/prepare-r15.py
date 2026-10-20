#!/usr/bin/env python3
import argparse
import copy
import importlib.util
import json
from pathlib import Path

p=Path('/opt/fdbperf/artifacts/pr9-r15-build')
v7=Path('/opt/fdbperf/artifacts/lifecycle-v7-build')
scripts=Path('/opt/fdbperf/sources/lifecycle-v7-baseline/tools/clientbench/scripts')
spec=importlib.util.spec_from_file_location('generator',scripts/'r3_plan.py')
generator=importlib.util.module_from_spec(spec);spec.loader.exec_module(generator)
base='8ab5c5a4d6ee4858a4eacd6ac58e2a7be85dbead'
p5='95c1a474c2eb59f7b493b538c0d7ab179d61617e3aca4f78a39c3ee0da26c7a8'
p9='f81ad05e2af0c0b68fe4558a824e71aea815267435b7f6dad1b52686c2e6991c'
harness=v7/'harness-v7-candidate.tar.gz'
for work,count,seed,namespace in [('read1000',5000,795106,'b7151'+'0'*24),('batchset50',2500,795206,'b7152'+'0'*24)]:
    args=argparse.Namespace(go_binary=Path('/opt/fdbperf/bin/clientbench-r15-old'),cgo_binary=Path('/opt/fdbperf/bin/clientbench-v7-cgo'),
        cluster_file=Path('/etc/foundationdb/fdb.cluster'),native_library=Path('/usr/lib/libfdb_c.so'),server_binary=Path('/usr/sbin/fdbserver'),
        source_revision=base,harness_id=generator.digest(harness),campaign_id=namespace,
        housekeeping_cpus='0-1',server_cpus='2-7',client_cpus='8-15',
        pin=[harness,v7/'standalone-wrapper-v7.tar.gz',v7/'parent-full.tar.gz',v7/'parent-git-tree.txt',v7/'review-acks.json',v7/'review-candidate-sha.txt',
             p/'pr5v3-direct-write-275875031.patch',p/'pr9-eager-lone-get.patch',p/'full-parent.sha256',p/'full-old.sha256',p/'full-new.sha256',
             p/'binaries.sha256',p/'runtime.sha256',p/'build-status.txt',p/'build-r15.sh',p/'prepare-r15.py',p/'run-r15.sh',p/'report-r15.py',
             p/'source-delta.json',Path('/etc/foundationdb/foundationdb.conf')])
    plan=generator.make_plan(args)
    old,cgo=plan['arms'];old['name']='old';old['source_revision']=base+'.patch.'+p5
    old['command'][old['command'].index('--source-revision')+1]=old['source_revision']
    old['command']=[v.replace('/go_/','/old/') for v in old['command']]
    new=copy.deepcopy(old);new['name']='new';new['binary']='/opt/fdbperf/bin/clientbench-r15-new';new['source_revision']=old['source_revision']+'.patch.'+p9
    new['command'][new['command'].index('--source-revision')+1]=new['source_revision']
    new['command']=[v.replace('/old/','/new/') for v in new['command']]
    plan['arms']=[old,new,cgo]
    plan['campaign']='pr9-r15-'+work;plan['seed']=seed
    plan['workloads']=[dict(name=work,iterations=count,warmup=200,workers=1)]
    plan['analysis']=dict(seed=795707,resamples=10000,method='common paired-block percentile95%bootstrap; six within-block ratios, not ratios of medians')
    plan['harness_version']='V7.06097ca4609b1b927a9827f4fff5e8cedf358e46'
    plan['purpose']='PR9 lone pipelined Get eager write, incremental over PR5v3 on exact8ab. OLD=base+patch'+p5+'; NEW=OLD+patch'+p9+'. Neither is the R11combinedstack. Both retain missing CAUSAL_READ_RISKY on write-only commit GRV. FrozenreviewedV7; window(C-B),drained(D-B),shutdown(E-A) perN; absoluteE unpairedonly. No profiler/build overlaps.'
    plan['budget_rationale']='Prespecified read5000/200; batch2500/200 to leave margin under180s for18runs given freshV7batch3000 needed123s for12runs. No adaptive rerun budget.'
    plan['correctness_scope']='Only frozen standalone benchmark unit tests plus rawresult/persistence/clientcounter checks; not parent/client correctness tests, reviews or CI approval of PR9.'
    paths=set(plan['pinned_files'])|{a['binary'] for a in plan['arms']}
    plan['expected_sha256']={path:generator.digest(path) for path in sorted(paths)}
    with (p/('r15-'+work+'-plan.json')).open('x') as f:f.write(json.dumps(plan,indent=2,sort_keys=True)+'\n')
