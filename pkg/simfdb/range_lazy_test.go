package simfdb

import (
	"testing"

	fdb "fdb.dev/pkg/fdbgo/fdb"
)

// A range read is LAZY: each fetch resolves against the read-your-writes view AS IT IS AT FETCH
// TIME. These pin the consequence in the one ordering that can tell a lazy read from an eager
// one — a write issued BETWEEN the GetRange() call and the consumption of its result.
//
// The bug they exist for: SimFDB used to materialize the whole range at GetRange() call time
// while registering the read conflict per batch at CONSUMPTION, filtered through the write map
// as of consumption. A write landing in between was then wrong twice. It was invisible in the
// returned rows (a real client's RYW shows it), AND its span was subtracted from the read
// conflict as locally-satisfied — even though the row handed back had come from STORAGE. A
// concurrent writer of that key therefore did not abort the reader, and SimFDB certified a lost
// update. Both halves are asserted here, because a fix that repairs only the rows leaves the
// conflict arithmetic resting on a claim that is no longer checked anywhere.
//
// The rows are also pinned against a real cluster by TestDifferentialArm_ReadThenLocalWrite.

// seedLazy lays down the alphabet the shapes below scan.
func seedLazy(db *SimDB) {
	seed(db, "a", "db", "b", "db", "c", "db", "d", "db", "e", "db", "f", "db")
}

func valueOf(kvs []fdb.KeyValue, key string) string {
	for _, kv := range kvs {
		if string(kv.Key) == key {
			return string(kv.Value)
		}
	}
	return "<absent>"
}

// TestWriteBetweenGetRangeAndGetSliceIsVisible is shape A: the write lands after GetRange()
// returns its handle and before GetSliceWithError consumes it. The client resolves the whole
// slice inside GetSliceWithError (goRangeResult holds only the request), so the write is in the
// view the read resolves against.
func TestWriteBetweenGetRangeAndGetSliceIsVisible(t *testing.T) {
	t.Parallel()
	db := New(nil)
	seedLazy(db)

	tx := db.newTxn()
	rr := tx.GetRange(fdb.KeyRange{Begin: k("a"), End: k("z")}, fdb.RangeOptions{})
	tx.Set(k("e"), []byte("mine"))
	kvs := rr.GetSliceOrPanic()

	if got := valueOf(kvs, "e"); got != "mine" {
		t.Fatalf("row e = %q, want \"mine\" — the range read was issued before the write but "+
			"CONSUMED after it, so the write is in the view it resolves against; %q is the "+
			"stored value, i.e. the result was materialized at GetRange() time", got, "mine")
	}
	// The conflict filter subtracted e's slot. That is correct ONLY because the row above
	// really did come from the buffer, so the two are asserted together: a concurrent writer of
	// e must not abort this reader. The rest of the coverage is probed by
	// TestWriteBetweenGetRangeAndGetSliceCoverage.
	commitAfterConcurrentSet(t, db, tx, "e", 0)
}

// TestWriteBetweenGetRangeAndGetSliceCoverage is the conflict half of shape A, stated as
// OUTCOMES rather than as the shape of the recorded list.
//
// A span is subtracted from the read conflict IFF the buffer answered the read that span
// covers. Recording ["a","e") and ["e\x00","z") separately and recording their union are the
// same behaviour — only the coverage is observable — so asserting the list would pin a
// representation and stand in the way of teaching SimFDB to coalesce read-conflict ranges the
// way a real client does.
func TestWriteBetweenGetRangeAndGetSliceCoverage(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		probe string
		want  int
		why   string
	}{
		{"e", 0, "the buffer answered this row, so the read never depended on storage"},
		{"d", 1020, "resolved from storage, so a concurrent writer must abort the reader"},
		{"a", 1020, "resolved from storage"},
		{"zz", 0, "outside the requested range"},
	} {
		tc := tc
		t.Run(tc.probe, func(t *testing.T) {
			t.Parallel()
			db := New(nil)
			seedLazy(db)
			tx := db.newTxn()
			rr := tx.GetRange(fdb.KeyRange{Begin: k("a"), End: k("z")}, fdb.RangeOptions{})
			tx.Set(k("e"), []byte("mine"))
			rr.GetSliceOrPanic()
			t.Log(tc.why)
			commitAfterConcurrentSet(t, db, tx, tc.probe, tc.want)
		})
	}
}

