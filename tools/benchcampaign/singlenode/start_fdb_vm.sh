#!/usr/bin/env bash
# Start the one owned fdbserver in the foreground: start_fdb_vm.sh ssd|memory.
set -euo pipefail
engine=${1:?ssd or memory required}
case "$engine" in ssd|memory) ;; *) exit 2 ;; esac
: "${FDB_BENCH_SERVER_ADDR:?owned server host:port, e.g. 192.0.2.1:4500}"
: "${FDB_BENCH_SERVER_CPUS:?server taskset CPU list, e.g. 2-7}"
root=${FDB_BENCH_ROOT:-/srv/fdb-bench}
addr=$FDB_BENCH_SERVER_ADDR
cpus=$FDB_BENCH_SERVER_CPUS
if pgrep -x fdbserver >/dev/null; then printf 'Refusing a second FDB server\n' >&2; exit 1; fi
mkdir -p "$root/cluster/$engine/data" "$root/cluster/$engine/logs"
printf '%s\n' "$$" > "$root/cluster/server.pid"
printf '%s\n' "$engine" > "$root/cluster/engine.txt"
printf '%s\n' "taskset --cpu-list $cpus /usr/sbin/fdbserver -C $root/cluster/fdb.cluster -p $addr -l $addr -d $root/cluster/$engine/data -L $root/cluster/$engine/logs --memory 16GiB --storage-memory 8GiB --cache-memory 2GiB --trace-format json --maxlogssize 256MiB --loggroup rebench-$engine" > "$root/provenance/server-$engine-command.txt"
exec taskset --cpu-list "$cpus" /usr/sbin/fdbserver -C "$root/cluster/fdb.cluster" -p "$addr" -l "$addr" -d "$root/cluster/$engine/data" -L "$root/cluster/$engine/logs" --memory 16GiB --storage-memory 8GiB --cache-memory 2GiB --trace-format json --maxlogssize 256MiB --loggroup "rebench-$engine"
