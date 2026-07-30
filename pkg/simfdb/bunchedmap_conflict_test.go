package simfdb_test

import (
	"errors"
	"testing"

	fdb "fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/simfdb"
)

// TestBunchedMapConcurrentPutDoesNotLoseUpdate is the record-layer-level regression for the
// snapshot fork.
//
// BunchedMap.Put is the canonical "snapshot read + explicit read conflict" shape: it takes
// tx.Snapshot() as the transaction's FIRST operation, range-reads the signpost around the map
// key, then declares the read conflict itself (addEntryListReadConflictRange /
// AddReadConflictKey) — Java's "Grand Theory of Conflict Ranges". Two Puts into the SAME bunch
// are a read-modify-write of one FDB key, so the loser must be told 1020 and must merge on
// retry. A Snapshot() that pins its own read version leaves the parent to pin a newer one at
// commit, the resolver sees the winner's commit version as not-newer, and the loser overwrites
// the bunch — the winner's entry silently disappears from the map.
func TestBunchedMapConcurrentPutDoesNotLoseUpdate(t *testing.T) {
	t.Parallel()
	db := simfdb.New(nil)
	ss := subspace.Sub(tuple.Tuple{"bm"})
	bm := recordlayer.NewBunchedMap(10) // bunch size 10: keys 0,1,2 all share one signpost

	// Seed a signpost so both concurrent Puts land in the SAME bunch.
	if _, err := db.Transact(func(tx fdb.WritableTransaction) (any, error) {
		_, _, err := bm.Put(tx, ss, tuple.Tuple{int64(0)}, []int{0})
		return nil, err
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	txA, err := db.CreateWritableTransaction()
	if err != nil {
		t.Fatalf("CreateWritableTransaction A: %v", err)
	}
	txB, err := db.CreateWritableTransaction()
	if err != nil {
		t.Fatalf("CreateWritableTransaction B: %v", err)
	}

	if _, _, err := bm.Put(txA, ss, tuple.Tuple{int64(1)}, []int{1}); err != nil {
		t.Fatalf("Put A: %v", err)
	}
	if _, _, err := bm.Put(txB, ss, tuple.Tuple{int64(2)}, []int{2}); err != nil {
		t.Fatalf("Put B: %v", err)
	}

	if err := txA.Commit().Get(); err != nil {
		t.Fatalf("commit A: %v", err)
	}

	err = txB.Commit().Get()
	var fe fdb.Error
	if !errors.As(err, &fe) || fe.Code != 1020 {
		t.Fatalf("commit B after a concurrent Put into the same bunch: got %v, want "+
			"not_committed(1020) — without it B overwrites the bunch and A's entry is lost", err)
	}

	// The retry must merge, not overwrite: re-run the Put on a fresh logical transaction.
	txB.Reset()
	if _, _, err := bm.Put(txB, ss, tuple.Tuple{int64(2)}, []int{2}); err != nil {
		t.Fatalf("Put B (retry): %v", err)
	}
	if err := txB.Commit().Get(); err != nil {
		t.Fatalf("commit B (retry): %v", err)
	}

	for _, key := range []int64{0, 1, 2} {
		got, err := db.Transact(func(tx fdb.WritableTransaction) (any, error) {
			ok, err := bm.ContainsKey(tx, ss, tuple.Tuple{key})
			return ok, err
		})
		if err != nil {
			t.Fatalf("ContainsKey(%d): %v", key, err)
		}
		if !got.(bool) {
			t.Errorf("key %d missing from the map after the concurrent Puts", key)
		}
	}
	if _, err := db.Transact(func(tx fdb.WritableTransaction) (any, error) {
		return nil, bm.VerifyIntegrity(tx, ss)
	}); err != nil {
		t.Fatalf("VerifyIntegrity: %v", err)
	}
}
