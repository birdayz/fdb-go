"""Classify mutex samples by exact shard/root unlock source lines, never broad stacks."""
import json
import pathlib
import re
import tarfile

P = pathlib.Path('/tmp/rfc263-rev4-screen.2KEIQu')
rows = [json.loads(line) for line in (P / 'run-metrics.jsonl').read_text().splitlines()]
result = {}
for arm in ('lazy', 'shards', 'lazy2b', 'merged', 'reclaim'):
    archive = P / (arm + '-source.tar.gz')
    if not archive.exists():
        continue
    with tarfile.open(archive) as source:
        lines = source.extractfile('pkg/fdbgo/client/timeout_queue.go').read().decode().splitlines()
    for workload in ('get100', 'set100', 'range100'):
        rawfile = P / 'pprof' / f'{arm}-{workload}-mutex-raw.txt'
        if not rawfile.exists():
            continue
        raw = rawfile.read_text()
        locations = {}
        current = None
        for line in raw.split('Locations\n', 1)[1].split('Mappings', 1)[0].splitlines():
            match = re.match(r'\s*(\d+):', line)
            if match:
                current = match[1]
                locations[current] = line
            elif current:
                locations[current] += '\n' + line
        totals = {'caller_delay_ns': 0, 'drainer_delay_ns': 0,
                  'root_drainer_delay_ns': 0, 'shard_drainer_delay_ns': 0,
                  'other_mutex_inside_queue_delay_ns': 0, 'unlock_source_lines': {}}
        for line in raw.split('Samples:', 1)[1].split('Locations', 1)[0].splitlines():
            match = re.match(r'\s*(\d+)\s+(\d+):\s*([\d ]+)$', line)
            if not match:
                continue
            names = '\n'.join(locations.get(loc, '') for loc in match[3].split())
            if 'sync.(*Mutex).Unlock' not in names:
                continue
            frames = re.findall(r'client\.\(\*(timeoutQueue(?:Root)?)\)\.(\w+) [^\n]*timeout_queue.go:(\d+):', names)
            queue_frames = [(owner, name, int(number)) for owner, name, number in frames
                            if re.search(r'\b(?:q|root)\.mu\.Unlock\(\)', lines[int(number) - 1])]
            if not queue_frames:
                if frames:
                    totals['other_mutex_inside_queue_delay_ns'] += int(match[2])
                continue
            assert len(queue_frames) == 1, queue_frames
            owner, name, number = queue_frames[0]
            key = f'{owner}.{name}:{number}'
            delay = int(match[2])
            totals['unlock_source_lines'][key] = totals['unlock_source_lines'].get(key, 0) + delay
            if name in ('schedule', 'remove'):
                totals['caller_delay_ns'] += delay
            else:
                totals['drainer_delay_ns'] += delay
                lock = 'root' if 'root.mu.Unlock()' in lines[number - 1] else 'shard'
                totals[lock + '_drainer_delay_ns'] += delay
        metric = next(row for row in rows if row['arm'] == arm and row['workload'] == workload and 'mutex-draining' in row['phase'])
        summary = json.loads((P / 'timed' / metric['run'] / 'summary.json').read_text())
        population = summary['completed']
        cpu_s = metric['cpu_us'] * population / 1e6
        wait_ns = totals['caller_delay_ns'] + totals['drainer_delay_ns']
        totals.update(population=population, measured_window_cpu_s=cpu_s,
                      queue_wait_ns=wait_ns, queue_wait_over_window_cpu_pct=100 * wait_ns / (cpu_s * 1e9),
                      scope='Whole mutex profile divided by measured-window client CPU: cumulative wait/CPU ratio, not CPU consumed. Exact q.mu and root.mu unlock source lines only.')
        assert population > 0 and wait_ns > 0 and totals['unlock_source_lines'], totals
        # Cleanup may empty all heaps before their first wake; no sampled
        # drainer contention is then a valid zero, not an empty-profile pass.
        assert totals['drainer_delay_ns'] == totals['root_drainer_delay_ns'] + totals['shard_drainer_delay_ns']
        result[f'{arm}-{workload}'] = totals
(P / 'queue-wait-analysis.json').write_text(json.dumps(result, indent=2))
print(json.dumps(result, indent=2))
