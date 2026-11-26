package simfdb

import "testing"

// TestInvertedReadRangeIsDroppedByFilter pins the reason the read side of the explicit
// conflict-range validation is not symmetric with the write side.
//
// AddWriteConflictRange's inverted_range(2005) rejection is a CONFLICT-VERDICT fix: addWriteConflict
// appends whatever it is given, and rangesOverlap — the plain half-open predicate
// `a.begin < b.end && b.begin < a.end` — is SATISFIED by an inverted [hi, lo) against any read range
// that straddles it, so an unvalidated inverted write range aborted innocent readers with 1020.
//
// AddReadConflictRange's is only a REPORTING fix — the caller now learns its range was rejected —
// because an inverted range never became a recorded read conflict in the first place. That is a
// negative result and it is load-bearing: it is the reason TestInvertedReadConflictRangeRejected's
// commit assertion cannot go red, and it is the only thing standing between the read path and the
// hazard the write path had.
//
// It is enforced REDUNDANTLY, at two levels — addFilteredReadConflict's own `begin >= end` early
// return (ryw_conflict.go:222) and writeMap.conflictRanges' identical one (:147). Neither is
// individually load-bearing: delete either alone and nothing observable changes. So this test pins
// the INVARIANT end-to-end and the deeper guard directly, rather than pretending one line is the
// thing that matters.
func TestInvertedReadRangeIsDroppedByFilter(t *testing.T) {
	t.Parallel()
	db := New(nil)
	tx := db.newTxn()

	// Straight to the filter, bypassing AddReadConflictRange's 2005 — the point is what the
	// filter does with an inverted range that reaches it, not whether one can.
	tx.addFilteredReadConflict([]byte("m"), []byte("c"))
	if n := len(tx.readConflicts); n != 0 {
		t.Fatalf("addFilteredReadConflict([m, c)) recorded %d range(s), want 0. Both inverted-range "+
			"guards are gone, which RE-ARMS on the read path the exact hazard "+
			"AddWriteConflictRange's inverted_range(2005) closes on the write path: rangesOverlap "+
			"matches an inverted [hi, lo) against any straddling range, so this transaction now "+
			"conflicts against writes it never read. got %v", n, tx.readConflicts)
	}

	// The forward control: the filter is not simply dropping everything.
	tx.addFilteredReadConflict([]byte("c"), []byte("m"))
	if n := len(tx.readConflicts); n != 1 {
		t.Fatalf("addFilteredReadConflict([c, m)) recorded %d range(s), want 1", n)
	}

	// The deeper guard, probed directly. This is the one that still holds when the early return
	// above is removed, so without this assertion the invariant is only half pinned.
	wm := tx.buildWriteMap()
	if got := wm.conflictRanges([]byte("m"), []byte("c")); got != nil {
		t.Fatalf("writeMap.conflictRanges([m, c)) = %v, want nil — the inverted-range guard at "+
			"ryw_conflict.go:147 is gone. It is the LAST line stopping an inverted explicit read "+
			"conflict range from being recorded and self-aborting its own transaction", got)
	}
	if got := wm.conflictRanges([]byte("c"), []byte("m")); len(got) != 1 {
		t.Fatalf("writeMap.conflictRanges([c, m)) = %v, want exactly one segment", got)
	}
}
