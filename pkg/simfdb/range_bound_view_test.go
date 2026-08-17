package simfdb

import (
	"fmt"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb"
)

// TestTrivialRangeBoundsBuildNoWholeKeyspaceView is a cost pin with a guard in
// BOTH directions, because either direction failing is a real defect.
//
// A GetRange resolves its two bounds before reading. Both trivial selectors —
// FirstGreaterOrEqual and FirstGreaterThan — resolve arithmetically and never
// read a single element of the view, and EVERY exact range in the tree
// (KeyRange, subspace, Tuple) reports FirstGreaterOrEqual bounds. Building the
// view anyway clones, maps and SORTS the whole keyspace per GetRange and then
// discards it unread: measured at 17.7% of total CPU in `sort.Strings` alone on
// a 20k-row SimFDB scan benchmark, and 2.8x-8.4x on the SQL read benchmarks
// once removed.
//
// No assertion on the returned rows can see this — the rows are identical
// either way — so the pin is on the work.
//
// The second direction is not symmetry for its own sake. A non-trivial bound
// genuinely NEEDS the resolution, and "never build it" would pass the first
// assertion while silently resolving LastLessThan against nothing, which
// under-conflicts: rows the transaction really read would fall outside the
// recorded range and a concurrent write to them would not conflict.
func TestTrivialRangeBoundsBuildNoWholeKeyspaceView(t *testing.T) {
	t.Parallel()

	const nRows = 500
	var kvs []string
	for i := 0; i < nRows; i++ {
		kvs = append(kvs, fmt.Sprintf("k%06d", i), "v")
	}
	db := New(nil)
	seed(db, kvs...)

	drain := func(t *testing.T, tx *simTxn, r fdb.Range) int {
		t.Helper()
		it := tx.GetRange(r, fdb.RangeOptions{Mode: fdb.StreamingModeIterator}).Iterator()
		n := 0
		for it.Advance() {
			n++
		}
		return n
	}

	t.Run("trivial bounds build nothing", func(t *testing.T) {
		t.Parallel()
		tx := db.newTxn()
		// A plain KeyRange: FirstGreaterOrEqual on both ends, the shape essentially
		// every read in the record layer issues.
		if got := drain(t, tx, fdb.KeyRange{Begin: fdb.Key("k"), End: fdb.Key("l")}); got != nRows {
			t.Fatalf("read %d rows, want %d — the range under test did not read the data", got, nRows)
		}
		if tx.wholeViewBuilds != 0 {
			t.Fatalf("a GetRange with two TRIVIAL bounds built the whole-keyspace view %d time(s). "+
				"Both trivial selectors resolve arithmetically and never read the view, so every "+
				"element of that build is sorted and then discarded — at O(keyspace log keyspace) "+
				"per range read, on the path every SimFDB-backed test drives.",
				tx.wholeViewBuilds)
		}
	})

	t.Run("a non-trivial bound still resolves against a real view", func(t *testing.T) {
		t.Parallel()
		tx := db.newTxn()
		// LastLessThan resolves BELOW its anchor and cannot be computed from the key
		// alone; it needs the view.
		r := fdb.SelectorRange{
			Begin: fdb.LastLessThan(fdb.Key("k000100")),
			End:   fdb.FirstGreaterOrEqual(fdb.Key("k000110")),
		}
		if got := drain(t, tx, r); got == 0 {
			t.Fatal("the non-trivial-bound range returned no rows, so it cannot show that the " +
				"bound was resolved at all")
		}
		if tx.wholeViewBuilds == 0 {
			t.Fatal("a LastLessThan bound resolved WITHOUT building the view. It cannot have " +
				"been resolved correctly: the selector points below its anchor, so skipping the " +
				"build resolves it against nothing and under-conflicts — rows the transaction " +
				"really read fall outside the recorded conflict range.")
		}

		// And the resolution is the right one, not merely non-zero work: LastLessThan
		// lands on the key strictly below the anchor.
		view := tx.buildView()
		if got := string(resolveRangeBound(view, fdb.LastLessThan(fdb.Key("k000100")).FDBKeySelector())); got != "k000099" {
			t.Fatalf("LastLessThan(k000100) resolved to %q, want %q", got, "k000099")
		}
	})
}
