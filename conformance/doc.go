// Package conformance holds the cross-engine conformance suite that compares the
// Go record layer against the Java reference. Every test is tagged `bazelrunfiles`
// because the suite needs the Java conformance-server jar (and Docker context)
// from Bazel runfiles; a plain `go test ./...` cannot start it. This file keeps
// the package non-empty so the standard Go test command stays clean (RFC-129).
package conformance
