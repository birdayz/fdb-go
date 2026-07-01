package client

import (
	"errors"
	"testing"

	"fdb.dev/pkg/fdbgo/wire"
)

// Client-side validation for AddReadConflictRange / AddWriteConflictRange — RFC-126 Divergence B.
// C++ rejects a conflict range whose endpoint exceeds the max key (ReadYourWrites.actor.cpp:1954 read /
// :2466 write) with key_outside_legal_range (2004); Go used to check only inverted (begin>end). These
// pin the SYNCHRONOUS validation edges (no DB): the inverted-wins ordering, the read-only
// metadataVersionKey exception, and the read/write maxKey ASYMMETRY (read uses maxReadKey + the
// exception; write uses maxWriteKey + no exception). Deterministic, no cgo.

func crCode(t *testing.T, err error) int {
	t.Helper()
	if err == nil {
		return 0
	}
	var fe *wire.FDBError
	if errors.As(err, &fe) {
		return fe.Code
	}
	t.Fatalf("non-FDBError: %v", err)
	return -1
}

// TestAddConflictRange_ClampsOversizedKeys pins that AddReadConflictRange / AddWriteConflictRange
// CLAMP an oversized endpoint to getMaxClearKeySize+1 bytes and DROP the range when the clamp
// collapses it to empty — matching C++ RYW add{Read,Write}ConflictRange
// (ReadYourWrites.actor.cpp:1958-1976 read / :2474-2492 write). getMaxReadKeySize == getMaxKeySize ==
// getMaxClearKeySize (hasRawAccess is always true for conflict ranges), so the non-system limit is
// keySizeLimit+tenantPrefixSize (10008) → clamp to 10009. A non-system key >10 KB is < \xff, so it
// PASSES the maxReadKey/maxWriteKey legal-range check and reaches the clamp. Revert-proof: drop the
// clamp and the recorded conflict range keeps the full 20000-byte key (or, in the empty case, records
// a bogus non-empty range libfdb_c would have dropped). No DB — the clamp is synchronous.
func TestAddConflictRange_ClampsOversizedKeys(t *testing.T) {
	t.Parallel()
	const nonSysMax = keySizeLimit + tenantPrefixSize // 10008; hasRawAccess=true for conflict ranges
	bigA := func(n int) []byte {
		b := make([]byte, n)
		for i := range b {
			b[i] = 'a'
		}
		return b
	}

	t.Run("read_clamps_both_endpoints", func(t *testing.T) {
		t.Parallel()
		tx := newTestTx()
		tx.rywDisabled = true // land directly in readConflicts, bypassing the write-map filter
		begin := bigA(20000)
		end := bigA(20000)
		end[0] = 'b' // begin < end, both non-system, both oversized
		if err := tx.AddReadConflictRange(begin, end); err != nil {
			t.Fatalf("AddReadConflictRange: %v", err)
		}
		if len(tx.readConflicts) != 1 {
			t.Fatalf("want 1 recorded read conflict, got %d", len(tx.readConflicts))
		}
		r := tx.readConflicts[0]
		if len(r.Begin) != nonSysMax+1 || len(r.End) != nonSysMax+1 {
			t.Fatalf("endpoints not clamped to %d: begin=%d end=%d", nonSysMax+1, len(r.Begin), len(r.End))
		}
	})

	t.Run("read_drops_when_clamp_empties_range", func(t *testing.T) {
		t.Parallel()
		tx := newTestTx()
		tx.rywDisabled = true
		begin := bigA(20000)
		end := bigA(20000)
		end[15000] = 'z' // begin < end, but they AGREE on the first 10009 bytes → clamp to identical prefix → empty
		if err := tx.AddReadConflictRange(begin, end); err != nil {
			t.Fatalf("AddReadConflictRange: %v", err)
		}
		if len(tx.readConflicts) != 0 {
			t.Fatalf("an oversized range that clamps to empty must be dropped, got %d conflicts", len(tx.readConflicts))
		}
	})

	t.Run("write_clamps_both_endpoints", func(t *testing.T) {
		t.Parallel()
		tx := newTestTx()
		begin := bigA(20000)
		end := bigA(20000)
		end[0] = 'b'
		if err := tx.AddWriteConflictRange(begin, end); err != nil {
			t.Fatalf("AddWriteConflictRange: %v", err)
		}
		if len(tx.writeConflicts) != 1 {
			t.Fatalf("want 1 recorded write conflict, got %d", len(tx.writeConflicts))
		}
		r := tx.writeConflicts[0]
		if len(r.Begin) != nonSysMax+1 || len(r.End) != nonSysMax+1 {
			t.Fatalf("endpoints not clamped to %d: begin=%d end=%d", nonSysMax+1, len(r.Begin), len(r.End))
		}
	})

	t.Run("write_drops_when_clamp_empties_range", func(t *testing.T) {
		t.Parallel()
		tx := newTestTx()
		begin := bigA(20000)
		end := bigA(20000)
		end[15000] = 'z'
		if err := tx.AddWriteConflictRange(begin, end); err != nil {
			t.Fatalf("AddWriteConflictRange: %v", err)
		}
		if len(tx.writeConflicts) != 0 {
			t.Fatalf("an oversized range that clamps to empty must be dropped, got %d conflicts", len(tx.writeConflicts))
		}
	})
}

