package client

import (
	"bytes"
	"context"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/transport"
	"fdb.dev/pkg/fdbgo/wire/types"
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

// TestIntersectRanges pins the shared sorted-merge (intersectRanges) that backs both the
// makeSelfConflicting guard and the dummy-barrier key picker — a 1:1 port of C++ intersects
// (NativeAPI.actor.cpp:6211-6228): sort both vectors by begin, linear-merge, return the FIRST
// overlap's intersection [max(begins), min(ends)). The sort is load-bearing and this is a
// revert-proof for it: drop the two sort.Slice calls (a naive linear merge on unsorted input,
// or the old O(w*r) scan replaced by a plain merge) and the first subtest goes red — the walk
// races lhs past both ranges while rhs stays pinned and misses the overlap entirely.
func TestIntersectRanges(t *testing.T) {
	t.Parallel()

	t.Run("finds_overlap_regardless_of_input_order", func(t *testing.T) {
		t.Parallel()
		lhs := []KeyRange{krFrom("m", "n"), krFrom("a", "b")}   // unsorted by begin
		rhs := []KeyRange{krFrom("y", "z"), krFrom("a5", "a7")} // a5..a7 ⊂ a..b
		kr, ok := intersectRanges(lhs, rhs)
		if !ok {
			t.Fatal("must find the a..b ∩ a5..a7 overlap even though inputs are unsorted (the sort is load-bearing)")
		}
		if !bytes.Equal(kr.Begin, []byte("a5")) || !bytes.Equal(kr.End, []byte("a7")) {
			t.Fatalf("intersection = [%q,%q), want [a5,a7) = [max(begins), min(ends))", kr.Begin, kr.End)
		}
	})

	t.Run("returns_first_overlap_in_sorted_order", func(t *testing.T) {
		t.Parallel()
		// sorted lhs: [a,d),[p,q); sorted rhs: [c,e),[p0,p9). First merge overlap: [a,d)∩[c,e)=[c,d).
		lhs := []KeyRange{krFrom("p", "q"), krFrom("a", "d")}
		rhs := []KeyRange{krFrom("c", "e"), krFrom("p0", "p9")}
		kr, ok := intersectRanges(lhs, rhs)
		if !ok {
			t.Fatal("expected an overlap")
		}
		if !bytes.Equal(kr.Begin, []byte("c")) || !bytes.Equal(kr.End, []byte("d")) {
			t.Fatalf("first sorted overlap = [%q,%q), want [c,d)", kr.Begin, kr.End)
		}
	})

	t.Run("disjoint", func(t *testing.T) {
		t.Parallel()
		if _, ok := intersectRanges([]KeyRange{krFrom("a", "b")}, []KeyRange{krFrom("c", "d")}); ok {
			t.Fatal("disjoint ranges must not intersect")
		}
	})

	t.Run("touching_half_open_not_overlapping", func(t *testing.T) {
		t.Parallel()
		// [a,b) and [b,c) share only the excluded endpoint b → no overlap (half-open).
		if _, ok := intersectRanges([]KeyRange{krFrom("a", "b")}, []KeyRange{krFrom("b", "c")}); ok {
			t.Fatal("touching half-open ranges must not intersect")
		}
	})

	t.Run("empty_either_side", func(t *testing.T) {
		t.Parallel()
		if _, ok := intersectRanges(nil, []KeyRange{krFrom("a", "b")}); ok {
			t.Fatal("empty lhs → no intersection (C++ `if (lhs.size() && rhs.size())`)")
		}
		if _, ok := intersectRanges([]KeyRange{krFrom("a", "b")}, nil); ok {
			t.Fatal("empty rhs → no intersection")
		}
	})
}

// marshaledConflictRanges builds the commit request tx WOULD ship and returns its read/write conflict
// vectors, deep-copied out of the pooled buffer so the caller can read them after the buffer is returned.
func marshaledConflictRanges(t *testing.T, tx *Transaction) (reads, writes []types.KeyRangeRef) {
	t.Helper()
	body, poolBuf := buildCommitTransactionRequest(tx, transport.UID{}, tx.mutations)
	defer marshalBufPool.Put(poolBuf)
	var req types.CommitTransactionRequest
	if err := req.UnmarshalFDB(body); err != nil {
		t.Fatalf("UnmarshalFDB: %v", err)
	}
	cp := func(rs []types.KeyRangeRef) []types.KeyRangeRef {
		out := make([]types.KeyRangeRef, len(rs))
		for i, r := range rs {
			out[i] = types.KeyRangeRef{Begin: append([]byte(nil), r.Begin...), End: append([]byte(nil), r.End...)}
		}
		return out
	}
	return cp(req.Transaction.ReadConflictRanges), cp(req.Transaction.WriteConflictRanges)
}

func anySelfConflictRange(rs []types.KeyRangeRef) bool {
	for _, r := range rs {
		if bytes.HasPrefix(r.Begin, selfConflictPrefix) {
			return true
		}
	}
	return false
}

func anySelfConflictKeyRange(rs []KeyRange) bool {
	for _, r := range rs {
		if bytes.HasPrefix(r.Begin, selfConflictPrefix) {
			return true
		}
	}
	return false
}

// TestCommit_CallsMaybeMakeSelfConflicting closes the LAST revert-hole (Torvalds): the helper-level and
// buildCommit-level tests all invoke maybeMakeSelfConflicting DIRECTLY, so deleting its single production
// call site (Commit) leaves them green while SC injection silently vanishes in prod. This drives the REAL
// Commit() end-to-end and asserts the injection actually happened. Trick: Commit runs maybeMakeSelfConflicting
// (transaction.go:1730) BEFORE ensureReadVersion (the commit-path GRV) and BEFORE tx.commit()/postCommitReset.
// A commit on an already-cancelled ctx fails at that GRV — AFTER the SC range is injected into the conflict
// sets, but BEFORE the success path resets them — so the injected \xFF/SC/ range persists in tx state for
// inspection. Revert-proof: delete the maybeMakeSelfConflicting() call in Commit → no SC range → red.
// No false green: if the GRV somehow succeeds the commit resets the conflicts and the SC assertion fails loudly.
func TestCommit_CallsMaybeMakeSelfConflicting(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)
	defer db.Close()

	tx := db.CreateTransaction()               // tenantId == NoTenantID
	tx.Set([]byte(t.Name()+"_k"), []byte("v")) // a write → not read-only; adds a write conflict, no read conflict

	cctx, ccancel := context.WithCancel(ctx)
	ccancel() // cancel so Commit fails at the commit-path GRV, after injection, before reset
	if err := tx.Commit(cctx); err == nil {
		t.Fatal("Commit on a cancelled ctx must fail at the commit-path GRV (before tx.commit); got nil")
	}

	tx.conflictMu.Lock()
	reads := append([]KeyRange(nil), tx.readConflicts...)
	writes := append([]KeyRange(nil), tx.writeConflicts...)
	tx.conflictMu.Unlock()
	if !anySelfConflictKeyRange(reads) {
		t.Fatalf("Commit() did not inject the \\xFF/SC/ range into readConflicts — the maybeMakeSelfConflicting call site is unwired; got %d ranges", len(reads))
	}
	if !anySelfConflictKeyRange(writes) {
		t.Fatalf("Commit() did not inject the \\xFF/SC/ range into writeConflicts; got %d ranges", len(writes))
	}
}

