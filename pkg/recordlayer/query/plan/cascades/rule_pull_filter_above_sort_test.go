package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func TestPullFilterAboveSortRule_Fires(t *testing.T) {
	t.Parallel()
	pT := predicates.NewConstantPredicate(predicates.TriTrue)
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	innerF := expressions.NewLogicalFilterExpression([]predicates.QueryPredicate{pT}, scanQ)
	innerFQ := expressions.ForEachQuantifier(expressions.InitialOf(innerF))
	keys := []expressions.SortKey{
		{Value: &values.FieldValue{Field: "k", Typ: values.UnknownType}, Reverse: false},
	}
	src := expressions.NewLogicalSortExpression(keys, innerFQ)
	ref := expressions.InitialOf(src)
	yielded := FireExpressionRule(NewPullFilterAboveSortRule(), ref)
	if len(yielded) != 1 {
		t.Fatalf("yielded %d, want 1", len(yielded))
	}
	newF, ok := yielded[0].(*expressions.LogicalFilterExpression)
	if !ok {
		t.Fatalf("yielded %T, want *LogicalFilterExpression", yielded[0])
	}
	if got := newF.GetPredicates(); len(got) != 1 || got[0] != pT {
		t.Fatalf("filter predicates wrong: got %v", got)
	}
	innerSort, ok := newF.GetInner().GetRangesOver().Get().(*expressions.LogicalSortExpression)
	if !ok {
		t.Fatalf("filter inner = %T, want *LogicalSortExpression", newF.GetInner().GetRangesOver().Get())
	}
	if got := innerSort.GetSortKeys(); len(got) != 1 {
		t.Fatalf("sort keys len=%d, want 1", len(got))
	}
}

func TestPullFilterAboveSortRule_DeclinesOnNonFilterInner(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	q := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	keys := []expressions.SortKey{
		{Value: &values.FieldValue{Field: "k", Typ: values.UnknownType}},
	}
	src := expressions.NewLogicalSortExpression(keys, q)
	ref := expressions.InitialOf(src)
	yielded := FireExpressionRule(NewPullFilterAboveSortRule(), ref)
	if len(yielded) != 0 {
		t.Fatalf("yielded %d on non-Filter inner, want 0", len(yielded))
	}
}

func TestPullFilterAboveSortRule_FixpointTerminates(t *testing.T) {
	t.Parallel()
	pT := predicates.NewConstantPredicate(predicates.TriTrue)
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	innerF := expressions.NewLogicalFilterExpression([]predicates.QueryPredicate{pT}, scanQ)
	innerFQ := expressions.ForEachQuantifier(expressions.InitialOf(innerF))
	keys := []expressions.SortKey{
		{Value: &values.FieldValue{Field: "k", Typ: values.UnknownType}},
	}
	src := expressions.NewLogicalSortExpression(keys, innerFQ)
	ref := expressions.InitialOf(src)
	progress, converged := FixpointApply([]ExpressionRule{NewPullFilterAboveSortRule()}, ref, 50)
	if !converged {
		t.Fatalf("FixpointApply did not converge — progress=%d, members=%d", progress, len(ref.Members()))
	}
}
