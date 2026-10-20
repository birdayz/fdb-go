#!/usr/bin/env bash
# usage: splitrate.sh <reps> ; set100/8 runs, prints arm, run, mean latency ms, tx/s
set -euo pipefail
W=${FDB_PERF_HOME}/cpu-work
reps=$1
run() {
  local arm=$1 bin=$2 gogc=$3 ld=$4; shift 4
  local out=$W/splitrate/$arm-$i
  rm -rf "$out"
  env -i PATH=/usr/bin:/bin HOME=$HOME GOMAXPROCS=8 GOGC=$gogc GOMEMLIMIT=off LD_LIBRARY_PATH=$ld \
    taskset -c 0-7 "$bin" -cluster-file $W/fdb3.cluster -workload set100 -workers 8 \
    -iterations 6000 -warmup 300 -allow-writes -output "$out" "$@" > "$out.log" 2>&1
  python3 - "$out" "$arm" "$i" <<'EOF'
import csv, json, statistics, sys
d, arm, i = sys.argv[1:]
s = json.load(open(f'{d}/summary.json'))
lat = [int(r['latency_ns']) for r in csv.DictReader(open(f'{d}/samples.csv'))]
print(arm, i, f'{statistics.fmean(lat)/1e6:.2f}', round(s['completed'] / (s['measured_wall_ns'] / 1e9)))
EOF
}
mkdir -p $W/splitrate
for ((i=0; i<reps; i++)); do
  run go $W/bin/clientbench-go-coalesce-final 100 ''
  run cgo $W/bin/clientbench-cgo 100 ${HOME}/.local/share/foundationdb/7.3.77/lib
  run go-gcoff $W/bin/clientbench-go-coalesce-final off ''
done
