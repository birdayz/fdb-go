package simfdb

import (
	"fmt"
	"testing"

	fdb "fdb.dev/pkg/fdbgo/fdb"
)

// drainBatches drains a range iterator to exhaustion and reports the row count plus the
// per-batch returned-row counts observed through the trace hook. The trace hook is the only
// way batch DIVISION is observable at all: two drains can agree on every row and still have
// split them differently, and the split is where a read-conflict range and a cursor
// continuation land.
func drainBatches(t *testing.T, db *SimDB, begin, end string, opts fdb.RangeOptions) (rows int, batches []int) {
	t.Helper()
	tx := db.newTxn()
	it := tx.GetRange(fdb.KeyRange{Begin: k(begin), End: k(end)}, opts).Iterator()
	it.SetTraceLog(func(iteration, requested, returned int, more bool, err error) {
		batches = append(batches, returned)
	})
	for it.Advance() {
		if _, err := it.Get(); err != nil {
			t.Fatalf("Get: %v", err)
		}
		rows++
	}
	if _, err := it.Get(); err != nil {
		t.Fatalf("drain ended in error: %v", err)
	}
	return rows, batches
}

// repeatN returns a slice of count copies of v — the expected division of a drain that runs
// at a constant mode-sized batch.
func repeatN(v, count int) []int {
	out := make([]int, count)
	for i := range out {
		out[i] = v
	}
	return out
}

