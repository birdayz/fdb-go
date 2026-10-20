#!/usr/bin/env bash
set -euo pipefail
W=${FDB_PERF_HOME}/cpu-work
S=${FDB_GO_WORKTREES}/fdb-go-bench267/tools/clientbench/scripts/servercost.py
OUT=$W/fdb3-grv-sample-screen.jsonl
test ! -e "$OUT"
export TMPDIR=$W/tmp
for block in 0 1 2 3 4; do
    order='base late yield cgo'
    if ((block % 2)); then order='cgo yield late base'; fi
    for mode in $order; do
        args=()
        if [[ $mode = cgo ]]; then
            binary=$W/bin/clientbench-cgo
            args+=(--ld-library-path ${HOME}/.local/share/foundationdb/7.3.77/lib)
        else
            binary=$W/bin/clientbench-go-grv-sample
            if [[ $mode != base ]]; then args+=(--env "FDBGO_EXP_GRV_OBSERVE=$mode"); fi
        fi
        python3 "$S" --server-pid 1388261,1388262,1388263 --binary "$binary" --cluster-file "$W/fdb3.cluster" \
            --workload get100 --workers 32 --short 20000 --long 60000 --reps 1 --label "$mode-b$block" --out "$OUT" "${args[@]}"
    done
done
