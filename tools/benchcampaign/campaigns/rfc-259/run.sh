#!/bin/sh
# usage: run.sh <binary> <workload> <workers> <iterations> <outdir> [profile]
W=${FDB_PERF_HOME}/cpu-work
rm -rf "$5"
exec env -i PATH=/usr/bin:/bin HOME=$HOME GOMAXPROCS=8 GOGC=${GOGC_OVR:-100} GOMEMLIMIT=off \
  LD_LIBRARY_PATH=${HOME}/.local/share/foundationdb/7.3.77/lib FDBGO_EXP_NODIRECT=${EXP_NODIRECT:-} FDBGO_EXP_COALESCE_US=${EXP_COALESCE:-} FDBGO_EXP_GRV=${EXP_GRV:-} FDBGO_EXP_YIELD=${EXP_YIELD:-} FDBGO_EXP_TRACE=${EXP_TRACE:-} FDBGO_EXP_DIRECT_MAXPENDING=${EXP_DIRECT_MAXPENDING:-} FDBGO_EXP_QUIET_US=${EXP_QUIET:-} FDBGO_EXP_PRECISE=${EXP_PRECISE:-} FDBGO_EXP_BUSYWRITE=${EXP_BUSYWRITE:-} FDBGO_EXP_JOINCTX=${EXP_JOINCTX:-} FDBGO_EXP_INLINE_COMMIT=${EXP_INLINE:-} \
  taskset -c 0-7 "$1" -cluster-file ${CLUSTER:-$W/fdb.cluster} -workload "$2" -workers "$3" \
  -iterations "$4" -warmup $(( $4 / 20 )) -allow-writes -output "$5" -profile "${6:-none}" >"$5.log" 2>&1
