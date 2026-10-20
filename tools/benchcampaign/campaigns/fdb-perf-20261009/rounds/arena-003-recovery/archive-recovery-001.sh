#!/usr/bin/env bash
set -Eeuo pipefail
cd /opt/fdbperf
bundle=hillclimb-arena-003-recovery-001
p=artifacts/hillclimb-arena-003-build
test ! -e "exports/$bundle.tar.gz";test ! -e "exports/$bundle.files.sha256"
sha256sum -c "$p/binaries.sha256" > "$p/recovery-binaries-check.log"
sha256sum -c "$p/runtime.sha256" > "$p/recovery-runtime-check.log"
for arm in base new; do
 (cd sources/hillclimb-arena-003-$arm;sha256sum -c /opt/fdbperf/"$p/full-$arm.sha256") > "$p/recovery-full-$arm-source-check.log"
done
cp artifacts/ownership-001-build/harness-v6.tar.gz "$p/harness-v6.tar.gz"
cp artifacts/ownership-001-build/harness-v6.sha256 "$p/harness-v6.sha256"
awk '{print $2}' "$p/candidate.sha256" > "$p/candidate.paths"
test ! -e "$p/candidate-client-and-harness.tar.gz"
tar -czf "$p/candidate-client-and-harness.tar.gz" -C sources/hillclimb-arena-003-new -T /opt/fdbperf/"$p/candidate.paths" tools/clientbench
paths=("$p" artifacts/hillclimb-arena-003 artifacts/hillclimb-arena-003-read1000 artifacts/hillclimb-arena-003-get1000 artifacts/hillclimb-arena-003-range100 artifacts/hillclimb-arena-003-set100 artifacts/hillclimb-arena-003-read-cpu bin/candidate-hc03-base bin/candidate-hc03-arena bin/candidate-v6-cgo scripts/astra-perf-run.py scripts/astra-perf-analyze.py)
find "${paths[@]}" -type f -print0 | sort -z | xargs -0 sha256sum > "exports/$bundle.files.sha256"
sha256sum -c "exports/$bundle.files.sha256" > "exports/$bundle.prearchive-verification.log"
tar -czf "exports/$bundle.tar.gz" "${paths[@]}" "exports/$bundle.files.sha256" "exports/$bundle.prearchive-verification.log"
sha256sum "exports/$bundle.tar.gz" > "exports/$bundle.tar.gz.sha256"
cat "exports/$bundle.tar.gz.sha256";wc -l "exports/$bundle.files.sha256";ls -lh "exports/$bundle.tar.gz"
