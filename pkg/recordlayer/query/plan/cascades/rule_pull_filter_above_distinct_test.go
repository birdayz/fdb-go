package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func TestPullFilterAboveDistinctRule_Fires(t *testing.T) {
	t.Parallel()
	pT := predicates.NewConstantPredicate(predicates.TriTrue)
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	innerF := expressions.NewLogicalFilterExpression([]predicates.QueryPredicate{pT}, scanQ)
	innerFQ := expressions.ForEachQuantifier(expressions.InitialOf(innerF))
	src := expressions.NewLogicalDistinctExpression(innerFQ)
	ref := expressions.InitialOf(src)
	yielded := FireExpressionRule(NewPullFilterAboveDistinctRule(), ref)
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
	innerD, ok := newF.GetInner().GetRangesOver().Get().(*expressions.LogicalDistinctExpression)
	if !ok {
		t.Fatalf("filter inner = %T, want *LogicalDistinctExpression", newF.GetInner().GetRangesOver().Get())
	}
	if _, scanOK := innerD.GetInner().GetRangesOver().Get().(*expressions.FullUnorderedScanExpression); !scanOK {
		t.Fatalf("distinct inner = %T, want Scan", innerD.GetInner().GetRangesOver().Get())
	}
}

func TestPullFilterAboveDistinctRule_DeclinesOnNonFilterInner(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	q := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	src := expressions.NewLogicalDistinctExpression(q)
	ref := expressions.InitialOf(src)
	yielded := FireExpressionRule(NewPullFilterAboveDistinctRule(), ref)
	if len(yielded) != 0 {
		t.Fatalf("yielded %d on non-Filter inner, want 0", len(yielded))
	}
}

func TestPullFilterAboveDistinctRule_FixpointTerminates(t *testing.T) {
	t.Parallel()
	pT := predicates.NewConstantPredicate(predicates.TriTrue)
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	innerF := expressions.NewLogicalFilterExpression([]predicates.QueryPredicate{pT}, scanQ)
	innerFQ := expressions.ForEachQuantifier(expressions.InitialOf(innerF))
	src := expressions.NewLogicalDistinctExpression(innerFQ)
	ref := expressions.InitialOf(src)
	progress, converged := FixpointApply([]ExpressionRule{NewPullFilterAboveDistinctRule()}, ref, 50)
	if !converged {
		t.Fatalf("FixpointApply did not converge — progress=%d, members=%d", progress, len(ref.Members()))
	}
}
