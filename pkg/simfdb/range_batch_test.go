package simfdb

import (
	"bytes"
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
				fdb.RangeOptions{Mode: tc.mode}).Iterator()

			// The trace log is the client's own per-fetch surface, so reading batch sizes
			// through it pins the batching AND the iteration numbering the client reports:
			// the client traces `ri.iteration-1` AFTER incrementing, so the first fetch is
			// iteration 1 (fdb/range_result.go:255-257). A sim that numbered from 0 would
			// mislabel every trace a continuation bug was being chased through.
			var sizes, iters []int
			it.SetTraceLog(func(iteration, _, returned int, _ bool, _ error) {
				if returned == 0 {
					return // the trailing empty fetch returns no rows
				}
				sizes = append(sizes, returned)
				iters = append(iters, iteration)
			})
			for it.Advance() {
			}
			for i, n := range iters {
				if n != i+1 {
					t.Fatalf("trace iteration numbers = %v, want 1-based consecutive "+
						"(the client traces iteration-1 after incrementing)", iters)
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
			// The COVERAGE of the drained scan, not the number of recorded ranges. Counting
			// them would pin a representation: one range per batch and their union are the
			// same behaviour, and the count assertion would have to be relaxed the day
			// SimFDB coalesces read-conflict ranges the way a real client does. The batching
			// itself is already pinned above, through the client's own trace surface.
			if _, err := db.Transact(func(w fdb.WritableTransaction) (any, error) {
				w.Set(k("kz"), []byte("phantom")) // inside [k,l), never returned by any batch
				return nil, nil
			}); err != nil {
				t.Fatalf("concurrent write: %v", err)
			}
			tx.Set(k("zzz"), []byte("x"))
			if code := errCode(t, tx.Commit().Get()); code != 1020 {
				t.Fatalf("commit code = %d, want 1020: a fully drained scan conflicts over the "+
					"whole requested range, trailing gap included (conflicts were %v)",
					code, conflictStrings(tx))
			}
		})
	}
}

// TestRangeOptionValidationPerSurface pins the range-option validation on BOTH consumption
// paths, and pins the PRECEDENCE between the two codes, which is the part that is easy to get
// wrong in either direction.
//
//   - A row limit below ROW_LIMIT_UNLIMITED(-1) is range_limits_invalid(2012). GetSlice used to
//     skip it and silently returned every row for a Limit of -2.
//   - EXACT with no row budget is exact_mode_without_limits(2210), for both spellings of "no
//     budget" (0 and -1 — libfdb_c maps a zero limit to ROW_LIMIT_UNLIMITED before the gate).
//   - 2012 WINS over 2210: a limit below ROW_LIMIT_UNLIMITED is invalid rather than unlimited, so
//     EXACT with -7 is 2012. A "Limit <= 0" test for the 2210 case gets this backwards on any
//     surface that reaches 2012 later than it reaches the mode check.
//
// This was modelled as Iterator-only 2210 on the argument that GetSlice never forwards the
// streaming mode in either real backend. That argument was from source and it was wrong: Apple's
// binding issues the first batch's future eagerly with the caller's mode and rewrites the mode
// only for later batches. TestDifferential_ExactModeWithoutLimits measures every cell below
// against libfdb_c directly (TestDifferential_RangeOptionValidation only reaches the pure-Go
// client, so it could not have caught a Go-vs-C divergence here).
func TestRangeOptionValidationPerSurface(t *testing.T) {
	t.Parallel()
	db := New(nil)
	seed(db, "a", "1")
	for _, tc := range []struct {
		name                string
		opts                fdb.RangeOptions
		wantIter, wantSlice int
	}{
		{"limit -2", fdb.RangeOptions{Limit: -2}, 2012, 2012},
		{"limit -7 with exact", fdb.RangeOptions{Mode: fdb.StreamingModeExact, Limit: -7}, 2012, 2012},
		{"exact without limit", fdb.RangeOptions{Mode: fdb.StreamingModeExact}, 2210, 2210},
		{"exact with limit -1", fdb.RangeOptions{Mode: fdb.StreamingModeExact, Limit: -1}, 2210, 2210},
		{"exact with limit is fine", fdb.RangeOptions{Mode: fdb.StreamingModeExact, Limit: 1}, 0, 0},
		{"limit -1 is unlimited", fdb.RangeOptions{Limit: -1}, 0, 0},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			check := func(surface string, want int, err error) {
				t.Helper()
				if want == 0 {
					if err != nil {
						t.Fatalf("%s: %v, want no error", surface, err)
					}
					return
				}
				if code := errCode(t, err); code != want {
					t.Fatalf("%s error code = %d, want %d", surface, code, want)
				}
			}
			tx := db.newTxn()
			rr := tx.GetRange(fdb.KeyRange{Begin: k("a"), End: k("z")}, tc.opts)
			it := rr.Iterator()
			it.Advance()
			_, err := it.Get()
			check("Iterator", tc.wantIter, err)

			_, err = tx.GetRange(fdb.KeyRange{Begin: k("a"), End: k("z")}, tc.opts).GetSliceWithError()
			check("GetSliceWithError", tc.wantSlice, err)
		})
	}
}

