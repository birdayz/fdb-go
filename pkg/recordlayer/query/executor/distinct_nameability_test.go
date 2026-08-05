package executor

import (
	"context"
	"testing"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// The scratch's retirement rule is NAMEABILITY: an entry must live exactly as
// long as some continuation the statement still holds can name its token. Three
// earlier designs keyed retirement on a CURSOR lifecycle event instead — park,
// page-local marking, exhaustion — and each was wrong in its own way. These two
// tests are the counterexamples that killed the last of them, kept so the
// mistake cannot be re-made.

// TestDistinctAggregateFinalRowStaysResumable pins that EXHAUSTION DOES NOT
// IMPLY UN-NAMEABILITY.
//
// aggregateCursor.emitFinal emits its final group carrying
// previousContinuationInGroup — a RETAINED EARLIER inner continuation
// (streaming_cursors.go:393-401). So a token minted before the inner exhausted
// is still named by a row continuation the caller legitimately holds AFTER the
// inner has ended. Retiring the entry when the exhausted cursor closed turned
// that valid continuation into a hard "seen-set not held" error.
func TestDistinctAggregateFinalRowStaysResumable(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const n = 6
	evalCtx, distinctPlan := distinctFixture(t, n, n)
	scratch := NewExecutionScratch()
	evalCtx = evalCtx.WithExecutionScratch(scratch)

	props := recordlayer.DefaultExecuteProperties()
	props.State = recordlayer.NewExecuteState(1 << 30)
	innerCursor, err := ExecutePlan(ctx, distinctPlan, nil, evalCtx, nil, props)
	if err != nil {
		t.Fatalf("inner: %v", err)
	}
	agg := newAggregateCursor(
		innerCursor,
		nil, // scalar aggregate: one final row, carrying the retained inner position
		[]expressions.AggregateSpec{{
			Function: expressions.AggCount,
			Operand:  values.NewFieldValueWithResolvedOrdinal("V", 0, values.NotNullInt),
			Alias:    "C",
		}},
		distinctPlan,
		evalCtx,
	)
	var finalCont []byte
	for {
		res, nerr := agg.OnNext(ctx)
		if nerr != nil {
			t.Fatalf("agg OnNext: %v", nerr)
		}
		if !res.HasNext() {
			break
		}
		b, berr := res.GetContinuation().ToBytes()
		if berr != nil {
			t.Fatalf("agg ToBytes: %v", berr)
		}
		finalCont = b
	}
	// Close the aggregate — which closes the exhausted DISTINCT beneath it.
	_ = agg.Close()
	if finalCont == nil {
		t.Fatal("aggregate produced no final-row continuation; the shape no longer applies")
	}

	var ac gen.AggregateCursorContinuation
	if uerr := ac.UnmarshalVT(finalCont); uerr != nil {
		t.Fatalf("decode aggregate continuation: %v", uerr)
	}
	innerBytes := ac.GetContinuation()
	if innerBytes == nil {
		t.Fatal("aggregate final row carries no inner position; the shape no longer applies")
	}

	resumeProps := recordlayer.DefaultExecuteProperties()
	resumeProps.State = recordlayer.NewExecuteState(1 << 30)
	resumed, rerr := ExecutePlan(ctx, distinctPlan, nil, evalCtx, innerBytes, resumeProps)
	if rerr != nil {
		t.Fatalf(
			"resuming the aggregate's final-row continuation failed: %v — the DISTINCT "+
				"beneath it had exhausted and closed, but its token is still NAMED by that "+
				"row's continuation. Exhaustion does not prove un-nameability, so retirement "+
				"must not key on it",
			rerr,
		)
	}
	_ = resumed.Close()
}

// TestDistinctPagedFlatMapInnerRetires pins the other half: a RESUMED inner's
// adopted entry must retire too.
//
// A cursor retires nothing itself, so a rule that discarded only the entry a
// cursor PARKED left every entry it ADOPTED behind. Paging a FlatMap over a
// DISTINCT resumes an inner on most pages, so those adopted entries accumulated
// — measured 12 live after a 12-outer-row drain in 3-row pages. Retiring at the
// page boundary covers both, because it asks about nameability rather than
// about which cursor happened to create the entry.
func TestDistinctPagedFlatMapInnerRetires(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const outerRows, innerRows, perPage = 12, 4, 3
	evalCtx, plan := flatMapOverDistinct(t, outerRows, innerRows)
	scratch := NewExecutionScratch()
	evalCtx = evalCtx.WithExecutionScratch(scratch)

	var cont []byte
	rows := 0
	for page := 0; page < 500; page++ {
		scratch.BeginPage()
		props := recordlayer.DefaultExecuteProperties()
		props.State = recordlayer.NewExecuteState(1 << 30)
		props = props.WithReturnedRowLimit(perPage)
		cursor, err := ExecutePlan(ctx, plan, nil, evalCtx, cont, props)
		if err != nil {
			t.Fatalf("page %d: %v", page, err)
		}
		var last recordlayer.RecordCursorResult[QueryResult]
		for {
			res, nerr := cursor.OnNext(ctx)
			if nerr != nil {
				t.Fatalf("page %d OnNext: %v", page, nerr)
			}
			last = res
			if !res.HasNext() {
				break
			}
			rows++
			// Per-row serialization: the LIMIT envelope's exact behaviour, and
			// what makes each inner cursor park.
			if _, berr := res.GetContinuation().ToBytes(); berr != nil {
				t.Fatalf("row ToBytes: %v", berr)
			}
		}
		var b []byte
		if c := last.GetContinuation(); c != nil && !c.IsEnd() {
			var berr error
			if b, berr = c.ToBytes(); berr != nil {
				t.Fatalf("page ToBytes: %v", berr)
			}
		}
		_ = cursor.Close()
		scratch.SweepAfterPage(b == nil)
		if b == nil {
			break
		}
		cont = b
	}
	if rows != outerRows*innerRows {
		t.Fatalf("paged drain emitted %d rows, want %d", rows, outerRows*innerRows)
	}
	if minted := scratch.MintedDistinctSets(); minted < int64(outerRows) {
		t.Fatalf("only %d entries parked over %d outer rows — the shape is no longer "+
			"exercising per-inner-cursor parking", minted, outerRows)
	}
	if live := scratch.LiveDistinctSets(); live != 0 {
		t.Fatalf(
			"%d live scratch states after a paged FlatMap-over-DISTINCT drain (want 0): "+
				"entries a resumed inner ADOPTED are retired by nameability at the page "+
				"boundary, not by whichever cursor created them",
			live,
		)
	}
}

// TestDistinctSweepIsRetrySafe pins that retiring at the page boundary does not
// break the FDB retry loop. The sweep runs INSIDE the page's transaction
// closure, so the commit can still fail retryably afterwards and the retry
// re-executes from the very same continuation — which must still resolve.
//
// This is the corner that the park-time eviction got wrong once already; the
// sweep is a different mechanism and needs its own proof, not an argument by
// analogy.
func TestDistinctSweepIsRetrySafe(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const n, distinct, perPage = 40, 8, 3
	evalCtx, plan := distinctFixture(t, n, distinct)
	scratch := NewExecutionScratch()
	evalCtx = evalCtx.WithExecutionScratch(scratch)

	page := func(cont []byte) []byte {
		t.Helper()
		scratch.BeginPage()
		props := recordlayer.DefaultExecuteProperties()
		props.State = recordlayer.NewExecuteState(1 << 30)
		props = props.WithReturnedRowLimit(perPage)
		cursor, err := ExecutePlan(ctx, plan, nil, evalCtx, cont, props)
		if err != nil {
			t.Fatalf("ExecutePlan: %v", err)
		}
		var last recordlayer.RecordCursorResult[QueryResult]
		for {
			res, nerr := cursor.OnNext(ctx)
			if nerr != nil {
				t.Fatalf("OnNext: %v", nerr)
			}
			last = res
			if !res.HasNext() {
				break
			}
		}
		var b []byte
		if c := last.GetContinuation(); c != nil && !c.IsEnd() {
			var berr error
			if b, berr = c.ToBytes(); berr != nil {
				t.Fatalf("ToBytes: %v", berr)
			}
		}
		_ = cursor.Close()
		scratch.SweepAfterPage(b == nil)
		return b
	}

	c1 := page(nil)
	if c1 == nil {
		t.Fatal("page 1 produced no continuation")
	}
	// Page 2 runs and sweeps — and then its COMMIT fails retryably, so the
	// retry re-executes from c1.
	if c2 := page(c1); c2 == nil {
		t.Fatal("page 2 produced no continuation")
	}
	if again := page(c1); again == nil {
		t.Fatal("the retry of page 2 produced no continuation")
	}
	// And once more from the ORIGINAL, since a retry can happen twice.
	if again := page(c1); again == nil {
		t.Fatal("the second retry of page 2 produced no continuation")
	}
}

// TestDistinctParkedEntryStaysCharged pins that a parked entry keeps holding
// its bytes against the statement budget after its cursor closes.
//
// The producing cursor hands its page charge to the entry rather than releasing
// it at close. Without that hand-off an accumulating shape is INVISIBLE: the
// entries pile up while the budget reads zero, which is exactly how a
// per-outer-row leak grew silently. Charged, it fails loudly instead.
func TestDistinctParkedEntryStaysCharged(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const n, perPage = 20, 3
	evalCtx, plan := distinctFixture(t, n, n)
	scratch := NewExecutionScratch()
	evalCtx = evalCtx.WithExecutionScratch(scratch)

	state := recordlayer.NewExecuteState(1 << 30)
	scratch.BeginPage()
	props := recordlayer.DefaultExecuteProperties()
	props.State = state
	props = props.WithReturnedRowLimit(perPage)
	cursor, err := ExecutePlan(ctx, plan, nil, evalCtx, nil, props)
	if err != nil {
		t.Fatalf("ExecutePlan: %v", err)
	}
	var last recordlayer.RecordCursorResult[QueryResult]
	for {
		res, nerr := cursor.OnNext(ctx)
		if nerr != nil {
			t.Fatalf("OnNext: %v", nerr)
		}
		last = res
		if !res.HasNext() {
			break
		}
	}
	if _, berr := last.GetContinuation().ToBytes(); berr != nil {
		t.Fatalf("ToBytes: %v", berr)
	}
	_ = cursor.Close()

	if used := state.MemUsed(); used <= 0 {
		t.Fatalf(
			"the statement budget reads %d bytes after a page parked a seen-set and closed: "+
				"a parked entry outlives its cursor, so its bytes must stay charged or an "+
				"accumulating shape grows invisibly", used,
		)
	}
	// Retiring it gives the bytes back.
	scratch.SweepAfterPage(true)
	if used := state.MemUsed(); used != 0 {
		t.Fatalf("budget still holds %d bytes after every entry retired — retirement must "+
			"release what parking charged", used)
	}
}
