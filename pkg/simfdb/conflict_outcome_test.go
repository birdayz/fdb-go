package simfdb

import (
	"fmt"
	"testing"

	fdb "fdb.dev/pkg/fdbgo/fdb"
)

// TestSimFDB_ConflictOutcomeParity checks SimFDB's SSI conflict-outcome against the outcomes the
// go-vs-cgo conflict-outcome differential (bench/differential_conflict_outcome_fuzz_test.go)
// established against real FDB + libfdb_c. It is the differential's oracle, encoded as known
// outcomes — so it validates the subtlest SSI-fidelity point (GetRange clamps its read conflict
// to the data actually RETURNED, not the requested span) without needing cgo or a container. A
// divergence here is exactly the under-/over-conflict class the differential exists to catch.
func TestSimFDB_ConflictOutcomeParity(t *testing.T) {
	t.Parallel()
	kk := func(i int) fdb.Key { return fdb.Key(fmt.Sprintf("k%02d", i)) }
	fullRange := fdb.KeyRange{Begin: kk(0), End: kk(10)}

	// run seeds k00..k09, applies txnA's reads, commits a concurrent probe write to k<probe>,
	// then commits txnA (writing a disjoint key). Returns whether txnA committed.
	run := func(read func(fdb.WritableTransaction), probe int) bool {
		db := New(nil)
		db.Transact(func(tx fdb.WritableTransaction) (any, error) {
			for i := 0; i < 10; i++ {
				tx.Set(kk(i), []byte{byte(i)})
			}
			return nil, nil
		})
		txA := db.newTxn(false)
		read(txA)
		db.Transact(func(tx fdb.WritableTransaction) (any, error) {
			tx.Set(kk(probe), []byte{0xEE})
			return nil, nil
		})
		txA.Set(fdb.Key("zz-disjoint"), []byte("x"))
		return txA.Commit().Get() == nil
	}

	cases := []struct {
		name       string
		read       func(fdb.WritableTransaction)
		probe      int
		wantCommit bool
	}{
		// Forward limited read returns k00,k01,k02 → read conflict clamped to [k00,k02]; a probe
		// in the unread tail (k05) does NOT conflict → COMMIT.
		{"fwd_limited_probe_in_unread_tail", func(tx fdb.WritableTransaction) {
			tx.GetRange(fullRange, fdb.RangeOptions{Limit: 3}).GetSliceOrPanic()
		}, 5, true},
		// Unlimited drain reads all k00..k09 → a probe inside (k04) conflicts → ABORT.
		{"unlimited_drain_probe_inside", func(tx fdb.WritableTransaction) {
			tx.GetRange(fullRange, fdb.RangeOptions{}).GetSliceOrPanic()
		}, 4, false},
		// Reverse limited read returns k09,k08,k07 → conflict clamped to [k07,k09]; probe in the
		// scanned suffix (k08) conflicts → ABORT.
		{"reverse_limited_probe_in_suffix", func(tx fdb.WritableTransaction) {
			tx.GetRange(fullRange, fdb.RangeOptions{Reverse: true, Limit: 3}).GetSliceOrPanic()
		}, 8, false},
		// Same reverse read, but the probe (k02) is outside the scanned suffix → COMMIT.
		{"reverse_limited_probe_outside", func(tx fdb.WritableTransaction) {
			tx.GetRange(fullRange, fdb.RangeOptions{Reverse: true, Limit: 3}).GetSliceOrPanic()
		}, 2, true},
		// A point read conflicts with a probe on the same key → ABORT.
		{"point_read_probe_on_key", func(tx fdb.WritableTransaction) {
			tx.Get(kk(3)).MustGet()
		}, 3, false},
		// A point read does not conflict with a probe on a different key → COMMIT.
		{"point_read_probe_other_key", func(tx fdb.WritableTransaction) {
			tx.Get(kk(3)).MustGet()
		}, 7, true},
		// A snapshot read adds no read conflict → COMMIT regardless of the probe.
		{"snapshot_read_no_conflict", func(tx fdb.WritableTransaction) {
			tx.Snapshot().GetRange(fullRange, fdb.RangeOptions{}).GetSliceOrPanic()
		}, 4, true},
	}
	for _, c := range cases {
		if got := run(c.read, c.probe); got != c.wantCommit {
			t.Errorf("%s: committed=%v, want %v — SimFDB SSI conflict-outcome diverges from real FDB",
				c.name, got, c.wantCommit)
		}
	}
}
