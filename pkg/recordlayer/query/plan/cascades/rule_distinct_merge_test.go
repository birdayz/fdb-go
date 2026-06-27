package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
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

func TestDistinctMergeRule_TripleNestedCollapsesToSingle(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	d1 := expressions.NewLogicalDistinctExpression(scanQ)
	d1Q := expressions.ForEachQuantifier(expressions.InitialOf(d1))
	d2 := expressions.NewLogicalDistinctExpression(d1Q)
	d2Q := expressions.ForEachQuantifier(expressions.InitialOf(d2))
	d3 := expressions.NewLogicalDistinctExpression(d2Q)
	ref := expressions.InitialOf(d3)

	rule := NewDistinctMergeRule()
	// First fire: Distinct(Distinct(Distinct(Scan))) → Distinct(Distinct(Scan))
	y1 := FireExpressionRule(rule, ref)
	if len(y1) != 1 {
		t.Fatalf("first merge: yielded=%d, want 1", len(y1))
	}
	// The merged result is a Distinct whose inner's reference now has
	// both the original Distinct(Scan) and the newly-merged Distinct(Scan).
	// Re-fire on the new result to collapse the remaining nesting.
	ref2 := expressions.InitialOf(y1[0].(expressions.RelationalExpression))
	y2 := FireExpressionRule(rule, ref2)
	if len(y2) != 1 {
		t.Fatalf("second merge: yielded=%d, want 1", len(y2))
	}
	final := y2[0].(*expressions.LogicalDistinctExpression)
	inner := final.GetInner().GetRangesOver().Get()
	if _, ok := inner.(*expressions.FullUnorderedScanExpression); !ok {
		t.Fatalf("after two merges, inner=%T, want Scan", inner)
	}
}

func TestDistinctMergeRule_DeclinesOnNonDistinctInner(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	// Distinct over Sort — inner is Sort, not Distinct
	sort := expressions.NewLogicalSortExpression(
		[]expressions.SortKey{{Value: &values.FieldValue{Field: "x", Typ: values.UnknownType}}},
		scanQ,
	)
	sortQ := expressions.ForEachQuantifier(expressions.InitialOf(sort))
	d := expressions.NewLogicalDistinctExpression(sortQ)
	ref := expressions.InitialOf(d)

	rule := NewDistinctMergeRule()
	yielded := FireExpressionRule(rule, ref)
	if len(yielded) != 0 {
		t.Fatalf("rule should decline when inner is Sort, got %d yields", len(yielded))
	}
}
