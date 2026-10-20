#!/usr/bin/env python3
import copy
import json
from pathlib import Path
here=Path(__file__).resolve().parent
base=json.loads((here.parent/'work-pr1-r5/r5-plan.json').read_text())
for work,n,seed,code in [('batchset50',2500,786006,'r6s1'),('batchset50x1000',2000,786106,'r6l1')]:
 p=copy.deepcopy(base);p['campaign']='pr2-r6-'+work;p['seed']=seed
 p['purpose']='Perf-lead R6 PR2 commit-capture borrowing versus exact9377old,withoutPR1stacked;existingold/CGo,onlynewrebuilt. Source-identifiable experimental comparison,not parentcorrectness/finalreview approval.'
 p['workloads']=[{'name':work,'iterations':n,'warmup':200,'workers':1}]
 p['budget_rationale']='Small2500,large2000 were explicitly prespecified/approved byperf-lead beforeexecution;eachseparate18run campaignhas180scap. No repeatuntilfits/green.'
 p['correctness_status_at_authorization']='Perf-lead authorizes R6;newfrozenstandalone15harness tests passed. ParentBazel/concurrency/finalreview gates remainrootowned andarenot implied byrawbyte checks.'
 build='/opt/fdbperf/artifacts/pr2-r6-build/'
 p['pinned_files']=['/usr/lib/libfdb_c.so','/usr/sbin/fdbserver','/etc/foundationdb/fdb.cluster','/etc/foundationdb/foundationdb.conf','/opt/fdbperf/scripts/astra-perf-run.py','/opt/fdbperf/scripts/astra-perf-analyze.py','/opt/fdbperf/artifacts/ownership-001-build/harness-v6.tar.gz','/opt/fdbperf/artifacts/ownership-001-build/harness-v6.sha256','/opt/fdbperf/artifacts/pr1-r5-build/old-full.tar.gz','/opt/fdbperf/artifacts/pr1-r5-build/old-git-tree.txt',*[build+f for f in ['pr2-commit-capture-borrow.patch','full-old.sha256','full-new.sha256','binaries.sha256','runtime.sha256']]]
 for arm in p['arms']:
  if arm['name']=='new':
   arm['binary']='/opt/fdbperf/bin/candidate-pr2-r6-new'
   arm['source_revision']=p['source_commit']+'.pr2patch.b7736a4a40a159b1c1e75904c28292ee7be0e0f763e89a5a8d9c5d756c876a18'
   arm['command'][arm['command'].index('--source-revision')+1]=arm['source_revision']
  arm['command'][arm['command'].index('--prefix')+1]='clientbench/'+code+'/xxxxxxxxxxxxxxxxxxxxxxxx/{workload:_<16}/b{block:03d}/'+arm['name']+'/'
 p['prefix_policy']='All18unique prefixes68bytes percampaign;identicalkey/value shapeacrossarms. Eachtransactiondoes50Sets+Commit,100Bvaluesforsmall/1000Bvaluesforlarge.'
 (here/('r6-'+work+'-plan.json')).write_text(json.dumps(p,indent=2)+'\n')
