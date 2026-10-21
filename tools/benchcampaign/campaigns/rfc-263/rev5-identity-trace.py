"""Separate instrumented identity-detector run; never qualification timing."""
import importlib.util, json, pathlib, subprocess, sys
P=pathlib.Path(sys.argv[1])
spec=importlib.util.spec_from_file_location('primary',pathlib.Path(__file__).with_name('rev5-primary.py'))
c=importlib.util.module_from_spec(spec); spec.loader.exec_module(c)
c.E=P; c.RUNS=P/'runs'; c.S={'identity':'tree:'+(P/'identity-tree.txt').read_text().strip()}
expected=(P/'identity-binary.sha256').read_text().split()[0]
assert c.sha(P/'bin'/'identity')==expected
name='tail-diag-trace-range100-p0-identity'
with (c.RUNS/(name+'.uptime-before.txt')).open('w') as f: subprocess.run(['uptime'],stdout=f,check=True)
try: row=c.one('tail-diag-trace','range100',0,'identity',workers=32,iterations=500000,profile='trace')
finally:
    with (c.RUNS/(name+'.uptime-after.txt')).open('w') as f: subprocess.run(['uptime'],stdout=f,check=True)
assert c.sha(P/'bin'/'identity')==expected
(P/'identity-trace-row.json').write_text(json.dumps(row,indent=2)+'\n')
