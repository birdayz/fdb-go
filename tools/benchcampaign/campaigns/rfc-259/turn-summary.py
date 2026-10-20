import json, statistics as st, math, sys
rows=[json.loads(l) for l in open(sys.argv[1])]
from collections import defaultdict
g=defaultdict(dict)
for r in rows:
    arm, wl, rate, wk, b = r['label'].split('-'); g[(wl,int(rate),int(wk))][(arm,b)]=r
for k in sorted(g, key=lambda k:(k[0],k[1]==0,k[1],k[2])):
    d=g[k]; blocks=sorted({b for _,b in d})
    col=lambda a,f:[d[(a,b)][f] for b in blocks]
    print(f"\n{k[0]} rate={k[1] or 'closed'} workers={k[2]}")
    for f,name in (('client_cpu_us','client µs'),('server_cycles','server cyc'),('mean_latency_us','lat µs'),('tx_per_s','tx/s')):
        m={a:st.median(col(a,f)) for a in ('base','turn','cgo')}
        rt=[t/bb for t,bb in zip(col('turn',f),col('base',f))]
        rc=[t/c for t,c in zip(col('turn',f),col('cgo',f))]
        print(f"  {name:10} base {m['base']:>10,.1f} turn {m['turn']:>10,.1f} cgo {m['cgo']:>10,.1f} | turn/base {st.median(rt):.3f} [{min(rt):.3f}-{max(rt):.3f}] turn/cgo {st.median(rc):.3f} [{min(rc):.3f}-{max(rc):.3f}]")
    if k[1]:
        for a in ('base','turn','cgo'):
            p=[(d[(a,b)].get('pacing') or {}) for b in blocks]
            print(f"  pacing {a}: late>1ms {[round(x.get('LateOver1ms',0),4) for x in p]} p99 lag µs {[round(x.get('LagP99NS',0)/1000,1) for x in p]}")
