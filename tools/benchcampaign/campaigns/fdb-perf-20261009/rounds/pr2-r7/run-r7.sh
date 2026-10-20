#!/usr/bin/env bash
set -Eeuo pipefail
p=/opt/fdbperf/artifacts/pr2-r7-evidence
base=/opt/fdbperf/artifacts/pr2-r6-build
exec > >(tee "$p/run-r7.log") 2>&1
trap 'rc=$?;printf "exit_code=%s\nfinished_utc=%s\n" "$rc" "$(date -u +%FT%TZ)" > "$p/run-r7-status.txt"' EXIT
sha256sum -c "$p/r7-input.sha256"
sha256sum -c "$base/measurement-input.sha256"
sha256sum -c "$base/binaries.sha256"
sha256sum -c "$base/runtime.sha256"
for campaign in a b; do
  out=/opt/fdbperf/artifacts/pr2-r7-$campaign
  for arm in old new; do
    src=/opt/fdbperf/sources/pr2-r6-new
    if [[ "$arm" == old ]]; then src=/opt/fdbperf/sources/pr1-r5-old; fi
    (cd "$src"; find . -type f -print0 | sort -z | xargs -0 sha256sum) > "$p/precampaign-$campaign-$arm.sha256"
    cmp "$base/full-$arm.sha256" "$p/precampaign-$campaign-$arm.sha256"
  done
  printf 'BEGIN_R7_%s %s\n' "$campaign" "$(date -u +%FT%TZ)"
  timeout --signal=TERM --kill-after=10s 180s taskset --cpu-list 0-1 python3 /opt/fdbperf/scripts/astra-perf-run.py --plan "$p/r7-$campaign-plan.json" --output "$out"
  printf 'END_R7_%s_TIMING %s\n' "$campaign" "$(date -u +%FT%TZ)"
  sha256sum -c "$base/measurement-input.sha256" > "$p/after-$campaign-input-check.log"
  sha256sum -c "$base/binaries.sha256" > "$p/after-$campaign-binaries-check.log"
  sha256sum -c "$base/runtime.sha256" > "$p/after-$campaign-runtime-check.log"
  for arm in old new; do
    src=/opt/fdbperf/sources/pr2-r6-new
    if [[ "$arm" == old ]]; then src=/opt/fdbperf/sources/pr1-r5-old; fi
    (cd "$src"; find . -type f -print0 | sort -z | xargs -0 sha256sum) > "$p/postcampaign-$campaign-$arm.sha256"
    cmp "$base/full-$arm.sha256" "$p/postcampaign-$campaign-$arm.sha256"
  done
  taskset --cpu-list 0-1 python3 /opt/fdbperf/scripts/astra-perf-analyze.py --denominator cgo --resamples 10000 "$out" > "$out/analysis.stdout"
  taskset --cpu-list 0-1 python3 "$base/validate-r6.py" "$out" > "$out/validation.stdout"
  printf 'R7_%s_VALIDATED %s\n' "$campaign" "$(date -u +%FT%TZ)"
done
sha256sum -c "$p/r7-input.sha256"
printf 'ALL_R7_VALIDATED %s\n' "$(date -u +%FT%TZ)"
