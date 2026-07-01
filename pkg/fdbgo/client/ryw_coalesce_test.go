package client

import (
	"bytes"
	"encoding/binary"
	"testing"
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
