from pathlib import Path
import subprocess
p=Path('pkg/recordlayer/index_scan.go')
s=p.read_text()
a='if c.hasNoNext && !c.closed {'
b='if false && c.hasNoNext && !c.closed {'
assert s.count(a)==1
try:
 p.write_text(s.replace(a,b))
 assert p.read_text().count(b)==1
 with open('/var/tmp/fdb-upgrade-recovery/ws-b/rank-terminal-mutation.log','w') as out:
  out.write('Mutation verified present: terminal replay disabled\n'); out.flush()
  r=subprocess.run(['bazelisk','test','//pkg/recordlayer:recordlayer_test','--nocache_test_results','--test_output=all','--test_arg=--ginkgo.focus=Rank-valued scans'],stdout=out,stderr=subprocess.STDOUT)
 log=Path(out.name).read_text()
 assert r.returncode!=0 and '[FAIL] Rank-valued scans' in log and 'Ran 8 of' in log, log[-4000:]
 print('Compiled terminal-replay mutant killed by rank regression')
finally:
 p.write_text(s)
 assert p.read_text()==s
