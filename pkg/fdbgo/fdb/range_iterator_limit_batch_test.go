package fdb_test

// Per-batch pins for the CLIENT's range iterator (goRangeIterator) under a row Limit.
//
// The sibling arms in pkg/simfdb (TestExactModeIsStructurallySingleBatch,
// TestBoundedModeReDerivesRemainingBudget) assert the same properties against the SIMULATOR's
// iterator, which is a reimplementation of this loop rather than this loop. The two share only
// fdb.BatchSize; the `remaining -= len(kvs)` bookkeeping, the continuation advance and the
// exhaustion latch are written twice. A sim-only pin therefore certifies the model, not the
// client every real caller runs through — so these arms drive the real one against real FDB.

import (
	"fmt"
	"testing"

	gofdb "fdb.dev/pkg/fdbgo/fdb"
)

// seedSequentialRows writes n rows named <pfx>%06d, chunked to stay inside the transaction size
// and lifetime limits, and returns nothing — the keys are recomputable from the index.
func seedSequentialRows(t *testing.T, db gofdb.Database, pfx string, n int) {
	t.Helper()
	const perTx = 500
	for start := 0; start < n; start += perTx {
		start := start
		if _, err := db.Transact(func(tr gofdb.WritableTransaction) (any, error) {
			for i := start; i < start+perTx && i < n; i++ {
				tr.Set(gofdb.Key(fmt.Sprintf("%s%06d", pfx, i)), []byte("v"))
			}
			return nil, nil
		}); err != nil {
			t.Fatalf("seed rows [%d,%d): %v", start, start+perTx, err)
		}
	}
}

// drainClientBatches drains a client range iterator, returning the keys it yielded and the
// per-batch returned-row counts observed through the trace hook.
func drainClientBatches(t *testing.T, db gofdb.Database, begin, end string, opts gofdb.RangeOptions) (keys []string, batches []int) {
	t.Helper()
	if _, err := db.Transact(func(tr gofdb.WritableTransaction) (any, error) {
		tx := tr.(gofdb.Transaction)
		keys, batches = nil, nil
		it := tx.GetRange(gofdb.KeyRange{Begin: gofdb.Key(begin), End: gofdb.Key(end)}, opts).Iterator()
		it.SetTraceLog(func(iteration, requested, returned int, more bool, err error) {
			batches = append(batches, returned)
		})
		for it.Advance() {
			kv, err := it.Get()
			if err != nil {
				return nil, err
			}
			keys = append(keys, string(kv.Key))
		}
		_, err := it.Get()
		return nil, err
	}); err != nil {
		t.Fatalf("drain: %v", err)
	}
	return keys, batches
}

// TestRangeIterator_ExactModeIsStructurallySingleBatch pins a NEGATIVE result on the real
// client: StreamingModeExact cannot span a batch boundary, at any result size.
//
// batchSize(Exact, _, remaining) returns the WHOLE remaining budget (range_result.go:199-200),
// and a fetch never returns short with more pending — rangeScanImpl (client/readpath.go:669)
// loops internally across shards until the row budget is filled, returning either a filled
// budget with more=true or an exhausted range with more=false. So the first fetch either fills
// the budget (remaining hits 0) or exhausts the range. Either way: exactly one batch.
//
// This is why no seed size can force a multi-batch EXACT read here, and why a "make the seed
// big enough to cross a boundary" probe of exact mode measures nothing. The Iterator-mode
// control below is what keeps that claim honest: it shows the SAME seed does cross boundaries,
// many times, so the single-batch result is a property of EXACT and not of a small seed.
//
// WHAT RE-ARMS THIS: EXACT taking more than one batch means a byte-dimension budget now exists
// (a GetRangeLimits{rows,bytes} port, per the batchSize ITERATOR note). At that point EXACT's
// batch DIVISION becomes observable behaviour for which libfdb_c is the spec, and a final
// row-set comparison against the C client stops being sufficient — a per-batch differential
// becomes required. This test failing is that signal.
func TestRangeIterator_ExactModeIsStructurallySingleBatch(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	const pfx = "exactbatch_"
	const n = 5000
	seedSequentialRows(t, db, pfx, n)

	// CONTROL — non-vacuity. Iterator mode is 2,4,8,...,1024 saturating, so this seed must
	// take well over ten batches. Without this arm, a seed too small to ever split would pass
	// the EXACT assertions below while proving nothing at all.
	ctlKeys, ctlBatches := drainClientBatches(t, db, pfx, pfx+"~", gofdb.RangeOptions{
		Mode: gofdb.StreamingModeIterator,
	})
	if len(ctlKeys) != n {
		t.Fatalf("control drain returned %d rows, want %d", len(ctlKeys), n)
	}
	if len(ctlBatches) <= 10 {
		t.Fatalf("control (Iterator mode, %d rows) took %d batches %v, want >10 — the seed does "+
			"not force a batch boundary, so the single-batch assertions below are vacuous",
			n, len(ctlBatches), ctlBatches)
	}
	t.Logf("MEASURED control Iterator mode: %d rows in %d batches %v",
		len(ctlKeys), len(ctlBatches), ctlBatches)

	for _, limit := range []int{1, 1023, 1024, 1025, n - 1, n, n + 1} {
		limit := limit
		want := min(limit, n)
		keys, batches := drainClientBatches(t, db, pfx, pfx+"~", gofdb.RangeOptions{
			Mode:  gofdb.StreamingModeExact,
			Limit: limit,
		})
		if len(keys) != want {
			t.Errorf("Exact limit=%d returned %d rows, want %d", limit, len(keys), want)
		}
		if len(batches) != 1 {
			t.Errorf("Exact limit=%d took %d batches %v, want exactly 1 — EXACT asks for the "+
				"whole remaining budget in one fetch and a fetch never returns short with more "+
				"pending, so a second batch means a byte-dimension budget now exists and EXACT's "+
				"batch division needs a per-batch differential against libfdb_c",
				limit, len(batches), batches)
			continue
		}
		if batches[0] != want {
			t.Errorf("Exact limit=%d: batch[0] returned %d rows, want %d (the whole result)",
				limit, batches[0], want)
		}
		t.Logf("MEASURED Exact limit=%-5d -> %d rows in %d batch %v", limit, len(keys), len(batches), batches)
	}
}

