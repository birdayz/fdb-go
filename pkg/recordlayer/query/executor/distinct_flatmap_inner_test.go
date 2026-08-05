package executor

import (
	"context"
	"testing"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// flatMapOverDistinct builds FlatMap(outer, Distinct(inner)) — a hash DISTINCT
// as a CORRELATED INNER. FlatMap creates a fresh inner cursor per outer row
// (flat_map_cursor.go:384) and closes it the moment the inner exhausts (:289),
// so this is the shape where per-outer-row scratch entries accumulate.
func flatMapOverDistinct(t *testing.T, outerRows, innerRows int) (*EvaluationContext, plans.RecordQueryPlan) {
	t.Helper()
	evalCtx := EmptyEvaluationContext()
	outerAlias := values.NamedCorrelationIdentifier("fm_outer_" + t.Name())
	innerAlias := values.NamedCorrelationIdentifier("fm_inner_" + t.Name())
	innerSrc := values.NamedCorrelationIdentifier("fm_src_" + t.Name())

	outerTbl := evalCtx.GetOrCreateTempTable(outerAlias, nil)
	for i := 0; i < outerRows; i++ {
		if err := outerTbl.Add(dmap(map[string]any{"O": int64(i)})); err != nil {
			t.Fatalf("seed outer: %v", err)
		}
	}
	innerTbl := evalCtx.GetOrCreateTempTable(innerSrc, nil)
	for i := 0; i < innerRows; i++ {
		if err := innerTbl.Add(dmap(map[string]any{"V": int64(i)})); err != nil {
			t.Fatalf("seed inner: %v", err)
		}
	}
	plan := plans.NewRecordQueryFlatMapPlan(
		plans.NewRecordQueryTempTableScanPlan(outerAlias),
		plans.NewRecordQueryDistinctPlan(plans.NewRecordQueryTempTableScanPlan(innerSrc)),
		outerAlias, innerAlias,
		values.NewQuantifiedObjectValue(innerAlias),
		false,
	)
	return evalCtx, plan
}

// TestDistinctAsFlatMapInnerDoesNotAccumulate pins that a hash DISTINCT used as
// a correlated inner leaves no scratch residue per outer row.
//
// THE DEFECT IT DETECTS: FlatMap builds a fresh inner cursor for every outer
// row, an operator above serializes each emitted row's continuation (the LIMIT
// envelope does exactly this), and each of those inner cursors therefore parks
// an entry. The inner then EXHAUSTS and FlatMap advances the outer, so no
// successor ever adopts that entry — one uncharged seen-set per outer row,
// alive for the whole statement. Measured before the fix: 12 outer rows, 12
// live states. It is uncharged because a page releases its memory charges at
// teardown, so the budget cannot see it either.
//
// The reclaim is keyed on EXHAUSTION, which is what discharges the proof
// obligation: once the inner ends, FlatMap's exhausted-inner branch advances
// the outer and writes NO inner continuation (flat_map_cursor.go:875-883), so
// nothing the statement still holds can name that entry.
func TestDistinctAsFlatMapInnerDoesNotAccumulate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const outerRows, innerRows = 12, 4
	evalCtx, plan := flatMapOverDistinct(t, outerRows, innerRows)
	scratch := NewExecutionScratch()
	evalCtx = evalCtx.WithExecutionScratch(scratch)

	props := recordlayer.DefaultExecuteProperties()
	props.State = recordlayer.NewExecuteState(1 << 30)
	cursor, err := ExecutePlan(ctx, plan, nil, evalCtx, nil, props)
	if err != nil {
		t.Fatalf("ExecutePlan: %v", err)
	}
	rows := 0
	for {
		res, nerr := cursor.OnNext(ctx)
		if nerr != nil {
			t.Fatalf("OnNext: %v", nerr)
		}
		if !res.HasNext() {
			break
		}
		rows++
		// Serialize on EVERY row — the LIMIT envelope's exact behaviour, and
		// what makes each inner cursor park an entry. Without it nothing parks
		// and the shape cannot express the defect.
		if _, berr := res.GetContinuation().ToBytes(); berr != nil {
			t.Fatalf("row %d ToBytes: %v", rows, berr)
		}
	}
	_ = cursor.Close()

	if rows != outerRows*innerRows {
		t.Fatalf("FlatMap emitted %d rows, want %d", rows, outerRows*innerRows)
	}
	if minted := scratch.MintedDistinctSets(); minted < int64(outerRows) {
		t.Fatalf("only %d entries were parked over %d outer rows — the shape is no longer "+
			"parking per inner cursor, so it cannot detect the accumulation it exists for",
			minted, outerRows)
	}
	// The bound is 0, not 2: every inner cursor ran to exhaustion, so every
	// entry is provably unreachable and none should survive.
	if live := scratch.LiveDistinctSets(); live != 0 {
		t.Fatalf(
			"%d live scratch states after %d outer rows (want 0): each correlated inner "+
				"cursor parked an entry that nothing adopts, and they must be reclaimed when "+
				"the exhausted cursor closes or they accumulate per outer row for the whole "+
				"statement, invisible to the memory budget",
			live, outerRows,
		)
	}
}

// TestDistinctClosedMidStreamKeepsItsEntry pins the OTHER direction of the same
// rule, which is the dangerous one: a cursor closed WITHOUT reaching the end
// must KEEP its entry, because the page may have stopped mid-stream and handed
// back a continuation naming it.
//
// Reclaiming unconditionally on Close would look like a tidier rule and would
// silently break every paged resume — the same failure mode as the page sweep
// that was deleted for over-collecting.
func TestDistinctClosedMidStreamKeepsItsEntry(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const n = 12
	evalCtx, plan := distinctFixture(t, n, n)
	scratch := NewExecutionScratch()
	evalCtx = evalCtx.WithExecutionScratch(scratch)

	props := recordlayer.DefaultExecuteProperties()
	props.State = recordlayer.NewExecuteState(1 << 30)
	cursor, err := ExecutePlan(ctx, plan, nil, evalCtx, nil, props.WithReturnedRowLimit(3))
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
	cont, err := last.GetContinuation().ToBytes()
	if err != nil {
		t.Fatalf("continuation: %v", err)
	}
	// Close the page's cursor, exactly as the paging loop does, and only THEN
	// resume — the entry has to have survived that close.
	_ = cursor.Close()
	if cont == nil {
		t.Fatal("mid-stream page produced no continuation")
	}

	resumeProps := recordlayer.DefaultExecuteProperties()
	resumeProps.State = recordlayer.NewExecuteState(1 << 30)
	resumed, err := ExecutePlan(ctx, plan, nil, evalCtx, cont, resumeProps)
	if err != nil {
		t.Fatalf(
			"resuming after the mid-stream page closed failed: %v — a cursor that did NOT "+
				"reach the end must keep its scratch entry; only exhaustion proves nothing "+
				"can name it", err,
		)
	}
	_ = resumed.Close()
}
