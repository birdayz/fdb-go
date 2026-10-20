#!/usr/bin/env bash
# usage: roundtrace.sh <label> <binary> [ld_library_path]
# Traces the stateless server's socket reads and writes for 2 s of a steady set100/8 run.
set -euo pipefail
W=${FDB_PERF_HOME}/cpu-work
SRV=$(pgrep -u "$(id -u)" -f 'fdbserver.*4721')
label=$1 bin=$2 ld=${3:-}
out=$W/roundtrace/$label
rm -rf "$out"; mkdir -p "$out"
env -i PATH=/usr/bin:/bin HOME=$HOME GOMAXPROCS=8 GOGC=100 GOMEMLIMIT=off LD_LIBRARY_PATH=$ld \
  taskset -c 0-7 "$bin" -cluster-file $W/fdb3.cluster -workload set100 -workers 8 \
  -iterations 8000 -warmup 400 -allow-writes -output "$out/run" > "$out/run.log" 2>&1 &
client=$!
sleep 3
ss -tinpH state established > "$out/ss.txt"
timeout 4 perf trace -e recvfrom,sendmsg -p "$SRV" -o "$out/trace.txt" -- sleep 2 >/dev/null 2>&1 || true
wait $client
echo "server=$SRV client=$client"
grep -c recvfrom "$out/trace.txt"
