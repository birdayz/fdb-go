#!/usr/bin/env python3
import argparse
import copy
import importlib.util
import json
from pathlib import Path

p=Path('/opt/fdbperf/artifacts/fullstack-r16-build');v7=Path('/opt/fdbperf/artifacts/lifecycle-v7-build')
scripts=Path('/opt/fdbperf/sources/lifecycle-v7-baseline/tools/clientbench/scripts')
spec=importlib.util.spec_from_file_location('generator',scripts/'r3_plan.py');generator=importlib.util.module_from_spec(spec);spec.loader.exec_module(generator)
base='7922a2e5a8e3a2d6cf17dfee8b8134181c88b298'
patch='d0a6ae57ac1143aa1247ce2461f84f74264825f901a53cb7ab943a0efe83be85'
native_origin='8ab5c5a4d6ee4858a4eacd6ac58e2a7be85dbead'
harness=v7/'harness-v7-candidate.tar.gz'
for work,count,seed,namespace in [('read1000',5000,796106,'b7161'+'0'*24),('batchset50',2500,796206,'b7162'+'0'*24)]:
    args=argparse.Namespace(go_binary=Path('/opt/fdbperf/bin/clientbench-r16-old'),cgo_binary=Path('/opt/fdbperf/bin/clientbench-v7-cgo'),
        cluster_file=Path('/etc/foundationdb/fdb.cluster'),native_library=Path('/usr/lib/libfdb_c.so'),server_binary=Path('/usr/sbin/fdbserver'),
        source_revision=base,harness_id=generator.digest(harness),campaign_id=namespace,
        housekeeping_cpus='0-1',server_cpus='2-7',client_cpus='8-15',
        pin=[harness,v7/'standalone-wrapper-v7.tar.gz',v7/'parent-full.tar.gz',v7/'review-acks.json',v7/'review-candidate-sha.txt',
             p/'parent-full.tar.gz',p/'parent-git-tree.txt',p/'root-module-pins-equivalence.patch',p/'fullstack-on-7922a2e5a.patch',
             p/'full-parent.sha256',p/'full-old.sha256',p/'full-new.sha256',p/'binaries.sha256',p/'runtime.sha256',p/'build-status.txt',
             p/'build-r16.sh',p/'prepare-r16.py',p/'run-r16.sh',p/'report-r16.py',p/'source-delta.json',Path('/etc/foundationdb/foundationdb.conf')])
    plan=generator.make_plan(args)
    old,cgo=plan['arms'];old['name']='old';old['command']=[v.replace('/go_/','/old/') for v in old['command']]
    new=copy.deepcopy(old);new['name']='new';new['binary']='/opt/fdbperf/bin/clientbench-r16-new';new['source_revision']=base+'.patch.'+patch
    new['command'][new['command'].index('--source-revision')+1]=new['source_revision'];new['command']=[v.replace('/old/','/new/') for v in new['command']]
    cgo['source_revision']=native_origin;cgo['command'][cgo['command'].index('--source-revision')+1]=native_origin
    plan['arms']=[old,new,cgo];plan['campaign']='fullstack-r16-'+work;plan['seed']=seed
    plan['workloads']=[dict(name=work,iterations=count,warmup=200,workers=1)]
    plan['analysis']=dict(seed=796707,resamples=10000,method='common paired-block percentile95%bootstrap; six within-block ratios, not ratios of medians')
    plan['harness_version']='V7.06097ca4609b1b927a9827f4fff5e8cedf358e46'
    plan['purpose']='Cumulative fullstack on exact7922a2e5a8e3a2d6cf17dfee8b8134181c88b298, patch'+patch+' (PRs812,818,816,834,824,827,841,828,832 plus reply-error allocation). OLD=base; NEW=base+patch. OLD retains missing CAUSAL_READ_RISKY on write-only commit GRV; NEW fixes it, part of the declared intervention. CGo is unchanged V7native7.3.77 built from8ab; dependency/build pins identical to7922 (empty root-module equivalence diff archived). FrozenreviewedV7: window(C-B),drained(D-B),shutdown(E-A) perN; absoluteE unpairedonly. No profiler/build overlaps.'
    plan['budget_rationale']='Read5000/200 and batch2500/200 fixed before timing;18runs each/all6three-armorders,180scap independently. PriorV7batch3000 took123s for12runs;2500 leaves margin for18. No adaptive rerun.'
    plan['correctness_scope']='Frozen standalone benchmark unit tests and rawresult/persistence/clientcounter checks only; no parent/client correctness tests, reviews or CI approval of combinedpatch.'
    paths=set(plan['pinned_files'])|{a['binary'] for a in plan['arms']};plan['expected_sha256']={path:generator.digest(path) for path in sorted(paths)}
    with (p/('r16-'+work+'-plan.json')).open('x') as f:f.write(json.dumps(plan,indent=2,sort_keys=True)+'\n')
