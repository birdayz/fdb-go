package cascades

import "testing"

// TestValuePredicateConstantFold_True pins ConstantValue(true) →
// ConstantPredicate(TriTrue).
func TestValuePredicateConstantFold_True(t *testing.T) {
	t.Parallel()
	rule := NewValuePredicateConstantFoldRule()
	pred := NewValuePredicate(&ConstantValue{Value: true, Typ: TypeBool})
	got := FireRule(rule, pred)
	if len(got) != 1 {
		t.Fatalf("expected 1 yield, got %d", len(got))
	}
	cp := got[0].(*ConstantPredicate)
	if cp.Value != TriTrue {
		t.Fatalf("got %v, want TriTrue", cp.Value)
	}
}

// TestValuePredicateConstantFold_False pins the FALSE case.
func TestValuePredicateConstantFold_False(t *testing.T) {
	t.Parallel()
	rule := NewValuePredicateConstantFoldRule()
	pred := NewValuePredicate(&ConstantValue{Value: false, Typ: TypeBool})
	got := FireRule(rule, pred)
	cp := got[0].(*ConstantPredicate)
	if cp.Value != TriFalse {
		t.Fatalf("got %v, want TriFalse", cp.Value)
	}
}

// TestValuePredicateConstantFold_BooleanValue pins that the
// BooleanValue input also folds (parallel to ConstantValue input).
func TestValuePredicateConstantFold_BooleanValue(t *testing.T) {
	t.Parallel()
	rule := NewValuePredicateConstantFoldRule()
	pred := NewValuePredicate(NewBooleanValue(true))
	got := FireRule(rule, pred)
	cp := got[0].(*ConstantPredicate)
	if cp.Value != TriTrue {
		t.Fatalf("got %v, want TriTrue", cp.Value)
	}
}

// TestValuePredicateConstantFold_Null pins that NullValue / nil-
// constant collapse to UNKNOWN (Kleene 3VL).
func TestValuePredicateConstantFold_Null(t *testing.T) {
	t.Parallel()
	rule := NewValuePredicateConstantFoldRule()

	// NullValue.
	pred := NewValuePredicate(NewNullValue(TypeBool))
	got := FireRule(rule, pred)
	cp := got[0].(*ConstantPredicate)
	if cp.Value != TriUnknown {
		t.Fatalf("NullValue: got %v, want TriUnknown", cp.Value)
	}

	// ConstantValue(nil).
	pred = NewValuePredicate(&ConstantValue{Value: nil, Typ: TypeBool})
	got = FireRule(rule, pred)
	cp = got[0].(*ConstantPredicate)
	if cp.Value != TriUnknown {
		t.Fatalf("ConstantValue(nil): got %v, want TriUnknown", cp.Value)
	}

	// BooleanValue with nil (UNKNOWN-typed bool).
	pred = NewValuePredicate(&BooleanValue{Value: nil})
	got = FireRule(rule, pred)
	cp = got[0].(*ConstantPredicate)
	if cp.Value != TriUnknown {
		t.Fatalf("BooleanValue{nil}: got %v, want TriUnknown", cp.Value)
	}
}

// TestValuePredicateConstantFold_NonBoolConstantDegrades pins the
// type-degraded arm: a constant-but-non-bool Value (int / string /
// float) folds to TriUnknown matching ValuePredicate.Eval's runtime
// degradation. Java's ConstantFoldingValuePredicateRule does the same.
func TestValuePredicateConstantFold_NonBoolConstantDegrades(t *testing.T) {
	t.Parallel()
	rule := NewValuePredicateConstantFoldRule()
	cases := []Value{
		&ConstantValue{Value: int64(1), Typ: TypeInt},
		&ConstantValue{Value: "hello", Typ: TypeString},
		&ConstantValue{Value: 1.5, Typ: TypeFloat},
	}
	for _, child := range cases {
		pred := NewValuePredicate(child)
		got := FireRule(rule, pred)
		cp := got[0].(*ConstantPredicate)
		if cp.Value != TriUnknown {
			t.Fatalf("ValuePredicate(%v): got %v, want TriUnknown", child.Name(), cp.Value)
		}
	}
}

// TestValuePredicateConstantFold_NonConstantValue_Declines pins that
// a non-constant Value (FieldValue, ArithmeticValue with field
// children) leaves the ValuePredicate alone — the rule yields zero
// replacements.
func TestValuePredicateConstantFold_NonConstantValue_Declines(t *testing.T) {
	t.Parallel()
	rule := NewValuePredicateConstantFoldRule()

	pred := NewValuePredicate(&FieldValue{Field: "active", Typ: TypeBool})
	if got := FireRule(rule, pred); len(got) != 0 {
		t.Fatalf("FieldValue: expected no yield, got %d", len(got))
	}

	// Arithmetic with field child — composite but not constant.
	arith := &ArithmeticValue{
		Op:    OpAdd,
		Left:  &FieldValue{Field: "x", Typ: TypeInt},
		Right: &ConstantValue{Value: int64(1), Typ: TypeInt},
	}
	pred = NewValuePredicate(arith)
	if got := FireRule(rule, pred); len(got) != 0 {
		t.Fatalf("Arithmetic(field, 1): expected no yield, got %d", len(got))
	}
}

// TestValuePredicateConstantFold_NilValue_Declines pins that
// ValuePredicate{Value: nil} doesn't fold. Folding a nil Value to
// UNKNOWN would silently swallow the structural error — the
// ValuePredicate.Explain '<nil-value>' path handles it instead.
func TestValuePredicateConstantFold_NilValue_Declines(t *testing.T) {
	t.Parallel()
	rule := NewValuePredicateConstantFoldRule()
	pred := &ValuePredicate{Value: nil}
	if got := FireRule(rule, pred); len(got) != 0 {
		t.Fatalf("nil Value: expected no yield, got %d", len(got))
	}
}

// TestValuePredicateConstantFold_NonValuePredicate_Declines pins
// that the rule's matcher rejects non-ValuePredicate inputs.
func TestValuePredicateConstantFold_NonValuePredicate_Declines(t *testing.T) {
	t.Parallel()
	rule := NewValuePredicateConstantFoldRule()
	if got := FireRule(rule, NewConstantPredicate(TriTrue)); len(got) != 0 {
		t.Fatalf("ConstantPredicate: expected no yield (not a ValuePredicate)")
	}
	cp := NewComparisonPredicate(
		&FieldValue{Field: "x", Typ: TypeInt},
		Comparison{Type: ComparisonEquals, Operand: LiteralValue(int64(1))},
	)
	if got := FireRule(rule, cp); len(got) != 0 {
		t.Fatalf("ComparisonPredicate: expected no yield (not a ValuePredicate)")
	}
}

// TestSimplify_ValuePredicateInAndCollapses pins the end-to-end
// effect: AND(VP(true), x > 5) — under DefaultSimplifyRules the
// ValuePredicateConstantFoldRule unwraps VP(true) to TriTrue, then
// AndConstantSimplifyRule's identity-drop removes the TRUE child,
// leaving the surviving comparison.
func TestSimplify_ValuePredicateInAndCollapses(t *testing.T) {
	t.Parallel()
	cmp := NewComparisonPredicate(
		&FieldValue{Field: "x", Typ: TypeInt},
		Comparison{Type: ComparisonGreaterThan, Operand: LiteralValue(int64(5))},
	)
	pred := NewAnd(NewValuePredicate(NewBooleanValue(true)), cmp)
	got := Simplify(pred, DefaultSimplifyRules())
	if got != QueryPredicate(cmp) {
		t.Fatalf("expected the comparison to survive, got %T %s", got, got.Explain())
	}
}
