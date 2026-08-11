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
	// FAT rows, because the division is byte-driven: ITERATOR's first fetch targets 4096 bytes
	// and a row costs key+value+serverRowOverheadBytes against it, with the row that REACHES the
	// budget included. At a 1-byte key and lazyValueLen(2048) a row costs 1+2048+24 = 2073, so
	// row 1 leaves the reply under budget (2073 < 4096) and row 2 reaches it (4146 >= 4096) —
	// the reply stops at two rows with more still pending. With the 1-byte values this fixture
	// used to carry, all eight rows cost 8*26 = 208 bytes and arrived in ONE fetch: there was no
	// boundary left to abandon the cursor at, so the assertion below could not fail.
	seed(db, "a", fatValue("1"), "b", fatValue("2"), "c", fatValue("3"), "d", fatValue("4"),
		"e", fatValue("5"), "f", fatValue("6"), "g", fatValue("7"), "h", fatValue("8"))

	tx := db.newTxn()
	// ITERATOR mode: its first fetch is the smallest byte target any mode gives (4096), which is
	// what puts the boundary after two of the rows seeded above.
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
// client (fdb.ModeTargetBytes plus the server's truncation), not by some sim-local convention.
// Batch boundaries are where a continuation lands and where each conflict range ends, so a
// different rule means a different paging and a different conflict shape.
//
// The rule is BYTE-driven: the streaming mode supplies a per-fetch byte target, and the reply
// accumulates rows until key+value+serverRowOverheadBytes reaches it, INCLUDING the row that
// reaches it. So what distinguishes the modes here is how many of these rows fit in 4096, 256
// and 80000 bytes — not a per-mode row page, which no longer exists.
func TestCursorBatchSizesFollowTheStreamingMode(t *testing.T) {
	t.Parallel()
	keys := []string{
		"k0", "k1", "k2", "k3", "k4", "k5", "k6", "k7", "k8", "k9",
		"ka", "kb", "kc", "kd", "ke", "kf",
	}
	// FAT rows, so the modes' byte targets actually divide these 16 rows differently. The keys
	// are 2 bytes and lazyValueLen is 2048, so one row costs 2+2048+24 = 2074 bytes against a
	// reply's budget, with the row that REACHES the budget included. With the 1-byte values this
	// fixture used to carry a row cost 27 bytes and every mode swallowed all 16 rows in its
	// first fetch — the arms became indistinguishable and the test stopped saying anything about
	// the mode.
	var kvs []string
	for _, key := range keys {
		kvs = append(kvs, key, fatValue("v"))
	}

	// Every expected division below is derived from the mode's byte target and the 2074-byte row
	// cost, never read back off a run:
	//
	//	ITERATOR, targets 4096, 6144, 9216, 13824 (fdb_c.cpp:1006, one per iteration):
	//	  4096: 2074, 4148 -> stops at 2 rows (4148 >= 4096)
	//	  6144: 2074, 4148, 6222 -> 3 rows
	//	  9216: 4*2074 = 8296 < 9216, 5*2074 = 10370 -> 5 rows
	//	  13824: 6 rows are left and cost 12444 < 13824, so the range exhausts first -> 6 rows
	//	SMALL, target 256 (fdb_c.cpp:1002): the FIRST row already costs 2074 >= 256, so every
	//	  fetch carries exactly one row -> sixteen 1-row fetches.
	//	WANT_ALL is rewritten to SERIAL, target 80000: 16*2074 = 33184 < 80000, one fetch.
	for _, tc := range []struct {
		mode  fdb.StreamingMode
		name  string
		batch []int // rows per fetched batch, in order
	}{
		{fdb.StreamingModeIterator, "iterator follows the byte progression", []int{2, 3, 5, 6}},
		{fdb.StreamingModeSmall, "small carries one fat row per fetch", repeatN(1, 16)},
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
// TestCursorBatchSizesFollowTheStreamingMode cannot reach: with 16 rows that scan ends at
// iteration 4, long before the clamp bites, so it is green whether or not the progression is
// bounded.
//
// WHAT SATURATES IS THE BYTE TARGET, NOT A ROW COUNT. libfdb_c gives ITERATOR a per-fetch byte
// target from a fixed table (bindings/c/fdb_c.cpp:1006, "goes 1.5 * previous") and CLAMPS the
// index into it (:1019 `iteration = std::min(iteration, max_iteration)`), so the target stops
// growing at the table's last entry, 120000 bytes, and no fetch ever targets more. Without the
// clamp the target keeps multiplying and every fetch is bigger than the last, so one fetch
// eventually covers the whole remaining range.
//
// Because batch boundaries are where a continuation lands and where each conflict range ends,
// the sim has to saturate at exactly the point the client does — so this pins it on the sim's
// own iterator, through the client's trace surface, the same way the batching rule itself is
// pinned. The client-side counterpart against real FDB is
// fdb:TestRangeIterator_DrainsPastIteratorSaturation.
func TestCursorBatchSizesSaturate(t *testing.T) {
	t.Parallel()

	// rowCost is what one seeded row charges against a reply's byte budget: a 7-byte key
	// ("k" + 6 digits), a 1-byte value, and the server's per-row overhead.
	const rowCost = 7 + 1 + serverRowOverheadBytes // 32

	// saturateTargets is libfdb_c's iteration_progression (fdb_c.cpp:1006) written out here
	// rather than read back from fdb.ModeTargetBytes: a test that asks the implementation what
	// it does agrees with any implementation, including one that never clamps. The LAST entry
	// is the saturated target — from the tenth iteration on, every fetch targets it.
	saturateTargets := []int{4096, 6144, 9216, 13824, 20736, 31104, 46656, 69984, 80000, 120000}

	// Enough rows that the growth phase is followed by SEVERAL fetches at the saturated target.
	// Growth consumes the sum of the first nine targets, 281760 bytes = 8805 rows at 32 bytes
	// each; each saturated fetch then takes 120000/32 = 3750. 25000 rows leaves four full
	// saturated fetches plus a short final one, so the plateau is unmistakable. (The old 6000-row
	// seed drains during growth and never reaches the clamp at all.)
	const nRows = 25000
	var kvs []string
	for i := 0; i < nRows; i++ {
		kvs = append(kvs, fmt.Sprintf("k%06d", i), "v")
	}
	db := New(nil)
	seed(db, kvs...)

	// The expected division, derived from the targets above and rowCost — never read off a run.
	// A reply accumulates rows until the running byte sum REACHES its target, including the row
	// that reaches it, so a target of T admits ceil(T/rowCost) rows; the final fetch is short
	// because the range runs out. That yields
	// [128 192 288 432 648 972 1458 2187 2500 3750 3750 3750 3750 1195].
	var want []int
	for i, remaining := 0, nRows; remaining > 0; i++ {
		target := saturateTargets[min(i, len(saturateTargets)-1)]
		rows := (target + rowCost - 1) / rowCost
		if rows > remaining {
			rows = remaining
		}
		want = append(want, rows)
		remaining -= rows
	}
	// The saturated fetch size, which is also the ceiling on ANY fetch: the clamp is what keeps
	// the eleventh iteration from targeting 180000 bytes and returning 5625 rows.
	wantPeak := (saturateTargets[len(saturateTargets)-1] + rowCost - 1) / rowCost // 3750

	tx := db.newTxn()
	it := tx.GetRange(fdb.KeyRange{Begin: k("k"), End: k("l")},
		fdb.RangeOptions{Mode: fdb.StreamingModeIterator}).Iterator()
	var got []int
	it.SetTraceLog(func(_, _, returned int, _ bool, _ error) {
		if returned == 0 {
			return // a trailing empty fetch returns no rows
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
	if peak > wantPeak {
		t.Fatalf("peak batch = %d rows, want <= %d — the ITERATOR byte target must SATURATE at "+
			"the progression's last entry (%d bytes, fdb_c.cpp:1019); an unclamped target keeps "+
			"multiplying by 1.5 and every fetch outgrows the last. batches were %v",
			peak, wantPeak, saturateTargets[len(saturateTargets)-1], got)
	}
	if peak != wantPeak {
		t.Fatalf("peak batch = %d rows, want exactly %d — the target must still GROW up to the "+
			"clamp; batches were %v", peak, wantPeak, got)
	}
	// And it must have grown to get there: a client that targeted the saturated value from
	// iteration 1 would satisfy the two checks above while skipping the progression entirely.
	// The first fetch targets 4096 bytes, well under a thirtieth of the peak.
	if len(got) < 2 || got[0] >= peak {
		t.Fatalf("first fetch returned %d rows against a peak of %d — the progression starts at "+
			"%d bytes and grows 1.5x per iteration, so a first fetch already at the peak means "+
			"the iteration number never reached the target derivation; batches were %v",
			got[0], peak, saturateTargets[0], got)
	}
	// At least three fetches must sit AT the saturated size, excluding the short final one: that
	// plateau is the clamp made observable, and it is what a still-growing progression cannot
	// produce.
	const wantSaturatedFetches = 3
	saturated := 0
	for i, n := range got {
		if i == len(got)-1 {
			continue // the range ran out here; it is not evidence either way about the clamp
		}
		if n == wantPeak {
			saturated++
		}
	}
	if saturated < wantSaturatedFetches {
		t.Fatalf("only %d fetch(es) sat at the saturated size of %d rows, want >= %d — without "+
			"the clamp no plateau ever forms; batches were %v",
			saturated, wantPeak, wantSaturatedFetches, got)
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
	t.Logf("MEASURED iterator division over %d rows: %v (peak %d, %d at the plateau)",
		nRows, got, peak, saturated)
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

// TestCursorPageWithBufferedClearsCostsThePage pins the widening loop's exit condition.
//
// A bounded view build cannot know in advance how many of the rows the store counted will
// survive the merge, so a buffered clear leaves the page short and the budget has to widen.
// The trap is what it widens UNTIL: comparing len(view) against the current, already-doubled
// budget is never satisfied while a clear keeps removing rows, so the loop widens all the way
// to the end of the range. One clear made a 1024-row page over 10k rows examine 25,360 store
// rows; clears spread through a saturated drain restore the quadratic drain the bound was
// added to remove, and make it slower than the unbounded one-pass build it replaced.
//
// The exit has to be "the requested page is filled" — budget headroom past `want` is not
// information the caller asked for. TestCursorBoundedViewMatchesUnbounded is the correctness
// half of this (it drains with clears and a clear RANGE in the buffer and compares against the
// unbounded build); this is the cost half.
func TestCursorPageWithBufferedClearsCostsThePage(t *testing.T) {
	t.Parallel()

	const nRows = 10000
	const page = 1024
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
			// Clears spread through the range so EVERY page hits the shortfall, which is
			// the case that compounds across a drain.
			for i := 0; i < nRows; i += 500 {
				tx.Clear(fdb.Key(fmt.Sprintf("k%06d", i)))
			}

			it := tx.GetRange(fdb.KeyRange{Begin: k("k"), End: k("l")},
				fdb.RangeOptions{Mode: fdb.StreamingModeIterator, Reverse: reverse}).Iterator()

			// Drain into the saturated region, then measure ONE page in isolation.
			rows := 0
			for rows < 3000 && it.Advance() {
				rows++
			}
			before := tx.viewRowsTouched
			target := rows + page
			for rows < target && it.Advance() {
				rows++
			}
			touched := tx.viewRowsTouched - before

			// A shortfall costs at most a geometric widening or two, never the tail. The
			// budget-comparing exit examines thousands of rows here instead.
			if touched > 6*page {
				t.Fatalf("%s: resolving a %d-row page with buffered clears touched %d store "+
					"rows over a %d-row range — a clear-driven shortfall must cost a "+
					"widening, not the whole tail. The loop has to stop once the REQUESTED "+
					"page is filled; comparing against the doubled budget never terminates "+
					"early while a clear keeps removing rows.",
					name, page, touched, nRows)
			}
		})
	}
}
