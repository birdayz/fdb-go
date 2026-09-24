import pathlib,subprocess,hashlib,json,re,collections
R=pathlib.Path('/var/tmp/fdb-upgrade-recovery/runtime');p=pathlib.Path('pkg/relational/sqldriver/stress/latency_attribution_stress_test.go');original=p.read_bytes();old=b'func validateLatencySample(s latencySample) error {\n';new=old+b'\tif s.SQL == "" {\n\t\treturn nil\n\t}\n';assert original.count(old)==1
cmd=['bazelisk','test','//pkg/relational/sqldriver/stress:stress_test','--nocache_test_results','--test_output=all','--test_arg=--test.run=^TestLatencySampleValidation$']
results=[]
try:
 p.write_bytes(original.replace(old,new));assert p.read_bytes().count(new)==1
 with (R/'latency-validation-mutant.log').open('w') as f:r=subprocess.run(cmd,stdout=f,stderr=subprocess.STDOUT)
 text=(R/'latency-validation-mutant.log').read_text();fails=re.findall(r'^\s*--- FAIL: (\S+)',text,re.M)
 results.append(dict(kind='bypass-validation',exit=r.returncode,failures=fails,mutated_sha256=hashlib.sha256(p.read_bytes()).hexdigest(),command=cmd))
 assert r.returncode!=0 and 'FAILED TO BUILD' not in text and 'FAIL: TestLatencySampleValidation/wrong_rows' in text and len(fails)==12
finally:p.write_bytes(original)
with (R/'latency-validation-restored.log').open('w') as f:r=subprocess.run(cmd,stdout=f,stderr=subprocess.STDOUT)
text=(R/'latency-validation-restored.log').read_text();runs=collections.Counter(re.findall(r'^=== RUN\s+(\S+)',text,re.M));passes=collections.Counter(re.findall(r'^\s*--- PASS: (\S+)',text,re.M))
assert r.returncode==0 and runs==passes and sum(runs.values())==13 and p.read_bytes()==original
results.append(dict(kind='restored',exit=r.returncode,runs=dict(runs),passes=dict(passes),sha256=hashlib.sha256(original).hexdigest(),command=cmd));(R/'latency-validation-mutation.json').write_text(json.dumps(results,indent=2)+'\n');print('Mutation compiled: 11 negative arms + parent failed; restored: 13 RUN/PASS')
