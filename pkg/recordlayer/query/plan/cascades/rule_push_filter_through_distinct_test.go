package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// filterOverDistinct builds Filter([P], Distinct(Scan)).
func filterOverDistinct(p predicates.QueryPredicate) *expressions.LogicalFilterExpression {
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	innerQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	dist := expressions.NewLogicalDistinctExpression(innerQ)
	distQ := expressions.ForEachQuantifier(expressions.InitialOf(dist))
	return expressions.NewLogicalFilterExpression([]predicates.QueryPredicate{p}, distQ)
}

func TestPushFilterThroughDistinctRule_Fires(t *testing.T) {
	t.Parallel()
	pT := predicates.NewConstantPredicate(predicates.TriTrue)
	src := filterOverDistinct(pT)
	ref := expressions.InitialOf(src)
	yielded := FireExpressionRule(NewPushFilterThroughDistinctRule(), ref)
	if len(yielded) != 1 {
		t.Fatalf("yielded %d, want 1", len(yielded))
	}
	dist, ok := yielded[0].(*expressions.LogicalDistinctExpression)
	if !ok {
		t.Fatalf("yielded %T, want *LogicalDistinctExpression", yielded[0])
	}
	innerFilter, ok := dist.GetInner().GetRangesOver().Get().(*expressions.LogicalFilterExpression)
	if !ok {
		t.Fatalf("distinct inner = %T, want *LogicalFilterExpression", dist.GetInner().GetRangesOver().Get())
	}
	if got := innerFilter.GetPredicates(); len(got) != 1 || got[0] != pT {
		t.Fatalf("filter predicates wrong: got %v, want [%v]", got, pT)
	}
	if _, ok := innerFilter.GetInner().GetRangesOver().Get().(*expressions.FullUnorderedScanExpression); !ok {
		t.Fatalf("filter inner = %T, want Scan", innerFilter.GetInner().GetRangesOver().Get())
	}
}

func TestPushFilterThroughDistinctRule_DeclinesOnNonDistinctInner(t *testing.T) {
	t.Parallel()
	pT := predicates.NewConstantPredicate(predicates.TriTrue)
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	q := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	filter := expressions.NewLogicalFilterExpression([]predicates.QueryPredicate{pT}, q)
	ref := expressions.InitialOf(filter)
	yielded := FireExpressionRule(NewPushFilterThroughDistinctRule(), ref)
	if len(yielded) != 0 {
		t.Fatalf("yielded %d on non-Distinct inner, want 0", len(yielded))
	}
}

func TestPushFilterThroughDistinctRule_FixpointTerminates(t *testing.T) {
	t.Parallel()
	pT := predicates.NewConstantPredicate(predicates.TriTrue)
	src := filterOverDistinct(pT)
	ref := expressions.InitialOf(src)
	progress, converged := exploreRewriting(NewPlanner([]ExpressionRule{NewPushFilterThroughDistinctRule()}, nil), ref)
	if !converged {
		t.Fatalf("exploration did not converge — tasks=%d, members=%d", progress, len(ref.Members()))
	}
}
