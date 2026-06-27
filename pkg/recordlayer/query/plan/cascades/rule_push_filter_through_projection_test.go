package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func filterOverProjection(p predicates.QueryPredicate, projVals []values.Value) *expressions.LogicalFilterExpression {
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	innerQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	proj := expressions.NewLogicalProjectionExpression(projVals, innerQ)
	projQ := expressions.ForEachQuantifier(expressions.InitialOf(proj))
	return expressions.NewLogicalFilterExpression([]predicates.QueryPredicate{p}, projQ)
}

func TestPushFilterThroughProjectionRule_Fires(t *testing.T) {
	t.Parallel()
	pT := predicates.NewConstantPredicate(predicates.TriTrue)
	src := filterOverProjection(pT,
		[]values.Value{&values.FieldValue{Field: "id", Typ: values.UnknownType}},
	)
	ref := expressions.InitialOf(src)
	yielded := FireExpressionRule(NewPushFilterThroughProjectionRule(), ref)
	if len(yielded) != 1 {
		t.Fatalf("yielded %d, want 1", len(yielded))
	}
	newProj, ok := yielded[0].(*expressions.LogicalProjectionExpression)
	if !ok {
		t.Fatalf("yielded %T, want *LogicalProjectionExpression", yielded[0])
	}
	innerFilter, ok := newProj.GetInner().GetRangesOver().Get().(*expressions.LogicalFilterExpression)
	if !ok {
		t.Fatalf("projection inner = %T, want *LogicalFilterExpression", newProj.GetInner().GetRangesOver().Get())
	}
	if got := innerFilter.GetPredicates(); len(got) != 1 || got[0] != pT {
		t.Fatalf("filter predicates wrong: got %v", got)
	}
	// Projection's projected-values list preserved.
	if got := newProj.GetProjectedValues(); len(got) != 1 {
		t.Fatalf("projected values len=%d, want 1", len(got))
	}
}

func TestPushFilterThroughProjectionRule_DeclinesOnNonProjectionInner(t *testing.T) {
	t.Parallel()
	pT := predicates.NewConstantPredicate(predicates.TriTrue)
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	q := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	src := expressions.NewLogicalFilterExpression([]predicates.QueryPredicate{pT}, q)
	ref := expressions.InitialOf(src)
	yielded := FireExpressionRule(NewPushFilterThroughProjectionRule(), ref)
	if len(yielded) != 0 {
		t.Fatalf("yielded %d on non-Projection inner, want 0", len(yielded))
	}
}

func TestPushFilterThroughProjectionRule_FixpointTerminates(t *testing.T) {
	t.Parallel()
	pT := predicates.NewConstantPredicate(predicates.TriTrue)
	src := filterOverProjection(pT,
		[]values.Value{&values.FieldValue{Field: "id", Typ: values.UnknownType}},
	)
	ref := expressions.InitialOf(src)
	progress, converged := FixpointApply([]ExpressionRule{NewPushFilterThroughProjectionRule()}, ref, 50)
	if !converged {
		t.Fatalf("FixpointApply did not converge — progress=%d, members=%d", progress, len(ref.Members()))
	}
}
