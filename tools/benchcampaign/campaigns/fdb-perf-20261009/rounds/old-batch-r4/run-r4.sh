#!/usr/bin/env bash
set -Eeuo pipefail
mode=${1:?fp or dwarf}
case "$mode" in fp|dwarf);;*)exit 2;;esac
p=/opt/fdbperf/artifacts/old-batch-r4-build
b=/opt/fdbperf/artifacts/hillclimb-arena-003-build
out=/opt/fdbperf/artifacts/old-batch-r4-$mode
exec > >(tee "$p/run-r4-$mode.log") 2>&1
trap 'rc=$?;printf "exit_code=%s\nfinished_utc=%s\n" "$rc" "$(date -u +%FT%TZ)" > "$p/run-r4-'"$mode"'-status.txt"' EXIT
sha256sum -c "$p/input.sha256"
sha256sum -c "$b/binaries.sha256"
sha256sum -c "$b/runtime.sha256"
(cd /opt/fdbperf/sources/hillclimb-arena-003-base;sha256sum -c "$b/full-base.sha256") > "$p/before-$mode-source-check.log"
printf 'BEGIN_R4_%s %s\n' "$mode" "$(date -u +%FT%TZ)"
timeout --signal=TERM --kill-after=10s 180s taskset --cpu-list 0-1 python3 /opt/fdbperf/scripts/astra-perf-record-r4.py --plan "$p/r4-$mode-plan.json" --output "$out"
printf 'END_R4_%s %s\n' "$mode" "$(date -u +%FT%TZ)"
taskset --cpu-list 0-1 python3 "$p/validate-r4.py" "$out" > "$out/validation.stdout"
sha256sum -c "$p/input.sha256" > "$p/after-$mode-input-check.log"
(cd /opt/fdbperf/sources/hillclimb-arena-003-base;sha256sum -c "$b/full-base.sha256") > "$p/after-$mode-source-check.log"
printf 'ALL_R4_%s_VALIDATED %s\n' "$mode" "$(date -u +%FT%TZ)"
