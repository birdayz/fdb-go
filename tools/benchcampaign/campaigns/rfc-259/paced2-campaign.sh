#!/usr/bin/env bash
set -euo pipefail
W=${FDB_PERF_HOME}/cpu-work
OUT=$W/paced2-campaign.jsonl
test ! -e "$OUT"
export TMPDIR=$W/tmp
sha256sum --check $W/paced2-binaries.sha256
SRV=$(pgrep -u "$(id -u)" -x fdbserver | paste -sd,)
for ((b=0; b<5; b++)); do
  for spec in 'set100 1000 2000 12000' 'set100 2000 4000 24000' 'set100 4000 8000 48000' 'get100 10000 20000 120000' 'get100 30000 60000 360000'; do
    read -r wl rate s l <<<"$spec"
    order='go cgo'; ((b % 2)) && order='cgo go'
    for c in $order; do
      args=(); bin=$W/bin/clientbench-go-paced2
      [[ $c = cgo ]] && { bin=$W/bin/clientbench-cgo-paced2; args+=(--ld-library-path ${HOME}/.local/share/foundationdb/7.3.77/lib); }
      python3 ${FDB_GO_WORKTREES}/fdb-go-clientcpu/tools/clientbench/scripts/servercost.py --no-syscall-count --server-pid "$SRV" --binary "$bin" --cluster-file $W/fdb3.cluster --workload $wl --workers 64 \
        --short $s --long $l --reps 1 --label "$c-r$rate-b$b" --out "$OUT" --client-arg=-rate=$rate --client-arg=-seed=$((b+1)) "${args[@]}"
    done
  done
done
