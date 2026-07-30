package simfdb

import (
	"fmt"
	"testing"

	fdb "fdb.dev/pkg/fdbgo/fdb"
)

// TestSimFDB_ConflictOutcomeParity pins fourteen named SSI conflict outcomes.
//
// The expectations are HAND-DERIVED from the pure-Go client's documented clamping rule
// (rangeConflictExtent) — they were not observed on a cluster, and the header used to say they
// were. That mattered: an expectation derived from the same reading of the rule that the
// implementation was derived from cannot independently confirm the rule, and calling it
// "the differential's oracle" invited exactly that mistake.
//
// What it IS good for is naming the cases. Fourteen labelled shapes — empty range, leading gap,
// trailing gap, limit met exactly, probe just outside — say which situations are covered in a
// way a randomized sweep cannot, and they run without Docker. The independent confirmation comes
// from the container-driven differential (differential_test.go, differential_arms_test.go),
// which sweeps these same shapes against a real cluster with a rotating seed; if the two ever
// disagree, the cluster is right.
//
// The adversarial cases (empty-range and gap probes) are the write-skew / phantom axis: an empty
// or exhausted read must conflict on the FULL requested range so a concurrent insert in a gap
// still aborts.
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
		txA := db.newTxn()
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
			t.Errorf("%s: committed=%v, want %v — SimFDB diverges from the client's documented clamping rule",
				c.name, got, c.wantCommit)
		}
	}
}
