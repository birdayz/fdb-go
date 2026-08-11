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
	"strings"
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
// THE PROPERTY IS EMERGENT, NOT ENFORCED. Nothing asserts "EXACT is single-batch" anywhere in
// the source; it falls out of several independent sites agreeing by construction. Every success
// return in rangeScanImpl gates the same way (readpath.go:697 `false`, :793 `true` only once
// remaining hits 0, :852 `remaining <= 0`), the RYW layer's own returns gate `more` on the
// budget being exactly consumed (client/ryw.go:691,701,757,768,790), and simfdb's fetchRange
// does the same (txn.go:626,673). Change any one of them to hand back a partial prefix and the
// property is gone. The unreadable-cap arm is the closest such trigger: it currently ERRORS
// rather than returning what it read (ryw.go:689,699 and simfdb/txn.go:667), and relaxing that
// to return the prefix would make EXACT multi-fetch the same afternoon. Nothing pins those
// sites, which is exactly why this test asserts the emergent property directly.
//
// THIS MATCHES libfdb_c — it is not a Go quirk being documented. C's EXACT is single-API-batch
// for the same reason: mode_bytes_array[EXACT] is BYTE_LIMIT_UNLIMITED (bindings/c/fdb_c.cpp:1002),
// so EXACT carries no byte target, and C++'s getRange absorbs a byte-capped short reply and
// re-queries rather than returning it — it stops only on limits.isReached()
// (NativeAPI.actor.cpp:4761, :4814), which for EXACT means the ROW budget alone.
// Measured through libfdbc.CGetRangeBatch over 97 KB of rows against an 80000-byte reply cap
// (libfdbc:TestLibFDBC_ExactModeAbsorbsByteCappedReplies): ONE C call with EXACT limit=100
// returns all 100 rows, where SERIAL and WANT_ALL stop at 78 and SMALL at 1.
//
// WHAT RE-ARMS THIS: one of the sites above starting to return partial results. NOT the
// byte-dimension port booked in TODO.md Phase 12 — an earlier version of this note claimed a
// byte budget would make EXACT multi-fetch, and that is wrong: EXACT's entry in the C table is
// UNLIMITED, so porting the table leaves EXACT row-bounded and single-batch. The claim was
// prose asserting a behaviour the code does not have, which is the failure this repo pays for
// most often, so it is corrected here rather than softened.
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
		// The rows are the contiguous prefix of the range. This catches a continuation that
		// FAILS TO ADVANCE (a repeat) while leaving the division intact. It does NOT catch an
		// overshoot: these keys are evenly spaced, so keyAfter(keyAfter(lastKey)) still sorts
		// below the next real row and skips nothing. TestRangeIterator_MultiBatchContinuationIdentity
		// is the arm that covers the overshoot direction, and it needs a different fixture to
		// do it.
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

// boundaryStraddlingKeys builds a key list in which the last row of every batch-sized batch is
// IMMEDIATELY followed by its own byte successor, key+"\x00".
//
// The forward continuation is `ri.begin = keyAfter(lastKey)` (range_result.go:341), i.e.
// lastKey+"\x00". With evenly-spaced keys such as "boundbatch_000099"/"boundbatch_000100"
// there is a WIDE gap between the two: every string from "boundbatch_000099\x00" through
// "boundbatch_00009~" sorts between them, so a continuation that OVERSHOOTS — advancing two
// successors instead of one — still lands below the next real row and skips nothing. A drain
// over such keys is green under a genuinely broken continuation. That blindness was measured
// in the simfdb twin and is why this fixture exists.
//
// Placing a real row at exactly keyAfter(lastKey) removes the slack: the successor position is
// occupied, so an overshoot skips a row that exists and the drain comes up short.
//
// This duplicates simfdb's helper of the same name rather than sharing it: that one lives in
// package simfdb's internal test binary and this is package fdb_test. The duplication is the
// point of the arm — the two continuations are written twice (simfdb/range_result.go:245 and
// range_result.go:341), so a fixture that only exercises the model leaves the line every real
// caller executes unpinned.
func boundaryStraddlingKeys(pfx string, batch, total int) []string {
	keys := make([]string, 0, total+total/batch+1)
	for i := 0; len(keys) < total; i++ {
		base := fmt.Sprintf("%s%06d", pfx, i)
		keys = append(keys, base)
		if len(keys)%batch == 0 && len(keys) < total {
			keys = append(keys, base+"\x00")
		}
	}
	return keys
}

// TestRangeIterator_MultiBatchContinuationIdentity pins BOTH continuation expressions on the
// real client across a batch boundary, in both directions of failure.
//
// The two directions are separate code, not one mechanism seen twice: forward advances
// `ri.begin = keyAfter(lastKey)` (range_result.go:341), reverse retreats `ri.end = lastKey`
// (range_result.go:338) — an exclusive end, so it has no keyAfter and no successor slack.
// Neither had client-side multi-batch coverage.
func TestRangeIterator_MultiBatchContinuationIdentity(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)

	for _, c := range []struct {
		name    string
		mode    gofdb.StreamingMode
		batch   int
		reverse bool
	}{
		{"forward/small", gofdb.StreamingModeSmall, 10, false},
		{"forward/medium", gofdb.StreamingModeMedium, 100, false},
		{"reverse/small", gofdb.StreamingModeSmall, 10, true},
		{"reverse/medium", gofdb.StreamingModeMedium, 100, true},
	} {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			pfx := fmt.Sprintf("contid_%s_", strings.ReplaceAll(c.name, "/", "_"))
			const total = 350
			want := boundaryStraddlingKeys(pfx, c.batch, total)
			const perTx = 200
			for start := 0; start < len(want); start += perTx {
				start := start
				if _, err := db.Transact(func(tr gofdb.WritableTransaction) (any, error) {
					for i := start; i < start+perTx && i < len(want); i++ {
						tr.Set(gofdb.Key(want[i]), []byte("v"))
					}
					return nil, nil
				}); err != nil {
					t.Fatalf("seed [%d,%d): %v", start, start+perTx, err)
				}
			}
			// Reverse yields the same rows in descending order.
			expect := append([]string(nil), want...)
			if c.reverse {
				for i, j := 0, len(expect)-1; i < j; i, j = i+1, j-1 {
					expect[i], expect[j] = expect[j], expect[i]
				}
			}

			keys, batches := drainClientBatches(t, db, pfx, pfx+"~", gofdb.RangeOptions{
				Mode: c.mode, Reverse: c.reverse,
			})
			if len(batches) < 2 {
				t.Fatalf("took %d batches over %d rows — this arm is vacuous unless the drain "+
					"crosses at least one boundary", len(batches), len(want))
			}
			if len(keys) != len(expect) {
				t.Fatalf("returned %d rows in %d batches, want %d — a boundary dropped or "+
					"repeated rows", len(keys), len(batches), len(expect))
			}
			for i := range expect {
				if keys[i] != expect[i] {
					t.Fatalf("row %d = %q, want %q — the continuation across a batch boundary is "+
						"off (a gap or a repeat), which a row-COUNT check alone passes",
						i, keys[i], expect[i])
				}
			}
			t.Logf("MEASURED %-15s %d rows across %d batches, contiguous and in order",
				c.name, len(keys), len(batches))
		})
	}
}
