"""Untimed attribution runs only; never pooled into the acceptance screen."""
import importlib.util
import pathlib
import subprocess
P = pathlib.Path('/tmp/rfc263-rev4-screen.2KEIQu')
subprocess.run(['sha256sum', '-c', str(P / 'merged-inputs.sha256')], check=True)
spec = importlib.util.spec_from_file_location('campaign', '/tmp/rfc263-phase1.owjNRR/primary.py')
c = importlib.util.module_from_spec(spec)
spec.loader.exec_module(c)
c.E = P
c.RUNS = P / 'timed'
c.S['merged'] = 'tree:' + (P / 'merged-tree.txt').read_text().strip()
for arm in ('baseline', 'merged'):
    c.one('rev4-attribution-cpu-w32', 'range100', 0, arm,
          workers=32, profile='cpu', iterations=500000)
subprocess.run(['sha256sum', '-c', str(P / 'merged-inputs.sha256')], check=True)
