package simfdb

import (
	"fmt"
	"testing"

	fdb "fdb.dev/pkg/fdbgo/fdb"
)

func conflictStrings(tx *simTxn) []string {
	out := make([]string, len(tx.readConflicts))
	for i, r := range tx.readConflicts {
		out[i] = fmt.Sprintf("[%q,%q)", r.begin, r.end)
	}
	return out
}

// TestAbandonedCursorConflictsOnlyOverWhatItFetched is the batch-boundary pin.
//
// A cursor is a batched read: the real backend fetches a streaming-mode-sized page, and EACH
// FETCH takes its own read-conflict range clamped to what it returned
// (fdb/range_result.go:245-283). A reader that stops after the first page has therefore
// conflicted over one page. Taking the whole requested range at GetRange() call time
// over-conflicts every early-abandoned scan — and the record layer's cursors abandon at every
// continuation boundary, which is most of them.
func TestAbandonedCursorConflictsOnlyOverWhatItFetched(t *testing.T) {
	t.Parallel()
	db := New(nil)
	seed(db, "a", "1", "b", "2", "c", "3", "d", "4", "e", "5", "f", "6", "g", "7", "h", "8")

	tx := db.newTxn()
	// StreamingModeSmall fetches 10 rows per batch; a 2-row limit on the ITERATOR mode's first
	// batch is what makes the boundary observable, so use Iterator mode (2, 4, 8, ... rows).
	it := tx.GetRange(fdb.KeyRange{Begin: k("a"), End: k("z")},
		fdb.RangeOptions{Mode: fdb.StreamingModeIterator}).Iterator()
	var got []string
	for i := 0; i < 2; i++ {
		if !it.Advance() {
			t.Fatalf("iterator exhausted after %d rows", i)
		}
		kv, err := it.Get()
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		got = append(got, string(kv.Key))
	}
	// Abandon the cursor here — two rows consumed, one batch fetched.
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("consumed %v, want [a b]", got)
	}

	if len(tx.readConflicts) != 1 {
		t.Fatalf("recorded %d read-conflict ranges %v, want 1 (one fetched batch)",
			len(tx.readConflicts), conflictStrings(tx))
	}
	rc := tx.readConflicts[0]
	if string(rc.begin) != "a" || string(rc.end) != "b\x00" {
		t.Fatalf("conflict range = [%q,%q), want [\"a\",\"b\\x00\") — the abandoned cursor read "+
			"only the first batch, so it must not conflict past it", rc.begin, rc.end)
	}

	// The behavioural consequence: a concurrent insert past the abandoned point does not abort.
	if _, err := db.Transact(func(w fdb.WritableTransaction) (any, error) {
		w.Set(k("g2"), []byte("late"))
		return nil, nil
	}); err != nil {
		t.Fatalf("concurrent write: %v", err)
	}
	tx.Set(k("zzz"), []byte("x"))
	if err := tx.Commit().Get(); err != nil {
		t.Fatalf("commit after abandoning the cursor at row 2: %v — an insert beyond the last "+
			"fetched batch must not abort the reader", err)
	}
}

// TestDrainedCursorConflictsOverTheWholeRange is the other side: once the cursor runs to
// exhaustion, the union of its per-batch conflicts covers the full requested range, including
// the trailing gap that no batch returned a row from. Narrowing per batch must not lose phantom
// protection for a reader that actually read everything.
func TestDrainedCursorConflictsOverTheWholeRange(t *testing.T) {
	t.Parallel()
	db := New(nil)
	seed(db, "a", "1", "b", "2", "c", "3", "d", "4", "e", "5")

	tx := db.newTxn()
	it := tx.GetRange(fdb.KeyRange{Begin: k("a"), End: k("z")},
		fdb.RangeOptions{Mode: fdb.StreamingModeIterator}).Iterator()
	n := 0
	for it.Advance() {
		n++
	}
	if n != 5 {
		t.Fatalf("drained %d rows, want 5", n)
	}

	// A concurrent insert in the TRAILING gap — past the last row, inside the requested range —
	// must abort the reader.
	if _, err := db.Transact(func(w fdb.WritableTransaction) (any, error) {
		w.Set(k("y"), []byte("phantom"))
		return nil, nil
	}); err != nil {
		t.Fatalf("concurrent write: %v", err)
	}
	tx.Set(k("zzz"), []byte("x"))
	if code := errCode(t, tx.Commit().Get()); code != 1020 {
		t.Fatalf("commit code = %d, want 1020: a fully drained scan must still conflict on a "+
			"phantom inserted in its trailing gap (conflicts were %v)", code, conflictStrings(tx))
	}
}

