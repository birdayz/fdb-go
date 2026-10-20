import csv,itertools,json,math,statistics,sys
from pathlib import Path
p=Path(sys.argv[1]);a=json.loads((p/'analysis.json').read_text());plan=json.loads((p/'plan.json').read_text());schedule=json.loads((p/'schedule.json').read_text());pins=json.loads((p/'binaries.json').read_text())
assert a['valid_zero_error_campaign'] and plan['blocks']==6
assert {x['name'] for x in plan['arms']}=={'old','new','cgo'}
assert len(schedule)==len(a['per_run'])==18*len(plan['workloads'])
report=dict(campaign=plan['campaign'],workloads={},prefixes=[],telemetry={},scope=plan['purpose'])
metrics=['mean_ns','p50_ns','p99_ns','process_cpu_ns_per_op']
memmetrics=['go_alloc_bytes_per_op','go_mallocs_per_op']
absolute=['|Workload|Arm|Mean µs/op|p99 µs/op|CPU µs/op|','|---|---|---:|---:|---:|']
paired=['|Workload|Ratio|Mean|p99|CPU/op|','|---|---|---:|---:|---:|']
for workload_cfg in plan['workloads']:
 workload=workload_cfg['name'];n=workload_cfg['iterations'];warm=workload_cfg['warmup']
 orders=[tuple(r['arm'] for r in schedule if r['block']==b and r['workload']['name']==workload) for b in range(6)]
 assert set(orders)==set(itertools.permutations(['old','new','cgo']))
 rows=[r for r in a['per_run'] if r['workload']==workload];by={(r['block'],r['arm']):r for r in rows}
 wr=dict(orders=orders,comparisons=[],arms={})
 for numerator,denominator in [('new','old'),('new','cgo'),('old','cgo')]:
  ratios_by={}
  for metric in metrics+(memmetrics if denominator=='old' else []):
   ratios=[by[b,numerator][metric]/by[b,denominator][metric] for b in range(6)]
   result=dict(numerator=numerator,denominator=denominator,metric=metric,paired_run_geomean_ratio=math.exp(statistics.mean(map(math.log,ratios))),minimum_pair_ratio=min(ratios),maximum_pair_ratio=max(ratios),pairs_below_one=sum(x<1 for x in ratios),ratios=ratios)
   wr['comparisons'].append(result);ratios_by[metric]=result['paired_run_geomean_ratio']
  paired.append(f"|{workload}|{numerator}/{denominator}|{ratios_by['mean_ns']:.6f}|{ratios_by['p99_ns']:.6f}|{ratios_by['process_cpu_ns_per_op']:.6f}|")
 for arm in ['cgo','old','new']:
  ar=[r for r in rows if r['arm']==arm]
  v=dict(runs=len(ar),measured_ops=sum(r['observations'] for r in ar),errors=sum(r['errors'] for r in ar),callback_retries=sum(r['retries'] for r in ar),warmup_ops=0,warmup_callback_retries=0,go_gc_cycles=sum(r['go_gc_cycles'] for r in ar),go_gc_pause_ns=0,counters={} if arm!='cgo' else 'unobserved')
  assert v['runs']==6 and v['measured_ops']==6*n and v['errors']==v['callback_retries']==0
  for metric in metrics+memmetrics+['process_lifetime_high_water_rss_kib']:
   v[metric+'_median']=statistics.median(r[metric] for r in ar)
  for run in [r for r in schedule if r['arm']==arm and r['workload']['name']==workload]:
   d=p/run['run_id'];s=json.loads((d/'summary.json').read_text());m=json.loads((d/'manifest.json').read_text());gate=json.loads((d/'quiescence.json').read_text());cfg=m['config'];spec=next(x for x in plan['arms'] if x['name']==arm)
   assert s['success'] and s['persisted_validation']=='ok' and gate['passed']
   assert cfg['workload']==workload and cfg['iterations']==n and cfg['warmup']==warm and cfg['workers']==1 and cfg['profile']=='none' and cfg['transaction_timeout_ms']==30000 and cfg['retry_limit']==10 and cfg['allow_writes']
   assert cfg['source_revision']==spec['source_revision'] and cfg['harness_id']==plan['harness_id']
   assert m['backend']['executable']==spec['binary'] and m['backend']['executable_sha256']==pins[spec['binary']]
   assert m['go_version']=='go1.26.9' and m['gomaxprocs']==8
   for fn,count,phase in [('samples.csv',n,'measure'),('warmup.csv',warm,'warmup')]:
    with (d/fn).open() as f:raw=list(csv.DictReader(f))
    assert len(raw)==count and sorted(int(x['index']) for x in raw)==list(range(count))
    assert all(x['phase']==phase and x['check_status']=='ok' and x['error']=='' and int(x['attempts'])==1 and int(x['worker'])==0 and int(x['latency_ns'])>0 for x in raw)
    if phase=='warmup':v['warmup_ops']+=count
   delta=s['client_counters_delta']
   if arm=='cgo':
    assert delta['observation']=='unobserved' and m['backend']['native_library']
    assert list(m['backend']['loaded_fdb_libraries_sha256'].values())==['e4ad7fed7b7580bf5f7b38f0a4279d37e698f69d534182e5de0b8beeef6cd1b1']
   else:
    assert delta['observation']=='observed' and not m['backend']['native_library'] and not m['backend']['loaded_fdb_libraries_sha256']
    c=delta['values'];assert c['transaction_retries']==c['grv_cache_hits']==c['connection_failures']==0
    assert c['read_versions_completed']==c['grv_successful_batch_replies']==n
    commits=n if workload in ['batchset50','batchset50x1000','set100'] else 0
    assert c['commit_started']==c['commit_completed']==commits
    for key,value in c.items():v['counters'][key]=v['counters'].get(key,0)+value
   v['go_gc_pause_ns']+=s['stats_after']['go_gc_pause_ns']-s['stats_before']['go_gc_pause_ns']
   prefix=cfg['prefix'];assert prefix==run['command'][run['command'].index('--prefix')+1]
   report['prefixes'].append(prefix)
   telem=[json.loads(x) for x in (d/'telemetry.jsonl').read_text().splitlines()]
   client=[q for tick in telem for q in tick['processes'] if q['comm'].startswith('candidate-')]
   server=[q for tick in telem for q in tick['processes'] if q['comm']=='fdbserver']
   report['telemetry'][run['run_id']]=dict(ticks=len(telem),client_observations=len(client),server_observations=len(server))
  v['all_raw_persistence_identity_and_quiescence_checks_passed']=True
  wr['arms'][arm]=v
  absolute.append(f"|{workload}|{arm}|{v['mean_ns_median']/1000:.3f}|{v['p99_ns_median']/1000:.3f}|{v['process_cpu_ns_per_op_median']/1000:.3f}|")
 report['workloads'][workload]=wr
