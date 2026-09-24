from pathlib import Path
import subprocess,json,os
root=Path.cwd(); base=Path('/var/tmp/fdb-upgrade-recovery/ws-b')
p=root/'pkg/recordlayer/metadata.go'; original=p.read_text()
mutations=[
 ('non-string','if field.Kind() != protoreflect.StringKind {','if field.Kind() == protoreflect.BytesKind {'),
 ('repeated','if field.IsList() {','if field.IsMap() {'),
 ('group-position','position := textFieldPosition(idx.RootExpression)','position := 0'),
 ('universal', 'for name, rt := range b.recordTypes {\n\t\t\tif rt.Descriptor != nil {\n\t\t\t\tfields, err := validateKeyExpressionFields(idx.RootExpression, rt.Descriptor)', 'for name, rt := range b.recordTypes {\n\t\t\tif rt.Descriptor != nil {\n\t\t\t\tfields, err := validateKeyExpressionFields(idx.RootExpression, rt.Descriptor)\n\t\t\t\tif idx.Type == IndexTypeText { continue }'),
]
results=[]
try:
 for name,old,new in mutations:
  assert original.count(old)==1,(name,original.count(old))
  p.write_text(original.replace(old,new)); assert new in p.read_text()
  subprocess.run([os.environ['GOFUMPT'],'-w',str(p)],check=True)
  log=base/('text-body-mutant-'+name+'.log')
  with log.open('w') as f:
   r=subprocess.run(['bazelisk','test','//pkg/recordlayer:recordlayer_test','--nocache_test_results','--test_output=all','--test_arg=--ginkgo.focus=validates the text body descriptor'],stdout=f,stderr=subprocess.STDOUT)
  text=log.read_text(); compiled='Ran 1 of 3330 Specs' in text
  killed=r.returncode!=0 and compiled and 'FAIL! --' in text
  results.append(dict(name=name,exit=r.returncode,compiled=compiled,killed=killed,log=log.name))
  (base/'text-body-mutations.json').write_text(json.dumps(results,indent=2)+'\n')
  print(results[-1],flush=True); assert killed,name
  p.write_text(original)
finally:
 p.write_text(original)
