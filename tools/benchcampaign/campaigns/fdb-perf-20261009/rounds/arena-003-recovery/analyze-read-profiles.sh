#!/usr/bin/env bash
set -Eeuo pipefail
export GOTOOLCHAIN=go1.26.9 GOMAXPROCS=8
out=/opt/fdbperf/artifacts/hillclimb-arena-003-read-analysis
mkdir "$out"
exec > >(tee "$out/analysis.log") 2>&1
trap 'rc=$?; printf "exit_code=%s\nfinished_utc=%s\n" "$rc" "$(date -u +%FT%TZ)" > "$out/status.txt"' EXIT
for arm in old new cgo; do
 case "$arm" in old) binary=/opt/fdbperf/bin/candidate-hc03-base; source=/opt/fdbperf/sources/hillclimb-arena-003-base;;new) binary=/opt/fdbperf/bin/candidate-hc03-arena; source=/opt/fdbperf/sources/hillclimb-arena-003-new;;cgo) binary=/opt/fdbperf/bin/candidate-v6-cgo; source=/opt/fdbperf/sources/hillclimb-arena-003-base;;esac
 cpu=(/opt/fdbperf/artifacts/hillclimb-arena-003-read-cpu/b*-$arm/cpu.pprof)
 heap=(/opt/fdbperf/artifacts/hillclimb-arena-003-read-heap/b*-$arm/heap.pprof)
 test "${#cpu[@]}" -eq 6;test "${#heap[@]}" -eq 6
 go tool pprof -proto -output "$out/$arm-cpu-merged.pprof" "$binary" "${cpu[@]}" 2> "$out/$arm-cpu-merge.stderr"
 go tool pprof -proto -output "$out/$arm-heap-merged.pprof" "$binary" "${heap[@]}" 2> "$out/$arm-heap-merge.stderr"
 for order in top cum; do
  opts=(-top -nodecount=80);if [[ "$order" == cum ]];then opts+=(-cum);fi
  go tool pprof "${opts[@]}" "$binary" "$out/$arm-cpu-merged.pprof" > "$out/$arm-cpu-$order.txt" 2> "$out/$arm-cpu-$order.stderr"
 done
 for index in alloc_space alloc_objects inuse_space inuse_objects; do
  go tool pprof -sample_index="$index" -top -nodecount=80 "$binary" "$out/$arm-heap-merged.pprof" > "$out/$arm-$index-top.txt" 2> "$out/$arm-$index-top.stderr"
  go tool pprof -sample_index="$index" -tree -nodecount=100 "$binary" "$out/$arm-heap-merged.pprof" > "$out/$arm-$index-tree.txt" 2> "$out/$arm-$index-tree.stderr"
 done
 if [[ "$arm" != cgo ]]; then
  go tool pprof -sample_index=alloc_space -source_path="$source" -trim_path=fdb.dev@v0.0.0/ '-list=conflictBufAlloc|resetConflictBuf|captureCommit|ReadTransact|CreateTransaction|opContext' "$binary" "$out/$arm-heap-merged.pprof" > "$out/$arm-heap-critical-source.txt" 2> "$out/$arm-heap-critical-source.stderr"
  go tool pprof -source_path="$source" -trim_path=fdb.dev@v0.0.0/ '-list=conflictBufAlloc|resetConflictBuf|ReadTransact|CreateTransaction|opContext' "$binary" "$out/$arm-cpu-merged.pprof" > "$out/$arm-cpu-critical-source.txt" 2> "$out/$arm-cpu-critical-source.stderr"
  go tool objdump '-s=fdb.dev/pkg/fdbgo/client.\(\*Transaction\).conflictBufAlloc' "$binary" > "$out/$arm-conflictBufAlloc-objdump.txt"
 fi
done
sha256sum /opt/fdbperf/bin/candidate-hc03-base /opt/fdbperf/bin/candidate-hc03-arena /opt/fdbperf/bin/candidate-v6-cgo > "$out/binaries-after.sha256"
find "$out" -name '*.stderr' -size +0 -print
printf 'PROFILE_ANALYSIS_COMPLETE %s\n' "$(date -u +%FT%TZ)"
