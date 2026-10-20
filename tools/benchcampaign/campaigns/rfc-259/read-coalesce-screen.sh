#!/usr/bin/env bash
set -euo pipefail
W=${FDB_PERF_HOME}/cpu-work
S=${FDB_GO_WORKTREES}/fdb-go-bench267/tools/clientbench/scripts/servercost.py
OUT=$W/fdb3-read-coalesce-screen.jsonl
test ! -e "$OUT"
export TMPDIR=$W/tmp
for block in 0 1 2 3 4; do
    order='0 2 5 10 20 cgo'
    if ((block % 2)); then order='cgo 20 10 5 2 0'; fi
    for delay in $order; do
        args=()
        if [[ $delay = cgo ]]; then
            binary=$W/bin/clientbench-cgo
            args+=(--ld-library-path ${HOME}/.local/share/foundationdb/7.3.77/lib)
        else
            binary=$W/bin/clientbench-go-native-schedule
            args+=(--env FDBGO_EXP_WRITE_PORT=4723 --env "FDBGO_EXP_WRITE_DELAY=${delay}us")
        fi
        python3 "$S" --server-pid 1388261,1388262,1388263 --binary "$binary" --cluster-file "$W/fdb3.cluster" \
            --workload get100 --workers 32 --short 20000 --long 60000 --reps 1 --label "$delay-b$block" --out "$OUT" "${args[@]}"
    done
done
