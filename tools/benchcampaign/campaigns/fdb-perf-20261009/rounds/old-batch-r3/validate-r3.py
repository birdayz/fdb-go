#!/usr/bin/env python3
import csv
import json
import math
from pathlib import Path
import statistics
import sys
p = Path(sys.argv[1])
plan = json.loads((p / 'plan.json').read_text())
schedule = json.loads((p / 'schedule.json').read_text())
complete = json.loads((p / 'complete.json').read_text())
blocks = 1 if plan['diagnostic_only'] else 6
assert plan['blocks'] == blocks and {a['name'] for a in plan['arms']} == {'old', 'cgo'}
assert len(schedule) == complete['scheduled'] == complete['completed'] == 2 * blocks
pins = json.loads((p / 'binaries.json').read_text())
assert complete['binaries'] == pins
assert pins['/opt/fdbperf/bin/candidate-hc03-base'] == '7976a1892c6de2d33d51c12ea6fd63430e0f8c78f9fb2fb82d9c24cff27f0419'
assert pins['/opt/fdbperf/bin/candidate-v6-cgo'] == 'cd762ea429647784378270613f404573ba598a5049f422e055a887f7746ecbdb'
rows = []
prefixes = []
for r in schedule:
    d = p / r['run_id']
    s = json.loads((d / 'summary.json').read_text())
    m = json.loads((d / 'manifest.json').read_text())
    result = json.loads((d / 'result.json').read_text())
    gate = json.loads((d / 'quiescence.json').read_text())
    cfg = m['config']
    spec = next(a for a in plan['arms'] if a['name'] == r['arm'])
    n, warm = r['workload']['iterations'], r['workload']['warmup']
    assert n == 3000 and warm == 200
    assert result['returncode'] == 0 and gate['passed']
    assert s['success'] and s['persisted_validation'] == 'ok'
    assert s['completed'] == s['requested'] == n and s['errors'] == s['retries'] == s['warmup_errors'] == s['warmup_retries'] == 0
    assert s['warmup_completed'] == warm
    assert cfg['workload'] == 'batchset50' and cfg['iterations'] == n and cfg['warmup'] == warm and cfg['workers'] == 1 and cfg['profile'] == 'none'
    assert cfg['source_revision'] == spec['source_revision'] and cfg['harness_id'] == plan['harness_id']
    assert cfg['transaction_timeout_ms'] == 30000 and cfg['retry_limit'] == 10 and cfg['allow_writes']
    assert m['backend']['executable'] == spec['binary'] and m['backend']['executable_sha256'] == pins[spec['binary']]
    assert m['go_version'] == 'go1.26.9' and m['gomaxprocs'] == 8
    prefix = cfg['prefix']
    assert len(prefix.encode()) == 68 and prefix not in prefixes
    prefixes.append(prefix)
    measured = []
    for filename, count, phase in [('samples.csv', n, 'measure'), ('warmup.csv', warm, 'warmup')]:
        with (d / filename).open() as f: raw = list(csv.DictReader(f))
        assert len(raw) == count and sorted(int(x['index']) for x in raw) == list(range(count))
        assert all(x['phase'] == phase and x['check_status'] == 'ok' and x['error'] == '' and int(x['attempts']) == 1 and int(x['worker']) == 0 and int(x['latency_ns']) > 0 for x in raw)
        if phase == 'measure': measured = sorted(int(x['latency_ns']) for x in raw)
    delta = s['client_counters_delta']
    if r['arm'] == 'old':
        assert delta['observation'] == 'observed' and not m['backend']['native_library'] and not m['backend']['loaded_fdb_libraries_sha256']
        c = delta['values']
        assert c['transaction_retries'] == c['grv_cache_hits'] == c['connection_failures'] == 0
        assert c['commit_started'] == c['commit_completed'] == c['read_versions_completed'] == n
    else:
        assert delta['observation'] == 'unobserved' and m['backend']['native_library']
        assert list(m['backend']['loaded_fdb_libraries_sha256'].values()) == ['e4ad7fed7b7580bf5f7b38f0a4279d37e698f69d534182e5de0b8beeef6cd1b1']
    before, after = s['stats_before'], s['stats_after']
    user = (after['process_user_cpu_us'] - before['process_user_cpu_us']) / n
    system = (after['process_system_cpu_us'] - before['process_system_cpu_us']) / n
    row = {'run_id': r['run_id'], 'block': r['block'], 'position': r['position'], 'arm': r['arm'], 'measured_tx': n, 'warmup_tx': warm, 'mean_us': statistics.mean(measured)/1000, 'p99_us': measured[math.ceil(.99*n)-1]/1000, 'cpu_us_per_tx': user+system, 'user_us_per_tx': user, 'system_us_per_tx': system, 'go_B_per_tx': (after['go_total_alloc_bytes']-before['go_total_alloc_bytes'])/n, 'go_allocs_per_tx': (after['go_mallocs']-before['go_mallocs'])/n, 'go_gc_cycles': after['go_gc_cycles']-before['go_gc_cycles'], 'go_gc_cycles_per_tx': (after['go_gc_cycles']-before['go_gc_cycles'])/n, 'go_gc_pause_ns': after['go_gc_pause_ns']-before['go_gc_pause_ns']}
    rows.append(row)
