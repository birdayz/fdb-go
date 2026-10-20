#!/usr/bin/env bash
set -euo pipefail
W=${FDB_PERF_HOME}/cpu-work
S=${FDB_GO_WORKTREES}/fdb-go-bench267/tools/clientbench/scripts/servercost.py
OUT=$W/fdb3-grv-observe-64.jsonl
test ! -e "$OUT"
export TMPDIR=$W/tmp
sha256sum --check "$W/grv-observe-binaries.sha256"
for spec in 'grv 30000 90000' 'range100 30000 90000' 'set100 1000 30000'; do
    read -r workload short long <<<"$spec"
    for ((block=0; block<5; block++)); do
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
            python3 "$S" --server-pid 1388261,1388262,1388263 --binary "$binary" --cluster-file "$W/fdb3.cluster" \
                --workload "$workload" --workers 64 --short "$short" --long "$long" \
                --reps 1 --label "$label-b$block" --out "$OUT" "${args[@]}"
        done
    done
done