// TestCursorBatchSizesSaturate pins the SATURATION half of the ITERATOR progression, which
// TestCursorBatchSizesFollowTheStreamingMode cannot reach: with 16 rows the scan ends at
// iteration 4, long before the clamp bites, so that test is green whether or not the
// progression is bounded.
//
// libfdb_c gives ITERATOR a byte target from a fixed table and clamps the index into it
// (bindings/c/fdb_c.cpp:1006 table, :1019 `iteration = std::min(iteration, max_iteration)`),
// so its per-fetch target stops growing. This client's budget is a row count that used to
// double without a clamp, which made one fetch eventually cover the whole remaining range.
// Because batch boundaries are where a continuation lands and where each conflict range
// ends, the sim has to saturate at exactly the point the client does — so this pins it on
// the sim's own iterator, through the client's trace surface, the same way the batching
// rule itself is pinned.
func TestCursorBatchSizesSaturate(t *testing.T) {
	t.Parallel()

	// Enough rows to run well past the saturation point: the doubling phase consumes
	// 2+4+...+1024 = 2046 rows, so 6000 leaves several fully-saturated batches behind it.
	const nRows = 6000
	var kvs []string
	for i := 0; i < nRows; i++ {
		kvs = append(kvs, fmt.Sprintf("k%06d", i), "v")
	}
	db := New(nil)
	seed(db, kvs...)

	// The expected progression, written out independently of fdb.BatchSize rather than
	// derived from it — a test that asked the implementation what it does would agree with
	// any implementation, including the unbounded one.
	const wantCap = 1024
	var want []int
	remaining := nRows
	for size := 2; remaining > 0; size *= 2 {
		if size > wantCap {
			size = wantCap
		}
		if size > remaining {
			size = remaining
		}
		want = append(want, size)
		remaining -= size
	}

	tx := db.newTxn()
	it := tx.GetRange(fdb.KeyRange{Begin: k("k"), End: k("l")},
		fdb.RangeOptions{Mode: fdb.StreamingModeIterator}).Iterator()
	var got []int
	it.SetTraceLog(func(_, _, returned int, _ bool, _ error) {
		if returned == 0 {
			return // the trailing empty fetch returns no rows
		}
		got = append(got, returned)
	})
	rows := 0
	for it.Advance() {
		rows++
	}
	if rows != nRows {
		t.Fatalf("iterated %d rows, want %d", rows, nRows)
	}

	peak := 0
	for _, n := range got {
		if n > peak {
			peak = n
		}
	}
	if peak > wantCap {
		t.Fatalf("peak batch = %d, want <= %d — the ITERATOR progression must SATURATE, "+
			"not keep doubling; batches were %v", peak, wantCap, got)
	}
	if peak != wantCap {
		t.Fatalf("peak batch = %d, want exactly %d — the progression must still grow up to "+
			"the saturation point; batches were %v", peak, wantCap, got)
	}
	if len(got) != len(want) {
		t.Fatalf("batches = %v (%d fetches), want %v (%d fetches)",
			got, len(got), want, len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("batch %d = %d, want %d (full sequence %v, want %v)",
				i+1, got[i], want[i], got, want)
		}
	}
}

// TestCursorPageCostsThePageNotTheTail is the sim's cost pin, the counterpart to the client's
// TestSnapshotCacheWalkCostsThePageNotTheTail.
//
// Every cursor fetch resolves its page through buildViewRange, which used to clone, map and
// SORT the entire unconsumed range before slicing off one page. That was tolerable while the
// ITERATOR progression doubled without bound (a drain took a logarithmic number of fetches);
// once the progression saturates, a drain takes LINEARLY many fetches, so linearly many full
// builds — a quadratic drain, which is what makes large deterministic-sim workloads
// impractical.
//
// As with the client, no assertion on the returned rows can see this: the page is identical
// either way. The pin is on the work, via the viewRowsTouched counter.
func TestCursorPageCostsThePageNotTheTail(t *testing.T) {
	t.Parallel()

	// Past saturation by a wide margin, so the tail behind an early page is far larger than
	// the page itself and the two costs are unmistakably different.
	const nRows = 20000
	const cap = 1024
	var kvs []string
	for i := 0; i < nRows; i++ {
		kvs = append(kvs, fmt.Sprintf("k%06d", i), "v")
	}
	db := New(nil)
	seed(db, kvs...)

	for _, reverse := range []bool{false, true} {
		reverse := reverse
		name := "forward"
		if reverse {
			name = "reverse"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			tx := db.newTxn()
			it := tx.GetRange(fdb.KeyRange{Begin: k("k"), End: k("l")},
				fdb.RangeOptions{Mode: fdb.StreamingModeIterator, Reverse: reverse}).Iterator()

			// Drain past saturation so several equal-sized pages are resolved, then measure
			// ONE page in isolation.
			rows := 0
			for rows < 4000 && it.Advance() {
				rows++
			}
			before := tx.viewRowsTouched
			target := rows + cap
			for rows < target && it.Advance() {
				rows++
			}
			touched := tx.viewRowsTouched - before

			// A page costs the page (plus at most the geometric widening a clear would
			// force, and there are no clears here), not the ~15000 rows still ahead of it.
			if touched > 4*cap {
				t.Fatalf("%s: resolving a %d-row page touched %d store rows with ~%d rows "+
					"still unconsumed — a page must cost the page, not the tail. An "+
					"unbounded view build makes a saturated drain quadratic.",
					name, cap, touched, nRows-rows)
			}
		})
	}
}

