//go:build libfdbc

package fdbclient

import "testing"

// TestBackend_LibFDBCTagIsLibFDBC pins the libfdb_c variant of the build-tag
// selection. It compiles only under -tags libfdbc (a cgo build), the same
// configuration that links open_libfdbc.go, so `go test -tags libfdbc` exercises
// that the tag really swaps the compiled-in client.
func TestBackend_LibFDBCTagIsLibFDBC(t *testing.T) {
	t.Parallel()
	if Backend != "libfdb_c" {
		t.Fatalf("-tags libfdbc Backend = %q, want %q", Backend, "libfdb_c")
	}
}
