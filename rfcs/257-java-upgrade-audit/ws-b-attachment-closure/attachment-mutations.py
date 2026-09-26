import json, subprocess
from pathlib import Path
root=Path('/home/birdy/projects/fdb-go')
out=Path('/var/tmp/fdb-upgrade-recovery/ws-b')
mutations=[
 ('cache-history','pkg/recordlayer/index_state.go','\tfor _, cleared := range rc.indexStateClears {','\tfor _, cleared := range []fdb.KeyRange{} {','Index-state clear cache coherence'),
 ('cache-stamp','pkg/recordlayer/index_state.go','\t\t\t// current context\'s cache reads and entries used after commit.\n\t\t\trc.SetDirtyStoreState(true)\n\t\t\trc.SetMetaDataVersionStamp()','\t\t\t// current context\'s cache reads and entries used after commit.\n\t\t\trc.SetDirtyStoreState(true)','Index-state clear cache coherence'),
 ('reload-publication','pkg/recordlayer/store.go','\tstore.indexStateView.states = maps.Clone(states)','\t// mutation: omit shared reload publication','Shared index-state explicit reload'),
]
results=[]
for name, filename, old, new, focus in mutations:
 p=root/filename
 original=p.read_text()
 assert original.count(old)==1, (name,original.count(old))
 try:
  mutated=original.replace(old,new)
  p.write_text(mutated)
  assert p.read_text()==mutated and new in p.read_text() and old not in p.read_text()
  log=out/('attachment-mutant-'+name+'.log')
  with log.open('w') as f:
   f.write('MUTATION PRESENT: '+name+'\n'); f.flush()
   run=subprocess.run(['bazelisk','test','//pkg/recordlayer:recordlayer_test','--nocache_test_results','--test_output=streamed','--test_arg=--test.run=^TestRecordLayer$','--test_arg=--ginkgo.focus='+focus],cwd=root,stdout=f,stderr=subprocess.STDOUT,timeout=240)
  text=log.read_text()
  assert run.returncode!=0 and 'FAILED TO BUILD' not in text and '[FAILED]' in text and 'Ran ' in text, (name,run.returncode)
  results.append(dict(name=name,compiled=True,killed=True,log=str(log)))
 finally:
  p.write_text(original)
  assert p.read_text()==original
(out/'attachment-mutations.json').write_text(json.dumps(results,indent=2)+'\n')
print(json.dumps(results,indent=2))
