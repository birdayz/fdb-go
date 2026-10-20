#!/usr/bin/env bash
set -Eeuo pipefail
p=/opt/fdbperf/artifacts/pr2-r6-build
base=/opt/fdbperf/artifacts/pr1-r5-build
old=/opt/fdbperf/sources/pr1-r5-old
new=/opt/fdbperf/sources/pr2-r6-new
binary=/opt/fdbperf/bin/candidate-pr2-r6-new
exec > >(tee "$p/build.log") 2>&1
trap 'rc=$?;printf "exit_code=%s\nfinished_utc=%s\n" "$rc" "$(date -u +%FT%TZ)" > "$p/build-status.txt"' EXIT
export GOTOOLCHAIN=go1.26.9 GOMAXPROCS=8 GOFLAGS=-mod=readonly CGO_ENABLED=0
test ! -e "$new";test ! -e "$binary"
sha256sum -c "$p/upload.sha256"
printf '4d236cbfc53cea32211b5042a86e0e14dc006f8896a1204b27aee3921fb17e71  /opt/fdbperf/bin/candidate-pr1-r5-old\ncd762ea429647784378270613f404573ba598a5049f422e055a887f7746ecbdb  /opt/fdbperf/bin/candidate-v6-cgo\n' | sha256sum -c -
find /opt/fdbperf/bin -maxdepth 1 -type f -print0 | sort -z | xargs -0 sha256sum > "$p/prior-binaries.sha256"
cp "$base/full-old.sha256" "$p/full-old.sha256"
cp "$base/runtime.sha256" "$p/runtime.sha256"
sha256sum -c "$p/runtime.sha256"
(cd "$old";find . -type f -print0 | sort -z | xargs -0 sha256sum) > "$p/full-old-before.sha256"
cmp "$p/full-old.sha256" "$p/full-old-before.sha256"
cp -a "$old" "$new"
(cd "$new";git apply --check "$p/pr2-commit-capture-borrow.patch";git apply --stat "$p/pr2-commit-capture-borrow.patch";git apply "$p/pr2-commit-capture-borrow.patch";sha256sum -c /opt/fdbperf/artifacts/ownership-001-build/harness-v6.sha256) > "$p/patch-and-harness-new-check.log"
(cd "$new";find . -type f -print0 | sort -z | xargs -0 sha256sum) > "$p/full-new.sha256"
# Frozen standalone module only; no parent/client tests are executed here.
(cd "$new/tools/clientbench";go test -count=1 -v ./...;go build -trimpath -o "$binary" ./cmd/clientbench;go list -m all) > "$p/standalone-new.log" 2>&1
go version -m "$binary" > "$p/binary-new-go-version-m.txt"
for arm in old new;do
 src="$new";if [[ "$arm" == old ]];then src="$old";fi
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
assert changed==['./pkg/fdbgo/client/BUILD.bazel','./pkg/fdbgo/client/commit_capture_borrow_unit_test.go','./pkg/fdbgo/client/commit_input.go','./pkg/fdbgo/client/ryw.go','./pkg/fdbgo/client/transaction.go'],changed
(p/'source-delta.json').write_text(json.dumps({'old_files':len(a),'new_files':len(b),'changed':changed},indent=2)+'\n')
PY
sha256sum /opt/fdbperf/bin/candidate-pr1-r5-old "$binary" /opt/fdbperf/bin/candidate-v6-cgo > "$p/binaries.sha256"
sha256sum -c "$p/prior-binaries.sha256" > "$p/prior-binaries-after-check.log"
sha256sum -c "$p/runtime.sha256" > "$p/runtime-after-check.log"
cat "$p/binaries.sha256" "$p/source-delta.json"
grep -c '^--- PASS:' "$p/standalone-new.log"
