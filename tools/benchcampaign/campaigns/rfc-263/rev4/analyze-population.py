import json
import pathlib
P = pathlib.Path('/tmp/rfc263-rev4-screen.2KEIQu')
result = {}
for arm in ('diag-baseline', 'diag'):
    file = P / 'timed' / f'rev4-diagnostic-population-w32-range100-p0-{arm}.stdout.log'
    rows = []
    for line in file.read_text().splitlines():
        try:
            row = json.loads(line)
        except ValueError:
            continue
        if row.get('event') == 'rfc263_diagnostics':
            rows.append(row)
    assert len(rows) >= 10
    first, last = rows[0], rows[-1]
    before, after = first['metrics'], last['metrics']
    gc_delta = after['/cpu/classes/gc/total:cpu-seconds'] - before['/cpu/classes/gc/total:cpu-seconds']
    idle_delta = after['/cpu/classes/gc/mark/idle:cpu-seconds'] - before['/cpu/classes/gc/mark/idle:cpu-seconds']
    total_delta = after['/cpu/classes/total:cpu-seconds'] - before['/cpu/classes/total:cpu-seconds']
    summary = dict(scope='Instrumented diagnostics only, no CPU-performance claims. One-Hz samples, not full-run or exact measured-window endpoints; queue snapshots sequential per-shard, not globally atomic. Counts are physical weak queue entries, not strongly reachable transactions.',
                   sample_count=len(rows), interval_seconds=(last['elapsed_monotonic_ns']-first['elapsed_monotonic_ns'])/1e9,
                   gc_total_cpu_s_delta=gc_delta, gc_idle_cpu_s_delta=idle_delta,
                   gc_nonidle_cpu_s_delta=gc_delta-idle_delta, available_cpu_s_delta=total_delta,
                   gc_nonidle_over_available_cpu_pct=100*(gc_delta-idle_delta)/total_delta,
                   final_cumulative_gc_fraction=last['gc_available_cpu_fraction'],
                   peak_heap_objects_bytes_1hz=max(r['metrics']['/memory/classes/heap/objects:bytes'] for r in rows),
                   peak_post_gc_live_bytes_1hz=max(r['metrics']['/gc/heap/live:bytes'] for r in rows),
                   peak_heap_goal_bytes_1hz=max(r['metrics']['/gc/heap/goal:bytes'] for r in rows))
    populations = []
    for row in rows:
        roots = [root for root in row['roots'] if root.get('role') == 'database']
        if not roots:
            continue
        assert len(roots) == 1
        populations.append(dict(seconds=row['elapsed_monotonic_ns']/1e9, **roots[0]))
    if populations:
        summary.update(peak_physical_queued_nodes_1hz=max(r['physical_queued_nodes'] for r in populations),
                       peak_heap_pointer_capacity_1hz=max(r['physical_heap_pointer_capacity'] for r in populations),
                       peak_node_weak_class_bytes_1hz=max(r['physical_node_weak_class_bytes_estimate'] for r in populations),
                       last_population=populations[-1], population_curve=populations)
    result[arm] = summary
(P / 'population-analysis.json').write_text(json.dumps(result, indent=2))
print(json.dumps({a:{k:v for k,v in x.items() if k != 'population_curve'} for a,x in result.items()}, indent=2))
