import json,re,shutil
from pathlib import Path
D=Path('${FDB_PERF_HOME}/results/fdb-perf-20261009')
source=D/'work-pr5-r10b'
base='8ab5c5a4d6ee4858a4eacd6ac58e2a7be85dbead'
for ident,round,patch,phash,workloads,seed,description in [
 ('pr6-r12','r12','pr6-commit-grv-causal-v2.patch','07912ec16db4bb91a776a914aa772108d2d0329f1bdc2fd22bfba4fbbfcd55ce',[('batchset50',2500,'r16b'),('set100',3000,'r16s')],792106,'PR6 missing-read-version commit GRV uses CAUSAL_READ_RISKY, matching C++; no other optimization stacked'),
 ('pr7-r13','r13','pr7-lease-on-stack.patch','e3d3b651ce8cb613b9383a6a4645276b6f663be3d90f79643c6f050e52c26b1b',[('batchset50',2500,'r17b')],793106,'PR7 executionLease kept on the state-call stack; no PR3/PR4/PR5/PR6 stacked'),
]:
 p=D/('work-'+ident);p.mkdir()
 for f in ('parent-full.tar.gz','parent-git-identity.txt','parent-git-tree.txt','old-reuse-equivalence.txt','build-pr5v2.sh'):
  shutil.copy2(source/f,p/f)
 shutil.copy2(D.parent/'perf-lead-patches'/patch,p/patch)
 expected=sorted('./'+line.split(' b/',1)[1] for line in (p/patch).read_text().splitlines() if line.startswith('diff --git '))
 (p/'expected-delta.json').write_text(json.dumps(expected)+'\n')
 purpose='Exact base '+base+'; OLD freshly rebuilt as candidate-pr5-r10b-old. NEW='+description+'; patchSHA256='+phash+'. FrozenV6 unchanged. CPU is window-only, NOT lifecycle-complete. Counts fixed before measurement.'
 for f in ('validate-r10b.py','report-r10b.py','cpu-scope.py'):
  s=(source/f).read_text()
  s=s.replace('r10b',round).replace('R10B',round.upper()).replace('pr5-'+round,ident)
  s=s.replace("{'read1000':5000,'batchset50':2500}","{'read1000':5000,'batchset50':2500,'set100':3000}")
  s=s.replace('790907',str(seed+601))
  if f.startswith('validate'):
   s=re.sub(r'old=8ab5c5a4d[^\n]*?native7\.3\.77\.', 'source identities are retained in plan.json and verified against each manifest; frozen native7.3.77.',s)
  if f.startswith('report'):
   s=s.replace('PR5v2 nonblocking direct idle writer',description)
   start=s.index('\\n\\nOld=');end=s.index('\\n\\nAbsolute values',start)
   s=s[:start]+'\\n\\n{plan["purpose"]}\\nFrozenV6,{w["iterations"]} measured+200warmup/run,workers1,all6three-armorders.'+s[end:]
  if round=='r12' and f=='cpu-scope.py':
   start=s.index("    text.append('\\nThis frozen base8ab5c5a4d")
   end=s.index("\nnote=",start)
   s=s[:start]+"    text.append('\\nOLD base8ab5c5a4d omits CAUSAL_READ_RISKY on write-only commit GRVs; NEW explicitly adds the C++ flag when no read version exists. This is the PR6 intervention, not an unstated algorithmic-equivalence assumption. C++ NativeAPI.actor.cpp:6577-6578; GrvProxyServer.actor.cpp:647-648.\\n')\n"+s[end:]
  (p/f.replace('r10b',round)).write_text(s)
 for index,(work,n,code) in enumerate(workloads):
  plan=json.loads((source/'r10b-batchset50-plan.json').read_text())
  plan.update(campaign=ident+'-'+work,source_commit=base,seed=seed+index*100,purpose=purpose)
  plan['harness_version']='FrozenV6'
  plan['workloads']=[dict(name=work,iterations=n,warmup=200,workers=1)]
  plan['analysis']['seed']=seed+601
  plan['budget_rationale']='Six three-arm orders; batchset50 2500/200 and set100 3000/200 if requested. Independent180s campaign caps; no adaptive budgets/repeats.'
  build='/opt/fdbperf/artifacts/'+ident+'-build/'
  plan['pinned_files']=[x for x in plan['pinned_files'] if '/pr5-r10b-build/' not in x]+[build+x for x in ['parent-full.tar.gz','parent-git-tree.txt',patch,'full-old.sha256','full-parent.sha256','full-new.sha256','binaries.sha256','runtime.sha256','expected-delta.json']]
  plan['correctness_status_at_authorization']='Perf-lead authorizes '+round+' standalone patch timing; no parent/client tests or Bazel on VM. Harness rawchecks do not confer semantic/review approval.'
  for arm in plan['arms']:
   if arm['name']=='new':
    arm['binary']='/opt/fdbperf/bin/candidate-'+ident+'-new'
    arm['source_revision']=base+'.patch.'+phash
    arm['command'][arm['command'].index('--source-revision')+1]=arm['source_revision']
   arm['command'][arm['command'].index('--prefix')+1]='clientbench/'+code+'/xxxxxxxxxxxxxxxxxxxxxxxx/{workload:_<16}/b{block:03d}/'+arm['name']+'/'
  (p/(round+'-'+work+'-plan.json')).write_text(json.dumps(plan,indent=2)+'\n')
 (p/('prepare-'+round+'.py')).write_text(Path(__file__).read_text())
 (p/'workloads.json').write_text(json.dumps([dict(name=w,iterations=n,warmup=200,workers=1) for w,n,_ in workloads])+'\n')
