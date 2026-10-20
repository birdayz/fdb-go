#!/usr/bin/env bash
# usage: splitperf.sh <reps> ; set100/8 split rate under each perf configuration
set -euo pipefail
W=${FDB_PERF_HOME}/cpu-work
SRV=$(pgrep -u "$(id -u)" -x fdbserver | paste -sd,)
reps=$1
TP=cycles
for s in read readv recvfrom recvmsg write writev sendto sendmsg epoll_wait epoll_pwait epoll_ctl futex nanosleep clock_nanosleep sched_yield io_submit io_getevents fdatasync pwrite64; do TP="$TP,syscalls:sys_enter_$s"; done
one() {
  local arm=$1 cfg=$2 bin=$3 ld=$4
  local out=$W/splitperf/$arm-$cfg-$i
  rm -rf "$out"
  local client=(env -i PATH=/usr/bin:/bin HOME=$HOME GOMAXPROCS=8 GOGC=100 GOMEMLIMIT=off LD_LIBRARY_PATH=$ld
    taskset -c 0-7 "$bin" -cluster-file $W/fdb3.cluster -workload set100 -workers 8
    -iterations 6000 -warmup 300 -allow-writes -output "$out")
  case $cfg in
    none) "${client[@]}" ;;
    srv-cycles) perf stat -x, -o /dev/null -p "$SRV" -e cycles,instructions,task-clock,raw_syscalls:sys_enter -- "${client[@]}" ;;
    srv-tp) perf stat -x, -o /dev/null --per-thread -p "$SRV" -e "$TP" -- "${client[@]}" ;;
    cli-tp) perf stat -x, -o /dev/null -e "$TP" -- "${client[@]}" ;;
  esac > "$out.log" 2>&1
  python3 - "$out" "$arm" "$cfg" <<'EOF'
import csv, statistics, sys
d, arm, cfg = sys.argv[1:]
lat = [int(r['latency_ns']) for r in csv.DictReader(open(f'{d}/samples.csv'))]
print(arm, cfg, f'{statistics.fmean(lat)/1e6:.2f}')
EOF
}
mkdir -p $W/splitperf
for ((i=0; i<reps; i++)); do
  for cfg in none srv-cycles srv-tp cli-tp; do
    one go $cfg $W/bin/clientbench-go-coalesce-final ''
    one cgo $cfg $W/bin/clientbench-cgo ${HOME}/.local/share/foundationdb/7.3.77/lib
  done
done
