#!/usr/bin/env bash
set -Eeuo pipefail
p=/opt/fdbperf/artifacts/fullstack-r16-build
parent=/opt/fdbperf/sources/fullstack-r16-parent
old=/opt/fdbperf/sources/fullstack-r16-old
new=/opt/fdbperf/sources/fullstack-r16-new
v7=/opt/fdbperf/artifacts/lifecycle-v7-build
exec > >(tee "$p/build.log") 2>&1
trap 'rc=$?;printf "exit_code=%s\nfinished_utc=%s\n" "$rc" "$(date -u +%FT%TZ)" > "$p/build-status.txt"' EXIT
export GOTOOLCHAIN=go1.26.9 GOMAXPROCS=8 GOFLAGS=-mod=readonly CGO_ENABLED=0
sha256sum -c "$p/upload.sha256"
for dir in "$parent" "$old" "$new";do test ! -e "$dir";done
for arm in old new;do test ! -e "/opt/fdbperf/bin/clientbench-r16-$arm";done
find /opt/fdbperf/bin -maxdepth 1 -type f -print0 | sort -z | xargs -0 sha256sum > "$p/prior-binaries.sha256"
cp "$v7/runtime.sha256" "$p/runtime.sha256"
sha256sum -c "$p/runtime.sha256"
mkdir "$parent"
tar -xzf "$p/parent-full.tar.gz" -C "$parent"
test ! -e "$parent/tools/clientbench"
tar -xzf "$v7/harness-v7-candidate.tar.gz" -C "$parent"
tar -xzf "$v7/standalone-wrapper-v7.tar.gz" -C "$parent"
(cd "$parent";sha256sum -c "$v7/harness-v7.sha256";find . -type f -print0 | sort -z | xargs -0 sha256sum > "$p/full-parent.sha256") > "$p/harness-parent-check.log"
cp -a "$parent" "$old"
cp -a "$parent" "$new"
(cd "$new";git apply --check "$p/fullstack-on-7922a2e5a.patch";git apply "$p/fullstack-on-7922a2e5a.patch") > "$p/new-apply.log"
for arm in old new;do
 src="/opt/fdbperf/sources/fullstack-r16-$arm"
 (cd "$src";sha256sum -c "$v7/harness-v7.sha256";find . -type f -print0 | sort -z | xargs -0 sha256sum > "$p/full-$arm.sha256") > "$p/harness-$arm-check.log"
 # Frozen V7 benchmark units only; no parent/client/integration tests or Bazel.
 (cd "$src/tools/clientbench";go test -count=1 -v ./internal/bench ./cmd/clientbench;go build -trimpath -o "/opt/fdbperf/bin/clientbench-r16-$arm" ./cmd/clientbench;go list -m all) > "$p/standalone-$arm.log" 2>&1
 (cd "$src";find . -type f -print0 | sort -z | xargs -0 sha256sum) > "$p/full-$arm-after.sha256"
 cmp "$p/full-$arm.sha256" "$p/full-$arm-after.sha256"
 go version -m "/opt/fdbperf/bin/clientbench-r16-$arm" > "$p/$arm-go-version-m.txt"
 readelf -d "/opt/fdbperf/bin/clientbench-r16-$arm" > "$p/$arm-readelf-d.txt"
 grep -c '^--- PASS:' "$p/standalone-$arm.log"
done
cmp "$p/full-parent.sha256" "$p/full-old.sha256"
python3 -B - "$p" <<'PY'
import json,re,sys
from pathlib import Path
p=Path(sys.argv[1])
def load(name):return {line.split('  ',1)[1]:line.split('  ',1)[0] for line in (p/('full-'+name+'.sha256')).read_text().splitlines()}
a,b=load('parent'),load('new')
actual=sorted(k for k in set(a)|set(b) if a.get(k)!=b.get(k))
expected=sorted('./'+x for x in re.findall(r'^diff --git a/\S+ b/(\S+)$',(p/'fullstack-on-7922a2e5a.patch').read_text(),re.M))
assert actual==expected,(actual,expected)
(p/'source-delta.json').write_text(json.dumps(dict(changed=actual),indent=2)+'\n')
PY
(cd "$parent";find . -type f -print0 | sort -z | xargs -0 sha256sum) > "$p/full-parent-after.sha256"
cmp "$p/full-parent.sha256" "$p/full-parent-after.sha256"
sha256sum /opt/fdbperf/bin/clientbench-r16-{old,new} /opt/fdbperf/bin/clientbench-v7-cgo > "$p/binaries.sha256"
sha256sum -c "$p/prior-binaries.sha256" > "$p/prior-binaries-after-check.log"
sha256sum -c "$p/runtime.sha256" > "$p/runtime-after-check.log"
cat "$p/binaries.sha256" "$p/source-delta.json"
