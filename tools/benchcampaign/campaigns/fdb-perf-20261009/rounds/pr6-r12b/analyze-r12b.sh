#!/usr/bin/env bash
set -Eeuo pipefail
export GOTOOLCHAIN=go1.26.9 GOMAXPROCS=8
p=/opt/fdbperf/artifacts/pr6-r12b-build
source=/opt/fdbperf/artifacts/pr6-r12b-profile
out=/opt/fdbperf/artifacts/pr6-r12b-profile-analysis
test "$(python3 -B -c 'import json,sys;print(json.load(open(sys.argv[1]))["completed"])' "$source/complete.json")" = 4
mkdir "$out"
exec > >(tee "$out/analysis.log") 2>&1
trap 'rc=$?;printf "exit_code=%s\nfinished_utc=%s\n" "$rc" "$(date -u +%FT%TZ)" > "$out/status.txt"' EXIT
for d in "$source"/0*-batchset50-pprof-cpu;do
 name=${d##*/};arm=new;binary=/opt/fdbperf/bin/candidate-pr6-r12-new
 if [[ "$name" == *-old-* ]];then arm=old;binary=/opt/fdbperf/bin/candidate-pr5-r10b-old;fi
 for order in flat cum;do
  opts=(-top -nodecount=40);if [[ "$order" == cum ]];then opts+=(-cum);fi
  go tool pprof "${opts[@]}" "$binary" "$d/cpu.pprof" > "$out/$name-$order-top40.txt" 2> "$out/$name-$order-top40.stderr"
 done
done
for pair in 1 2;do
 old="$source/00-old-batchset50-pprof-cpu/cpu.pprof";new="$source/01-new-batchset50-pprof-cpu/cpu.pprof"
 if [[ "$pair" == 2 ]];then old="$source/03-old-batchset50-pprof-cpu/cpu.pprof";new="$source/02-new-batchset50-pprof-cpu/cpu.pprof";fi
 go tool pprof -diff_base "$old" -top -nodecount=40 /opt/fdbperf/bin/candidate-pr6-r12-new "$new" > "$out/pair$pair-diff-new-minus-old-top40.txt" 2> "$out/pair$pair-diff-top40.stderr"
done
taskset -c 0-1 python3 -B "$p/pooled-r12b.py" > "$p/pooled.stdout"
sha256sum -c "$p/binaries.sha256" > "$out/binary-after-check.log"
printf 'R12B_ANALYSIS_COMPLETE %s\n' "$(date -u +%FT%TZ)"
