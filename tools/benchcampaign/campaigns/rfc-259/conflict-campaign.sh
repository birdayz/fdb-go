#!/usr/bin/env bash
set -euo pipefail
W=${FDB_PERF_HOME}/cpu-work
S=${FDB_GO_WORKTREES}/fdb-go-bench267/tools/clientbench/scripts/servercost.py
OUT=$W/fdb3-conflict-alloc.jsonl
mkdir -p "$W/tmp"
test ! -e "$OUT"
export TMPDIR=$W/tmp
PIDS=$(pgrep -u "$(id -u)" -x fdbserver | paste -sd,)
test "$(tr ',' '\n' <<<"$PIDS" | wc -l)" = 3
sha256sum "$W/bin/clientbench-go-conflict-base" "$W/bin/clientbench-go-conflict-fix" "$W/bin/clientbench-cgo" > "$W/conflict-binaries.sha256"
for spec in '8 2000 8000' '32 6000 24000' '1 300 900'; do
    read -r workers short long <<<"$spec"
    for block in 0 1 2 3 4; do
        order='base fix cgo'
        if ((block % 2)); then order='cgo fix base'; fi
        for label in $order; do
            args=()
            if [[ $label = cgo ]]; then
                binary=$W/bin/clientbench-cgo
                args+=(--ld-library-path ${HOME}/.local/share/foundationdb/7.3.77/lib)
            else
                binary=$W/bin/clientbench-go-conflict-$label
            fi
            python3 "$S" --server-pid "$PIDS" --binary "$binary" --cluster-file "$W/fdb3.cluster" \
                --workload set100 --workers "$workers" --short "$short" --long "$long" \
                --reps 1 --label "$label-b$block" --out "$OUT" "${args[@]}"
        done
    done
done
