#!/usr/bin/env bash
set -euo pipefail
W=${FDB_PERF_HOME}/cpu-work
S=${FDB_GO_WORKTREES}/fdb-go-bench267/tools/clientbench/scripts/servercost.py
OUT=$W/fdb3-grv-write-confirm.jsonl
test ! -e "$OUT"
export TMPDIR=$W/tmp
for ((block=0; block<20; block++)); do
    order='base fix'
    if ((block % 2)); then order='fix base'; fi
    for label in $order; do
        binary=$W/bin/clientbench-go-grv-rpc-base
        if [[ $label = fix ]]; then binary=$W/bin/clientbench-go-grv-observe; fi
        python3 "$S" --server-pid 1388261,1388262,1388263 --binary "$binary" --cluster-file "$W/fdb3.cluster" \
            --workload set100 --workers 8 --short 500 --long 6000 --reps 1 --label "$label-b$block" --out "$OUT"
    done
    if ((block % 5 == 0)); then
        python3 "$S" --server-pid 1388261,1388262,1388263 --binary "$W/bin/clientbench-cgo" --cluster-file "$W/fdb3.cluster" \
            --ld-library-path ${HOME}/.local/share/foundationdb/7.3.77/lib \
            --workload set100 --workers 8 --short 500 --long 6000 --reps 1 --label "cgo-b$block" --out "$OUT"
    fi
done
