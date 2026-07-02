package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func TestPullFilterAboveProjectionRule_Fires(t *testing.T) {
	t.Parallel()
	pT := predicates.NewConstantPredicate(predicates.TriTrue)
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	innerF := expressions.NewLogicalFilterExpression([]predicates.QueryPredicate{pT}, scanQ)
	innerFQ := expressions.ForEachQuantifier(expressions.InitialOf(innerF))
	src := expressions.NewLogicalProjectionExpression(
		[]values.Value{&values.FieldValue{Field: "id", Typ: values.UnknownType}}, innerFQ,
	)
	ref := expressions.InitialOf(src)
	yielded := FireExpressionRule(NewPullFilterAboveProjectionRule(), ref)
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
	if _, ok := newF.GetInner().GetRangesOver().Get().(*expressions.LogicalProjectionExpression); !ok {
		t.Fatalf("filter inner = %T, want *LogicalProjectionExpression", newF.GetInner().GetRangesOver().Get())
	}
}

func TestPullFilterAboveProjectionRule_DeclinesOnNonFilterInner(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	q := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	src := expressions.NewLogicalProjectionExpression(
		[]values.Value{&values.FieldValue{Field: "id", Typ: values.UnknownType}}, q,
	)
	ref := expressions.InitialOf(src)
	yielded := FireExpressionRule(NewPullFilterAboveProjectionRule(), ref)
	if len(yielded) != 0 {
		t.Fatalf("yielded %d on non-Filter inner, want 0", len(yielded))
	}
}

func TestPullFilterAboveProjectionRule_FixpointTerminates(t *testing.T) {
	t.Parallel()
	pT := predicates.NewConstantPredicate(predicates.TriTrue)
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	innerF := expressions.NewLogicalFilterExpression([]predicates.QueryPredicate{pT}, scanQ)
	innerFQ := expressions.ForEachQuantifier(expressions.InitialOf(innerF))
	src := expressions.NewLogicalProjectionExpression(
		[]values.Value{&values.FieldValue{Field: "id", Typ: values.UnknownType}}, innerFQ,
	)
	ref := expressions.InitialOf(src)
	progress, converged := exploreRewriting(NewPlanner([]ExpressionRule{NewPullFilterAboveProjectionRule()}, nil), ref)
	if !converged {
		t.Fatalf("exploration did not converge — tasks=%d, members=%d", progress, len(ref.Members()))
	}
}
