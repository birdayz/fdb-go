#!/usr/bin/env bash
set -euo pipefail
W=${FDB_PERF_HOME}/cpu-work
S=${FDB_GO_WORKTREES}/fdb-go-clientcpu/tools/clientbench/scripts/servercost.py
OUT=$W/fdb3-read-send-noack-screen.jsonl
test ! -e "$OUT"
export TMPDIR=$W/tmp
sha256sum --check "$W/read-send-noack-binaries.sha256"
for ((block=0; block<10; block++)); do
    order='base readnoack'
    if ((block % 2)); then order='readnoack base'; fi
    for workload in get100 grv set100; do
        short=15000
        long=45000
        if [[ $workload = set100 ]]; then short=1000; long=15000; fi
        for label in $order; do
            binary=clientbench-go-commit-noack
            if [[ $label = readnoack ]]; then binary=clientbench-go-read-noack; fi
            python3 "$S" --server-pid 1388261,1388262,1388263 --binary "$W/bin/$binary" --cluster-file "$W/fdb3.cluster" \
                --workload "$workload" --workers 32 --short "$short" --long "$long" \
                --reps 1 --label "$label-b$block" --out "$OUT"
        done
        if ((block % 5 == 0)); then
            python3 "$S" --server-pid 1388261,1388262,1388263 --binary "$W/bin/clientbench-cgo" --cluster-file "$W/fdb3.cluster" \
                --ld-library-path ${HOME}/.local/share/foundationdb/7.3.77/lib \
                --workload "$workload" --workers 32 --short "$short" --long "$long" \
                --reps 1 --label "cgo-b$block" --out "$OUT"
        fi
    done
done
python3 "$W/paired-summary.py" "$OUT" readnoack
