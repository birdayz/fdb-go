#!/usr/bin/env bash
set -Eeuo pipefail
p=/opt/fdbperf/artifacts/lifecycle-v7-build
base=/opt/fdbperf/sources/lifecycle-v7-baseline
stack=/opt/fdbperf/sources/lifecycle-v7-stack
exec > >(tee "$p/build.log") 2>&1
trap 'rc=$?;printf "exit_code=%s\nfinished_utc=%s\n" "$rc" "$(date -u +%FT%TZ)" > "$p/build-status.txt"' EXIT
python3 - "$p" <<'PY'
import json,sys
from pathlib import Path
p=Path(sys.argv[1]);sha=(p/'review-candidate-sha.txt').read_text().strip();acks=json.loads((p/'review-acks.json').read_text())
assert set(acks)=={'code','spec'}
assert all(a['sha']==sha and a['verdict']=='ACK' for a in acks.values())
PY
export GOTOOLCHAIN=go1.26.9 GOMAXPROCS=8 GOFLAGS=-mod=readonly
sha256sum -c "$p/upload.sha256"
test ! -e "$base";test ! -e "$stack"
for b in baseline-go stack-go cgo;do test ! -e "/opt/fdbperf/bin/clientbench-v7-$b";done
find /opt/fdbperf/bin -maxdepth 1 -type f -print0 | sort -z | xargs -0 sha256sum > "$p/prior-binaries.sha256"
sha256sum /usr/lib/libfdb_c.so /usr/sbin/fdbserver /etc/foundationdb/fdb.cluster /etc/foundationdb/foundationdb.conf > "$p/runtime.sha256"
mkdir "$base"
tar -xzf "$p/parent-full.tar.gz" -C "$base"
test ! -e "$base/tools/clientbench"
tar -xzf "$p/harness-v7-candidate.tar.gz" -C "$base"
(cd "$base";find tools/clientbench -type f -print0 | sort -z | xargs -0 sha256sum) > "$p/harness-v7.sha256"
cp "$p/frozen-v6.go.mod" "$base/tools/clientbench/go.mod"
cp "$p/frozen-v6.go.sum" "$base/tools/clientbench/go.sum"
printf '\nreplace github.com/apple/foundationdb/bindings/go => ../../native-binding\n' >> "$base/tools/clientbench/go.mod"
module="$(go env GOMODCACHE)/github.com/apple/foundationdb/bindings/go@v0.0.0-20250702211439-37fcf1c8ce08"
test -f "$module/src/fdb/range.go"
(cd "$module";find . -type f -print0 | sort -z | xargs -0 sha256sum) > "$p/native-module-before.sha256"
cp -a "$module" "$base/native-binding"
chmod -R u+w "$base/native-binding"
(cd "$base/native-binding";git apply --check "$p/fdb-go-range-iterator-done.patch";git apply "$p/fdb-go-range-iterator-done.patch")
# Match Bazel's native binding override without mutating the shared Go module cache.
(cd "$base";tar --sort=name --mtime=@0 --owner=0 --group=0 --numeric-owner -czf "$p/standalone-wrapper-v7.tar.gz" tools/clientbench/go.mod tools/clientbench/go.sum native-binding)
sha256sum "$p/harness-v7-candidate.tar.gz" "$p/standalone-wrapper-v7.tar.gz" > "$p/harness-and-wrapper.sha256"
cp -a "$base" "$stack"
(cd "$stack";git apply --check "$p/stack-pr3-pr4-pr5v2-on-8ab5c5a4d.patch";git apply "$p/stack-pr3-pr4-pr5v2-on-8ab5c5a4d.patch") > "$p/stack-apply.log"
for arm in baseline stack;do
 src="$base";if [[ "$arm" == stack ]];then src="$stack";fi
 (cd "$src";sha256sum -c "$p/harness-v7.sha256";find . -type f -print0 | sort -z | xargs -0 sha256sum > "$p/full-$arm.sha256") > "$p/harness-$arm-check.log"
 # Owner-authorized nested benchmark units only; no parent/client tests or Bazel.
 (cd "$src/tools/clientbench";CGO_ENABLED=0 go test -count=1 -v ./internal/bench ./cmd/clientbench;CGO_ENABLED=0 go build -trimpath -o "/opt/fdbperf/bin/clientbench-v7-$arm-go" ./cmd/clientbench;go list -m all) > "$p/standalone-$arm.log" 2>&1
done
(cd "$base/tools/clientbench";CGO_ENABLED=1 go test -count=1 -v ./internal/bench ./cmd/clientbench;CGO_ENABLED=1 go build -trimpath -o /opt/fdbperf/bin/clientbench-v7-cgo ./cmd/clientbench) > "$p/standalone-cgo.log" 2>&1
for arm in baseline stack;do
 src="$base";if [[ "$arm" == stack ]];then src="$stack";fi
 (cd "$src";find . -type f -print0 | sort -z | xargs -0 sha256sum) > "$p/full-$arm-after.sha256"
 cmp "$p/full-$arm.sha256" "$p/full-$arm-after.sha256"
done
(cd "$module";find . -type f -print0 | sort -z | xargs -0 sha256sum) > "$p/native-module-after.sha256"
cmp "$p/native-module-before.sha256" "$p/native-module-after.sha256"
for b in baseline-go stack-go cgo;do
 go version -m "/opt/fdbperf/bin/clientbench-v7-$b" > "$p/$b.go-version-m.txt"
 readelf -d "/opt/fdbperf/bin/clientbench-v7-$b" > "$p/$b.readelf-d.txt"
done
ldd /opt/fdbperf/bin/clientbench-v7-cgo > "$p/native-ldd.txt"
go version > "$p/go-version.txt";go env > "$p/go-env.txt"
sha256sum /opt/fdbperf/bin/clientbench-v7-{baseline-go,stack-go,cgo} > "$p/binaries.sha256"
sha256sum -c "$p/prior-binaries.sha256" > "$p/prior-binaries-after-check.log"
sha256sum -c "$p/runtime.sha256" > "$p/runtime-after-check.log"
cat "$p/binaries.sha256" "$p/harness-and-wrapper.sha256"
