package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// buildProjectionOverJoin constructs:
//
//	Projection(projVals, Select(rv, [qA, qB], joinPreds, aliases))
func buildProjectionOverJoin(
	projVals []values.Value,
	joinPreds []predicates.QueryPredicate,
	aliases []string,
) *expressions.Reference {
	scanA := expressions.NewFullUnorderedScanExpression([]string{"A"}, values.UnknownType)
	scanAQ := expressions.ForEachQuantifier(expressions.InitialOf(scanA))
	scanB := expressions.NewFullUnorderedScanExpression([]string{"B"}, values.UnknownType)
	scanBQ := expressions.ForEachQuantifier(expressions.InitialOf(scanB))

	rv := values.NewQuantifiedObjectValue(values.UniqueCorrelationIdentifier())
	sel := expressions.NewSelectExpressionWithJoinType(
		rv,
		[]expressions.Quantifier{scanAQ, scanBQ},
		joinPreds,
		aliases,
		expressions.JoinInner,
	)
	selQ := expressions.ForEachQuantifier(expressions.InitialOf(sel))
	proj := expressions.NewLogicalProjectionExpression(projVals, selQ)
	return expressions.InitialOf(proj)
}

func TestPushProjectionBelowJoin_AllColumnsOneSide(t *testing.T) {
	t.Parallel()

	// Project([A.ID, A.NAME]) over Join(A, B) — all columns from side A.
	projVals := []values.Value{
		&values.FieldValue{Field: "A.ID", Typ: values.TypeInt},
		&values.FieldValue{Field: "A.NAME", Typ: values.TypeString},
	}

	ref := buildProjectionOverJoin(projVals, nil, []string{"A", "B"})

	yielded := FireExpressionRule(NewPushProjectionBelowJoinRule(), ref)
	if len(yielded) != 1 {
		t.Fatalf("yielded %d, want 1", len(yielded))
	}

	// Result should be a Projection over a Select.
	newProj, ok := yielded[0].(*expressions.LogicalProjectionExpression)
	if !ok {
		t.Fatalf("yielded %T, want *LogicalProjectionExpression", yielded[0])
	}

	innerSel, ok := newProj.GetInner().GetRangesOver().Get().(*expressions.SelectExpression)
	if !ok {
		t.Fatalf("inner = %T, want *SelectExpression", newProj.GetInner().GetRangesOver().Get())
	}

	qs := innerSel.GetQuantifiers()
	if len(qs) != 2 {
		t.Fatalf("quantifier count %d, want 2", len(qs))
	}

	// Side A should have a pushed projection.
	innerA := qs[0].GetRangesOver().Get()
	projA, ok := innerA.(*expressions.LogicalProjectionExpression)
	if !ok {
		t.Fatalf("quantifier 0 inner = %T, want *LogicalProjectionExpression", innerA)
	}
	if len(projA.GetProjectedValues()) != 2 {
		t.Fatalf("pushed projection A has %d values, want 2", len(projA.GetProjectedValues()))
	}

	// Side B should be untouched (no columns needed from B).
	innerB := qs[1].GetRangesOver().Get()
	if _, ok := innerB.(*expressions.FullUnorderedScanExpression); !ok {
		t.Fatalf("quantifier 1 inner = %T, want *FullUnorderedScanExpression", innerB)
	}
}

func TestPushProjectionBelowJoin_ColumnsBothSides(t *testing.T) {
	t.Parallel()

	// Project([A.ID, B.STATUS]) over Join(A, B) — columns from both sides.
	projVals := []values.Value{
		&values.FieldValue{Field: "A.ID", Typ: values.TypeInt},
		&values.FieldValue{Field: "B.STATUS", Typ: values.TypeString},
	}

	ref := buildProjectionOverJoin(projVals, nil, []string{"A", "B"})

	yielded := FireExpressionRule(NewPushProjectionBelowJoinRule(), ref)
	if len(yielded) != 1 {
		t.Fatalf("yielded %d, want 1", len(yielded))
	}

	newProj, ok := yielded[0].(*expressions.LogicalProjectionExpression)
	if !ok {
		t.Fatalf("yielded %T, want *LogicalProjectionExpression", yielded[0])
	}

	innerSel, ok := newProj.GetInner().GetRangesOver().Get().(*expressions.SelectExpression)
	if !ok {
		t.Fatalf("inner = %T, want *SelectExpression", newProj.GetInner().GetRangesOver().Get())
	}

	qs := innerSel.GetQuantifiers()

	// Both sides should have pushed projections.
	innerA := qs[0].GetRangesOver().Get()
	projA, ok := innerA.(*expressions.LogicalProjectionExpression)
	if !ok {
		t.Fatalf("quantifier 0 inner = %T, want *LogicalProjectionExpression", innerA)
	}
	if len(projA.GetProjectedValues()) != 1 {
		t.Fatalf("pushed projection A has %d values, want 1 (just ID)", len(projA.GetProjectedValues()))
	}

	innerB := qs[1].GetRangesOver().Get()
	projB, ok := innerB.(*expressions.LogicalProjectionExpression)
	if !ok {
		t.Fatalf("quantifier 1 inner = %T, want *LogicalProjectionExpression", innerB)
	}
	if len(projB.GetProjectedValues()) != 1 {
		t.Fatalf("pushed projection B has %d values, want 1 (just STATUS)", len(projB.GetProjectedValues()))
	}
}

