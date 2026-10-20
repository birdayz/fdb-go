#!/bin/sh
# usage: [EXP_*=..] sc2.sh <binary> <workload> <workers> <iterations> -> client per-tx syscalls + server per-tx syscalls
W=${FDB_PERF_HOME}/cpu-work; cd $W
n=$(( $4 + $4 / 20 ))
SPID=$(pgrep -u $(id -u) -x fdbserver)
perf stat -x, -p $SPID -e syscalls:sys_enter_sendmsg,syscalls:sys_enter_recvmsg,syscalls:sys_enter_epoll_wait,syscalls:sys_enter_epoll_pwait,syscalls:sys_enter_write,syscalls:sys_enter_read,syscalls:sys_enter_writev,syscalls:sys_enter_readv,syscalls:sys_enter_sendto,syscalls:sys_enter_recvfrom -o sc2-srv.txt &
P=$!
sleep 0.3
perf stat -x, -e syscalls:sys_enter_write,syscalls:sys_enter_writev,syscalls:sys_enter_sendmsg,syscalls:sys_enter_read,syscalls:sys_enter_recvmsg,syscalls:sys_enter_futex,syscalls:sys_enter_epoll_pwait,syscalls:sys_enter_epoll_wait -o sc2-cli.txt -- ./run.sh $1 $2 $3 $4 sc2out
kill -INT $P; wait $P 2>/dev/null
for f in cli srv; do awk -F, -v n=$n -v tag="$f" '$3 ~ /sys_enter/ { v=$1/n; if (v >= 0.01) { sub("syscalls:sys_enter_","",$3); s = s sprintf(" %s=%.3f", $3, v) } } END { print tag ":" s }' sc2-$f.txt; done
