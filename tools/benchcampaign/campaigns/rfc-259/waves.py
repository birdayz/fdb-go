import sys, collections
ev=[l.split() for l in open(sys.argv[1])]
ev=[(int(t),k,int(n)) for t,k,n in ev]
start=int(len(ev)*0.5); ev=ev[start:start+int(sys.argv[2]) if len(sys.argv)>2 else start+300]
t0=ev[0][0]
# compress consecutive same-kind events
out=[]
for t,k,n in ev:
    if out and out[-1][1]==k and t-out[-1][3]<200_000:
        out[-1][2]+=n; out[-1][3]=t; out[-1][4]+=1
    else:
        out.append([t,k,n,t,1])
for t,k,n,te,c in out:
    print(f"{(t-t0)/1000:9.1f}us {k} x{c:<3} n={n:<4} span={(te-t)/1000:7.1f}us")
