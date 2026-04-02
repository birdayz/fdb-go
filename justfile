default:
    @just --list

# Generate protobuf code (unchanged — not in Bazel)
generate:
    buf generate

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
    bazelisk build //cmd/fdb-schema-extract:generate_wire_types
    rm -f pkg/fdbgo/wire/types/*_generated.go
    tar xf bazel-bin/cmd/fdb-schema-extract/wire_types.tar -C pkg/fdbgo/wire/types/ --strip-components=1
    just gazelle
    @echo "Generated: $$(ls pkg/fdbgo/wire/types/*_generated.go | wc -l) files"

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
    bazelisk test //cmd/fdb-stacktester/bindingtester --test_output=streamed

# Run tests with coverage
coverage:
    bazelisk coverage //...
