package cascades

import "testing"

// Simplifier converges on a single constant after folding.
func TestSimplify_AllConstantsFoldToConstant(t *testing.T) {
	t.Parallel()
	// (TRUE AND FALSE) OR NOT TRUE → FALSE OR FALSE → FALSE
	pred := NewOr(
		NewAnd(NewConstantPredicate(TriTrue), NewConstantPredicate(TriFalse)),
		NewNot(NewConstantPredicate(TriTrue)),
	)
	got := Simplify(pred, DefaultSimplifyRules())
	cp, ok := got.(*ConstantPredicate)
	if !ok {
		t.Fatalf("expected ConstantPredicate, got %T: %s", got, got.Explain())
	}
	if cp.Value != TriFalse {
		t.Fatalf("expected FALSE, got %v (explain: %s)", cp.Value, got.Explain())
	}
}

// Simplify drops identity children but preserves non-trivial ones.
func TestSimplify_DropIdentities(t *testing.T) {
	t.Parallel()
	leaf := NewConstantPredicate(TriUnknown) // stands in for any non-constant
	// AND(TRUE, leaf, TRUE) → leaf.
	pred := NewAnd(
		NewConstantPredicate(TriTrue),
		leaf,
		NewConstantPredicate(TriTrue),
	)
	got := Simplify(pred, DefaultSimplifyRules())
	if got != QueryPredicate(leaf) {
		t.Fatalf("expected the UNKNOWN leaf to survive, got %T %s", got, got.Explain())
	}
}

// Simplify descends into children.
func TestSimplify_DescendsIntoChildren(t *testing.T) {
	t.Parallel()
	// Inner NOT NOT x → x; outer AND still has the surviving child.
	leaf := NewConstantPredicate(TriUnknown)
	pred := NewAnd(
		NewConstantPredicate(TriTrue),
		NewNot(NewNot(leaf)),
	)
	got := Simplify(pred, DefaultSimplifyRules())
	// After recursion: AND(TRUE, leaf) → leaf.
	if got != QueryPredicate(leaf) {
		t.Fatalf("expected the UNKNOWN leaf, got %T %s", got, got.Explain())
	}
}

// Fixpoint convergence: a tree that requires multiple passes to
// fully simplify.
func TestSimplify_FixpointConvergence(t *testing.T) {
	t.Parallel()
	// ((NOT TRUE) OR FALSE) AND NOT NOT TRUE
	// = (FALSE OR FALSE) AND TRUE
	// = FALSE AND TRUE
	// = FALSE
	pred := NewAnd(
		NewOr(
			NewNot(NewConstantPredicate(TriTrue)),
			NewConstantPredicate(TriFalse),
		),
		NewNot(NewNot(NewConstantPredicate(TriTrue))),
	)
	got := Simplify(pred, DefaultSimplifyRules())
	cp, ok := got.(*ConstantPredicate)
	if !ok || cp.Value != TriFalse {
		t.Fatalf("expected FALSE, got %T %s", got, got.Explain())
	}
}

// Nil predicate / empty rules: identity.
func TestSimplify_Degenerate(t *testing.T) {
	t.Parallel()
	if got := Simplify(nil, DefaultSimplifyRules()); got != nil {
		t.Fatalf("nil input: expected nil, got %v", got)
	}
	leaf := NewConstantPredicate(TriTrue)
	if got := Simplify(leaf, nil); got != QueryPredicate(leaf) {
		t.Fatalf("empty rules: expected identity, got %v", got)
	}
}

// Cross-rule cooperation: And/Or/Not rules all fire in one
// Simplify call.
func TestSimplify_CrossRuleCooperation(t *testing.T) {
	t.Parallel()
	// NOT (TRUE AND FALSE) → NOT FALSE → TRUE
	pred := NewNot(NewAnd(
		NewConstantPredicate(TriTrue),
		NewConstantPredicate(TriFalse),
	))
	got := Simplify(pred, DefaultSimplifyRules())
	cp, ok := got.(*ConstantPredicate)
	if !ok || cp.Value != TriTrue {
		t.Fatalf("expected TRUE, got %T %s", got, got.Explain())
	}
}

// Comparison fold feeds into AND fold — end-to-end demonstration
// that the Simplify driver's rule set cooperates across different
// predicate types.
func TestSimplify_ComparisonPlusAnd(t *testing.T) {
	t.Parallel()
	// (5 = 5) AND (3 > 1) AND (age >= 18) → after comparison
	// folds: (TRUE AND TRUE AND age >= 18). AND identity-drop
	// removes the TRUEs, leaving the surviving ComparisonPredicate.
	agePred := NewComparisonPredicate(
		&FieldValue{Field: "age", Typ: TypeInt},
		Comparison{Type: ComparisonGreaterThanEq, Operand: int64(18)},
	)
	pred := NewAnd(
		NewComparisonPredicate(
			&ConstantValue{Value: int64(5), Typ: TypeInt},
			Comparison{Type: ComparisonEquals, Operand: int64(5)},
		),
		NewComparisonPredicate(
			&ConstantValue{Value: int64(3), Typ: TypeInt},
			Comparison{Type: ComparisonGreaterThan, Operand: int64(1)},
		),
		agePred,
	)
	got := Simplify(pred, DefaultSimplifyRules())
	if got != QueryPredicate(agePred) {
		t.Fatalf("expected the age predicate to survive, got %T %s", got, got.Explain())
	}
}
