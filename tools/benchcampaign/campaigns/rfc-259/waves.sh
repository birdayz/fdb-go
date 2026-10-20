#!/bin/sh
# usage: [ENV=..] waves.sh <cluster> <workload> <workers> <iters> -> summary + commit wave sizes
W=${FDB_PERF_HOME}/cpu-work; cd $W; rm -rf tr/wv
env -i PATH=/usr/bin:/bin HOME=$HOME GOMAXPROCS=8 GOGC=${GOGC_OVR:-100} GOMEMLIMIT=off FDBGO_EXP_TRACE=$W/tr/wv.txt FDBGO_EXP_OLDGRV=${OLDGRV:-} FDBGO_EXP_WCOAL=${WCOAL:-} taskset -c 0-7 bin/clientbench-go-trace -cluster-file $1 -workload $2 -workers $3 -iterations $4 -warmup $(( $4 / 20 )) -allow-writes -output tr/wv > tr/wv.log 2>&1
python3 summ.py tr/wv | tail -1
python3 - <<'PY'
import collections, statistics
W='${FDB_PERF_HOME}/cpu-work'
ev=[l.split() for l in open(W+'/tr/wv.txt')]
ev=[(int(t)/1e3,k,int(n)) for t,k,n in ev]
ev=ev[len(ev)//3:]
G=[n for t,k,n in ev if k=='G']
waves=[]
for t,k,n in ev:
    if k!='c': continue
    if waves and t-waves[-1][1]<1000: waves[-1][1]=t; waves[-1][2]+=1
    else: waves.append([t,t,1])
print("  GRV batches/commit", round(len(G)/max(1,sum(w[2] for w in waves)),3), "waves", collections.Counter(w[2] for w in waves).most_common(5))
PY
