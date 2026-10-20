#!/usr/bin/env bash
set -Eeuo pipefail
p=/opt/fdbperf/artifacts/old-batch-r3-build
b=/opt/fdbperf/artifacts/hillclimb-arena-003-build
exec > >(tee "$p/run-r3.log") 2>&1
trap 'rc=$?;printf "exit_code=%s\nfinished_utc=%s\n" "$rc" "$(date -u +%FT%TZ)" > "$p/run-r3-status.txt"' EXIT
sha256sum -c "$p/input.sha256"
sha256sum -c "$b/binaries.sha256"
sha256sum -c "$b/runtime.sha256"
(cd /opt/fdbperf/sources/hillclimb-arena-003-base;sha256sum -c "$b/full-base.sha256") > "$p/before-source-check.log"
for kind in timed perf strace;do
 out=/opt/fdbperf/artifacts/old-batch-r3-$kind
 runner=/opt/fdbperf/scripts/astra-perf-run.py
 if [[ "$kind" != timed ]];then runner=/opt/fdbperf/scripts/astra-perf-diagnostic-r3.py;fi
 printf 'BEGIN_%s %s\n' "$kind" "$(date -u +%FT%TZ)"
 timeout --signal=TERM --kill-after=10s 180s taskset --cpu-list 0-1 python3 "$runner" --plan "$p/r3-$kind-plan.json" --output "$out"
 printf 'END_%s %s\n' "$kind" "$(date -u +%FT%TZ)"
 taskset --cpu-list 0-1 python3 "$p/validate-r3.py" "$out" > "$out/validation.stdout"
 if [[ "$kind" == timed ]];then
  taskset --cpu-list 0-1 python3 /opt/fdbperf/scripts/astra-perf-analyze.py --denominator cgo "$out" > "$out/analysis.stdout"
  cat "$out/R3-TABLES.md"
 fi
done
sha256sum -c "$p/input.sha256" > "$p/after-input-check.log"
(cd /opt/fdbperf/sources/hillclimb-arena-003-base;sha256sum -c "$b/full-base.sha256") > "$p/after-source-check.log"
taskset --cpu-list 0-1 python3 "$p/report-instruments-r3.py" /opt/fdbperf/artifacts/old-batch-r3-instruments > "$p/instrument-report.stdout"
printf 'ALL_R3_VALIDATED %s\n' "$(date -u +%FT%TZ)"
cat /opt/fdbperf/artifacts/old-batch-r3-instruments/INSTRUMENTS.md
