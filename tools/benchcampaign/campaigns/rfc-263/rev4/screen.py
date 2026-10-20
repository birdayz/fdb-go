import importlib.util,json,pathlib,statistics,sys
P=pathlib.Path('/tmp/rfc263-rev4-screen.2KEIQu')
spec=importlib.util.spec_from_file_location('campaign','/tmp/rfc263-phase1.owjNRR/primary.py')
c=importlib.util.module_from_spec(spec);spec.loader.exec_module(c)
c.E=P;c.RUNS=P/'timed';c.RUNS.mkdir(exist_ok=True)
for arm in ('lazy','shards'): c.S[arm]='tree:'+(P/(arm+'-tree.txt')).read_text().strip()
if len(sys.argv)>1 and sys.argv[1]=='profiles':
    for workload,count in [('get100',1000000),('set100',400000),('range100',500000)]:
        for arm in ('lazy','shards'): c.one('rev4-mutex-draining-w32',workload,0,arm,workers=32,profile='mutex',iterations=count)
else:
    rows=[]
    orders=[('baseline','lazy','shards'),('lazy','shards','baseline'),('shards','baseline','lazy')]
    for workload in ('get100','set100','range100'):
        for pair,order in enumerate(orders):
            for arm in order: rows.append(c.one('rev4-screen-w8',workload,pair,arm))
    result={}
    for w in ('get100','set100','range100'):
        groups={a:[r for r in rows if r['workload']==w and r['arm']==a] for a in ('baseline','lazy','shards')}
        result[w]={a:{k:{'median':statistics.median(r[k] for r in rs),'minimum':min(r[k] for r in rs),'maximum':max(r[k] for r in rs)} for k in ('cpu_us','allocs','bytes','mean_us','p99_us')} for a,rs in groups.items()}
        for arm in ('lazy','shards'):
            result[w][arm]['paired_cpu_ratios']=[next(r['cpu_us'] for r in groups[arm] if r['pair']==i)/next(r['cpu_us'] for r in groups['baseline'] if r['pair']==i) for i in range(3)]
    (P/'screen-analysis.json').write_text(json.dumps(result,indent=2))
    print(json.dumps(result,indent=2),flush=True)
