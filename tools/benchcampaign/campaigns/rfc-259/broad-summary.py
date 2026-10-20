import json, sys, statistics as st
from collections import defaultdict
g=defaultdict(dict)
for l in open(sys.argv[1]):
    r=json.loads(l); arm,wl,wk,b=r['label'].split('-'); g[(wl,int(wk))][(arm,b)]=r
print(f"{'workload':9} {'w':>3} | {'client go/cgo':>14} | {'server go/cgo':>14} | {'tput go/cgo':>12} | {'lat go/cgo':>11}")
for k in sorted(g, key=lambda k:(k[0],k[1])):
    d=g[k]; blocks=sorted({b for _,b in d})
    def ratio(f):
        rs=[d[('go',b)][f]/d[('cgo',b)][f] for b in blocks if ('go',b) in d and ('cgo',b) in d]
        return st.median(rs), min(rs), max(rs)
    out=[]
    for f in ('client_cpu_us','server_cycles','tx_per_s','mean_latency_us'):
        m,lo,hi=ratio(f); out.append(f"{m:5.2f} [{lo:.2f}-{hi:.2f}]")
    print(f"{k[0]:9} {k[1]:>3} | "+" | ".join(out))
