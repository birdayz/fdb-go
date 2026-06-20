// Package bindingtester runs the official FoundationDB binding tester against
// the pure-Go stacktester. Its test (binding_test.go) is tagged `bazelrunfiles`
// because it needs a Bazel-built Docker context from runfiles; this file keeps
// the package non-empty under a plain `go test ./...` (which excludes the test),
// so the standard Go test command stays clean (RFC-129).
package bindingtester
