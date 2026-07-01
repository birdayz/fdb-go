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

// TestCoalesceCommit_VersionstampTxnShipsRaw pins codex #28 P2: a SetVersionstampedKey followed by a Set on
// the same TEMPLATE key can't go through the coalesced materialize — ryw.set replaces the entry, so the SVK
// is dropped and the stamped-key write is lost (libfdb_c PUSHES the Set onto the unreadable SVK entry and
// commits BOTH, WriteMap.cpp:139-146). The fix ships such txns' raw op-log (the mutationsHaveVersionstamp
// gate in Commit). This test pins the detection AND demonstrates the drop hazard the gate avoids.
func TestCoalesceCommit_VersionstampTxnShipsRaw(t *testing.T) {
	t.Parallel()
	// SVK and Set target the SAME template key (the zero-placeholder key with its 4-byte LE offset suffix,
	// as when a read version isn't cached yet — codex's exact scenario). ryw.set replaces the entry, so
	// the coalesced materialize would emit only the Set.
	key := append([]byte("tmpl"), 0, 0, 0, 0)
	svk := Mutation{Type: MutSetVersionstampedKey, Key: key, Value: []byte("sv")}
	set := Mutation{Type: MutSetValue, Key: key, Value: []byte("v2")}
	muts := []Mutation{svk, set}

	if !mutationsHaveVersionstamp(muts) {
		t.Fatal("mutationsHaveVersionstamp must flag the SVK so Commit ships the raw op-log")
	}
	// Demonstrate the hazard the gate avoids: materializing the keyed write map DROPS the SVK.
	coalesced := coalesceCommitMutations(muts)
	for _, m := range coalesced {
		if m.Type == MutSetVersionstampedKey {
			t.Fatal("unexpected: materialize kept the SVK — the hazard this gate guards is gone, revisit the gate")
		}
	}
	// Confirmed dropped by materialize → the raw-ship path is load-bearing; it retains BOTH ops.
	if len(muts) != 2 || muts[0].Type != MutSetVersionstampedKey || muts[1].Type != MutSetValue {
		t.Fatalf("raw op-log must retain [SVK, Set]; got %v", muts)
	}
}

// TestCommit_ShipsSelfConflictRange pins codex #28 P1: a write-only NON-tenant commit with no intersecting
// read conflict gets a synthetic \xff/SC/ self-conflict range from maybeMakeSelfConflicting (RFC-090
// idempotency). The shipped write-conflict set must be re-derived AFTER maybeMakeSelfConflicting — the
// size-time snapshot predates it — else the SC range is dropped and a maybe-committed retry can't detect an
// already-applied commit. Pins the SC range ABSENT before and PRESENT after (the Commit re-derive point).
func TestCommit_ShipsSelfConflictRange(t *testing.T) {
	t.Parallel()
	tx := newTestTx()
	tx.tenantId = NoTenantID
	tx.mutations = []Mutation{{Type: MutSetValue, Key: []byte("k"), Value: []byte("v")}}
	tx.writeConflicts = []KeyRange{{Begin: []byte("k"), End: []byte("k\x00")}}

	scIn := func(rs []types.KeyRangeRef) bool {
		for _, kr := range rs {
			if bytes.HasPrefix(kr.Begin, selfConflictPrefix) {
				return true
			}
		}
		return false
	}

	// Mirror Commit's ORDER: the size-time conflict snapshot is taken FIRST (pre-SC), then
	// maybeMakeSelfConflicting adds the \xff/SC/ range, then the SHIPPED conflicts are re-derived (the fix).
	sizeConflicts := coalesceWriteConflicts(tx.writeConflicts)
	tx.maybeMakeSelfConflicting()
	shipConflicts := coalesceWriteConflicts(tx.writeConflicts)

	// Precondition: the size-time snapshot (what the pre-fix code shipped) carries NO SC range.
	scInRanges := func(rs []KeyRange) bool {
		for _, kr := range rs {
			if bytes.HasPrefix(kr.Begin, selfConflictPrefix) {
				return true
			}
		}
		return false
	}
	if scInRanges(sizeConflicts) {
		t.Fatal("precondition: no \\xff/SC/ range in the size-time snapshot")
	}

	// The SHIPPED commit request (built from the re-derived conflicts) must carry the SC range on the wire.
	body, bufp := buildCommitTransactionRequest(tx, transport.UID{First: 1, Second: 2}, tx.mutations, shipConflicts)
	defer marshalBufPool.Put(bufp)
	var req types.CommitTransactionRequest
	if err := req.UnmarshalFDB(body); err != nil {
		t.Fatalf("UnmarshalFDB: %v", err)
	}
	if !scIn(req.Transaction.WriteConflictRanges) {
		t.Fatal("#28 P1: shipped commit request is missing the \\xff/SC/ self-conflict range — Commit must " +
			"re-derive shipConflicts AFTER maybeMakeSelfConflicting, not from the size-time snapshot")
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
