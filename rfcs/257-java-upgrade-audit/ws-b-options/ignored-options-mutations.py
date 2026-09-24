from pathlib import Path
import subprocess,json,os
root=Path.cwd(); base=Path('/var/tmp/fdb-upgrade-recovery/ws-b')
p=root/'pkg/recordlayer/metadata_evolution_validator.go'; original=p.read_text()
mutations=[
 ('build-copy','v.ignoredIndexOptions = maps.Clone(b.v.ignoredIndexOptions)','v.ignoredIndexOptions = b.v.ignoredIndexOptions'),
 ('builder-copy','copy.ignoredIndexOptions = maps.Clone(v.ignoredIndexOptions)','copy.ignoredIndexOptions = v.ignoredIndexOptions'),
 ('nil-clears','b.v.ignoredIndexOptions = make(map[string]bool, len(options))','if options == nil { return b }; b.v.ignoredIndexOptions = make(map[string]bool, len(options))'),
 ('filter','delete(changed, option)','delete(changed, option+"__not_ignored")'),
 ('supplied-set','func ValidateChangedIndexOptions(oldIdx, newIdx *Index, changed map[string]bool) error {','func ValidateChangedIndexOptions(oldIdx, newIdx *Index, changed map[string]bool) error {\nif len(changed) > 0 { changed = computeChangedOptions(oldIdx.Options, newIdx.Options) }'),
 ('tokenizer-default','if oldTokenizer.Name() != newTokenizer.Name() {','if oldTokenizer.Name() != newTokenizer.Name() || oldIdx.Options[IndexOptionTextTokenizerName] != newIdx.Options[IndexOptionTextTokenizerName] {'),
]
results=[]
try:
 for name,old,new in mutations:
  assert original.count(old)==1,(name,original.count(old))
  p.write_text(original.replace(old,new))
  assert new in p.read_text()
  subprocess.run([os.environ['GOFUMPT'],'-w',str(p)],check=True)
  log=base/('ignored-options-mutant-'+name+'.log')
  with log.open('w') as f:
   r=subprocess.run(['bazelisk','test','//pkg/recordlayer:recordlayer_test','--nocache_test_results','--test_output=all','--test_arg=--ginkgo.focus=ignored index options|persists ignored index options'],stdout=f,stderr=subprocess.STDOUT)
  text=log.read_text(); compiled='Ran 9 of 3330 Specs' in text
  killed=r.returncode!=0 and compiled and 'FAIL! --' in text
  results.append(dict(name=name,exit=r.returncode,compiled=compiled,killed=killed,log=log.name))
  (base/'ignored-options-mutations.json').write_text(json.dumps(results,indent=2)+'\n')
  print(results[-1],flush=True)
  assert killed,name
  p.write_text(original)
finally:
 p.write_text(original)
