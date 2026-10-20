#!/bin/sh
# usage: sc.sh <binary> <workload> <iterations>  -> per-tx syscall counts (incl. 5% warmup)
W=${FDB_PERF_HOME}/cpu-work; out=$W/sc-tmp; rm -rf $out
n=$(( $3 + $3 / 20 ))
env -i PATH=/usr/bin:/bin HOME=$HOME GOMAXPROCS=8 GOGC=100 GOMEMLIMIT=off LD_LIBRARY_PATH=${HOME}/.local/share/foundationdb/7.3.77/lib \
  taskset -c 0-7 perf stat -x, -e raw_syscalls:sys_enter,syscalls:sys_enter_write,syscalls:sys_enter_writev,syscalls:sys_enter_sendmsg,syscalls:sys_enter_sendto,syscalls:sys_enter_read,syscalls:sys_enter_recvmsg,syscalls:sys_enter_recvfrom,syscalls:sys_enter_futex,syscalls:sys_enter_epoll_pwait,syscalls:sys_enter_epoll_wait,syscalls:sys_enter_nanosleep,syscalls:sys_enter_sched_yield \
  "$1" -cluster-file $W/fdb.cluster -workload "$2" -workers 32 -iterations "$3" -warmup $(( $3 / 20 )) -allow-writes -output $out 2>&1 >/dev/null | \
  awk -F, -v n=$n -v tag="$(basename $1) $2" '$3 ~ /sys_enter/ { v=$1/n; if (v >= 0.05) { sub("syscalls:sys_enter_","",$3); sub("raw_syscalls:sys_enter","TOTAL",$3); s = s sprintf(" %s=%.2f", $3, v) } } END { print tag ":" s }'
