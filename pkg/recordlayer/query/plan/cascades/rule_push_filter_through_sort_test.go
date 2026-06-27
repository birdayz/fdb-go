package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func TestPushFilterThroughSortRule_Fires(t *testing.T) {
	t.Parallel()
	pT := predicates.NewConstantPredicate(predicates.TriTrue)
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	keys := []expressions.SortKey{
		{Value: &values.FieldValue{Field: "k", Typ: values.UnknownType}, Reverse: false},
	}
	sort := expressions.NewLogicalSortExpression(keys, scanQ)
	sortQ := expressions.ForEachQuantifier(expressions.InitialOf(sort))
	src := expressions.NewLogicalFilterExpression([]predicates.QueryPredicate{pT}, sortQ)
	ref := expressions.InitialOf(src)
	yielded := FireExpressionRule(NewPushFilterThroughSortRule(), ref)
	if len(yielded) != 1 {
		t.Fatalf("yielded %d, want 1", len(yielded))
	}
	newSort, ok := yielded[0].(*expressions.LogicalSortExpression)
	if !ok {
		t.Fatalf("yielded %T, want *LogicalSortExpression", yielded[0])
	}
	// Sort keys preserved.
	if got := newSort.GetSortKeys(); len(got) != 1 {
		t.Fatalf("sort keys len=%d, want 1", len(got))
	}
	innerFilter, ok := newSort.GetInner().GetRangesOver().Get().(*expressions.LogicalFilterExpression)
	if !ok {
		t.Fatalf("sort inner = %T, want *LogicalFilterExpression", newSort.GetInner().GetRangesOver().Get())
	}
	if _, ok := innerFilter.GetInner().GetRangesOver().Get().(*expressions.FullUnorderedScanExpression); !ok {
		t.Fatalf("filter inner = %T, want Scan", innerFilter.GetInner().GetRangesOver().Get())
	}
}

func TestPushFilterThroughSortRule_DeclinesOnNonSortInner(t *testing.T) {
	t.Parallel()
	pT := predicates.NewConstantPredicate(predicates.TriTrue)
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	q := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	src := expressions.NewLogicalFilterExpression([]predicates.QueryPredicate{pT}, q)
	ref := expressions.InitialOf(src)
	yielded := FireExpressionRule(NewPushFilterThroughSortRule(), ref)
	if len(yielded) != 0 {
		t.Fatalf("yielded %d on non-Sort inner, want 0", len(yielded))
	}
}

func TestPushFilterThroughSortRule_FixpointTerminates(t *testing.T) {
	t.Parallel()
	pT := predicates.NewConstantPredicate(predicates.TriTrue)
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	keys := []expressions.SortKey{
		{Value: &values.FieldValue{Field: "k", Typ: values.UnknownType}, Reverse: false},
	}
	sort := expressions.NewLogicalSortExpression(keys, scanQ)
	sortQ := expressions.ForEachQuantifier(expressions.InitialOf(sort))
	src := expressions.NewLogicalFilterExpression([]predicates.QueryPredicate{pT}, sortQ)
	ref := expressions.InitialOf(src)
	progress, converged := FixpointApply([]ExpressionRule{NewPushFilterThroughSortRule()}, ref, 50)
	if !converged {
		t.Fatalf("FixpointApply did not converge — progress=%d, members=%d", progress, len(ref.Members()))
	}
}
