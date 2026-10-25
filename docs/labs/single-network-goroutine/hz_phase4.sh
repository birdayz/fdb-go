#!/usr/bin/env bash
# Phase 4 (DESIGN.md benchmark): arms L, A, D, D-NG, D at 4 Ps; 9 cells; 3 rotated rounds.
# Then: set100/64 rate-capped to 18k ops/s (below L's closed-loop 19k) for L, A, D, D-NG;
# one-worker set100 captures (requests per transaction, Go vs libfdb_c); and one loopback
# capture per arm (except D at 4 Ps) and cell for frames per packet.
set -euo pipefail
cd /root/netg/repo/docs/labs/single-network-goroutine
export NETG_MY_CPUS=2-15 CLIENT_CPUS=8-15 FOREIGN_MAX=1000 RETRIES=0 MAX_WAIT=0 BIN=/root/netg/bin/clientbench-go CF=/root/netg/fdb/fdb.cluster PIDS=$(pgrep -x fdbserver | paste -sd,)
CELLS="get100:8:50000:150000 get100:64:400000:1200000 get100:256:1600000:4800000 set100:8:20000:60000 set100:64:100000:300000 set100:256:330000:1000000 getset100:8:13000:40000 getset100:64:90000:260000 getset100:256:280000:840000"
ARMS_FILE=$PWD/hz4_arms.json OUT=$PWD/rawhz4/ladder.jsonl ROUNDS=3 CELLS="$CELLS" ./ladder.sh > rawhz4/ladder.log 2>&1
ARMS_FILE=$PWD/hz4_rate_arms.json OUT=$PWD/rawhz4/rate.jsonl ROUNDS=3 CELLS="set100:64:100000:300000" ./ladder.sh > rawhz4/rate.log 2>&1
WL=set100 WORKERS=1 CAP_ITERS=3000 CAP_WARMUP=300 SKIP_METRICS=1 OUT=$PWD/rawhz4/wire-set100-1 LOGS=/root/netg/fdb WIRETRACE=/root/netg/bin/wiretrace WIRECAPTURE_CPUS=0-1 LOAD_MAX=1000 \
  ARMS_FILE=$PWD/hz4_wire_arms.json ARMS=L,A,D,DNG ROUNDS=1 ./wire_campaign.sh >> rawhz4/wire.log 2>&1
for spec in get100:8:30000 get100:64:200000 get100:256:600000 set100:8:12000 set100:64:60000 set100:256:200000 getset100:8:8000 getset100:64:50000 getset100:256:160000; do
  IFS=: read -r wl w it <<<"$spec"
  WL=$wl WORKERS=$w CAP_ITERS=$it SKIP_METRICS=1 OUT=$PWD/rawhz4/wire-$wl-$w LOGS=/root/netg/fdb WIRETRACE=/root/netg/bin/wiretrace WIRECAPTURE_CPUS=0-1 LOAD_MAX=1000 \
    ARMS_FILE=$PWD/hz4_wire_arms.json ARMS=L,A,D,DNG ROUNDS=1 ./wire_campaign.sh >> rawhz4/wire.log 2>&1
done
echo phase4-done