by = {(r['block'], r['arm']): r for r in rows}
assert len(by) == 2 * blocks
report = {'valid': True, 'scope': plan['interpretation'], 'per_run': rows, 'prefixes': prefixes}
if not plan['diagnostic_only']:
    orders = [tuple(r['arm'] for r in schedule if r['block'] == b) for b in range(blocks)]
    assert orders.count(('old', 'cgo')) == orders.count(('cgo', 'old')) == 3
    metrics = ['mean_us', 'p99_us', 'cpu_us_per_tx', 'user_us_per_tx', 'system_us_per_tx']
    ratios = {k: [by[b, 'old'][k]/by[b, 'cgo'][k] for b in range(blocks)] for k in metrics}
    gm = {k: math.exp(statistics.mean(math.log(v) for v in values)) for k, values in ratios.items()}
    report.update(paired_ratios=ratios, paired_geomean=gm, orders=orders)
    text = '# R3 longer-run batchset50 — unprofiled\n\nExisting old/CGo binaries,3000 measured transactions+200 warmup/run,one worker,six counterbalanced blocks. Absolute values are medians of six run statistics; paired ratios are within-block geometric means, NOT ratios of medians. CPU includes process user+system time.\n\n|Arm|Mean µs|p99 µs|CPU µs/tx|User µs/tx|System µs/tx|Go GC cycles total|Go GC cycles/tx aggregate|\n|---|---:|---:|---:|---:|---:|---:|---:|\n'
    for arm in ['cgo', 'old']:
        rr = [r for r in rows if r['arm'] == arm]
        text += f'|{arm}|' + '|'.join(f'{statistics.median(r[k] for r in rr):.3f}' for k in metrics) + f'|{sum(r["go_gc_cycles"] for r in rr)}|{sum(r["go_gc_cycles"] for r in rr)/sum(r["measured_tx"] for r in rr):.6f}|\n'
    text += '\n|Ratio|Mean|p99|CPU|User|System|\n|---|---:|---:|---:|---:|---:|\n|old/cgo|' + '|'.join(f'{gm[k]:.6f}' for k in metrics) + '|\n'
    text += '\n|Block|Order|Mean ratio|p99 ratio|CPU ratio|User ratio|System ratio|Old Go GC/tx|CGo Go GC/tx|\n|---|---|---:|---:|---:|---:|---:|---:|---:|\n'
    for b in range(blocks):
        text += f'|{b}|{"/".join(orders[b])}|' + '|'.join(f'{ratios[k][b]:.6f}' for k in metrics) + f'|{by[b,"old"]["go_gc_cycles_per_tx"]:.6f}|{by[b,"cgo"]["go_gc_cycles_per_tx"]:.6f}|\n'
    text += '\nSix-block exploratory single-VM result, not capacity/noninferiority/global-native claims. Longer duration and warmup are changed conditions, not a causal isolation of GC/warmup. Native internal counters are unobserved. Go GC/MemStats values include only the Go heap even in CGo, not the native allocator. Source/binary/byte checks are not main-module correctness approval.\n'
    (p / 'R3-TABLES.md').write_text(text)
    print(text)
(p / 'r3-validation.json').write_text(json.dumps(report, indent=2) + '\n')
with (p / 'r3-per-run.csv').open('w') as f:
    writer = csv.DictWriter(f, fieldnames=list(rows[0]));writer.writeheader();writer.writerows(rows)
print(f'Validated {len(rows)} runs; diagnostic_only={plan["diagnostic_only"]}')
