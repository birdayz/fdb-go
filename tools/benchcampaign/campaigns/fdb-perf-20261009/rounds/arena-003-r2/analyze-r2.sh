#!/usr/bin/env bash
set -Eeuo pipefail
export GOTOOLCHAIN=go1.26.9 GOMAXPROCS=8
out=/opt/fdbperf/artifacts/old-batch-attribution-r2-analysis
binary=/opt/fdbperf/bin/candidate-hc03-base
source=/opt/fdbperf/sources/hillclimb-arena-003-base
mkdir "$out"
exec > >(tee "$out/analysis.log") 2>&1
trap 'rc=$?; printf "exit_code=%s\nfinished_utc=%s\n" "$rc" "$(date -u +%FT%TZ)" > "$out/status.txt"' EXIT
for work in batchset50 batchset50x1000;do
 profiles=(/opt/fdbperf/artifacts/old-batch-attribution-r2-cpu/b*-$work-p*-old/cpu.pprof)
 test "${#profiles[@]}" -eq 1;test -s "${profiles[0]}"
 cp "${profiles[0]}" "$out/$work-cpu.pprof"
 for order in flat cum;do
  opts=(-top -nodecount=40);if [[ "$order" == cum ]];then opts+=(-cum);fi
  go tool pprof "${opts[@]}" "$binary" "${profiles[0]}" > "$out/$work-cpu-$order-top40.txt" 2> "$out/$work-cpu-$order-top40.stderr"
 done
 go tool pprof -source_path="$source" -trim_path=fdb.dev@v0.0.0/ '-list=conflictBufAlloc|ownMutationLocked|captureCommit|setPoint' "$binary" "${profiles[0]}" > "$out/$work-cpu-source.txt" 2> "$out/$work-cpu-source.stderr"
done
profiles=(/opt/fdbperf/artifacts/old-batch-attribution-r2-heap/b*-batchset50-p*-old/heap.pprof)
test "${#profiles[@]}" -eq 1;test -s "${profiles[0]}"
cp "${profiles[0]}" "$out/batchset50-heap.pprof"
for order in flat cum;do
 opts=(-top -nodecount=40 -sample_index=inuse_space);if [[ "$order" == cum ]];then opts+=(-cum);fi
 go tool pprof "${opts[@]}" "$binary" "${profiles[0]}" > "$out/batchset50-heap-inuse_space-$order-top40.txt" 2> "$out/batchset50-heap-inuse_space-$order-top40.stderr"
done
for index in alloc_space alloc_objects;do
 go tool pprof -top -nodecount=30 -sample_index="$index" "$binary" "${profiles[0]}" > "$out/batchset50-heap-$index-top30.txt" 2> "$out/batchset50-heap-$index-top30.stderr"
 go tool pprof -top -cum -nodecount=40 -sample_index="$index" "$binary" "${profiles[0]}" > "$out/batchset50-heap-$index-cum-top40.txt" 2> "$out/batchset50-heap-$index-cum-top40.stderr"
done
go tool pprof -sample_index=alloc_space -source_path="$source" -trim_path=fdb.dev@v0.0.0/ '-list=conflictBufAlloc|ownMutationLocked|captureCommit|setPoint' "$binary" "${profiles[0]}" > "$out/batchset50-heap-source.txt" 2> "$out/batchset50-heap-source.stderr"
sha256sum "$binary" > "$out/binary-after.sha256"
find "$out" -name '*.stderr' -size +0 -print
printf 'ATTRIBUTION_ANALYSIS_COMPLETE %s\n' "$(date -u +%FT%TZ)"
