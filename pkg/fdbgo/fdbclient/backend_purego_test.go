//go:build !libfdbc

package fdbclient

import "testing"

// TestBackend_DefaultIsPureGo pins the build-tag selection: a default build (no
// -tags libfdbc) must compile the pure-Go client. This is the test that runs in
// the normal suite; the libfdb_c variant is pinned by backend_libfdbc_test.go,
// which compiles only under -tags libfdbc.
func TestBackend_DefaultIsPureGo(t *testing.T) {
	t.Parallel()
	if Backend != "pure-go" {
		t.Fatalf("default build Backend = %q, want %q", Backend, "pure-go")
	}
}
