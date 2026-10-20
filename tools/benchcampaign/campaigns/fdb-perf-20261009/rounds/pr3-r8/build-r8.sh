#!/usr/bin/env bash
set -Eeuo pipefail
p=/opt/fdbperf/artifacts/pr3-r8-build
old=/opt/fdbperf/sources/pr1-r5-old
parent=/opt/fdbperf/sources/pr3-r8-parent
new=/opt/fdbperf/sources/pr3-r8-new
binary=/opt/fdbperf/bin/candidate-pr3-r8-new
exec > >(tee "$p/build.log") 2>&1
trap 'rc=$?;printf "exit_code=%s\nfinished_utc=%s\n" "$rc" "$(date -u +%FT%TZ)" > "$p/build-status.txt"' EXIT
export GOTOOLCHAIN=go1.26.9 GOMAXPROCS=8 GOFLAGS=-mod=readonly CGO_ENABLED=0
test ! -e "$parent";test ! -e "$new";test ! -e "$binary"
sha256sum -c "$p/upload.sha256"
printf '4d236cbfc53cea32211b5042a86e0e14dc006f8896a1204b27aee3921fb17e71  /opt/fdbperf/bin/candidate-pr1-r5-old\ncd762ea429647784378270613f404573ba598a5049f422e055a887f7746ecbdb  /opt/fdbperf/bin/candidate-v6-cgo\n' | sha256sum -c -
find /opt/fdbperf/bin -maxdepth 1 -type f -print0 | sort -z | xargs -0 sha256sum > "$p/prior-binaries.sha256"
cp /opt/fdbperf/artifacts/pr1-r5-build/full-old.sha256 "$p/full-old.sha256"
cp /opt/fdbperf/artifacts/pr1-r5-build/runtime.sha256 "$p/runtime.sha256"
sha256sum -c "$p/runtime.sha256"
(cd "$old";find . -type f -print0 | sort -z | xargs -0 sha256sum) > "$p/full-old-before.sha256"
cmp "$p/full-old.sha256" "$p/full-old-before.sha256"
mkdir "$parent"
tar -xzf "$p/parent-full.tar.gz" -C "$parent"
tar -xzf /opt/fdbperf/artifacts/ownership-001-build/harness-v6.tar.gz -C "$parent"
(cd "$parent";sha256sum -c /opt/fdbperf/artifacts/ownership-001-build/harness-v6.sha256;diff -r "$old/pkg/fdbgo" pkg/fdbgo;diff -r "$old/tools" tools;cmp "$old/go.mod" go.mod;cmp "$old/go.sum" go.sum) > "$p/parent-reuse-verification.log"
(cd "$parent";find . -type f -print0 | sort -z | xargs -0 sha256sum) > "$p/full-parent.sha256"
cp -a "$parent" "$new"
(cd "$new";git apply --check "$p/pr3-readloop-buffered.patch";git apply --stat "$p/pr3-readloop-buffered.patch";git apply "$p/pr3-readloop-buffered.patch";sha256sum -c /opt/fdbperf/artifacts/ownership-001-build/harness-v6.sha256) > "$p/patch-and-harness-new-check.log"
(cd "$new";find . -type f -print0 | sort -z | xargs -0 sha256sum) > "$p/full-new.sha256"
# Only the unchanged frozen nested harness; no parent/client tests here.
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
assert changed==['./pkg/fdbgo/transport/BUILD.bazel','./pkg/fdbgo/transport/conn.go','./pkg/fdbgo/transport/conn_read_buffer_test.go'],changed
(p/'source-delta.json').write_text(json.dumps({'parent_files':len(a),'new_files':len(b),'changed':changed},indent=2)+'\n')
PY
sha256sum /opt/fdbperf/bin/candidate-pr1-r5-old "$binary" /opt/fdbperf/bin/candidate-v6-cgo > "$p/binaries.sha256"
sha256sum -c "$p/prior-binaries.sha256" > "$p/prior-binaries-after-check.log"
sha256sum -c "$p/runtime.sha256" > "$p/runtime-after-check.log"
cat "$p/binaries.sha256" "$p/source-delta.json"
grep -c '^--- PASS:' "$p/standalone-new.log"
