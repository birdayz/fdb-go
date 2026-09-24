from pathlib import Path
import hashlib, json, subprocess
root=Path.cwd()
out=Path('/var/tmp/fdb-upgrade-recovery/ws-b')
mutations=[('fresh-policy', 'pkg/recordlayer/store_builder.go', 'if len(index.GetReplacedByIndexNames()) == 0 {\n\t\t\tdesired =', 'if len(index.GetReplacedByIndexNames()) >= 0 {\n\t\t\tdesired ='), ('reconcile-enumeration', 'pkg/recordlayer/store_builder.go', 'indexesToBuild := store.metaData.GetIndexesSince(oldMetaDataVersion)', 'indexesToBuild := store.metaData.GetIndexesToBuildSince(oldMetaDataVersion)'), ('immediate-retirement', 'pkg/recordlayer/store_builder.go', 'return store.removeReplacedIndexes()', 'return nil'), ('eligible-original', 'pkg/recordlayer/metadata.go', 'if len(index.GetReplacedByIndexNames()) == 0 {\n\t\t\tresult = append(result, index)', 'if len(index.GetReplacedByIndexNames()) >= 0 {\n\t\t\tresult = append(result, index)'), ('lock-preservation', 'pkg/recordlayer/index_state.go', 'subkeys := []int64{\n\t\tindexBuildScannedRecordsSubKey,', 'subkeys := []int64{\n\t\t0,\n\t\tindexBuildScannedRecordsSubKey,'), ('queue-cleanup', 'pkg/recordlayer/index_state.go', '9, // INDEX_PENDING_WRITE_QUEUE_SIZE', '8, // INDEX_PENDING_WRITE_QUEUE_SIZE'), ('version-cleanup', 'pkg/recordlayer/index_state.go', 'store.context.ClearRange(idxPrefixRange)', 'store.context.Transaction().ClearRange(idxPrefixRange)'), ('former-state-name', 'pkg/recordlayer/index_state.go', 'store.setIndexState(former.FormerName, IndexStateReadable)', 'store.setIndexState(fmt.Sprint(subKey), IndexStateReadable)')]
results=[]
for name,file,old,new in mutations:
 p=Path(file); original=p.read_bytes(); text=original.decode(); assert text.count(old)==1,(name,text.count(old))
 try:
  p.write_text(text.replace(old,new)); assert new in p.read_text(); assert p.read_bytes()!=original
  log=out/f'init-cleanup-mutant-{name}.log'
  with log.open('w') as f:
   f.write(f'APPLIED {name}: {file}\nSHA256 {hashlib.sha256(p.read_bytes()).hexdigest()}\n'); f.flush()
   proc=subprocess.run(['bazelisk','test','//pkg/recordlayer:recordlayer_test','--nocache_test_results','--test_output=all','--test_arg=--ginkgo.focus=Replacement retirement|Commit hooks|CommitWithVersionstamp','--test_arg=--ginkgo.v'],stdout=f,stderr=subprocess.STDOUT,timeout=240)
  text=log.read_text(); compiled='Ran ' in text and 'Specs in ' in text; killed=proc.returncode!=0 and compiled and '[FAIL] ' in text
  results.append(dict(name=name,compiled=compiled,killed=killed,exit=proc.returncode,log=log.name))
  assert killed,(name,proc.returncode,text[-2000:])
 finally:
  p.write_bytes(original); assert hashlib.sha256(p.read_bytes()).digest()==hashlib.sha256(original).digest()
  (out/'init-cleanup-mutations.json').write_text(json.dumps(results,indent=2)+'\n')
print(json.dumps(results,indent=2))