// TestWriteMidIterationIsVisibleInLaterBatches is shape B — the record layer's scan-and-update
// cursor. Two rows are consumed, a write lands, and the scan drains. The write must appear in
// the batches fetched AFTER it and must not retroactively alter the rows already returned,
// which is exactly what the client does: each Advance() that needs a new page re-reads
// [begin,end) through RYW, and pages already handed back are history.
func TestWriteMidIterationIsVisibleInLaterBatches(t *testing.T) {
	t.Parallel()
	db := New(nil)
	seedLazy(db)

	tx := db.newTxn()
	it := tx.GetRange(fdb.KeyRange{Begin: k("a"), End: k("z")},
		fdb.RangeOptions{Mode: fdb.StreamingModeIterator}).Iterator()

	// Batch 1 under StreamingModeIterator is 2 rows: a, b.
	var fetched []fdb.KeyValue
	for i := 0; i < 2; i++ {
		if !it.Advance() {
			t.Fatalf("iterator exhausted after %d rows", i)
		}
		fetched = append(fetched, it.MustGet())
	}
	// One write lands INSIDE the not-yet-fetched tail, one on a row already returned.
	tx.Set(k("e"), []byte("mine"))
	tx.Set(k("a"), []byte("late"))
	for it.Advance() {
		fetched = append(fetched, it.MustGet())
	}

	if got := valueOf(fetched, "e"); got != "mine" {
		t.Fatalf("row e = %q, want \"mine\" — e was fetched AFTER the write, so the batch that "+
			"returned it must resolve against a view containing the write", got)
	}
	if got := valueOf(fetched, "a"); got != "db" {
		t.Fatalf("row a = %q, want \"db\" — a was returned by a batch fetched BEFORE the write; "+
			"a lazy read does not rewrite pages it has already handed back", got)
	}
	// a was returned by a batch fetched BEFORE the write to a landed, so its conflict was
	// already banked against storage and a concurrent writer of a must abort us — even though a
	// is in the write buffer by the time the transaction commits.
	commitAfterConcurrentSet(t, db, tx, "a", 1020)
}

// TestWriteMidIterationCoverage is the conflict half of shape B: a span is subtracted from the
// read conflict IFF the buffer answered the read that span covers. Stated as commit outcomes,
// so it says nothing about how the ranges happen to be grouped.
func TestWriteMidIterationCoverage(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		probe string
		want  int
		why   string
	}{
		{"a", 1020, "returned by a batch fetched BEFORE the write to a, so it came from storage"},
		{"b", 1020, "returned by the first batch, from storage"},
		{"e", 0, "fetched AFTER the write to e, so the buffer answered it"},
		{"d", 1020, "fetched after the writes but resolved from storage"},
	} {
		tc := tc
		t.Run(tc.probe, func(t *testing.T) {
			t.Parallel()
			db := New(nil)
			seedLazy(db)
			tx := db.newTxn()
			it := tx.GetRange(fdb.KeyRange{Begin: k("a"), End: k("z")},
				fdb.RangeOptions{Mode: fdb.StreamingModeIterator}).Iterator()
			for i := 0; i < 2; i++ {
				if !it.Advance() {
					t.Fatalf("iterator exhausted after %d rows", i)
				}
			}
			tx.Set(k("e"), []byte("mine"))
			tx.Set(k("a"), []byte("late"))
			for it.Advance() {
			}
			t.Log(tc.why)
			commitAfterConcurrentSet(t, db, tx, tc.probe, tc.want)
		})
	}
}

// TestWriteMidIterationShiftsWhichRowsALimitReturns is the sharper form of shape B: with a row
// limit, a phantom inserted ahead of the cursor does not merely change a value, it changes WHICH
// rows come back and where the scan stops. An eagerly-materialized result cannot express that at
// all — its row set was decided before the insert existed.
func TestWriteMidIterationShiftsWhichRowsALimitReturns(t *testing.T) {
	t.Parallel()
	db := New(nil)
	seedLazy(db)

	tx := db.newTxn()
	it := tx.GetRange(fdb.KeyRange{Begin: k("a"), End: k("z")},
		fdb.RangeOptions{Mode: fdb.StreamingModeIterator, Limit: 4}).Iterator()
	for i := 0; i < 2; i++ {
		if !it.Advance() {
			t.Fatalf("iterator exhausted after %d rows", i)
		}
	}
	// "bb" sorts into the unread tail, ahead of c.
	tx.Set(k("bb"), []byte("phantom"))
	var got []string
	for it.Advance() {
		got = append(got, string(it.MustGet().Key))
	}
	want := []string{"bb", "c"} // a,b already consumed; the 4-row budget stops after c
	if len(got) != len(want) {
		t.Fatalf("tail rows = %v, want %v — the insert must consume budget from the rows "+
			"fetched after it", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("tail rows = %v, want %v", got, want)
		}
	}
}

