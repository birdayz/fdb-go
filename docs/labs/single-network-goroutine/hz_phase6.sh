#!/usr/bin/env bash
# Round 6: L, A, A0 (A with no coalescing delay), D-NG, D-NG+yield, D-NG-chan,
# D-NG-ring; 9 cells x 3 rotated rounds. Then get100/64 and set100/64 capped at
# 90% of L's closed-loop median from the ladder; then client syscalls per op for
# the Go arms at get100/64 and /256.
set -euo pipefail
cd /root/netg/repo/docs/labs/single-network-goroutine
mkdir -p rawhz6
export NETG_MY_CPUS=2-15 CLIENT_CPUS=8-15 FOREIGN_MAX=1000 RETRIES=0 MAX_WAIT=0 BIN=/root/netg/bin/clientbench-go CF=/root/netg/fdb/fdb.cluster PIDS=$(pgrep -x fdbserver | paste -sd,)
CELLS="get100:8:50000:150000 get100:64:400000:1200000 get100:256:1600000:4800000 set100:8:20000:60000 set100:64:100000:300000 set100:256:330000:1000000 getset100:8:13000:40000 getset100:64:90000:260000 getset100:256:280000:840000"
ARMS_FILE=$PWD/hz6_arms.json OUT=$PWD/rawhz6/ladder.jsonl ROUNDS=3 CELLS="$CELLS" ./ladder.sh > rawhz6/ladder.log 2>&1
for cell in get100:64:400000:1200000 set100:64:100000:300000; do
  IFS=: read -r wl w short long <<<"$cell"
  rate=$(python3 -c "
import json,statistics
v=[json.loads(l)['ops_per_s'] for l in open('rawhz6/ladder.jsonl')]
v=[r['ops_per_s'] for r in map(json.loads,open('rawhz6/ladder.jsonl')) if r['arm']=='L' and r['workload']=='$wl' and r['workers']==$w]
print(int(0.9*statistics.median(v)//500*500))")
  python3 -c "
import json;a=json.load(open('hz6_arms.json'))
for v in a.values(): v['args']=v.get('args',[])+['-rate=$rate']
json.dump(a,open('rawhz6/rate_arms_$wl.json','w'),indent=1)"
  echo "$wl/$w rate=$rate" >> rawhz6/rate.log
  ARMS_FILE=$PWD/rawhz6/rate_arms_$wl.json OUT=$PWD/rawhz6/rate.jsonl ROUNDS=3 CELLS="$cell" ./ladder.sh >> rawhz6/rate.log 2>&1
done
python3 -c "
import json;a=json.load(open('hz6_arms.json'));a.pop('L');json.dump(a,open('rawhz6/sys_arms.json','w'),indent=1)"
SYSCALLS=1 ARMS_FILE=$PWD/rawhz6/sys_arms.json OUT=$PWD/rawhz6/syscalls.jsonl ROUNDS=2 CELLS="get100:64:400000:1200000 get100:256:1600000:4800000" ./ladder.sh > rawhz6/syscalls.log 2>&1
echo phase6-done
