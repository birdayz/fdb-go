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

# Regenerate FDB wire schema (run when upgrading FDB version)
wire-schema:
    bazelisk build //cmd/fdb-wire-schema-generator:wire_schema
    cp bazel-bin/cmd/fdb-wire-schema-generator/wire_schema.json pkg/fdbgo/wire_schema.json
    @echo "Updated pkg/fdbgo/wire_schema.json"

# Run tests with coverage
coverage:
    bazelisk coverage //...
