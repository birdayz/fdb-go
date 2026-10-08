package expr

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// A LIKE macro body is stored as Java's LikeOperatorValue, and a WHERE over
// it lifts back to the LIKE predicate (LikeOperatorValue.toQueryPredicate),
// not to `like = TRUE`.
func TestLikeMacroBodyRoundTripsThroughThePredicate(t *testing.T) {
	t.Parallel()
	probe := &values.ConstantValue{Value: "abc", Typ: values.NotNullString}
	pattern := values.NewPatternForLikeValue(&values.ConstantValue{Value: "a%", Typ: values.NotNullString}, values.NewNullValue(values.NullType))
	pred := predicates.NewComparisonPredicate(probe, predicates.Comparison{Type: predicates.ComparisonLike, Operand: pattern})

	body, ok := MacroBodyValue(&predicateValue{pred: pred}).(*values.LikeOperatorValue)
	if !ok || body.Probe != probe || body.Pattern != pattern {
		t.Fatalf("macro body = %#v, want a LikeOperatorValue over the probe and pattern", body)
	}
	lifted, err := (&Resolver{}).liftValueToPredicate(body)
	if err != nil {
		t.Fatal(err)
	}
	cp, ok := lifted.(*predicates.ComparisonPredicate)
	if !ok || cp.Operand != probe || cp.Comparison.Type != predicates.ComparisonLike || cp.Comparison.Operand != pattern {
		t.Fatalf("lifted = %#v, want the LIKE predicate", lifted)
	}
}
