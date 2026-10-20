#!/bin/bash
# Broad closed-loop comparison: Go (BIN_GO) vs libfdb_c (BIN_CGO), all workloads.
set -e
W=${FDB_PERF_HOME}/cpu-work
cd ${FDB_GO_WORKTREES}/fdb-go-wdelay/tools/clientbench/scripts
SRV=$(pgrep -u "$(id -u)" -x fdbserver | paste -sd,)
OUT=${OUT:-$W/broad.jsonl}; rm -f $OUT
iters() { # workload workers -> short long
  case $1 in
    grv) s=$((3000*$2)); [ $s -gt 60000 ] && s=60000;;
    get100) s=$((2000*$2)); [ $s -gt 60000 ] && s=60000;;
    range100) s=$((1000*$2)); [ $s -gt 30000 ] && s=30000;;
    set100) s=$((300*$2)); [ $s -lt 1000 ] && s=1000; [ $s -gt 12000 ] && s=12000;;
  esac
  echo "$s $((s*4))"
}
for b in 0 1 2; do
 for wl in get100 set100 range100 grv; do
  for wk in 1 8 32 64; do
   read -r s l <<<"$(iters $wl $wk)"
   order='go cgo'; ((b % 2)) && order='cgo go'
   for arm in $order; do
    extra=(); bin=$BIN_GO
    [[ $arm = cgo ]] && { bin=$BIN_CGO; extra+=(--ld-library-path ${HOME}/.local/share/foundationdb/7.3.77/lib); }
    TMPDIR=$W/tmp taskset -c 12-15 python3 servercost.py --server-pid $SRV --binary $bin --cluster-file $W/fdb3.cluster \
      --workload $wl --workers $wk --short $s --long $l --reps 1 --label $arm-$wl-$wk-b$b --out $OUT --no-syscall-count "${extra[@]}" >/dev/null
   done
  done
 done
done
