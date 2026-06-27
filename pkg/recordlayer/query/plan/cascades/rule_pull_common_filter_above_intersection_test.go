package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func TestPullCommonFilterAboveIntersectionRule_CommonPredicate_Pulls(t *testing.T) {
	t.Parallel()
	pT := predicates.NewConstantPredicate(predicates.TriTrue)
	keys := []values.Value{&values.FieldValue{Field: "id", Typ: values.UnknownType}}
	mkChild := func(name string) expressions.Quantifier {
		scan := expressions.NewFullUnorderedScanExpression([]string{name}, values.UnknownType)
		scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
		f := expressions.NewLogicalFilterExpression([]predicates.QueryPredicate{pT}, scanQ)
		return expressions.ForEachQuantifier(expressions.InitialOf(f))
	}
	src := expressions.NewLogicalIntersectionExpression(
		[]expressions.Quantifier{mkChild("A"), mkChild("B")}, keys,
	)
	ref := expressions.InitialOf(src)
	yielded := FireExpressionRule(NewPullCommonFilterAboveIntersectionRule(), ref)
	if len(yielded) != 1 {
		t.Fatalf("yielded %d, want 1", len(yielded))
	}
	newF, ok := yielded[0].(*expressions.LogicalFilterExpression)
	if !ok {
		t.Fatalf("yielded %T, want *LogicalFilterExpression", yielded[0])
	}
	newX, ok := newF.GetInner().GetRangesOver().Get().(*expressions.LogicalIntersectionExpression)
	if !ok {
		t.Fatalf("filter inner = %T, want *LogicalIntersectionExpression", newF.GetInner().GetRangesOver().Get())
	}
	// Comparison keys preserved.
	if got := newX.GetComparisonKeyValues(); len(got) != 1 || got[0] != keys[0] {
		t.Fatalf("comparison keys not preserved: got %v, want %v", got, keys)
	}
	if got := len(newX.GetQuantifiers()); got != 2 {
		t.Fatalf("intersection has %d children, want 2", got)
	}
}

func TestPullCommonFilterAboveIntersectionRule_DifferentPredicates_NoFire(t *testing.T) {
	t.Parallel()
	pT := predicates.NewConstantPredicate(predicates.TriTrue)
	pF := predicates.NewConstantPredicate(predicates.TriFalse)
	keys := []values.Value{&values.FieldValue{Field: "id", Typ: values.UnknownType}}
	mkChild := func(name string, p predicates.QueryPredicate) expressions.Quantifier {
		scan := expressions.NewFullUnorderedScanExpression([]string{name}, values.UnknownType)
		scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
		f := expressions.NewLogicalFilterExpression([]predicates.QueryPredicate{p}, scanQ)
		return expressions.ForEachQuantifier(expressions.InitialOf(f))
	}
	src := expressions.NewLogicalIntersectionExpression(
		[]expressions.Quantifier{mkChild("A", pT), mkChild("B", pF)}, keys,
	)
	ref := expressions.InitialOf(src)
	yielded := FireExpressionRule(NewPullCommonFilterAboveIntersectionRule(), ref)
	if len(yielded) != 0 {
		t.Fatalf("yielded %d on different-predicate intersection children, want 0", len(yielded))
	}
}

func TestPullCommonFilterAboveIntersectionRule_FixpointTerminates(t *testing.T) {
	t.Parallel()
	pT := predicates.NewConstantPredicate(predicates.TriTrue)
	keys := []values.Value{&values.FieldValue{Field: "id", Typ: values.UnknownType}}
	mkChild := func(name string) expressions.Quantifier {
		scan := expressions.NewFullUnorderedScanExpression([]string{name}, values.UnknownType)
		scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
		f := expressions.NewLogicalFilterExpression([]predicates.QueryPredicate{pT}, scanQ)
		return expressions.ForEachQuantifier(expressions.InitialOf(f))
	}
	src := expressions.NewLogicalIntersectionExpression(
		[]expressions.Quantifier{mkChild("A"), mkChild("B")}, keys,
	)
	ref := expressions.InitialOf(src)
	progress, converged := FixpointApply([]ExpressionRule{NewPullCommonFilterAboveIntersectionRule()}, ref, 50)
	if !converged {
		t.Fatalf("FixpointApply did not converge — progress=%d, members=%d", progress, len(ref.Members()))
	}
}
