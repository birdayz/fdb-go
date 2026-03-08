default:
    @just --list

# Generate protobuf code (unchanged — not in Bazel)
generate:
    buf generate

# Build all targets (includes nogo lint)
build:
    bazel build //...

# Test all targets
test:
    bazel test //...

# Run conformance server
run-conformance-server:
    bazel run //conformance/java:conformance_server

# Regenerate BUILD files after adding/removing Go files or deps
gazelle:
    bazel run //:gazelle

# Clean bazel outputs
clean:
    bazel clean

# Go mod tidy
tidy:
    go mod tidy
