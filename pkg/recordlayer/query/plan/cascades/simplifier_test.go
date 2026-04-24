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

// Full-pipeline test: Flatten + ComparisonFold + Not + AndDedup +
// AndConstant all cooperating. The input tree exercises every rule
// the seed ships. End-to-end predicate simplification.
func TestSimplify_FullPipeline(t *testing.T) {
	t.Parallel()
	// Input:
	//   AND(
	//     AND(                  ← nested AND (flatten fires)
	//       5 = 5,              ← ComparisonConstant fires → TRUE
	//       NOT NOT TRUE        ← NotConstant fires → TRUE
	//     ),
	//     age >= 18,            ← opaque
	//     age >= 18,            ← duplicate (AndDedup fires)
	//     TRUE                  ← AndConstant drops identity
	//   )
	// After simplification: just `age >= 18`.
	agePred := NewComparisonPredicate(
		&FieldValue{Field: "age", Typ: TypeInt},
		Comparison{Type: ComparisonGreaterThanEq, Operand: int64(18)},
	)
	pred := NewAnd(
		NewAnd(
			NewComparisonPredicate(
				&ConstantValue{Value: int64(5), Typ: TypeInt},
				Comparison{Type: ComparisonEquals, Operand: int64(5)},
			),
			NewNot(NewNot(NewConstantPredicate(TriTrue))),
		),
		agePred,
		agePred,
		NewConstantPredicate(TriTrue),
	)
	got := Simplify(pred, DefaultSimplifyRules())
	if got != QueryPredicate(agePred) {
		t.Fatalf("expected agePred to survive, got %T %s", got, got.Explain())
	}
}

// Simplify recurses through NotPredicate children too. Use a
// ValuePredicate leaf (not a ComparisonPredicate) so the
// NotComparisonRewrite rule declines — isolates the assertion to
// the "recursion visits the NOT body" property. Without recursion,
// the inner AND-fold wouldn't happen and the final tree would still
// be `NOT(AND(TRUE, leaf))` instead of `NOT(leaf)`.
func TestSimplify_RecursesThroughNot(t *testing.T) {
	t.Parallel()
	leaf := NewValuePredicate(&FieldValue{Field: "is_active", Typ: TypeBool})
	// NOT(AND(TRUE, leaf)) → inner AND folds to leaf → NOT(leaf).
	pred := NewNot(NewAnd(NewConstantPredicate(TriTrue), leaf))
	got := Simplify(pred, DefaultSimplifyRules())
	not, ok := got.(*NotPredicate)
	if !ok {
		t.Fatalf("expected NotPredicate, got %T: %s", got, got.Explain())
	}
	if not.Child != QueryPredicate(leaf) {
		t.Fatalf("expected NOT(leaf), got NOT(%T): %s", not.Child, got.Explain())
	}
}

// Kleene 3VL defensive: constants mixed with UNKNOWN must never
// collapse to FALSE/TRUE incorrectly. Regression cover for any future
// edit to And/OrConstantSimplifyRule that would break SQL NULL
// semantics. Expectations trace directly from Kleene truth tables.
func TestSimplify_Kleene3VLConstants(t *testing.T) {
	t.Parallel()
	u := NewConstantPredicate(TriUnknown)
	T := NewConstantPredicate(TriTrue)
	F := NewConstantPredicate(TriFalse)
	rules := DefaultSimplifyRules()

	// AND(TRUE, UNKNOWN) → UNKNOWN (TRUE is identity, UNKNOWN survives).
	if got := Simplify(NewAnd(T, u), rules); got != QueryPredicate(u) {
		t.Fatalf("AND(T,U): got %T %s", got, got.Explain())
	}
	// AND(FALSE, UNKNOWN) → FALSE (short-circuit).
	got := Simplify(NewAnd(F, u), rules)
	if cp, ok := got.(*ConstantPredicate); !ok || cp.Value != TriFalse {
		t.Fatalf("AND(F,U): got %T %s", got, got.Explain())
	}
	// OR(TRUE, UNKNOWN) → TRUE (short-circuit).
	got = Simplify(NewOr(T, u), rules)
	if cp, ok := got.(*ConstantPredicate); !ok || cp.Value != TriTrue {
		t.Fatalf("OR(T,U): got %T %s", got, got.Explain())
	}
	// OR(FALSE, UNKNOWN) → UNKNOWN (FALSE is identity).
	if got := Simplify(NewOr(F, u), rules); got != QueryPredicate(u) {
		t.Fatalf("OR(F,U): got %T %s", got, got.Explain())
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
