#!/usr/bin/env bash
# Round 6 linger sweep + multi-rate capped + M (D-M) on netg-r6 (3-process cluster).
set -euo pipefail
cd /root/netg/repo/docs/labs/single-network-goroutine
mkdir -p rawhz6
export NETG_MY_CPUS=2-15 CLIENT_CPUS=8-15 FOREIGN_MAX=1000 RETRIES=0 MAX_WAIT=0 BIN=/root/netg/bin/clientbench-go CF=/root/netg/fdb/fdb.cluster PIDS=$(pgrep -x fdbserver | paste -sd,)
rm -f rawhz6/linger.jsonl rawhz6/linger_rate.jsonl rawhz6/linger.log rawhz6/linger_rate.log

CELLS="get100:8:12000:36000 get100:64:80000:240000 get100:256:250000:750000 set100:8:5000:15000 set100:64:25000:75000 set100:256:80000:240000 getset100:8:4000:12000 getset100:64:25000:75000 getset100:256:70000:210000"
ARMS_FILE=$PWD/hz6_linger_arms.json OUT=$PWD/rawhz6/linger.jsonl ROUNDS=3 CELLS="$CELLS" ./ladder.sh > rawhz6/linger.log 2>&1

for spec in get100:64:12000:36000:8000 get100:64:30000:90000:19500 get100:64:42000:126000:28000 set100:64:7500:22500:4800 set100:64:18000:54000:11500 set100:64:26000:78000:17000; do
  IFS=: read -r wl w short long rate <<<"$spec"
  af="rawhz6/linger_rate_arms_${wl}_${rate}.json"
  python3 -c "
import json; a=json.load(open('hz6_linger_rate_arms.json'))
for v in a.values(): v['args']=v.get('args',[])+['-rate=$rate']
json.dump(a,open('$af','w'),indent=1)"
  echo "== rate-cap $wl/$w rate=$rate $(date -Is)" >> rawhz6/linger_rate.log
  ARMS_FILE="$PWD/$af" OUT=$PWD/rawhz6/linger_rate.jsonl ROUNDS=3 CELLS="$wl:$w:$short:$long" ./ladder.sh >> rawhz6/linger_rate.log 2>&1
done

python3 -c "
import json
a = {k: v for k, v in json.load(open('hz6_linger_arms.json')).items() if k in ('M', 'D', 'D5', 'D50', 'DNGa20', 'DNGa100')}
json.dump(a, open('rawhz6/sys_linger_arms.json', 'w'), indent=1)"
SYSCALLS=1 ARMS_FILE=$PWD/rawhz6/sys_linger_arms.json OUT=$PWD/rawhz6/syscalls.jsonl ROUNDS=2 CELLS="get100:64:80000:240000 get100:256:250000:750000" ./ladder.sh >> rawhz6/syscalls.log 2>&1

echo linger6-done
