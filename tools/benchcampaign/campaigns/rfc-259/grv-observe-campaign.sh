#!/usr/bin/env bash
set -euo pipefail
W=${FDB_PERF_HOME}/cpu-work
S=${FDB_GO_WORKTREES}/fdb-go-bench267/tools/clientbench/scripts/servercost.py
OUT=$W/fdb3-grv-observe.jsonl
test ! -e "$OUT"
export TMPDIR=$W/tmp
PIDS=$(pgrep -u "$(id -u)" -x fdbserver | paste -sd,)
test "$(tr ',' '\n' <<<"$PIDS" | wc -l)" = 3
sha256sum "$W/bin/clientbench-go-grv-rpc-base" "$W/bin/clientbench-go-grv-observe" "$W/bin/clientbench-cgo" > "$W/grv-observe-binaries.sha256"
for spec in 'get100 32 20000 60000 10' 'get100 8 15000 45000 5' 'get100 64 30000 90000 5' 'get100 1 3000 9000 5' 'grv 1 5000 15000 5' 'grv 8 15000 45000 5' 'grv 32 20000 60000 5' 'range100 1 1000 3000 5' 'range100 8 10000 30000 5' 'range100 32 20000 60000 5' 'set100 1 100 1200 5' 'set100 8 500 10000 5' 'set100 32 1000 30000 5'; do
    read -r workload workers short long reps <<<"$spec"
    for ((block=0; block<reps; block++)); do
        case $((block % 5)) in
            0) order='base fix cgo';;
            1) order='cgo fix base';;
            2) order='fix base cgo';;
            3) order='cgo base fix';;
            4) order='base cgo fix';;
        esac
        for label in $order; do
            args=()
            case $label in
                cgo) binary=$W/bin/clientbench-cgo; args+=(--ld-library-path ${HOME}/.local/share/foundationdb/7.3.77/lib);;
                base) binary=$W/bin/clientbench-go-grv-rpc-base;;
                fix) binary=$W/bin/clientbench-go-grv-observe;;
            esac
            python3 "$S" --server-pid "$PIDS" --binary "$binary" --cluster-file "$W/fdb3.cluster" \
                --workload "$workload" --workers "$workers" --short "$short" --long "$long" \
                --reps 1 --label "$label-b$block" --out "$OUT" "${args[@]}"
        done
    done
done
