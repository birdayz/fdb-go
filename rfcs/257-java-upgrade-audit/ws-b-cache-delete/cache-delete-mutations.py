from pathlib import Path
import subprocess,json,hashlib,re
r=Path('/var/tmp/fdb-upgrade-recovery/ws-b');base=Path('pkg/recordlayer');results=[]
cases=[('cache-admission','store_state_cache.go','if entry.recordStoreState.StoreHeader.GetCacheable() {\n\t\t\tc.addToCache','if entry.recordStoreState.StoreHeader != nil {\n\t\t\tc.addToCache'),('cache-eviction','store_state_cache.go','c.invalidateOlderEntry(subKey, currentStamp)','c.invalidateOlderEntry(subKey, nil)'),('cache-old-header','store.go','header := proto.Clone(store.storeHeader).(*gen.DataStoreInfo)\n\theader.Cacheable','header := store.storeHeader\n\theader.Cacheable'),('delete-header-conflict','store_api.go','ctx.Transaction().Get(ss.Pack(tuple.Tuple{StoreInfoKey})).Get()','ctx.Transaction().Snapshot().Get(ss.Pack(tuple.Tuple{StoreInfoKey})).Get()'),('delete-version-cleanup','store_api.go','ctx.ClearRange(fdb.KeyRange{Begin: begin, End: end})','ctx.Transaction().ClearRange(fdb.KeyRange{Begin: begin, End: end})'),('delete-invalidation','store_api.go','err != nil || header.GetCacheable()','err != nil && header.GetCacheable()')]
cmd=['bazelisk','test','//pkg/recordlayer:recordlayer_test','--nocache_test_results','--test_output=all','--test_arg=--ginkgo.focus=Store State Cache|DeleteStore']
for name,file,old,new in cases:
 p=base/file;original=p.read_bytes();text=original.decode();assert text.count(old)==1,(name,text.count(old));changed=text.replace(old,new);log=r/('cache-delete-mutant-'+name+'.log');assert not log.exists()
 backup=r/('cache-delete-original-'+name+'.bin');backup.write_bytes(original);(r/'cache-delete-mutation-active.json').write_text(json.dumps(dict(name=name,file=str(p),backup=str(backup))))
 try:
  p.write_text(changed);assert p.read_text()==changed
  with log.open('w') as f:proc=subprocess.run(cmd,stdout=f,stderr=subprocess.STDOUT)
  text=log.read_text();fail=re.findall(r'^.*\[FAIL\].*$',text,re.M);assert proc.returncode!=0 and fail and 'FAILED TO BUILD' not in text,(name,proc.returncode)
  results.append(dict(name=name,file=str(p),old=old,new=new,mutated_sha256=hashlib.sha256(p.read_bytes()).hexdigest(),log=str(log),sha256=hashlib.sha256(log.read_bytes()).hexdigest(),failures=fail,exit=proc.returncode,command=cmd));print(name,'compiled/killed',flush=True)
 finally:
  p.write_bytes(original);assert p.read_bytes()==original;(r/'cache-delete-mutation-active.json').unlink()
(r/'cache-delete-mutations.json').write_text(json.dumps(results,indent=2)+'\n')
