#!/usr/bin/env bash
# One diagnostic 1x run of every testing.B binary in the client namespace.
# These are NOT timing samples.
set -euo pipefail
: "${FDB_BENCH_CLIENT_CPUS:?client taskset CPU list, e.g. 8-15}"
: "${FDB_BENCH_GOMAXPROCS:?GOMAXPROCS for the client, the client CPU count}"
root=${FDB_BENCH_ROOT:-/srv/fdb-bench}
netns=${FDB_BENCH_NETNS:-fdb-bench-client}
mkdir -p "$root/results/smoke"
export FDB_BENCH_CLUSTER_FILE="$root/cluster/fdb.cluster" GOMAXPROCS="$FDB_BENCH_GOMAXPROCS" GOGC=100 GOMEMLIMIT=off GODEBUG= LC_ALL=C TZ=UTC
run_case() {
  local name=$1 binary=$2 pattern=$3
  shift 3
  timeout --signal=TERM --kill-after=5 300 ip netns exec "$netns" taskset --cpu-list "$FDB_BENCH_CLIENT_CPUS" "$root/bin/$binary" -test.run='^$' -test.bench="$pattern" -test.benchtime=1x -test.count=1 -test.benchmem -test.timeout=4m "$@" > "$root/results/smoke/$name.log" 2>&1
  grep -Eq '^Benchmark[^[:space:]]+[[:space:]]+[1-9][0-9]*[[:space:]]+[0-9.]+[[:space:]]+ns/op' "$root/results/smoke/$name.log"
  if grep -Eq '^--- FAIL:|^FAIL$|panic:' "$root/results/smoke/$name.log"; then return 1; fi
  printf 'SMOKE PASS %s\n' "$name"
}
for side in legacy before final; do
  run_case "rl-$side" "rl-$side" '^Benchmark(LoadRecord|SaveRecord|SaveRecordWithIndex|ScanIndexRecords|ScanRecords|ScanRecordsSplitVersioned|SaveSplitRecord)$' --ginkgo.skip='.*'
  run_case "sql-$side" "sql-$side" '^BenchmarkFDB_(PlanCacheHit|IndexScanRange|TimestampInsert)$'
done
for side in pr2 final; do
  run_case "fetch-$side" "fetch-$side" '^BenchmarkIndexFetch$'
done
run_case planskip-final planskip-final '^BenchmarkPlanSkip$'
run_case sqlfetch-final sqlfetch-final '^BenchmarkSQLIndexFetch$'
sha256sum -c "$root/provenance/all-binaries.sha256"
printf 'All benchmark binary smokes passed. Diagnostic 1x runs are NOT timing samples.\n'