assert len(report['prefixes'])==len(set(report['prefixes']))==len(schedule) and {len(x.encode()) for x in report['prefixes']}=={68}
report['inference']='Six paired exploratory run blocks; geometric mean within-block ratios, no confirmatory/noninferiority/global-speed/capacity claim. Native internal counters unobserved, not zero. Go MemStats excludes C++ heap, so no Go/CGo allocation ratios. Pooled tails descriptive. Lifetime RSS includes setup, not measured-phase peak. Source/binary identity verified per run; rawchecks do not approve candidate semantics.'
(p/'counter-resource-validation.json').write_text(json.dumps(report,indent=2)+'\n')
text='# '+plan['campaign']+'\n\nAbsolute values are medians of six run-level statistics; ratios are geometric means of six paired within-block ratios, NOT ratios of these medians. µs per transaction operation. old=8ab5c5a4d6ee4858a4eacd6ac58e2a7be85dbead, new=master8ab5c5a4d6ee4858a4eacd6ac58e2a7be85dbead+PR5patch8199a1c86c4df4ea2a877cd39355376d07ccebb3847dc2f4e4d74eb7e882eb54, cgo=immutable native7.3.77.\n\n'+'\n'.join(absolute)+'\n\n'+'\n'.join(paired)+'\n\n'+report['inference']+'\n'
(p/'TABLES.md').write_text(text)
print(text)
