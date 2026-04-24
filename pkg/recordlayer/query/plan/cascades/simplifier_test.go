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
		Comparison{Type: ComparisonGreaterThanEq, Operand: LiteralValue(int64(18))},
	)
	pred := NewAnd(
		NewAnd(
			NewComparisonPredicate(
				&ConstantValue{Value: int64(5), Typ: TypeInt},
				Comparison{Type: ComparisonEquals, Operand: LiteralValue(int64(5))},
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

// Non-constant RHS comparisons survive the fixpoint untouched —
// they aren't foldable at plan time (RHS is a row-dependent Value).
// Pins that ComparisonConstantSimplifyRule's IsConstantValue gate
// declines correctly when RHS is a FieldValue.
func TestSimplify_NonConstantRHS_Survives(t *testing.T) {
	t.Parallel()
	pred := NewComparisonPredicate(
		&FieldValue{Field: "age", Typ: TypeInt},
		Comparison{Type: ComparisonEquals, Operand: &FieldValue{Field: "cutoff", Typ: TypeInt}},
	)
	got := Simplify(pred, DefaultSimplifyRules())
	cp, ok := got.(*ComparisonPredicate)
	if !ok {
		t.Fatalf("expected *ComparisonPredicate to survive, got %T: %s", got, got.Explain())
	}
	if got.Explain() != "age = cutoff" {
		t.Fatalf("Explain: got %q, want %q", got.Explain(), "age = cutoff")
	}
	// Identity preserved — same pointer, no rewrite happened.
	if cp != pred {
		t.Errorf("expected predicate identity preserved through Simplify")
	}
}

// NOT over a non-constant comparison still rewrites via
// NotComparisonRewriteRule — Negate doesn't depend on RHS being
// constant, only on the Type having a direct negation. Pins that
// the rule fires on `NOT(a = b)` → `a <> b` even when `b` is a
// FieldValue.
func TestSimplify_NotComparison_NonConstantRHS_Rewrites(t *testing.T) {
	t.Parallel()
	pred := NewNot(NewComparisonPredicate(
		&FieldValue{Field: "a", Typ: TypeInt},
		Comparison{Type: ComparisonEquals, Operand: &FieldValue{Field: "b", Typ: TypeInt}},
	))
	got := Simplify(pred, DefaultSimplifyRules())
	cp, ok := got.(*ComparisonPredicate)
	if !ok {
		t.Fatalf("expected NOT to be pushed past comparison, got %T: %s", got, got.Explain())
	}
	if cp.Comparison.Type != ComparisonNotEquals {
		t.Fatalf("Type: got %v, want NotEquals", cp.Comparison.Type)
	}
	if got.Explain() != "a <> b" {
		t.Fatalf("Explain: got %q, want %q", got.Explain(), "a <> b")
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

// Triple-NOT collapse: NOT(NOT(NOT(x = 5))) → x <> 5. Exercises
// NotConstantSimplifyRule's double-neg elimination + the new
// NotComparisonRewriteRule cooperating across the fixpoint.
func TestSimplify_TripleNotCollapses(t *testing.T) {
	t.Parallel()
	age := &FieldValue{Field: "age", Typ: TypeInt}
	cp := NewComparisonPredicate(age, Comparison{Type: ComparisonEquals, Operand: LiteralValue(int64(5))})
	got := Simplify(
		NewNot(NewNot(NewNot(cp))),
		DefaultSimplifyRules(),
	)
	out, ok := got.(*ComparisonPredicate)
	if !ok {
		t.Fatalf("expected ComparisonPredicate, got %T %s", got, got.Explain())
	}
	if out.Comparison.Type != ComparisonNotEquals {
		t.Fatalf("expected age <> 5, got %s", got.Explain())
	}
}

// Simplify idempotence: running it a second time on its own output
// is a no-op. This is the defining fixpoint property — if it fails,
// a rule is non-reducing (loops on stable input) or the driver's
// pointer-equality break-out is broken.
func TestSimplify_Idempotent(t *testing.T) {
	t.Parallel()
	rules := DefaultSimplifyRules()
	age := &FieldValue{Field: "age", Typ: TypeInt}
	samples := []QueryPredicate{
		// Fully simplifiable → collapses to a constant.
		NewAnd(NewConstantPredicate(TriTrue), NewConstantPredicate(TriFalse)),
		// Partially simplifiable → surviving field predicate.
		NewAnd(
			NewComparisonPredicate(age, Comparison{Type: ComparisonGreaterThanEq, Operand: LiteralValue(int64(18))}),
			NewConstantPredicate(TriTrue),
		),
		// Exercises absorption: p AND (p OR q) → p.
		func() QueryPredicate {
			p := NewComparisonPredicate(age, Comparison{Type: ComparisonGreaterThanEq, Operand: LiteralValue(int64(18))})
			q := NewComparisonPredicate(age, Comparison{Type: ComparisonLessThan, Operand: LiteralValue(int64(65))})
			return NewAnd(p, NewOr(p, q))
		}(),
		// NOT-rewrite: NOT(x = 1) → x <> 1.
		NewNot(NewComparisonPredicate(age, Comparison{Type: ComparisonEquals, Operand: LiteralValue(int64(1))})),
		// Opaque: should be identity.
		NewValuePredicate(&FieldValue{Field: "flag", Typ: TypeBool}),
	}
	for _, s := range samples {
		once := Simplify(s, rules)
		twice := Simplify(once, rules)
		if once != twice {
			t.Fatalf("not idempotent for %s: once=%s twice=%s",
				s.Explain(), once.Explain(), twice.Explain())
		}
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
		Comparison{Type: ComparisonGreaterThanEq, Operand: LiteralValue(int64(18))},
	)
	pred := NewAnd(
		NewComparisonPredicate(
			&ConstantValue{Value: int64(5), Typ: TypeInt},
			Comparison{Type: ComparisonEquals, Operand: LiteralValue(int64(5))},
		),
		NewComparisonPredicate(
			&ConstantValue{Value: int64(3), Typ: TypeInt},
			Comparison{Type: ComparisonGreaterThan, Operand: LiteralValue(int64(1))},
		),
		agePred,
	)
	got := Simplify(pred, DefaultSimplifyRules())
	if got != QueryPredicate(agePred) {
		t.Fatalf("expected the age predicate to survive, got %T %s", got, got.Explain())
	}
}
