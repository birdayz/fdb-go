from pathlib import Path
import subprocess,json,os
base=Path('/var/tmp/fdb-upgrade-recovery/ws-b')
mutations=[
 ('smallest-key','metadata.go','if int(field.Number()) < existing.RecordTypeIndex {','if int(field.Number()) > existing.RecordTypeIndex {'),
 ('canonical-tag','metadata.go','canonical := protoreflect.Name("_" + recordTypeName)','canonical := protoreflect.Name("never_" + recordTypeName)'),
 ('alias-map','metadata.go','if field.Message() == rt.Descriptor {','if field.Message() == rt.Descriptor && field.Number() == rt.unionFieldNumber {'),
 ('factory','metadata.go','err != nil || msgType.Descriptor() != rt.Descriptor','err != nil'),
 ('known-alias','store.go','store.metaData.fieldNumberToRecordType[fieldNum] != recordType','fieldNum != recordType.unionFieldNumber'),
 ('pair-memo','metadata_evolution_validator.go','pair := descriptorPair{old: oldDesc, new: newDesc}','pair := descriptorPair{old: oldDesc, new: oldDesc}'),
 ('name-identity','metadata_evolution_validator.go','oldName, newName := string(oldDesc.Name()), string(newDesc.Name())','oldName, newName := string(oldDesc.Name()), string(newDesc.Name()); if oldName != newName { newName = oldName }'),
 ('since-waiver','metadata_evolution_validator.go','if rt.SinceVersion <= old.Version() {','if rt.SinceVersion == 0 && v.allowNoSinceVersion { continue }; if rt.SinceVersion <= old.Version() {'),
]
results=[]
for name,file,old,new in mutations:
 p=Path('pkg/recordlayer')/file;original=p.read_text()
 try:
  assert original.count(old)==1,(name,original.count(old))
  p.write_text(original.replace(old,new));assert new in p.read_text()
  subprocess.run([os.environ['GOFUMPT'],'-w',str(p)],check=True)
  log=base/('union-mutant-'+name+'.log')
  with log.open('w') as f:
   r=subprocess.run(['bazelisk','test','//pkg/recordlayer:recordlayer_test','--nocache_test_results','--test_output=all','--test_arg=-test.run=TestRecordLayer|TestUnion(Evolution|Alias)','--test_arg=--ginkgo.focus=Union identity persistence|requires newer index scope','--test_arg=-test.v'],stdout=f,stderr=subprocess.STDOUT)
  text=log.read_text();compiled='=== RUN   TestUnionEvolutionUsesTagIdentity' in text
  killed=compiled and r.returncode!=0 and ('--- FAIL: TestUnion' in text or 'FAIL! --' in text)
  results.append(dict(name=name,compiled=compiled,killed=killed,exit=r.returncode,log=log.name))
  (base/'union-mutations.json').write_text(json.dumps(results,indent=2)+'\n')
  print(results[-1],flush=True);assert killed,name
 finally:p.write_text(original)
