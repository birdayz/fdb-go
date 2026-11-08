#!/bin/bash
# G2B log ingest vs the merge-base: four alternating invocations (base, head, head, base),
# two fresh runs each, every invocation under measured.sh (load < 12, both locks).
cd /var/tmp/otelcmp/g2bmb
HEAD=/home/birdy/projects/fdb-go-otel-g2bhead   # detached dc6a2f835
BASE=/home/birdy/projects/fdb-go-otel-g2bbase   # detached 839cc5d3f + logs_custom_store_bench_test.go
bench() { # WT BENCH
  env -C "$1" bazelisk test //layers/otel:otel_test --test_arg=-test.run='^$' \
    --test_arg=-test.bench="^$2\$" --test_arg=-test.benchtime=1x \
    --test_output=streamed --nocache_test_results
}
export -f bench
for step in base:1 head:1 head:2 base:2; do
  arm=${step%:*} i=${step#*:}
  if [ $arm = base ]; then wt=$BASE b=BenchmarkLogExportCustomStore; else wt=$HEAD b=BenchmarkLogExport; fi
  ./measured.sh "g2bmb_${arm}_$i" "inv-${arm}_$i" bash -c 'bench "$@"' _ "$wt" "$b"
done
echo campaign done
