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
//   - EXACT carries no BYTE target — mode_bytes_array[EXACT] is BYTE_LIMIT_UNLIMITED
//     (fdb_c.cpp:1002), which fdb.ModeTargetBytes reports as fdb.ByteLimitUnlimited — while the
//     fetch asks for the whole remaining row budget. So nothing truncates the reply and the
//     first fetch asks for every row the caller asked for.
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
// THE PROPERTY IS EMERGENT, NOT ENFORCED. Nothing in the source asserts "EXACT is
// single-batch"; it falls out of several independent sites happening to agree. Each success
// return in the client's rangeScanImpl gates the same way (readpath.go:697, :793, :852), the
// RYW layer gates `more` on the budget being exactly consumed (client/ryw.go:691,701,757,768,790),
// and this package's fetchRange does the same (txn.go:626,673). The unreadable-cap arm is the
// nearest trigger: it ERRORS rather than returning the prefix it read (txn.go:667, mirroring
// ryw.go:689,699), and relaxing that to return the partial prefix would make EXACT multi-fetch
// immediately. None of those sites is pinned, which is why this asserts the emergent property
// directly rather than trusting it to hold.
//
// THIS MATCHES libfdb_c. C's EXACT is single-API-batch too: mode_bytes_array[EXACT] is
// BYTE_LIMIT_UNLIMITED (bindings/c/fdb_c.cpp:1002), so EXACT carries no byte target, and
// C++'s getRange absorbs a byte-capped short reply and re-queries rather than returning it,
// stopping only on limits.isReached() (NativeAPI.actor.cpp:4761, :4814) — for EXACT, the ROW
// budget alone. Measured in libfdbc:TestLibFDBC_ExactModeAbsorbsByteCappedReplies.
//
// WHAT RE-ARMS THIS: one of the sites above starting to return partial results. NOT the
// byte-dimension port booked in TODO.md Phase 12 — an earlier version of this note claimed a
// byte budget would make EXACT multi-fetch, and that is wrong: EXACT's entry in the C table is
// UNLIMITED, so porting the table leaves EXACT row-bounded and single-batch.
func TestExactModeIsStructurallySingleBatch(t *testing.T) {
	t.Parallel()
	db := New(nil)
	const n = 5000
	seedRows(t, db, "exm_", n)

	// CONTROL — non-vacuity. "EXACT took one batch" means nothing unless the same seed
	// demonstrably forces a boundary for a mode that DOES batch. Iterator mode grows a per-fetch
	// BYTE target through iteration_progression, so on this seed (10-byte keys, 1-byte values:
	// 35 bytes a row) it divides as ceil(4096/35)=118, ceil(6144/35)=176, 264, 395, 593, 889,
	// 1334 and a short tail — eight fetches.
	//
	// THE FLOOR IS 2, AND MORE THAN ONE IS THE WHOLE PROPERTY. It used to be ">10", which was a
	// restatement of the removed 2,4,8,...,1024 ROW progression: under a byte target the same
	// 5000 rows come back in fewer, larger fetches, and any floor above 2 would just re-encode
	// a particular division as if it were the invariant. What has to hold is that this seed
	// splits AT ALL — the exact division is pinned by TestBoundedModeReDerivesRemainingBudget
	// and by TestCursorBatchSizesSaturate, on fixtures built for it.
	ctlRows, ctlBatches := drainBatches(t, db, "exm_", "exm`", fdb.RangeOptions{
		Mode: fdb.StreamingModeIterator,
	})
	if ctlRows != n {
		t.Fatalf("control drain returned %d rows, want %d", ctlRows, n)
	}
	if len(ctlBatches) < 2 {
		t.Fatalf("control (Iterator mode, %d rows) took %d batch(es) %v, want at least 2 — the "+
			"seed is too small to force a batch boundary, so the single-batch assertion below "+
			"would be vacuous", n, len(ctlBatches), ctlBatches)
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
// clamped by the leftover budget rather than by the mode: Medium's 1000-byte target holds 29 of
// these rows, so a limit of 250 must divide as eight 29s and a final 18 — that last batch
// truncated by `remaining`, not by the mode. Asserting only the total (250 rows) cannot tell
// that from nine 29s over-reading and discarding, nor from the right division with the
// continuation left one key short.
//
// The division, not just the count, is therefore what is asserted. THIS test separates two
// failure modes: same rows / different division, and an off-by-one at the limit boundary —
// both caught by the exact per-batch counts. It has no row-identity check, so a correct
// division carrying a WRONG CONTINUATION is invisible here; that direction belongs to
// TestBoundedModeMultiBatchRowIdentity, which asserts the rows themselves over a fixture built
// to expose it.
func TestBoundedModeReDerivesRemainingBudget(t *testing.T) {
	t.Parallel()
	db := New(nil)
	const n = 600
	seedRows(t, db, "bnd_", n)

	// THE MODE SIZE IS A BYTE TARGET, so the per-fetch row count is derived, not declared.
	// seedRows lays down 10-byte keys ("bnd_" + 6 digits) with 1-byte values, and a row charges
	// key+value+serverRowOverheadBytes = 35 bytes against a reply's budget, with the row that
	// REACHES the budget included. So:
	//
	//	MEDIUM (1000 bytes, fdb_c.cpp:1002): 28*35 = 980 < 1000, 29*35 = 1015 >= 1000 -> 29 rows
	//	SMALL  ( 256 bytes):                  7*35 = 245 <  256,  8*35 =  280 >=  256 ->  8 rows
	//
	// Those replace the old 100 and 10 row pages. What the test is about is unchanged: the FINAL
	// batch is clamped by the leftover row budget rather than by the mode.
	const mediumRows = 29
	const smallRows = 8
	for _, c := range []struct {
		name  string
		mode  fdb.StreamingMode
		limit int
		want  []int
	}{
		// The final batch is truncated by the leftover budget, not by the mode size:
		// 8*29 = 232, leaving 18 of the 250.
		{"medium/250", fdb.StreamingModeMedium, 250, append(repeatN(mediumRows, 8), 18)},
		// 6*29 = 174 of 200, so the tail is 26 — and the three cases below walk the limit
		// across that boundary one row at a time.
		{"medium/200", fdb.StreamingModeMedium, 200, append(repeatN(mediumRows, 6), 26)},
		{"medium/199", fdb.StreamingModeMedium, 199, append(repeatN(mediumRows, 6), 25)},
		{"medium/201", fdb.StreamingModeMedium, 201, append(repeatN(mediumRows, 6), 27)},
		// A different mode target, to prove the clamp follows the mode and not a constant:
		// 3*8 = 24 of 25, leaving 1.
		{"small/25", fdb.StreamingModeSmall, 25, append(repeatN(smallRows, 3), 1)},
		// Budget exceeds the data: the range exhausts before the limit does. 600 rows divide
		// evenly into 8-row replies, so this ends after exactly 75 fetches with NO trailing
		// empty one.
		//
		// THAT TRAILING FETCH USED TO BE HERE AND ITS ABSENCE IS NOW THE INVARIANT. Under a row
		// page the 60th batch filled its 10-row request, so `more` was true, nothing yet proved
		// the range had ended, and a 61st empty fetch discovered exhaustion. Under a byte target
		// `more` is set by the server's truncation, which can only fire when rows were actually
		// left behind: the 75th reply is not truncated (its 8 rows are all that remain, so the
		// cut lands on the last of them), `more` stays false, and exhaustion is discovered in
		// the same fetch that returns the last rows.
		//
		// Phantom protection for the trailing span did NOT move with it: that final untruncated
		// batch clamps its conflict to the full requested range rather than to the rows it
		// returned, which is what TestDrainedCursorConflictsOverTheWholeRange pins. If a
		// trailing empty fetch ever reappears here, `more` has stopped describing the
		// truncation — check that pairing before adjusting this expectation.
		{"small/1000", fdb.StreamingModeSmall, 1000, repeatN(smallRows, n/smallRows)},
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
