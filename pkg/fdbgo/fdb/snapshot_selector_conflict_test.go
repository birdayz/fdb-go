package fdb_test

// Regression: a SNAPSHOT range read with a non-trivial key selector must add NO
// read-conflict range — selector resolution must go through the snapshot GetKey,
// not the non-snapshot one. Before the fix, resolveSelector always used the
// non-snapshot tx.GetKey (which addGetKeyConflictRange), so a snapshot-only
// transaction that did nothing but a SelectorRange snapshot read could fail
// commit with not_committed(1020) where libfdb_c adds zero conflicts.
//
// C++ gates addConflictRange on !snapshot and resolves selectors within the same
// snapshot read (ReadYourWrites.actor.cpp:387-388).

import (
	"testing"

	"fdb.dev/pkg/fdbgo/fdb"
)

func TestSnapshot_SelectorRangeRead_AddsNoConflictRange(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	w := &conflictRangeWorkload{prefix: "snapcflct_" + t.Name() + "_", maxKeySpace: 20, maxOffset: 5}

	// Seed a dense, fully-known keyspace: D0..D19 + guards.
	mustCommit(t, db, func(tr fdb.WritableTransaction) {
		w.setGuards(tr)
		for i := 0; i < w.maxKeySpace; i++ {
			tr.Set(fdb.Key(w.dkey(i)), w.val(i))
		}
	})

	// tr1 pinned to a read version BEFORE the concurrent writer commits.
	tr1, err := db.CreateTransaction()
	if err != nil {
		t.Fatalf("CreateTransaction(tr1): %v", err)
	}
	tr1.SetReadVersion(mustReadVersion(t, db))

	// Concurrent writer modifies D19 — the key the End selector LastLessThan(D20)
	// resolves to, and which the pre-fix non-snapshot getKey conflict range
	// covered. (The snapshot DATA read of [D0,D19] adds no conflict, so the
	// selector resolution is the ONLY possible conflict source — which is exactly
	// the bug.)
	mustCommit(t, db, func(tr fdb.WritableTransaction) {
		tr.Set(fdb.Key(w.dkey(w.maxKeySpace-1)), w.val(99))
	})

	// tr1 does a SNAPSHOT selector range read: End = LastLessThan(D20) is
	// non-trivial (requires a getKey round-trip). With the fix it resolves via the
	// snapshot getKey and adds no read-conflict range.
	rr := tr1.Snapshot().GetRange(fdb.SelectorRange{
		Begin: fdb.FirstGreaterOrEqual(fdb.Key(w.dkey(0))),
		End:   fdb.LastLessThan(fdb.Key(w.dkey(w.maxKeySpace))),
	}, fdb.RangeOptions{})
	if _, rerr := rr.GetSliceWithError(); rerr != nil {
		t.Fatalf("snapshot selector GetRange: %v", rerr)
	}

	tr1.Clear(fdb.Key(w.dummyKey())) // make the transaction committable (non read-only)
	cerr := tr1.Commit().Get()
	if cerr != nil {
		code, _ := codeOf(cerr)
		t.Fatalf("REGRESSION: a SNAPSHOT selector range read added a spurious read-conflict range — tr1 "+
			"failed commit (code=%d, %v) despite reading at SNAPSHOT isolation. The selector must resolve via "+
			"the snapshot getKey; libfdb_c adds zero conflicts here.", code, cerr)
	}
}
