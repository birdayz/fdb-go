#!/usr/bin/env python3
import csv
import hashlib
import json
from pathlib import Path
import sys
p = Path(sys.argv[1])
plan = json.loads((p / 'plan.json').read_text())
schedule = json.loads((p / 'schedule.json').read_text())
complete = json.loads((p / 'complete.json').read_text())
assert plan['attribution_only'] is True and plan['blocks'] == 1 and len(plan['arms']) == 1
assert len(schedule) == len(plan['workloads']) == complete['scheduled'] == complete['completed']
pins = json.loads((p / 'binaries.json').read_text())
spec = plan['arms'][0]
assert spec['binary'] == '/opt/fdbperf/bin/candidate-hc03-base'
assert pins[spec['binary']] == '7976a1892c6de2d33d51c12ea6fd63430e0f8c78f9fb2fb82d9c24cff27f0419'
assert complete['binaries'] == pins
rows = []
profiles = []
prefixes = []
for r in schedule:
    d = p / r['run_id']
    s = json.loads((d / 'summary.json').read_text())
    m = json.loads((d / 'manifest.json').read_text())
    result = json.loads((d / 'result.json').read_text())
    gate = json.loads((d / 'quiescence.json').read_text())
    cfg = m['config']
    n, warm = r['workload']['iterations'], r['workload']['warmup']
    assert result['returncode'] == 0 and gate['passed']
    assert s['success'] and s['persisted_validation'] == 'ok'
    assert s['completed'] == s['requested'] == n and s['errors'] == s['retries'] == s['warmup_errors'] == s['warmup_retries'] == 0
    assert s['warmup_completed'] == warm == 100
    assert cfg['workload'] == r['workload']['name'] and cfg['iterations'] == n and cfg['warmup'] == warm and cfg['workers'] == 1
    assert cfg['profile'] in ['cpu', 'heap'] and cfg['profile'] == r['command'][r['command'].index('--profile')+1]
    assert cfg['source_revision'] == spec['source_revision'] and cfg['harness_id'] == plan['harness_id']
    assert cfg['transaction_timeout_ms'] == 30000 and cfg['retry_limit'] == 10 and cfg['allow_writes']
    assert m['backend']['executable'] == spec['binary'] and m['backend']['executable_sha256'] == pins[spec['binary']]
    assert m['go_version'] == 'go1.26.9' and m['gomaxprocs'] == 8
    assert not m['backend']['native_library'] and not m['backend']['loaded_fdb_libraries_sha256']
    prefix = cfg['prefix']
    assert len(prefix.encode()) == 68 and prefix not in prefixes
    prefixes.append(prefix)
    for filename, count, phase in [('samples.csv', n, 'measure'), ('warmup.csv', warm, 'warmup')]:
        with (d / filename).open() as f: raw = list(csv.DictReader(f))
        assert len(raw) == count and sorted(int(x['index']) for x in raw) == list(range(count))
        assert all(x['phase'] == phase and x['check_status'] == 'ok' and x['error'] == '' and int(x['attempts']) == 1 and int(x['worker']) == 0 and int(x['latency_ns']) > 0 for x in raw)
    delta = s['client_counters_delta']
    assert delta['observation'] == 'observed'
    c = delta['values']
    assert c['transaction_retries'] == c['grv_cache_hits'] == c['connection_failures'] == 0
    assert c['commit_started'] == c['commit_completed'] == n
    before, after = s['stats_before'], s['stats_after']
    row = {'workload': cfg['workload'], 'profile': cfg['profile'], 'run_id': r['run_id'], 'measured_transactions': n, 'warmup_transactions': warm, 'go_alloc_bytes_per_tx': (after['go_total_alloc_bytes']-before['go_total_alloc_bytes'])/n, 'go_mallocs_per_tx': (after['go_mallocs']-before['go_mallocs'])/n, 'go_gc_cycles': after['go_gc_cycles']-before['go_gc_cycles'], 'go_gc_cycles_per_tx': (after['go_gc_cycles']-before['go_gc_cycles'])/n, 'go_gc_pause_ns': after['go_gc_pause_ns']-before['go_gc_pause_ns'], 'counters': c}
    profile = d / (cfg['profile'] + '.pprof')
    assert profile.is_file() and profile.stat().st_size > 0
    profiles.append({'path': str(profile), 'sha256': hashlib.sha256(profile.read_bytes()).hexdigest()})
    rows.append(row)
report = {'scope': 'Single-arm old Go attribution only, not a benchmark comparison; root#491 CGo exception.', 'valid': True, 'per_run': rows, 'profiles': profiles}
(p / 'attribution-validation.json').write_text(json.dumps(report, indent=2) + '\n')
text = '# Old Go R2 attribution only\n\nNo performance comparison. Exact measured-phase MemStats below; heap pprof is sampled lifetime (fixture/warmup included), not these exact deltas.\n\n|Workload|Profile|Transactions|B/tx|Allocs/tx|GC cycles|GC cycles/tx|\n|---|---|---:|---:|---:|---:|---:|\n'
for r in rows:
    text += f'|{r["workload"]}|{r["profile"]}|{r["measured_transactions"]}|{r["go_alloc_bytes_per_tx"]:.3f}|{r["go_mallocs_per_tx"]:.3f}|{r["go_gc_cycles"]}|{r["go_gc_cycles_per_tx"]:.6f}|\n'
(p / 'ATTRIBUTION.md').write_text(text)
print(text)
