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

# Check Go formatting + style rules (fails if any file needs fixing)
lint:
    #!/usr/bin/env bash
    set -euo pipefail
    gofiles=$(find . -name '*.go' -not -path './fdb-record-layer/*' -not -path './bazel-*' -not -path './gen/*' -not -path './.claude/*')
    unformatted=$(gofmt -l $gofiles)
    if [ -n "$unformatted" ]; then
        echo "Unformatted files:"
        echo "$unformatted"
        echo "Run 'just fmt' to fix."
        exit 1
    fi
    # Ban interface{} — use any instead (Go 1.18+)
    offenders=$(grep -rn 'interface{}' $gofiles || true)
    if [ -n "$offenders" ]; then
        echo "Use 'any' instead of 'interface{}':"
        echo "$offenders"
        echo "Run: sed -i 's/interface{}/any/g' <files>"
        exit 1
    fi

# Run all benchmarks (skips Ginkgo specs, runs only Go benchmarks)
bench:
    bazelisk test //pkg/recordlayer:recordlayer_test --test_arg="-test.bench=." --test_arg="-test.benchtime=3s" --test_arg="--ginkgo.skip=.*" --test_output=all --nocache_test_results --test_timeout=300

# Run a specific benchmark by name regex
bench-one NAME:
    bazelisk test //pkg/recordlayer:recordlayer_test --test_arg="-test.bench={{NAME}}" --test_arg="-test.benchtime=3s" --test_arg="--ginkgo.skip=.*" --test_output=all --nocache_test_results --test_timeout=300

# Regenerate FDB wire schema (Bazel, sandboxed)
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

# Run tests with coverage
coverage:
    bazelisk coverage //...
