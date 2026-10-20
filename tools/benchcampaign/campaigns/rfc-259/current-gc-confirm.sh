#!/usr/bin/env bash
set -euo pipefail
W=${FDB_PERF_HOME}/cpu-work
S=${FDB_GO_WORKTREES}/fdb-go-bench267/tools/clientbench/scripts/servercost.py
OUT=$W/fdb3-current-gc-confirm.jsonl
test ! -e "$OUT"
export TMPDIR=$W/tmp
sha256sum --check "$W/grv-observe-binaries.sha256"
for spec in '8 500 6000' '32 1000 15000'; do
    read -r workers short long <<<"$spec"
    for ((block=0; block<10; block++)); do
        order='base off'
        if ((block % 2)); then order='off base'; fi
        for label in $order; do
            gc=100
            if [[ $label = off ]]; then gc=off; fi
            python3 "$S" --server-pid 1388261,1388262,1388263 --binary "$W/bin/clientbench-go-grv-observe" --cluster-file "$W/fdb3.cluster" \
                --gogc "$gc" --workload set100 --workers "$workers" --short "$short" --long "$long" \
                --reps 1 --label "$label-b$block" --out "$OUT"
        done
        if ((block % 5 == 0)); then
            python3 "$S" --server-pid 1388261,1388262,1388263 --binary "$W/bin/clientbench-cgo" --cluster-file "$W/fdb3.cluster" \
                --ld-library-path ${HOME}/.local/share/foundationdb/7.3.77/lib \
                --workload set100 --workers "$workers" --short "$short" --long "$long" \
                --reps 1 --label "cgo-b$block" --out "$OUT"
        fi
    done
done
