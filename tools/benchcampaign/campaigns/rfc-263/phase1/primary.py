import csv, hashlib, json, os, pathlib, resource, statistics, subprocess, sys, time, uuid
E=pathlib.Path('/tmp/rfc263-phase1.owjNRR')
CLUSTER='${FDB_PERF_HOME}/cpu-work/fdb4.cluster'
AFF='2,3,14,15'
RUNS=E/'timed'; RUNS.mkdir(exist_ok=True)
H=hashlib.sha256((E/'harness.tar.gz').read_bytes()).hexdigest()
S={'baseline':(E/'baseline-revision.txt').read_text().strip(),'candidate':'tree:'+(E/'candidate-tree.txt').read_text().strip()}
ENV=dict(os.environ,GOMAXPROCS='4',GOGC='100',GOMEMLIMIT='off',GODEBUG='')
for k in list(ENV):
    if k.startswith('FDB_NETWORK_OPTION_'): raise RuntimeError('Forbidden inherited network option: '+k)

def sha(path): return hashlib.sha256(pathlib.Path(path).read_bytes()).hexdigest()
def load(): return pathlib.Path('/proc/loadavg').read_text().strip()
def stat():
    v=list(map(int,pathlib.Path('/proc/stat').read_text().splitlines()[0].split()[1:]))
    return dict(total=sum(v[:8]),idle=v[3]+v[4],steal=v[7])
def quantile(vals,p): return sorted(vals)[min(len(vals)-1,int((len(vals)-1)*p))]
def one(phase, workload, pair, arm, timeout=30000, workers=8, profile='none', iterations=20000, binary=None):
    runid=f'{phase}-{workload}-p{pair}-{arm}'
    out=RUNS/runid
    if out.exists(): raise RuntimeError('Refuse overwrite '+str(out))
    prefix=('clientbench/rfc263/'+uuid.uuid4().hex).ljust(67,'0')+'/'
    exe=E/'bin'/(binary or arm)
    cmd=['taskset','-c',AFF,str(exe),'--cluster-file',CLUSTER,'--workload',workload,'--workers',str(workers),'--iterations',str(iterations),'--warmup','2000','--transaction-timeout-ms',str(timeout),'--retry-limit','10','--allow-writes','--prefix',prefix,'--output',str(out),'--run-id',runid,'--block',str(pair),'--source-revision',S[arm],'--harness-id',H,'--profile',profile]
    time.sleep(2)
    a=stat(); time.sleep(1); b=stat()
    gate=dict(idle_fraction=(b['idle']-a['idle'])/(b['total']-a['total']),steal_ticks=b['steal']-a['steal'])
    meta=dict(argv=cmd,env={k:ENV[k] for k in ['GOMAXPROCS','GOGC','GOMEMLIMIT','GODEBUG']},before_loadavg=load(),idle_gate=gate,executable_sha256=sha(exe),start_utc=time.strftime('%Y-%m-%dT%H:%M:%SZ',time.gmtime()),source=S[arm],prefix_bytes=len(prefix))
    (RUNS/(runid+'.command.json')).write_text(json.dumps(meta,indent=2))
    with (RUNS/(runid+'.stdout.log')).open('w') as log:
        usage_before=resource.getrusage(resource.RUSAGE_CHILDREN)
        p=subprocess.run(cmd,env=ENV,stdout=log,stderr=subprocess.STDOUT,timeout=240)
        usage_after=resource.getrusage(resource.RUSAGE_CHILDREN)
        (RUNS/(runid+'.whole-process-cpu.json')).write_text(json.dumps(dict(user_seconds=usage_after.ru_utime-usage_before.ru_utime,system_seconds=usage_after.ru_stime-usage_before.ru_stime)))
    meta.update(returncode=p.returncode,after_loadavg=load(),end_utc=time.strftime('%Y-%m-%dT%H:%M:%SZ',time.gmtime()))
    assert sha(exe)==meta['executable_sha256']
    (RUNS/(runid+'.command.json')).write_text(json.dumps(meta,indent=2))
    if p.returncode: raise RuntimeError('Run failed, retained: '+runid)
    summary=json.loads((out/'summary.json').read_text())
    assert summary['success'] and summary['completed']==iterations and summary['errors']==0, summary
    before,after=summary['stats_before'],summary['stats_after']
    cpu=((after['process_user_cpu_us']-before['process_user_cpu_us'])+(after['process_system_cpu_us']-before['process_system_cpu_us']))/iterations
    alloc=(after['go_mallocs']-before['go_mallocs'])/iterations
    byt=(after['go_total_alloc_bytes']-before['go_total_alloc_bytes'])/iterations
    samples=list(csv.DictReader((out/'samples.csv').open()))
    assert len(samples)==iterations
    lat=[int(x['latency_ns'])/1000 for x in samples]
    row=dict(run=runid,phase=phase,workload=workload,pair=pair,arm=arm,cpu_us=cpu,allocs=alloc,bytes=byt,mean_us=statistics.mean(lat),p50_us=quantile(lat,.5),p99_us=quantile(lat,.99),gc=after['go_gc_cycles']-before['go_gc_cycles'],loadavg=meta['before_loadavg'],idle=gate['idle_fraction'],sha=meta['executable_sha256'])
    print(json.dumps(row),flush=True)
    with (E/'run-metrics.jsonl').open('a') as f: f.write(json.dumps(row)+'\n')
    return row

def report(rows):
    result={}
    for w in ['get100','set100','range100']:
        groups={a:[r for r in rows if r['workload']==w and r['arm']==a] for a in ['baseline','candidate']}
        result[w]={a:{k:dict(median=statistics.median([r[k] for r in rs]),minimum=min(r[k] for r in rs),maximum=max(r[k] for r in rs)) for k in ['cpu_us','allocs','bytes','mean_us','p99_us']} for a,rs in groups.items()}
        result[w]['cpu_ratios']=[next(r['cpu_us'] for r in groups['candidate'] if r['pair']==i)/next(r['cpu_us'] for r in groups['baseline'] if r['pair']==i) for i in range(3)]
        result[w]['cpu_beyond_observed_spread']=min(r['cpu_us'] for r in groups['candidate'])>max(r['cpu_us'] for r in groups['baseline'])
    (E/'primary-analysis.json').write_text(json.dumps(result,indent=2))
    print(json.dumps(result,indent=2),flush=True)
    return result

if __name__=='__main__':
    rows=[]
    for workload in ['get100','set100','range100']:
        for pair in range(3):
            for arm in (['baseline','candidate'] if pair%2==0 else ['candidate','baseline']):
                rows.append(one('primary',workload,pair,arm))
    result=report(rows)
    if any(v['cpu_beyond_observed_spread'] for v in result.values()):
        print('STOP: CPU regression beyond observed arm ranges; attribution/report required before commit.',flush=True)
        sys.exit(4)
