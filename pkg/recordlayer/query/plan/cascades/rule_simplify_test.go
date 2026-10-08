package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func simplifyField(name string, typ values.Type) values.Value {
	rowType := values.NewRecordType("Simplify_"+name, false, []values.Field{{
		Name: name, FieldType: typ,
	}})
	root := mustTypeRewriteConstruct(values.NewQuantifiedObjectValue(
		values.NamedCorrelationIdentifier("SIMPLIFY_"+name), rowType))
	return mustTypeRewriteConstruct(values.ResolveFieldOrdinals(root, []int{0}))
}

func firePredicateRule(t testing.TB, rule CascadesRule, input any) []any {
	t.Helper()
	result, err := FireRule(rule, input)
	if err != nil {
		t.Fatalf("FireRule: %v", err)
	}
	return result
}

func simplifyPredicate(
	t testing.TB, predicate predicates.QueryPredicate, rules []CascadesRule,
) predicates.QueryPredicate {
	t.Helper()
	result, err := Simplify(predicate, rules)
	if err != nil {
		t.Fatalf("Simplify: %v", err)
	}
	return result
}

var (
	_ CascadesRule = (*AndConstantSimplifyRule)(nil)
	_ CascadesRule = (*OrConstantSimplifyRule)(nil)
	_ CascadesRule = (*NotConstantSimplifyRule)(nil)
	_ CascadesRule = (*AndAbsorbOrRule)(nil)
	_ CascadesRule = (*OrAbsorbAndRule)(nil)
	_ CascadesRule = (*NotComparisonRewriteRule)(nil)
)

func TestNotSimplify_ConstantFold(t *testing.T) {
	t.Parallel()
	rule := NewNotConstantSimplifyRule()
	cases := []struct {
		in   predicates.TriBool
		want predicates.TriBool
	}{
		{predicates.TriTrue, predicates.TriFalse},
		{predicates.TriFalse, predicates.TriTrue},
		{predicates.TriUnknown, predicates.TriUnknown},
	}
	for _, tc := range cases {
		got := firePredicateRule(t, rule, predicates.NewNot(predicates.NewConstantPredicate(tc.in)))
		if len(got) != 1 {
			t.Fatalf("%v: expected 1 replacement, got %d", tc.in, len(got))
		}
		cp, ok := got[0].(*predicates.ConstantPredicate)
		if !ok || cp.Value != tc.want {
			t.Fatalf("%v: got %v, want ConstantPredicate(%v)", tc.in, got[0], tc.want)
		}
	}
}

// NOT NOT x → x (double-negation elimination).
func TestNotSimplify_DoubleNegation(t *testing.T) {
	t.Parallel()
	rule := NewNotConstantSimplifyRule()
	inner := predicates.NewConstantPredicate(predicates.TriUnknown)
	got := firePredicateRule(t, rule, predicates.NewNot(predicates.NewNot(inner)))
	if len(got) != 1 {
		t.Fatalf("expected 1 replacement, got %d", len(got))
	}
	if got[0] != predicates.QueryPredicate(inner) {
		t.Fatalf("double-negation: expected inner predicate, got %T", got[0])
	}
}

// NOT over a non-constant, non-NOT predicate — rule declines.
func TestNotSimplify_NoChange(t *testing.T) {
	t.Parallel()
	rule := NewNotConstantSimplifyRule()
	and := predicates.NewAnd(predicates.NewConstantPredicate(predicates.TriTrue))
	// NewNot(AndPredicate) — inner is neither ConstantPredicate nor
	// another NotPredicate, so NotConstantSimplifyRule declines.
	if got := firePredicateRule(t, rule, predicates.NewNot(and)); len(got) != 0 {
		t.Fatalf("expected 0 yields, got %d", len(got))
	}
}

