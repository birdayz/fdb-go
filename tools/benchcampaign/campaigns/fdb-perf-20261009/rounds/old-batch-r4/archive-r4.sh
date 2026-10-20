#!/usr/bin/env bash
set -Eeuo pipefail
cd /opt/fdbperf
bundle=old-batch-r4
p=artifacts/old-batch-r4-build
b=artifacts/hillclimb-arena-003-build
test ! -e "exports/$bundle.tar.gz"
sha256sum -c "$p/input.sha256" > "$p/archive-input-check.log"
sha256sum -c "$b/binaries.sha256" > "$p/archive-binaries-check.log"
sha256sum -c "$b/runtime.sha256" > "$p/archive-runtime-check.log"
paths=(artifacts/old-batch-r4-fp artifacts/old-batch-r4-analysis-fp artifacts/old-batch-r4-analysis-fp-v2 artifacts/old-batch-r4-analysis-fp-v3 "$p" scripts/astra-perf-record-r4.py scripts/astra-perf-run.py)
for f in binaries.sha256 runtime.sha256 full-base.sha256 base.sha256;do paths+=("$b/$f");done
find "${paths[@]}" -type f -print0 | sort -z | xargs -0 sha256sum > "exports/$bundle.files.sha256"
sha256sum -c "exports/$bundle.files.sha256" > "exports/$bundle.prearchive-verification.log"
tar -czf "exports/$bundle.tar.gz" "${paths[@]}" "exports/$bundle.files.sha256" "exports/$bundle.prearchive-verification.log"
sha256sum "exports/$bundle.tar.gz" > "exports/$bundle.tar.gz.sha256"
cat "exports/$bundle.tar.gz.sha256"
wc -l "exports/$bundle.files.sha256"
ls -lh "exports/$bundle.tar.gz"
