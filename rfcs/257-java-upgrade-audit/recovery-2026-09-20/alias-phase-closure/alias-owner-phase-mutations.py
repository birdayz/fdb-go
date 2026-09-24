import pathlib,subprocess,json,re,hashlib
ROOT=pathlib.Path('/home/birdy/projects/fdb-go'); R=pathlib.Path('/var/tmp/fdb-upgrade-recovery/runtime'); E=ROOT/'pkg/relational/core/embedded'
cases=[
 ('ordinary-eligibility',E/'logical_predicate.go','names = make([]string, len(sq.selectSlots))\n\t\t\tfor i, slot := range sq.selectSlots {\n\t\t\t\tif _, authored := slot.element.(*antlrgen.SelectExpressionElementContext); authored {\n\t\t\t\t\tnames[i] = slot.alias\n\t\t\t\t}\n\t\t\t}','names = projectionOutputNames(sq)'),
 ('early-named-duplicates',E/'select_parser.go','kb, kq, kqf, ksegs := splitColumnRef(obExpr.Expression())','for _, previous := range cls.orderBy {\n\t\t\t\t\tif previous.colName == colName {\n\t\t\t\t\t\treturn nil, api.NewErrorf(api.ErrCodeColumnAlreadyExists, "duplicate column %q in ORDER BY", colName)\n\t\t\t\t\t}\n\t\t\t\t}\n\t\t\t\tkb, kq, kqf, ksegs := splitColumnRef(obExpr.Expression())'),
 ('ignore-named-duplicates',E/'logical_predicate.go','if err := validateNamedSortColumns(sq); err != nil {','if err := validateNamedSortColumns(sq); err != nil && false {'),
]
cmd=['bazelisk','test','//pkg/relational/core/embedded:embedded_test','//pkg/relational/sqldriver:sqldriver_test','--nocache_test_results','--test_output=all','--test_arg=--test.run=^TestOrderByExactMetadata_|^TestFDB_NoFromSelectProbe$']
results=[]
for name,path,old,new in cases:
 original=path.read_bytes(); text=original.decode(); assert text.count(old)==1,(name,text.count(old)); changed=text.replace(old,new); log=R/('alias-owner-phase-mutant2-'+name+'.log'); assert not log.exists()
 try:
  path.write_text(changed); assert path.read_text()==changed and new in changed
  with log.open('w') as f:p=subprocess.run(cmd,cwd=ROOT,stdout=f,stderr=subprocess.STDOUT)
  data=log.read_text(); failures=re.findall(r'^\s*--- FAIL: (.+)$',data,re.M); assert p.returncode!=0 and failures and 'FAILED TO BUILD' not in data,(name,p.returncode)
  results.append(dict(name=name,file=str(path.relative_to(ROOT)),old=old,new=new,mutation_sha256=hashlib.sha256(path.read_bytes()).hexdigest(),command=cmd,exit=p.returncode,failures=failures,log=str(log),sha256=hashlib.sha256(log.read_bytes()).hexdigest()))
  print(name,'compiled/killed',failures,flush=True)
 finally:
  path.write_bytes(original); assert path.read_bytes()==original
(R/'alias-owner-phase-mutations.json').write_text(json.dumps(results,indent=2)+'\n')