// AndPredicate with all-TRUE children → TRUE.
func TestAndSimplify_AllTrueToConstant(t *testing.T) {
	t.Parallel()
	rule := NewAndConstantSimplifyRule()
	and := predicates.NewAnd(
		predicates.NewConstantPredicate(predicates.TriTrue),
		predicates.NewConstantPredicate(predicates.TriTrue),
	)
	got := firePredicateRule(t, rule, and)
	if len(got) != 1 {
		t.Fatalf("expected 1 replacement, got %d", len(got))
	}
	cp, ok := got[0].(*predicates.ConstantPredicate)
	if !ok || cp.Value != predicates.TriTrue {
		t.Fatalf("expected ConstantPredicate(TRUE), got %v", got[0])
	}
}

// AndPredicate with a FALSE child → FALSE (short-circuit).
func TestAndSimplify_FalseShortCircuit(t *testing.T) {
	t.Parallel()
	rule := NewAndConstantSimplifyRule()
	and := predicates.NewAnd(
		predicates.NewConstantPredicate(predicates.TriTrue),
		predicates.NewConstantPredicate(predicates.TriFalse),
		predicates.NewConstantPredicate(predicates.TriTrue),
	)
	got := firePredicateRule(t, rule, and)
	if len(got) != 1 {
		t.Fatalf("expected 1 replacement, got %d", len(got))
	}
	cp, ok := got[0].(*predicates.ConstantPredicate)
	if !ok || cp.Value != predicates.TriFalse {
		t.Fatalf("expected ConstantPredicate(FALSE), got %v", got[0])
	}
}

// Drop TRUE children from an AND, leaving the non-trivial children.
func TestAndSimplify_DropTrueChildren(t *testing.T) {
	t.Parallel()
	rule := NewAndConstantSimplifyRule()
	// UNKNOWN is technically a ConstantPredicate too, but the And
	// rule keeps it — only TRUE (identity-drop) and FALSE
	// (absorbing) trigger folds. UNKNOWN-leaf stands in here for
	// any predicate the rule treats as opaque.
	leaf := predicates.NewConstantPredicate(predicates.TriUnknown)
	and := predicates.NewAnd(
		predicates.NewConstantPredicate(predicates.TriTrue),
		leaf,
		predicates.NewConstantPredicate(predicates.TriTrue),
	)
	got := firePredicateRule(t, rule, and)
	if len(got) != 1 {
		t.Fatalf("expected 1 replacement, got %d", len(got))
	}
	// Single non-constant child remains — rule yields it directly.
	if got[0] != predicates.QueryPredicate(leaf) {
		t.Fatalf("expected the UNKNOWN leaf, got %T %v", got[0], got[0])
	}
}

// No constant children → rule declines to yield (idempotent).
func TestAndSimplify_NoChange(t *testing.T) {
	t.Parallel()
	rule := NewAndConstantSimplifyRule()
	leaf := predicates.NewConstantPredicate(predicates.TriUnknown)
	and := predicates.NewAnd(leaf, leaf)
	got := firePredicateRule(t, rule, and)
	if len(got) != 0 {
		t.Fatalf("expected rule to decline (0 yields), got %d", len(got))
	}
}

// OrPredicate with a TRUE child → TRUE.
func TestOrSimplify_TrueShortCircuit(t *testing.T) {
	t.Parallel()
	rule := NewOrConstantSimplifyRule()
	or := predicates.NewOr(
		predicates.NewConstantPredicate(predicates.TriFalse),
		predicates.NewConstantPredicate(predicates.TriTrue),
	)
	got := firePredicateRule(t, rule, or)
	if len(got) != 1 {
		t.Fatalf("expected 1 replacement, got %d", len(got))
	}
	cp, ok := got[0].(*predicates.ConstantPredicate)
	if !ok || cp.Value != predicates.TriTrue {
		t.Fatalf("expected ConstantPredicate(TRUE), got %v", got[0])
	}
}

