package expr

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
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

// TestPredicateValue_SemanticHash pins the memo-hash folding (Graefe full-stack
// review): without values.SelfSemanticHash, every predicateValue collided into the
// bare "v:predicate" bucket, degrading memo lookup for predicate-in-projection /
// CASE-heavy SQL. The hash must (a) distinguish predicateValues wrapping different
// predicates, and (b) keep equal⟹same-hash — equal predicateValues hash
// identically (the memo invariant; predicates.StructuralHash mirrors the
// StructurallyEqual that EqualsWithoutChildrenValue uses).
func TestPredicateValue_SemanticHash(t *testing.T) {
	t.Parallel()

	pTrueA := &predicateValue{pred: &predicates.ConstantPredicate{Value: predicates.TriTrue}}
	pTrueB := &predicateValue{pred: &predicates.ConstantPredicate{Value: predicates.TriTrue}}
	pFalse := &predicateValue{pred: &predicates.ConstantPredicate{Value: predicates.TriFalse}}

	// (a) Distinct predicates → distinct hashes. Pre-fix both hashed to
	// "v:predicate" and shared a bucket.
	if values.SemanticHashCode(pTrueA) == values.SemanticHashCode(pFalse) {
		t.Error("predicate values wrapping different predicates should not share a memo hash bucket")
	}

	// (b) equal⟹same-hash: structurally-equal predicate values hash identically.
	if !values.EqualsWithoutChildren(pTrueA, pTrueB) {
		t.Fatal("precondition: pTrueA and pTrueB must compare equal")
	}
	if values.SemanticHashCode(pTrueA) != values.SemanticHashCode(pTrueB) {
		t.Error("equal predicate values must hash equal (equal⟹same-hash memo invariant)")
	}
}
