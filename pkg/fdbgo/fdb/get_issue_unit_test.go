package fdb

import (
	"context"
	"testing"

	"fdb.dev/pkg/fdbgo/client"
)

// Snapshot().Get is issued in the call, as fdb_transaction_get is (ThreadSafeTransaction.cpp:426-434),
// so a Set right after it is not visible to it.
func TestSnapshotGetIsIssuedInTheCall(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	inner := &client.Transaction{}
	t.Cleanup(cancel)
	t.Cleanup(inner.Cancel)
	tr := Transaction{t: &transaction{inner: inner, ctx: ctx}}
	for i := range 100 {
		tr.Set(Key("k"), []byte("old"))
		f := tr.Snapshot().Get(Key("k"))
		tr.Set(Key("k"), []byte("new"))
		if v, err := f.Get(); err != nil || string(v) != "old" {
			t.Fatalf("round %d: Snapshot().Get then Set = %q, %v; want \"old\"", i, v, err)
		}
	}
}
