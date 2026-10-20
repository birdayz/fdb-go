"""Archived-binary GC/heap diagnostics, separate from CPU profiles and acceptance runs."""
import importlib.util
import pathlib
import subprocess
P = pathlib.Path('/tmp/rfc263-rev4-screen.2KEIQu')
spec = importlib.util.spec_from_file_location('campaign', '/tmp/rfc263-phase1.owjNRR/primary.py')
c = importlib.util.module_from_spec(spec)
spec.loader.exec_module(c)
c.E = P
c.RUNS = P / 'timed'
c.S['merged'] = 'tree:' + (P / 'merged-tree.txt').read_text().strip()
c.ENV['GODEBUG'] = 'gctrace=1'
for arm in ('baseline', 'merged'):
    c.one('rev4-attribution-gc-heap-w32', 'range100', 0, arm,
          workers=32, profile='heap', iterations=500000)
subprocess.run(['sha256sum', '-c', str(P / 'merged-inputs.sha256')], check=True)
