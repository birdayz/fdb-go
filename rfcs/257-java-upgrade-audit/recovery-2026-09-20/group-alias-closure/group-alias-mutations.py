from pathlib import Path
import subprocess,json,hashlib
r=Path('/var/tmp/fdb-upgrade-recovery/runtime');parser='pkg/relational/core/embedded/select_parser.go';visitor='pkg/relational/core/embedded/plan_visitor.go'
mutants=[
 ('bound-owner',parser,'ac.groupColValue == nil && ac.groupColBare != ""','ac.groupColBare != ""','//pkg/relational/core/embedded:embedded_test','^TestGroupAliasPreservesBareBoundStar$'),
 ('qualified-output',parser,' && !ac.groupColQualified {',' {','//pkg/relational/sqldriver:sqldriver_test','^TestFDB_NoFromSelectProbe$/^group_alias_unqualified_name$'),
 ('qualified-argument',parser,' && !ac.aggArgQualified && ac.aggExpr == nil',' && ac.aggExpr == nil','//pkg/relational/sqldriver:sqldriver_test','^TestFDB_NoFromSelectProbe$/^group_alias_qualified_argument$'),
 ('qualified-sort',visitor,'if bareRef && !outputAlias {','if !outputAlias {','//pkg/relational/sqldriver:sqldriver_test','^TestFDB_NoFromSelectProbe$/^group_alias_qualified_order$'),
 ('select-alias-sort',visitor,'if bareRef && !outputAlias {','if bareRef || !outputAlias {','//pkg/relational/sqldriver:sqldriver_test','^TestFDB_NoFromSelectProbe$/^group_alias_select_alias_order$'),
 ('identifier-segments',parser,'slices.EqualFunc(previous.segs, ksegs, strings.EqualFold)','slices.EqualFunc(ksegs, ksegs, strings.EqualFold)','//pkg/relational/sqldriver:sqldriver_test','^TestFDB_NoFromSelectProbe$/^group_alias_quoted_order$')]
results=[]
for name,file,old,new,target,pattern in mutants:
 p=Path(file);original=p.read_bytes();text=original.decode();assert text.count(old)==1,(name,text.count(old));changed=text.replace(old,new);log=r/('group-alias-mutant-'+name+'.log')
 try:
  p.write_text(changed);assert p.read_text()==changed and p.read_bytes()!=original
  command=['bazelisk','test',target,'--nocache_test_results','--test_output=all','--test_arg=--test.run='+pattern]
  with log.open('w') as f:run=subprocess.run(command,stdout=f,stderr=subprocess.STDOUT)
  text=log.read_text();print(name,'exit',run.returncode,'\n'+'\n'.join(l for l in text.splitlines() if '--- FAIL:' in l or 'Executed ' in l),flush=True)
  assert run.returncode!=0 and '--- FAIL:' in text and 'Executed 1 out of 1 test:' in text and 'FAILED TO BUILD' not in text,(name,log)
  results.append(dict(name=name,file=file,original_sha256=hashlib.sha256(original).hexdigest(),mutated_sha256=hashlib.sha256(changed.encode()).hexdigest(),command=command,exit=run.returncode,log_sha256=hashlib.sha256(log.read_bytes()).hexdigest()))
 finally:p.write_bytes(original)
 assert p.read_bytes()==original
(r/'group-alias-mutations.json').write_text(json.dumps(results,indent=2)+'\n')
