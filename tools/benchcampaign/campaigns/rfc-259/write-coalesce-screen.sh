#!/usr/bin/env bash
set -euo pipefail
W=${FDB_PERF_HOME}/cpu-work
OUT=$W/wirediff-coalesce-screen.jsonl
test ! -e "$OUT"
export TMPDIR=$W/tmp
sha256sum --check "$W/write-coalesce-binaries.sha256"
for rep in 0 1 2; do
  for spec in 'get100 1 2000 6000' 'get100 8 6000 18000' 'get100 32 15000 45000' 'range100 32 15000 45000' \
              'grv 8 6000 18000' 'set100 1 300 1200' 'set100 8 1000 6000' 'set100 32 1000 15000'; do
    read -r wl wk short long <<<"$spec"
    order='base coalesce'
    if ((rep % 2)); then order='coalesce base'; fi
    for c in $order; do
      bin=$W/bin/clientbench-go-commit-noack
      [[ $c = coalesce ]] && bin=$W/bin/clientbench-go-coalesce
      python3 "$W/wirediff.py" --binary "$bin" --label "$c-r$rep" --workload "$wl" --workers "$wk" \
        --short "$short" --long "$long" --server-pid 1863401,1863402,1863403 --cluster-file "$W/fdb3.cluster" --out "$OUT"
    done
  done
done
