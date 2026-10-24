#!/usr/bin/env bash
# Round 2, goals 2-3: arms A, B, W, S rotated as a Latin square, per cell.
set -euo pipefail
HERE=$(cd "$(dirname "$0")" && pwd)
BIN=${BIN:-/var/tmp/netg-bench/bin/clientbench-go}
CF=${CF:-/var/tmp/netg-fdb/fdb.cluster}
PIDS=${PIDS:?comma-separated fdbserver pids}
OUT=${OUT:-$HERE/raw2}
mkdir -p "$OUT"
orders=(ABWS BWSA WSAB SABW)
cell() { # workload workers short long rounds
  for ((r=0; r<$5; r++)); do
    echo "== $1/$2 round $r $(date -Is) $(cat /proc/loadavg)"
    python3 "$HERE/run_cell.py" --binary "$BIN" --cluster-file "$CF" --server-pids "$PIDS" \
      --workload "$1" --workers "$2" --short "$3" --long "$4" --order "${orders[$((r % 4))]}" \
      --round-len 4 --round-base "$r" --out "$OUT/cells.jsonl"
  done
}
cell set100 8 5000 15000 8
cell get100 64 1000000 3000000 3
cell get100 8 200000 600000 3
cell set100 64 50000 150000 3
