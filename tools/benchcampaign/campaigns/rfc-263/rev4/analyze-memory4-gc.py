import json
import pathlib
import re
P = pathlib.Path('/tmp/rfc263-rev4-screen.2KEIQu')
pattern = re.compile(r'^gc (\d+) @([\d.]+)s (\d+)%[^:]*: ([\d.]+)\+([\d.]+)\+([\d.]+) ms clock, ([\d.]+)\+([\d.]+)/([\d.]+)/([\d.]+)\+([\d.]+) ms cpu, (\d+)->(\d+)->(\d+) MB, (\d+) MB goal, .*?, (\d+) P(.*)$')
result = {}
for workers, arm in ((8, 'baseline'), (8, 'reclaim'), (32, 'baseline'), (32, 'reclaim')):
    run = f'rev4-memory4-gc-w{workers}-range100-p0-{arm}'
    summary = json.loads((P / 'timed' / run / 'summary.json').read_text())
    rows = []
    for line in (P / 'timed' / (run + '.stdout.log')).read_text().splitlines():
        if not line.startswith('gc '):
            continue
        match = pattern.match(line)
        assert match, line
        g = match.groups()
        cpu = list(map(float, g[6:11]))
        rows.append(dict(cycle=int(g[0]), start_s=float(g[1]), cumulative_nonidle_available_cpu_pct=int(g[2]),
                         clock_ms=list(map(float, g[3:6])), cpu_ms=cpu,
                         gc_total_cpu_s=sum(cpu)/1000, gc_nonidle_cpu_s=(sum(cpu)-cpu[3])/1000,
                         heap_start_mb=int(g[11]), heap_end_mb=int(g[12]), live_mb=int(g[13]),
                         goal_mb=int(g[14]), procs=int(g[15]), forced='forced' in g[16]))
    before = summary['stats_before']['go_gc_cycles']
    after = summary['stats_after']['go_gc_cycles']
    window = [r for r in rows if before < r['cycle'] <= after]
    assert len(window) == after-before and len(window) > 0, (arm, before, after, len(window))
    cpu = (summary['stats_after']['process_user_cpu_us']-summary['stats_before']['process_user_cpu_us'] + summary['stats_after']['process_system_cpu_us']-summary['stats_before']['process_system_cpu_us'])/1e6
    available = summary['measured_wall_ns']/1e9*4
    total = sum(r['gc_total_cpu_s'] for r in window)
    nonidle = sum(r['gc_nonidle_cpu_s'] for r in window)
    result[f'w{workers}-{arm}'] = dict(scope='GC cycles ending inside measured window selected by NumGC snapshots. First cycle may start before the window; gctrace CPU figures are rounded. Archived binary, gctrace=1, heap profile only at end; not acceptance timing.',
                       before=before, after=after, cycles=len(window), measured_wall_s=summary['measured_wall_ns']/1e9,
                       measured_client_cpu_s=cpu, gc_total_cpu_s=total, gc_nonidle_cpu_s=nonidle,
                       gc_cpu_s_per_cycle=total/len(window), gc_total_over_client_cpu_pct=100*total/cpu,
                       gc_nonidle_over_available_cpu_pct=100*nonidle/available,
                       maximum_heap_start_mb=max(r['heap_start_mb'] for r in window), maximum_heap_end_mb=max(r['heap_end_mb'] for r in window),
                       maximum_live_mb=max(r['live_mb'] for r in window), final_live_mb=window[-1]['live_mb'],
                       maximum_goal_mb=max(r['goal_mb'] for r in window), all_cycles=rows)
(P / 'memory4-gctrace-analysis.json').write_text(json.dumps(result, indent=2))
print(json.dumps({arm: {k:v for k,v in data.items() if k != 'all_cycles'} for arm,data in result.items()}, indent=2))
