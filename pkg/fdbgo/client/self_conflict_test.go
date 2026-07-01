package client

import (
	"bytes"
	"testing"
)

func krFrom(b, e string) KeyRange { return KeyRange{Begin: []byte(b), End: []byte(e)} }

// TestConflictRangesIntersect pins the overlap predicate that gates makeSelfConflicting (finding #27),
// mirroring C++ intersects(write_cr, read_cr) (NativeAPI.actor.cpp:6859): [a,b) overlaps [c,d) ⟺ a<d && c<b.
func TestConflictRangesIntersect(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		w, r []KeyRange
		want bool
	}{
		{"no_read_conflicts", []KeyRange{krFrom("a", "b")}, nil, false},
		{"disjoint", []KeyRange{krFrom("a", "b")}, []KeyRange{krFrom("c", "d")}, false},
		{"touching_no_overlap", []KeyRange{krFrom("a", "b")}, []KeyRange{krFrom("b", "c")}, false},
		{"overlap", []KeyRange{krFrom("a", "m")}, []KeyRange{krFrom("g", "z")}, true},
		{"contained", []KeyRange{krFrom("a", "z")}, []KeyRange{krFrom("m", "n")}, true},
		{"second_pair_overlaps", []KeyRange{krFrom("a", "b"), krFrom("m", "z")}, []KeyRange{krFrom("p", "q")}, true},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := conflictRangesIntersect(c.w, c.r); got != c.want {
				t.Fatalf("conflictRangesIntersect = %v, want %v", got, c.want)
			}
		})
	}
}

// TestMakeSelfConflicting_AddsSCRangeToBoth pins that makeSelfConflictingLocked adds ONE ephemeral
// \xFF/SC/<16-byte UID> single-key range to BOTH the read and write conflict sets, at the SAME key
// (C++ Transaction::makeSelfConflicting, NativeAPI.actor.cpp:5952 — one singleKeyRange pushed to both).
func TestMakeSelfConflicting_AddsSCRangeToBoth(t *testing.T) {
	t.Parallel()
	tx := newTestTx()
	tx.conflictMu.Lock()
	tx.makeSelfConflictingLocked()
	tx.conflictMu.Unlock()
	if len(tx.readConflicts) != 1 || len(tx.writeConflicts) != 1 {
		t.Fatalf("must add one range to each set, got read=%d write=%d", len(tx.readConflicts), len(tx.writeConflicts))
	}
	for _, cr := range []KeyRange{tx.readConflicts[0], tx.writeConflicts[0]} {
		if !bytes.HasPrefix(cr.Begin, selfConflictPrefix) {
			t.Fatalf("self-conflict key %x lacks the \\xFF/SC/ prefix", cr.Begin)
		}
		if len(cr.Begin) != len(selfConflictPrefix)+16 {
			t.Fatalf("self-conflict key len %d, want prefix+16 random bytes", len(cr.Begin))
		}
		if !bytes.Equal(cr.End, keyAfterBytes(cr.Begin)) {
			t.Fatal("self-conflict range end != keyAfter(begin) (must be a single-key range)")
		}
	}
	if !bytes.Equal(tx.readConflicts[0].Begin, tx.writeConflicts[0].Begin) {
		t.Fatal("read and write self-conflict keys differ — must be the SAME synthetic key")
	}
}

// TestDummyBarrier_PicksSCKeyOverRealKey pins the fix's payoff (finding #27): for a write-only txn
// (real write conflict, no real read conflict — real ranges don't intersect, so makeSelfConflicting
// added the SC range), the commit_unknown_result dummy barrier's key picker (intersectConflictRanges)
// returns the synthetic \xFF/SC/ key, NOT the real user key — so recovery doesn't perturb a hot user
// key and spuriously conflict other clients (1020). Revert-proof: without makeSelfConflicting the only
// intersection is absent and intersectConflictRanges falls back to the real writes[0].Begin.
func TestDummyBarrier_PicksSCKeyOverRealKey(t *testing.T) {
	t.Parallel()
	realKey := []byte("user/hot/counter")
	tx := newTestTx()
	tx.addWriteConflict(realKey, keyAfterBytes(realKey)) // a real write conflict; no read conflicts
	tx.conflictMu.Lock()
	tx.makeSelfConflictingLocked()
	tx.conflictMu.Unlock()

	key := intersectConflictRanges(tx.writeConflicts, tx.readConflicts)
	if bytes.Equal(key, realKey) {
		t.Fatalf("dummy barrier picked the REAL user key %q — it must pick the synthetic \\xFF/SC/ key", key)
	}
	if !bytes.HasPrefix(key, selfConflictPrefix) {
		t.Fatalf("dummy barrier key %x is not the \\xFF/SC/ self-conflict key", key)
	}
}
