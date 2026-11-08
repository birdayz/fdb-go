#!/bin/bash
# g2a-query.sh ARM LABEL: suite all, 1,200 iterations, tenant 0, on run 1 of the arm.
arm=$1 label=$2
cd /var/tmp/otelcmp/g3mb
CF=/var/tmp/otelcmp/g3mb/otelg3mb.cluster
F="--seed 283 --count 50000 --tenants 10 --attrs 8 --match-every 100 --order time --cluster-file $CF --workers 8 --batch 32 --save-mode batch"
if [ $arm = base ]; then exe=./otelbench-base; be="--backend fdb --posting-value-format tuple --hash-bytes 8 --record-compression brotli-4 --bulk-index-conflicts"; rev=839cc5d3f+harness; run=g2amb_fdbp_1; tag=q_fdbp_$label
else exe=./otelbench-head; be="--backend sql --record-compression zlib-1 --sql-ingest direct --sql-rows compiled --sql-query direct"; rev=8d3959aa6; run=g2amb_sqlz1_1; tag=q_sqld_$label; fi
./measured.sh "g2amb_$tag" q-g2amb_$tag $exe $be --mode query $F --suite all --iterations 1200 --revision $rev --run $run --output q-g2amb_$tag.json