// Drop FALSE children from an OR, leaving the non-trivial children.
// Symmetric to TestAndSimplify_DropTrueChildren.
func TestOrSimplify_DropFalseChildren(t *testing.T) {
	t.Parallel()
	rule := NewOrConstantSimplifyRule()
	leaf := predicates.NewConstantPredicate(predicates.TriUnknown)
	or := predicates.NewOr(
		predicates.NewConstantPredicate(predicates.TriFalse),
		leaf,
		predicates.NewConstantPredicate(predicates.TriFalse),
	)
	got := firePredicateRule(t, rule, or)
	if len(got) != 1 {
		t.Fatalf("expected 1 replacement, got %d", len(got))
	}
	if got[0] != predicates.QueryPredicate(leaf) {
		t.Fatalf("expected the UNKNOWN leaf, got %T %v", got[0], got[0])
	}
}

// No FALSE children → rule declines. Symmetric to
// TestAndSimplify_NoChange.
func TestOrSimplify_NoChange(t *testing.T) {
	t.Parallel()
	rule := NewOrConstantSimplifyRule()
	leaf := predicates.NewConstantPredicate(predicates.TriUnknown)
	or := predicates.NewOr(leaf, leaf)
	got := firePredicateRule(t, rule, or)
	if len(got) != 0 {
		t.Fatalf("expected rule to decline (0 yields), got %d", len(got))
	}
}

// OrPredicate with all-FALSE children → FALSE.
func TestOrSimplify_AllFalseToConstant(t *testing.T) {
	t.Parallel()
	rule := NewOrConstantSimplifyRule()
	or := predicates.NewOr(
		predicates.NewConstantPredicate(predicates.TriFalse),
		predicates.NewConstantPredicate(predicates.TriFalse),
	)
	got := firePredicateRule(t, rule, or)
	if len(got) != 1 {
		t.Fatalf("expected 1 replacement, got %d", len(got))
	}
	cp, ok := got[0].(*predicates.ConstantPredicate)
	if !ok || cp.Value != predicates.TriFalse {
		t.Fatalf("expected ConstantPredicate(FALSE), got %v", got[0])
	}
}

// Rules do not fire when the input isn't the matcher's type.
func TestAndSimplify_WrongType(t *testing.T) {
	t.Parallel()
	rule := NewAndConstantSimplifyRule()
	// Feed an OrPredicate — AND rule's matcher should bail.
	or := predicates.NewOr(predicates.NewConstantPredicate(predicates.TriTrue))
	if got := firePredicateRule(t, rule, or); len(got) != 0 {
		t.Fatalf("expected AND rule to not fire on OR, got %d yields", len(got))
	}
}

// AndAbsorbOrRule: p AND (p OR q) → drop the OR, leaving just `p`.
func TestAndAbsorbOr_DropsRedundantOrChild(t *testing.T) {
	t.Parallel()
	rule := NewAndAbsorbOrRule()
	p := predicates.NewComparisonPredicate(
		simplifyField("age", values.NullableLong),
		predicates.Comparison{Type: predicates.ComparisonGreaterThanEq, Operand: values.LiteralValue(int64(18))},
	)
	q := predicates.NewComparisonPredicate(
		simplifyField("rank", values.NullableLong),
		predicates.Comparison{Type: predicates.ComparisonGreaterThan, Operand: values.LiteralValue(int64(0))},
	)
	and := predicates.NewAnd(p, predicates.NewOr(p, q))
	got := firePredicateRule(t, rule, and)
	if len(got) != 1 {
		t.Fatalf("expected 1 yield, got %d", len(got))
	}
	if got[0] != predicates.QueryPredicate(p) {
		t.Fatalf("expected p, got %T %v", got[0], got[0])
	}
}

