import sys
ev=[l.split() for l in open(sys.argv[1])]
ev=[(int(t),k,int(n)) for t,k,n in ev]
N=int(sys.argv[2])
t0=ev[0][0]
waves=[]
for i,(t,k,n) in enumerate(ev):
    if k!='c': continue
    if waves and t-waves[-1][1]<1_000_000: waves[-1][1]=t; waves[-1][2]+=1
    else: waves.append([t,t,1,i])
# first wave after warm start with fewer than N
first=None
skip=int(sys.argv[3]) if len(sys.argv)>3 else 0
for w in range(2,len(waves)):
    if waves[w][2] < N and (waves[w][0]-t0)/1e6 > skip:
        first=w; break
print("waves:", [(round((a-t0)/1e3,1),c) for a,b,c,i in waves[max(0,first-3):first+4]])
a=waves[first-1][3]; b=waves[first][3]+5
out=[]
for t,k,n in ev[a:b]:
    if k=='r': continue
    if out and out[-1][1]==k and k in 'cAC' and t-out[-1][3]<15_000:
        out[-1][2]+=n; out[-1][3]=t
    else: out.append([t,k,n,t])
for t,k,n,te in out:
    print(f"{(t-t0)/1000:9.1f}-{(te-t0)/1000:<9.1f} {k} {n}")
