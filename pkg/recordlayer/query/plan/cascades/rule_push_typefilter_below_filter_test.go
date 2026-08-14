package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
)

func TestPushTypeFilterBelowFilterRule_Fires(t *testing.T) {
	t.Parallel()
	pT := predicates.NewConstantPredicate(predicates.TriTrue)
	scan := typeRewriteScan("Order", "Customer")
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	innerF := mustTypeRewriteConstruct(expressions.NewLogicalFilterExpression([]predicates.QueryPredicate{pT}, scanQ))
	innerFQ := expressions.ForEachQuantifier(expressions.InitialOf(innerF))
	src := typeRewriteFilter([]string{"Order"}, innerFQ)
	ref := expressions.InitialOf(src)
	yielded := fireTypeRewriteRule(t, NewPushTypeFilterBelowFilterRule(), ref)
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
	innerTF, ok := newF.GetInner().GetRangesOver().Get().(*expressions.LogicalTypeFilterExpression)
	if !ok {
		t.Fatalf("filter inner = %T, want *LogicalTypeFilterExpression", newF.GetInner().GetRangesOver().Get())
	}
	if got := innerTF.GetRecordTypes(); len(got) != 1 || got[0] != "Order" {
		t.Fatalf("type-filter record types = %v, want [Order]", got)
	}
}

func TestPushTypeFilterBelowFilterRule_DeclinesOnNonFilterInner(t *testing.T) {
	t.Parallel()
	scan := typeRewriteScan("Order")
	q := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	src := typeRewriteFilter([]string{"Order"}, q)
	ref := expressions.InitialOf(src)
	yielded := fireTypeRewriteRule(t, NewPushTypeFilterBelowFilterRule(), ref)
	if len(yielded) != 0 {
		t.Fatalf("yielded %d on non-Filter inner, want 0", len(yielded))
	}
}

func TestPushTypeFilterBelowFilterRule_FixpointTerminates(t *testing.T) {
	t.Parallel()
	pT := predicates.NewConstantPredicate(predicates.TriTrue)
	scan := typeRewriteScan("Order")
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	innerF := mustTypeRewriteConstruct(expressions.NewLogicalFilterExpression([]predicates.QueryPredicate{pT}, scanQ))
	innerFQ := expressions.ForEachQuantifier(expressions.InitialOf(innerF))
	src := typeRewriteFilter([]string{"Order"}, innerFQ)
	ref := expressions.InitialOf(src)
	progress, converged := exploreTypeRewriting(NewPlanner([]ExpressionRule{NewPushTypeFilterBelowFilterRule()}, nil), ref)
	if !converged {
		t.Fatalf("exploration did not converge — tasks=%d, members=%d", progress, len(ref.Members()))
	}
}