func seedRows(t *testing.T, db *SimDB, prefix string, n int) {
	t.Helper()
	if _, err := db.Transact(func(tx fdb.WritableTransaction) (any, error) {
		for i := 0; i < n; i++ {
			tx.Set(fdb.Key(fmt.Sprintf("%s%06d", prefix, i)), []byte("v"))
		}
		return nil, nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

// TestExactModeIsStructurallySingleBatch pins a NEGATIVE result: StreamingModeExact cannot
// span a batch boundary, at ANY result size. There is no multi-batch exact-mode path to
// verify — not an under-tested one, an unreachable one.
//
// Two facts compose to make it unreachable, and the test drives both:
//
//   - batchSize(Exact, _, remaining) returns the WHOLE remaining budget
//     (fdb/range_result.go:199-200), so the first fetch asks for every row the caller asked
//     for. There is no per-iteration progression to re-derive.
//   - a fetch never returns SHORT with more pending. The pure-Go client's rangeScanImpl
//     (client/readpath.go:669) loops internally across shards until the row budget is filled
//     — it returns either a filled budget with more=true, or an exhausted range with
//     more=false. The sim's fetchRange (simfdb/txn.go:641) has the same property: its `more`
//     is precisely "the limit was filled".
//
// So the first fetch either fills the budget (remaining hits 0 and Advance stops) or exhausts
// the range (exhausted=true). Either way: exactly one batch. The `remaining -= len(kvs)` and
// keyAfter(lastKey) re-derivation in the iterator is live code, but EXACT never reaches it —
// the bounded modes do, which is what TestBoundedModeReDerivesRemainingBudget covers.
//
// WHAT RE-ARMS THIS: if EXACT ever takes more than one batch, a byte-dimension budget has
// been introduced (a GetRangeLimits{rows,bytes} port, per the batchSize ITERATOR note), and
// EXACT's batch DIVISION becomes observable behaviour that libfdb_c is the spec for. At that
// point a final-row-set comparison against the C client is no longer sufficient and a
// per-batch differential is required. This test failing is that signal, not a nuisance.
func TestExactModeIsStructurallySingleBatch(t *testing.T) {
	t.Parallel()
	db := New(nil)
	const n = 5000
	seedRows(t, db, "exm_", n)

	// CONTROL — non-vacuity. "EXACT took one batch" means nothing unless the same seed
	// demonstrably forces boundaries for a mode that DOES batch. Iterator mode is
	// 2,4,8,...,1024 saturating, so 5000 rows must take well over ten batches. Without this
	// arm a seed too small to ever split would pass the assertion below while proving nothing.
	ctlRows, ctlBatches := drainBatches(t, db, "exm_", "exm`", fdb.RangeOptions{
		Mode: fdb.StreamingModeIterator,
	})
	if ctlRows != n {
		t.Fatalf("control drain returned %d rows, want %d", ctlRows, n)
	}
	if len(ctlBatches) < 10 {
		t.Fatalf("control (Iterator mode, %d rows) took %d batches %v, want >10 — the seed is "+
			"too small to force a batch boundary, so the single-batch assertion below would be "+
			"vacuous", n, len(ctlBatches), ctlBatches)
	}
	t.Logf("MEASURED control Iterator mode: %d rows in %d batches %v", ctlRows, len(ctlBatches), ctlBatches)

	// SUBJECT — the same range, the same row count, EXACT mode. One batch, every time.
	for _, limit := range []int{1, 2, 1023, 1024, 1025, n - 1, n, n + 1, n * 2} {
		limit := limit
		want := min(limit, n)
		rows, batches := drainBatches(t, db, "exm_", "exm`", fdb.RangeOptions{
			Mode:  fdb.StreamingModeExact,
			Limit: limit,
		})
		if rows != want {
			t.Errorf("Exact limit=%d returned %d rows, want %d", limit, rows, want)
		}
		if len(batches) != 1 {
			t.Errorf("Exact limit=%d took %d batches %v, want exactly 1 — EXACT asks for the "+
				"whole remaining budget in one fetch and a fetch never returns short with more "+
				"pending, so a second batch means a byte-dimension budget now exists and EXACT's "+
				"batch division needs a per-batch differential against libfdb_c", limit, len(batches), batches)
			continue
		}
		if batches[0] != want {
			t.Errorf("Exact limit=%d: batch[0] returned %d rows, want %d (the whole result)",
				limit, batches[0], want)
		}
		t.Logf("MEASURED Exact limit=%-5d -> %d rows in %d batch %v", limit, rows, len(batches), batches)
	}
}

// TestBoundedModeReDerivesRemainingBudget is the POSITIVE half, and the one that actually
// exercises the per-batch limit re-derivation the exact-mode path never reaches.
//
// A bounded streaming mode combined with a row limit is the only shape where a LATER batch is
// clamped by the leftover budget rather than by the mode: Medium fetches 100 at a time, so a
// limit of 250 must divide as 100,100,50 — the final batch truncated by `remaining`, not by
// the mode's own size. Asserting only the total (250 rows) cannot tell that from 100,100,100
// over-reading and discarding, nor from 100,100,50 with the continuation left one key short.
//
// The division, not just the count, is therefore what is asserted. The failure modes this
// separates: same rows / different division; a correct division with a wrong continuation
// (caught by the row IDENTITY check, which a mis-advanced begin key would perturb); and an
// off-by-one at the limit boundary (caught by the exact per-batch counts).
func TestBoundedModeReDerivesRemainingBudget(t *testing.T) {
	t.Parallel()
	db := New(nil)
	const n = 600
	seedRows(t, db, "bnd_", n)

	for _, c := range []struct {
		name  string
		mode  fdb.StreamingMode
		limit int
		want  []int
	}{
		// The final batch is truncated by the leftover budget, not by the mode size.
		{"medium/250", fdb.StreamingModeMedium, 250, []int{100, 100, 50}},
		// Limit lands exactly on a mode boundary: no short final batch at all.
		{"medium/200", fdb.StreamingModeMedium, 200, []int{100, 100}},
		// Off-by-one either side of that boundary.
		{"medium/199", fdb.StreamingModeMedium, 199, []int{100, 99}},
		{"medium/201", fdb.StreamingModeMedium, 201, []int{100, 100, 1}},
		// A different mode size, to prove the clamp follows the mode and not a constant.
		{"small/25", fdb.StreamingModeSmall, 25, []int{10, 10, 5}},
		// Budget exceeds the data: the range exhausts before the limit does, and the drain
		// takes a TRAILING ZERO-ROW FETCH. That empty fetch is deliberate, not an off-by-one:
		// the 60th batch filled its 10-row request, so `more` is true (the sim's more is
		// exactly "the limit was filled") and nothing yet proves the range ended. The 61st
		// fetch is what discovers exhaustion, and it registers the read conflict for the
		// trailing span it scanned and found empty — phantom protection for the tail
		// (simfdb/range_result.go:220-229). Dropping it would let a concurrent insert past the
		// last row go unconflicted.
		//
		// Contrast medium/199 above, which ends [100,99] with NO trailing fetch: there the
		// budget hit zero exactly, so Advance stops on `remaining <= 0` before fetching again.
		// Whether a drain ends on an empty fetch is therefore a function of which of the two
		// bounds ran out first, and both spellings are pinned here.
		{"small/1000", fdb.StreamingModeSmall, 1000, append(repeatN(10, n/10), 0)},
	} {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			want := c.want
			wantRows := 0
			for _, b := range want {
				wantRows += b
			}
			rows, batches := drainBatches(t, db, "bnd_", "bnd`", fdb.RangeOptions{
				Mode: c.mode, Limit: c.limit,
			})
			if rows != wantRows {
				t.Errorf("%s: %d rows, want %d", c.name, rows, wantRows)
			}
			if len(batches) != len(want) {
				t.Fatalf("%s: %d batches %v, want %d %v — the per-batch budget is re-derived "+
					"from the REMAINING limit, so a wrong division here is a mis-split even when "+
					"the total row count is right", c.name, len(batches), batches, len(want), want)
			}
			for i := range want {
				if batches[i] != want[i] {
					t.Errorf("%s: batch[%d] returned %d rows, want %d (full division %v, want %v)",
						c.name, i, batches[i], want[i], batches, want)
				}
			}
			t.Logf("MEASURED %-12s limit=%-5d -> %d rows, division %v", c.name, c.limit, rows, batches)
		})
	}
}

// boundaryStraddlingKeys builds a key list in which the last row of every batchSize-sized batch
// is IMMEDIATELY followed by its own byte successor, key+"\x00".
//
// This layout is the whole point of the arm below, and it is load-bearing rather than
// decorative. The continuation is `scanBegin = keyAfter(lastKey)`, i.e. lastKey+"\x00". With
// evenly-spaced keys like "idn_000009"/"idn_000010" there is a WIDE gap between the two: every
// string from "idn_000009\x00" through "idn_00000~" sorts between them, so a continuation that
// overshoots — advancing two successors instead of one — still lands before the next real row
// and skips NOTHING. A drain over such keys is therefore green under a genuinely broken
// continuation, which was measured: mutating the advance to keyAfter(keyAfter(last)) left an
// evenly-spaced version of this test passing.
//
// Placing a real row exactly at keyAfter(lastKey) removes that slack. The successor position is
// occupied, so an overshoot skips a row that exists and the drain comes up short.
func boundaryStraddlingKeys(batch, total int) []string {
	keys := make([]string, 0, total+total/batch+1)
	for i := 0; len(keys) < total; i++ {
		base := fmt.Sprintf("idn_%06d", i)
		keys = append(keys, base)
		// The key just appended is the LAST row of a batch, so the next row fetched is the
		// first of the following batch — put it at exactly keyAfter(base).
		if len(keys)%batch == 0 && len(keys) < total {
			keys = append(keys, base+"\x00")
		}
	}
	return keys
}

// TestBoundedModeMultiBatchRowIdentity pins the CONTINUATION across every batch boundary: the
// rows a multi-batch drain returns must be exactly the contiguous run the range holds, in
// order, with no gap and no repeat. A continuation that advances too far drops a row at each
// boundary; one that fails to advance repeats a row. Both keep the batch DIVISION correct, so
// the division assertions in TestBoundedModeReDerivesRemainingBudget cannot see either.
//
// Both directions are pinned, and they need different things from the data: the repeat
// direction shows up on any layout, the skip direction only when the successor position is
// occupied — see boundaryStraddlingKeys.
func TestBoundedModeMultiBatchRowIdentity(t *testing.T) {
	t.Parallel()

	for _, c := range []struct {
		mode  fdb.StreamingMode
		batch int // rows per batch for this mode, which fixes where the boundaries fall
	}{
		{fdb.StreamingModeSmall, 10},
		{fdb.StreamingModeMedium, 100},
	} {
		c := c
		t.Run(fmt.Sprintf("mode%d", c.mode), func(t *testing.T) {
			t.Parallel()
			db := New(nil)
			const total = 350
			want := boundaryStraddlingKeys(c.batch, total)
			if _, err := db.Transact(func(tx fdb.WritableTransaction) (any, error) {
				for _, key := range want {
					tx.Set(fdb.Key(key), []byte("v"))
				}
				return nil, nil
			}); err != nil {
				t.Fatalf("seed: %v", err)
			}

			tx := db.newTxn()
			it := tx.GetRange(fdb.KeyRange{Begin: k("idn_"), End: k("idn`")},
				fdb.RangeOptions{Mode: c.mode}).Iterator()
			var got []string
			nBatches := 0
			it.SetTraceLog(func(iteration, requested, returned int, more bool, err error) { nBatches++ })
			for it.Advance() {
				kv, err := it.Get()
				if err != nil {
					t.Fatalf("Get: %v", err)
				}
				got = append(got, string(kv.Key))
			}
			if nBatches < 2 {
				t.Fatalf("took %d batches over %d rows — this arm is vacuous unless the drain "+
					"crosses at least one boundary", nBatches, len(want))
			}
			if len(got) != len(want) {
				t.Fatalf("returned %d rows in %d batches, want %d — a boundary dropped or "+
					"repeated rows", len(got), nBatches, len(want))
			}
			for i := range want {
				if got[i] != want[i] {
					t.Fatalf("row %d = %q, want %q — the continuation across a batch boundary "+
						"is off (a gap or a repeat), which a row-COUNT check alone passes",
						i, got[i], want[i])
				}
			}
			t.Logf("MEASURED mode %d: %d rows across %d batches, contiguous and in order",
				c.mode, len(got), nBatches)
		})
	}
}
