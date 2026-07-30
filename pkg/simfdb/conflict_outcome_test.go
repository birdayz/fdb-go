package simfdb

import (
	"fmt"
	"testing"

	fdb "fdb.dev/pkg/fdbgo/fdb"
)

// TestSimFDB_ConflictOutcomeParity checks SimFDB's SSI conflict-outcome against the outcomes real
// FDB + libfdb_c produce (the go-vs-cgo conflict-outcome differential is the ground truth; the
// read-conflict extent is the pure-Go client's rangeConflictExtent). It is the differential's
// oracle encoded as known outcomes — validating the subtlest SSI-fidelity point (how a GetRange's
// read conflict range is clamped) without cgo or a container. The adversarial cases (empty-range
// and gap probes) are the write-skew / phantom axis: an empty or exhausted read must conflict on
// the FULL requested range so a concurrent insert in a gap still aborts.
func TestSimFDB_ConflictOutcomeParity(t *testing.T) {
	t.Parallel()
	kk := func(i int) fdb.Key { return fdb.Key(fmt.Sprintf("k%02d", i)) }
	full := fdb.KeyRange{Begin: kk(0), End: kk(10)}

	// run seeds the given keys, applies txnA's read, commits a concurrent probe write to
	// probeKey, then commits txnA (writing a disjoint key). Returns whether txnA committed.
	run := func(seed []int, read func(fdb.WritableTransaction), probeKey fdb.Key) bool {
		db := New(nil)
		if _, err := db.Transact(func(tx fdb.WritableTransaction) (any, error) {
			for _, i := range seed {
				tx.Set(kk(i), []byte{byte(i)})
			}
			return nil, nil
		}); err != nil {
			t.Fatalf("seed: %v", err)
		}
		txA := db.newTxn(false)
		read(txA)
		if _, err := db.Transact(func(tx fdb.WritableTransaction) (any, error) {
			tx.Set(probeKey, []byte{0xEE})
			return nil, nil
		}); err != nil {
			t.Fatalf("probe: %v", err)
		}
		txA.Set(fdb.Key("zz-disjoint"), []byte("x"))
		return txA.Commit().Get() == nil
	}

	dense := []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9}
	sparse := []int{2, 4, 6, 8} // gaps at k00,k01,k03,k05,k07,k09

	cases := []struct {
		name       string
		seed       []int
		read       func(fdb.WritableTransaction)
		probe      fdb.Key
		wantCommit bool
	}{
		// --- limit-truncated reads: narrow to the returned data ---
		{"fwd_limited_probe_in_unread_tail", dense, func(tx fdb.WritableTransaction) {
			tx.GetRange(full, fdb.RangeOptions{Limit: 3}).GetSliceOrPanic() // returns k00,k01,k02
		}, kk(5), true},
		{"reverse_limited_probe_in_suffix", dense, func(tx fdb.WritableTransaction) {
			tx.GetRange(full, fdb.RangeOptions{Reverse: true, Limit: 3}).GetSliceOrPanic() // k09,k08,k07
		}, kk(8), false},
		{"reverse_limited_probe_outside", dense, func(tx fdb.WritableTransaction) {
			tx.GetRange(full, fdb.RangeOptions{Reverse: true, Limit: 3}).GetSliceOrPanic()
		}, kk(2), true},

		// --- point reads ---
		{"point_read_probe_on_key", dense, func(tx fdb.WritableTransaction) {
			tx.Get(kk(3)).MustGet()
		}, kk(3), false},
		{"point_read_probe_other_key", dense, func(tx fdb.WritableTransaction) {
			tx.Get(kk(3)).MustGet()
		}, kk(7), true},
		{"snapshot_read_no_conflict", dense, func(tx fdb.WritableTransaction) {
			tx.Snapshot().GetRange(full, fdb.RangeOptions{}).GetSliceOrPanic()
		}, kk(4), true},

		// --- GetKey selector-oriented conflict range (backward selectors must not invert) ---
		// LastLessThan(k05) resolves to k04; conflict span [k04, k05). A probe in the span aborts;
		// a naive [selKey, keyAfter(resolved)) would be inverted and never conflict.
		{"getkey_backward_probe_in_span", dense, func(tx fdb.WritableTransaction) {
			tx.GetKey(fdb.LastLessThan(kk(5))).MustGet()
		}, kk(4), false},
		{"getkey_backward_probe_outside_span", dense, func(tx fdb.WritableTransaction) {
			tx.GetKey(fdb.LastLessThan(kk(5))).MustGet()
		}, kk(2), true},
		// FirstGreaterThan(k05) resolves to k06; conflict span (k05, k06].
		{"getkey_forward_probe_in_span", dense, func(tx fdb.WritableTransaction) {
			tx.GetKey(fdb.FirstGreaterThan(kk(5))).MustGet()
		}, kk(6), false},

		// --- write-skew / phantom axis: exhausted & empty reads conflict on the FULL range ---
		// Unlimited (exhausted) read of a densely-seeded range: any probe inside conflicts.
		{"unlimited_dense_probe_inside", dense, func(tx fdb.WritableTransaction) {
			tx.GetRange(full, fdb.RangeOptions{}).GetSliceOrPanic()
		}, kk(4), false},
		// EMPTY read of [k03,k06): a concurrent insert INSIDE it must abort (phantom protection),
		// even though zero rows were returned.
		{"empty_range_probe_inside", nil, func(tx fdb.WritableTransaction) {
			tx.GetRange(fdb.KeyRange{Begin: kk(3), End: kk(6)}, fdb.RangeOptions{}).GetSliceOrPanic()
		}, kk(4), false},
		{"empty_range_probe_outside", nil, func(tx fdb.WritableTransaction) {
			tx.GetRange(fdb.KeyRange{Begin: kk(3), End: kk(6)}, fdb.RangeOptions{}).GetSliceOrPanic()
		}, kk(8), true},
		// Sparse seed, unlimited read → returns k02,k04,k06,k08 (exhausted). A probe in the
		// LEADING gap (k00, before the first returned key) or TRAILING gap (k09, after the last)
		// is inside the requested [k00,k10) → must abort. This is the exact case a
		// clamp-to-returned-data implementation gets wrong.
		{"unlimited_sparse_probe_leading_gap", sparse, func(tx fdb.WritableTransaction) {
			tx.GetRange(full, fdb.RangeOptions{}).GetSliceOrPanic()
		}, kk(0), false},
		{"unlimited_sparse_probe_trailing_gap", sparse, func(tx fdb.WritableTransaction) {
			tx.GetRange(full, fdb.RangeOptions{}).GetSliceOrPanic()
		}, kk(9), false},
	}
	for _, c := range cases {
		if got := run(c.seed, c.read, c.probe); got != c.wantCommit {
			t.Errorf("%s: committed=%v, want %v — SimFDB SSI conflict-outcome diverges from real FDB",
				c.name, got, c.wantCommit)
		}
	}
}
