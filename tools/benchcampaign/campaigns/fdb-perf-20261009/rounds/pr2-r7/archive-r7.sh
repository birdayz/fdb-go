#!/usr/bin/env bash
set -Eeuo pipefail
cd /opt/fdbperf
bundle=pr2-r7
p=artifacts/pr2-r7-evidence
base=artifacts/pr2-r6-build
test ! -e "exports/$bundle.tar.gz"
grep -qx 'exit_code=0' "$p/run-r7-status.txt"
sha256sum -c "$p/r7-input.sha256" > "$p/archive-r7-input-check.log"
sha256sum -c "$base/measurement-input.sha256" > "$p/archive-r6-input-check.log"
sha256sum -c "$base/binaries.sha256" > "$p/archive-binaries-check.log"
sha256sum -c "$base/runtime.sha256" > "$p/archive-runtime-check.log"
ps -eo pid,ppid,comm,args > "$p/archive-processes.txt"
paths=(artifacts/pr2-r6-batchset50x1000 artifacts/pr2-r7-a artifacts/pr2-r7-b artifacts/pr2-r7-analysis "$base" "$p" bin/candidate-pr1-r5-old bin/candidate-pr2-r6-new bin/candidate-v6-cgo scripts/astra-perf-run.py scripts/astra-perf-analyze.py)
find "${paths[@]}" -type f -print0 | sort -z | xargs -0 sha256sum > "exports/$bundle.files.sha256"
sha256sum -c "exports/$bundle.files.sha256" > "exports/$bundle.prearchive-verification.log"
tar -czf "exports/$bundle.tar.gz" "${paths[@]}" "exports/$bundle.files.sha256" "exports/$bundle.prearchive-verification.log"
sha256sum "exports/$bundle.tar.gz" > "exports/$bundle.tar.gz.sha256"
cat "exports/$bundle.tar.gz.sha256"
wc -l "exports/$bundle.files.sha256"
ls -lh "exports/$bundle.tar.gz"
