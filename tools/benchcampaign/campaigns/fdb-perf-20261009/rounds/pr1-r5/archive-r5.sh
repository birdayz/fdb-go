#!/usr/bin/env bash
set -Eeuo pipefail
cd /opt/fdbperf
bundle=pr1-r5
p=artifacts/pr1-r5-build
test ! -e "exports/$bundle.tar.gz"
sha256sum -c "$p/measurement-input.sha256" > "$p/archive-input-check.log"
sha256sum -c "$p/binaries.sha256" > "$p/archive-binaries-check.log"
sha256sum -c "$p/runtime.sha256" > "$p/archive-runtime-check.log"
cp artifacts/ownership-001-build/harness-v6.tar.gz "$p/harness-v6.tar.gz"
cp artifacts/ownership-001-build/harness-v6.sha256 "$p/harness-v6.sha256"
paths=(artifacts/pr1-r5-batchset50 "$p" bin/candidate-pr1-r5-old bin/candidate-pr1-r5-new bin/candidate-v6-cgo scripts/astra-perf-run.py scripts/astra-perf-analyze.py)
find "${paths[@]}" -type f -print0 | sort -z | xargs -0 sha256sum > "exports/$bundle.files.sha256"
sha256sum -c "exports/$bundle.files.sha256" > "exports/$bundle.prearchive-verification.log"
tar -czf "exports/$bundle.tar.gz" "${paths[@]}" "exports/$bundle.files.sha256" "exports/$bundle.prearchive-verification.log"
sha256sum "exports/$bundle.tar.gz" > "exports/$bundle.tar.gz.sha256"
cat "exports/$bundle.tar.gz.sha256"
wc -l "exports/$bundle.files.sha256"
ls -lh "exports/$bundle.tar.gz"