func TestAddReadConflictRange_Validation(t *testing.T) {
	t.Parallel()
	mvk := string(metadataVersionKeyBytes)
	mvkEnd := string(metadataVersionKeyEndBytes)
	cases := []struct {
		name              string
		begin, end        string
		readSys, writeSys bool
		want              int
	}{
		{"in_range", "a", "m", false, false, 0},
		{"end_past_maxReadKey", "a", "\xff\xff\xff", false, false, 2004},
		// Inverted wins over maxKey: begin>end AND both past maxKey → inverted_range (2005), checked first.
		{"inverted_beats_maxKey", "\xff\xff\xff\xff", "\xff\xff\xff", false, false, 2005},
		// metadataVersionKey EXACT range is exempt (read path only).
		{"mvk_exact_exception", mvk, mvkEnd, false, false, 0},
		// metadataVersionKey non-exact end is NOT exempt → 2004 (still past maxReadKey).
		{"mvk_non_exact", mvk, "\xff/metadataVersionZZ", false, false, 2004},
		// READ_SYSTEM_KEYS raises maxReadKey to \xff\xff → an endpoint in (\xff, \xff\xff] is in range.
		{"readsys_allows_system_endpoint", "a", "\xff\x05", true, false, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			tx := newTestTx()
			tx.readSystemKeys = c.readSys
			tx.writeSystemKeys = c.writeSys
			got := crCode(t, tx.AddReadConflictRange([]byte(c.begin), []byte(c.end)))
			if got != c.want {
				t.Errorf("AddReadConflictRange(%q,%q): code=%d, want %d", c.begin, c.end, got, c.want)
			}
		})
	}
}

func TestAddWriteConflictRange_Validation(t *testing.T) {
	t.Parallel()
	mvk := string(metadataVersionKeyBytes)
	mvkEnd := string(metadataVersionKeyEndBytes)
	cases := []struct {
		name              string
		begin, end        string
		readSys, writeSys bool
		want              int
	}{
		{"in_range", "a", "m", false, false, 0},
		{"end_past_maxWriteKey", "a", "\xff\xff\xff", false, false, 2004},
		{"inverted_beats_maxKey", "\xff\xff\xff\xff", "\xff\xff\xff", false, false, 2005},
		// NO metadataVersionKey exception on the write path (asymmetric with read) → 2004.
		{"mvk_exact_no_exception", mvk, mvkEnd, false, false, 2004},
		// READ_SYSTEM_KEYS does NOT raise maxWriteKey (gated on writeSystemKeys) → still 2004 (the asymmetry).
		{"readsys_does_not_help_write", "a", "\xff\x05", true, false, 2004},
		// ACCESS_SYSTEM_KEYS (writeSystemKeys) raises maxWriteKey to \xff\xff → in range.
		{"writesys_allows_system_endpoint", "a", "\xff\x05", false, true, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			tx := newTestTx()
			tx.readSystemKeys = c.readSys
			tx.writeSystemKeys = c.writeSys
			got := crCode(t, tx.AddWriteConflictRange([]byte(c.begin), []byte(c.end)))
			if got != c.want {
				t.Errorf("AddWriteConflictRange(%q,%q): code=%d, want %d", c.begin, c.end, got, c.want)
			}
		})
	}
}
