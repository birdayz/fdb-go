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
