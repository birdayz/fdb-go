#!/usr/bin/env bash
set -Eeuo pipefail
mode=${1:?fp or dwarf}
out=/opt/fdbperf/artifacts/old-batch-r4-analysis-$mode-v3
mkdir "$out"
exec > >(tee "$out/analysis.log") 2>&1
trap 'rc=$?;printf "exit_code=%s\nfinished_utc=%s\n" "$rc" "$(date -u +%FT%TZ)" > "$out/status.txt"' EXIT
for d in /opt/fdbperf/artifacts/old-batch-r4-$mode/b*-old /opt/fdbperf/artifacts/old-batch-r4-$mode/b*-cgo;do
 test -d "$d" || continue
 arm=${d##*-};data="$d/perf.data"
 perf report --no-inline --stdio --stdio-color never --header-only -i "$data" > "$out/$arm-header.txt" 2> "$out/$arm-header.stderr"
 perf report --no-inline --stdio --stdio-color never --no-children --sort dso,sym --percent-limit 0.3 -i "$data" > "$out/$arm-flat-full.txt" 2> "$out/$arm-flat.stderr"
 perf report --no-inline --stdio --stdio-color never --children --sort sym -i "$data" > "$out/$arm-children-full.txt" 2> "$out/$arm-children.stderr"
 perf script --no-inline -i "$data" -F comm,pid,tid,time,event,misc,ip,sym,dso > "$out/$arm-stacks.perf-script" 2> "$out/$arm-stacks.stderr"
 perf script --no-inline -i "$data" -G -F misc,period,event,ip,sym,dso > "$out/$arm-leaves.perf-script" 2> "$out/$arm-leaves.stderr"
 perf script --no-inline -D -i "$data" > "$out/$arm-raw-events.txt" 2> "$out/$arm-raw-events.stderr"
 perf buildid-list -i "$data" > "$out/$arm-buildids.txt" 2> "$out/$arm-buildids.stderr"
 cp "$d/stderr.txt" "$out/$arm-record.stderr"
done
cat /proc/sys/kernel/perf_event_max_sample_rate > "$out/max-sample-rate-after.txt"
printf 'ANALYSIS_%s_COMPLETE %s\n' "$mode" "$(date -u +%FT%TZ)"
