#!/usr/bin/env bash
set -Eeuo pipefail
cd /opt/fdbperf
bundle=pr4-r9
p=artifacts/pr4-r9-build
old=artifacts/pr1-r5-build
v6=artifacts/ownership-001-build
test ! -e "exports/$bundle.tar.gz"
grep -qx 'exit_code=0' "$p/run-r9-status.txt"
grep -qx 'exit_code=0' "$p/build-status.txt"
mkdir "$p/reused-old-build" "$p/reused-cgo-build"
for f in old-full.tar.gz old-git-identity.txt old-git-tree.txt binary-old-go-version-m.txt build-r5.sh build.log build-status.txt standalone-old.log harness-old-check.log full-old.sha256; do
  cp "$old/$f" "$p/reused-old-build/$f"
done
for f in harness-v6.tar.gz harness-v6.sha256; do
  cp "$old/$f" "$p/$f"
done
for f in candidate-v6-cgo.go-version-m.txt candidate-v6-cgo.readelf-d.txt candidate-v6-cgo.readelf-l.txt native-ldd.txt go-env.txt build-v6.sh build.log build-status.txt cgo-unit.log new-binaries.sha256 old-client-and-harness.tar.gz old-client-frozen.sha256 old-client-complete-base.patch root-client-frozen.tar.gz root-client-frozen.sha256; do
  cp "$v6/$f" "$p/reused-cgo-build/$f"
done
sha256sum -c "$p/measurement-input.sha256" > "$p/archive-input-check.log"
sha256sum -c "$p/binaries.sha256" > "$p/archive-binaries-check.log"
sha256sum -c "$p/runtime.sha256" > "$p/archive-runtime-check.log"
sha256sum -c "$p/diagnostics-input.sha256" > "$p/archive-diagnostics-input-check.log"
ps -eo pid,ppid,comm,args > "$p/archive-processes.txt"
paths=(artifacts/pr4-r9-read1000 artifacts/pr4-r9-batchset50 artifacts/pr4-r9-strace artifacts/pr4-r9-perf "$p" bin/candidate-pr1-r5-old bin/candidate-pr4-r9-new bin/candidate-v6-cgo scripts/astra-perf-run.py scripts/astra-perf-analyze.py)
find "${paths[@]}" -type f -print0 | sort -z | xargs -0 sha256sum > "exports/$bundle.files.sha256"
sha256sum -c "exports/$bundle.files.sha256" > "exports/$bundle.prearchive-verification.log"
tar -czf "exports/$bundle.tar.gz" "${paths[@]}" "exports/$bundle.files.sha256" "exports/$bundle.prearchive-verification.log"
sha256sum "exports/$bundle.tar.gz" > "exports/$bundle.tar.gz.sha256"
cat "exports/$bundle.tar.gz.sha256"
wc -l "exports/$bundle.files.sha256"
ls -lh "exports/$bundle.tar.gz"
