#!/usr/bin/env bash
set -Eeuo pipefail
p=/opt/fdbperf/artifacts/pr1-r5-build
old=/opt/fdbperf/sources/pr1-r5-old
new=/opt/fdbperf/sources/pr1-r5-new
exec > >(tee "$p/build.log") 2>&1
trap 'rc=$?;printf "exit_code=%s\nfinished_utc=%s\n" "$rc" "$(date -u +%FT%TZ)" > "$p/build-status.txt"' EXIT
export GOTOOLCHAIN=go1.26.9 GOMAXPROCS=8 GOFLAGS=-mod=readonly CGO_ENABLED=0
for path in "$old" "$new" /opt/fdbperf/bin/candidate-pr1-r5-old /opt/fdbperf/bin/candidate-pr1-r5-new;do test ! -e "$path";done
sha256sum -c "$p/upload.sha256"
printf '3d2504ae0db14fad14f0384287377998231cf049310c8289d6e7245d3428e0d0  /opt/fdbperf/artifacts/ownership-001-build/harness-v6.tar.gz\n' | sha256sum -c -
ps -eo pid,comm,args > "$p/prebuild-processes.txt"
find /opt/fdbperf/bin -maxdepth 1 -type f -print0 | sort -z | xargs -0 sha256sum > "$p/prior-binaries.sha256"
sha256sum /usr/lib/libfdb_c.so /usr/sbin/fdbserver /etc/foundationdb/fdb.cluster /etc/foundationdb/foundationdb.conf > "$p/runtime.sha256"
mkdir "$old"
tar -xzf "$p/old-full.tar.gz" -C "$old"
# Exclusive newly extracted scratch export: substitute only the immutable harness.
rm -rf "$old/tools/clientbench"
tar -xzf /opt/fdbperf/artifacts/ownership-001-build/harness-v6.tar.gz -C "$old"
(cd "$old";sha256sum -c /opt/fdbperf/artifacts/ownership-001-build/harness-v6.sha256) > "$p/harness-old-check.log"
cp -a "$old" "$new"
(cd "$new";git apply --check "$p/pr1-keyrange-memeq.patch";git apply --stat "$p/pr1-keyrange-memeq.patch";git apply "$p/pr1-keyrange-memeq.patch";sha256sum -c /opt/fdbperf/artifacts/ownership-001-build/harness-v6.sha256) > "$p/patch-and-harness-new-check.log"
for arm in old new;do
 src=/opt/fdbperf/sources/pr1-r5-$arm
 binary=/opt/fdbperf/bin/candidate-pr1-r5-$arm
 (cd "$src";find . -type f -print0 | sort -z | xargs -0 sha256sum) > "$p/full-$arm.sha256"
 # Explicit frozen standalone-module exception only; not parent-module tests.
 (cd "$src/tools/clientbench";go test -count=1 -v ./...;go build -trimpath -o "$binary" ./cmd/clientbench;go list -m all) > "$p/standalone-$arm.log" 2>&1
 go version -m "$binary" > "$p/binary-$arm-go-version-m.txt"
 (cd "$src";find . -type f -print0 | sort -z | xargs -0 sha256sum) > "$p/full-$arm-after.sha256"
 cmp "$p/full-$arm.sha256" "$p/full-$arm-after.sha256"
done
python3 - "$p" <<'PY'
import json,sys
from pathlib import Path
p=Path(sys.argv[1])
def load(name):return {line.split('  ',1)[1]:line.split('  ',1)[0] for line in (p/name).read_text().splitlines()}
a,b=load('full-old.sha256'),load('full-new.sha256')
changed=sorted(k for k in set(a)|set(b) if a.get(k)!=b.get(k))
assert changed==['./pkg/fdbgo/wire/types/BUILD.bazel','./pkg/fdbgo/wire/types/keyrange_equals_key_after_test.go','./pkg/fdbgo/wire/types/keyrangeref_custom.go'],changed
(p/'source-delta.json').write_text(json.dumps({'old_files':len(a),'new_files':len(b),'changed':changed},indent=2)+'\n')
PY
sha256sum /opt/fdbperf/bin/candidate-pr1-r5-old /opt/fdbperf/bin/candidate-pr1-r5-new /opt/fdbperf/bin/candidate-v6-cgo > "$p/binaries.sha256"
sha256sum -c "$p/prior-binaries.sha256" > "$p/prior-binaries-after-check.log"
sha256sum -c "$p/runtime.sha256" > "$p/runtime-after-check.log"
cat "$p/binaries.sha256" "$p/source-delta.json"
grep -c '^--- PASS:' "$p"/standalone-{old,new}.log
