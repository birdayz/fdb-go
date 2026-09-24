from pathlib import Path
import subprocess,json
root=Path('/var/tmp/fdb-upgrade-recovery/ws-b')
scan=Path('pkg/recordlayer/rank_scan.go')
maint=Path('pkg/recordlayer/rank_index_maintainer.go')
cases=[
 ('omit-enrichment',scan,'if bounds.IncludeRankAsValue {','if !bounds.IncludeRankAsValue {'),
 ('append-pk-to-score',scan,'entry.Key[:columns], true','entry.Key, true'),
 ('missing-rank-empty',scan,'enriched.Value = tuple.Tuple{nil}','enriched.Value = tuple.Tuple{}'),
 ('offset-absolute-rank',scan,'enriched.Value[0] = *rank','enriched.Value[0] = *rank + 1'),
 ('ignore-unreadable',scan,'if !state.IsScannable() {','if false && !state.IsScannable() {'),
 ('snapshot-rank-read',maint,'rankedSet.Rank(m.tx, scoreTuple.Pack(), nullIfMissing)','rankedSet.Rank(m.tx.Snapshot(), scoreTuple.Pack(), nullIfMissing)'),
 ('serializable-preload',maint,'rankedSet.PreloadForLookup(m.tx.Snapshot())\n\treturn rankedSet.Rank', 'rankedSet.PreloadForLookup(m.tx)\n\treturn rankedSet.Rank'),
 ('omit-instrumentation',scan,'return instrumentCursor(store.context.Timer(), EventScanIndex, cursor)','return cursor'),
]
results=[]
for name,p,a,b in cases:
 original=p.read_text();assert original.count(a)==1,(name,original.count(a))
 try:
  p.write_text(original.replace(a,b));assert p.read_text().count(b)==1
  log=root/('rank-mutant-'+name+'.log')
  with log.open('w') as f:
   f.write('Verified mutation present: '+name+'\n');f.flush()
   r=subprocess.run(['bazelisk','test','//pkg/recordlayer:recordlayer_test','--nocache_test_results','--test_output=all','--test_arg=--ginkgo.focus=Rank-valued scans'],stdout=f,stderr=subprocess.STDOUT)
  text=log.read_text(); killed=r.returncode!=0 and 'Ran 10 of' in text and '[FAIL] Rank-valued scans' in text
  results.append({'name':name,'compiled_and_killed':killed,'exit':r.returncode})
  (root/'rank-mutations.json').write_text(json.dumps(results,indent=2)+'\n')
  assert killed,text[-4000:]
  print(name, 'compiled and killed',flush=True)
 finally:
  p.write_text(original);assert p.read_text()==original
