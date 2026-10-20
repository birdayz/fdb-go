#!/usr/bin/env bash
set -Eeuo pipefail
export GOTOOLCHAIN=go1.26.9 GOMAXPROCS=8
p=/opt/fdbperf/artifacts/stack-r14-build
source=/opt/fdbperf/artifacts/stack-r14
out=/opt/fdbperf/artifacts/stack-r14-analysis
binary=/opt/fdbperf/bin/candidate-stack-r11v2-new
test "$(python3 -B -c 'import json,sys;print(json.load(open(sys.argv[1]))["completed"])' "$source/complete.json")" = 6
mkdir "$out"
exec > >(tee "$out/analysis.log") 2>&1
trap 'rc=$?;printf "exit_code=%s\nfinished_utc=%s\n" "$rc" "$(date -u +%FT%TZ)" > "$out/status.txt"' EXIT
for d in "$source"/0*-new-*-pprof-*;do
 name=${d##*/};profile=cpu
 if [[ "$name" == *-heap ]];then profile=heap;fi
 indices=(cpu);if [[ "$profile" == heap ]];then indices=(alloc_space alloc_objects inuse_space inuse_objects);fi
 for index in "${indices[@]}";do
  for order in flat cum;do
   opts=(-top -nodecount=40 -sample_index="$index");if [[ "$order" == cum ]];then opts+=(-cum);fi
   go tool pprof "${opts[@]}" "$binary" "$d/$profile.pprof" > "$out/$name-$index-$order-top40.txt" 2> "$out/$name-$index-$order-top40.stderr"
  done
 done
 go tool pprof -raw "$binary" "$d/$profile.pprof" > "$out/$name-raw.txt" 2> "$out/$name-raw.stderr"
 python3 -B "$p/fold-pprof.py" "$d/$profile.pprof" "$out/$name-folded" > "$out/$name-fold-validation.stdout"
done
for arm in new cgo;do
 index=04;if [[ "$arm" == cgo ]];then index=05;fi
 d="$source/$index-$arm-read1000-perf-none";data="$d/perf.data"
 perf report --no-inline --stdio --stdio-color never --header-only -i "$data" > "$out/$arm-header.txt" 2> "$out/$arm-header.stderr"
 perf report --no-inline --stdio --stdio-color never --no-children --sort dso,sym --percent-limit 0.3 -i "$data" > "$out/$arm-flat-full.txt" 2> "$out/$arm-flat.stderr"
 perf report --no-inline --stdio --stdio-color never --children --sort sym -i "$data" > "$out/$arm-children-full.txt" 2> "$out/$arm-children.stderr"
 perf script --no-inline -i "$data" -F comm,pid,tid,time,event,misc,ip,sym,dso > "$out/$arm-stacks.perf-script" 2> "$out/$arm-stacks.stderr"
 perf script --no-inline -i "$data" -G -F misc,period,event,ip,sym,dso > "$out/$arm-leaves.perf-script" 2> "$out/$arm-leaves.stderr"
 perf script --no-inline -D -i "$data" > "$out/$arm-raw-events.txt" 2> "$out/$arm-raw-events.stderr"
 perf buildid-list -i "$data" > "$out/$arm-buildids.txt" 2> "$out/$arm-buildids.stderr"
 cp "$d/stderr.txt" "$out/$arm-record.stderr"
done
python3 -B "$p/fold-perf.py" "$out" > "$out/fold-perf-validation.stdout"
cat /proc/sys/kernel/perf_event_max_sample_rate > "$out/max-sample-rate-after.txt"
sha256sum "$binary" /opt/fdbperf/bin/candidate-v6-cgo > "$out/binary-after.sha256"
printf 'R14_ANALYSIS_COMPLETE %s\n' "$(date -u +%FT%TZ)"
