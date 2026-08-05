package executor

import (
	"bytes"
	"context"
	"testing"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer"
)

// TestDistinctForwardedInnerStaysNameable pins that a token named by bytes the
// page FORWARDED — rather than adopted — survives the sweep.
//
// THE SHAPE. FlatMap resumes with a pending inner continuation (the saved inner
// position of the outer row the previous page stopped inside). The inner cursor
// is built only when the outer re-delivers that row, so an outer that stops
// out-of-band BEFORE re-reaching it never constructs the DISTINCT at all:
// wrapOuterContinuation forwards the pending inner BYTES verbatim into this
// page's continuation. Nothing adopted the token, so the sweep — whose ground
// truth is adoption — judged the entry unreachable and retired it, while the
// continuation the statement kept still names it. The next page then fails hard
// with "...which this execution's scratch does not hold".
//
// Retirement follows NAMEABILITY, and a forwarded name is a name.
func TestDistinctForwardedInnerStaysNameable(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const outerRows, innerRows = 3, 4
	// Stop mid-inner of the SECOND outer row: the first row's inner is fully
	// drained, so the saved outer position is a real one and the resumed page
	// has an outer row to re-reach (and to stop before).
	const perPage = innerRows + 2
	evalCtx, plan := flatMapOverDistinct(t, outerRows, innerRows)
	scratch := NewExecutionScratch()
	evalCtx = evalCtx.WithExecutionScratch(scratch)
	state := recordlayer.NewExecuteState(1 << 30)

	// Page 1: stop mid-inner, so the continuation carries a DISTINCT token.
	scratch.BeginPage()
	props := recordlayer.DefaultExecuteProperties()
	props.State = state
	cursor, err := ExecutePlan(ctx, plan, nil, evalCtx, nil, props.WithReturnedRowLimit(perPage))
	if err != nil {
		t.Fatalf("page 1: %v", err)
	}
	rows := 0
	var last recordlayer.RecordCursorResult[QueryResult]
	for {
		res, nerr := cursor.OnNext(ctx)
		if nerr != nil {
			t.Fatalf("page 1 OnNext: %v", nerr)
		}
		last = res
		if !res.HasNext() {
			break
		}
		rows++
	}
	c1, err := last.GetContinuation().ToBytes()
	if err != nil {
		t.Fatalf("page 1 ToBytes: %v", err)
	}
	_ = cursor.Close()
	scratch.SweepAfterPage(c1 == nil)
	if c1 == nil {
		t.Fatal("page 1 exhausted the plan; the shape needs a mid-inner page boundary")
	}
	var fmc1 gen.FlatMapContinuation
	if uerr := fmc1.UnmarshalVT(c1); uerr != nil {
		t.Fatalf("decode page 1 continuation: %v", uerr)
	}
	var dhc gen.DistinctHashContinuation
	if uerr := dhc.UnmarshalVT(fmc1.GetInnerContinuation()); uerr != nil {
		t.Fatalf("decode page 1 inner continuation: %v", uerr)
	}
	if dhc.GetStateToken() == 0 {
		t.Fatal("page 1's inner continuation names no scratch token; the shape no longer " +
			"exercises the by-reference carry")
	}

	// Page 2: the outer stops out-of-band BEFORE re-delivering the saved row, so
	// the pending inner is forwarded without ever being adopted.
	scratch.BeginPage()
	props2 := recordlayer.DefaultExecuteProperties()
	props2.State = state
	resumed, err := ExecutePlan(ctx, plan, nil, evalCtx, c1, props2)
	if err != nil {
		t.Fatalf("page 2: %v", err)
	}
	fm, ok := resumed.(*flatMapCursor)
	if !ok {
		t.Fatalf("ExecutePlan returned %T, want *flatMapCursor", resumed)
	}
	if !fm.hasPendingInner {
		t.Fatal("resumed FlatMap carries no pending inner; the shape no longer applies")
	}
	_ = fm.outerCursor.Close()
	// The outer reports the position page 1 saved and no row — a scan/time limit
	// consumed before the saved outer row came back.
	fm.outerCursor = &stallInner{cont: recordlayer.NewBytesContinuation(fmc1.GetOuterContinuation())}
	res, err := fm.OnNext(ctx)
	if err != nil {
		t.Fatalf("page 2 OnNext: %v", err)
	}
	if res.HasNext() {
		t.Fatal("page 2 produced a row; the stalled outer must yield none")
	}
	c2, err := res.GetContinuation().ToBytes()
	if err != nil {
		t.Fatalf("page 2 ToBytes: %v", err)
	}
	_ = fm.Close()
	scratch.SweepAfterPage(false)

	var fmc2 gen.FlatMapContinuation
	if uerr := fmc2.UnmarshalVT(c2); uerr != nil {
		t.Fatalf("decode page 2 continuation: %v", uerr)
	}
	if !bytes.Equal(fmc2.GetInnerContinuation(), fmc1.GetInnerContinuation()) {
		t.Fatalf("page 2 did not forward the pending inner verbatim (%x vs %x); the shape "+
			"no longer exercises a forwarded name",
			fmc2.GetInnerContinuation(), fmc1.GetInnerContinuation())
	}

	// Page 3: the forwarded name is resolved. It must still be held.
	scratch.BeginPage()
	props3 := recordlayer.DefaultExecuteProperties()
	props3.State = state
	page3, err := ExecutePlan(ctx, plan, nil, evalCtx, c2, props3)
	if err != nil {
		t.Fatalf("page 3 ExecutePlan: %v", err)
	}
	for {
		r, nerr := page3.OnNext(ctx)
		if nerr != nil {
			t.Fatalf(
				"resuming a FORWARDED distinct token failed: %v — page 2 never constructed "+
					"the DISTINCT (its outer stopped before re-reaching the saved row), so it "+
					"adopted nothing, but it forwarded the naming bytes into its own "+
					"continuation. Retirement follows NAMEABILITY: a forwarded name is a name",
				nerr,
			)
		}
		if !r.HasNext() {
			break
		}
		rows++
		if !r.GetContinuation().IsEnd() {
			if _, berr := r.GetContinuation().ToBytes(); berr != nil {
				t.Fatalf("page 3 row ToBytes: %v", berr)
			}
		}
	}
	_ = page3.Close()
	if rows != outerRows*innerRows {
		t.Fatalf("the three pages emitted %d rows, want %d", rows, outerRows*innerRows)
	}
}
