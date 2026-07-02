package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// distinctOverSort builds Distinct(Sort([sortKey], Scan)).
func distinctOverSort(sortKey string) *expressions.LogicalDistinctExpression {
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	keys := []expressions.SortKey{
		{Value: &values.FieldValue{Field: sortKey, Typ: values.UnknownType}, Reverse: false},
	}
	sort := expressions.NewLogicalSortExpression(keys, scanQ)
	sortQ := expressions.ForEachQuantifier(expressions.InitialOf(sort))
	return expressions.NewLogicalDistinctExpression(sortQ)
}

func TestDistinctOverSortElimRule_Fires(t *testing.T) {
	t.Parallel()
	d := distinctOverSort("k")
	ref := expressions.InitialOf(d)
	yielded := FireExpressionRule(NewDistinctOverSortElimRule(), ref)
	if len(yielded) != 1 {
		t.Fatalf("yielded %d, want 1", len(yielded))
	}
	flat, ok := yielded[0].(*expressions.LogicalDistinctExpression)
	if !ok {
		t.Fatalf("yielded %T, want *LogicalDistinctExpression", yielded[0])
	}
	// New Distinct's inner should be the Scan, not the Sort.
	innerInner := flat.GetInner().GetRangesOver().Get()
	if _, ok := innerInner.(*expressions.FullUnorderedScanExpression); !ok {
		t.Fatalf("rewritten inner = %T, want *FullUnorderedScanExpression", innerInner)
	}
}

func TestDistinctOverSortElimRule_DeclinesOnNonSortInner(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	q := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	d := expressions.NewLogicalDistinctExpression(q)
	ref := expressions.InitialOf(d)
	yielded := FireExpressionRule(NewDistinctOverSortElimRule(), ref)
	if len(yielded) != 0 {
		t.Fatalf("yielded %d on non-Sort inner, want 0", len(yielded))
	}
}

func TestDistinctOverSortElimRule_FixpointTerminates(t *testing.T) {
	t.Parallel()
	d := distinctOverSort("k")
	ref := expressions.InitialOf(d)
	progress, converged := exploreRewriting(NewPlanner([]ExpressionRule{NewDistinctOverSortElimRule()}, nil), ref)
	if !converged {
		t.Fatalf("exploration did not converge — tasks=%d, members=%d", progress, len(ref.Members()))
	}
}
