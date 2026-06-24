package expr

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// TestPredicateValue_EqualsWithoutChildren pins the fix for the planner panic the
// SQL fuzz net found ("SELECT * FROM orders ORDER BY !amount"): values.
// EqualsWithoutChildren hit its unhandled-type default on *predicateValue and
// panicked, because predicateValue lives in a package that imports values (so it
// can't be added to the switch without a cycle). predicateValue now carries its
// own structural equality via values.SelfEqualsWithoutChildren.
func TestPredicateValue_EqualsWithoutChildren(t *testing.T) {
	t.Parallel()

	pTrueA := &predicateValue{pred: &predicates.ConstantPredicate{Value: predicates.TriTrue}}
	pTrueB := &predicateValue{pred: &predicates.ConstantPredicate{Value: predicates.TriTrue}}
	pFalse := &predicateValue{pred: &predicates.ConstantPredicate{Value: predicates.TriFalse}}

	// The bug: this panicked (unhandled Value type *expr.predicateValue) instead
	// of comparing. It must now return a clean bool.
	if !values.EqualsWithoutChildren(pTrueA, pTrueB) {
		t.Error("structurally-equal predicate values must compare equal")
	}
	if values.EqualsWithoutChildren(pTrueA, pFalse) {
		t.Error("predicate values wrapping different predicates must not be equal")
	}

	// A predicateValue vs a non-predicate Value: not equal, and (the point) no panic.
	other := values.NewNullValue(values.TypeBool)
	if values.EqualsWithoutChildren(pTrueA, other) {
		t.Error("a predicate value must not equal a non-predicate value")
	}

	// The full structural walk (EqualsWithoutChildren + child recursion) agrees;
	// predicateValue is a leaf (Children() empty) so node-equality is decisive.
	if !values.ValuesStructurallyEqual(pTrueA, pTrueB) {
		t.Error("ValuesStructurallyEqual must agree for equal predicate values")
	}
	if values.ValuesStructurallyEqual(pTrueA, pFalse) {
		t.Error("ValuesStructurallyEqual must distinguish different predicate values")
	}
}