// TestClearMidIterationRemovesRowsFromLaterBatches is the negative direction of the same rule:
// a local Clear must remove a row from a batch not yet fetched. A read that only ever ADDS the
// buffer's values on top of a materialized set passes the Set-shaped tests and fails this one.
func TestClearMidIterationRemovesRowsFromLaterBatches(t *testing.T) {
	t.Parallel()
	db := New(nil)
	seedLazy(db)

	tx := db.newTxn()
	it := tx.GetRange(fdb.KeyRange{Begin: k("a"), End: k("z")},
		fdb.RangeOptions{Mode: fdb.StreamingModeIterator}).Iterator()
	for i := 0; i < 2; i++ {
		if !it.Advance() {
			t.Fatalf("iterator exhausted after %d rows", i)
		}
	}
	tx.ClearRange(fdb.KeyRange{Begin: k("d"), End: k("f")})
	var got []string
	for it.Advance() {
		got = append(got, string(it.MustGet().Key))
	}
	want := []string{"c", "f"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("tail rows = %v, want %v — the cleared span must be absent from batches "+
			"fetched after the clear", got, want)
	}
}

// TestWriteBeforeGetRangeStillReads is the control: the ordering the old eager implementation
// DID get right must stay right. If this ever goes red, the fix broke plain read-your-writes
// rather than the write-after-issue case.
func TestWriteBeforeGetRangeStillReads(t *testing.T) {
	t.Parallel()
	db := New(nil)
	seedLazy(db)

	tx := db.newTxn()
	tx.Set(k("e"), []byte("mine"))
	kvs := tx.GetRange(fdb.KeyRange{Begin: k("a"), End: k("z")}, fdb.RangeOptions{}).GetSliceOrPanic()
	if got := valueOf(kvs, "e"); got != "mine" {
		t.Fatalf("row e = %q, want \"mine\"", got)
	}
	commitAfterConcurrentSet(t, db, tx, "e", 0)
}

// TestCancelBetweenGetRangeAndConsumptionFailsTheRead pins that the range read's cancellation
// check happens where the READ does — at consumption — not at GetRange() call time. The client's
// GetRange builds a handle and touches nothing; the 1025 comes from ensureReadVersion inside the
// fetch. A sim that checked at call time would answer a read no real client answers.
func TestCancelBetweenGetRangeAndConsumptionFailsTheRead(t *testing.T) {
	t.Parallel()
	db := New(nil)
	seedLazy(db)

	tx := db.newTxn()
	rr := tx.GetRange(fdb.KeyRange{Begin: k("a"), End: k("z")}, fdb.RangeOptions{})
	tx.Cancel()
	if _, err := rr.GetSliceWithError(); errCode(t, err) != 1025 {
		t.Fatalf("GetSliceWithError after Cancel = %v, want transaction_cancelled(1025)", err)
	}
	it := tx.GetRange(fdb.KeyRange{Begin: k("a"), End: k("z")}, fdb.RangeOptions{}).Iterator()
	it.Advance()
	if _, err := it.Get(); errCode(t, err) != 1025 {
		t.Fatalf("Iterator Get after Cancel = %v, want transaction_cancelled(1025)", err)
	}
}

// commitAfterConcurrentSet commits tx after a concurrent transaction writes key, and asserts the
// resulting code (0 for success, 1020 for not_committed).
func commitAfterConcurrentSet(t *testing.T, db *SimDB, tx *simTxn, key string, want int) {
	t.Helper()
	if _, err := db.Transact(func(w fdb.WritableTransaction) (any, error) {
		w.Set(k(key), []byte("concurrent"))
		return nil, nil
	}); err != nil {
		t.Fatalf("concurrent write: %v", err)
	}
	tx.Set(k("zzz"), []byte("x")) // make it a write transaction so the resolver runs
	err := tx.Commit().Get()
	got := 0
	if err != nil {
		got = errCode(t, err)
	}
	if got != want {
		t.Fatalf("commit after a concurrent write to %q = code %d (%v), want %d "+
			"(conflicts were %v)", key, got, err, want, conflictStrings(tx))
	}
}
