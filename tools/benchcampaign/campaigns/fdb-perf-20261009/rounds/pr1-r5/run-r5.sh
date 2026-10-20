#!/usr/bin/env bash
set -Eeuo pipefail
p=/opt/fdbperf/artifacts/pr1-r5-build
out=/opt/fdbperf/artifacts/pr1-r5-batchset50
exec > >(tee "$p/run-r5.log") 2>&1
trap 'rc=$?;printf "exit_code=%s\nfinished_utc=%s\n" "$rc" "$(date -u +%FT%TZ)" > "$p/run-r5-status.txt"' EXIT
sha256sum -c "$p/measurement-input.sha256"
sha256sum -c "$p/binaries.sha256"
sha256sum -c "$p/runtime.sha256"
for arm in old new;do
 (cd /opt/fdbperf/sources/pr1-r5-$arm;find . -type f -print0 | sort -z | xargs -0 sha256sum) > "$p/precampaign-$arm.sha256"
 cmp "$p/full-$arm.sha256" "$p/precampaign-$arm.sha256"
done
printf 'BEGIN_R5 %s\n' "$(date -u +%FT%TZ)"
timeout --signal=TERM --kill-after=10s 180s taskset --cpu-list 0-1 python3 /opt/fdbperf/scripts/astra-perf-run.py --plan "$p/r5-plan.json" --output "$out"
printf 'END_R5_TIMING %s\n' "$(date -u +%FT%TZ)"
sha256sum -c "$p/measurement-input.sha256" > "$p/after-measurement-input-check.log"
for arm in old new;do
 (cd /opt/fdbperf/sources/pr1-r5-$arm;find . -type f -print0 | sort -z | xargs -0 sha256sum) > "$p/postcampaign-$arm.sha256"
 cmp "$p/full-$arm.sha256" "$p/postcampaign-$arm.sha256"
done
taskset --cpu-list 0-1 python3 /opt/fdbperf/scripts/astra-perf-analyze.py --denominator cgo "$out" > "$out/analysis.stdout"
taskset --cpu-list 0-1 python3 "$p/validate-r5.py" "$out" > "$out/validation.stdout"
taskset --cpu-list 0-1 python3 "$p/report-r5.py" "$out" > "$out/r5-report.stdout"
printf 'ALL_R5_VALIDATED %s\n' "$(date -u +%FT%TZ)"
cat "$out/R5-TABLES.md"
