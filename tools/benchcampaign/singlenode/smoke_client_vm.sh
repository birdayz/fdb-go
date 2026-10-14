#!/usr/bin/env bash
# 32-transaction diagnostic run of every client arm and workload, with exact
# value and persistence checks. These are NOT performance samples.
set -euo pipefail
: "${FDB_BENCH_CLIENT_CPUS:?client taskset CPU list, e.g. 8-15}"
: "${FDB_BENCH_GOMAXPROCS:?GOMAXPROCS for the client, the client CPU count}"
: "${FDB_BENCH_GO_VERSION:?Go version the binaries must report, e.g. go1.26.9}"
: "${FDB_BENCH_BEFORE_REVISION:?production revision of the before-go binary}"
: "${FDB_BENCH_FINAL_REVISION:?production revision of the final-go and final-cgo binaries}"
: "${FDB_BENCH_CAMPAIGN_TAG:?short campaign tag for smoke key prefixes, e.g. smoke20261014}"
root=${FDB_BENCH_ROOT:-/srv/fdb-bench}
netns=${FDB_BENCH_NETNS:-fdb-bench-client}
export GOMAXPROCS="$FDB_BENCH_GOMAXPROCS" GOGC=100 GOMEMLIMIT=off GODEBUG= LC_ALL=C TZ=UTC
harness=$(sha256sum "$root/sources/final.tar.gz" | cut -d' ' -f1)
for arm in before-go final-go final-cgo; do
  revision=$FDB_BENCH_FINAL_REVISION
  if [[ $arm == before-go ]]; then revision=$FDB_BENCH_BEFORE_REVISION; fi
  for workload in grv get100 get1000 get10000 read1000 range10 range100 set100 ryw batchset50 batchset50x1000; do
    out="$root/results/smoke-client/$arm-$workload"
    test ! -e "$out"
    timeout --signal=TERM --kill-after=5 60 ip netns exec "$netns" taskset --cpu-list "$FDB_BENCH_CLIENT_CPUS" "$root/bin/client-$arm" --cluster-file "$root/cluster/fdb.cluster" --workload "$workload" --iterations 32 --warmup 8 --workers 1 --output "$out" --run-id "smoke-$arm-$workload" --block 0 --source-revision "$revision" --harness-id "$harness" --allow-writes --transaction-timeout-ms 30000 --retry-limit 10 --prefix "clientbench/$FDB_BENCH_CAMPAIGN_TAG/$arm/$workload/" > "$root/results/smoke-client-$arm-$workload.log" 2>&1
    jq -e '.success==true and .persisted_validation=="ok" and .requested==32 and .completed==32 and .errors==0 and .warmup_completed==8 and .warmup_errors==0' "$out/summary.json" >/dev/null
    jq -e --arg go "$FDB_BENCH_GO_VERSION" --argjson procs "$FDB_BENCH_GOMAXPROCS" '.go_version==$go and .gomaxprocs==$procs' "$out/manifest.json" >/dev/null
    printf 'SMOKE PASS client %s %s 32 measured-diagnostic/8 warmup, exact values and persistence\n' "$arm" "$workload"
  done
done
printf 'Client diagnostic smokes complete; these are NOT performance samples.\n'