// TestCommit_NonTenantWriteOnlyInjectsSelfConflictToWire pins the WIRING and payoff of finding #27:
// maybeMakeSelfConflicting (the block Commit runs after the read-only fast path + size check) must
// leave a \xFF/SC/ range in BOTH conflict vectors of the ACTUAL marshaled CommitTransactionRequest
// for a non-tenant write-only commit whose real ranges don't intersect (non-tenant applies no prefix,
// so the raw \xFF/SC/ key is visible on the wire). Revert-proof: remove the makeSelfConflictingLocked
// call and this goes red — the earlier helper-only tests would NOT (Torvalds).
func TestCommit_NonTenantWriteOnlyInjectsSelfConflictToWire(t *testing.T) {
	t.Parallel()
	tx := newTestTx()
	tx.tenantId = NoTenantID
	tx.addWriteConflict([]byte("wk"), keyAfterBytes([]byte("wk"))) // write-only: one write conflict, no reads
	tx.maybeMakeSelfConflicting()
	reads, writes := marshaledConflictRanges(t, tx)
	if !anySelfConflictRange(reads) || !anySelfConflictRange(writes) {
		t.Fatalf("non-tenant write-only commit must ship the \\xFF/SC/ range in BOTH conflict vectors; reads=%v writes=%v",
			anySelfConflictRange(reads), anySelfConflictRange(writes))
	}
}

// TestCommit_TenantSkipsSelfConflict pins the tenant-scoping GATE: a tenant commit must NOT inject a
// \xFF/SC/ range (threading a raw system key through tenant-prefixed conflict ranges is a documented
// follow-up). The assertion is prefix-INDEPENDENT and counts ranges: buildCommit tenant-prefixes the
// SC key (only metadataVersion is exempt), so a \xFF/SC/ prefix check would be masked — instead, a
// gated write-only tenant commit ships EXACTLY the one (prefixed) write range and ZERO read ranges.
// Revert-proof: drop the `tx.tenantId != NoTenantID` early return and the injected SC range adds a
// read range (and a second write range) → this goes red.
func TestCommit_TenantSkipsSelfConflict(t *testing.T) {
	t.Parallel()
	tx := newTestTx()
	tx.tenantId = 42
	tx.addWriteConflict([]byte("wk"), keyAfterBytes([]byte("wk"))) // write-only: one write conflict, no reads
	tx.maybeMakeSelfConflicting()
	reads, writes := marshaledConflictRanges(t, tx)
	if len(reads) != 0 {
		t.Fatalf("tenant write-only commit must ship 0 read conflict ranges (gate must skip SC injection); got %d", len(reads))
	}
	if len(writes) != 1 {
		t.Fatalf("tenant write-only commit must ship exactly 1 write conflict range (the real one, no SC); got %d", len(writes))
	}
}

// TestCommit_IntersectingConflictsSkipSelfConflict pins the C++ guard `!intersects(...)`
// (NativeAPI.actor.cpp:6859): when the real read/write conflict ranges ALREADY intersect, no synthetic
// range is added (the existing overlap is a fine barrier key). Non-tenant, so a leaked SC range would be
// visible with its raw \xFF/SC/ prefix. Revert-proof: drop the conflictRangesIntersect guard and an SC
// range is added on top of the real overlap → this goes red.
func TestCommit_IntersectingConflictsSkipSelfConflict(t *testing.T) {
	t.Parallel()
	tx := newTestTx()
	tx.tenantId = NoTenantID
	tx.addWriteConflict([]byte("a"), []byte("z"))
	tx.addReadConflict([]byte("m"), []byte("n")) // [m,n) ⊂ [a,z): the real ranges already intersect
	tx.maybeMakeSelfConflicting()
	reads, writes := marshaledConflictRanges(t, tx)
	if anySelfConflictRange(reads) || anySelfConflictRange(writes) {
		t.Fatal("already-intersecting real conflict ranges must NOT get a \\xFF/SC/ range")
	}
}
