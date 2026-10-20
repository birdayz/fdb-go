import json,pathlib,re,statistics
E=pathlib.Path('/tmp/rfc263-phase1.owjNRR')
rows=[json.loads(x) for x in (E/'run-metrics.jsonl').read_text().splitlines()]
out={}
for w in ['get100','set100','range100']:
    raw=(E/'pprof'/f'candidate-{w}-mutex-raw.txt').read_text()
    locations={}
    body=raw.split('Locations\n',1)[-1] if 'Locations\n' in raw else raw.split('Locations:',1)[-1]
    body=body.split('Mappings',1)[0]
    current=None
    for line in body.splitlines():
        m=re.match(r'\s*(\d+):',line)
        if m: current=m[1];locations[current]=line
        elif current: locations[current]+='\n'+line
    totals={'caller_delay_ns':0,'drainer_delay_ns':0,'caller_contentions':0,'drainer_contentions':0,'caller_samples':0,'drainer_samples':0}
    for line in raw.split('Samples:',1)[1].split('Locations',1)[0].splitlines():
        m=re.match(r'\s*(\d+)\s+(\d+):\s*([\d ]+)$',line)
        if not m: continue
        names='\n'.join(locations.get(x,'') for x in m[3].split())
        if 'sync.(*Mutex).Unlock' not in names: continue
        kind=None
        if 'client.(*timeoutQueue).fire' in names: kind='drainer'
        elif 'client.(*timeoutQueue).schedule' in names or 'client.(*timeoutQueue).remove' in names: kind='caller'
        if kind:
            totals[kind+'_delay_ns']+=int(m[2]);totals[kind+'_contentions']+=int(m[1]);totals[kind+'_samples']+=1
    assert totals['caller_samples'] and totals['drainer_samples'],totals
    r=next(x for x in rows if x['phase']=='mutex-draining-w32' and x['workload']==w and x['arm']=='candidate')
    s=json.loads((E/'timed'/r['run']/'summary.json').read_text())
    a=s['stats_after_identity'];e=s['lifecycle']['stats_after_shutdown']
    shutdown_cpu_us=(e['process_user_cpu_us']-a['process_user_cpu_us'])+(e['process_system_cpu_us']-a['process_system_cpu_us'])
    n=s['completed'];delay=totals['caller_delay_ns']+totals['drainer_delay_ns']
    # Independent positive control: sum of Go pprof's cumulative schedule,
    # remove and fire rows (no duplicate fire-fm row). Our narrower filter
    # excludes runtime-internal locks; require agreement within 0.5%.
    expected={'get100':1792306750,'set100':1257816690,'range100':1368420000}[w]
    assert abs(delay-expected)<expected*0.005,(w,delay,expected)
    totals['pprof_all_locks_cumulative_reference_ns']=expected
    totals.update(population=n,window_cpu_seconds=r['cpu_us']*n/1e6,post_identity_shutdown_cpu_seconds=shutdown_cpu_us/1e6,queue_delay_us_per_tx=delay/n/1000,queue_delay_over_window_cpu_pct=100*delay/(r['cpu_us']*n*1000),queue_delay_over_post_identity_shutdown_cpu_pct=100*delay/(shutdown_cpu_us*1000))
    out[w]=totals
(E/'contention-analysis.json').write_text(json.dumps(out,indent=2))
print(json.dumps(out,indent=2))
