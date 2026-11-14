//go:build !cgo || !(libfdbc || libfdbc_backend)

package libfdbc_test

import (
	"testing"

	"fdb.dev/pkg/fdbgo/libfdbc"
	"fdb.dev/pkg/internal/fdbclient"
)

// TestNoBackendTagIsTheStub pins the default build: without either tag the package is
// the stub, which opens nothing, and the Record Layer's client seam is pure-Go.
func TestNoBackendTagIsTheStub(t *testing.T) {
	t.Parallel()
	if db, err := libfdbc.Open("/nonexistent.cluster"); err == nil || db != nil {
		t.Fatalf("the stub opened %v, %v", db, err)
	}
	if err := libfdbc.SetKnob("x=1"); err == nil {
		t.Fatal("the stub set a knob")
	}
	if fdbclient.Backend != "pure-go" {
		t.Fatalf("default Backend = %q, want %q", fdbclient.Backend, "pure-go")
	}
}
