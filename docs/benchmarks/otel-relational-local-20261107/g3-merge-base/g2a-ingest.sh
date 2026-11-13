#!/bin/bash
# g2a-ingest.sh ARM R: G2A ingest (client CPU via bash time) then audit (logical and prefix bytes), 50k spans.
arm=$1 r=$2
cd /var/tmp/otelcmp/g3mb
CF=/var/tmp/otelcmp/g3mb/otelg3mb.cluster
F="--seed 283 --count 50000 --tenants 10 --attrs 8 --match-every 100 --order time --cluster-file $CF --workers 8 --batch 32 --save-mode batch"
if [ $arm = base ]; then exe=./otelbench-base; be="--backend fdb --posting-value-format tuple --hash-bytes 8 --record-compression brotli-4 --bulk-index-conflicts"; rev=839cc5d3f+harness; run=g2amb_fdbp_$r
else exe=./otelbench-head; be="--backend sql --record-compression zlib-1 --sql-ingest direct --sql-rows compiled"; rev=8d3959aa6; run=g2amb_sqlz1_$r; fi
./measured.sh "$run ingest" ing-$run $exe $be --mode ingest $F --revision $rev --run $run --output ing-$run.json
./measured.sh "$run audit" aud-$run $exe $be --mode audit $F --revision $rev --run $run --output aud-$run.json
