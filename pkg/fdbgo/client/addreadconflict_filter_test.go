package client

import "testing"

// TestAddReadConflict_FiltersSelfWrite pins the RYW write-map filter on the explicit
// AddReadConflictRange/Key APIs (C++ addReadConflictRange → updateConflictMap,
// ReadYourWrites.actor.cpp:1986). A read-conflict over a locally-written INDEPENDENT key is
// subtracted (the Set satisfied it with no DB read), so a concurrent write to that key must not
// abort the transaction — matching libfdb_c. Pre-fix Go added the full range unconditionally →
// over-conflict → spurious not_committed (1020). White-box on tx.readConflicts (no FDB needed).
// Revert-proof: backing out the rywDisabled/else split in AddReadConflictRange/Key fails the
// self-written subtests.
func TestAddReadConflict_FiltersSelfWrite(t *testing.T) {
	t.Parallel()

	t.Run("key_self_written_no_conflict", func(t *testing.T) {
		tx := newTestTx()
		k := []byte("k")
		tx.Set(k, []byte("v"))   // local independent write
		tx.AddReadConflictKey(k) // C++ updateConflictMap subtracts the self-written key
		if n := len(tx.readConflicts); n != 0 {
			t.Fatalf("AddReadConflictKey on a self-written key must add NO conflict, got %d", n)
		}
	})

	t.Run("key_unwritten_conflicts", func(t *testing.T) {
		tx := newTestTx()
		tx.AddReadConflictKey([]byte("other")) // unmodified gap → a real conflict
		if n := len(tx.readConflicts); n != 1 {
			t.Fatalf("AddReadConflictKey on an unwritten key must add a conflict, got %d", n)
		}
	})

	t.Run("range_over_self_written_key_no_conflict", func(t *testing.T) {
		tx := newTestTx()
		k := []byte("m")
		tx.Set(k, []byte("v"))
		// [m, m\x00) covers exactly the self-written key m.
		if err := tx.AddReadConflictRange(k, append(append([]byte(nil), k...), 0)); err != nil {
			t.Fatalf("AddReadConflictRange: %v", err)
		}
		if n := len(tx.readConflicts); n != 0 {
			t.Fatalf("AddReadConflictRange over a self-written key must add NO conflict, got %d", n)
		}
	})

	t.Run("rywDisabled_adds_full_unfiltered", func(t *testing.T) {
		tx := newTestTx()
		tx.rywDisabled = true
		tx.AddReadConflictKey([]byte("k")) // rywDisabled → full conflict, no filter (C++ :1979)
		if n := len(tx.readConflicts); n != 1 {
			t.Fatalf("rywDisabled AddReadConflictKey must add the full conflict, got %d", n)
		}
	})
}
