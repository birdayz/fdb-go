package client

import (
	"bytes"
	"testing"
)

// TestRangeConflictExtent pins RFC-121 D1: the GetRange read-conflict clamp reduces, for a plain
// [begin,end) (both selectors firstGreaterOrEqual, offset +1), to the libfdb_c table
// (ReadYourWrites.actor.cpp:245-319 / NativeAPI.actor.cpp:4558-4587):
//   - forward, more, non-empty → [begin, keyAfter(lastKey))
//   - reverse, more, non-empty → [firstKey (=kvs[last]), end)
//   - !more or empty           → [begin, end)   (full extent observed — phantom protection)
//
// Revert-proof: drop the clamp (always return begin,end) and the more=true rows go red.
func TestRangeConflictExtent(t *testing.T) {
	t.Parallel()
	begin, end := []byte("k00"), []byte("kzz")
	fwd := []KeyValue{{Key: []byte("k00")}, {Key: []byte("k09")}} // ascending → last = highest
	rev := []KeyValue{{Key: []byte("k19")}, {Key: []byte("k10")}} // descending → last = lowest

	cases := []struct {
		name             string
		kvs              []KeyValue
		more, reverse    bool
		wantBeg, wantEnd []byte
	}{
		{"forward more clamps end", fwd, true, false, begin, keyAfterBytes([]byte("k09"))},
		{"forward drained keeps full", fwd, false, false, begin, end},
		{"forward empty keeps full", nil, false, false, begin, end},
		{"forward more+empty keeps full (degenerate)", nil, true, false, begin, end},
		{"reverse more clamps begin", rev, true, true, []byte("k10"), end},
		{"reverse drained keeps full", rev, false, true, begin, end},
		{"reverse empty keeps full", nil, false, true, begin, end},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gotBeg, gotEnd := rangeConflictExtent(begin, end, tc.kvs, tc.more, tc.reverse)
			if !bytes.Equal(gotBeg, tc.wantBeg) || !bytes.Equal(gotEnd, tc.wantEnd) {
				t.Fatalf("rangeConflictExtent = [%q,%q), want [%q,%q)", gotBeg, gotEnd, tc.wantBeg, tc.wantEnd)
			}
		})
	}
}

// TestAddReadConflictForKeyRYW pins RFC-121 D2: a single-key Get records the read-conflict only
// when the key is in an UNMODIFIED range or a DEPENDENT op, and SKIPS it when a local INDEPENDENT
// write (plain Set) or a cleared range already satisfies the read — mirroring C++
// updateConflictMap(ryw, key, it) (ReadYourWrites.actor.cpp:322-332). rywDisabled adds the full
// single-key conflict (no write map), matching native getValue. White-box because, like the GetKey
// analog (getkey_conflict_unit_test.go), the under-conflict it would otherwise hide is not reachable
// via the legal differential API; the conflict-outcome differential covers the end-to-end behavior.
func TestAddReadConflictForKeyRYW(t *testing.T) {
	t.Parallel()
	key := []byte("c")
	full := KeyRange{Begin: key, End: keyAfterBytes(key)}

	assertSingleFull := func(t *testing.T, tx *Transaction) {
		t.Helper()
		if len(tx.readConflicts) != 1 {
			t.Fatalf("want 1 single-key conflict, got %d: %v", len(tx.readConflicts), tx.readConflicts)
		}
		if !bytes.Equal(tx.readConflicts[0].Begin, full.Begin) || !bytes.Equal(tx.readConflicts[0].End, full.End) {
			t.Fatalf("conflict [%q,%q), want [%q,%q)", tx.readConflicts[0].Begin, tx.readConflicts[0].End, full.Begin, full.End)
		}
	}

	// rywDisabled: no write map → full single-key conflict.
	t.Run("rywDisabled full", func(t *testing.T) {
		t.Parallel()
		tx := &Transaction{rywDisabled: true}
		tx.addReadConflictForKeyRYW(key)
		assertSingleFull(t, tx)
	})

	// RYW enabled, key unmodified (read resolved from storage) → full single-key conflict.
	t.Run("unmodified gap conflicts", func(t *testing.T) {
		t.Parallel()
		tx := &Transaction{}
		tx.addReadConflictForKeyRYW(key)
		assertSingleFull(t, tx)
	})

	// RYW enabled, key served by a local INDEPENDENT Set → NO conflict (the D2 bug).
	t.Run("independent write skipped", func(t *testing.T) {
		t.Parallel()
		tx := &Transaction{}
		tx.ryw.set(key, []byte("v"))
		tx.addReadConflictForKeyRYW(key)
		if len(tx.readConflicts) != 0 {
			t.Fatalf("read served by a local Set must add no read-conflict, got %v", tx.readConflicts)
		}
	})

	// RYW enabled, key inside a locally cleared range → NO conflict (known empty, no DB read).
	t.Run("cleared range skipped", func(t *testing.T) {
		t.Parallel()
		tx := &Transaction{}
		tx.ryw.addClearedRange([]byte("a"), []byte("z"))
		tx.addReadConflictForKeyRYW(key)
		if len(tx.readConflicts) != 0 {
			t.Fatalf("read served by a local clear must add no read-conflict, got %v", tx.readConflicts)
		}
	})
}
