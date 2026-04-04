BUF_VERSION := "1.67.0"

default:
    @just --list

# Ensure buf is installed at pinned version
[private]
ensure-buf:
    #!/usr/bin/env bash
    set -euo pipefail
    BUF=.tools/buf
    if [ -x "$BUF" ] && "$BUF" --version 2>/dev/null | grep -q "{{BUF_VERSION}}"; then
        exit 0
    fi
    mkdir -p .tools
    curl -fsSL -o "$BUF" "https://github.com/bufbuild/buf/releases/download/v{{BUF_VERSION}}/buf-$(uname -s)-$(uname -m)"
    chmod +x "$BUF"

# Generate protobuf code
generate: ensure-buf
    .tools/buf generate

# Build all targets (includes nogo lint)
build:
    bazelisk build //...

# Test all targets
test:
    bazelisk test //...

# Run conformance server
run-conformance-server:
    bazelisk run //conformance/java:conformance_server

# Regenerate BUILD files after adding/removing Go files or deps
gazelle:
    bazelisk run //:gazelle

# Clean bazel outputs
clean:
    bazelisk clean

# Go mod tidy
tidy:
    go mod tidy

# Format Go source files
fmt:
    gofmt -w $(find . -name '*.go' -not -path './fdb-record-layer/*' -not -path './bazel-*' -not -path './gen/*' -not -path './.claude/*')

# Check Go formatting (fails if any file needs formatting)
# Note: interface{} → any is enforced by nogo (noemptyiface analyzer)
lint:
    #!/usr/bin/env bash
    set -euo pipefail
    unformatted=$(gofmt -l $(find . -name '*.go' -not -path './fdb-record-layer/*' -not -path './bazel-*' -not -path './gen/*' -not -path './.claude/*'))
    if [ -n "$unformatted" ]; then
        echo "Unformatted files:"
        echo "$unformatted"
        echo "Run 'just fmt' to fix."
        exit 1
    fi

# Run all benchmarks (skips Ginkgo specs, runs only Go benchmarks)
bench:
    bazelisk test //pkg/recordlayer:recordlayer_test --test_arg="-test.bench=." --test_arg="-test.benchtime=3s" --test_arg="--ginkgo.skip=.*" --test_output=all --nocache_test_results --test_timeout=300

# Run a specific benchmark by name regex
bench-one NAME:
    bazelisk test //pkg/recordlayer:recordlayer_test --test_arg="-test.bench={{NAME}}" --test_arg="-test.benchtime=3s" --test_arg="--ginkgo.skip=.*" --test_output=all --nocache_test_results --test_timeout=300

