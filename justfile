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
    gofmt -w $(find . -name '*.go' -not -path './fdb-record-layer/*' -not -path './bazel-*' -not -path './gen/*')

# Run tests with coverage
coverage:
    bazelisk coverage //...
