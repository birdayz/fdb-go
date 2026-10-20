import sys
ev=[l.split() for l in open(sys.argv[1])]
ev=[(int(t),k,int(n)) for t,k,n in ev]
t0=ev[0][0]
# rounds: start at each commit-reply wave (c cluster), collect events until next wave
waves=[]
for i,(t,k,n) in enumerate(ev):
    if k=='c' and (not waves or t-waves[-1][1]>1_000_000):
        waves.append([t,t,i])
    elif k=='c': waves[-1][1]=t
rows=[]
for wi in range(len(waves)-1):
    a=waves[wi][2]; b=waves[wi+1][2]
    seg=ev[a:b]
    cs=[t for t,k,n in seg if k=='c']; As=[t for t,k,n in seg if k=='A']; Cs=[t for t,k,n in seg if k=='C']; Gs=[n for t,k,n in seg if k=='G']
    if len(cs)<4 or not Cs: continue
    rows.append(((cs[0]-t0)/1000, len(cs),(cs[-1]-cs[0])/1000,(As[-1]-As[0])/1000 if As else 0,len(Gs),(Cs[-1]-Cs[0])/1000,(Cs[-1]-cs[0])/1000))
print(" t_ms   n  c_spread A_spread nGRV C_spread c0->lastC")
for r in rows[:int(sys.argv[2]) if len(sys.argv)>2 else 40]:
    print(f"{r[0]/1000:6.1f} {r[1]:3d} {r[2]:8.0f} {r[3]:8.0f} {r[4]:4d} {r[5]:8.0f} {r[6]:8.0f}")
import statistics
if rows:
    cs=[r[5] for r in rows]; print("C_spread median",statistics.median(cs),"p90",sorted(cs)[int(len(cs)*.9)],"max",max(cs), "n",len(cs))
