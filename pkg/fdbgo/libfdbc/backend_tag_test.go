//go:build cgo && libfdbc_backend && !libfdbc

package libfdbc_test

import (
	"testing"

	"fdb.dev/pkg/fdbgo/libfdbc"
	"fdb.dev/pkg/internal/fdbclient"
)

// TestBackendTagLeavesTheRecordLayerPure pins what libfdbc_backend selects: the native
// backend package compiles, and the Record Layer's client seam stays pure-Go.
func TestBackendTagLeavesTheRecordLayerPure(t *testing.T) {
	t.Parallel()
	// COpenDatabase is declared only by the native files: this compiles only with them.
	_ = libfdbc.COpenDatabase
	if fdbclient.Backend != "pure-go" {
		t.Fatalf("-tags libfdbc_backend Backend = %q, want %q", fdbclient.Backend, "pure-go")
	}
}
