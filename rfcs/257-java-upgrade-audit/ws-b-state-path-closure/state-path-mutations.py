from pathlib import Path
import subprocess,json,re
root=Path('/var/tmp/fdb-upgrade-recovery/ws-b')
cases=[
 ('omit-state-conflict','pkg/recordlayer/index_state.go','if !view.conflicts[indexName] {','if false && !view.conflicts[indexName] {','//pkg/recordlayer:recordlayer_test','--ginkgo.focus=Transactional function selection|Bulk deletion state conflicts','[FAIL]'),
 ('isolate-state-handles','pkg/recordlayer/index_state.go','view := rc.indexStateViews[key]','view := rc.indexStateViews[key+"\\x00"]','//pkg/recordlayer:recordlayer_test','--ginkgo.focus=Transactional function selection|Bulk deletion state conflicts','[FAIL]'),
 ('omit-write-liveness','pkg/recordlayer/store.go','if _, err := store.context.Transaction().GetReadVersion().Get(); err != nil {\n\t\treturn err\n\t}', '', '//pkg/recordlayer:recordlayer_test','--ginkgo.focus=index update boundary','[FAIL]'),
 ('omit-executor-readable-gate','pkg/recordlayer/query/executor/executor.go','if state == recordlayer.IndexStateReadable {','if state >= recordlayer.IndexStateReadable {','//pkg/recordlayer/query/executor:executor_test','-test.run=^TestExecutePlanTransactionalIndexState$','--- FAIL: TestExecutePlanTransactionalIndexState'),
]
formatter=subprocess.check_output(["bazelisk","run","--run_under=echo","@cc_mvdan_gofumpt//:gofumpt"],stderr=subprocess.DEVNULL,text=True).strip()
results=[]
for name,file,old,new,target,focus,failure in cases:
 p=Path(file);original=p.read_text();assert original.count(old)==1,(name,original.count(old))
 try:
  p.write_text(original.replace(old,new))
  subprocess.run([formatter,"-w",str(p)],check=True)
  assert old not in p.read_text()
  if new: assert p.read_text().count(new)==1
  log=root/('state-mutant-'+name+'.log')
  with log.open('w') as out:
   out.write('Verified mutation present: '+name+'\n');out.flush()
   r=subprocess.run(['bazelisk','test',target,'--nocache_test_results','--test_output=all','--test_arg='+focus,'--test_arg=-test.v'],stdout=out,stderr=subprocess.STDOUT)
  text=log.read_text();ran=bool(re.search(r'Ran [1-9][0-9]* of',text)) if 'recordlayer:recordlayer_test' in target else '=== RUN   TestExecutePlanTransactionalIndexState/' in text
  killed=r.returncode!=0 and ran and failure in text and 'FAILED TO BUILD' not in text
  results.append(dict(name=name,exit=r.returncode,compiled_and_killed=killed))
  (root/'state-path-mutations.json').write_text(json.dumps(results,indent=2)+'\n')
  assert killed,text[-4000:]
  print(name,'compiled and killed',flush=True)
 finally:
  p.write_text(original);assert p.read_text()==original