// AndAbsorbOrRule leaves AND alone when no OR child shares an
// operand with a sibling.
// TestAndAbsorbOr_KeepsMultipleSurvivors pins the default arm:
// AND(p, OR(p, q), r) drops OR(p,q) leaving AND(p, r) — TWO
// surviving children, so the rule rebuilds an AndPredicate (case
// default in OnMatch's switch). The DropsRedundantOrChild test
// only exercised the case-1 arm.
func TestAndAbsorbOr_KeepsMultipleSurvivors(t *testing.T) {
	t.Parallel()
	rule := NewAndAbsorbOrRule()
	p := predicates.NewComparisonPredicate(
		simplifyField("age", values.NullableLong),
		predicates.Comparison{Type: predicates.ComparisonGreaterThanEq, Operand: values.LiteralValue(int64(18))},
	)
	q := predicates.NewComparisonPredicate(
		simplifyField("rank", values.NullableLong),
		predicates.Comparison{Type: predicates.ComparisonGreaterThan, Operand: values.LiteralValue(int64(0))},
	)
	r := predicates.NewComparisonPredicate(
		simplifyField("score", values.NullableLong),
		predicates.Comparison{Type: predicates.ComparisonGreaterThan, Operand: values.LiteralValue(int64(50))},
	)
	and := predicates.NewAnd(p, predicates.NewOr(p, q), r) // p AND (p OR q) AND r
	got := firePredicateRule(t, rule, and)
	if len(got) != 1 {
		t.Fatalf("expected 1 yield, got %d", len(got))
	}
	out, ok := got[0].(*predicates.AndPredicate)
	if !ok {
		t.Fatalf("expected AndPredicate, got %T", got[0])
	}
	if len(out.SubPredicates) != 2 {
		t.Fatalf("expected 2 surviving children (p, r), got %d", len(out.SubPredicates))
	}
	if out.SubPredicates[0] != predicates.QueryPredicate(p) || out.SubPredicates[1] != predicates.QueryPredicate(r) {
		t.Fatalf("survivors out of order: got [%T, %T]", out.SubPredicates[0], out.SubPredicates[1])
	}
}

func TestAndAbsorbOr_NoOpWhenNoSharedOperand(t *testing.T) {
	t.Parallel()
	rule := NewAndAbsorbOrRule()
	p := predicates.NewComparisonPredicate(
		simplifyField("age", values.NullableLong),
		predicates.Comparison{Type: predicates.ComparisonGreaterThanEq, Operand: values.LiteralValue(int64(18))},
	)
	q := predicates.NewComparisonPredicate(
		simplifyField("rank", values.NullableLong),
		predicates.Comparison{Type: predicates.ComparisonGreaterThan, Operand: values.LiteralValue(int64(0))},
	)
	r := predicates.NewComparisonPredicate(
		simplifyField("score", values.NullableLong),
		predicates.Comparison{Type: predicates.ComparisonGreaterThan, Operand: values.LiteralValue(int64(50))},
	)
	and := predicates.NewAnd(p, predicates.NewOr(q, r))
	if got := firePredicateRule(t, rule, and); len(got) != 0 {
		t.Fatalf("expected rule to decline, got %d yields", len(got))
	}
}

// OrAbsorbAndRule: p OR (p AND q) → drop the AND, leaving just `p`.
func TestOrAbsorbAnd_DropsRedundantAndChild(t *testing.T) {
	t.Parallel()
	rule := NewOrAbsorbAndRule()
	p := predicates.NewComparisonPredicate(
		simplifyField("age", values.NullableLong),
		predicates.Comparison{Type: predicates.ComparisonGreaterThanEq, Operand: values.LiteralValue(int64(18))},
	)
	q := predicates.NewComparisonPredicate(
		simplifyField("rank", values.NullableLong),
		predicates.Comparison{Type: predicates.ComparisonGreaterThan, Operand: values.LiteralValue(int64(0))},
	)
	or := predicates.NewOr(p, predicates.NewAnd(p, q))
	got := firePredicateRule(t, rule, or)
	if len(got) != 1 {
		t.Fatalf("expected 1 yield, got %d", len(got))
	}
	if got[0] != predicates.QueryPredicate(p) {
		t.Fatalf("expected p, got %T %v", got[0], got[0])
	}
}

