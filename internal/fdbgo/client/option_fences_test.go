package client

import (
	"context"
	"testing"
)

// TestReadLockAware_ReadOnlyState pins C++ Transaction::setOption: READ_LOCK_AWARE sets
// readOnly only when LOCK_AWARE is not already set, and LOCK_AWARE clears it
// (NativeAPI.actor.cpp:7071-7091).
func TestReadLockAware_ReadOnlyState(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		set      func(tx *Transaction)
		readOnly bool
	}{
		{"read_lock_aware", func(tx *Transaction) { tx.SetReadLockAware() }, true},
		{"lock_aware", func(tx *Transaction) { tx.SetLockAware() }, false},
		{"lock_then_read", func(tx *Transaction) { tx.SetLockAware(); tx.SetReadLockAware() }, false},
		{"read_then_lock", func(tx *Transaction) { tx.SetReadLockAware(); tx.SetLockAware() }, false},
		{"read_twice", func(tx *Transaction) { tx.SetReadLockAware(); tx.SetReadLockAware() }, true},
	}
	for _, tc := range cases {
		tx := newTestTx()
		tc.set(tx)
		if tx.readOnly != tc.readOnly {
			t.Errorf("%s: readOnly = %v, want %v", tc.name, tx.readOnly, tc.readOnly)
		}
		if !tx.lockAware && !tx.readLockAware {
			t.Errorf("%s: reads must be lock-aware", tc.name)
		}
	}
}

// TestReadLockAware_CommitWithWritesFails2023 pins the commitMutations readOnly check
// (NativeAPI.actor.cpp:6810-6811) for a mutation and for a bare write-conflict range;
// it precedes the size check, so a commit over the size limit still reports 2023.
func TestReadLockAware_CommitWithWritesFails2023(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		write func(tx *Transaction)
	}{
		{"set", func(tx *Transaction) { tx.Set([]byte("k"), []byte("v")) }},
		{"clear", func(tx *Transaction) { tx.Clear([]byte("k")) }},
		{"write_conflict_only", func(tx *Transaction) { tx.AddWriteConflictKey([]byte("k")) }},
		{"over_size_limit", func(tx *Transaction) { tx.SetSizeLimit(32); tx.Set([]byte("k"), make([]byte, 64)) }},
	}
	for _, tc := range cases {
		tx := newTestTx()
		tx.SetReadLockAware()
		tc.write(tx)
		if code := deferredCodeOf(tx.Commit(context.Background())); code != ErrTransactionReadOnly {
			t.Errorf("%s: Commit = %d, want 2023", tc.name, code)
		}
	}
}

// TestReadLockAware_ClearedByReset pins readOnly as a non-persistent NativeAPI option:
// TransactionOptions::clear resets it (NativeAPI.actor.cpp:6140) on retry and on reset.
func TestReadLockAware_ClearedByReset(t *testing.T) {
	t.Parallel()
	for _, userReset := range []bool{false, true} {
		tx := txWithDB()
		tx.SetReadLockAware()
		tx.reset(userReset)
		if tx.readOnly || tx.readLockAware {
			t.Errorf("reset(%v): readOnly=%v readLockAware=%v, want both cleared", userReset, tx.readOnly, tx.readLockAware)
		}
	}
}

// TestSizeLimit_OutOfRangeDefersAtSet pins SIZE_LIMIT's extractIntOption(value, 32, 1e7)
// (NativeAPI.actor.cpp:7067-7069, :2092-2104): an out-of-range value throws before
// assignment and the throw lands in deferredError, so the prior limit stays and every later
// op, not only the commit, returns 2006. 0 is out of range, not "no limit".
func TestSizeLimit_OutOfRangeDefersAtSet(t *testing.T) {
	t.Parallel()
	for _, bad := range []int64{0, -1, 31, transactionSizeLimit + 1} {
		tx := newTestTx()
		tx.SetSizeLimit(100)
		tx.SetSizeLimit(bad)
		if tx.sizeLimit != 100 {
			t.Errorf("SetSizeLimit(%d) applied: sizeLimit = %d, want 100", bad, tx.sizeLimit)
		}
		if _, err := tx.GetApproximateSize(); deferredCodeOf(err) != ErrInvalidOptionValue {
			t.Errorf("SetSizeLimit(%d): GetApproximateSize = %v, want 2006", bad, err)
		}
		tx.Set([]byte("k"), []byte("v"))
		if code := deferredCodeOf(tx.Commit(context.Background())); code != ErrInvalidOptionValue {
			t.Errorf("SetSizeLimit(%d): Commit = %d, want 2006", bad, code)
		}
	}
	for _, ok := range []int64{32, transactionSizeLimit} {
		tx := newTestTx()
		tx.SetSizeLimit(ok)
		if e := tx.deferredErr.Load(); e != nil || tx.sizeLimit != ok {
			t.Errorf("SetSizeLimit(%d): deferred=%v sizeLimit=%d, want nil/%d", ok, e, tx.sizeLimit, ok)
		}
	}
}

// TestSizeLimit_InvalidDBDefaultPoisons pins the DB-level default through the same check:
// each incarnation keeps the 10 MB limit and carries 2006, so no transaction runs unlimited.
func TestSizeLimit_InvalidDBDefaultPoisons(t *testing.T) {
	t.Parallel()
	for _, userReset := range []bool{false, true} {
		tx := txWithDB()
		tx.db.mutateDefaults(func(td *TransactionDefaults) { td.SizeLimit = 31 })
		tx.reset(userReset)
		if tx.sizeLimit != transactionSizeLimit {
			t.Errorf("reset(%v): sizeLimit = %d, want %d", userReset, tx.sizeLimit, transactionSizeLimit)
		}
		if e := tx.deferredErr.Load(); e == nil || e.Code != ErrInvalidOptionValue {
			t.Errorf("reset(%v): deferred = %v, want 2006", userReset, e)
		}
	}
}
