package client

import (
	"bytes"
	"testing"
)

// TestAddReadConflictRange_RYWFilter pins that an explicit AddReadConflictRange /
// AddReadConflictKey runs through the RYW write-map filter (C++
// ReadYourWritesTransaction::addReadConflictRange → updateConflictMap): a segment
// the transaction already satisfied with a local INDEPENDENT write (a plain Set)
// must NOT be registered as a read-conflict. Adding the raw range OVER-CONFLICTS
// vs libfdb_c. When RYW is disabled there is no write map, so the full range is
// added (matching native).
//
// Revert-proof: restore `tx.addReadConflict(begin, end)` (raw) and the
// "independent write subtracted" / "key skips local write" cases go red.
func TestAddReadConflictRange_RYWFilter(t *testing.T) {
	t.Parallel()

	covers := func(rcs []KeyRange, k []byte) bool {
		for _, rc := range rcs {
			if bytes.Compare(k, rc.Begin) >= 0 && bytes.Compare(k, rc.End) < 0 {
				return true
			}
		}
		return false
	}

	// RYW enabled: a local Set(c) means c is served locally, so [a,e) must conflict
	// on the unmodified parts ([a,c) and [c+1,e)) but SKIP the written key c.
	t.Run("independent write subtracted from range", func(t *testing.T) {
		t.Parallel()
		tx := &Transaction{}
		tx.ryw.set([]byte("c"), []byte("v"))
		if err := tx.AddReadConflictRange([]byte("a"), []byte("e")); err != nil {
			t.Fatalf("AddReadConflictRange: %v", err)
		}
		if covers(tx.readConflicts, []byte("c")) {
			t.Fatalf("read-conflict covers locally-written key c — over-conflict vs libfdb_c: %v", tx.readConflicts)
		}
		if !covers(tx.readConflicts, []byte("a")) || !covers(tx.readConflicts, []byte("d")) {
			t.Fatalf("unmodified keys a and d must stay read-conflicted (no under-conflict): %v", tx.readConflicts)
		}
	})

	// RYW disabled: no write map → the full [a,e) range is added (no filtering).
	t.Run("rywDisabled keeps full range", func(t *testing.T) {
		t.Parallel()
		tx := &Transaction{rywDisabled: true}
		if err := tx.AddReadConflictRange([]byte("a"), []byte("e")); err != nil {
			t.Fatalf("AddReadConflictRange: %v", err)
		}
		if len(tx.readConflicts) != 1 ||
			!bytes.Equal(tx.readConflicts[0].Begin, []byte("a")) ||
			!bytes.Equal(tx.readConflicts[0].End, []byte("e")) {
			t.Fatalf("rywDisabled must add the full [a,e) range, got %v", tx.readConflicts)
		}
	})

	// AddReadConflictKey on a locally-written key adds NO read-conflict.
	t.Run("AddReadConflictKey skips local write", func(t *testing.T) {
		t.Parallel()
		tx := &Transaction{}
		tx.ryw.set([]byte("c"), []byte("v"))
		tx.AddReadConflictKey([]byte("c"))
		if len(tx.readConflicts) != 0 {
			t.Fatalf("AddReadConflictKey on a locally-written key must add no read-conflict, got %v", tx.readConflicts)
		}
	})

	// AddReadConflictKey on an unmodified key adds the full single-key conflict.
	t.Run("AddReadConflictKey unmodified conflicts", func(t *testing.T) {
		t.Parallel()
		tx := &Transaction{}
		tx.AddReadConflictKey([]byte("c"))
		if !covers(tx.readConflicts, []byte("c")) {
			t.Fatalf("AddReadConflictKey on an unmodified key must add a read-conflict, got %v", tx.readConflicts)
		}
	})
}
