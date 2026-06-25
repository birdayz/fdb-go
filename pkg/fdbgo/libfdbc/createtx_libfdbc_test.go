//go:build cgo && libfdbc

package libfdbc_test

import (
	"bytes"
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/libfdbc"
)

// TestLibFDBC_CreateWritableTransaction proves the libfdb_c backend can create a
// STANDALONE (caller-managed) transaction as the fdb.WritableTransaction interface
// — the capability the SQL engine's database/sql explicit transactions (BeginTx /
// COMMIT) need (they span multiple driver calls and so can't use the closure-based
// Run gold path).
//
// Before BackendDatabase.CreateWritableTransaction existed, this was pure-Go-only:
// FDBDatabase.CreateTransaction returned the concrete pure-Go fdb.Transaction and
// fail-fasted with BackendCapabilityError on a non-pure-Go backend, so explicit SQL
// transactions were silently unavailable under -tags libfdbc. The C client is fully
// capable (it already creates transactions internally for its Transact loop); this
// is the regression that pins the now-exposed capability end to end against real FDB.
func TestLibFDBC_CreateWritableTransaction(t *testing.T) {
	t.Parallel()
	clusterFile := startCluster(t)

	be, err := libfdbc.Open(clusterFile)
	if err != nil {
		t.Fatalf("open libfdb_c backend: %v", err)
	}
	defer be.Close()

	key := fdb.Key("/test/libfdbc_createtx/k1")
	want := []byte("standalone-tx-value")

	// Write via a standalone transaction the CALLER commits (the BeginTx shape).
	w, err := be.CreateWritableTransaction()
	if err != nil {
		t.Fatalf("CreateWritableTransaction (write): %v", err)
	}
	w.Set(key, want)
	if err := w.Commit().Get(); err != nil {
		t.Fatalf("commit standalone tx: %v", err)
	}

	// Read it back via a second standalone transaction → proves the write committed
	// and a fresh standalone tx reads it.
	r, err := be.CreateWritableTransaction()
	if err != nil {
		t.Fatalf("CreateWritableTransaction (read): %v", err)
	}
	got, getErr := r.Get(key).Get()
	r.Cancel()
	if getErr != nil {
		t.Fatalf("get: %v", getErr)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("round-trip mismatch: got %q, want %q", got, want)
	}
}
