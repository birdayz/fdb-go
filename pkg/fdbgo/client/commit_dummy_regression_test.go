package client

import (
	"bytes"
	"context"
	"testing"
)

// TestCommitDummyTransaction_DoesNotOverwriteUserKey pins the fix for a data-
// corruption bug: commitDummyTransaction (the commit_unknown_result idempotency
// barrier) used to do `dummy.Set(key, "")` on a key taken from the original
// transaction's conflict ranges — a real user key (e.g. a record's unsplit
// key). Committing SET(key, "") overwrote that key's value with empty, which a
// later read sees as present-empty (HasValue=true, len 0).
//
// The C++ commitDummyTransaction (NativeAPI.actor.cpp:6306) adds ONLY a read +
// write conflict range and commits — it never writes a value. The write
// conflict range alone keeps the dummy from being optimized away as a no-op.
//
// Revert-proof: with the stray `dummy.Set(key, []byte{})` restored, this test
// reads back an EMPTY value and fails. With the fix (conflict ranges only) the
// user key keeps its original value.
func TestCommitDummyTransaction_DoesNotOverwriteUserKey(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openTestDB(t, ctx)

	key := []byte("\x02commit-dummy-regression\x00\x15\x01\x16\x05\x39\x14") // record-like user key
	want := []byte("a-real-record-value-that-must-survive")

	// 1. Commit the key with a real, non-empty value.
	if _, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(key, want)
		return nil, nil
	}); err != nil {
		t.Fatalf("seed write: %v", err)
	}

	// 2. Run commitDummyTransaction with `key` in the transaction's conflict
	//    ranges (exactly how the real commit_unknown_result path reaches it).
	//    Abort the outer transaction so ITS Set never commits — only the dummy
	//    barrier's effect (if any) is observed.
	if _, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(key, []byte("uncommitted")) // puts key in writeConflicts
		tx.addReadConflictForKey(key)      // ...and readConflicts (matches intersect path)
		tx.commitDummyTransaction(ctx)     // the barrier — must not touch `key`'s value
		return nil, errAbortRegression     // abort: do not commit the outer tx's Set
	}); err != errAbortRegression {
		t.Fatalf("expected abort sentinel, got %v", err)
	}

	// 3. The seeded value must be intact — not overwritten to empty by the dummy.
	got, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.Get(ctx, key)
	})
	if err != nil {
		t.Fatalf("read-back: %v", err)
	}
	gotBytes, _ := got.([]byte)
	if gotBytes == nil {
		t.Fatalf("key vanished (nil) after commitDummyTransaction")
	}
	if len(gotBytes) == 0 {
		t.Fatalf("BUG: commitDummyTransaction overwrote the user key with an EMPTY value (present-empty)")
	}
	if !bytes.Equal(gotBytes, want) {
		t.Fatalf("user key value changed: got %q want %q", gotBytes, want)
	}
}

type abortRegressionErr struct{}

func (abortRegressionErr) Error() string { return "abort regression (intentional)" }

var errAbortRegression = abortRegressionErr{}
