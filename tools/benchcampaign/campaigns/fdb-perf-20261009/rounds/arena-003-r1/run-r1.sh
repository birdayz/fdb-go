#!/usr/bin/env bash
set -Eeuo pipefail
p=/opt/fdbperf/artifacts/hillclimb-arena-003-r1-build
b=/opt/fdbperf/artifacts/hillclimb-arena-003-build
out=/opt/fdbperf/artifacts/hillclimb-arena-003-read-aa-r1
exec > >(tee "$p/run-r1.log") 2>&1
trap 'rc=$?; printf "exit_code=%s\nfinished_utc=%s\n" "$rc" "$(date -u +%FT%TZ)" > "$p/run-r1-status.txt"' EXIT
sha256sum -c "$p/input.sha256"
sha256sum -c "$b/binaries.sha256"
sha256sum -c "$b/runtime.sha256"
for arm in base new; do
 (cd /opt/fdbperf/sources/hillclimb-arena-003-$arm; sha256sum -c "$b/full-$arm.sha256") > "$p/before-$arm-source-check.log"
done
printf 'BEGIN R1 %s\n' "$(date -u +%FT%TZ)"
timeout --signal=TERM --kill-after=10s 180s taskset --cpu-list 0-1 python3 /opt/fdbperf/scripts/astra-perf-run-r1.py --plan "$p/r1-plan.json" --output "$out"
printf 'END_TIMING R1 %s\n' "$(date -u +%FT%TZ)"
sha256sum -c "$p/input.sha256" > "$p/after-input-check.log"
for arm in base new; do
 (cd /opt/fdbperf/sources/hillclimb-arena-003-$arm; sha256sum -c "$b/full-$arm.sha256") > "$p/after-$arm-source-check.log"
done
taskset --cpu-list 0-1 python3 /opt/fdbperf/scripts/astra-perf-analyze.py --denominator cgo "$out" > "$out/analysis.stdout"
taskset --cpu-list 0-1 python3 "$p/validate-r1.py" "$out" > "$out/validation.stdout"
taskset --cpu-list 0-1 python3 "$p/report-r1.py" "$out" > "$out/r1-report.stdout"
printf 'COMPLETE_VALIDATED R1 %s\n' "$(date -u +%FT%TZ)"
cat "$out/R1-TABLES.md"
