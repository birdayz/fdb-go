#!/usr/bin/env bash
set -euo pipefail
W=${FDB_PERF_HOME}/cpu-work
S=${FDB_GO_WORKTREES}/fdb-go-bench267/tools/clientbench/scripts/servercost.py
OUT=$W/fdb3-header-reserve-screen.jsonl
test ! -e "$OUT"
export TMPDIR=$W/tmp
sha256sum --check "$W/header-reserve-binary.sha256"
for spec in '8 500 6000 10' '32 1000 15000 10' '1 100 1200 3'; do
    read -r workers short long reps <<<"$spec"
    for ((block=0; block<reps; block++)); do
        case $((block % 6)) in
            0) order='base prefetch pad';;
            1) order='pad prefetch base';;
            2) order='prefetch base pad';;
            3) order='pad base prefetch';;
            4) order='base pad prefetch';;
            5) order='prefetch pad base';;
        esac
        for label in $order; do
            mode=$label
            if [[ $label = base ]]; then mode=''; fi
            python3 "$S" --server-pid 1388261,1388262,1388263 --binary "$W/bin/clientbench-go-header-reserve" --cluster-file "$W/fdb3.cluster" \
                --env FDBGO_EXP_GRV_OBSERVE=yield --env FDBGO_EXP_LAZY_TIMEOUT=0 --env FDBGO_EXP_COMMIT_YIELD= --env "FDBGO_EXP_HEADER_RESERVE=$mode" \
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
