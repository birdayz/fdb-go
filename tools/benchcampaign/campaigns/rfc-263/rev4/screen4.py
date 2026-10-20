"""Run only after root explicitly releases the shared-machine timing window."""
import argparse
import hashlib
import importlib.util
import json
import pathlib
import statistics
import subprocess
import time

P = pathlib.Path('/tmp/rfc263-rev4-screen.2KEIQu')
parser = argparse.ArgumentParser()
parser.add_argument('--release-note', required=True)
args = parser.parse_args()
(P / 'timing-screen4-note.json').write_text(json.dumps({
    'release_note': args.release_note,
    'start_utc': time.strftime('%Y-%m-%dT%H:%M:%SZ', time.gmtime()),
    'prior_attempts': ['discarded-local-rebuild-attempt1', 'discarded-early-sequence-attempt2'],
}, indent=2))
subprocess.run(['sha256sum', '-c', str(P / 'reclaim-inputs.sha256')], check=True)
patch = subprocess.check_output(['git', '-C', str(P / 'reclaim'), 'diff', 'HEAD', '--binary'])
assert patch == (P / 'reclaim.patch').read_bytes(), 'Frozen prototype source changed'
assert hashlib.sha256((P / 'bin/baseline').read_bytes()).hexdigest() == '2dd943fdc9d58ff136449a181189ba5843465e86e6ae696d616a9b1bac4a439e'
spec = importlib.util.spec_from_file_location('campaign', '/tmp/rfc263-phase1.owjNRR/primary.py')
c = importlib.util.module_from_spec(spec)
spec.loader.exec_module(c)
c.E = P
c.RUNS = P / 'timed'
c.S['reclaim'] = 'tree:' + (P / 'reclaim-tree.txt').read_text().strip()
rows = []
for workload in ('get100', 'set100', 'range100'):
    for pair in range(3):
        for arm in (('baseline', 'reclaim') if pair % 2 == 0 else ('reclaim', 'baseline')):
            rows.append(c.one('rev4-screen4-w8', workload, pair, arm))
result = {}
for workload in ('get100', 'set100', 'range100'):
    groups = {arm: [r for r in rows if r['workload'] == workload and r['arm'] == arm]
              for arm in ('baseline', 'reclaim')}
    result[workload] = {
        arm: {key: {'median': statistics.median(r[key] for r in rs),
                    'minimum': min(r[key] for r in rs),
                    'maximum': max(r[key] for r in rs)}
              for key in ('cpu_us', 'allocs', 'bytes', 'mean_us', 'p99_us')}
        for arm, rs in groups.items()}
    result[workload]['paired_cpu_ratios'] = [
        next(r['cpu_us'] for r in groups['reclaim'] if r['pair'] == i) /
        next(r['cpu_us'] for r in groups['baseline'] if r['pair'] == i)
        for i in range(3)]
(P / 'screen4-analysis.json').write_text(json.dumps(result, indent=2))
print(json.dumps(result, indent=2), flush=True)
for workload, count in (('get100', 1000000), ('set100', 400000), ('range100', 500000)):
    for arm in ('baseline', 'reclaim'):
        c.one('rev4-mutex-draining4-w32', workload, 0, arm,
              workers=32, profile='mutex', iterations=count)
for pair in range(3):
    for arm in (('baseline', 'reclaim') if pair % 2 == 0 else ('reclaim', 'baseline')):
        c.one('rev4-range4-w32', 'range100', pair, arm, workers=32, iterations=500000)
assert subprocess.check_output(['git', '-C', str(P / 'reclaim'), 'diff', 'HEAD', '--binary']) == patch
subprocess.run(['sha256sum', '-c', str(P / 'reclaim-inputs.sha256')], check=True)
