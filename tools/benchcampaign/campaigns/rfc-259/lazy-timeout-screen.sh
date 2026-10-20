#!/usr/bin/env bash
set -euo pipefail
W=${FDB_PERF_HOME}/cpu-work
S=${FDB_GO_WORKTREES}/fdb-go-bench267/tools/clientbench/scripts/servercost.py
OUT=$W/fdb3-lazy-timeout-screen.jsonl
test ! -e "$OUT"
export TMPDIR=$W/tmp
sha256sum --check "$W/lazy-timeout-binary.sha256"
for spec in 'get100 32 20000 60000 10' 'get100 64 30000 90000 5' 'range100 32 20000 60000 5' 'grv 32 20000 60000 5' 'set100 8 500 6000 10' 'set100 32 1000 15000 10' 'get100 1 3000 9000 3' 'set100 1 100 1200 3'; do
    read -r workload workers short long reps <<<"$spec"
    for ((block=0; block<reps; block++)); do
        order='base lazy'
        if ((block % 2)); then order='lazy base'; fi
        for label in $order; do
            enabled=0
            if [[ $label = lazy ]]; then enabled=1; fi
            python3 "$S" --server-pid 1388261,1388262,1388263 --binary "$W/bin/clientbench-go-lazy-timeout" --cluster-file "$W/fdb3.cluster" \
                --env FDBGO_EXP_GRV_OBSERVE=yield --env "FDBGO_EXP_LAZY_TIMEOUT=$enabled" \
                --workload "$workload" --workers "$workers" --short "$short" --long "$long" \
                --reps 1 --label "$label-b$block" --out "$OUT"
        done
        if ((block % 5 == 0)); then
            python3 "$S" --server-pid 1388261,1388262,1388263 --binary "$W/bin/clientbench-cgo" --cluster-file "$W/fdb3.cluster" \
                --ld-library-path ${HOME}/.local/share/foundationdb/7.3.77/lib \
                --workload "$workload" --workers "$workers" --short "$short" --long "$long" \
                --reps 1 --label "cgo-b$block" --out "$OUT"
        fi
    done
done
