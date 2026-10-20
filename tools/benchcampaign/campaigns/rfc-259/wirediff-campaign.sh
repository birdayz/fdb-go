#!/usr/bin/env bash
set -euo pipefail
W=${FDB_PERF_HOME}/cpu-work
OUT=$W/wirediff-590-vs-cgo.jsonl
test ! -e "$OUT"
export TMPDIR=$W/tmp
PIDS=1863401,1863402,1863403
sha256sum "$W/bin/clientbench-go-commit-noack" "$W/bin/clientbench-cgo" > "$W/wirediff-binaries.sha256"
for rep in 0 1 2; do
  for spec in 'get100 8 6000 18000' 'get100 32 15000 45000' 'grv 8 6000 18000' 'grv 32 15000 45000' \
              'range100 8 6000 18000' 'range100 32 15000 45000' 'set100 8 1000 6000' 'set100 32 1000 15000'; do
    read -r wl wk short long <<<"$spec"
    order='go cgo'
    if ((rep % 2)); then order='cgo go'; fi
    for c in $order; do
      args=()
      if [[ $c = cgo ]]; then
        bin=$W/bin/clientbench-cgo
        args+=(--ld-library-path ${HOME}/.local/share/foundationdb/7.3.77/lib)
      else
        bin=$W/bin/clientbench-go-commit-noack
      fi
      python3 "$W/wirediff.py" --binary "$bin" --label "$c-r$rep" --workload "$wl" --workers "$wk" \
        --short "$short" --long "$long" --server-pid "$PIDS" --cluster-file "$W/fdb3.cluster" --out "$OUT" "${args[@]}"
    done
  done
done