func TestPushProjectionBelowJoin_JoinPredicateColumnsPreserved(t *testing.T) {
	t.Parallel()

	// Project([A.NAME]) over Join(A, B, ON A.ID = B.AID)
	// The projection only mentions A.NAME, but the join predicate
	// references A.ID and B.AID — both must be preserved.
	projVals := []values.Value{
		&values.FieldValue{Field: "A.NAME", Typ: values.TypeString},
	}
	joinPred := predicates.NewComparisonPredicate(
		&values.FieldValue{Field: "A.ID", Typ: values.TypeInt},
		predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: &values.FieldValue{Field: "B.AID", Typ: values.TypeInt},
		},
	)

	ref := buildProjectionOverJoin(projVals, []predicates.QueryPredicate{joinPred}, []string{"A", "B"})

	yielded := FireExpressionRule(NewPushProjectionBelowJoinRule(), ref)
	if len(yielded) != 1 {
		t.Fatalf("yielded %d, want 1", len(yielded))
	}

	newProj, ok := yielded[0].(*expressions.LogicalProjectionExpression)
	if !ok {
		t.Fatalf("yielded %T, want *LogicalProjectionExpression", yielded[0])
	}

	innerSel, ok := newProj.GetInner().GetRangesOver().Get().(*expressions.SelectExpression)
	if !ok {
		t.Fatalf("inner = %T, want *SelectExpression", newProj.GetInner().GetRangesOver().Get())
	}

	qs := innerSel.GetQuantifiers()

	// Side A should have a projection with NAME and ID (from join predicate).
	innerA := qs[0].GetRangesOver().Get()
	projA, ok := innerA.(*expressions.LogicalProjectionExpression)
	if !ok {
		t.Fatalf("quantifier 0 inner = %T, want *LogicalProjectionExpression", innerA)
	}
	// Should have both ID and NAME.
	if len(projA.GetProjectedValues()) != 2 {
		t.Fatalf("pushed projection A has %d values, want 2 (ID + NAME)", len(projA.GetProjectedValues()))
	}

	// Side B should have a projection with AID (from join predicate).
	innerB := qs[1].GetRangesOver().Get()
	projB, ok := innerB.(*expressions.LogicalProjectionExpression)
	if !ok {
		t.Fatalf("quantifier 1 inner = %T, want *LogicalProjectionExpression", innerB)
	}
	if len(projB.GetProjectedValues()) != 1 {
		t.Fatalf("pushed projection B has %d values, want 1 (AID)", len(projB.GetProjectedValues()))
	}

	// Verify the B-side projection is "AID".
	bfv, ok := projB.GetProjectedValues()[0].(*values.FieldValue)
	if !ok {
		t.Fatalf("B projection value %T, want *FieldValue", projB.GetProjectedValues()[0])
	}
	if bfv.Field != "AID" {
		t.Fatalf("B projection field = %q, want %q", bfv.Field, "AID")
	}
}

func TestPushProjectionBelowJoin_NoPruningPossible(t *testing.T) {
	t.Parallel()

	// Non-FieldValue projection — computed expression — rule should not fire.
	projVals := []values.Value{
		&values.ConstantValue{Value: 42, Typ: values.TypeInt},
	}

	ref := buildProjectionOverJoin(projVals, nil, []string{"A", "B"})

	yielded := FireExpressionRule(NewPushProjectionBelowJoinRule(), ref)
	if len(yielded) != 0 {
		t.Fatalf("yielded %d, want 0 (non-FieldValue projection can't be pushed)", len(yielded))
	}
}
