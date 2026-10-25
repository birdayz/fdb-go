#!/usr/bin/env bash
# Cross-box check: the phase-5 binaries (box 2) on box 1, arms L and A, shared cells.
set -euo pipefail
cd /root/netg/repo/docs/labs/single-network-goroutine
mkdir -p rawhz4/cross
export NETG_MY_CPUS=2-15 CLIENT_CPUS=8-15 FOREIGN_MAX=1000 RETRIES=0 MAX_WAIT=0 BIN=/root/netg/bin/clientbench-go-m CF=/root/netg/fdb/fdb.cluster PIDS=$(pgrep -x fdbserver | paste -sd,)
ARMS_FILE=$PWD/hz_cross.json OUT=$PWD/rawhz4/cross/ladder.jsonl ROUNDS=3 CELLS="get100:64:400000:1200000 set100:64:100000:300000 getset100:64:90000:260000" ./ladder.sh > rawhz4/cross/ladder.log 2>&1
echo cross-done
