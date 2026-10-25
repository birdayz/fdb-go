#!/usr/bin/env bash
set -euo pipefail
cd /root/netg/repo/docs/labs/single-network-goroutine
export NETG_MY_CPUS=2-15 CLIENT_CPUS=8-15 FOREIGN_MAX=1000 RETRIES=0 MAX_WAIT=0 BIN=/root/netg/bin/clientbench-go-na CF=/root/netg/fdb/fdb.cluster PIDS=$(pgrep -x fdbserver | paste -sd,)
for wl in get100 set100 getset100; do
  for e in FDBGO_EXP_NA=1 FDBGO_EXP_NA=nodelay; do
    rm -rf /tmp/nasmoke; env -i PATH=/usr/bin:/bin HOME=/root GOMAXPROCS=8 $e taskset -c 8-15 $BIN -cluster-file $CF -workload $wl -workers 64 -iterations 20000 -warmup 1000 -allow-writes -output /tmp/nasmoke > /tmp/nasmoke.log 2>&1
    echo "smoke $e $wl exit=$? $(python3 -c "import json;s=json.load(open(\"/tmp/nasmoke/summary.json\"));print(s[\"success\"],s[\"errors\"],s[\"retries\"])")" >> rawhz/na_smoke.txt
  done
done
ARMS_FILE=$PWD/hz_na_arms.json OUT=$PWD/rawhz/ladder_na.jsonl ROUNDS=3 CELLS="get100:64:400000:1200000 get100:256:1600000:4800000 set100:64:100000:300000 getset100:64:90000:260000" ./ladder.sh > rawhz/ladder_na.log 2>&1
OUT=$PWD/rawhz/wire_na LOGS=/root/netg/fdb WIRETRACE=/root/netg/bin/wiretrace WIRECAPTURE_CPUS=0-1 LOAD_MAX=1000 ARMS_FILE=$PWD/hz_na_arms.json ARMS=A,Wbest,NA,NG ROUNDS=2 BIN=$BIN ./wire_campaign.sh > rawhz/wire_na.log 2>&1
echo na-done
