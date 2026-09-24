from pathlib import Path
import subprocess,json,hashlib,re
r=Path('/var/tmp/fdb-upgrade-recovery/ws-b');p=Path('pkg/recordlayer/sliding_window_index_maintainer.go');original=p.read_bytes();s=original.decode();results=[]
cases=[('tracked-read','if existing != nil {','if existing != nil && false {'),('overflow-refresh','if m.extremumType.isInWindow(entryKey, boundary) {','if m.extremumType.isInWindow(entryKey, boundary) || true {'),('delegate-refresh','return m.instrument(EventSWDelegateInsert, delegateInsert)','return m.instrument(EventSWDelegateInsert, func() error { return nil })'),('missing-boundary','if boundaryBytes == nil {\n\t\t\treturn &SlidingWindowCorruptionError','if boundaryBytes == nil && false {\n\t\t\treturn &SlidingWindowCorruptionError')]
cmd=['bazelisk','test','//pkg/recordlayer:recordlayer_test','--nocache_test_results','--test_output=all','--test_arg=--ginkgo.focus=replays tracked inserts|refuses replay of a tracked insert']
for name,old,new in cases:
 assert s.count(old)==1,(name,s.count(old));changed=s.replace(old,new);log=r/('sliding-mutant3-'+name+'.log');assert not log.exists()
 (r/'sliding-mutation-original.bin').write_bytes(original);(r/'sliding-mutation-active.json').write_text(json.dumps(dict(name=name,file=str(p),original_sha256=hashlib.sha256(original).hexdigest())))
 try:
  p.write_text(changed);assert p.read_text()==changed
  with log.open('w') as f:proc=subprocess.run(cmd,stdout=f,stderr=subprocess.STDOUT)
  text=log.read_text();fail=re.findall(r'^.*\[FAIL\].*$',text,re.M);assert proc.returncode!=0 and fail and 'FAILED TO BUILD' not in text,(name,proc.returncode)
  results.append(dict(name=name,old=old,new=new,exit=proc.returncode,failures=fail,log=str(log),sha256=hashlib.sha256(log.read_bytes()).hexdigest(),mutated_sha256=hashlib.sha256(p.read_bytes()).hexdigest(),command=cmd));print(name,'compiled/killed',flush=True)
 finally:
  p.write_bytes(original);assert p.read_bytes()==original;(r/'sliding-mutation-active.json').unlink()
(r/'sliding-mutations.json').write_text(json.dumps(results,indent=2)+'\n')
