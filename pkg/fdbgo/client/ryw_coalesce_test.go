package client

import (
	"bytes"
	"encoding/binary"
	"testing"

	"fdb.dev/pkg/fdbgo/transport"
	"fdb.dev/pkg/fdbgo/wire/types"
)

// le8 is an 8-byte little-endian operand (the canonical FDB counter width).
func le8(n uint64) []byte {
	b := make([]byte, 8)
	binary.LittleEndian.PutUint64(b, n)
	return b
}

// TestCoalesceOverAtomics_FoldTable pins the #28 write-map fold against C++ WriteMap::coalesceOver
// (WriteMap.cpp:480-494) + coalesce (:357) / NON_ASSOCIATIVE_MASK (CommitTransaction.h:576-578). Each row
// asserts the resulting stack length AND, for a fold, the combined operand.
func TestCoalesceOverAtomics_FoldTable(t *testing.T) {
	t.Parallel()
	m := func(op MutationType, p []byte) rywMutation { return rywMutation{typ: op, param: p} }

	t.Run("empty stack pushes", func(t *testing.T) {
		t.Parallel()
		got := coalesceOverAtomics(nil, m(MutAddValue, le8(1)))
		if len(got) != 1 {
			t.Fatalf("empty+ADD: len=%d, want 1", len(got))
		}
	})

	t.Run("same-type non-associative equal length folds", func(t *testing.T) {
		t.Parallel()
		// ADD 1 then ADD 1 (both 8-byte) → one ADD 2.
		s := coalesceOverAtomics(nil, m(MutAddValue, le8(1)))
		s = coalesceOverAtomics(s, m(MutAddValue, le8(1)))
		if len(s) != 1 {
			t.Fatalf("ADD+ADD(equal len): len=%d, want 1 (fold)", len(s))
		}
		if !bytes.Equal(s[0].param, le8(2)) {
			t.Fatalf("ADD 1 + ADD 1 = %v, want le8(2)=%v", s[0].param, le8(2))
		}
		if s[0].typ != MutAddValue {
			t.Fatalf("folded op type=%d, want MutAddValue (%d) — fold keeps the atomic type", s[0].typ, MutAddValue)
		}
	})

	t.Run("same-type non-associative DIFFERENT length pushes", func(t *testing.T) {
		t.Parallel()
		// ADD 8-byte then ADD 4-byte → keep both (non-associative, size mismatch).
		s := coalesceOverAtomics(nil, m(MutAddValue, le8(1)))
		s = coalesceOverAtomics(s, m(MutAddValue, []byte{1, 0, 0, 0}))
		if len(s) != 2 {
			t.Fatalf("ADD(8)+ADD(4): len=%d, want 2 (non-associative size mismatch pushes)", len(s))
		}
	})

	t.Run("same-type associative folds regardless of length", func(t *testing.T) {
		t.Parallel()
		// AND is associative (not in NON_ASSOCIATIVE_MASK): fold even on different operand length.
		s := coalesceOverAtomics(nil, m(MutAnd, []byte{0xff, 0xff}))
		s = coalesceOverAtomics(s, m(MutAnd, []byte{0x0f}))
		if len(s) != 1 {
			t.Fatalf("AND+AND(diff len): len=%d, want 1 (associative folds regardless of length)", len(s))
		}
	})

	t.Run("different type pushes", func(t *testing.T) {
		t.Parallel()
		s := coalesceOverAtomics(nil, m(MutAddValue, le8(1)))
		s = coalesceOverAtomics(s, m(MutOr, le8(1)))
		if len(s) != 2 {
			t.Fatalf("ADD+OR: len=%d, want 2 (different atomic types keep both)", len(s))
		}
	})

	t.Run("CompareAndClear pushes", func(t *testing.T) {
		t.Parallel()
		s := coalesceOverAtomics(nil, m(MutAddValue, le8(1)))
		s = coalesceOverAtomics(s, m(MutCompareAndClear, le8(1)))
		if len(s) != 2 {
			t.Fatalf("ADD+CompareAndClear: len=%d, want 2 (CAC excluded from same-type fold, pushes)", len(s))
		}
	})

	t.Run("versionstamp pushes and never merges into a prior versionstamp", func(t *testing.T) {
		t.Parallel()
		s := coalesceOverAtomics(nil, m(MutSetVersionstampedValue, make([]byte, 14)))
		s = coalesceOverAtomics(s, m(MutSetVersionstampedValue, make([]byte, 14)))
		if len(s) != 2 {
			t.Fatalf("VSV+VSV: len=%d, want 2 (versionstamps kept intact, never folded)", len(s))
		}
	})

	// Torvalds gap: the differential + rows above pinned the combined OPERAND only for ADD. Pin it for
	// every fold op with HAND-computed expected values (independent of applyAtomic, which the fold reuses).
	t.Run("every fold op combines operands correctly", func(t *testing.T) {
		t.Parallel()
		rows := []struct {
			name string
			op   MutationType
			a, b []byte
			want []byte
		}{
			{"AND", MutAnd, []byte{0xf0, 0xff}, []byte{0x3c, 0x0f}, []byte{0x30, 0x0f}}, // bitwise AND
			{"OR", MutOr, []byte{0x01, 0x00}, []byte{0x02, 0x80}, []byte{0x03, 0x80}},   // bitwise OR
			{"XOR", MutXor, []byte{0x0f, 0xff}, []byte{0x03, 0x0f}, []byte{0x0c, 0xf0}}, // bitwise XOR
			{"MAX", MutMax, le8(5), le8(3), le8(5)},                                     // LE unsigned max
			{"MIN", MutMin, le8(5), le8(3), le8(3)},                                     // LE unsigned min
			{"ByteMin", MutByteMin, []byte("b"), []byte("a"), []byte("a")},              // lexicographic min
			{"ByteMax", MutByteMax, []byte("a"), []byte("b"), []byte("b")},              // lexicographic max
			{"AppendIfFits", MutAppendIfFits, []byte("a"), []byte("b"), []byte("ab")},   // concat
		}
		for _, r := range rows {
			s := coalesceOverAtomics(nil, m(r.op, r.a))
			s = coalesceOverAtomics(s, m(r.op, r.b))
			if len(s) != 1 {
				t.Errorf("%s: stack len=%d, want 1 (fold)", r.name, len(s))
				continue
			}
			if s[0].typ != r.op {
				t.Errorf("%s: folded type=%d, want %d (fold keeps the op type)", r.name, s[0].typ, r.op)
			}
			if !bytes.Equal(s[0].param, r.want) {
				t.Errorf("%s: folded operand = %x, want %x", r.name, s[0].param, r.want)
			}
		}
	})
}

