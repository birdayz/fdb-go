#!/usr/bin/env bash
set -Eeuo pipefail
p=/opt/fdbperf/artifacts/pr9-r15-build
base=/opt/fdbperf/sources/lifecycle-v7-baseline
old=/opt/fdbperf/sources/pr9-r15-old
new=/opt/fdbperf/sources/pr9-r15-new
v7=/opt/fdbperf/artifacts/lifecycle-v7-build
exec > >(tee "$p/build.log") 2>&1
trap 'rc=$?;printf "exit_code=%s\nfinished_utc=%s\n" "$rc" "$(date -u +%FT%TZ)" > "$p/build-status.txt"' EXIT
export GOTOOLCHAIN=go1.26.9 GOMAXPROCS=8 GOFLAGS=-mod=readonly CGO_ENABLED=0
sha256sum -c "$p/upload.sha256"
test ! -e "$old";test ! -e "$new"
for arm in old new;do test ! -e "/opt/fdbperf/bin/clientbench-r15-$arm";done
find /opt/fdbperf/bin -maxdepth 1 -type f -print0 | sort -z | xargs -0 sha256sum > "$p/prior-binaries.sha256"
cp "$v7/runtime.sha256" "$p/runtime.sha256"
sha256sum -c "$p/runtime.sha256"
(cd "$base";find . -type f -print0 | sort -z | xargs -0 sha256sum) > "$p/full-parent.sha256"
cmp "$v7/full-baseline.sha256" "$p/full-parent.sha256"
cp -a "$base" "$old"
(cd "$old";git apply --check "$p/pr5v3-direct-write-275875031.patch";git apply "$p/pr5v3-direct-write-275875031.patch") > "$p/old-apply.log"
cp -a "$old" "$new"
(cd "$new";git apply --check "$p/pr9-eager-lone-get.patch";git apply "$p/pr9-eager-lone-get.patch") > "$p/new-apply.log"
for arm in old new;do
 src="/opt/fdbperf/sources/pr9-r15-$arm"
 (cd "$src";sha256sum -c "$v7/harness-v7.sha256";find . -type f -print0 | sort -z | xargs -0 sha256sum > "$p/full-$arm.sha256") > "$p/harness-$arm-check.log"
 # Frozen V7 benchmark units only; no parent/client/integration tests or Bazel.
 (cd "$src/tools/clientbench";go test -count=1 -v ./internal/bench ./cmd/clientbench;go build -trimpath -o "/opt/fdbperf/bin/clientbench-r15-$arm" ./cmd/clientbench;go list -m all) > "$p/standalone-$arm.log" 2>&1
 (cd "$src";find . -type f -print0 | sort -z | xargs -0 sha256sum) > "$p/full-$arm-after.sha256"
 cmp "$p/full-$arm.sha256" "$p/full-$arm-after.sha256"
 go version -m "/opt/fdbperf/bin/clientbench-r15-$arm" > "$p/$arm-go-version-m.txt"
 readelf -d "/opt/fdbperf/bin/clientbench-r15-$arm" > "$p/$arm-readelf-d.txt"
 grep -c '^--- PASS:' "$p/standalone-$arm.log"
done
python3 -B - "$p" <<'PY'
import json,re,sys
from pathlib import Path
p=Path(sys.argv[1])
def load(name):return {line.split('  ',1)[1]:line.split('  ',1)[0] for line in (p/('full-'+name+'.sha256')).read_text().splitlines()}
result=[]
for source,target,patch in [('parent','old','pr5v3-direct-write-275875031.patch'),('old','new','pr9-eager-lone-get.patch')]:
 a,b=load(source),load(target)
 actual=sorted(k for k in set(a)|set(b) if a.get(k)!=b.get(k))
 expected=sorted('./'+x for x in re.findall(r'^diff --git a/\S+ b/(\S+)$',(p/patch).read_text(),re.M))
 assert actual==expected,(actual,expected)
 result.append(dict(source=source,target=target,patch=patch,changed=actual))
(p/'source-delta.json').write_text(json.dumps(result,indent=2)+'\n')
PY
(cd "$base";find . -type f -print0 | sort -z | xargs -0 sha256sum) > "$p/full-parent-after.sha256"
cmp "$p/full-parent.sha256" "$p/full-parent-after.sha256"
sha256sum /opt/fdbperf/bin/clientbench-r15-{old,new} /opt/fdbperf/bin/clientbench-v7-cgo > "$p/binaries.sha256"
sha256sum -c "$p/prior-binaries.sha256" > "$p/prior-binaries-after-check.log"
sha256sum -c "$p/runtime.sha256" > "$p/runtime-after-check.log"
cat "$p/binaries.sha256" "$p/source-delta.json"
