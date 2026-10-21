import json, pathlib, statistics, sys
P=pathlib.Path(sys.argv[1] if len(sys.argv)>1 else '/tmp/rfc263-rev5.QgGUbU')
rows=json.loads((P/'pricing-rows.json').read_text())
result={}
for phase in sorted({r['phase'] for r in rows}):
    result[phase]={}
    for workload in sorted({r['workload'] for r in rows if r['phase']==phase}):
        rs=[r for r in rows if r['phase']==phase and r['workload']==workload]
        arms=sorted({r['arm'] for r in rs}); first=next(a for a in arms if a!='rev5')
        groups={a:[r for r in rs if r['arm']==a] for a in arms}
        metrics={a:{k:{'median':statistics.median(r[k] for r in g),'minimum':min(r[k] for r in g),'maximum':max(r[k] for r in g)} for k in ['cpu_us','allocs','bytes','mean_us','p99_us']} for a,g in groups.items()}
        paired=[100*(next(r['cpu_us'] for r in groups['rev5'] if r['pair']==i)/next(r['cpu_us'] for r in groups[first] if r['pair']==i)-1) for i in range(3)]
        result[phase][workload]={'arms':metrics,'change_percent':100*(metrics['rev5']['cpu_us']['median']/metrics[first]['cpu_us']['median']-1),'paired_change_percent':paired,'rev5_entire_range_above_reference':metrics['rev5']['cpu_us']['minimum']>metrics[first]['cpu_us']['maximum']}
result['run_count']=len(rows)
result['loadavg_range']=[min(float(r['loadavg'].split()[0]) for r in rows),max(float(r['loadavg'].split()[0]) for r in rows)]
result['idle_range']=[min(r['idle'] for r in rows),max(r['idle'] for r in rows)]
(P/'pricing-analysis.json').write_text(json.dumps(result,indent=2)+'\n')
print(json.dumps(result,indent=2))
