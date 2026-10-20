import sys
ev=[l.split() for l in open(sys.argv[1])]
ev=[(int(t),k,int(n)) for t,k,n in ev]
ev=ev[len(ev)//2:]
ev=ev[:int(sys.argv[2])]
t0=ev[0][0]
# commit-reply waves: cluster 'c' events separated by >1ms gaps
waves=[]
for t,k,n in ev:
    if k!='c': continue
    if waves and t-waves[-1][1]<1_000_000: waves[-1][1]=t; waves[-1][2]+=1
    else: waves.append([t,t,1])
for a,b,c in waves[:40]:
    print(f"{(a-t0)/1000:9.1f}us  commit replies x{c:<3} over {(b-a)/1000:6.1f}us")