// TestRangeIterator_BoundedModeReDerivesRemainingBudget is the POSITIVE half on the real
// client, and it covers the per-batch limit re-derivation that exact mode never reaches.
//
// A bounded streaming mode combined with a row limit is the only shape where a LATER batch is
// clamped by the leftover budget rather than by the mode: Medium fetches 100 at a time, so a
// limit of 250 must divide 100,100,50 — the final batch truncated by `remaining`, not by the
// mode size. Asserting only the total cannot separate that from 100,100,100 over-reading and
// discarding the surplus, which returns the same 250 rows while issuing a larger final fetch
// and taking a wider read-conflict range than the caller's budget justifies.
func TestRangeIterator_BoundedModeReDerivesRemainingBudget(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	const pfx = "boundbatch_"
	const n = 600
	seedSequentialRows(t, db, pfx, n)

	for _, c := range []struct {
		name  string
		mode  gofdb.StreamingMode
		limit int
		want  []int
	}{
		{"medium/250", gofdb.StreamingModeMedium, 250, []int{100, 100, 50}},
		{"medium/200", gofdb.StreamingModeMedium, 200, []int{100, 100}},
		{"medium/199", gofdb.StreamingModeMedium, 199, []int{100, 99}},
		{"medium/201", gofdb.StreamingModeMedium, 201, []int{100, 100, 1}},
		{"small/25", gofdb.StreamingModeSmall, 25, []int{10, 10, 5}},
	} {
		c := c
		wantRows := 0
		for _, b := range c.want {
			wantRows += b
		}
		keys, batches := drainClientBatches(t, db, pfx, pfx+"~", gofdb.RangeOptions{
			Mode: c.mode, Limit: c.limit,
		})
		if len(keys) != wantRows {
			t.Errorf("%s: %d rows, want %d", c.name, len(keys), wantRows)
		}
		if len(batches) != len(c.want) {
			t.Errorf("%s: %d batches %v, want %d %v — the per-batch budget is re-derived from "+
				"the REMAINING limit, so a wrong division is a mis-split even when the total row "+
				"count is right", c.name, len(batches), batches, len(c.want), c.want)
			continue
		}
		for i := range c.want {
			if batches[i] != c.want[i] {
				t.Errorf("%s: batch[%d] returned %d rows, want %d (division %v, want %v)",
					c.name, i, batches[i], c.want[i], batches, c.want)
			}
		}
		// The rows are the contiguous prefix of the range: a continuation that overshot or
		// failed to advance keeps the division intact while changing WHICH rows come back.
		for i, key := range keys {
			if w := fmt.Sprintf("%s%06d", pfx, i); key != w {
				t.Errorf("%s: row %d = %q, want %q — the continuation across a batch boundary "+
					"is off, which the division check alone passes", c.name, i, key, w)
				break
			}
		}
		t.Logf("MEASURED %-12s limit=%-4d -> %d rows, division %v", c.name, c.limit, len(keys), batches)
	}
}
