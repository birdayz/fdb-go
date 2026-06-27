package properties

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
)

func TestEvaluatePredicateComplexity_Nil(t *testing.T) {
	t.Parallel()
	if got := EvaluatePredicateComplexity(nil); got != 0 {
		t.Fatalf("EvaluatePredicateComplexity(nil) = %d, want 0", got)
	}
}

func TestEvaluatePredicateComplexity_NoPredicates(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, nil)
	if got := EvaluatePredicateComplexity(scan); got != 0 {
		t.Fatalf("EvaluatePredicateComplexity(scan) = %d, want 0", got)
	}
}

func TestEvaluatePredicateComplexity_SinglePredicate(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, nil)
	ref := expressions.InitialOf(scan)
	inner := expressions.ForEachQuantifier(ref)
	pred := predicates.NewConstantPredicate(predicates.TriTrue)
	filter := expressions.NewLogicalFilterExpression([]predicates.QueryPredicate{pred}, inner)
	// A single leaf predicate has diameter 1.
	if got := EvaluatePredicateComplexity(filter); got != 1 {
		t.Fatalf("EvaluatePredicateComplexity(filter with 1 pred) = %d, want 1", got)
	}
}

func TestEvaluatePredicateComplexity_AndPredicate(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, nil)
	ref := expressions.InitialOf(scan)
	inner := expressions.ForEachQuantifier(ref)
	// AND(a, b, c) has diameter 3 (width at root level).
	a := predicates.NewConstantPredicate(predicates.TriTrue)
	b := predicates.NewConstantPredicate(predicates.TriFalse)
	c := predicates.NewConstantPredicate(predicates.TriTrue)
	and := predicates.NewAnd(a, b, c)
	filter := expressions.NewLogicalFilterExpression([]predicates.QueryPredicate{and}, inner)
	if got := EvaluatePredicateComplexity(filter); got != 3 {
		t.Fatalf("EvaluatePredicateComplexity(AND of 3) = %d, want 3", got)
	}
}

func TestEvaluatePredicateComplexity_NestedPredicate(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, nil)
	ref := expressions.InitialOf(scan)
	inner := expressions.ForEachQuantifier(ref)
	// AND(OR(a, b, c, d), e) — inner OR has width 4, outer AND width 2.
	// Max diameter = 4.
	a := predicates.NewConstantPredicate(predicates.TriTrue)
	b := predicates.NewConstantPredicate(predicates.TriTrue)
	c := predicates.NewConstantPredicate(predicates.TriTrue)
	d := predicates.NewConstantPredicate(predicates.TriTrue)
	e := predicates.NewConstantPredicate(predicates.TriTrue)
	or := predicates.NewOr(a, b, c, d)
	and := predicates.NewAnd(or, e)
	filter := expressions.NewLogicalFilterExpression([]predicates.QueryPredicate{and}, inner)
	if got := EvaluatePredicateComplexity(filter); got != 4 {
		t.Fatalf("EvaluatePredicateComplexity(nested) = %d, want 4", got)
	}
}

func TestPredicateDiameter_LeafIsOne(t *testing.T) {
	t.Parallel()
	if got := predicateDiameter(predicates.NewConstantPredicate(predicates.TriTrue)); got != 1 {
		t.Fatalf("predicateDiameter(leaf) = %d, want 1", got)
	}
}

func TestPredicateDiameter_Nil(t *testing.T) {
	t.Parallel()
	if got := predicateDiameter(nil); got != 0 {
		t.Fatalf("predicateDiameter(nil) = %d, want 0", got)
	}
}
