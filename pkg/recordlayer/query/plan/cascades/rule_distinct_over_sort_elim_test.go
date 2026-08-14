package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
)

// distinctOverSort builds Distinct(Sort([sortKey], Scan)).
func distinctOverSort(t testing.TB, sortOrdinal int) *expressions.LogicalDistinctExpression {
	t.Helper()
	scan := distinctRuleScan(t, "T")
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	scanRoot := mustDistinctConstruct(scanQ.RequireFlowedObjectValue())
	keys := []expressions.SortKey{
		{Value: distinctRuleField(t, scanRoot, sortOrdinal), Reverse: false},
	}
	sort := mustDistinctConstruct(expressions.NewLogicalSortExpression(keys, scanQ))
	sortQ := expressions.ForEachQuantifier(expressions.InitialOf(sort))
	return distinctRuleDistinct(t, sortQ)
}

func TestDistinctOverSortElimRule_Fires(t *testing.T) {
	t.Parallel()
	d := distinctOverSort(t, 0)
	ref := expressions.InitialOf(d)
	yielded := mustFireDistinctExpressionRule(t, NewDistinctOverSortElimRule(), ref)
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
	scan := distinctRuleScan(t, "T")
	q := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	d := distinctRuleDistinct(t, q)
	ref := expressions.InitialOf(d)
	yielded := mustFireDistinctExpressionRule(t, NewDistinctOverSortElimRule(), ref)
	if len(yielded) != 0 {
		t.Fatalf("yielded %d on non-Sort inner, want 0", len(yielded))
	}
}

func TestDistinctOverSortElimRule_FixpointTerminates(t *testing.T) {
	t.Parallel()
	d := distinctOverSort(t, 0)
	ref := expressions.InitialOf(d)
	progress, converged := exploreDistinctRewriting(NewPlanner([]ExpressionRule{NewDistinctOverSortElimRule()}, nil), ref)
	if !converged {
		t.Fatalf("exploration did not converge — tasks=%d, members=%d", progress, len(ref.Members()))
	}
}
