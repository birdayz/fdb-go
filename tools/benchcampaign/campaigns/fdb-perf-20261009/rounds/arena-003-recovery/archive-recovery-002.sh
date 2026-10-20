#!/usr/bin/env bash
set -Eeuo pipefail
cd /opt/fdbperf
bundle=hillclimb-arena-003-recovery-002
p=artifacts/hillclimb-arena-003-build
r=artifacts/hillclimb-arena-003-diagnostic-report
test ! -e "exports/$bundle.tar.gz"
test ! -e "exports/$bundle.files.sha256"
sha256sum -c "$p/binaries.sha256" > "$r/binaries-check.log"
sha256sum -c "$p/runtime.sha256" > "$r/runtime-check.log"
for arm in base new; do
 (cd sources/hillclimb-arena-003-$arm;sha256sum -c /opt/fdbperf/"$p/full-$arm.sha256") > "$r/full-$arm-source-check.log"
done
paths=(artifacts/hillclimb-arena-003-read-heap artifacts/hillclimb-arena-003-read-analysis "$r")
for file in analyze-read-profiles.sh compile-diagnostic-report.py archive-recovery-002.sh run-profile.sh validate-profile.py validate-report.py read-cpu-plan.json read-heap-plan.json run-read-cpu.log run-read-cpu-status.txt run-read-heap.log run-read-heap-status.txt binaries.sha256 runtime.sha256 full-base.sha256 full-new.sha256 base.sha256 candidate.sha256 arena-003.patch; do
 test -f "$p/$file"
 paths+=("$p/$file")
done
find "${paths[@]}" -type f -print0 | sort -z | xargs -0 sha256sum > "exports/$bundle.files.sha256"
sha256sum -c "exports/$bundle.files.sha256" > "exports/$bundle.prearchive-verification.log"
tar -czf "exports/$bundle.tar.gz" "${paths[@]}" "exports/$bundle.files.sha256" "exports/$bundle.prearchive-verification.log"
sha256sum "exports/$bundle.tar.gz" > "exports/$bundle.tar.gz.sha256"
cat "exports/$bundle.tar.gz.sha256"
wc -l "exports/$bundle.files.sha256"
ls -lh "exports/$bundle.tar.gz"
