package cascades

import "testing"

// TestDeMorgan_NotOverAnd pins the canonical case:
//
//	NOT(AND(p1, p2)) → OR(NOT p1, NOT p2)
//
// Mirrors Java's testQueryPredicateNotPushDownOptimization.
func TestDeMorgan_NotOverAnd(t *testing.T) {
	t.Parallel()
	rule := NewDeMorganRule()
	a := &FieldValue{Field: "a", Typ: TypeString}
	b := &FieldValue{Field: "b", Typ: TypeString}
	p1 := NewComparisonPredicate(a, Comparison{Type: ComparisonEquals, Operand: LiteralValue("Hello")})
	p2 := NewComparisonPredicate(b, Comparison{Type: ComparisonEquals, Operand: LiteralValue("World")})
	pred := NewNot(NewAnd(p1, p2))

	got := FireRule(rule, pred)
	if len(got) != 1 {
		t.Fatalf("expected 1 yield, got %d", len(got))
	}
	or, ok := got[0].(*OrPredicate)
	if !ok {
		t.Fatalf("expected OrPredicate, got %T", got[0])
	}
	if len(or.SubPredicates) != 2 {
		t.Fatalf("expected 2 children, got %d", len(or.SubPredicates))
	}
	for i, sp := range or.SubPredicates {
		not, ok := sp.(*NotPredicate)
		if !ok {
			t.Fatalf("child %d: expected NotPredicate, got %T", i, sp)
		}
		// The wrapped child should be the original predicate.
		want := []QueryPredicate{p1, p2}[i]
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
	a := &FieldValue{Field: "a", Typ: TypeString}
	b := &FieldValue{Field: "b", Typ: TypeString}
	p1 := NewComparisonPredicate(a, Comparison{Type: ComparisonEquals, Operand: LiteralValue("x")})
	p2 := NewComparisonPredicate(b, Comparison{Type: ComparisonEquals, Operand: LiteralValue("y")})
	pred := NewNot(NewOr(p1, p2))

	got := FireRule(rule, pred)
	if len(got) != 1 {
		t.Fatalf("expected 1 yield, got %d", len(got))
	}
	and, ok := got[0].(*AndPredicate)
	if !ok {
		t.Fatalf("expected AndPredicate, got %T", got[0])
	}
	if len(and.SubPredicates) != 2 {
		t.Fatalf("expected 2 children, got %d", len(and.SubPredicates))
	}
	for _, sp := range and.SubPredicates {
		if _, ok := sp.(*NotPredicate); !ok {
			t.Fatalf("expected NOT-wrapped child, got %T", sp)
		}
	}
}

// TestDeMorgan_NotOverLeaf_DoesNotFire pins that the rule declines on
// NOT-over-leaf — that's NotComparisonRewriteRule's job.
func TestDeMorgan_NotOverLeaf_DoesNotFire(t *testing.T) {
	t.Parallel()
	rule := NewDeMorganRule()
	a := &FieldValue{Field: "a", Typ: TypeString}
	pred := NewNot(NewComparisonPredicate(a, Comparison{Type: ComparisonEquals, Operand: LiteralValue("x")}))
	if got := FireRule(rule, pred); len(got) != 0 {
		t.Fatalf("expected no yield (leaf child), got %d yields", len(got))
	}
}

// TestDeMorgan_NestedNot_DoesNotFire pins that NOT(NOT(p)) is also out
// of scope — that's NotConstantSimplifyRule's double-negation case.
// De Morgan only fires on AND/OR children.
func TestDeMorgan_NestedNot_DoesNotFire(t *testing.T) {
	t.Parallel()
	rule := NewDeMorganRule()
	a := &FieldValue{Field: "a", Typ: TypeString}
	leaf := NewComparisonPredicate(a, Comparison{Type: ComparisonEquals, Operand: LiteralValue("x")})
	pred := NewNot(NewNot(leaf))
	if got := FireRule(rule, pred); len(got) != 0 {
		t.Fatalf("expected no yield (NOT child), got %d yields", len(got))
	}
}

// TestDeMorgan_PreservesOrder pins that the negated children appear
// in the same order as the original — important for diff stability
// and rule ordering.
func TestDeMorgan_PreservesOrder(t *testing.T) {
	t.Parallel()
	rule := NewDeMorganRule()
	a := &FieldValue{Field: "a", Typ: TypeInt}
	b := &FieldValue{Field: "b", Typ: TypeInt}
	c := &FieldValue{Field: "c", Typ: TypeInt}
	p1 := NewComparisonPredicate(a, Comparison{Type: ComparisonEquals, Operand: LiteralValue(int64(1))})
	p2 := NewComparisonPredicate(b, Comparison{Type: ComparisonEquals, Operand: LiteralValue(int64(2))})
	p3 := NewComparisonPredicate(c, Comparison{Type: ComparisonEquals, Operand: LiteralValue(int64(3))})

	pred := NewNot(NewAnd(p1, p2, p3))
	got := FireRule(rule, pred)
	or := got[0].(*OrPredicate)
	want := []QueryPredicate{p1, p2, p3}
	for i, sp := range or.SubPredicates {
		not := sp.(*NotPredicate)
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
	a := &FieldValue{Field: "a", Typ: TypeInt}
	cp := NewComparisonPredicate(a, Comparison{Type: ComparisonEquals, Operand: LiteralValue(int64(5))})

	pred := NewNot(NewOr(cp, NewConstantPredicate(TriFalse)))
	got := Simplify(pred, NormalizationRules())

	out, ok := got.(*ComparisonPredicate)
	if !ok {
		t.Fatalf("expected ComparisonPredicate after full normalisation, got %T: %s", got, got.Explain())
	}
	if out.Comparison.Type != ComparisonNotEquals {
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
	a := &FieldValue{Field: "a", Typ: TypeString}
	b := &FieldValue{Field: "b", Typ: TypeString}
	c := &FieldValue{Field: "c", Typ: TypeString}
	p := NewComparisonPredicate(a, Comparison{Type: ComparisonEquals, Operand: LiteralValue("x")})
	q := NewComparisonPredicate(b, Comparison{Type: ComparisonEquals, Operand: LiteralValue("y")})
	r := NewComparisonPredicate(c, Comparison{Type: ComparisonEquals, Operand: LiteralValue("z")})

	pred := NewNot(NewAnd(p, NewOr(q, r)))
	got := Simplify(pred, NormalizationRules())

	// After full distribution + NotComparisonRewrite:
	// OR(p<>, AND(q<>, r<>))
	or, ok := got.(*OrPredicate)
	if !ok {
		t.Fatalf("expected OrPredicate at top, got %T %s", got, got.Explain())
	}
	if len(or.SubPredicates) != 2 {
		t.Fatalf("expected 2 OR children, got %d", len(or.SubPredicates))
	}
	// First child: p<>.
	cp1, ok := or.SubPredicates[0].(*ComparisonPredicate)
	if !ok || cp1.Comparison.Type != ComparisonNotEquals {
		t.Fatalf("first OR child: expected ComparisonPredicate(<>), got %T %v", or.SubPredicates[0], cp1)
	}
	// Second child: AND(q<>, r<>).
	innerAnd, ok := or.SubPredicates[1].(*AndPredicate)
	if !ok {
		t.Fatalf("second OR child: expected AndPredicate (inner DeMorgan), got %T", or.SubPredicates[1])
	}
	if len(innerAnd.SubPredicates) != 2 {
		t.Fatalf("inner AND: expected 2 children, got %d", len(innerAnd.SubPredicates))
	}
	for i, sp := range innerAnd.SubPredicates {
		cp, ok := sp.(*ComparisonPredicate)
		if !ok || cp.Comparison.Type != ComparisonNotEquals {
			t.Fatalf("inner AND child %d: expected ComparisonPredicate(<>), got %T", i, sp)
		}
	}
}

// TestNormalizationRules_VPConstantFoldChain pins the rule
// composition: NOT(VP(constant)) folds via the chain
// ValuePredicateConstantFoldRule + NotConstantSimplifyRule.
//
// Specifically: NOT(VP(BooleanValue(true))) → NOT(TRUE) →
// ConstantPredicate(TriFalse). Both rules need to fire in sequence.
func TestNormalizationRules_VPConstantFoldChain(t *testing.T) {
	t.Parallel()
	pred := NewNot(NewValuePredicate(NewBooleanValue(true)))
	got := Simplify(pred, NormalizationRules())
	cp, ok := got.(*ConstantPredicate)
	if !ok {
		t.Fatalf("expected ConstantPredicate after NOT(VP(true)) fold, got %T %s", got, got.Explain())
	}
	if cp.Value != TriFalse {
		t.Fatalf("expected TriFalse, got %v", cp.Value)
	}
}

// TestNormalizationRules_DeMorganIntoVPFold pins the longer chain:
// NOT(AND(VP(true), VP(false))) — DeMorgan distributes to
// OR(NOT(VP(true)), NOT(VP(false))), then VP folds + NOT
// folds + Or-identity-drop collapse to ConstantPredicate(TriTrue).
//
// Trace:
//
//	NOT(AND(VP(true), VP(false)))
//	→ OR(NOT(VP(true)), NOT(VP(false)))   [DeMorgan]
//	→ OR(NOT(TRUE), NOT(FALSE))           [VPConstantFold ×2]
//	→ OR(FALSE, TRUE)                      [NotConstantSimplify ×2]
//	→ ConstantPredicate(TriTrue)           [OrConstantSimplify, TRUE child]
func TestNormalizationRules_DeMorganIntoVPFold(t *testing.T) {
	t.Parallel()
	pred := NewNot(NewAnd(
		NewValuePredicate(NewBooleanValue(true)),
		NewValuePredicate(NewBooleanValue(false)),
	))
	got := Simplify(pred, NormalizationRules())
	cp, ok := got.(*ConstantPredicate)
	if !ok {
		t.Fatalf("expected ConstantPredicate, got %T %s", got, got.Explain())
	}
	if cp.Value != TriTrue {
		t.Fatalf("expected TriTrue (NOT(true AND false) = NOT(false) = TRUE), got %v", cp.Value)
	}
}

// TestNormalizationRules_DeMorganMixed pins the cross-shape: an AND
// containing a comparison + a constant under NOT exercises DeMorgan
// distributing into both shapes simultaneously.
//
//	NOT(AND(a = 5, VP(false)))
//	→ OR(NOT(a = 5), NOT(VP(false)))     [DeMorgan]
//	→ OR(a <> 5, NOT(FALSE))              [NotComparisonRewrite + VP fold]
//	→ OR(a <> 5, TRUE)                    [NotConstantSimplify]
//	→ ConstantPredicate(TriTrue)          [OrConstantSimplify, TRUE absorbs]
//
// Pins the rule pipeline doesn't fall through any of the 4 transforms.
func TestNormalizationRules_DeMorganMixed(t *testing.T) {
	t.Parallel()
	a := &FieldValue{Field: "a", Typ: TypeInt}
	cp := NewComparisonPredicate(a, Comparison{Type: ComparisonEquals, Operand: LiteralValue(int64(5))})
	pred := NewNot(NewAnd(cp, NewValuePredicate(NewBooleanValue(false))))
	got := Simplify(pred, NormalizationRules())
	out, ok := got.(*ConstantPredicate)
	if !ok {
		t.Fatalf("expected ConstantPredicate (TRUE absorbs), got %T %s", got, got.Explain())
	}
	if out.Value != TriTrue {
		t.Fatalf("expected TriTrue, got %v", out.Value)
	}
}

// TestNormalizationRules_NotOverAndProducesOr pins that NOT(AND(...))
// distributes to OR(NOT...) under the normalisation rule set, while
// the same input under DefaultSimplifyRules survives as NOT(AND(...)).
// The two rule sets producing different shapes is the documented
// behaviour we want.
func TestNormalizationRules_NotOverAndProducesOr(t *testing.T) {
	t.Parallel()
	a := &FieldValue{Field: "a", Typ: TypeString}
	b := &FieldValue{Field: "b", Typ: TypeString}
	p1 := NewComparisonPredicate(a, Comparison{Type: ComparisonEquals, Operand: LiteralValue("x")})
	p2 := NewComparisonPredicate(b, Comparison{Type: ComparisonEquals, Operand: LiteralValue("y")})
	pred := NewNot(NewAnd(p1, p2))

	// Under default rules: NOT(AND(...)) survives.
	defaultGot := Simplify(pred, DefaultSimplifyRules())
	if _, ok := defaultGot.(*NotPredicate); !ok {
		t.Fatalf("default rules: expected NotPredicate (no De Morgan), got %T", defaultGot)
	}

	// Under normalisation rules: distributes into OR(NOT, NOT) ->
	// NotComparisonRewriteRule then turns each NOT(=) into <>, so
	// final shape is OR(<>, <>).
	normGot := Simplify(pred, NormalizationRules())
	or, ok := normGot.(*OrPredicate)
	if !ok {
		t.Fatalf("normalisation rules: expected OrPredicate, got %T: %s", normGot, normGot.Explain())
	}
	for i, sp := range or.SubPredicates {
		cp, ok := sp.(*ComparisonPredicate)
		if !ok {
			t.Fatalf("child %d: expected ComparisonPredicate after NOT-rewrite, got %T", i, sp)
		}
		if cp.Comparison.Type != ComparisonNotEquals {
			t.Fatalf("child %d: expected <>, got %v", i, cp.Comparison.Type)
		}
	}
}
