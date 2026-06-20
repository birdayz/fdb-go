// Package docscheck holds repository documentation-consistency guards (RFC-131).
//
// It is a test-only package: the assertions live in docs_consistency_test.go. The
// package declaration keeps `go test ./...` from reporting "build constraints exclude
// all Go files" for this directory.
package docscheck
