#!/usr/bin/env bash
# Round 2 lever ladder: every arm of ARMS_FILE (default ladder_arms.json) per cell,
# rotating the order each round.
set -euo pipefail
HERE=$(cd "$(dirname "$0")" && pwd)
BIN=${BIN:-/var/tmp/netg-bench/bin/clientbench-go-ladder}
CF=${CF:-/var/tmp/netg-fdb/fdb.cluster}
PIDS=${PIDS:?comma-separated fdbserver pids}
ARMS_FILE=${ARMS_FILE:-$HERE/ladder_arms.json}
OUT=${OUT:-$HERE/raw2/ladder.jsonl}
ROUNDS=${ROUNDS:-2}
ROUND_BASE=${ROUND_BASE:-0}
CELLS=${CELLS:-"get100:64:1000000:3000000 get100:256:1000000:3000000 set100:64:50000:150000 set100:256:100000:300000 get100:8:200000:600000"}
mapfile -t arms < <(python3 -c "import json,sys;print('\n'.join(json.load(open('$ARMS_FILE'))))")
for spec in $CELLS; do
  IFS=: read -r wl w short long <<<"$spec"
  for ((r=ROUND_BASE; r<ROUND_BASE+ROUNDS; r++)); do
    n=${#arms[@]}; k=$(( (r * (n / 2 + 1)) % n ))
    order=$(printf '%s\n' "${arms[@]:$k}" "${arms[@]:0:$k}" | paste -sd,)
    (( r % 2 )) && order=$(echo "$order" | tr , '\n' | tac | paste -sd,)
    echo "== $wl/$w round $r [$order] $(date -Is) $(cat /proc/loadavg)"
    python3 "$HERE/run_cell.py" --binary "$BIN" --cluster-file "$CF" --server-pids "$PIDS" --arms "$ARMS_FILE" \
      --workload "$wl" --workers "$w" --short "$short" --long "$long" --order "$order" \
      --round-len "$n" --round-base "$r" --out "$OUT"
  done
done
