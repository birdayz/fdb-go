import sys
W='${FDB_PERF_HOME}/cpu-work'
ev=[l.split() for l in open(W+'/tr/wv.txt')]
ev=[(int(t)/1e3,k,int(n)) for t,k,n in ev]
ev=ev[len(ev)//2:]
waves=[]; sends=[]
for t,k,n in ev:
    if k=='C':
        if sends and t-sends[-1][1]<1000: sends[-1][1]=t; sends[-1][2]+=1
        else: sends.append([t,t,1])
    if k!='c': continue
    if waves and t-waves[-1][1]<1000: waves[-1][1]=t; waves[-1][2]+=1
    else: waves.append([t,t,1])
t0=waves[0][0]
print("reply waves (t ms, n, spread us):", [(round((a-t0)/1000,2), n, round(b-a)) for a,b,n in waves[:10]])
print("send waves  (t ms, n, spread us):", [(round((a-t0)/1000,2), n, round(b-a)) for a,b,n in sends if a>=t0][:10])
