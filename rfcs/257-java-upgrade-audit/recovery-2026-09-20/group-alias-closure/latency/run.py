import pathlib,subprocess,json,hashlib,re,os,time,collections
R=pathlib.Path('/var/tmp/fdb-upgrade-recovery/runtime');OUT=R/'latency-comparison';ROOT=pathlib.Path('/home/birdy/projects/fdb-go');BASE=pathlib.Path('/home/birdy/projects/fdb-java-upgrade-baseline')
GO='/home/birdy/.cache/bazel/_bazel_birdy/cf0955a431539502aaf740b39f444075/external/rules_go++go_sdk+main___download_0/bin/go'
def snap(root):
 names=subprocess.check_output(['git','-C',str(root),'ls-files','--cached','--others','--exclude-standard','-z']).decode().split('\0');return {p:hashlib.sha256((root/p).read_bytes()).hexdigest() for p in sorted(set(names)) if p and (root/p).is_file()}
def tree(root,side):
 env=dict(os.environ,GIT_INDEX_FILE=str(OUT/(side+'.index')));assert not pathlib.Path(env['GIT_INDEX_FILE']).exists();subprocess.run(['git','read-tree','HEAD'],cwd=root,env=env,check=True)
 paths=subprocess.check_output(['git','ls-files','--cached','--others','--exclude-standard','-z'],cwd=root);fp=OUT/(side+'-paths.nul');fp.write_bytes(paths);subprocess.run(['git','add','--pathspec-from-file='+str(fp),'--pathspec-file-nul'],cwd=root,env=env,check=True);return subprocess.check_output(['git','write-tree'],cwd=root,env=env,text=True).strip()
assert os.stat(ROOT).st_dev==os.stat(BASE).st_dev
for file in ['go.mod','pkg/relational/sqldriver/stress/stress_test.go','pkg/relational/sqldriver/stress/latency_attribution_stress_test.go']:
 assert (ROOT/file).read_bytes()==(BASE/file).read_bytes(),file
assert subprocess.check_output(['git','-C',str(BASE),'rev-parse','HEAD'],text=True).strip()=='e48f5b4965543cd4d99b5578356059e12d969c7c'
freeze={side:snap(root) for side,root in [('baseline',BASE),('current',ROOT)]};assert freeze['current']==json.loads((R/'latency-freeze.json').read_text())
trees={side:tree(root,side) for side,root in [('baseline',BASE),('current',ROOT)]}
for side in freeze:(OUT/(side+'-freeze.json')).write_text(json.dumps(freeze[side],indent=2)+'\n')
(OUT/'trees.json').write_text(json.dumps(trees,indent=2)+'\n')
old=json.loads((R/'slot-identity-stress/stress-samples.json').read_text())[0]['runs'];expected={k.replace('TestFDB_Stress_1M','TestFDB_Stress_1M_LatencyAttribution') for k in old};assert len(expected)==24
records=[]
for side,n in [('baseline',1),('current',1),('current',2),('baseline',2)]:
 root=BASE if side=='baseline' else ROOT;name=f'{side}-{n}';log=OUT/(name+'.log');assert not log.exists();bep=OUT/(name+'-bep.jsonl');trace=OUT/(name+'.trace')
 cmd=['bazelisk','test','//pkg/relational/sqldriver/stress:stress_test','--nocache_test_results','--test_output=all','--test_arg=--test.run=^TestFDB_Stress_1M_LatencyAttribution$','--test_arg=-test.trace='+str(trace),'--sandbox_writable_path='+str(OUT),'--build_event_json_file='+str(bep)]
 load=os.getloadavg();started=time.time()
 with log.open('w') as f:p=subprocess.run(cmd,cwd=root,stdout=f,stderr=subprocess.STDOUT)
 text=log.read_text();runs=collections.Counter(re.findall(r'^=== RUN\s+(\S+)',text,re.M));passes=collections.Counter(re.findall(r'^\s*--- PASS: (\S+)',text,re.M));samples=[json.loads(l.split('LATENCY_ATTRIBUTION ',1)[1]) for l in text.splitlines() if 'LATENCY_ATTRIBUTION ' in l]
 summaries=[e['testSummary'] for l in bep.read_text().splitlines() if 'testSummary' in (e:=json.loads(l))]
 binary=root/'bazel-bin/pkg/relational/sqldriver/stress/stress_test_/stress_test';compiler=subprocess.check_output([GO,'version',str(binary)],text=True).strip()
 record=dict(side=side,sample=n,tree=trees[side],command=cmd,started=started,elapsed=time.time()-started,load_start=load,load_end=os.getloadavg(),exit=p.returncode,runs=dict(runs),passes=dict(passes),compiler=compiler,attribution=samples,explain=re.findall(r'EXPLAIN: (.*)',text),log_sha256=hashlib.sha256(log.read_bytes()).hexdigest(),trace_sha256=hashlib.sha256(trace.read_bytes()).hexdigest(),trace_bytes=trace.stat().st_size)
 records.append(record);(OUT/'samples.json').write_text(json.dumps(records,indent=2)+'\n')
 print(name,'exit',p.returncode,'runs',sum(runs.values()),'telemetry',len(samples),'load',load,record['load_end'],'trace',trace.stat().st_size,flush=True)
 for x in samples[:4]:print(x['sql'],x['query_ns']/1e6,'ms','plan',x['plans'][0]['PlanningDuration']/1e6,'exec',x['executions'][0]['ExecutionDuration']/1e6,'store_open',x['store_timers'].get('open_store'),flush=True)
 assert p.returncode==0 and len(summaries)==1 and summaries[0]['overallStatus']=='PASSED' and summaries[0].get('numCached',0)==0
 assert runs==passes and set(runs)==expected and all(v==1 for v in runs.values())
 assert len(samples)==20 and 'COUNT(*) = 1000000 (expected 1000000)' in text and len(record['explain'])==11
 assert compiler.endswith('go1.26.6') and snap(root)==freeze[side]
 assert all(len(x['plans'])==len(x['executions'])==1 and x['rows']==x['executions'][0]['RowsReturned'] and x['query_ns']>0 and x['store_timers'] for x in samples)
print('four balanced instrumented samples completed on frozen trees',trees,flush=True)
