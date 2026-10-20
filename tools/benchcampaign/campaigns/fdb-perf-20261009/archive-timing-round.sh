#!/usr/bin/env bash
set -Eeuo pipefail
cd /opt/fdbperf
bundle=${1:?bundle}
round=${2:?round}
p=artifacts/$bundle-build
v6=artifacts/ownership-001-build
test ! -e "exports/$bundle.tar.gz"
grep -qx 'exit_code=0' "$p/run-$round-status.txt"
grep -qx 'exit_code=0' "$p/build-status.txt"
mkdir "$p/reused-cgo-build"
for f in harness-v6.tar.gz harness-v6.sha256;do cp "$v6/$f" "$p/$f";done
for f in candidate-v6-cgo.go-version-m.txt candidate-v6-cgo.readelf-d.txt candidate-v6-cgo.readelf-l.txt native-ldd.txt go-env.txt build-v6.sh build.log build-status.txt cgo-unit.log new-binaries.sha256 old-client-and-harness.tar.gz old-client-frozen.sha256 old-client-complete-base.patch root-client-frozen.tar.gz root-client-frozen.sha256;do
 cp "$v6/$f" "$p/reused-cgo-build/$f"
done
if [[ "$bundle" != pr5-r10b ]];then
 mkdir "$p/reused-old-build"
 for f in parent-full.tar.gz parent-git-identity.txt parent-git-tree.txt binary-old-go-version-m.txt build-pr5v2.sh build.log build-status.txt standalone-old.log harness-parent-check.log full-old.sha256;do
  cp "artifacts/pr5-r10b-build/$f" "$p/reused-old-build/$f"
 done
fi
sha256sum -c "$p/measurement-input.sha256" > "$p/archive-input-check.log"
sha256sum -c "$p/binaries.sha256" > "$p/archive-binaries-check.log"
sha256sum -c "$p/runtime.sha256" > "$p/archive-runtime-check.log"
mapfile -t workloads < <(python3 -B -c 'import json,sys;print("\n".join(w["name"] for w in json.load(open(sys.argv[1]))))' "$p/workloads.json")
test "${#workloads[@]}" -gt 0
paths=("$p" bin/candidate-pr5-r10b-old "bin/candidate-$bundle-new" bin/candidate-v6-cgo scripts/astra-perf-run.py scripts/astra-perf-analyze.py)
for w in "${workloads[@]}";do paths+=("artifacts/$bundle-$w");done
if [[ "$bundle" == pr5-r10b ]];then
 sha256sum -c "$p/diagnostics-input.sha256" > "$p/archive-diagnostics-input-check.log"
 paths+=(artifacts/pr5-r10b-strace artifacts/pr5-r10b-perf)
fi
ps -eo pid,ppid,comm,args > "$p/archive-processes.txt"
find "${paths[@]}" -type f -print0 | sort -z | xargs -0 sha256sum > "exports/$bundle.files.sha256"
sha256sum -c "exports/$bundle.files.sha256" > "exports/$bundle.prearchive-verification.log"
tar -czf "exports/$bundle.tar.gz" "${paths[@]}" "exports/$bundle.files.sha256" "exports/$bundle.prearchive-verification.log"
sha256sum "exports/$bundle.tar.gz" > "exports/$bundle.tar.gz.sha256"
cat "exports/$bundle.tar.gz.sha256"
wc -l "exports/$bundle.files.sha256"
ls -lh "exports/$bundle.tar.gz"
