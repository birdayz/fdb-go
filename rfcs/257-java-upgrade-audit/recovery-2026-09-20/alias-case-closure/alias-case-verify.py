import pathlib,subprocess,json,hashlib,re,collections,time,os,urllib.parse
R=pathlib.Path('/var/tmp/fdb-upgrade-recovery/runtime'); ROOT=pathlib.Path('/home/birdy/projects/fdb-go'); BASE=pathlib.Path('/home/birdy/projects/fdb-java-upgrade-baseline'); OUT=R/'alias-case-verification';OUT.mkdir(exist_ok=True)
def snap(root):
 names=subprocess.check_output(['git','ls-files','--cached','--others','--exclude-standard','-z'],cwd=root).decode().split('\0');return {p:hashlib.sha256((root/p).read_bytes()).hexdigest() for p in sorted(set(names)) if p and (root/p).is_file()}
freeze=snap(ROOT);basefreeze=snap(BASE);assert not subprocess.check_output(['git','status','--porcelain'],cwd=BASE).strip()
(OUT/'freeze.json').write_text(json.dumps(freeze,indent=2)+'\n')
env=os.environ.copy();env['GIT_INDEX_FILE']=str(OUT/'snapshot.index');assert not pathlib.Path(env['GIT_INDEX_FILE']).exists()
subprocess.run(['git','read-tree','HEAD'],cwd=ROOT,env=env,check=True);subprocess.run(['git','add','--pathspec-from-file=-','--pathspec-file-nul'],cwd=ROOT,env=env,input=('\0'.join(freeze)+'\0').encode(),check=True)
tree=subprocess.check_output(['git','write-tree'],cwd=ROOT,env=env,text=True).strip();(OUT/'tree.txt').write_text(tree+'\n');print('FROZEN',tree,len(freeze),flush=True)
results=[]
def run(name,cmd,root=ROOT,expected=None):
 assert snap(ROOT)==freeze and snap(BASE)==basefreeze
 log=OUT/(name+'.log');assert not log.exists();start=time.time();load=os.getloadavg()
 with log.open('w') as f: p=subprocess.run(cmd,cwd=root,stdout=f,stderr=subprocess.STDOUT)
 text=log.read_text();runs=collections.Counter(re.findall(r'^\s*=== RUN\s+(\S+)',text,re.M));passes=collections.Counter(re.findall(r'^\s*--- PASS: (\S+)',text,re.M));skips=collections.Counter(re.findall(r'^\s*--- SKIP: (\S+)',text,re.M))
 rec=dict(name=name,command=cmd,exit=p.returncode,elapsed=time.time()-start,started=start,load_start=load,load_end=os.getloadavg(),runs=dict(runs),passes=dict(passes),skips=dict(skips),sha256=hashlib.sha256(log.read_bytes()).hexdigest());results.append(rec);(OUT/'results.json').write_text(json.dumps(results,indent=2)+'\n')
 assert snap(ROOT)==freeze and snap(BASE)==basefreeze,'source changed';print(name,'exit',p.returncode,'elapsed',rec['elapsed'],flush=True);assert p.returncode==0,name
 if expected is not None:
  bep=OUT/(name+'-bep.jsonl'); events=[json.loads(x) for x in bep.read_text().splitlines()];summaries=[e for e in events if 'testSummary' in e];assert len(summaries)==expected
  logs=OUT/(name+'-logs');logs.mkdir(exist_ok=True);tests=[]
  for e in summaries:
   label=e['id']['testSummary']['label'];s=e['testSummary'];assert s['overallStatus']=='PASSED' and s.get('numCached',0)==0
   assert len(s['passed'])==1;src=pathlib.Path(urllib.parse.unquote(urllib.parse.urlparse(s['passed'][0]['uri']).path));data=src.read_bytes();dest=logs/(label.replace('//','').replace('/','__').replace(':','__')+'.log');dest.write_bytes(data); txt=data.decode()
   ns={k:collections.Counter(re.findall(pat,txt,re.M)) for k,pat in [('run',r'^\s*=== RUN\s+(\S+)'),('pass',r'^\s*--- PASS: (\S+)'),('skip',r'^\s*--- SKIP: (\S+)'),('fail',r'^\s*--- FAIL: (\S+)')]};assert not ns['fail'] and ns['run']==ns['pass']+ns['skip'],label
   tests.append(dict(label=label,sha256=hashlib.sha256(data).hexdigest(),log=str(dest),counts={k:sum(v.values()) for k,v in ns.items()},skips=dict(ns['skip'])))
  (OUT/(name+'-population.json')).write_text(json.dumps(tests,indent=2)+'\n');print(name,expected,'uncached targets',dict(sum((collections.Counter(t['counts']) for t in tests),collections.Counter())),flush=True)
 return text,rec
pathlib.Path('/tmp/alias-owner-fuzz').mkdir(exist_ok=True)
run('fuzz',['bazelisk','test','//pkg/relational/core/embedded:embedded_test','--nocache_test_results','--test_output=all','--test_arg=--test.run=^$','--test_arg=--test.fuzz=^FuzzPositionalSortColumnIdentity$','--test_arg=--test.fuzztime=30s','--test_arg=--test.parallel=4','--test_arg=--test.fuzzcachedir=/tmp/alias-owner-fuzz','--sandbox_writable_path=/tmp/alias-owner-fuzz'])
for name,old,expected in [('full','group-alias-full-command.json',93),('race','alias-source-race-command.json',15)]:
 cmd=json.loads((R/old).read_text());cmd=[('--build_event_json_file='+str(OUT/(name+'-bep.jsonl'))) if a.startswith('--build_event_json_file=') else a for a in cmd];run(name,cmd,expected=expected)
run('just-test',['just','test'])
stress=[]
for side,n in [('baseline',1),('current',1),('current',2),('baseline',2)]:
 name=side+'-'+str(n);root=BASE if side=='baseline' else ROOT;cmd=['bazelisk','test','//pkg/relational/sqldriver/stress:stress_test','--nocache_test_results','--test_output=all','--test_arg=--test.run=^TestFDB_Stress_1M$','--build_event_json_file='+str(OUT/(name+'-bep.jsonl'))]
 text,rec=run(name,cmd,root,expected=1);assert rec['runs']==rec['passes'] and len(rec['runs'])==24 and not rec['skips'] and 'COUNT(*) = 1000000 (expected 1000000)' in text
 rec.update(side=side,sample=n,tree=tree if side=='current' else subprocess.check_output(['git','rev-parse','HEAD^{tree}'],cwd=BASE,text=True).strip(),commit=subprocess.check_output(['git','rev-parse','HEAD'],cwd=root,text=True).strip(),explain=re.findall(r'EXPLAIN: (.*)',text));assert len(rec['explain'])==11
 stress.append(rec);assert rec['explain']==stress[0]['explain'];(OUT/'stress-samples.json').write_text(json.dumps(stress,indent=2)+'\n')
assert snap(ROOT)==freeze and snap(BASE)==basefreeze;print('ALL VERIFICATION COMPLETE; hashes unchanged',tree,flush=True)
