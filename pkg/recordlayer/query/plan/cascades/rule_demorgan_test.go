package cascades

import (
	"fmt"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func mustDeMorganConstruct[T any](value T, err error) T {
	if err != nil {
		panic("construct De Morgan fixture: " + err.Error())
	}
	return value
}

func demorganField(name string, typ values.Type) values.Value {
	rowType := values.NewRecordType("DeMorgan", false, []values.Field{
		{Name: name, FieldType: typ},
	})
	root := mustDeMorganConstruct(values.NewQuantifiedObjectValue(
		values.NamedCorrelationIdentifier("demorgan_"+name), rowType))
	return mustDeMorganConstruct(values.ResolveFieldOrdinals(root, []int{0}))
}

func demorganLiteral(literal any) values.Value {
	var typ values.Type
	switch literal.(type) {
	case int, int32, int64:
		typ = values.NotNullLong
	case string:
		typ = values.NotNullString
	default:
		panic(fmt.Sprintf("unsupported De Morgan literal type %T", literal))
	}
	return &values.ConstantValue{Value: literal, Typ: typ}
}

// TestDeMorgan_NotOverAnd pins the canonical case:
//
//	NOT(AND(p1, p2)) → OR(NOT p1, NOT p2)
//
// Mirrors Java's testQueryPredicateNotPushDownOptimization.
func TestDeMorgan_NotOverAnd(t *testing.T) {
	t.Parallel()
	rule := NewDeMorganRule()
	a := demorganField("a", values.TypeString)
	b := demorganField("b", values.TypeString)
	p1 := predicates.NewComparisonPredicate(a, predicates.Comparison{Type: predicates.ComparisonEquals, Operand: demorganLiteral("Hello")})
	p2 := predicates.NewComparisonPredicate(b, predicates.Comparison{Type: predicates.ComparisonEquals, Operand: demorganLiteral("World")})
	pred := predicates.NewNot(predicates.NewAnd(p1, p2))

	got := mustFireRule(t, rule, pred)
	if len(got) != 1 {
		t.Fatalf("expected 1 yield, got %d", len(got))
	}
	or, ok := got[0].(*predicates.OrPredicate)
	if !ok {
		t.Fatalf("expected OrPredicate, got %T", got[0])
	}
	if len(or.SubPredicates) != 2 {
		t.Fatalf("expected 2 children, got %d", len(or.SubPredicates))
	}
	for i, sp := range or.SubPredicates {
		not, ok := sp.(*predicates.NotPredicate)
		if !ok {
			t.Fatalf("child %d: expected NotPredicate, got %T", i, sp)
		}
		// The wrapped child should be the original predicate.
		want := []predicates.QueryPredicate{p1, p2}[i]
		if not.Child != want {
			t.Fatalf("child %d: NOT-wrapped wrong predicate", i)
		}
	}
}

// TestDeMorgan_NotOverOr pins the symmetric:
//
//	NOT(OR(p1, p2)) → AND(NOT p1, NOT p2)
func TestDeMorgan_NotOverOr(t *testing.T) {
	t.Parallel()
	rule := NewDeMorganRule()
	a := demorganField("a", values.TypeString)
	b := demorganField("b", values.TypeString)
	p1 := predicates.NewComparisonPredicate(a, predicates.Comparison{Type: predicates.ComparisonEquals, Operand: demorganLiteral("x")})
	p2 := predicates.NewComparisonPredicate(b, predicates.Comparison{Type: predicates.ComparisonEquals, Operand: demorganLiteral("y")})
	pred := predicates.NewNot(predicates.NewOr(p1, p2))

	got := mustFireRule(t, rule, pred)
	if len(got) != 1 {
		t.Fatalf("expected 1 yield, got %d", len(got))
	}
	and, ok := got[0].(*predicates.AndPredicate)
	if !ok {
		t.Fatalf("expected AndPredicate, got %T", got[0])
	}
	if len(and.SubPredicates) != 2 {
		t.Fatalf("expected 2 children, got %d", len(and.SubPredicates))
	}
	for _, sp := range and.SubPredicates {
		if _, ok := sp.(*predicates.NotPredicate); !ok {
			t.Fatalf("expected NOT-wrapped child, got %T", sp)
		}
	}
}

// TestDeMorgan_NotOverLeaf_DoesNotFire pins that the rule declines on
// NOT-over-leaf — that's NotComparisonRewriteRule's job.
func TestDeMorgan_NotOverLeaf_DoesNotFire(t *testing.T) {
	t.Parallel()
	rule := NewDeMorganRule()
	a := demorganField("a", values.TypeString)
	pred := predicates.NewNot(predicates.NewComparisonPredicate(a, predicates.Comparison{Type: predicates.ComparisonEquals, Operand: demorganLiteral("x")}))
	if got := mustFireRule(t, rule, pred); len(got) != 0 {
		t.Fatalf("expected no yield (leaf child), got %d yields", len(got))
	}
}

// TestDeMorgan_NestedNot_DoesNotFire pins that NOT(NOT(p)) is also out
// of scope — that's NotConstantSimplifyRule's double-negation case.
// De Morgan only fires on AND/OR children.
func TestDeMorgan_NestedNot_DoesNotFire(t *testing.T) {
	t.Parallel()
	rule := NewDeMorganRule()
	a := demorganField("a", values.TypeString)
	leaf := predicates.NewComparisonPredicate(a, predicates.Comparison{Type: predicates.ComparisonEquals, Operand: demorganLiteral("x")})
	pred := predicates.NewNot(predicates.NewNot(leaf))
	if got := mustFireRule(t, rule, pred); len(got) != 0 {
		t.Fatalf("expected no yield (NOT child), got %d yields", len(got))
	}
}

// TestDeMorgan_PreservesOrder pins that the negated children appear
// in the same order as the original — important for diff stability
// and rule ordering.
func TestDeMorgan_PreservesOrder(t *testing.T) {
	t.Parallel()
	rule := NewDeMorganRule()
	a := demorganField("a", values.NullableLong)
	b := demorganField("b", values.NullableLong)
	c := demorganField("c", values.NullableLong)
	p1 := predicates.NewComparisonPredicate(a, predicates.Comparison{Type: predicates.ComparisonEquals, Operand: demorganLiteral(int64(1))})
	p2 := predicates.NewComparisonPredicate(b, predicates.Comparison{Type: predicates.ComparisonEquals, Operand: demorganLiteral(int64(2))})
	p3 := predicates.NewComparisonPredicate(c, predicates.Comparison{Type: predicates.ComparisonEquals, Operand: demorganLiteral(int64(3))})

	pred := predicates.NewNot(predicates.NewAnd(p1, p2, p3))
	got := mustFireRule(t, rule, pred)
	or := got[0].(*predicates.OrPredicate)
	want := []predicates.QueryPredicate{p1, p2, p3}
	for i, sp := range or.SubPredicates {
		not := sp.(*predicates.NotPredicate)
		if not.Child != want[i] {
			t.Fatalf("child %d: out of order", i)
		}
	}
}

// TestNormalizationRules_AppliesDeMorganThenSimplify pins the
// composite contract: NOT(OR(p, FALSE)) under NormalizationRules
// becomes AND(NOT p, NOT FALSE) → AND(NOT p, TRUE) → NOT p →
// NotComparisonRewriteRule applies → p with op-negated.
//
// Concretely: NOT(a = 5 OR FALSE) → a <> 5.
func TestNormalizationRules_AppliesDeMorganThenSimplify(t *testing.T) {
	t.Parallel()
	a := demorganField("a", values.NullableLong)
	cp := predicates.NewComparisonPredicate(a, predicates.Comparison{Type: predicates.ComparisonEquals, Operand: demorganLiteral(int64(5))})

	pred := predicates.NewNot(predicates.NewOr(cp, predicates.NewConstantPredicate(predicates.TriFalse)))
	got := mustSimplify(t, pred, ConstantFoldingRules())

	out, ok := got.(*predicates.ComparisonPredicate)
	if !ok {
		t.Fatalf("expected ComparisonPredicate after full normalisation, got %T: %s", got, got.Explain())
	}
	if out.Comparison.Type != predicates.ComparisonNotEquals {
		t.Fatalf("expected a <> 5, got %s", got.Explain())
	}
}

// TestNormalizationRules_NestedNotDistributesRecursively pins that
// the Simplify driver applies DeMorganRule at every NOT-level it
// reaches:
//
//	NOT(AND(p, OR(q, r)))
//	→ OR(NOT(p), NOT(OR(q, r)))   (top-level DeMorgan)
//	→ OR(NOT(p), AND(NOT(q), NOT(r)))  (inner DeMorgan via child recursion)
//
// Without driver recursion, the inner NOT(OR) would survive. This
// exercises the `recurse into children + re-simplify` arm of
// simplifier.go.
func TestNormalizationRules_NestedNotDistributesRecursively(t *testing.T) {
	t.Parallel()
	a := demorganField("a", values.TypeString)
	b := demorganField("b", values.TypeString)
	c := demorganField("c", values.TypeString)
	p := predicates.NewComparisonPredicate(a, predicates.Comparison{Type: predicates.ComparisonEquals, Operand: demorganLiteral("x")})
	q := predicates.NewComparisonPredicate(b, predicates.Comparison{Type: predicates.ComparisonEquals, Operand: demorganLiteral("y")})
	r := predicates.NewComparisonPredicate(c, predicates.Comparison{Type: predicates.ComparisonEquals, Operand: demorganLiteral("z")})

	pred := predicates.NewNot(predicates.NewAnd(p, predicates.NewOr(q, r)))
	got := mustSimplify(t, pred, ConstantFoldingRules())

	// After full distribution + NotComparisonRewrite:
	// OR(p<>, AND(q<>, r<>))
	or, ok := got.(*predicates.OrPredicate)
	if !ok {
		t.Fatalf("expected OrPredicate at top, got %T %s", got, got.Explain())
	}
	if len(or.SubPredicates) != 2 {
		t.Fatalf("expected 2 OR children, got %d", len(or.SubPredicates))
	}
	// First child: p<>.
	cp1, ok := or.SubPredicates[0].(*predicates.ComparisonPredicate)
	if !ok || cp1.Comparison.Type != predicates.ComparisonNotEquals {
		t.Fatalf("first OR child: expected ComparisonPredicate(<>), got %T %v", or.SubPredicates[0], cp1)
	}
	// Second child: AND(q<>, r<>).
	innerAnd, ok := or.SubPredicates[1].(*predicates.AndPredicate)
	if !ok {
		t.Fatalf("second OR child: expected AndPredicate (inner DeMorgan), got %T", or.SubPredicates[1])
	}
	if len(innerAnd.SubPredicates) != 2 {
		t.Fatalf("inner AND: expected 2 children, got %d", len(innerAnd.SubPredicates))
	}
	for i, sp := range innerAnd.SubPredicates {
		cp, ok := sp.(*predicates.ComparisonPredicate)
		if !ok || cp.Comparison.Type != predicates.ComparisonNotEquals {
			t.Fatalf("inner AND child %d: expected ComparisonPredicate(<>), got %T", i, sp)
		}
	}
}

// wantNotOverConstant asserts got is NOT over the constant v: Java's
// ConstantFoldingRuleSet folds the leaf but has no rule for NOT over a
// constant predicate (NotPredicate.not is a plain constructor, and
// NotOverComparisonRule matches only a comparison child).
func wantNotOverConstant(t *testing.T, got predicates.QueryPredicate, v predicates.TriBool) {
	t.Helper()
	not, ok := got.(*predicates.NotPredicate)
	if !ok {
		t.Fatalf("expected NOT over a constant, got %T %s", got, got.Explain())
	}
	if cp, ok := not.Child.(*predicates.ConstantPredicate); !ok || cp.Value != v {
		t.Fatalf("expected NOT (%v), got %s", v, got.Explain())
	}
}

// TestNormalizationRules_VPConstantFoldChain pins the leaf fold under a NOT:
// NOT(VP(TRUE)) → NOT(TRUE), the boolean value predicate folded as Java's
// ValuePredicate(value, EQUALS TRUE), and the NOT kept.
func TestNormalizationRules_VPConstantFoldChain(t *testing.T) {
	t.Parallel()
	pred := predicates.NewNot(predicates.NewValuePredicate(values.NewBooleanValue(true)))
	wantNotOverConstant(t, mustSimplify(t, pred, ConstantFoldingRules()), predicates.TriTrue)
}

// TestNormalizationRules_DeMorganIntoVPFold pins the order the driver folds
// in: children first, so NOT(AND(VP(true), VP(false))) folds its AND to FALSE
// (the leaves fold, FALSE annuls) before De Morgan could distribute, and the
// NOT over that constant stays.
func TestNormalizationRules_DeMorganIntoVPFold(t *testing.T) {
	t.Parallel()
	pred := predicates.NewNot(predicates.NewAnd(
		predicates.NewValuePredicate(values.NewBooleanValue(true)),
		predicates.NewValuePredicate(values.NewBooleanValue(false)),
	))
	wantNotOverConstant(t, mustSimplify(t, pred, ConstantFoldingRules()), predicates.TriFalse)
}

// TestNormalizationRules_DeMorganMixed pins the same order over a mixed AND:
// NOT(AND(a = 5, VP(false))) annuls its AND to FALSE first, so the result is
// NOT(FALSE), not De Morgan's OR(a <> 5, TRUE).
func TestNormalizationRules_DeMorganMixed(t *testing.T) {
	t.Parallel()
	a := demorganField("a", values.NullableLong)
	cp := predicates.NewComparisonPredicate(a, predicates.Comparison{Type: predicates.ComparisonEquals, Operand: demorganLiteral(int64(5))})
	pred := predicates.NewNot(predicates.NewAnd(cp, predicates.NewValuePredicate(values.NewBooleanValue(false))))
	wantNotOverConstant(t, mustSimplify(t, pred, ConstantFoldingRules()), predicates.TriFalse)
}

// TestNormalizationRules_NotOverAndProducesOr pins that NOT(AND(...))
// distributes to OR(NOT...) under Java's ConstantFoldingRuleSet, whose
// default predicate rules include De Morgan.
func TestNormalizationRules_NotOverAndProducesOr(t *testing.T) {
	t.Parallel()
	a := demorganField("a", values.TypeString)
	b := demorganField("b", values.TypeString)
	p1 := predicates.NewComparisonPredicate(a, predicates.Comparison{Type: predicates.ComparisonEquals, Operand: demorganLiteral("x")})
	p2 := predicates.NewComparisonPredicate(b, predicates.Comparison{Type: predicates.ComparisonEquals, Operand: demorganLiteral("y")})
	pred := predicates.NewNot(predicates.NewAnd(p1, p2))

	// Distributes into OR(NOT, NOT) ->
	// NotComparisonRewriteRule then turns each NOT(=) into <>, so
	// final shape is OR(<>, <>).
	normGot := mustSimplify(t, pred, ConstantFoldingRules())
	or, ok := normGot.(*predicates.OrPredicate)
	if !ok {
		t.Fatalf("normalisation rules: expected OrPredicate, got %T: %s", normGot, normGot.Explain())
	}
	for i, sp := range or.SubPredicates {
		cp, ok := sp.(*predicates.ComparisonPredicate)
		if !ok {
			t.Fatalf("child %d: expected ComparisonPredicate after NOT-rewrite, got %T", i, sp)
		}
		if cp.Comparison.Type != predicates.ComparisonNotEquals {
			t.Fatalf("child %d: expected <>, got %v", i, cp.Comparison.Type)
		}
	}
}

// TestNormalizationRules_Idempotent mirrors TestSimplify_Idempotent but
// for the larger ConstantFoldingRules() set (DeMorgan + NOT-rewrite + the
// default reductions). Re-running Simplify on its own output must be a
// no-op on the same pointer — anything else means a rule loops on its
// own stable input or the driver's pointer-equality break-out is broken
// for this rule set.
func TestNormalizationRules_Idempotent(t *testing.T) {
	t.Parallel()
	rules := ConstantFoldingRules()
	a := demorganField("a", values.TypeString)
	b := demorganField("b", values.TypeString)
	age := demorganField("age", values.NullableLong)
	samples := []predicates.QueryPredicate{
		// DeMorgan-then-NOT-rewrite: NOT(AND(a=x, b=y)) → OR(a<>x, b<>y).
		predicates.NewNot(predicates.NewAnd(
			predicates.NewComparisonPredicate(a, predicates.Comparison{Type: predicates.ComparisonEquals, Operand: demorganLiteral("x")}),
			predicates.NewComparisonPredicate(b, predicates.Comparison{Type: predicates.ComparisonEquals, Operand: demorganLiteral("y")}),
		)),
		// VP-fold chain: NOT(VP(true)) → ConstantPredicate(FALSE).
		predicates.NewNot(predicates.NewValuePredicate(values.NewBooleanValue(true))),
		// Mixed-shape DeMorgan + VP-fold + AND-identity collapse to TRUE.
		predicates.NewNot(predicates.NewAnd(
			predicates.NewComparisonPredicate(age, predicates.Comparison{Type: predicates.ComparisonEquals, Operand: demorganLiteral(int64(5))}),
			predicates.NewValuePredicate(values.NewBooleanValue(false)),
		)),
		// Opaque field VP: identity (no rule fires).
		predicates.NewValuePredicate(demorganField("flag", values.TypeBool)),
	}
	for _, s := range samples {
		once := mustSimplify(t, s, rules)
		twice := mustSimplify(t, once, rules)
		if once != twice {
			t.Fatalf("not idempotent for %s: once=%s twice=%s",
				s.Explain(), once.Explain(), twice.Explain())
		}
	}
}
