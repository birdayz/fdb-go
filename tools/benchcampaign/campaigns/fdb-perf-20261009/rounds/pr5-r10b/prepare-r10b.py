import json, pathlib, shutil, subprocess
D=pathlib.Path('${FDB_PERF_HOME}/results/fdb-perf-20261009')
W='${FDB_PERF_HOME}/worktrees/astra-clientbench'
base='8ab5c5a4d6ee4858a4eacd6ac58e2a7be85dbead'
patches=D.parent/'perf-lead-patches'
oldbase='590145e3161c8dc5cda26b62a95d578b3bdafd3b'
oldpatch='4a413f5d64d829b87e681bd9202a5bfa1fd48c1e74b5c3c68ec4ea88e3ef6063'
pr5='8199a1c86c4df4ea2a877cd39355376d07ccebb3847dc2f4e4d74eb7e882eb54'
stack='999bc230aadbf6f5069b237d29bd4c6a690f30d015971ec175d417e2e0dd6316'
for ident,oldident,round,oldround,patch,phash in [
 ('pr5-r10b','pr5-r10','r10b','r10','pr5v2-direct-write-nonblocking.patch',pr5),
 ('stack-r11v2','stack-r11','r11','r11','stack-pr3-pr4-pr5v2-on-8ab5c5a4d.patch',stack)]:
 p=D/('work-'+ident);p.mkdir()
 prev=D/('work-'+oldident)
 for src in prev.iterdir():
  if src.suffix not in ('.py','.sh'):continue
  if src.name.startswith(('prepare-','build-')):continue
  dst=p/src.name.replace(oldround,round)
  s=src.read_text().replace(oldident,ident).replace(oldbase,base).replace(oldpatch,phash)
  if round!=oldround:
   s=s.replace(oldround,round).replace(oldround.upper(),round.upper())
   # Restore identity strings already changed by replacing the full campaign ID.
   s=s.replace('pr5-r10bb','pr5-r10b')
   s=s.replace('r10bp','r12p').replace('r10bd','r12d')
  s=s.replace('/opt/fdbperf/bin/candidate-pr1-r5-old','/opt/fdbperf/bin/candidate-pr5-r10b-old')
  s=s.replace('/opt/fdbperf/sources/pr1-r5-old','/opt/fdbperf/sources/pr5-r10b-parent')
  s=s.replace('published9377c0863aef7ecdee250cdc323e40ebf69d5e46',base)
  s=s.replace('Old=9377c0863aef7ecdee250cdc323e40ebf69d5e46','Old='+base)
  s=s.replace('old9377 code matches590145 in pkg/fdbgo/tools/dependency pins','OLD freshly rebuilt from the exact base; frozenV6 unchanged')
  s=s.replace('seed790707','seed790907').replace('Random(790707)','Random(790907)').replace("'bootstrap_seed':790707","'bootstrap_seed':790907")
  s=s.replace('PR5 direct idle writer','PR5v2 nonblocking direct idle writer')
  dst.write_text(s)
 shutil.copy2(patches/patch,p/patch)
 subprocess.run(['git','-C',W,'archive','--format=tar.gz','-o',str(p/'parent-full.tar.gz'),base],check=True)
 (p/'parent-git-identity.txt').write_bytes(subprocess.check_output(['git','-C',W,'show','-s','--format=fuller',base]))
 (p/'parent-git-tree.txt').write_bytes(subprocess.check_output(['git','-C',W,'ls-tree','-r',base]))
 (p/'old-reuse-equivalence.txt').write_bytes(subprocess.check_output(['git','-C',W,'diff','--stat',oldbase,base,'--','pkg/fdbgo','go.mod','go.sum','MODULE.bazel','MODULE.bazel.lock','.bazelrc']))
 expected=sorted('./'+line.split(' b/',1)[1] for line in (p/patch).read_text().splitlines() if line.startswith('diff --git '))
 (p/'expected-delta.json').write_text(json.dumps(expected)+'\n')
 for work,n,seed,code in [('read1000',5000,790306 if round=='r10b' else 791106,'r12r' if round=='r10b' else 'r11r'),('batchset50',2500,790406 if round=='r10b' else 791206,'r12b' if round=='r10b' else 'r11b')]:
  plan=json.loads((prev/(oldround+'-'+work+'-plan.json')).read_text())
  plan.update(campaign=ident+'-'+work,source_commit=base,seed=seed)
  plan['purpose']='Exact base '+base+'; OLD freshly rebuilt, NEW '+('PR5v2 nonblocking direct-write only' if round=='r10b' else 'authoritative combined PR3+PR4+PR5v2 patch')+' '+phash+'. FrozenV6 unchanged; window CPU is NOT complete-lifecycle CPU. Fixed budgets before first run.'
  plan['analysis']['seed']=790907 if round=='r10b' else 791707
  build='/opt/fdbperf/artifacts/'+ident+'-build/'
  plan['pinned_files']=[x for x in plan['pinned_files'] if '/'+oldident+'-build/' not in x]+[build+x for x in ['parent-full.tar.gz','parent-git-tree.txt',patch,'full-old.sha256','full-parent.sha256','full-new.sha256','binaries.sha256','runtime.sha256','expected-delta.json']]
  plan['correctness_status_at_authorization']='Perf-lead authorizes '+round+'; only frozen standalone harness tests/build remote. Candidate code review/CI correctness is not implied. The stacked tree has not itself been through the hook.'
  for arm in plan['arms']:
   if arm['name'] in ('old','new'):
    arm['binary']='/opt/fdbperf/bin/candidate-'+('pr5-r10b-old' if arm['name']=='old' else ident+'-new')
    arm['source_revision']=base+('' if arm['name']=='old' else '.patch.'+phash)
    arm['command'][arm['command'].index('--source-revision')+1]=arm['source_revision']
   arm['command'][arm['command'].index('--prefix')+1]='clientbench/'+code+'/xxxxxxxxxxxxxxxxxxxxxxxx/{workload:_<16}/b{block:03d}/'+arm['name']+'/'
  (p/(round+'-'+work+'-plan.json')).write_text(json.dumps(plan,indent=2)+'\n')
 (p/('prepare-'+round+'.py')).write_text(pathlib.Path(__file__).read_text())
