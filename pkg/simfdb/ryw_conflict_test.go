package simfdb

import (
	"testing"

	fdb "fdb.dev/pkg/fdbgo/fdb"
)

// concurrentWrite commits a write to key in its own transaction — the abort trigger for every
// case below.
func concurrentWrite(t *testing.T, db *SimDB, key string) {
	t.Helper()
	if _, err := db.Transact(func(w fdb.WritableTransaction) (any, error) {
		w.Set(k(key), []byte("concurrent"))
		return nil, nil
	}); err != nil {
		t.Fatalf("concurrent write to %q: %v", key, err)
	}
}

// commitAfterConcurrentWrite runs body on a fresh transaction, then lands a concurrent write on
// raceKey, then commits, returning the commit's error code (0 = success). The read version is
// pinned by body's first read, so the concurrent write is strictly newer and any read conflict
// covering raceKey must abort.
func commitAfterConcurrentWrite(t *testing.T, db *SimDB, raceKey string, body func(tx *simTxn)) int {
	t.Helper()
	tx := db.newTxn()
	body(tx)
	concurrentWrite(t, db, raceKey)
	// A write of its own guarantees the commit is not the client-side read-only fast path,
	// which never consults conflicts at all.
	tx.Set(k("zzz-unrelated"), []byte("x"))
	err := tx.Commit().Get()
	if err == nil {
		return 0
	}
	return errCode(t, err)
}

