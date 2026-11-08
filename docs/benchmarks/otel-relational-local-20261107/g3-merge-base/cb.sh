#!/bin/bash
# cb.sh ARM MSG R: a fresh run's default collector-bench (post-ingest queries, not gated), then,
# after the queues drain, --query-only on that run (every class x100, gated). ARM base|head, MSG fts|off.
arm=$1 msg=$2 r=$3
cd /var/tmp/otelcmp/g3mb
CF=/var/tmp/otelcmp/g3mb/otelg3mb.cluster
if [ $arm = base ]; then exe="./otelbench-base --backend fdb"; rev=839cc5d3f+harness; else exe=./otelbench-head; rev=8d3959aa6; fi
extra=""; [ $msg = off ] && extra="--enable-message-deltas=false --enable-message-fts=false"
run=g3mb${arm}_${msg}_$r
./measured.sh "$run full" full-$run $exe --mode collector-bench --cluster-file $CF --run $run --conversations 1000 --revision $rev $extra --output full-$run.json
./measured.sh "$run query-only" qo-$run $exe --mode collector-bench --cluster-file $CF --run $run --conversations 1000 --revision $rev $extra --query-only --output qo-$run.json
