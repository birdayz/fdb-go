"""Complete per-run latency histograms/quantiles; never pool timing and diagnostics."""
import argparse, collections, csv, hashlib, json, pathlib, statistics
parser=argparse.ArgumentParser(); parser.add_argument('evidence',type=pathlib.Path)
parser.add_argument('--phase-prefix',default='')
parser.add_argument('--output-prefix',default='tail')
args=parser.parse_args(); P=args.evidence; H=P/'histograms'; H.mkdir(exist_ok=True)
rows=[json.loads(s) for s in (P/'run-metrics.jsonl').read_text().splitlines()]
rows=[r for r in rows if r['phase'].startswith(args.phase_prefix)]
result=[]
for r in rows:
    sample=P/'runs'/r['run']/'samples.csv'
    vals=[]; hist=collections.Counter(); slow=[]
    for s in csv.DictReader(sample.open()):
        assert s['phase']=='measure' and not s['error']
        ns=int(s['latency_ns']); vals.append(ns); hist[ns//1000]+=1
        slow.append((ns,int(s['start_offset_ns']),int(s['worker']),int(s['index'])))
    vals.sort(); n=len(vals)
    summary=json.loads((sample.parent/'summary.json').read_text()); assert n==summary['completed']
    q={f'p{label}_us':vals[int((n-1)*p)]/1000 for label,p in [('50',.5),('90',.9),('99',.99),('999',.999)]}
    histogram=H/(r['run']+'.json')
    histogram.write_text(json.dumps({'run':r['run'],'count':n,'bin_width_ns':1000,'bounds':'lower inclusive, upper exclusive; no overflow bucket','bins':[[i*1000,c] for i,c in sorted(hist.items())]},separators=(',',':'))+'\n')
    slow.sort(reverse=True); (H/(r['run']+'.slowest.json')).write_text(json.dumps({'fields':['latency_ns','start_offset_ns','worker','index'],'slowest_0_1_percent':slow[:max(1,n//1000)]},separators=(',',':'))+'\n')
    result.append(dict(r,**q,count=n,minimum_us=vals[0]/1000,maximum_us=vals[-1]/1000,histogram=str(histogram),histogram_sha256=hashlib.sha256(histogram.read_bytes()).hexdigest(),samples_sha256=hashlib.sha256(sample.read_bytes()).hexdigest()))
(P/(args.output_prefix+'-rows.json')).write_text(json.dumps(result,indent=2)+'\n')
analysis={}
for phase in sorted({r['phase'] for r in result}):
    rr=[r for r in result if r['phase']==phase]; metrics=['cpu_us','mean_us','p50_us','p90_us','p99_us','p999_us','maximum_us']
    groups={a:[r for r in rr if r['arm']==a] for a in sorted({r['arm'] for r in rr})}
    reference='baseline' if 'baseline' in groups else ('rev5' if 'rev5' in groups else next(iter(groups)))
    byarm={a:{k:{'median':statistics.median(r[k] for r in rs),'minimum':min(r[k] for r in rs),'maximum':max(r[k] for r in rs)} for k in metrics} for a,rs in groups.items()}
    def paired(a,b):
        return {k:[100*(next(r[k] for r in groups[a] if r['pair']==i)/next(r[k] for r in groups[b] if r['pair']==i)-1) for i in sorted(r['pair'] for r in groups[a])] for k in metrics}
    pairs={a:paired(a,reference) for a in groups if a!=reference}
    analysis[phase]={'arms':byarm,'reference':reference,'paired_change_percent':pairs,'run_count':len(rr),'diagnostic':phase.startswith('tail-diag-')}
    if 'identity' in groups and 'rev5' in groups:
        analysis[phase]['identity_vs_rev5_paired_change_percent']=paired('identity','rev5')
(P/(args.output_prefix+'-analysis.json')).write_text(json.dumps(analysis,indent=2)+'\n')
print(json.dumps(analysis,indent=2))
