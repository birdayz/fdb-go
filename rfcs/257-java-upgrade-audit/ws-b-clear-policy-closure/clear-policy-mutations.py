import json, subprocess
from pathlib import Path
root=Path('/home/birdy/projects/fdb-go'); out=Path('/var/tmp/fdb-upgrade-recovery/ws-b')
mutations=[
 ('reload-order','pkg/recordlayer/store.go','\tstore.stateMu.Lock()\n\tdefer store.stateMu.Unlock()\n\tstore.context.indexStateMu.Lock()\n\tdefer store.context.indexStateMu.Unlock()\n\tstore.indexStateView.mu.Lock()', '\tstore.context.indexStateMu.Lock()\n\tdefer store.context.indexStateMu.Unlock()\n\tstore.stateMu.Lock()\n\tdefer store.stateMu.Unlock()\n\tstore.indexStateView.mu.Lock()', 'Reload and uniqueness cleanup lock ordering'),
 ('unregistered-clear','pkg/recordlayer/index_state.go','\treturn true\n}\n\nfunc (rc *FDBRecordContext) clearRangeWithIndexStateViews','\treturn false\n}\n\nfunc (rc *FDBRecordContext) clearRangeWithIndexStateViews','Index-state clear cache coherence'),
 ('snapshot-authority','pkg/recordlayer/store_api.go','\t\tstates = store.indexStateView.states','\t\tstates = store.indexStates','Shared index-state explicit reload'),
 ('deletion-policy','pkg/recordlayer/store_api.go','ctx.clearRange(fdb.KeyRange{Begin: begin, End: end}, false)','ctx.clearRange(fdb.KeyRange{Begin: begin, End: end}, true)','Opened noncacheable store deletion stamp policy'),
]
results=[]
for name, filename, old, new, focus in mutations:
 p=root/filename; original=p.read_text()
 assert original.count(old)==1,(name,original.count(old))
 try:
  mutated=original.replace(old,new); p.write_text(mutated)
  assert p.read_text()==mutated and new in p.read_text() and old not in p.read_text()
  log=out/('clear-policy-mutant-'+name+'.log')
  with log.open('w') as f:
   f.write('MUTATION PRESENT: '+name+'\n'); f.flush()
   run=subprocess.run(['bazelisk','test','//pkg/recordlayer:recordlayer_test','--nocache_test_results','--test_output=streamed','--test_arg=--test.run=^TestRecordLayer$','--test_arg=--ginkgo.focus='+focus],cwd=root,stdout=f,stderr=subprocess.STDOUT,timeout=240)
  text=log.read_text()
  assert run.returncode!=0 and 'FAILED TO BUILD' not in text and '[FAILED]' in text and 'Ran ' in text,(name,run.returncode)
  results.append(dict(name=name,compiled=True,killed=True,log=str(log)))
 finally:
  p.write_text(original); assert p.read_text()==original
(out/'clear-policy-mutations.json').write_text(json.dumps(results,indent=2)+'\n')
print(json.dumps(results,indent=2))
