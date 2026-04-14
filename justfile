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

# Generate protobuf code (clean + regenerate all)
generate: ensure-buf
    rm -rf gen/
    .tools/buf generate
    bazelisk run //:gazelle

# Build all targets (includes nogo lint)
build:
    bazelisk build //...

# Test all targets.
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

# Format Go source files using Bazel-managed gofumpt (same version as nogo linter)
fmt:
    #!/usr/bin/env bash
    set -euo pipefail
    GOFUMPT=$(bazelisk run --run_under="echo" @cc_mvdan_gofumpt//:gofumpt 2>/dev/null)
    find . -name '*.go' -not -path './fdb-record-layer/*' -not -path './bazel-*' -not -path './gen/*' -not -path './.claude/*' -exec "$GOFUMPT" -w {} +

# Check Go formatting (fails if any file needs formatting)
# Uses Bazel-managed gofumpt — same version as nogo linter, no drift.
lint:
    #!/usr/bin/env bash
    set -euo pipefail
    GOFUMPT=$(bazelisk run --run_under="echo" @cc_mvdan_gofumpt//:gofumpt 2>/dev/null)
    unformatted=$("$GOFUMPT" -l $(find . -name '*.go' -not -path './fdb-record-layer/*' -not -path './bazel-*' -not -path './gen/*' -not -path './.claude/*'))
    if [ -n "$unformatted" ]; then
        echo "Unformatted files:"
        echo "$unformatted"
        echo "Run 'just fmt' to fix."
        exit 1
    fi

# Run all benchmarks (skips Ginkgo specs, runs only Go benchmarks).
# Uses 'bazelisk run' to avoid polluting the test action cache.
bench:
    #!/usr/bin/env bash
    set -euo pipefail
    bazelisk build //pkg/recordlayer:recordlayer_test
    BAZEL_BIN=$(bazelisk info bazel-bin)
    timeout 300 "$BAZEL_BIN/pkg/recordlayer/recordlayer_test_/recordlayer_test" \
        -test.run='^$' -test.bench=. -test.benchtime=3s -test.benchmem --ginkgo.skip='.*'

# Run a specific benchmark by name regex
bench-one NAME:
    #!/usr/bin/env bash
    set -euo pipefail
    bazelisk build //pkg/recordlayer:recordlayer_test
    BAZEL_BIN=$(bazelisk info bazel-bin)
    timeout 300 "$BAZEL_BIN/pkg/recordlayer/recordlayer_test_/recordlayer_test" \
        -test.run='^$' -test.bench='{{NAME}}' -test.benchtime=3s -test.benchmem --ginkgo.skip='.*'

# Run benchmarks for CI, capture results to bench-results.txt.
# Only runs record layer + wire type benchmarks (no FDB-heavy client bench).
# Uses benchtime=1s for speed — sufficient for regression detection.
# IMPORTANT: Uses 'bazelisk run' (not 'bazelisk test') so benchmark execution
# does not overwrite the test action cache. Using 'bazelisk test' with different
# --test_arg/--test_timeout flags overwrites the cached test result, causing
# the test step on the NEXT CI run to re-execute instead of hitting cache.
bench-ci:
    #!/usr/bin/env bash
    set -euo pipefail
    rm -f bench-raw.txt bench-results.txt
    bazelisk build //pkg/recordlayer:recordlayer_test //pkg/fdbgo/wire/types:types_test
    BAZEL_BIN=$(bazelisk info bazel-bin)
    {
        echo "=== Running benchmarks: //pkg/recordlayer:recordlayer_test ==="
        timeout 300 "$BAZEL_BIN/pkg/recordlayer/recordlayer_test_/recordlayer_test" \
            -test.run='^$' \
            -test.bench=. \
            -test.benchmem \
            -test.benchtime=1s \
            --ginkgo.skip='.*' 2>&1 || true
        echo "=== Running benchmarks: //pkg/fdbgo/wire/types:types_test ==="
        timeout 60 "$BAZEL_BIN/pkg/fdbgo/wire/types/types_test_/types_test" \
            -test.run='^$' \
            -test.bench=. \
            -test.benchmem \
            -test.benchtime=1s 2>&1 || true
    } | tee bench-raw.txt
    # Extract benchmark lines for bench-report.
    grep -E '^(Benchmark|goos:|goarch:|pkg:|cpu:)' bench-raw.txt > bench-results.txt || true
    NRESULTS=$(grep -c '^Benchmark' bench-results.txt || echo 0)
    echo "Benchmarks: $NRESULTS results → bench-results.txt"

# Compare benchmark results: just bench-report old.txt new.txt
bench-report old new:
    bazelisk run //cmd/bench-report -- -old {{old}} -new {{new}}

# Run Go vs Java performance comparison benchmark.
# Uses 'bazelisk run' to avoid polluting the test action cache.
bench-compare:
    #!/usr/bin/env bash
    set -euo pipefail
    bazelisk build //conformance:conformance_test
    BAZEL_BIN=$(bazelisk info bazel-bin)
    timeout 600 "$BAZEL_BIN/conformance/conformance_test_/conformance_test" \
        --ginkgo.focus='Performance Comparison' --ginkgo.v

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

# Binding tester stress: N seeds × M ops (report + logs → binding-stress-out/)
binding-stress runs="100" ops="1000":
    bazelisk run //cmd/fdb-binding-stress -- -seeds {{runs}} -ops {{ops}}

# Binding tester stress for a duration (e.g. 2h, 30m)
binding-stress-duration duration ops="1000":
    bazelisk run //cmd/fdb-binding-stress -- -duration {{duration}} -ops {{ops}}

# Directory layer binding stress: N seeds × M ops
binding-stress-directory runs="50" ops="500":
    bazelisk run //cmd/fdb-binding-stress -- -seeds {{runs}} -ops {{ops}} -test-name directory

# Generate HTML test report from the latest Bazel test run.
# Reads .bazel-bep.jsonl (produced automatically by every `just test` via .bazelrc).
# Builds the tool then runs it outside the sandbox (needs access to BEP + test.xml files).
report:
    #!/usr/bin/env bash
    set -euo pipefail
    if [ ! -f .bazel-bep.jsonl ]; then
        echo "error: .bazel-bep.jsonl not found — run 'just test' first" >&2
        exit 1
    fi
    bazelisk build //cmd/test-report
    BAZEL_BIN=$(bazelisk info bazel-bin)
    "$BAZEL_BIN/cmd/test-report/test-report_/test-report" .bazel-bep.jsonl > test-report.html
    TOTAL=$(grep 'stat-total' test-report.html | grep -oP '>\K\d+(?=</span>)' || echo '?')
    echo "Report: test-report.html ($TOTAL tests)"

# Run tests with coverage and generate HTML report
coverage:
    #!/usr/bin/env bash
    set -euo pipefail
    bazelisk coverage //... --combined_report=lcov
    LCOV=$(bazelisk info output_path 2>/dev/null)/_coverage/_coverage_report.dat
    bazelisk build //cmd/test-report
    BAZEL_BIN=$(bazelisk info bazel-bin)
    "$BAZEL_BIN/cmd/test-report/test-report_/test-report" -coverage "$LCOV" .bazel-bep.jsonl > test-report.html
    TOTAL=$(grep 'stat-total' test-report.html | grep -oP '>\K\d+(?=</span>)' || echo '?')
    COV=$(grep -oP 'Coverage</span></div>' test-report.html || true)
    COV_PCT=$(grep -oP 'style="color: #[0-9a-f]+">\K[0-9.]+%' test-report.html | head -1 || echo '?')
    echo "Report: test-report.html ($TOTAL tests, $COV_PCT coverage)"

# Run tests with race detector (slower — recompiles with instrumentation)
race:
    bazelisk test //pkg/fdbgo/client:client_test --@rules_go//go/config:race --test_timeout=300

# Run a specific test with forced rebuild (no stale binary)
test-fresh target *args:
    bazelisk test {{target}} --cache_test_results=no {{args}}
