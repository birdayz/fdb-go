#!/usr/bin/env bash
# Sequential campaign: 4 cells x ABABAB, then client syscall counts (ABAB).
set -euo pipefail
HERE=$(cd "$(dirname "$0")" && pwd)
BIN=${BIN:-/var/tmp/netg-bench/bin/clientbench-go}
CF=${CF:-/var/tmp/netg-fdb/fdb.cluster}
PIDS=${PIDS:?comma-separated fdbserver pids}
OUT=${OUT:-$HERE/raw}
mkdir -p "$OUT"
for spec in 'get100 8 200000 600000' 'get100 64 1000000 3000000' 'set100 8 5000 15000' 'set100 64 50000 150000'; do
  read -r wl w short long <<<"$spec"
  echo "== $wl/$w $(date -Is) $(cat /proc/loadavg)"
  python3 "$HERE/run_cell.py" --binary "$BIN" --cluster-file "$CF" --server-pids "$PIDS" \
    --workload "$wl" --workers "$w" --short "$short" --long "$long" --order ABABAB --out "$OUT/cells.jsonl"
done
for spec in 'get100 8 30000 100000' 'get100 64 100000 400000' 'set100 8 2000 6000' 'set100 64 10000 40000'; do
  read -r wl w short long <<<"$spec"
  echo "== syscalls $wl/$w $(date -Is) $(cat /proc/loadavg)"
  python3 "$HERE/run_cell.py" --binary "$BIN" --cluster-file "$CF" --server-pids "$PIDS" --syscalls \
    --workload "$wl" --workers "$w" --short "$short" --long "$long" --order ABAB --out "$OUT/syscalls.jsonl"
done