# Regenerate Go wire types from FDB C++ headers (v5 composable-primitives generator).
# Two Bazel genrules: fdb_cmake_build (cached on FDB version) → generate_wire_types (cached on generator code).
generate-wire-types:
    #!/usr/bin/env bash
    set -euo pipefail
    bazelisk build //cmd/fdb-schema-extract:generate_wire_types
    BAZEL_BIN=$(bazelisk info bazel-bin)
    rm -f pkg/fdbgo/wire/types/*_generated.go
    tar xf "$BAZEL_BIN/cmd/fdb-schema-extract/wire_types.tar" -C pkg/fdbgo/wire/types/ --strip-components=1
    just gazelle
    echo "Generated: $(ls pkg/fdbgo/wire/types/*_generated.go | wc -l) files"

# Regenerate FDB wire schema JSON files (Bazel, sandboxed)
wire-schema:
    bazelisk build //cmd/fdb-wire-schema-generator:gen_schema
    rm -rf pkg/fdbgo/wire/schema
    cp -r bazel-bin/cmd/fdb-wire-schema-generator/schema pkg/fdbgo/wire/schema
    @echo "Updated: $$(ls pkg/fdbgo/wire/schema/*.json | wc -l) schemas"

# Regenerate FDB test vectors (Docker, real FDB ObjectWriter)
wire-testvecs:
    bazelisk build //cmd/fdb-wire-schema-generator:gen_schema
    bash cmd/fdb-wire-schema-generator/docker_build.sh \
        $$(bazelisk info output_base)/external/foundationdb+ \
        bazel-bin/cmd/fdb-wire-schema-generator/generated_messages.cpp \
        /tmp/testvecs_docker
    rm -f pkg/fdbgo/wire/testdata/[A-Z]*.json
    cp /tmp/testvecs_docker/*.json pkg/fdbgo/wire/testdata/
    @echo "Updated: $$(ls pkg/fdbgo/wire/testdata/[A-Z]*.json | wc -l) test vectors"

# Run FDB binding tester (stack machine conformance via Bazel + testcontainers).
binding-test:
    bazelisk test //cmd/fdb-stacktester/bindingtester:bindingtester_test --test_output=streamed

# Binding tester stress: N seeds × M ops (report → binding-stress-report.txt)
binding-stress runs="100" ops="1000":
    #!/usr/bin/env bash
    set -euo pipefail
    REPORT="binding-stress-report.txt"
    bazelisk build //cmd/fdb-stacktester:fdb-stacktester 2>&1 | tail -1
    STACKTESTER=$(realpath bazel-bin/cmd/fdb-stacktester/fdb-stacktester_/fdb-stacktester)
    BTDIR=$(echo $HOME/.cache/bazel/_bazel_*/*/external/foundationdb+/bindings/bindingtester)
    BTRUN=/tmp/bt-run; mkdir -p "$BTRUN/bindingtester"
    cp -r $BTDIR/* "$BTRUN/bindingtester/"
    sed -i "s|sys.path\[:0\].*||" "$BTRUN/bindingtester/__init__.py"
    sed -i "s|import util|from bindingtester import util|" "$BTRUN/bindingtester/__init__.py"
    sed -i "s|from fdb import LATEST_API_VERSION|LATEST_API_VERSION = 730|" "$BTRUN/bindingtester/__init__.py"
    set +e
    {
    echo "binding-stress: {{runs}} seeds × {{ops}} ops"
    echo "Started: $(date -Iseconds)"
    echo ""
    PASS=0; FAIL=0; DEAD=0; FAILURES=""
    for seed in $(seq 1 {{runs}}); do
        docker rm -f fdb-stress 2>/dev/null >/dev/null
        docker run -d --name fdb-stress -p 4500:4500 foundationdb/foundationdb:7.3.75 2>/dev/null >/dev/null
        sleep 5
        docker exec fdb-stress fdbcli --exec "configure new single memory tenant_mode=optional_experimental" 2>/dev/null >/dev/null
        echo "docker:docker@127.0.0.1:4500" > /tmp/fdb-stress.cluster
        result=$(cd "$BTRUN" && timeout 300 bash -c "PYTHONPATH=$BTRUN python3 bindingtester/bindingtester.py \
            --cluster-file /tmp/fdb-stress.cluster --test-name api --api-version 730 \
            --num-ops {{ops}} --seed $seed --timeout 300 --no-threads --no-tenants \
            $STACKTESTER" 2>&1) || true
        alive="ALIVE"
        docker exec fdb-stress fdbcli --exec "status" >/dev/null 2>&1 || alive="DEAD"
        line=$(echo "$result" | grep "incorrect result" | tail -1)
        if [ -z "$line" ]; then
            FAIL=$((FAIL+1)); FAILURES="$FAILURES seed=$seed:TIMEOUT(FDB=$alive)"
            echo "FAIL seed $seed: TIMEOUT/HANG (FDB=$alive)"
        elif echo "$line" | grep -q "had 0 incorrect"; then
            PASS=$((PASS+1))
            if [ "$alive" = "DEAD" ]; then DEAD=$((DEAD+1)); echo "WARN seed $seed: PASS but FDB=$alive"; fi
        else
            FAIL=$((FAIL+1)); FAILURES="$FAILURES seed=$seed"
            echo "FAIL seed $seed: $line (FDB=$alive)"
        fi
        [ $((seed % 10)) -eq 0 ] && echo "--- $seed/{{runs}} (pass=$PASS fail=$FAIL dead=$DEAD) ---"
    done
    docker rm -f fdb-stress 2>/dev/null >/dev/null
    echo ""
    echo "Finished: $(date -Iseconds)"
    echo "========================================="
    echo "binding-stress: $PASS/{{runs}} pass, $FAIL fail, $DEAD FDB deaths ({{ops}} ops/seed)"
    [ -n "$FAILURES" ] && echo "Failures:$FAILURES"
    echo "========================================="
    } 2>&1 | tee "$REPORT"
    echo "Report: $REPORT"
    tail -5 "$REPORT" | grep -q "0 fail"

# Binding tester stress for a duration (e.g. 2h, 30m). Seeds until time runs out.
binding-stress-duration duration ops="1000":
    #!/usr/bin/env bash
    set -euo pipefail
    REPORT="binding-stress-report.txt"
    SECS=$(python3 -c "
    import re
    s=0
    for m in re.finditer(r'(\d+)(h|m|s)', '{{duration}}'):
        v,u = int(m.group(1)), m.group(2)
        s += v * {'h':3600,'m':60,'s':1}[u]
    if s==0: s=int('{{duration}}')
    print(s)
    ")
    bazelisk build //cmd/fdb-stacktester:fdb-stacktester 2>&1 | tail -1
    STACKTESTER=$(realpath bazel-bin/cmd/fdb-stacktester/fdb-stacktester_/fdb-stacktester)
    BTDIR=$(echo $HOME/.cache/bazel/_bazel_*/*/external/foundationdb+/bindings/bindingtester)
    BTRUN=/tmp/bt-run; mkdir -p "$BTRUN/bindingtester"
    cp -r $BTDIR/* "$BTRUN/bindingtester/"
    sed -i "s|sys.path\[:0\].*||" "$BTRUN/bindingtester/__init__.py"
    sed -i "s|import util|from bindingtester import util|" "$BTRUN/bindingtester/__init__.py"
    sed -i "s|from fdb import LATEST_API_VERSION|LATEST_API_VERSION = 730|" "$BTRUN/bindingtester/__init__.py"
    set +e
    DEADLINE=$(($(date +%s) + SECS))
    {
    echo "binding-stress: timed run, {{ops}} ops/seed, duration=${SECS}s"
    echo "Started: $(date -Iseconds)"
    echo ""
    PASS=0; FAIL=0; DEAD=0; FAILURES=""; seed=0
    while [ $(date +%s) -lt $DEADLINE ]; do
        seed=$((seed+1))
        docker rm -f fdb-stress 2>/dev/null >/dev/null
        docker run -d --name fdb-stress -p 4500:4500 foundationdb/foundationdb:7.3.75 2>/dev/null >/dev/null
        sleep 5
        docker exec fdb-stress fdbcli --exec "configure new single memory tenant_mode=optional_experimental" 2>/dev/null >/dev/null
        echo "docker:docker@127.0.0.1:4500" > /tmp/fdb-stress.cluster
        result=$(cd "$BTRUN" && timeout 300 bash -c "PYTHONPATH=$BTRUN python3 bindingtester/bindingtester.py \
            --cluster-file /tmp/fdb-stress.cluster --test-name api --api-version 730 \
            --num-ops {{ops}} --seed $seed --timeout 300 --no-threads --no-tenants \
            $STACKTESTER" 2>&1) || true
        alive="ALIVE"
        docker exec fdb-stress fdbcli --exec "status" >/dev/null 2>&1 || alive="DEAD"
        line=$(echo "$result" | grep "incorrect result" | tail -1)
        if [ -z "$line" ]; then
            FAIL=$((FAIL+1)); FAILURES="$FAILURES seed=$seed:TIMEOUT(FDB=$alive)"
            echo "FAIL seed $seed: TIMEOUT/HANG (FDB=$alive)"
        elif echo "$line" | grep -q "had 0 incorrect"; then
            PASS=$((PASS+1))
            if [ "$alive" = "DEAD" ]; then DEAD=$((DEAD+1)); echo "WARN seed $seed: PASS but FDB=$alive"; fi
        else
            FAIL=$((FAIL+1)); FAILURES="$FAILURES seed=$seed"
            echo "FAIL seed $seed: $line (FDB=$alive)"
        fi
        if [ $((seed % 10)) -eq 0 ]; then
            remaining=$(( (DEADLINE - $(date +%s)) / 60 ))
            echo "--- seed $seed (pass=$PASS fail=$FAIL dead=$DEAD) ${remaining}m remaining ---"
        fi
    done
    docker rm -f fdb-stress 2>/dev/null >/dev/null
    echo ""
    echo "Finished: $(date -Iseconds)"
    echo "========================================="
    echo "binding-stress: $PASS/$seed pass, $FAIL fail, $DEAD FDB deaths ({{ops}} ops/seed, timed)"
    [ -n "$FAILURES" ] && echo "Failures:$FAILURES"
    echo "========================================="
    } 2>&1 | tee "$REPORT"
    echo "Report: $REPORT"
    tail -5 "$REPORT" | grep -q "0 fail"

# Run tests with coverage
coverage:
    bazelisk coverage //...

# Run a specific test with forced rebuild (no stale binary)
test-fresh target *args:
    bazelisk test {{target}} --cache_test_results=no {{args}}
