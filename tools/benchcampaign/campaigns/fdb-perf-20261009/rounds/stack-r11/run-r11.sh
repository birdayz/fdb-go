#!/usr/bin/env bash
set -Eeuo pipefail
p=/opt/fdbperf/artifacts/stack-r11-build
exec > >(tee "$p/run-r11.log") 2>&1
trap 'rc=$?;printf "exit_code=%s\nfinished_utc=%s\n" "$rc" "$(date -u +%FT%TZ)" > "$p/run-r11-status.txt"' EXIT
sha256sum -c "$p/measurement-input.sha256"
sha256sum -c "$p/binaries.sha256"
sha256sum -c "$p/runtime.sha256"
for work in read1000 batchset50;do
 out=/opt/fdbperf/artifacts/stack-r11-$work
 for arm in old new;do
  src=/opt/fdbperf/sources/stack-r11-new;if [[ "$arm" == old ]];then src=/opt/fdbperf/sources/pr1-r5-old;fi
  (cd "$src";find . -type f -print0 | sort -z | xargs -0 sha256sum) > "$p/precampaign-$work-$arm.sha256"
  cmp "$p/full-$arm.sha256" "$p/precampaign-$work-$arm.sha256"
 done
 printf 'BEGIN_R11_%s %s\n' "$work" "$(date -u +%FT%TZ)"
 timeout --signal=TERM --kill-after=10s 180s taskset --cpu-list 0-1 python3 /opt/fdbperf/scripts/astra-perf-run.py --plan "$p/r11-$work-plan.json" --output "$out"
 printf 'END_R11_%s_TIMING %s\n' "$work" "$(date -u +%FT%TZ)"
 sha256sum -c "$p/measurement-input.sha256" > "$p/after-$work-input-check.log"
 for arm in old new;do
  src=/opt/fdbperf/sources/stack-r11-new;if [[ "$arm" == old ]];then src=/opt/fdbperf/sources/pr1-r5-old;fi
  (cd "$src";find . -type f -print0 | sort -z | xargs -0 sha256sum) > "$p/postcampaign-$work-$arm.sha256"
  cmp "$p/full-$arm.sha256" "$p/postcampaign-$work-$arm.sha256"
 done
 taskset --cpu-list 0-1 python3 /opt/fdbperf/scripts/astra-perf-analyze.py --denominator cgo "$out" > "$out/analysis.stdout"
 taskset --cpu-list 0-1 python3 "$p/validate-r11.py" "$out" > "$out/validation.stdout"
 printf 'R11_%s_VALIDATED %s\n' "$work" "$(date -u +%FT%TZ)"
done
printf 'ALL_R11_VALIDATED %s\n' "$(date -u +%FT%TZ)"
