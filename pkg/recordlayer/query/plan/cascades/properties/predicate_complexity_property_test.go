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
	scan := mustFullUnorderedScanExpression(t, []string{"T"}, propertyTestFlowedType())
	if got := EvaluatePredicateComplexity(scan); got != 0 {
		t.Fatalf("EvaluatePredicateComplexity(scan) = %d, want 0", got)
	}
}

func TestEvaluatePredicateComplexity_SinglePredicate(t *testing.T) {
	t.Parallel()
	scan := mustFullUnorderedScanExpression(t, []string{"T"}, propertyTestFlowedType())
	ref := expressions.InitialOf(scan)
	inner := expressions.ForEachQuantifier(ref)
	pred := predicates.NewConstantPredicate(predicates.TriTrue)
	filter := mustLogicalFilterExpression(t, []predicates.QueryPredicate{pred}, inner)
	// A single leaf predicate has diameter 1.
	if got := EvaluatePredicateComplexity(filter); got != 1 {
		t.Fatalf("EvaluatePredicateComplexity(filter with 1 pred) = %d, want 1", got)
	}
}

func TestEvaluatePredicateComplexity_AndPredicate(t *testing.T) {
	t.Parallel()
	scan := mustFullUnorderedScanExpression(t, []string{"T"}, propertyTestFlowedType())
	ref := expressions.InitialOf(scan)
	inner := expressions.ForEachQuantifier(ref)
	// A top-level AND(a, b, c) is lifted into the filter's predicate list by
	// its constructor (Java's SelectExpression does the same), so the property
	// sees three leaves of diameter 1 — Java's PredicateComplexityProperty
	// also takes the max over getPredicates(), never the width of the list.
	a := predicates.NewConstantPredicate(predicates.TriTrue)
	b := predicates.NewConstantPredicate(predicates.TriFalse)
	c := predicates.NewConstantPredicate(predicates.TriTrue)
	and := predicates.NewAnd(a, b, c)
	filter := mustLogicalFilterExpression(t, []predicates.QueryPredicate{and}, inner)
	if got := len(filter.GetPredicates()); got != 3 {
		t.Fatalf("filter holds %d predicates, want the three lifted conjuncts", got)
	}
	if got := EvaluatePredicateComplexity(filter); got != 1 {
		t.Fatalf("EvaluatePredicateComplexity(lifted AND of 3) = %d, want 1", got)
	}
	// The same AND under an OR is not lifted and keeps its width of 3.
	nested := mustLogicalFilterExpression(t, []predicates.QueryPredicate{
		predicates.NewOr(predicates.NewAnd(a, b, c), predicates.NewConstantPredicate(predicates.TriTrue)),
	}, inner)
	if got := EvaluatePredicateComplexity(nested); got != 3 {
		t.Fatalf("EvaluatePredicateComplexity(OR(AND of 3, d)) = %d, want 3", got)
	}
}

func TestEvaluatePredicateComplexity_NestedPredicate(t *testing.T) {
	t.Parallel()
	scan := mustFullUnorderedScanExpression(t, []string{"T"}, propertyTestFlowedType())
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
	filter := mustLogicalFilterExpression(t, []predicates.QueryPredicate{and}, inner)
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
