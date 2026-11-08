#!/usr/bin/env bash
# run.sh <run> <backend> <count> [extra otelbench flags...]
# Ingest (CPU, wall), audit (logical bytes), Redwood physical delta.
set -uo pipefail
cd /home/birdy/projects/fdb-go-otel-p0a
B=${B:?set B to the otelbench binary}
CF=/home/birdy/.fdb/otelcmp.cluster
run=$1 be=$2 count=$3; shift 3
F="--seed 283 --count $count --tenants 10 --attrs 8 --match-every 100 --order time --cluster-file $CF"
phys() { docker exec otelcmp fdbcli --exec "status json" --timeout 30 2>/dev/null | jq '[.cluster.processes[].roles[] | select(.role=="storage") | .kvstore_used_bytes] | add'; }
out=/tmp/otelcmp/r
mkdir -p $out
p0=0
s0=$(date +%s.%N)
TIMEFORMAT='%U %S'
cpu=$( { time $B --backend $be --mode ingest $F --workers 8 --batch 32 --save-mode batch "$@" --run $run --output $out/$run-ingest.json >/dev/null 2>&1; } 2>&1 )
wall=$(jq '.wall_ns/1e9' $out/$run-ingest.json)
err=$(jq -r '.error' $out/$run-ingest.json)

p1=0
$B --backend $be --mode audit $F --workers 8 --batch 32 --save-mode batch "$@" --run $run --output $out/$run-audit.json >/dev/null 2>&1
read u s <<<"$cpu"
jq -r --arg run $run --arg u $u --arg s $s --arg wall $wall --arg err "$err" --argjson count $count --argjson p0 $p0 --argjson p1 $p1 '
  .stats.structures as $st |
  "\($run): err=\($err) cpu_us_span=\((($u|tonumber)+($s|tonumber))*1e6/$count|round) wall_s=\($wall|tonumber*10|round/10) " +
  "logical_B_span=\(($st.owned_total.key_bytes+$st.owned_total.value_bytes)/$count|round) keys_span=\($st.owned_total.keys/$count*100|round/100) " +
  "records_B_span=\(($st.records.key_bytes+$st.records.value_bytes)/$count|round) prefix_B_span=\($st.owned_total.prefix_bytes/$count|round)"' $out/$run-audit.json
