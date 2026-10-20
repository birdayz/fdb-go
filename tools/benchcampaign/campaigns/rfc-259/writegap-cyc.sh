#!/usr/bin/env bash
set -euo pipefail
W=${FDB_PERF_HOME}/cpu-work
OUT=$W/writegap-cyc.jsonl
test ! -e "$OUT"
export TMPDIR=$W/tmp
SRV=$(pgrep -u "$(id -u)" -x fdbserver | paste -sd,)
for ((b=0; b<10; b++)); do
  for spec in '8 1000 6000' '32 1000 15000'; do
    read -r wk s l <<<"$spec"
    order='go cgo'; ((b % 2)) && order='cgo go'
    for c in $order; do
      args=(); bin=$W/bin/clientbench-go-coalesce-final
      [[ $c = cgo ]] && { bin=$W/bin/clientbench-cgo; args+=(--ld-library-path ${HOME}/.local/share/foundationdb/7.3.77/lib); }
      python3 $W/servercost-cyc.py --server-pid "$SRV" --binary "$bin" --cluster-file $W/fdb3.cluster --workload set100 --workers $wk --short $s --long $l --reps 1 --label "$c-b$b" --out "$OUT" "${args[@]}"
    done
  done
done
