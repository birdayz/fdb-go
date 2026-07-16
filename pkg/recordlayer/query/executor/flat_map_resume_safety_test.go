package executor

import (
	"context"
	"testing"

	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// TestDefaultOnEmpty_DefaultRow_ResumableAndNoReEmit pins F1: executeDefaultOnEmpty
// routes through OrElse (Java RecordQueryDefaultOnEmptyPlan.executePlan →
// RecordCursor.orElse), so the empty-inner default row is emitted with the OrElse
// USE_OTHER continuation — a REAL resumable position — and a resume PAST it does
// NOT re-emit the default. The pre-fix single-result default cursor emitted a nil
// StartContinuation, so a paging loop re-emitted the default forever (never
// advanced) and a mid-stream resume could fabricate a spurious default row.
func TestDefaultOnEmpty_DefaultRow_ResumableAndNoReEmit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// Empty inner (Explode over a nil collection) + a NULL default value.
	inner := plans.NewRecordQueryExplodePlan(nil)
	p := plans.NewRecordQueryDefaultOnEmptyPlan(inner, values.NewNullValue(values.UnknownType))

	cur, err := executeDefaultOnEmpty(ctx, p, nil, EmptyEvaluationContext(), nil, recordlayer.ExecuteProperties{})
	if err != nil {
		t.Fatalf("executeDefaultOnEmpty: %v", err)
	}
	res, err := cur.OnNext(ctx)
	if err != nil {
		t.Fatalf("OnNext: %v", err)
	}
	if !res.HasNext() {
		t.Fatal("empty inner must emit the default row")
	}
	cont := res.GetContinuation()
	if cont == nil {
		t.Fatal("default row must carry a resumable continuation, got nil (a paging loop never advances)")
	}
	contBytes, err := cont.ToBytes()
	if err != nil {
		t.Fatalf("continuation ToBytes: %v", err)
	}
	if len(contBytes) == 0 {
		t.Fatal("default row continuation must be non-empty/resumable, got empty (paging never advances)")
	}
	_ = cur.Close()

	// Resume from the default row's continuation: the default must NOT re-emit.
	cur2, err := executeDefaultOnEmpty(ctx, p, nil, EmptyEvaluationContext(), contBytes, recordlayer.ExecuteProperties{})
	if err != nil {
		t.Fatalf("resume executeDefaultOnEmpty: %v", err)
	}
	defer func() { _ = cur2.Close() }()
	res2, err := cur2.OnNext(ctx)
	if err != nil {
		t.Fatalf("resume OnNext: %v", err)
	}
	if res2.HasNext() {
		t.Fatalf("resume past the default re-emitted a spurious row: %+v", res2.GetValue())
	}
}

// (The former TestFlatMapLeftOuter_ResumedMidInner_NoSpuriousNull pinned the F2
// spurious-null resume bug on the cursor's in-memory leftOuter/innerHadMatch flag
// pair. That flag pair was dead in production — LEFT OUTER lowers to
// DefaultOnEmpty/OrElse, whose serialized USE_INNER state carries the decision
// resume-safely (pinned by TestDefaultOnEmpty_DefaultRow_ResumableAndNoReEmit
// above) — and has been deleted along with this test.)

// TestFlatMap_CheckValueMismatch_RestartsNotErrors pins F24(1): on a check-value
// mismatch (outer row changed/deleted between transactions), the FlatMap must
// DISCARD the stale inner continuation and rescan the inner from the start for the
// new outer — matching Java FlatMapPipelinedCursor (:210-220) — rather than
// hard-erroring "outer row changed". A legitimately-resumed page (or a deleted
// outer) must not abort.
func TestFlatMap_CheckValueMismatch_RestartsNotErrors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	outerRow := QueryResult{PrimaryKey: tuple.Tuple{int64(7)}}
	c := &flatMapCursor{
		outerCursor:       recordlayer.FromList([]QueryResult{outerRow}),
		innerPlan:         plans.NewRecordQueryExplodePlan(nil), // empty
		store:             nil,
		evalCtx:           EmptyEvaluationContext(),
		outerAlias:        values.NamedCorrelationIdentifier("O"),
		innerAlias:        values.NamedCorrelationIdentifier("I"),
		resultValue:       values.LiteralValue(int64(1)),
		props:             recordlayer.ExecuteProperties{},
		initialInnerCont:  []byte("stale"),
		hasPendingInner:   true,
		pendingCheckValue: tuple.Tuple{int64(999)}.Pack(), // MISMATCH vs PK=7
	}
	defer func() { _ = c.Close() }()

	res, err := c.OnNext(ctx)
	if err != nil {
		t.Fatalf("check-value mismatch must RESTART the inner (Java), not error: %v", err)
	}
	if res.HasNext() {
		t.Fatalf("empty inner (non-left-outer) must exhaust, got a row: %+v", res.GetValue())
	}
}

// TestFlatMapBuildContinuation_InnerExhausted_OmitsCheckValue pins F24(2): the
// inner-exhausted (advanced-outer) branch of buildContinuation must NOT write a
// check value. Java's Continuation.toByteString sets the check value ONLY
// alongside the inner continuation (the mid-inner branch). Writing it in the
// advanced-outer branch mismatches DETERMINISTICALLY on resume — the resumed
// outer is the NEXT row, whose PK differs from the exhausted row's PK.
func TestFlatMapBuildContinuation_InnerExhausted_OmitsCheckValue(t *testing.T) {
	t.Parallel()

	c := &flatMapCursor{
		priorOuterContinuation: recordlayer.NewBytesContinuation([]byte("PRIOR")),
		lastOuterContinuation:  recordlayer.NewBytesContinuation([]byte("LAST")),
		currentOuter:           &QueryResult{PrimaryKey: tuple.Tuple{int64(3)}},
	}
	b, err := c.buildContinuation(&recordlayer.EndContinuation{}).ToBytes()
	if err != nil {
		t.Fatalf("ToBytes: %v", err)
	}
	var fmc gen.FlatMapContinuation
	if err := proto.Unmarshal(b, &fmc); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(fmc.CheckValue) != 0 {
		t.Fatalf("inner-exhausted branch must NOT write a check value (Java writes it only with the inner continuation); got %x — the F24 deterministic-mismatch trap", fmc.CheckValue)
	}
	if string(fmc.OuterContinuation) != "LAST" {
		t.Fatalf("inner-exhausted must advance to lastOuterContinuation, got %q", fmc.OuterContinuation)
	}
	if len(fmc.InnerContinuation) != 0 {
		t.Fatalf("inner-exhausted must not encode an inner continuation, got %q", fmc.InnerContinuation)
	}
}