// TestCursorBatchSizesFollowTheStreamingMode pins that the sim batches by the SAME rule as the
// client (fdb.BatchSize), not by some sim-local convention. Batch boundaries are where a
// continuation lands and where each conflict range ends, so a different rule means a different
// paging and a different conflict shape.
func TestCursorBatchSizesFollowTheStreamingMode(t *testing.T) {
	t.Parallel()
	keys := []string{
		"k0", "k1", "k2", "k3", "k4", "k5", "k6", "k7", "k8", "k9",
		"ka", "kb", "kc", "kd", "ke", "kf",
	}
	var kvs []string
	for _, key := range keys {
		kvs = append(kvs, key, "v")
	}

	for _, tc := range []struct {
		mode  fdb.StreamingMode
		name  string
		batch []int // rows per fetched batch, in order
	}{
		{fdb.StreamingModeIterator, "iterator doubles", []int{2, 4, 8, 2}},
		{fdb.StreamingModeSmall, "small caps at 10", []int{10, 6}},
		{fdb.StreamingModeWantAll, "want-all fetches everything", []int{16}},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := New(nil)
			seed(db, kvs...)
			tx := db.newTxn()
			it := tx.GetRange(fdb.KeyRange{Begin: k("k"), End: k("l")},
				fdb.RangeOptions{Mode: tc.mode}).Iterator().(*rangeIter)

			var sizes []int
			last := 0
			for it.Advance() {
				if it.batchEnd != last {
					sizes = append(sizes, it.batchEnd-last)
					last = it.batchEnd
				}
			}
			if len(sizes) != len(tc.batch) {
				t.Fatalf("batches = %v, want %v", sizes, tc.batch)
			}
			for i := range sizes {
				if sizes[i] != tc.batch[i] {
					t.Fatalf("batches = %v, want %v", sizes, tc.batch)
				}
			}
			// Every batch takes exactly one conflict range (no self-writes here to split it).
			if len(tx.readConflicts) != len(tc.batch) {
				t.Fatalf("recorded %d conflict ranges %v, want one per batch (%d)",
					len(tx.readConflicts), conflictStrings(tx), len(tc.batch))
			}
		})
	}
}

// TestIteratorRejectsInvalidRangeOptions pins the two validations the client's Iterator()
// performs: a row limit below ROW_LIMIT_UNLIMITED(-1) is range_limits_invalid(2012), and EXACT
// without a limit is exact_mode_without_limits(2210) (libfdb_c
// validate_and_update_parameters). Without them a Limit of -2 silently returned zero rows.
func TestIteratorRejectsInvalidRangeOptions(t *testing.T) {
	t.Parallel()
	db := New(nil)
	seed(db, "a", "1")
	for _, tc := range []struct {
		name string
		opts fdb.RangeOptions
		want int
	}{
		{"limit -2", fdb.RangeOptions{Limit: -2}, 2012},
		{"exact without limit", fdb.RangeOptions{Mode: fdb.StreamingModeExact}, 2210},
		{"exact with limit is fine", fdb.RangeOptions{Mode: fdb.StreamingModeExact, Limit: 1}, 0},
		{"limit -1 is unlimited", fdb.RangeOptions{Limit: -1}, 0},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tx := db.newTxn()
			it := tx.GetRange(fdb.KeyRange{Begin: k("a"), End: k("z")}, tc.opts).Iterator()
			it.Advance()
			_, err := it.Get()
			if tc.want == 0 {
				if err != nil {
					t.Fatalf("Get: %v, want no error", err)
				}
				return
			}
			if code := errCode(t, err); code != tc.want {
				t.Fatalf("Get error code = %d, want %d", code, tc.want)
			}
		})
	}
}
