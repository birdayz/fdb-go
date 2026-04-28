package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

func TestDistinctMergeRule_FiresOnNested(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	innerD := expressions.NewLogicalDistinctExpression(scanQ)
	innerQ := expressions.ForEachQuantifier(expressions.InitialOf(innerD))
	outerD := expressions.NewLogicalDistinctExpression(innerQ)
	ref := expressions.InitialOf(outerD)

	rule := NewDistinctMergeRule()
	yielded := FireExpressionRule(rule, ref)
	if len(yielded) != 1 {
		t.Fatalf("yielded=%d, want 1", len(yielded))
	}
	merged := yielded[0].(*expressions.LogicalDistinctExpression)
	innerExpr := merged.GetInner().GetRangesOver().Get()
	if _, ok := innerExpr.(*expressions.FullUnorderedScanExpression); !ok {
		t.Fatalf("merged inner=%T, want *FullUnorderedScanExpression — rule didn't strip the inner Distinct", innerExpr)
	}
}

func TestDistinctMergeRule_DeclinesOnSingle(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	d := expressions.NewLogicalDistinctExpression(scanQ)
	ref := expressions.InitialOf(d)
	rule := NewDistinctMergeRule()
	yielded := FireExpressionRule(rule, ref)
	if len(yielded) != 0 {
		t.Fatalf("rule fired on a single Distinct — yielded %d, want 0", len(yielded))
	}
}
