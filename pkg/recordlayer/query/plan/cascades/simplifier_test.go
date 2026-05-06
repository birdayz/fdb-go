package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// Simplifier converges on a single constant after folding.
func TestSimplify_AllConstantsFoldToConstant(t *testing.T) {
	t.Parallel()
	// (TRUE AND FALSE) OR NOT TRUE → FALSE OR FALSE → FALSE
	pred := predicates.NewOr(
		predicates.NewAnd(predicates.NewConstantPredicate(predicates.TriTrue), predicates.NewConstantPredicate(predicates.TriFalse)),
		predicates.NewNot(predicates.NewConstantPredicate(predicates.TriTrue)),
	)
	got := Simplify(pred, DefaultSimplifyRules())
	cp, ok := got.(*predicates.ConstantPredicate)
	if !ok {
		t.Fatalf("expected ConstantPredicate, got %T: %s", got, got.Explain())
	}
	if cp.Value != predicates.TriFalse {
		t.Fatalf("expected FALSE, got %v (explain: %s)", cp.Value, got.Explain())
	}
}

// Simplify drops identity children but preserves non-trivial ones.
func TestSimplify_DropIdentities(t *testing.T) {
	t.Parallel()
	leaf := predicates.NewConstantPredicate(predicates.TriUnknown) // stands in for any non-constant
	// AND(TRUE, leaf, TRUE) → leaf.
	pred := predicates.NewAnd(
		predicates.NewConstantPredicate(predicates.TriTrue),
		leaf,
		predicates.NewConstantPredicate(predicates.TriTrue),
	)
	got := Simplify(pred, DefaultSimplifyRules())
	if got != predicates.QueryPredicate(leaf) {
		t.Fatalf("expected the UNKNOWN leaf to survive, got %T %s", got, got.Explain())
	}
}

