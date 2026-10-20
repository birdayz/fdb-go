import sys
ev=[l.split() for l in open(sys.argv[1])]
ev=[(int(t),k,int(n)) for t,k,n in ev]
ev=ev[len(ev)//2:]
minw=int(sys.argv[2]) if len(sys.argv)>2 else 20
skip=int(sys.argv[3]) if len(sys.argv)>3 else 0
idx=None
for i,(t,k,n) in enumerate(ev):
    if k=='c' and i>100:
        j=i; cnt=0
        while j<len(ev) and ev[j][0]-t<1_000_000:
            if ev[j][1]=='c': cnt+=1
            j+=1
        if cnt>=minw:
            if skip==0: idx=i; break
            skip-=1
t0=ev[idx][0]
out=[]
for t,k,n in ev[idx:idx+400]:
    if (t-t0)>2_000_000: break
    if k=='r': continue
    if out and out[-1][1]==k and k in 'cAC' and t-out[-1][3]<15_000:
        out[-1][2]+=n; out[-1][3]=t
    else: out.append([t,k,n,t])
for t,k,n,te in out:
    print(f"{(t-t0)/1000:8.1f}-{(te-t0)/1000:<8.1f} {k} {n}")