// TestReadConflictsAreFilteredThroughTheWriteMap is the port pin for
// conflictForKeyLocked/conflictRangesLocked (client/ryw.go, C++ updateConflictMap).
//
// A read the transaction's own buffer answered did not read the database, so it must take no
// read conflict. Without the filter every self-read conflicts and SimFDB certifies spurious
// 1020s — the systematic over-conflict direction.
//
// The dependent cases are the other half and are the reason the subtraction has to classify
// rather than just drop written keys: a standalone atomic DID read the stored base.
func TestReadConflictsAreFilteredThroughTheWriteMap(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		raceKey  string
		body     func(tx *simTxn)
		wantCode int
	}{
		{
			name:    "Set then Get takes no conflict",
			raceKey: "m",
			body: func(tx *simTxn) {
				tx.Set(k("m"), []byte("mine"))
				tx.Get(k("m")).MustGet()
			},
			wantCode: 0,
		},
		{
			name:    "Get with no local write conflicts",
			raceKey: "m",
			body: func(tx *simTxn) {
				tx.Get(k("m")).MustGet()
			},
			wantCode: 1020,
		},
		{
			name:    "Clear then Get takes no conflict",
			raceKey: "m",
			body: func(tx *simTxn) {
				tx.Clear(k("m"))
				tx.Get(k("m")).MustGet()
			},
			wantCode: 0,
		},
		{
			name:    "ClearRange then GetRange over the same range takes no conflict",
			raceKey: "d",
			body: func(tx *simTxn) {
				tx.ClearRange(fdb.KeyRange{Begin: k("a"), End: k("z")})
				tx.GetRange(fdb.KeyRange{Begin: k("a"), End: k("z")}, fdb.RangeOptions{}).GetSliceOrPanic()
			},
			wantCode: 0,
		},
		{
			name:    "a GetRange gap outside the cleared range still conflicts",
			raceKey: "zz",
			body: func(tx *simTxn) {
				tx.ClearRange(fdb.KeyRange{Begin: k("a"), End: k("z")})
				tx.GetRange(fdb.KeyRange{Begin: k("a"), End: k("zzzz")}, fdb.RangeOptions{}).GetSliceOrPanic()
			},
			wantCode: 1020,
		},
		{
			name:    "explicit AddReadConflictKey on a self-Set key takes no conflict",
			raceKey: "m",
			body: func(tx *simTxn) {
				tx.Set(k("m"), []byte("mine"))
				_ = tx.AddReadConflictKey(k("m"))
			},
			wantCode: 0,
		},
		{
			name:    "explicit AddReadConflictRange over a self-cleared range takes no conflict",
			raceKey: "d",
			body: func(tx *simTxn) {
				tx.ClearRange(fdb.KeyRange{Begin: k("a"), End: k("z")})
				_ = tx.AddReadConflictRange(fdb.KeyRange{Begin: k("a"), End: k("z")})
			},
			wantCode: 0,
		},
		{
			// A standalone atomic resolves against the STORED base, so it read the database.
			name:    "standalone atomic then Get DOES conflict",
			raceKey: "m",
			body: func(tx *simTxn) {
				tx.Add(k("m"), []byte{1, 0, 0, 0, 0, 0, 0, 0})
				tx.Get(k("m")).MustGet()
			},
			wantCode: 1020,
		},
		{
			// CompareAndClear is an atomic like any other: standalone, it read the base to
			// compare, whether or not the comparison matched.
			name:    "standalone CompareAndClear then Get DOES conflict",
			raceKey: "m",
			body: func(tx *simTxn) {
				tx.CompareAndClear(k("m"), []byte("no-such-value"))
				tx.Get(k("m")).MustGet()
			},
			wantCode: 1020,
		},
		{
			// An atomic FOLDED over a plain Set inherits the Set at the stack bottom: the
			// value is fully known locally, so no database read happened.
			name:    "Set then atomic then Get takes no conflict",
			raceKey: "m",
			body: func(tx *simTxn) {
				tx.Set(k("m"), []byte{0, 0, 0, 0, 0, 0, 0, 0})
				tx.Add(k("m"), []byte{1, 0, 0, 0, 0, 0, 0, 0})
				tx.Get(k("m")).MustGet()
			},
			wantCode: 0,
		},
		{
			// An atomic over a locally-CLEARED base is independent too: C++ pushes a
			// synthetic SetValue at the stack bottom (WriteMap.cpp:102-111).
			name:    "ClearRange then atomic then Get takes no conflict",
			raceKey: "m",
			body: func(tx *simTxn) {
				tx.ClearRange(fdb.KeyRange{Begin: k("a"), End: k("z")})
				tx.Add(k("m"), []byte{1, 0, 0, 0, 0, 0, 0, 0})
				tx.Get(k("m")).MustGet()
			},
			wantCode: 0,
		},
		{
			// A plain Set REPLACES the operation stack, so it clears an earlier atomic's
			// dependence (client/ryw.go set() rebuilds the entry).
			name:    "atomic then Set then Get takes no conflict",
			raceKey: "m",
			body: func(tx *simTxn) {
				tx.Add(k("m"), []byte{1, 0, 0, 0, 0, 0, 0, 0})
				tx.Set(k("m"), []byte("mine"))
				tx.Get(k("m")).MustGet()
			},
			wantCode: 0,
		},
		{
			// A versionstamped write's operand is written as given; the stamp comes from the
			// commit proxy, not from the stored value.
			name:    "versionstamped value then Get takes no conflict",
			raceKey: "m",
			body: func(tx *simTxn) {
				tx.SetVersionstampedValue(k("m"), versionstampOperand())
				tx.Get(k("m")).MustGet()
			},
			wantCode: 0,
		},
		{
			// With read-your-writes DISABLED the read went to storage and the buffer answered
			// nothing, so the full conflict must still be recorded.
			name:    "RYW disabled: Set then Get DOES conflict",
			raceKey: "m",
			body: func(tx *simTxn) {
				_ = tx.Options().SetReadYourWritesDisable()
				tx.Set(k("m"), []byte("mine"))
				tx.Get(k("m")).MustGet()
			},
			wantCode: 1020,
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := New(nil)
			seed(db, "d", "0", "m", "0", "zz", "0")
			got := commitAfterConcurrentWrite(t, db, tc.raceKey, tc.body)
			if got != tc.wantCode {
				verb := "aborted 1020"
				if tc.wantCode == 0 {
					verb = "committed"
				}
				t.Fatalf("commit code = %d, want %d (the transaction must have %s)", got, tc.wantCode, verb)
			}
		})
	}
}