func TestAbsorptionUsesWholeMinorSets(t *testing.T) {
	t.Parallel()
	p := predicates.NewComparisonPredicate(simplifyField("age", values.NullableLong), predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(1)))
	q := predicates.NewComparisonPredicate(simplifyField("rank", values.NullableLong), predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(2)))
	r := predicates.NewComparisonPredicate(simplifyField("score", values.NullableLong), predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(3)))
	for _, mode := range []normalFormMode{normalFormCNF, normalFormDNF} {
		t.Run(map[normalFormMode]string{normalFormCNF: "and", normalFormDNF: "or"}[mode], func(t *testing.T) {
			t.Parallel()
			var rule CascadesRule = NewAndAbsorbOrRule()
			if mode == normalFormDNF {
				rule = NewOrAbsorbAndRule()
			}
			small := mode.minorWithChildren([]predicates.QueryPredicate{p, q})
			large := mode.minorWithChildren([]predicates.QueryPredicate{r, q, p, p})
			equal := mode.minorWithChildren([]predicates.QueryPredicate{q, p})
			atomic := predicates.WithAtomicity(mode.minorWithChildren([]predicates.QueryPredicate{q, r}), true)
			for _, tc := range []struct {
				name  string
				terms []predicates.QueryPredicate
				want  []predicates.QueryPredicate
			}{
				{"superset_first", []predicates.QueryPredicate{large, small}, []predicates.QueryPredicate{small}},
				{"subset_first", []predicates.QueryPredicate{small, large}, []predicates.QueryPredicate{small}},
				{"equal_last_survives", []predicates.QueryPredicate{small, r, equal}, []predicates.QueryPredicate{r, equal}},
				{"rebuild_atomic_survivor", []predicates.QueryPredicate{large, small, atomic}, []predicates.QueryPredicate{small, predicates.WithAtomicity(atomic, false)}},
				{"incomparable", []predicates.QueryPredicate{small, atomic}, nil},
			} {
				t.Run(tc.name, func(t *testing.T) {
					t.Parallel()
					got := firePredicateRule(t, rule, mode.majorWithChildren(tc.terms))
					if tc.want == nil {
						if len(got) != 0 {
							t.Fatal("incomparable clauses were absorbed")
						}
						return
					}
					if len(got) != 1 {
						t.Fatalf("yields = %d, want 1", len(got))
					}
					result := got[0].(predicates.QueryPredicate)
					assertSimplificationTree(t, result, mode.majorWithChildren(tc.want))
				})
			}
		})
	}
}

// End-to-end through Simplify: a classic absorption plus flatten +
// dedup cooperation.
func TestSimplify_Absorption_EndToEnd(t *testing.T) {
	t.Parallel()
	p := predicates.NewComparisonPredicate(
		simplifyField("age", values.NullableLong),
		predicates.Comparison{Type: predicates.ComparisonGreaterThanEq, Operand: values.LiteralValue(int64(18))},
	)
	q := predicates.NewComparisonPredicate(
		simplifyField("rank", values.NullableLong),
		predicates.Comparison{Type: predicates.ComparisonGreaterThan, Operand: values.LiteralValue(int64(0))},
	)
	// AND(p, OR(p, q), TRUE) → AND(p, TRUE) → p.
	pred := predicates.NewAnd(
		p,
		predicates.NewOr(p, q),
		predicates.NewConstantPredicate(predicates.TriTrue),
	)
	got := simplifyPredicate(t, pred, ConstantFoldingRules())
	if got != predicates.QueryPredicate(p) {
		t.Fatalf("expected p to survive, got %T %s", got, got.Explain())
	}
}

