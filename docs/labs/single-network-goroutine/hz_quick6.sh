#!/usr/bin/env bash
# Round 6 quick check: D-NG vs D-NG-chan vs D-NG-ring (and A), one round, then
# CPU and mutex profiles of the three queues at get100/64 and /256.
set -euo pipefail
cd /root/netg/repo/docs/labs/single-network-goroutine
mkdir -p rawhz6/quick rawhz6/prof
export NETG_MY_CPUS=2-15 CLIENT_CPUS=8-15 FOREIGN_MAX=1000 RETRIES=0 MAX_WAIT=0 BIN=/root/netg/bin/clientbench-go CF=/root/netg/fdb/fdb.cluster PIDS=$(pgrep -x fdbserver | paste -sd,)
ARMS_FILE=$PWD/hz6_quick_arms.json OUT=$PWD/rawhz6/quick/ladder.jsonl ROUNDS=1 CELLS="get100:64:400000:1200000 get100:256:1600000:4800000 set100:64:100000:300000" ./ladder.sh > rawhz6/quick/ladder.log 2>&1
for cell in get100:64:1200000 get100:256:4800000; do
  IFS=: read -r wl w it <<<"$cell"
  for arm in ng ngc ngr; do
    for prof in cpu mutex block; do
      o=rawhz6/prof/$wl-$w-$arm-$prof
      env -i PATH=/usr/bin:/bin HOME=/root GOMAXPROCS=8 FDBGO_NET_DESIGN=$arm taskset -c 8-15 $BIN -cluster-file $CF -workload $wl -workers $w \
        -iterations $it -warmup 20000 -allow-writes -profile $prof -output $o > /dev/null 2>&1
      rm -f $o/samples.csv $o/warmup.csv
    done
  done
done
echo quick6-done
