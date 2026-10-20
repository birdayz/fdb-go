#!/usr/bin/env bash
set -euo pipefail
W=${FDB_PERF_HOME}/cpu-work
S=${FDB_GO_WORKTREES}/fdb-go-clientcpu/tools/clientbench/scripts/servercost.py
OUT=$W/fdb3-grv-dispatch-screen.jsonl
test ! -e "$OUT"
export TMPDIR=$W/tmp
sha256sum --check "$W/grv-dispatch-binaries.sha256"
for ((block=0; block<10; block++)); do
    order='base dispatch'
    if ((block % 2)); then order='dispatch base'; fi
    for label in $order; do
        binary=clientbench-go-commit-noack
        if [[ $label = dispatch ]]; then binary=clientbench-go-grv-dispatch; fi
        python3 "$S" --server-pid 1388261,1388262,1388263 --binary "$W/bin/$binary" --cluster-file "$W/fdb3.cluster" \
            --workload set100 --workers 32 --short 1000 --long 15000 \
            --reps 1 --label "$label-b$block" --out "$OUT"
    done
    if ((block % 5 == 0)); then
        python3 "$S" --server-pid 1388261,1388262,1388263 --binary "$W/bin/clientbench-cgo" --cluster-file "$W/fdb3.cluster" \
            --ld-library-path ${HOME}/.local/share/foundationdb/7.3.77/lib \
            --workload set100 --workers 32 --short 1000 --long 15000 \
            --reps 1 --label "cgo-b$block" --out "$OUT"
    fi
done
