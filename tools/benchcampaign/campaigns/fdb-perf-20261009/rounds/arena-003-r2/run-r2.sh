#!/usr/bin/env bash
set -Eeuo pipefail
p=/opt/fdbperf/artifacts/old-batch-attribution-r2-build
b=/opt/fdbperf/artifacts/hillclimb-arena-003-build
exec > >(tee "$p/run-r2.log") 2>&1
trap 'rc=$?; printf "exit_code=%s\nfinished_utc=%s\n" "$rc" "$(date -u +%FT%TZ)" > "$p/run-r2-status.txt"' EXIT
sha256sum -c "$p/input.sha256"
sha256sum -c "$b/binaries.sha256"
sha256sum -c "$b/runtime.sha256"
(cd /opt/fdbperf/sources/hillclimb-arena-003-base;sha256sum -c "$b/full-base.sha256") > "$p/before-source-check.log"
for profile in cpu heap;do
 out=/opt/fdbperf/artifacts/old-batch-attribution-r2-$profile
 printf 'BEGIN_ATTRIBUTION %s %s\n' "$profile" "$(date -u +%FT%TZ)"
 timeout --signal=TERM --kill-after=10s 180s taskset --cpu-list 0-1 python3 /opt/fdbperf/scripts/astra-perf-attribution-r2.py --plan "$p/r2-$profile-plan.json" --output "$out"
 printf 'END_ATTRIBUTION %s %s\n' "$profile" "$(date -u +%FT%TZ)"
 taskset --cpu-list 0-1 python3 "$p/validate-r2.py" "$out" > "$out/validation.stdout"
 cat "$out/ATTRIBUTION.md"
done
sha256sum -c "$p/input.sha256" > "$p/after-input-check.log"
(cd /opt/fdbperf/sources/hillclimb-arena-003-base;sha256sum -c "$b/full-base.sha256") > "$p/after-source-check.log"
printf 'ALL_ATTRIBUTION_VALIDATED %s\n' "$(date -u +%FT%TZ)"
