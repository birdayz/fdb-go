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

# Binding tester stress: N seeds × M ops (report + logs → binding-stress-out/)
binding-stress runs="100" ops="1000":
    bazelisk run //cmd/fdb-binding-stress -- -seeds {{runs}} -ops {{ops}}

# Binding tester stress for a duration (e.g. 2h, 30m)
binding-stress-duration duration ops="1000":
    bazelisk run //cmd/fdb-binding-stress -- -duration {{duration}} -ops {{ops}}

# Run tests with coverage
coverage:
    bazelisk coverage //...

# Run a specific test with forced rebuild (no stale binary)
test-fresh target *args:
    bazelisk test {{target}} --cache_test_results=no {{args}}
