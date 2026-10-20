#!/usr/bin/env bash
set -euo pipefail
W=${FDB_PERF_HOME}/cpu-work
S=${FDB_GO_WORKTREES}/fdb-go-clientcpu/tools/clientbench/scripts/servercost.py
OUT=$W/fdb3-commit-noack-confirm.jsonl
test ! -e "$OUT"
export TMPDIR=$W/tmp
sha256sum --check "$W/commit-send-noack-binaries.sha256"
for spec in '8 500 6000 5 20' '1 100 700 3 10' '64 1000 20000 0 10'; do
    read -r workers short long first blocks <<<"$spec"
    for ((block=first; block<blocks; block++)); do
        order='base noack'
        if ((block % 2)); then order='noack base'; fi
        for label in $order; do
            binary=clientbench-go-grv-observe
            if [[ $label = noack ]]; then binary=clientbench-go-commit-noack; fi
            python3 "$S" --server-pid 1388261,1388262,1388263 --binary "$W/bin/$binary" --cluster-file "$W/fdb3.cluster" \
                --workload set100 --workers "$workers" --short "$short" --long "$long" \
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
