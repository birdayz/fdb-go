#!/usr/bin/env bash
set -euo pipefail
W=${FDB_PERF_HOME}/cpu-work
S=${FDB_GO_WORKTREES}/fdb-go-bench267/tools/clientbench/scripts/servercost.py
OUT=$W/fdb3-native-full.jsonl
mkdir -p "$W/tmp"
test ! -e "$OUT"
export TMPDIR=$W/tmp
PIDS=$(pgrep -u "$(id -u)" -x fdbserver | paste -sd,)
test "$(tr ',' '\n' <<<"$PIDS" | wc -l)" = 3
sha256sum "$W/bin/clientbench-go-native-base" "$W/bin/clientbench-go-native-full" "$W/bin/clientbench-cgo" > "$W/native-binaries.sha256"
for spec in '8 2000 8000' '32 6000 24000' '1 300 900'; do
    read -r workers short long <<<"$spec"
    for block in 0 1 2 3 4; do
        order='base full cgo'
        if ((block % 2)); then order='cgo full base'; fi
        for label in $order; do
            args=()
            if [[ $label = cgo ]]; then
                binary=$W/bin/clientbench-cgo
                args+=(--ld-library-path ${HOME}/.local/share/foundationdb/7.3.77/lib)
            else
                binary=$W/bin/clientbench-go-native-$label
            fi
            python3 "$S" --server-pid "$PIDS" --binary "$binary" --cluster-file "$W/fdb3.cluster" \
                --workload set100 --workers "$workers" --short "$short" --long "$long" \
                --reps 1 --label "$label-b$block" --out "$OUT" "${args[@]}"
        done
    done
done
