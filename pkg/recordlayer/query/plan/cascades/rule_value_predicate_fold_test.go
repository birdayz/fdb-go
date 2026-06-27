package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TestValuePredicateConstantFold_True pins ConstantValue(true) →
// ConstantPredicate(TriTrue).
func TestValuePredicateConstantFold_True(t *testing.T) {
	t.Parallel()
	rule := NewValuePredicateConstantFoldRule()
	pred := predicates.NewValuePredicate(&values.ConstantValue{Value: true, Typ: values.TypeBool})
	got := FireRule(rule, pred)
	if len(got) != 1 {
		t.Fatalf("expected 1 yield, got %d", len(got))
	}
	cp := got[0].(*predicates.ConstantPredicate)
	if cp.Value != predicates.TriTrue {
		t.Fatalf("got %v, want TriTrue", cp.Value)
	}
}

// TestValuePredicateConstantFold_False pins the FALSE case.
func TestValuePredicateConstantFold_False(t *testing.T) {
	t.Parallel()
	rule := NewValuePredicateConstantFoldRule()
	pred := predicates.NewValuePredicate(&values.ConstantValue{Value: false, Typ: values.TypeBool})
	got := FireRule(rule, pred)
	cp := got[0].(*predicates.ConstantPredicate)
	if cp.Value != predicates.TriFalse {
		t.Fatalf("got %v, want TriFalse", cp.Value)
	}
}

// TestValuePredicateConstantFold_BooleanValue pins that the
// BooleanValue input also folds (parallel to ConstantValue input).
func TestValuePredicateConstantFold_BooleanValue(t *testing.T) {
	t.Parallel()
	rule := NewValuePredicateConstantFoldRule()
	pred := predicates.NewValuePredicate(values.NewBooleanValue(true))
	got := FireRule(rule, pred)
	cp := got[0].(*predicates.ConstantPredicate)
	if cp.Value != predicates.TriTrue {
		t.Fatalf("got %v, want TriTrue", cp.Value)
	}
}