// TestCoalesceCommitVectors_Decision pins the #28 ship-DECISION that Commit delegates to
// coalesceCommitVectors — the single source of "coalesce vs raw" (Torvalds: the decision must be pinned
// outside Commit's inline flow, not reconstructed in a test). Revert-provable per row.
func TestCoalesceCommitVectors_Decision(t *testing.T) {
	t.Parallel()
	conflicts := []KeyRange{{Begin: []byte("c"), End: []byte("c\x00")}}

	t.Run("normal RYW txn coalesces", func(t *testing.T) {
		t.Parallel()
		// 3× ADD 1 on one unread key must fold to ONE mutation. Revert (force coalesced=false) → len 3.
		muts := []Mutation{
			{Type: MutAddValue, Key: []byte("c"), Value: le8(1)},
			{Type: MutAddValue, Key: []byte("c"), Value: le8(1)},
			{Type: MutAddValue, Key: []byte("c"), Value: le8(1)},
		}
		ship, _, coalesced := coalesceCommitVectors(muts, conflicts, false)
		if !coalesced || len(ship) != 1 {
			t.Fatalf("normal txn: coalesced=%v len(ship)=%d, want true/1", coalesced, len(ship))
		}
	})

	t.Run("versionstamp txn ships raw with the SVK preserved", func(t *testing.T) {
		t.Parallel()
		// SVK then Set on the SAME template key: the keyed materialize would DROP the SVK (ryw.set replaces
		// the entry), so the txn must ship raw. Revert (drop the mutationsHaveVersionstamp check) → coalesced
		// materialize omits the SVK and this assertion fires (codex #28 P2).
		key := append([]byte("tmpl"), 0, 0, 0, 0)
		muts := []Mutation{
			{Type: MutSetVersionstampedKey, Key: key, Value: []byte("sv")},
			{Type: MutSetValue, Key: key, Value: []byte("v2")},
		}
		ship, _, coalesced := coalesceCommitVectors(muts, conflicts, false)
		if coalesced {
			t.Fatal("versionstamp txn must NOT coalesce (the keyed materialize drops the SVK)")
		}
		hasSVK := false
		for _, m := range ship {
			if m.Type == MutSetVersionstampedKey {
				hasSVK = true
			}
		}
		if !hasSVK {
			t.Fatal("#28 P2: versionstamp txn's shipMuts dropped the SVK — must ship the raw op-log")
		}
	})

	t.Run("rywDisabled ships raw", func(t *testing.T) {
		t.Parallel()
		muts := []Mutation{{Type: MutSetValue, Key: []byte("k"), Value: []byte("v")}}
		if _, _, coalesced := coalesceCommitVectors(muts, conflicts, true); coalesced {
			t.Fatal("rywDisabled must ship the raw op-log (no write map)")
		}
	})
}

