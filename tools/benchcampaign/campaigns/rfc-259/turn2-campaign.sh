#!/bin/bash
# Writer-turn campaign: master Go vs writer turn (turn2) vs libfdb_c (master harness).
set -e
W=${FDB_PERF_HOME}/cpu-work
cd ${FDB_GO_WORKTREES}/fdb-go-wdelay/tools/clientbench/scripts
SRV=$(pgrep -u "$(id -u)" -x fdbserver | paste -sd,)
OUT=$W/results-writer-turn.jsonl; rm -f $OUT
specs=('set100 1000 64 2000 12000' 'set100 2000 64 4000 24000' 'get100 10000 64 20000 120000' 'set100 0 8 2000 12000' 'set100 0 32 4000 40000' 'get100 0 8 20000 120000')
arms=(base turn cgo)
for b in 0 1 2 3; do
 for spec in "${specs[@]}"; do
  read -r wl r wk s l <<<"$spec"
  for k in 0 1 2; do
   arm=${arms[$(( (k + b) % 3 ))]}
   extra=()
   [[ $r != 0 ]] && extra+=(--client-arg=-rate=$r --client-arg=-seed=$((b+1)))
   case $arm in
    base) bin=$W/bin/clientbench-go-master;;
    turn) bin=$W/bin/clientbench-go-turn6;;
    cgo) bin=$W/bin/clientbench-cgo-master; extra+=(--ld-library-path ${HOME}/.local/share/foundationdb/7.3.77/lib);;
   esac
   TMPDIR=$W/tmp taskset -c 12-15 python3 servercost.py --server-pid $SRV --binary $bin \
     --cluster-file $W/fdb3.cluster --workload $wl --workers $wk --short $s --long $l --reps 1 \
     --label $arm-$wl-$r-$wk-b$b --out $OUT --no-syscall-count "${extra[@]}" >/dev/null
  done
 done
done