// TestValuePredicateConstantFold_Null pins that NullValue / nil-
// constant collapse to UNKNOWN (Kleene 3VL).
func TestValuePredicateConstantFold_Null(t *testing.T) {
	t.Parallel()
	rule := NewValuePredicateConstantFoldRule()

	// NullValue.
	pred := predicates.NewValuePredicate(values.NewNullValue(values.TypeBool))
	got := FireRule(rule, pred)
	cp := got[0].(*predicates.ConstantPredicate)
	if cp.Value != predicates.TriUnknown {
		t.Fatalf("NullValue: got %v, want TriUnknown", cp.Value)
	}

	// ConstantValue(nil).
	pred = predicates.NewValuePredicate(&values.ConstantValue{Value: nil, Typ: values.TypeBool})
	got = FireRule(rule, pred)
	cp = got[0].(*predicates.ConstantPredicate)
	if cp.Value != predicates.TriUnknown {
		t.Fatalf("ConstantValue(nil): got %v, want TriUnknown", cp.Value)
	}

	// BooleanValue with nil (UNKNOWN-typed bool).
	pred = predicates.NewValuePredicate(&values.BooleanValue{Value: nil})
	got = FireRule(rule, pred)
	cp = got[0].(*predicates.ConstantPredicate)
	if cp.Value != predicates.TriUnknown {
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
	cases := []values.Value{
		&values.ConstantValue{Value: int64(1), Typ: values.TypeInt},
		&values.ConstantValue{Value: "hello", Typ: values.TypeString},
		&values.ConstantValue{Value: 1.5, Typ: values.TypeFloat},
	}
	for _, child := range cases {
		pred := predicates.NewValuePredicate(child)
		got := FireRule(rule, pred)
		cp := got[0].(*predicates.ConstantPredicate)
		if cp.Value != predicates.TriUnknown {
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

	pred := predicates.NewValuePredicate(&values.FieldValue{Field: "active", Typ: values.TypeBool})
	if got := FireRule(rule, pred); len(got) != 0 {
		t.Fatalf("FieldValue: expected no yield, got %d", len(got))
	}

	// Arithmetic with field child — composite but not constant.
	arith := &values.ArithmeticValue{
		Op:    values.OpAdd,
		Left:  &values.FieldValue{Field: "x", Typ: values.TypeInt},
		Right: &values.ConstantValue{Value: int64(1), Typ: values.TypeInt},
	}
	pred = predicates.NewValuePredicate(arith)
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
	pred := &predicates.ValuePredicate{Value: nil}
	if got := FireRule(rule, pred); len(got) != 0 {
		t.Fatalf("nil Value: expected no yield, got %d", len(got))
	}
}

// TestValuePredicateConstantFold_NonValuePredicate_Declines pins
// that the rule's matcher rejects non-ValuePredicate inputs.
func TestValuePredicateConstantFold_NonValuePredicate_Declines(t *testing.T) {
	t.Parallel()
	rule := NewValuePredicateConstantFoldRule()
	if got := FireRule(rule, predicates.NewConstantPredicate(predicates.TriTrue)); len(got) != 0 {
		t.Fatalf("ConstantPredicate: expected no yield (not a ValuePredicate)")
	}
	cp := predicates.NewComparisonPredicate(
		&values.FieldValue{Field: "x", Typ: values.TypeInt},
		predicates.Comparison{Type: predicates.ComparisonEquals, Operand: values.LiteralValue(int64(1))},
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
	cmp := predicates.NewComparisonPredicate(
		&values.FieldValue{Field: "x", Typ: values.TypeInt},
		predicates.Comparison{Type: predicates.ComparisonGreaterThan, Operand: values.LiteralValue(int64(5))},
	)
	pred := predicates.NewAnd(predicates.NewValuePredicate(values.NewBooleanValue(true)), cmp)
	got := Simplify(pred, DefaultSimplifyRules())
	if got != predicates.QueryPredicate(cmp) {
		t.Fatalf("expected the comparison to survive, got %T %s", got, got.Explain())
	}
}

// TestSimplify_ValuePredicateInOrCollapses pins the dual integration
// effect: OR(VP(false), x > 5) → x > 5. VP(false) folds to FALSE,
// then OrConstantSimplifyRule's identity-drop removes the FALSE
// child.
func TestSimplify_ValuePredicateInOrCollapses(t *testing.T) {
	t.Parallel()
	cmp := predicates.NewComparisonPredicate(
		&values.FieldValue{Field: "x", Typ: values.TypeInt},
		predicates.Comparison{Type: predicates.ComparisonGreaterThan, Operand: values.LiteralValue(int64(5))},
	)
	pred := predicates.NewOr(predicates.NewValuePredicate(values.NewBooleanValue(false)), cmp)
	got := Simplify(pred, DefaultSimplifyRules())
	if got != predicates.QueryPredicate(cmp) {
		t.Fatalf("expected the comparison to survive, got %T %s", got, got.Explain())
	}
}

// TestSimplify_ValuePredicate_NotValue_FullyFolds pins the end-to-end
// chain across BOTH the Value-layer NotValue fold (commit 54) AND
// the predicate-layer ValuePredicateConstantFoldRule (this rule):
// VP(NotValue(BooleanValue(true))) — the Value tree NOT(TRUE) folds
// to BooleanValue(false), then ValuePredicateConstantFoldRule
// unwraps VP(false) to ConstantPredicate(TriFalse).
func TestSimplify_ValuePredicate_NotValue_FullyFolds(t *testing.T) {
	t.Parallel()
	pred := predicates.NewValuePredicate(values.NewNotValue(values.NewBooleanValue(true)))
	// SimplifyPredicateValues handles the inner Value fold first,
	// then the rule pipeline unwraps. Use Simplify with default rules.
	got := Simplify(predicates.SimplifyPredicateValues(pred), DefaultSimplifyRules())
	cp, ok := got.(*predicates.ConstantPredicate)
	if !ok {
		t.Fatalf("expected ConstantPredicate, got %T %s", got, got.Explain())
	}
	if cp.Value != predicates.TriFalse {
		t.Fatalf("expected TriFalse, got %v", cp.Value)
	}
}