// TestFinalizeShipConflicts pins the #28 P1/P2b ship-conflict DECISION that Commit delegates to
// finalizeShipConflicts: the self-conflict range is appended to the SIZED snapshot when added, and the
// helper is PURE (works from its snapshot arg) so a racing Set's live conflict can never leak in.
func TestFinalizeShipConflicts(t *testing.T) {
	t.Parallel()
	sized := []KeyRange{{Begin: []byte("a"), End: []byte("a\x00")}}
	scBegin := append(append([]byte(nil), selfConflictPrefix...), 1, 2, 3)
	sc := KeyRange{Begin: scBegin, End: keyAfterBytes(scBegin)}
	hasSC := func(rs []KeyRange) bool {
		for _, kr := range rs {
			if bytes.HasPrefix(kr.Begin, selfConflictPrefix) {
				return true
			}
		}
		return false
	}

	// P1: when maybeMakeSelfConflicting added an SC range, it MUST ship (revert: drop the scAdded append → no SC).
	out := finalizeShipConflicts(sized, sc, true, true)
	if !hasSC(out) {
		t.Fatal("#28 P1: finalizeShipConflicts must include the self-conflict range when scAdded")
	}
	if !hasSC(finalizeShipConflicts(sized, sc, true, false)) { // rywDisabled path too
		t.Fatal("#28 P1: SC must ship on the raw (non-coalesced) path as well")
	}
	// No SC when none was added.
	if hasSC(finalizeShipConflicts(sized, KeyRange{}, false, true)) {
		t.Fatal("no SC range must appear when scAdded=false")
	}
	// P2b: the helper works ONLY from its snapshot arg — a range never passed in (a racing Set's) cannot
	// leak into the shipped set. The sized range is present; nothing else non-SC is.
	for _, kr := range out {
		if !bytes.HasPrefix(kr.Begin, selfConflictPrefix) && !bytes.Equal(kr.Begin, []byte("a")) {
			t.Fatalf("#28 P2b: unexpected range %q shipped — finalizeShipConflicts must use only the sized snapshot", kr.Begin)
		}
	}
}

// TestCommit_ShipsSelfConflictRange_Wire pins that the #28 P1 fix reaches the WIRE: the conflicts produced
// by Commit's helper flow (maybeMakeSelfConflicting → finalizeShipConflicts) carry the \xff/SC/ range in
// the marshaled CommitTransactionRequest for a write-only non-tenant commit.
func TestCommit_ShipsSelfConflictRange_Wire(t *testing.T) {
	t.Parallel()
	tx := newTestTx()
	tx.tenantId = NoTenantID
	tx.mutations = []Mutation{{Type: MutSetValue, Key: []byte("k"), Value: []byte("v")}}
	tx.writeConflicts = []KeyRange{{Begin: []byte("k"), End: []byte("k\x00")}}

	// Commit's flow: freeze the PRE-SC write snapshot (mirroring Commit's writeConflictsSnap), then
	// maybeMakeSelfConflicting (decides on the snapshot, returns the SC range), then finalizeShip from the
	// SAME pre-SC snapshot. Feeding the pre-SC snapshot (NOT live tx.writeConflicts, which maybeMakeSelf-
	// Conflicting mutates to contain the SC) makes SC enter ONLY via the `sc` param — so this test reds if
	// the P1 sc-append is reverted (Torvalds round-3).
	_, _, coalesced := coalesceCommitVectors(tx.mutations, tx.writeConflicts, tx.rywDisabled)
	writeSnap := append([]KeyRange(nil), tx.writeConflicts...)
	sc, scAdded := tx.maybeMakeSelfConflicting(writeSnap)
	shipConflicts := finalizeShipConflicts(writeSnap, sc, scAdded, coalesced)

	body, bufp := buildCommitTransactionRequest(tx, transport.UID{First: 1, Second: 2}, tx.mutations, shipConflicts)
	defer marshalBufPool.Put(bufp)
	var req types.CommitTransactionRequest
	if err := req.UnmarshalFDB(body); err != nil {
		t.Fatalf("UnmarshalFDB: %v", err)
	}
	scOnWire := false
	for _, kr := range req.Transaction.WriteConflictRanges {
		if bytes.HasPrefix(kr.Begin, selfConflictPrefix) {
			scOnWire = true
		}
	}
	if !scOnWire {
		t.Fatal("#28 P1: shipped commit request is missing the \\xff/SC/ self-conflict range")
	}
}

