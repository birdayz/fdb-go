package cascades_test

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

func TestPushLimitThroughUnion(t *testing.T) {
	t.Parallel()

	scanA := expressions.NewFullUnorderedScanExpression([]string{"A"}, values.UnknownType)
	scanB := expressions.NewFullUnorderedScanExpression([]string{"B"}, values.UnknownType)
	qA := expressions.ForEachQuantifier(expressions.InitialOf(scanA))
	qB := expressions.ForEachQuantifier(expressions.InitialOf(scanB))

	union := expressions.NewLogicalUnionExpression([]expressions.Quantifier{qA, qB})
	unionRef := expressions.InitialOf(union)
	unionQ := expressions.ForEachQuantifier(unionRef)

	limit := expressions.NewLogicalLimitExpression(10, 5, unionQ)
	ref := expressions.InitialOf(limit)

	rule := cascades.NewPushLimitThroughUnionRule()
	results := cascades.FireExpressionRule(rule, ref)
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}

	outerLimit, ok := results[0].(*expressions.LogicalLimitExpression)
	if !ok {
		t.Fatalf("result is %T, want *LogicalLimitExpression", results[0])
	}
	if outerLimit.GetLimit() != 10 || outerLimit.GetOffset() != 5 {
		t.Fatalf("outer limit = %d/%d, want 10/5", outerLimit.GetLimit(), outerLimit.GetOffset())
	}

	innerExpr := outerLimit.GetInner().GetRangesOver().Get()
	innerUnion, ok := innerExpr.(*expressions.LogicalUnionExpression)
	if !ok {
		t.Fatalf("inner is %T, want *LogicalUnionExpression", innerExpr)
	}

	for i, q := range innerUnion.GetQuantifiers() {
		branchExpr := q.GetRangesOver().Get()
		branchLimit, ok := branchExpr.(*expressions.LogicalLimitExpression)
		if !ok {
			t.Fatalf("branch %d is %T, want *LogicalLimitExpression", i, branchExpr)
		}
		if branchLimit.GetLimit() != 15 {
			t.Fatalf("branch %d limit = %d, want 15 (10+5)", i, branchLimit.GetLimit())
		}
		if branchLimit.GetOffset() != 0 {
			t.Fatalf("branch %d offset = %d, want 0", i, branchLimit.GetOffset())
		}
	}
}

func TestPushLimitThroughUnion_NoOffset(t *testing.T) {
	t.Parallel()

	scanA := expressions.NewFullUnorderedScanExpression([]string{"A"}, values.UnknownType)
	scanB := expressions.NewFullUnorderedScanExpression([]string{"B"}, values.UnknownType)
	qA := expressions.ForEachQuantifier(expressions.InitialOf(scanA))
	qB := expressions.ForEachQuantifier(expressions.InitialOf(scanB))

	union := expressions.NewLogicalUnionExpression([]expressions.Quantifier{qA, qB})
	unionRef := expressions.InitialOf(union)
	unionQ := expressions.ForEachQuantifier(unionRef)

	limit := expressions.NewLogicalLimitExpression(10, 0, unionQ)
	ref := expressions.InitialOf(limit)

	rule := cascades.NewPushLimitThroughUnionRule()
	results := cascades.FireExpressionRule(rule, ref)
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}

	outerLimit := results[0].(*expressions.LogicalLimitExpression)
	innerUnion := outerLimit.GetInner().GetRangesOver().Get().(*expressions.LogicalUnionExpression)
	for i, q := range innerUnion.GetQuantifiers() {
		branchLimit := q.GetRangesOver().Get().(*expressions.LogicalLimitExpression)
		if branchLimit.GetLimit() != 10 {
			t.Fatalf("branch %d limit = %d, want 10", i, branchLimit.GetLimit())
		}
	}
}

func TestPushLimitThroughUnion_NotUnion(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	limit := expressions.NewLogicalLimitExpression(10, 0, scanQ)
	ref := expressions.InitialOf(limit)

	rule := cascades.NewPushLimitThroughUnionRule()
	results := cascades.FireExpressionRule(rule, ref)
	if len(results) != 0 {
		t.Fatalf("expected 0 results for non-union, got %d", len(results))
	}
}
