import json
import math
import random
import statistics
import sys
from collections import defaultdict

rows = [json.loads(line) for line in open(sys.argv[1])]
variants = sys.argv[2:] or ['noack']
cells = defaultdict(dict)
for row in rows:
    arm, block = row['label'].rsplit('-b', 1)
    key = (row['workload'], row['workers'])
    slot = (arm, int(block))
    if slot in cells[key]:
        raise ValueError(f'duplicate {key} {slot}')
    cells[key][slot] = row
print(f'completed configurations: {len(rows)}')
for cell, observations in cells.items():
    print('\n', cell)
    for arm in ['base', *variants, 'cgo']:
        sample = [r for (a, _), r in observations.items() if a == arm]
        if not sample:
            continue
        print(arm, 'n=', len(sample), ' '.join(f'{m}={statistics.median(r[m] for r in sample):.4f}' for m in ['server_cycles', 'server_syscalls', 'client_cpu_us', 'mallocs', 'gc_cycles', 'mean_latency_us', 'tx_per_s']), 'arithmetic_mean_latency_us=', statistics.fmean(r['mean_latency_us'] for r in sample))
    for arm in variants:
        blocks = sorted(b for a, b in observations if a == arm and ('base', b) in observations)
        for metric in ['server_cycles', 'client_cpu_us', 'mean_latency_us', 'server_syscalls']:
            logs = [math.log(observations[arm, b][metric] / observations['base', b][metric]) for b in blocks]
            if not logs:
                continue
            rng = random.Random(20261017)
            boot = sorted(math.exp(statistics.fmean(logs[rng.randrange(len(logs))] for _ in logs)) for _ in range(10000))
            ratio = math.exp(statistics.fmean(logs))
            print(f'{arm}/base {metric} pairs={len(logs)} ratio={ratio:.6f} CI95=[{boot[249]:.6f}, {boot[9749]:.6f}]')
