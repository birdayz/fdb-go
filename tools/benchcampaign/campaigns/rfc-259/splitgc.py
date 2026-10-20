import sys, re, collections
W='${FDB_PERF_HOME}/cpu-work'
tr, log = sys.argv[1], sys.argv[2]
gcs=[]
for l in open(log):
    m=re.match(r'gc (\d+) @([\d.]+)s \d+%: ([\d.]+)\+([\d.]+)\+([\d.]+) ms clock',l)
    if m: gcs.append((float(m.group(2))*1000, float(m.group(3))+float(m.group(4))+float(m.group(5))))
ev=[l.split() for l in open(tr)]
ev=[(int(t)/1e6,k,int(n)) for t,k,n in ev]
waves=[]
for i,(t,k,n) in enumerate(ev):
    if k!='c': continue
    if waves and t-waves[-1][1]<1.0: waves[-1][1]=t; waves[-1][2]+=1
    else: waves.append([t,t,1,i])
W8=int(sys.argv[3]) if len(sys.argv)>3 else 8
print("wave sizes:", collections.Counter(w[2] for w in waves).most_common(6), "gcs", len(gcs))
prev=None; n=0
for w in waves:
    if prev and prev[2]==W8 and w[2]<W8:
        near=[(round(g[0],1),round(g[1],2)) for g in gcs if prev[0]-8 < g[0] < w[0]+1]
        print(f"split at {w[0]:.1f}ms (prev full {prev[0]:.1f}); GC near: {near}")
        n+=1
        if n>6: break
    prev=w
