#!/usr/bin/env bash
set -Eeuo pipefail
ident=${1:?campaign id}
patch=${2:?patch filename}
p=/opt/fdbperf/artifacts/$ident-build
old=/opt/fdbperf/sources/pr5-r10b-parent
parent=/opt/fdbperf/sources/$ident-parent
new=/opt/fdbperf/sources/$ident-new
oldbinary=/opt/fdbperf/bin/candidate-pr5-r10b-old
binary=/opt/fdbperf/bin/candidate-$ident-new
exec > >(tee "$p/build.log") 2>&1
trap 'rc=$?;printf "exit_code=%s\nfinished_utc=%s\n" "$rc" "$(date -u +%FT%TZ)" > "$p/build-status.txt"' EXIT
export GOTOOLCHAIN=go1.26.9 GOMAXPROCS=8 GOFLAGS=-mod=readonly CGO_ENABLED=0
test ! -e "$parent";test ! -e "$new";test ! -e "$binary"
sha256sum -c "$p/upload.sha256"
printf 'cd762ea429647784378270613f404573ba598a5049f422e055a887f7746ecbdb  /opt/fdbperf/bin/candidate-v6-cgo\n3d2504ae0db14fad14f0384287377998231cf049310c8289d6e7245d3428e0d0  /opt/fdbperf/artifacts/ownership-001-build/harness-v6.tar.gz\n' | sha256sum -c -
find /opt/fdbperf/bin -maxdepth 1 -type f -print0 | sort -z | xargs -0 sha256sum > "$p/prior-binaries.sha256"
cp /opt/fdbperf/artifacts/pr1-r5-build/runtime.sha256 "$p/runtime.sha256"
sha256sum -c "$p/runtime.sha256"
mkdir "$parent"
tar -xzf "$p/parent-full.tar.gz" -C "$parent"
tar -xzf /opt/fdbperf/artifacts/ownership-001-build/harness-v6.tar.gz -C "$parent"
(cd "$parent";sha256sum -c /opt/fdbperf/artifacts/ownership-001-build/harness-v6.sha256) > "$p/harness-parent-check.log"
(cd "$parent";find . -type f -print0 | sort -z | xargs -0 sha256sum) > "$p/full-parent.sha256"
if [[ "$ident" == pr5-r10b ]];then
 test ! -e "$oldbinary"
 cp "$p/full-parent.sha256" "$p/full-old.sha256"
 # Authorized exception: unchanged frozen standalone nested harness only.
 (cd "$old/tools/clientbench";go test -count=1 -v ./...;go build -trimpath -o "$oldbinary" ./cmd/clientbench;go list -m all) > "$p/standalone-old.log" 2>&1
 go version -m "$oldbinary" > "$p/binary-old-go-version-m.txt"
else
 cp /opt/fdbperf/artifacts/pr5-r10b-build/full-old.sha256 "$p/full-old.sha256"
 cmp "$p/full-old.sha256" "$p/full-parent.sha256"
 sha256sum -c /opt/fdbperf/artifacts/pr5-r10b-build/binaries.sha256 > "$p/reused-old-binary-check.log"
fi
cp -a "$parent" "$new"
(cd "$new";git apply --check "$p/$patch";git apply --stat "$p/$patch";git apply "$p/$patch";sha256sum -c /opt/fdbperf/artifacts/ownership-001-build/harness-v6.sha256) > "$p/patch-and-harness-new-check.log"
(cd "$new";find . -type f -print0 | sort -z | xargs -0 sha256sum) > "$p/full-new.sha256"
(cd "$new/tools/clientbench";go test -count=1 -v ./...;go build -trimpath -o "$binary" ./cmd/clientbench;go list -m all) > "$p/standalone-new.log" 2>&1
go version -m "$binary" > "$p/binary-new-go-version-m.txt"
for arm in old parent new;do
 src="$new";if [[ "$arm" == old ]];then src="$old";elif [[ "$arm" == parent ]];then src="$parent";fi
 (cd "$src";find . -type f -print0 | sort -z | xargs -0 sha256sum) > "$p/full-$arm-after.sha256"
 cmp "$p/full-$arm.sha256" "$p/full-$arm-after.sha256"
done
python3 - "$p" <<'PY'
import json,sys
from pathlib import Path
p=Path(sys.argv[1])
def load(name):return {line.split('  ',1)[1]:line.split('  ',1)[0] for line in (p/name).read_text().splitlines()}
a,b=load('full-parent.sha256'),load('full-new.sha256')
changed=sorted(k for k in set(a)|set(b) if a.get(k)!=b.get(k))
assert changed==json.loads((p/'expected-delta.json').read_text()),changed
(p/'source-delta.json').write_text(json.dumps({'parent_files':len(a),'new_files':len(b),'changed':changed},indent=2)+'\n')
PY
sha256sum "$oldbinary" "$binary" /opt/fdbperf/bin/candidate-v6-cgo > "$p/binaries.sha256"
sha256sum -c "$p/prior-binaries.sha256" > "$p/prior-binaries-after-check.log"
sha256sum -c "$p/runtime.sha256" > "$p/runtime-after-check.log"
cat "$p/binaries.sha256" "$p/source-delta.json"
grep -c '^--- PASS:' "$p/standalone-new.log"