// TestMaybeMakeSelfConflicting_DecidesOnFrozenSnapshot pins codex #28 round-3: the SC injection decision
// must use the FROZEN write snapshot that ships, NOT live tx.writeConflicts. A Set racing this Commit after
// the snapshot (excluded from the shipped writes) must not flip the decision — else the frozen writes ship
// with no intersecting range and no \xff/SC/ key, breaking the commit_unknown_result barrier. Revert-proof:
// deciding on live tx.writeConflicts (which here holds the racing write intersecting the read) suppresses SC.
func TestMaybeMakeSelfConflicting_DecidesOnFrozenSnapshot(t *testing.T) {
	t.Parallel()
	tx := newTestTx()
	tx.tenantId = NoTenantID
	// A read on "b"; the frozen write snapshot is just [a] (does NOT intersect the read → SC should inject).
	tx.readConflicts = []KeyRange{{Begin: []byte("b"), End: []byte("b\x00")}}
	writeSnap := []KeyRange{{Begin: []byte("a"), End: []byte("a\x00")}}
	// A Set on "b" races Commit AFTER the snapshot → appended to LIVE tx.writeConflicts (not writeSnap). It
	// intersects the read range "b", so a LIVE-based decision would wrongly skip SC.
	tx.writeConflicts = []KeyRange{
		{Begin: []byte("a"), End: []byte("a\x00")},
		{Begin: []byte("b"), End: []byte("b\x00")},
	}
	if _, scAdded := tx.maybeMakeSelfConflicting(writeSnap); !scAdded {
		t.Fatal("#28 round-3: SC decision must use the frozen write snapshot [a] (which ships), not live " +
			"tx.writeConflicts — a racing Set on read key \"b\" wrongly suppressed the self-conflict range")
	}
}

// TestRywAtomic_ChainFolds proves the #28 collapse end-to-end through rywCache.atomic(): 150k `ADD 1` on a
// key that was never Set/read/cleared (so no eager value-fold at site B/C) accumulate into ONE folded
// atomic in the pending chain, not 150k. Revert-proof: swapping coalesceOverAtomics back to a plain append
// makes the chain 150000 long.
func TestRywAtomic_ChainFolds(t *testing.T) {
	t.Parallel()
	c := &rywCache{}
	key := []byte("counter")
	const n = 150_000
	for i := 0; i < n; i++ {
		c.atomic(MutAddValue, key, le8(1))
	}
	c.mu.Lock()
	entry := c.writes[string(key)]
	c.mu.Unlock()
	if !entry.hasAtomics {
		t.Fatal("expected a pending atomic chain on an unread key")
	}
	if len(entry.atomics) != 1 {
		t.Fatalf("chain length = %d, want 1 (150k ADD 1 must fold to one ADD 150000)", len(entry.atomics))
	}
	if !bytes.Equal(entry.atomics[0].param, le8(n)) {
		t.Fatalf("folded operand = %v, want le8(%d)", entry.atomics[0].param, n)
	}
	// Read-transparency: resolving the folded chain onto base 5 yields 5+150000, same as unfolded.
	got, cleared, unresolved := resolveAtomics(le8(5), entry.atomics)
	if cleared || unresolved {
		t.Fatalf("resolve flags: cleared=%v unresolved=%v, want both false", cleared, unresolved)
	}
	if !bytes.Equal(got, le8(5+n)) {
		t.Fatalf("resolved value = %v, want le8(%d)", got, 5+n)
	}
}
