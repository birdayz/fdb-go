import pathlib,subprocess,json,re,hashlib
ROOT=pathlib.Path('/home/birdy/projects/fdb-go'); R=pathlib.Path('/var/tmp/fdb-upgrade-recovery/runtime'); E=ROOT/'pkg/relational/core/embedded'
cases=[
 ('quoted-union-identity',(E/'logical_predicate.go').resolve(),'key := label\n\t\tmatches[key]++','key := strings.ToUpper(label)\n\t\tmatches[key]++'),
 ('quoted-select-identity',(E/'logical_builder.go').resolve(),'if alias != "" && alias == name {','if alias != "" && strings.EqualFold(alias, name) {'),
 ('quoted-duplicate-identity',(E/'logical_predicate.go').resolve(),'if err == nil && previousName == name &&\n\t\t\t\tslices.Equal(previousSegments, segments) {','if err == nil && strings.EqualFold(previousName, name) &&\n\t\t\t\tslices.EqualFunc(previousSegments, segments, strings.EqualFold) {'),
 ('resolved-attribute-qualification',(E/'logical_predicate.go').resolve(),'if len(segments) == 0 || resolver == nil || resolver.Scope() == nil {','if len(segments) != 1 || resolver == nil || resolver.Scope() == nil {'),
 ('record-qov-projection',(E/'plan_visitor.go').resolve(),'if qov, ok := values.AsQuantifiedObjectValue(rv); ok {','if qov, ok := values.AsQuantifiedObjectValue(rv); ok && qov.FlowedType().Code() != values.TypeCodeRecord {'),
 ('union-cardinality',(E/'logical_predicate.go').resolve(),'if count > 1 {','if count > 2 {'),
 ('lifted-star-width',(E/'logical_predicate.go').resolve(),'if simpleTable.GroupByClause() != nil || hasPositionalOrderBy(simpleTable) || hasMixedSelectStar(simpleTable) {','if simpleTable.GroupByClause() != nil || hasMixedSelectStar(simpleTable) {'),
 ('normalization-row-names',(E/'../query/cascades_translator.go').resolve(),'for i, name := range values.DedupFieldNames(names) {\n\t\tfields[i].Name = name\n\t}\n\treturn commonRow, true, nil','for i, name := range names {\n\t\tfields[i].Name = name\n\t}\n\treturn commonRow, true, nil'),
 ('aligned-row-preservation',(E/'../query/cascades_translator.go').resolve(),'if aligned {','if aligned && false {'),
]
cmd=['bazelisk','test','//pkg/relational/core/embedded:embedded_test','//pkg/relational/sqldriver:sqldriver_test','--nocache_test_results','--test_output=all','--test_arg=--test.run=^TestOrderByExactMetadata_|^TestUnionExactOutputContract_|^TestFDB_NoFromSelectProbe$']
results=[]
for name,path,old,new in cases:
 original=path.read_bytes(); text=original.decode(); assert text.count(old)==1,(name,text.count(old)); changed=text.replace(old,new); log=R/('alias-case-mutant-'+name+'.log'); assert not log.exists()
 try:
  backup=R/('alias-case-original-'+name+'.bin'); backup.write_bytes(original)
  (R/'alias-case-mutation-active.json').write_text(json.dumps(dict(name=name,file=str(path),backup=str(backup),original_sha256=hashlib.sha256(original).hexdigest())))
  path.write_text(changed); assert path.read_text()==changed and new in changed
  with log.open('w') as f:p=subprocess.run(cmd,cwd=ROOT,stdout=f,stderr=subprocess.STDOUT)
  data=log.read_text(); failures=re.findall(r'^\s*--- FAIL: (.+)$',data,re.M); assert p.returncode!=0 and failures and 'FAILED TO BUILD' not in data,(name,p.returncode)
  results.append(dict(name=name,file=str(path.relative_to(ROOT)),old=old,new=new,mutation_sha256=hashlib.sha256(path.read_bytes()).hexdigest(),command=cmd,exit=p.returncode,failures=failures,log=str(log),sha256=hashlib.sha256(log.read_bytes()).hexdigest()))
  print(name,'compiled/killed',failures,flush=True)
 finally:
  path.write_bytes(original); assert path.read_bytes()==original
  (R/'alias-case-mutation-active.json').unlink(missing_ok=True)
(R/'alias-case-mutations.json').write_text(json.dumps(results,indent=2)+'\n')