// Simplify descends into children.
func TestSimplify_DescendsIntoChildren(t *testing.T) {
	t.Parallel()
	// Inner NOT NOT x → x; outer AND still has the surviving child.
	leaf := predicates.NewConstantPredicate(predicates.TriUnknown)
	pred := predicates.NewAnd(
		predicates.NewConstantPredicate(predicates.TriTrue),
		predicates.NewNot(predicates.NewNot(leaf)),
	)
	got := Simplify(pred, DefaultSimplifyRules())
	// After recursion: AND(TRUE, leaf) → leaf.
	if got != predicates.QueryPredicate(leaf) {
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
	pred := predicates.NewAnd(
		predicates.NewOr(
			predicates.NewNot(predicates.NewConstantPredicate(predicates.TriTrue)),
			predicates.NewConstantPredicate(predicates.TriFalse),
		),
		predicates.NewNot(predicates.NewNot(predicates.NewConstantPredicate(predicates.TriTrue))),
	)
	got := Simplify(pred, DefaultSimplifyRules())
	cp, ok := got.(*predicates.ConstantPredicate)
	if !ok || cp.Value != predicates.TriFalse {
		t.Fatalf("expected FALSE, got %T %s", got, got.Explain())
	}
}

// Nil predicate / empty rules: identity.
func TestSimplify_Degenerate(t *testing.T) {
	t.Parallel()
	if got := Simplify(nil, DefaultSimplifyRules()); got != nil {
		t.Fatalf("nil input: expected nil, got %v", got)
	}
	leaf := predicates.NewConstantPredicate(predicates.TriTrue)
	if got := Simplify(leaf, nil); got != predicates.QueryPredicate(leaf) {
		t.Fatalf("empty rules: expected identity, got %v", got)
	}
}

// Cross-rule cooperation: And/Or/Not rules all fire in one
// Simplify call.
func TestSimplify_CrossRuleCooperation(t *testing.T) {
	t.Parallel()
	// NOT (TRUE AND FALSE) → NOT FALSE → TRUE
	pred := predicates.NewNot(predicates.NewAnd(
		predicates.NewConstantPredicate(predicates.TriTrue),
		predicates.NewConstantPredicate(predicates.TriFalse),
	))
	got := Simplify(pred, DefaultSimplifyRules())
	cp, ok := got.(*predicates.ConstantPredicate)
	if !ok || cp.Value != predicates.TriTrue {
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
	agePred := predicates.NewComparisonPredicate(
		&values.FieldValue{Field: "age", Typ: values.TypeInt},
		predicates.Comparison{Type: predicates.ComparisonGreaterThanEq, Operand: values.LiteralValue(int64(18))},
	)
	pred := predicates.NewAnd(
		predicates.NewAnd(
			predicates.NewComparisonPredicate(
				&values.ConstantValue{Value: int64(5), Typ: values.TypeInt},
				predicates.Comparison{Type: predicates.ComparisonEquals, Operand: values.LiteralValue(int64(5))},
			),
			predicates.NewNot(predicates.NewNot(predicates.NewConstantPredicate(predicates.TriTrue))),
		),
		agePred,
		agePred,
		predicates.NewConstantPredicate(predicates.TriTrue),
	)
	got := Simplify(pred, DefaultSimplifyRules())
	if got != predicates.QueryPredicate(agePred) {
		t.Fatalf("expected agePred to survive, got %T %s", got, got.Explain())
	}
}

// IS NULL / IS NOT NULL fold at plan time when the LHS is a known
// constant. Pin all three branches:
//   - FieldValue LHS: NOT foldable (depends on row context), survives
//   - NullValue LHS: IS NULL → TRUE
//   - ConstantValue LHS: IS NOT NULL → TRUE
//
// Catches a future regression where the constant-fold rule narrows
// its whitelist and stops recognising NullValue / BooleanValue LHS.
func TestSimplify_IsNullVariants(t *testing.T) {
	t.Parallel()
	t.Run("FieldValue LHS survives", func(t *testing.T) {
		t.Parallel()
		pred := predicates.NewComparisonPredicate(
			&values.FieldValue{Field: "name", Typ: values.TypeString},
			predicates.Comparison{Type: predicates.ComparisonIsNull},
		)
		got := Simplify(pred, DefaultSimplifyRules())
		if _, ok := got.(*predicates.ComparisonPredicate); !ok {
			t.Fatalf("expected ComparisonPredicate to survive (LHS row-dependent), got %T: %s", got, got.Explain())
		}
	})
	t.Run("NullValue LHS folds to TRUE", func(t *testing.T) {
		t.Parallel()
		pred := predicates.NewComparisonPredicate(
			&values.NullValue{Typ: values.TypeUnknown},
			predicates.Comparison{Type: predicates.ComparisonIsNull},
		)
		got := Simplify(pred, DefaultSimplifyRules())
		cp, ok := got.(*predicates.ConstantPredicate)
		if !ok || cp.Value != predicates.TriTrue {
			t.Fatalf("got %T %s, want ConstantPredicate{TRUE}", got, got.Explain())
		}
	})
	t.Run("ConstantValue LHS, IS NOT NULL folds to TRUE", func(t *testing.T) {
		t.Parallel()
		pred := predicates.NewComparisonPredicate(
			&values.ConstantValue{Value: int64(5), Typ: values.TypeInt},
			predicates.Comparison{Type: predicates.ComparisonIsNotNull},
		)
		got := Simplify(pred, DefaultSimplifyRules())
		cp, ok := got.(*predicates.ConstantPredicate)
		if !ok || cp.Value != predicates.TriTrue {
			t.Fatalf("got %T %s, want ConstantPredicate{TRUE}", got, got.Explain())
		}
	})
}

// `(1 + 2) > 0` and `0 < (1 + 2)` both fold to TRUE end-to-end
// through Simplify. Pins the EvaluateConstant fall-through path
// for composite-constant operands on either side of a comparison.
// Two halves: LHS-composite + RHS-leaf, then leaf + RHS-composite.
func TestSimplify_CompositeConstantOnEitherSide_Folds(t *testing.T) {
	t.Parallel()
	add12 := &values.ArithmeticValue{
		Op:    values.OpAdd,
		Left:  &values.ConstantValue{Value: int64(1), Typ: values.TypeInt},
		Right: &values.ConstantValue{Value: int64(2), Typ: values.TypeInt},
	}
	cases := []struct {
		name string
		pred *predicates.ComparisonPredicate
	}{
		{
			name: "(1+2) > 0",
			pred: predicates.NewComparisonPredicate(add12,
				predicates.Comparison{Type: predicates.ComparisonGreaterThan, Operand: &values.ConstantValue{Value: int64(0), Typ: values.TypeInt}}),
		},
		{
			name: "0 < (1+2)",
			pred: predicates.NewComparisonPredicate(
				&values.ConstantValue{Value: int64(0), Typ: values.TypeInt},
				predicates.Comparison{Type: predicates.ComparisonLessThan, Operand: add12}),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := Simplify(tc.pred, DefaultSimplifyRules())
			cp, ok := got.(*predicates.ConstantPredicate)
			if !ok {
				t.Fatalf("expected ConstantPredicate, got %T: %s", got, got.Explain())
			}
			if cp.Value != predicates.TriTrue {
				t.Fatalf("got %v, want TRUE", cp.Value)
			}
		})
	}
}

// Non-constant RHS comparisons survive the fixpoint untouched —
// they aren't foldable at plan time (RHS is a row-dependent Value).
// Pins that ComparisonConstantSimplifyRule's IsConstantValue gate
// declines correctly when RHS is a FieldValue.
func TestSimplify_NonConstantRHS_Survives(t *testing.T) {
	t.Parallel()
	pred := predicates.NewComparisonPredicate(
		&values.FieldValue{Field: "age", Typ: values.TypeInt},
		predicates.Comparison{Type: predicates.ComparisonEquals, Operand: &values.FieldValue{Field: "cutoff", Typ: values.TypeInt}},
	)
	got := Simplify(pred, DefaultSimplifyRules())
	cp, ok := got.(*predicates.ComparisonPredicate)
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
	pred := predicates.NewNot(predicates.NewComparisonPredicate(
		&values.FieldValue{Field: "a", Typ: values.TypeInt},
		predicates.Comparison{Type: predicates.ComparisonEquals, Operand: &values.FieldValue{Field: "b", Typ: values.TypeInt}},
	))
	got := Simplify(pred, DefaultSimplifyRules())
	cp, ok := got.(*predicates.ComparisonPredicate)
	if !ok {
		t.Fatalf("expected NOT to be pushed past comparison, got %T: %s", got, got.Explain())
	}
	if cp.Comparison.Type != predicates.ComparisonNotEquals {
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
	leaf := predicates.NewValuePredicate(&values.FieldValue{Field: "is_active", Typ: values.TypeBool})
	// NOT(AND(TRUE, leaf)) → inner AND folds to leaf → NOT(leaf).
	pred := predicates.NewNot(predicates.NewAnd(predicates.NewConstantPredicate(predicates.TriTrue), leaf))
	got := Simplify(pred, DefaultSimplifyRules())
	not, ok := got.(*predicates.NotPredicate)
	if !ok {
		t.Fatalf("expected NotPredicate, got %T: %s", got, got.Explain())
	}
	if not.Child != predicates.QueryPredicate(leaf) {
		t.Fatalf("expected NOT(leaf), got NOT(%T): %s", not.Child, got.Explain())
	}
}

// Triple-NOT collapse: NOT(NOT(NOT(x = 5))) → x <> 5. Exercises
// NotConstantSimplifyRule's double-neg elimination + the new
// NotComparisonRewriteRule cooperating across the fixpoint.
func TestSimplify_TripleNotCollapses(t *testing.T) {
	t.Parallel()
	age := &values.FieldValue{Field: "age", Typ: values.TypeInt}
	cp := predicates.NewComparisonPredicate(age, predicates.Comparison{Type: predicates.ComparisonEquals, Operand: values.LiteralValue(int64(5))})
	got := Simplify(
		predicates.NewNot(predicates.NewNot(predicates.NewNot(cp))),
		DefaultSimplifyRules(),
	)
	out, ok := got.(*predicates.ComparisonPredicate)
	if !ok {
		t.Fatalf("expected ComparisonPredicate, got %T %s", got, got.Explain())
	}
	if out.Comparison.Type != predicates.ComparisonNotEquals {
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
	age := &values.FieldValue{Field: "age", Typ: values.TypeInt}
	samples := []predicates.QueryPredicate{
		// Fully simplifiable → collapses to a constant.
		predicates.NewAnd(predicates.NewConstantPredicate(predicates.TriTrue), predicates.NewConstantPredicate(predicates.TriFalse)),
		// Partially simplifiable → surviving field predicate.
		predicates.NewAnd(
			predicates.NewComparisonPredicate(age, predicates.Comparison{Type: predicates.ComparisonGreaterThanEq, Operand: values.LiteralValue(int64(18))}),
			predicates.NewConstantPredicate(predicates.TriTrue),
		),
		// Exercises absorption: p AND (p OR q) → p.
		func() predicates.QueryPredicate {
			p := predicates.NewComparisonPredicate(age, predicates.Comparison{Type: predicates.ComparisonGreaterThanEq, Operand: values.LiteralValue(int64(18))})
			q := predicates.NewComparisonPredicate(age, predicates.Comparison{Type: predicates.ComparisonLessThan, Operand: values.LiteralValue(int64(65))})
			return predicates.NewAnd(p, predicates.NewOr(p, q))
		}(),
		// NOT-rewrite: NOT(x = 1) → x <> 1.
		predicates.NewNot(predicates.NewComparisonPredicate(age, predicates.Comparison{Type: predicates.ComparisonEquals, Operand: values.LiteralValue(int64(1))})),
		// Opaque: should be identity.
		predicates.NewValuePredicate(&values.FieldValue{Field: "flag", Typ: values.TypeBool}),
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
	u := predicates.NewConstantPredicate(predicates.TriUnknown)
	T := predicates.NewConstantPredicate(predicates.TriTrue)
	F := predicates.NewConstantPredicate(predicates.TriFalse)
	rules := DefaultSimplifyRules()

	// AND(TRUE, UNKNOWN) → UNKNOWN (TRUE is identity, UNKNOWN survives).
	if got := Simplify(predicates.NewAnd(T, u), rules); got != predicates.QueryPredicate(u) {
		t.Fatalf("AND(T,U): got %T %s", got, got.Explain())
	}
	// AND(FALSE, UNKNOWN) → FALSE (short-circuit).
	got := Simplify(predicates.NewAnd(F, u), rules)
	if cp, ok := got.(*predicates.ConstantPredicate); !ok || cp.Value != predicates.TriFalse {
		t.Fatalf("AND(F,U): got %T %s", got, got.Explain())
	}
	// OR(TRUE, UNKNOWN) → TRUE (short-circuit).
	got = Simplify(predicates.NewOr(T, u), rules)
	if cp, ok := got.(*predicates.ConstantPredicate); !ok || cp.Value != predicates.TriTrue {
		t.Fatalf("OR(T,U): got %T %s", got, got.Explain())
	}
	// OR(FALSE, UNKNOWN) → UNKNOWN (FALSE is identity).
	if got := Simplify(predicates.NewOr(F, u), rules); got != predicates.QueryPredicate(u) {
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
	agePred := predicates.NewComparisonPredicate(
		&values.FieldValue{Field: "age", Typ: values.TypeInt},
		predicates.Comparison{Type: predicates.ComparisonGreaterThanEq, Operand: values.LiteralValue(int64(18))},
	)
	pred := predicates.NewAnd(
		predicates.NewComparisonPredicate(
			&values.ConstantValue{Value: int64(5), Typ: values.TypeInt},
			predicates.Comparison{Type: predicates.ComparisonEquals, Operand: values.LiteralValue(int64(5))},
		),
		predicates.NewComparisonPredicate(
			&values.ConstantValue{Value: int64(3), Typ: values.TypeInt},
			predicates.Comparison{Type: predicates.ComparisonGreaterThan, Operand: values.LiteralValue(int64(1))},
		),
		agePred,
	)
	got := Simplify(pred, DefaultSimplifyRules())
	if got != predicates.QueryPredicate(agePred) {
		t.Fatalf("expected the age predicate to survive, got %T %s", got, got.Explain())
	}
}

// TestSimplify_NotOverOrDoesNotDistribute pins the documented
// SEPARATION: De Morgan's NOT distribution is INTENTIONALLY left out
// of DefaultSimplifyRules. Java's QueryPredicateTest.testQueryPredicate
// NotPushDownOptimization rewrites `NOT(OR(p1, p2))` to `AND(NOT p1,
// NOT p2)`; our seed leaves the NOT on top of the OR.
//
// Java does the De Morgan distribution in a separate normalisation
// pass (BooleanNormalizer); the seed Simplify driver runs only the
// constant-fold + identity-drop + absorbing-element + leaf-NOT-
// rewrite rules. Callers wanting the De Morgan rewrite use the
// `NormalizationRules()` rule set (which prepends `NewDeMorganRule`).
// See `rule_demorgan.go` + `rule_demorgan_test.go`.
func TestSimplify_NotOverOrDoesNotDistribute(t *testing.T) {
	t.Parallel()
	a := &values.FieldValue{Field: "a", Typ: values.TypeString}
	b := &values.FieldValue{Field: "b", Typ: values.TypeString}
	p1 := predicates.NewComparisonPredicate(a, predicates.Comparison{Type: predicates.ComparisonEquals, Operand: values.LiteralValue("x")})
	p2 := predicates.NewComparisonPredicate(b, predicates.Comparison{Type: predicates.ComparisonEquals, Operand: values.LiteralValue("y")})

	pred := predicates.NewNot(predicates.NewOr(p1, p2))
	got := Simplify(pred, DefaultSimplifyRules())

	// CURRENT behaviour: NOT(OR(p1, p2)) survives unchanged.
	notP, ok := got.(*predicates.NotPredicate)
	if !ok {
		t.Fatalf("expected NotPredicate (no De Morgan), got %T: %s", got, got.Explain())
	}
	if _, ok := notP.Child.(*predicates.OrPredicate); !ok {
		t.Fatalf("NOT child should still be OR, got %T", notP.Child)
	}
}

// TestSimplify_OrOfPredicateAndFalse pins the SQL identity-law fold
// that Java's testQueryPredicateIdentityLawOptimization exercises:
// `OR(p, FALSE) → p`. Our OrConstantSimplifyRule must reduce the OR
// to its surviving member.
func TestSimplify_OrOfPredicateAndFalse(t *testing.T) {
	t.Parallel()
	a := &values.FieldValue{Field: "a", Typ: values.TypeString}
	p1 := predicates.NewComparisonPredicate(a, predicates.Comparison{Type: predicates.ComparisonEquals, Operand: values.LiteralValue("Hello")})

	pred := predicates.NewOr(p1, predicates.NewConstantPredicate(predicates.TriFalse))
	got := Simplify(pred, DefaultSimplifyRules())

	// Identity law: OR(p, FALSE) collapses to p.
	if got != predicates.QueryPredicate(p1) {
		t.Fatalf("expected p1 to survive (Java identity-law optimization), got %T: %s", got, got.Explain())
	}
}

// TestSimplify_AndOfPredicateAndTrue: dual identity law. AND(p, TRUE)
// → p. Symmetric to OrOfPredicateAndFalse and pins our equivalent of
// Java's identity-law coverage on the AND side.
func TestSimplify_AndOfPredicateAndTrue(t *testing.T) {
	t.Parallel()
	a := &values.FieldValue{Field: "a", Typ: values.TypeString}
	p1 := predicates.NewComparisonPredicate(a, predicates.Comparison{Type: predicates.ComparisonEquals, Operand: values.LiteralValue("Hello")})

	pred := predicates.NewAnd(p1, predicates.NewConstantPredicate(predicates.TriTrue))
	got := Simplify(pred, DefaultSimplifyRules())

	if got != predicates.QueryPredicate(p1) {
		t.Fatalf("expected p1 to survive (AND TRUE identity), got %T: %s", got, got.Explain())
	}
}
