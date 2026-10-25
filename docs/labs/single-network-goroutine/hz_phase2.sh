#!/usr/bin/env bash
set -euo pipefail
cd /root/netg/repo/docs/labs/single-network-goroutine
export NETG_MY_CPUS=2-15 CLIENT_CPUS=8-15 FOREIGN_MAX=1000 RETRIES=0 MAX_WAIT=0 BIN=/root/netg/bin/clientbench-go CF=/root/netg/fdb/fdb.cluster PIDS=$(pgrep -x fdbserver | paste -sd,)
CELLS="get100:256:1600000:4800000 get100:64:400000:1200000 get100:8:50000:150000 getset100:64:90000:260000 getset100:256:280000:840000 set100:64:100000:300000 set100:256:330000:1000000"
ARMS_FILE=$PWD/hz2_arms.json OUT=$PWD/rawhz/ladder2.jsonl ROUNDS=3 CELLS="$CELLS" ./ladder.sh > rawhz/ladder2.log 2>&1
# Client syscalls per op.
for cell in "get100 8 30000 100000" "get100 64 200000 600000" "get100 256 800000 2400000"; do
  read -r wl w s l <<<"$cell"
  python3 run_cell.py --binary $BIN --cluster-file $CF --server-pids $PIDS --arms hz_sys_arms.json --client-cpus 8-15 --load-max 1000 --max-wait 0 --foreign-max 1000 --retries 0     --syscalls --workload $wl --workers $w --short $s --long $l --order A,W,WbAd,A4,G05,B,A,W,WbAd,A4,G05,B --round-len 6 --out rawhz/syscalls.jsonl >> rawhz/syscalls.log 2>&1
done
# Wire capture + server metrics, get100/64.
OUT=$PWD/rawhz/wire LOGS=/root/netg/fdb WIRETRACE=/root/netg/bin/wiretrace WIRECAPTURE_CPUS=0-1 LOAD_MAX=1000 ARMS_FILE=$PWD/hz_sys_arms.json ARMS=A,W,WbAd,G05,B ROUNDS=2 ./wire_campaign.sh > rawhz/wire.log 2>&1
# GRV window sizing.
for w in 1 8 64 256; do
  env -i PATH=/usr/bin:/bin HOME=/root GOMAXPROCS=8 FDBGO_EXP_GRV_WINDOW_LOG=1 taskset -c 8-15 $BIN -cluster-file $CF -workload get100 -workers $w -iterations $((w*3000)) -warmup 1000 -allow-writes -output /tmp/gw$w > /tmp/gw$w.log 2>&1
  echo "workers=$w $(grep grvwindow /tmp/gw$w.log | tail -5 | tr "\n" " ")" >> rawhz/grvwindow.txt
  rm -rf /tmp/gw$w
done
echo phase2-done
