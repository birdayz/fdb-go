"""GC/heap and population diagnostics for reclaim; separate from acceptance timings."""
import importlib.util
import json
import pathlib
import subprocess
P = pathlib.Path('/tmp/rfc263-rev4-screen.2KEIQu')
spec = importlib.util.spec_from_file_location('campaign', '/tmp/rfc263-phase1.owjNRR/primary.py')
c = importlib.util.module_from_spec(spec)
spec.loader.exec_module(c)
c.E = P
c.RUNS = P / 'timed'
c.S['reclaim'] = 'tree:' + (P / 'reclaim-tree.txt').read_text().strip()
c.ENV['GODEBUG'] = 'gctrace=1'
for workers, count in ((8, 20000), (32, 500000)):
    for arm in ('baseline', 'reclaim'):
        c.one(f'rev4-memory4-gc-w{workers}', 'range100', 0, arm,
              workers=workers, profile='heap', iterations=count)
c.ENV['GODEBUG'] = ''
c.ENV['FDB_RFC263_DIAGNOSTICS'] = '1'
for arm in ('diag-baseline', 'diag-reclaim'):
    subprocess.run(['sha256sum', '-c', str(P / (arm + '-inputs.sha256'))], check=True)
    c.S[arm] = 'DIAGNOSTIC-ONLY-tree:' + (P / (arm + '-tree.txt')).read_text().strip()
    row = c.one('rev4-diagnostic-reclaim-population-w32', 'range100', 0, arm,
                workers=32, profile='none', iterations=500000)
    meta = P / 'timed' / (row['run'] + '.command.json')
    value = json.loads(meta.read_text())
    value['env']['FDB_RFC263_DIAGNOSTICS'] = '1'
    value['diagnostic_only'] = True
    value['cpu_performance_claims'] = False
    meta.write_text(json.dumps(value, indent=2))
    subprocess.run(['sha256sum', '-c', str(P / (arm + '-inputs.sha256'))], check=True)
subprocess.run(['sha256sum', '-c', str(P / 'reclaim-inputs.sha256')], check=True)
