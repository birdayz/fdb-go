#!/usr/bin/env bash
set -euo pipefail
W=${FDB_PERF_HOME}/cpu-work
OUT=$W/wirediff-coalesce-final.jsonl
test ! -e "$OUT"
export TMPDIR=$W/tmp
sha256sum --check "$W/coalesce-final-binaries.sha256"
orders=('off default cgo' 'default cgo off' 'cgo off default')
for rep in 0 1 2; do
  for spec in 'get100 1 2000 6000' 'get100 8 6000 18000' 'get100 32 15000 45000' 'range100 32 15000 45000' \
              'grv 1 2000 6000' 'grv 8 6000 18000' 'set100 1 300 1200' 'set100 8 1000 6000' 'set100 32 1000 15000'; do
    read -r wl wk short long <<<"$spec"
    for arm in ${orders[$rep]}; do
      args=()
      bin=$W/bin/clientbench-go-coalesce-final
      case $arm in
        off) args+=(--client-arg=-coalesce-delay=0,0) ;;
        cgo) bin=$W/bin/clientbench-cgo; args+=(--ld-library-path ${HOME}/.local/share/foundationdb/7.3.77/lib) ;;
      esac
      python3 ${FDB_GO_WORKTREES}/fdb-go-clientcpu/tools/clientbench/scripts/wirediff.py --binary "$bin" --label "$arm-r$rep" --workload "$wl" --workers "$wk" \
        --short "$short" --long "$long" --server-pid 1863401,1863402,1863403 --cluster-file "$W/fdb3.cluster" --out "$OUT" "${args[@]}"
    done
  done
done
