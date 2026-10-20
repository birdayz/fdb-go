#!/usr/bin/env bash
set -Eeuo pipefail
cd /opt/fdbperf
bundle=pr2-r6
p=artifacts/pr2-r6-build
base=artifacts/pr1-r5-build
test ! -e "exports/$bundle.tar.gz"
sha256sum -c "$p/measurement-input.sha256" > "$p/archive-input-check.log"
sha256sum -c "$p/binaries.sha256" > "$p/archive-binaries-check.log"
sha256sum -c "$p/runtime.sha256" > "$p/archive-runtime-check.log"
for f in old-full.tar.gz old-git-tree.txt old-git-identity.txt harness-v6.tar.gz harness-v6.sha256 binary-old-go-version-m.txt standalone-old.log;do cp "$base/$f" "$p/$f";done
paths=(artifacts/pr2-r6-batchset50 artifacts/pr2-r6-batchset50x1000 "$p" bin/candidate-pr1-r5-old bin/candidate-pr2-r6-new bin/candidate-v6-cgo scripts/astra-perf-run.py scripts/astra-perf-analyze.py)
find "${paths[@]}" -type f -print0 | sort -z | xargs -0 sha256sum > "exports/$bundle.files.sha256"
sha256sum -c "exports/$bundle.files.sha256" > "exports/$bundle.prearchive-verification.log"
tar -czf "exports/$bundle.tar.gz" "${paths[@]}" "exports/$bundle.files.sha256" "exports/$bundle.prearchive-verification.log"
sha256sum "exports/$bundle.tar.gz" > "exports/$bundle.tar.gz.sha256"
cat "exports/$bundle.tar.gz.sha256"
wc -l "exports/$bundle.files.sha256"
ls -lh "exports/$bundle.tar.gz"