// TestWriteMapConflictRangesSplitAroundWrittenKeys pins the SEGMENTATION, not just the
// all-or-nothing outcome: a range read over a keyspace the transaction partly wrote must keep
// the unmodified gaps and drop only the independent operation keys. A filter that gave up and
// recorded the whole range (over-conflict) or dropped it entirely (under-conflict, a lost
// update) would both pass a single-key test.
func TestWriteMapConflictRangesSplitAroundWrittenKeys(t *testing.T) {
	t.Parallel()
	db := New(nil)
	tx := db.newTxn()
	tx.Set(k("b"), []byte("mine"))
	tx.Set(k("d"), []byte("mine"))
	wm := tx.buildWriteMap()

	got := wm.conflictRanges([]byte("a"), []byte("f"))
	want := []keyRange{
		{begin: []byte("a"), end: []byte("b")},
		{begin: []byte("b\x00"), end: []byte("d")},
		{begin: []byte("d\x00"), end: []byte("f")},
	}
	if len(got) != len(want) {
		t.Fatalf("conflictRanges = %v, want %v", fmtRanges(got), fmtRanges(want))
	}
	for i := range want {
		if string(got[i].begin) != string(want[i].begin) || string(got[i].end) != string(want[i].end) {
			t.Fatalf("conflictRanges = %v, want %v", fmtRanges(got), fmtRanges(want))
		}
	}

	// Adjacent qualifying segments coalesce: with the gap between b and d also written, the
	// two independent keys and the segment between them drop out as one hole.
	tx.Set(k("c"), []byte("mine"))
	tx.ClearRange(fdb.KeyRange{Begin: k("b\x00"), End: k("d\x00")})
	wm = tx.buildWriteMap()
	got = wm.conflictRanges([]byte("a"), []byte("f"))
	want = []keyRange{
		{begin: []byte("a"), end: []byte("b")},
		{begin: []byte("d\x00"), end: []byte("f")},
	}
	if len(got) != len(want) {
		t.Fatalf("after the clear, conflictRanges = %v, want %v", fmtRanges(got), fmtRanges(want))
	}
	for i := range want {
		if string(got[i].begin) != string(want[i].begin) || string(got[i].end) != string(want[i].end) {
			t.Fatalf("after the clear, conflictRanges = %v, want %v", fmtRanges(got), fmtRanges(want))
		}
	}
}

// TestGapAfterAWrittenKeyStillConflicts pins the boundary the write-map filter creates.
//
// Subtracting a written key from a read range cuts the range at keyAfter(k) — a byte position
// STRICTLY BETWEEN k and the next stored key. The bytes from there to the next key are still
// unmodified keyspace the read resolved from the database, so they must stay a conflict. A
// filter that rounded the subtraction up to "the whole key slot plus the gap after it" would
// silently drop them, and a concurrent ClearRange spanning the gap would stop aborting this
// transaction — an under-conflict, i.e. a lost update.
//
// This is the shape that made the range-conflict hunt's whole-key interval model disagree with
// the resolver, which is why the model now works in doubled positions.
func TestGapAfterAWrittenKeyStillConflicts(t *testing.T) {
	t.Parallel()
	db := New(nil)
	seed(db, "a", "0", "c", "0")

	tx := db.newTxn()
	tx.Set(k("a"), []byte("mine")) // "a" is answered locally...
	tx.GetRange(fdb.KeyRange{Begin: k("a"), End: k("d")}, fdb.RangeOptions{}).GetSliceOrPanic()

	// ...but the read still covered (a\x00, d), which nothing local answered. A concurrent
	// ClearRange over that gap must abort this transaction.
	if _, err := db.Transact(func(w fdb.WritableTransaction) (any, error) {
		w.ClearRange(fdb.KeyRange{Begin: k("a\x00"), End: k("b")})
		return nil, nil
	}); err != nil {
		t.Fatalf("concurrent ClearRange: %v", err)
	}

	tx.Set(k("zzz-unrelated"), []byte("x"))
	if code := errCode(t, tx.Commit().Get()); code != 1020 {
		t.Fatalf("commit code = %d, want 1020: the gap between the self-written key and the "+
			"next key was read from the database and must still conflict", code)
	}
}

func fmtRanges(rs []keyRange) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = string(r.begin) + ".." + string(r.end)
	}
	return out
}

// versionstampOperand builds a minimal SetVersionstampedValue operand: a 10-byte placeholder
// plus the 4-byte little-endian offset the API-version-520+ encoding requires.
func versionstampOperand() []byte {
	v := make([]byte, 14)
	for i := 0; i < 10; i++ {
		v[i] = 0xFF
	}
	return v // offset 0, little-endian, already zeroed
}
