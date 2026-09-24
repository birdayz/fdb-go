from pathlib import Path
import hashlib, json, subprocess
root=Path.cwd()
out=Path('/var/tmp/fdb-upgrade-recovery/ws-b')
mutations=[
 ('plain-commit','pkg/recordlayer/database.go','_, err := rc.CommitWithVersionstamp()\n\treturn err','return rc.tx.Commit().Get()'),
 ('named-dedup','pkg/recordlayer/database.go','if entry := rc.namedCommitChecks[name]; entry != nil {\n\t\treturn entry.check\n\t}\n\tentry :=','if entry := rc.namedCommitChecks[name]; entry != nil && !entry.active {\n\t\treturn entry.check\n\t}\n\tentry :='),
 ('named-cancel','pkg/recordlayer/database.go','entry.active = false','entry.active = true'),
 ('readiness','pkg/recordlayer/index_state.go','if state != IndexStateReadable {\n\t\t\t\tready = false','if !state.IsScannable() {\n\t\t\t\tready = false'),
 ('delete-cancel','pkg/recordlayer/store_api.go','ctx.removeCommitCheck(replacementRetirementCheckName(ss))','ctx.removeCommitCheck(replacementRetirementCheckName(ss) + "_not_registered")'),
]
p=Path('pkg/recordlayer/index_state.go')
s=p.read_text(); start=s.index('\tkey := store.indexStateSubspace().Pack(tuple.Tuple{indexName})',s.index('func (store *FDBRecordStore) readIndexState(')); end=s.index('\n}\n',start)
mutations.append(('state-read',str(p),s[start:end],'\treturn store.GetIndexState(indexName), nil'))
results=[]
for name,file,old,new in mutations:
 p=Path(file); original=p.read_bytes(); text=original.decode(); assert text.count(old)==1,(name,text.count(old))
 try:
  p.write_text(text.replace(old,new)); assert new in p.read_text(); assert p.read_bytes()!=original
  log=out/f'retirement-mutant-{name}.log'
  with log.open('w') as f:
   f.write(f'APPLIED {name}: {file}\nSHA256 {hashlib.sha256(p.read_bytes()).hexdigest()}\n'); f.flush()
   proc=subprocess.run(['bazelisk','test','//pkg/recordlayer:recordlayer_test','--nocache_test_results','--test_output=all','--test_arg=--ginkgo.focus=Replacement retirement|Commit hooks|CommitWithVersionstamp','--test_arg=--ginkgo.v'],stdout=f,stderr=subprocess.STDOUT,timeout=240)
  text=log.read_text(); compiled='Ran ' in text and 'Specs in ' in text; killed=proc.returncode!=0 and compiled and '[FAIL] ' in text
  results.append(dict(name=name,compiled=compiled,killed=killed,exit=proc.returncode,log=log.name))
  assert killed,(name,proc.returncode,text[-2000:])
 finally:
  p.write_bytes(original); assert hashlib.sha256(p.read_bytes()).digest()==hashlib.sha256(original).digest()
  (out/'retirement-mutations.json').write_text(json.dumps(results,indent=2)+'\n')
print(json.dumps(results,indent=2))
