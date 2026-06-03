package bench

import (
	"bytes"
	"testing"

	cgofdb "github.com/apple/foundationdb/bindings/go/src/fdb"
	gofdb "github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
)

// getKey BOUNDARY-resolution differential vs libfdb_c — RFC-065.
//
// The existing getKey differentials (RFC-055/056/057/058) resolve selectors WITHIN the keyspace
// ({a,b,c,d}) and CLAMP off-prefix results — masking the keyspace EDGES. This pins the edges.
//
// FOUND + FIXED a real divergence: a BACKWARD selector at/past maxReadKey (e.g. lastLessThan(\xff))
// wrongly returned maxReadKey itself (Go's resolveKeySelectorFromCache short-circuited every
// off-end seek to readThroughEnd, ignoring direction), instead of resolving backward to the
// greatest key < maxReadKey. libfdb_c's it.skip() clamps to the last segment and resolves
// backward. Go now matches (ryw_getkey.go off-end branch is direction-aware).
//
// Determinism: both clients pin the SAME read version, so getKey resolves against an identical
// committed snapshot and go==cgo is a clean assertion (no GRV-timing nondeterminism). A stale
// pin (1007/past_version, if the suite is slow) → retry with a fresh version.

func TestDifferential_GetKeyBoundary(t *testing.T) {
	t.Parallel()
	maxReadKey := []byte{0xff} // user keyspace upper bound (no system-key access)

	type boundaryCase struct {
		name           string
		key            string
		orEqual        bool
		offset         int
		assertBelowMax bool // resolved key must be < maxReadKey (the lastLess* contract the bug broke)
	}
	cases := []boundaryCase{
		// THE BUG: lastLessThan/Equal(maxReadKey) must resolve BACKWARD to the greatest key
		// < \xff, not return \xff itself.
		{"LLT_maxReadKey", "\xff", false, 0, true},
		{"LLE_maxReadKey", "\xff", true, 0, true},
		// lastLessThan(empty) → before the start → allKeysBegin (empty key).
		{"LLT_empty", "", false, 0, false},
		{"LLT_empty_bigneg", "", false, -100, false},
		// firstGreaterOrEqual/Than(maxReadKey) → nothing >= \xff in user space → \xff (readThroughEnd).
		{"FGE_maxReadKey", "\xff", false, 1, false},
		{"FGT_maxReadKey", "\xff", true, 1, false},
		// allKeysEnd (\xff\xff) and just-past-max (\xff\x00) are > maxReadKey → key_outside_legal_range.
		{"FGT_allKeysEnd", "\xff\xff", true, 1, false},
		{"FGE_allKeysEnd", "\xff\xff", false, 1, false},
		{"past_max", "\xff\x00", false, 1, false},
		// Large offsets walking off each end.
		{"large_pos_offset", "\x00", false, 1_000_000, false}, // → readThroughEnd (\xff)
		{"large_neg_offset", "\xff", false, -1_000_000, true}, // walk back far → still < \xff (or empty)
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			const maxAttempts = 12
			for attempt := 0; ; attempt++ {
				if attempt >= maxAttempts {
					t.Fatalf("%s: did not clear transient errors in %d attempts", c.name, maxAttempts)
				}
				v := freshSharedVersion(t)
				goKey, goErr := goGetKeyAtPinned(t, v, c.key, c.orEqual, c.offset)
				cKey, cErr := cgoGetKeyAtPinned(t, v, c.key, c.orEqual, c.offset)
				goCode, cCode := fdbErrorCode(goErr), fdbErrorCode(cErr)
				if (goErr != nil && isFDBRetryable(goErr)) || (cErr != nil && isFDBRetryable(cErr)) {
					continue // stale pin etc. — retry with a fresh version
				}
				if goCode != cCode {
					t.Fatalf("%s: error code differs: go=%d cgo=%d", c.name, goCode, cCode)
				}
				if goCode != 0 {
					return // both errored identically (e.g. 2004) — done
				}
				if !bytes.Equal(goKey, cKey) {
					t.Fatalf("%s: resolved key differs: go=%x cgo=%x", c.name, goKey, cKey)
				}
				if c.assertBelowMax && len(goKey) > 0 && bytes.Compare(goKey, maxReadKey) >= 0 {
					t.Fatalf("%s: lastLess* resolved to %x, NOT < maxReadKey %x (the RFC-065 bug)", c.name, goKey, maxReadKey)
				}
				return
			}
		})
	}
}

func goGetKeyAtPinned(t *testing.T, v int64, key string, orEqual bool, offset int) ([]byte, error) {
	t.Helper()
	tr, err := goClient.CreateTransaction()
	if err != nil {
		t.Fatalf("go create: %v", err)
	}
	defer tr.Cancel()
	tr.SetReadVersion(v)
	k, e := tr.GetKey(gofdb.KeySelector{Key: gofdb.Key(key), OrEqual: orEqual, Offset: offset}).Get()
	return k, e
}

func cgoGetKeyAtPinned(t *testing.T, v int64, key string, orEqual bool, offset int) ([]byte, error) {
	t.Helper()
	tr, err := cgoClient.CreateTransaction()
	if err != nil {
		t.Fatalf("cgo create: %v", err)
	}
	defer tr.Cancel()
	tr.SetReadVersion(v)
	k, e := tr.GetKey(cgofdb.KeySelector{Key: cgofdb.Key(key), OrEqual: orEqual, Offset: offset}).Get()
	return k, e
}
