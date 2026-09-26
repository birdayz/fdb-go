import pathlib,subprocess,json,hashlib,re,collections,time,os
R=pathlib.Path('/var/tmp/fdb-upgrade-recovery/runtime');ROOT=pathlib.Path('/home/birdy/projects/fdb-go');BASE=pathlib.Path('/home/birdy/projects/fdb-java-upgrade-baseline');OUT=R/'group-alias-stress';OUT.mkdir(exist_ok=True)
GO='/home/birdy/.cache/bazel/_bazel_birdy/cf0955a431539502aaf740b39f444075/external/rules_go++go_sdk+main___download_0/bin/go'
def snap(root):
 names=subprocess.check_output(['git','ls-files','--cached','--others','--exclude-standard','-z'],cwd=root).decode().split('\0');return {p:hashlib.sha256((root/p).read_bytes()).hexdigest() for p in sorted(set(names)) if p and (root/p).is_file()}
freeze=json.loads((R/'group-alias-final-code-freeze.json').read_text());assert snap(ROOT)==freeze
basefreeze=snap(BASE);assert not subprocess.check_output(['git','status','--porcelain'],cwd=BASE).strip();assert subprocess.check_output(['git','rev-parse','HEAD'],cwd=BASE,text=True).strip()=='e48f5b4965543cd4d99b5578356059e12d969c7c'
def run(name,cmd,root=ROOT):
 log=OUT/(name+'.log');assert not log.exists();start=time.time();load=os.getloadavg()
 with log.open('w') as f:r=subprocess.run(cmd,cwd=root,stdout=f,stderr=subprocess.STDOUT)
 text=log.read_text();runs=collections.Counter(re.findall(r'^=== RUN\s+(\S+)',text,re.M));passes=collections.Counter(re.findall(r'^\s*--- PASS: (\S+)',text,re.M));assert snap(ROOT)==freeze and snap(BASE)==basefreeze
 record=dict(name=name,command=cmd,exit=r.returncode,started=start,elapsed=time.time()-start,load_start=load,load_end=os.getloadavg(),runs=dict(runs),passes=dict(passes),log_sha256=hashlib.sha256(log.read_bytes()).hexdigest())
 (OUT/(name+'-result.json')).write_text(json.dumps(record,indent=2)+'\n');print(name,'exit',r.returncode,'runs',sum(runs.values()),'elapsed',record['elapsed'],flush=True);assert r.returncode==0,name
 return text,record
text,rec=run('stress-race',['bazelisk','--output_base=/home/birdy/.cache/bazel/_race_output_base','test','//pkg/relational/sqldriver/stress:stress_test','--@rules_go//go/config:race','--nocache_test_results','--test_output=all','--test_arg=--test.run=^(TestLatencySampleValidation|TestFDB_LatencyHarnessIsolation|TestFDB_Stress_1M_LatencyAttribution)$','--build_event_json_file='+str(OUT/'stress-race-bep.jsonl')]);assert rec['runs']==rec['passes'] and sum(rec['runs'].values())==40 and text.count('LATENCY_ATTRIBUTION ')==20 and 'COUNT(*) = 1000000 (expected 1000000)' in text
run('just-test',['just','test'])
old=json.loads((R/'slot-identity-stress/stress-samples.json').read_text())[0]['runs'];assert len(old)==24
records=[]
for side,n in [('baseline',1),('current',1),('current',2),('baseline',2)]:
 root=BASE if side=='baseline' else ROOT;name=side+'-'+str(n);bep=OUT/(name+'-bep.jsonl');cmd=['bazelisk','test','//pkg/relational/sqldriver/stress:stress_test','--nocache_test_results','--test_output=all','--test_arg=--test.run=^TestFDB_Stress_1M$','--build_event_json_file='+str(bep)]
 text,rec=run(name,cmd,root);assert rec['runs']==rec['passes'] and set(rec['runs'])==set(old) and all(v==1 for v in rec['runs'].values()) and 'COUNT(*) = 1000000 (expected 1000000)' in text
 summaries=[e['testSummary'] for line in bep.read_text().splitlines() if 'testSummary' in (e:=json.loads(line))];assert len(summaries)==1 and summaries[0]['overallStatus']=='PASSED' and summaries[0].get('numCached',0)==0
 rec.update(side=side,sample=n,tree=(R/'group-alias-final-code-tree.txt').read_text().strip() if side=='current' else subprocess.check_output(['git','rev-parse','HEAD^{tree}'],cwd=BASE,text=True).strip(),commit=subprocess.check_output(['git','rev-parse','HEAD'],cwd=root,text=True).strip(),compiler=subprocess.check_output([GO,'version',str(root/'bazel-bin/pkg/relational/sqldriver/stress/stress_test_/stress_test')],text=True).strip(),explain=re.findall(r'EXPLAIN: (.*)',text));assert rec['compiler'].endswith('go1.26.6') and len(rec['explain'])==11
 records.append(rec);(OUT/'samples.json').write_text(json.dumps(records,indent=2)+'\n')
print('Race40 RUN/PASS, just test and four nominal ABBA 1M samples complete; source hashes unchanged',flush=True)
