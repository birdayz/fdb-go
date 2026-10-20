"""Instrumented diagnostic builds only: no CPU-performance claims from these runs."""
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
c.ENV['FDB_RFC263_DIAGNOSTICS'] = '1'
for arm in ('diag-baseline', 'diag'):
    subprocess.run(['sha256sum', '-c', str(P / (arm + '-inputs.sha256'))], check=True)
    c.S[arm] = 'DIAGNOSTIC-ONLY-tree:' + (P / (arm + '-tree.txt')).read_text().strip()
    row = c.one('rev4-diagnostic-population-w32', 'range100', 0, arm,
                workers=32, profile='none', iterations=500000)
    meta = P / 'timed' / (row['run'] + '.command.json')
    value = json.loads(meta.read_text())
    value['env']['FDB_RFC263_DIAGNOSTICS'] = '1'
    value['diagnostic_only'] = True
    value['cpu_performance_claims'] = False
    meta.write_text(json.dumps(value, indent=2))
    subprocess.run(['sha256sum', '-c', str(P / (arm + '-inputs.sha256'))], check=True)