// NotComparisonRewriteRule: NOT(x = 5) → x <> 5.
func TestNotComparisonRewrite_NegatesEquals(t *testing.T) {
	t.Parallel()
	rule := NewNotComparisonRewriteRule()
	cp := predicates.NewComparisonPredicate(
		simplifyField("age", values.NullableLong),
		predicates.Comparison{Type: predicates.ComparisonEquals, Operand: values.LiteralValue(int64(5))},
	)
	got := firePredicateRule(t, rule, predicates.NewNot(cp))
	if len(got) != 1 {
		t.Fatalf("expected 1 yield, got %d", len(got))
	}
	out, ok := got[0].(*predicates.ComparisonPredicate)
	if !ok {
		t.Fatalf("expected ComparisonPredicate, got %T", got[0])
	}
	if out.Comparison.Type != predicates.ComparisonNotEquals {
		t.Fatalf("got %s, want <>", out.Comparison.Type.Symbol())
	}
	rhsLit, ok := values.EvaluateConstant(out.Comparison.Operand)
	if !ok || rhsLit != int64(5) {
		t.Fatalf("operand changed: got %v", out.Comparison.Operand)
	}
}

// NOT(x IS NULL) is NOT invertible — Java's invertComparisonType opens with
// `if (type.isUnary()) return null;`, so the rule declines even though
// IS NOT NULL is plainly the negation. The rule must leave the NOT in place.
func TestNotComparisonRewrite_IsNullDeclines(t *testing.T) {
	t.Parallel()
	rule := NewNotComparisonRewriteRule()
	cp := predicates.NewComparisonPredicate(
		simplifyField("email", values.TypeString),
		predicates.Comparison{Type: predicates.ComparisonIsNull},
	)
	if got := firePredicateRule(t, rule, predicates.NewNot(cp)); len(got) != 0 {
		t.Fatalf("expected rule to decline for IS NULL, got %d yields", len(got))
	}
}

// NOT(x IN (...)) declines — IN has no direct-negation type, the
// NOT must stay as a wrapper.
func TestNotComparisonRewrite_InDeclines(t *testing.T) {
	t.Parallel()
	rule := NewNotComparisonRewriteRule()
	cp := predicates.NewComparisonPredicate(
		simplifyField("age", values.NullableLong),
		predicates.Comparison{Type: predicates.ComparisonIn, Operand: values.LiteralValue([]any{int64(1), int64(2)})},
	)
	if got := firePredicateRule(t, rule, predicates.NewNot(cp)); len(got) != 0 {
		t.Fatalf("expected rule to decline, got %d yields", len(got))
	}
}

// NOT(<non-comparison>) declines — rule is comparison-specific.
func TestNotComparisonRewrite_NonComparisonDeclines(t *testing.T) {
	t.Parallel()
	rule := NewNotComparisonRewriteRule()
	inner := predicates.NewAnd(predicates.NewConstantPredicate(predicates.TriTrue), predicates.NewConstantPredicate(predicates.TriFalse))
	if got := firePredicateRule(t, rule, predicates.NewNot(inner)); len(got) != 0 {
		t.Fatalf("expected rule to decline on NOT(AND), got %d yields", len(got))
	}
}

// End-to-end: NOT(age = 18) fixes up through the simplifier to
// `age <> 18` and the outer NOT vanishes.
func TestSimplify_NotComparisonEndToEnd(t *testing.T) {
	t.Parallel()
	age := simplifyField("age", values.NullableLong)
	got := simplifyPredicate(t,
		predicates.NewNot(predicates.NewComparisonPredicate(age, predicates.Comparison{Type: predicates.ComparisonGreaterThan, Operand: values.LiteralValue(int64(18))})),
		ConstantFoldingRules(),
	)
	cp, ok := got.(*predicates.ComparisonPredicate)
	if !ok {
		t.Fatalf("expected ComparisonPredicate, got %T %s", got, got.Explain())
	}
	if cp.Comparison.Type != predicates.ComparisonLessThanOrEq {
		t.Fatalf("expected age <= 18, got %s", got.Explain())
	}
}