// TestCursorBoundedViewMatchesUnbounded is the sim's correctness control for the bounded view
// build: with the write buffer OVERLAPPING the scanned range — sets that add keys, sets that
// overwrite existing ones, a clear, and a clear range that removes a run — a bounded drain must
// return exactly the rows an unbounded build returns, in both directions, with and without a
// row limit. This is where the sub-window reasoning could be wrong, so it is checked against
// the unbounded build itself rather than hand-written expectations.
func TestCursorBoundedViewMatchesUnbounded(t *testing.T) {
	t.Parallel()

	const nRows = 3000
	var seedKV []string
	for i := 0; i < nRows; i++ {
		seedKV = append(seedKV, fmt.Sprintf("k%06d", i), fmt.Sprintf("v%06d", i))
	}
	db := New(nil)
	seed(db, seedKV...)

	// Mutations chosen to exercise every arm the merge has: a key BELOW everything, a key
	// interleaved mid-range, an overwrite, a single clear, and a clear range spanning a run
	// that straddles page boundaries.
	apply := func(tx *simTxn) {
		tx.Set(k("k000000a"), []byte("inserted-mid"))
		tx.Set(k("j999999"), []byte("inserted-low"))
		tx.Set(fdb.Key(fmt.Sprintf("k%06d", 1500)), []byte("overwritten"))
		tx.Clear(fdb.Key(fmt.Sprintf("k%06d", 2)))
		tx.ClearRange(fdb.KeyRange{
			Begin: fdb.Key(fmt.Sprintf("k%06d", 1000)),
			End:   fdb.Key(fmt.Sprintf("k%06d", 1400)),
		})
	}

	kr := fdb.KeyRange{Begin: k("j"), End: k("l")}
	for _, reverse := range []bool{false, true} {
		for _, limit := range []int{0, 1, 7, 1024, 2500} {
			// Reference: one unbounded build of the whole range, then the same limit and
			// direction applied to it by hand.
			ref := db.newTxn()
			// The iterator path pins the read version lazily on first read; a direct
			// buildViewRange call does not, and at version 0 no committed row is visible.
			ref.ensureReadVersion()
			apply(ref)
			refRows := ref.buildViewRange([]byte("j"), []byte("l"))
			if reverse {
				reverseKVs(refRows)
			}
			if limit > 0 && len(refRows) > limit {
				refRows = refRows[:limit]
			}

			// Actual: a real paged drain through the iterator, which uses the bounded build.
			tx := db.newTxn()
			apply(tx)
			it := tx.GetRange(kr, fdb.RangeOptions{
				Mode: fdb.StreamingModeIterator, Reverse: reverse, Limit: limit,
			}).Iterator()
			var got []fdb.KeyValue
			for it.Advance() {
				kv, err := it.Get()
				if err != nil {
					t.Fatalf("reverse=%v limit=%d: %v", reverse, limit, err)
				}
				got = append(got, kv)
			}

			if len(got) != len(refRows) {
				t.Fatalf("reverse=%v limit=%d: drained %d rows, unbounded build gives %d",
					reverse, limit, len(got), len(refRows))
			}
			for i := range refRows {
				if !bytes.Equal([]byte(got[i].Key), []byte(refRows[i].Key)) {
					t.Fatalf("reverse=%v limit=%d: row %d key = %q, want %q — the bounded "+
						"view's sub-window must fold in exactly the mutations the "+
						"unbounded build would have",
						reverse, limit, i, got[i].Key, refRows[i].Key)
				}
				if !bytes.Equal(got[i].Value, refRows[i].Value) {
					t.Fatalf("reverse=%v limit=%d: row %d (%q) value = %q, want %q",
						reverse, limit, i, got[i].Key, got[i].Value, refRows[i].Value)
				}
			}
		}
	}
}
