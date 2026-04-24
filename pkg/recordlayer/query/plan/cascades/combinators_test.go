package cascades

import "testing"

// AllOfMatcher: every downstream must match.
func TestAllOf_AllDownstreamsMustMatch(t *testing.T) {
	t.Parallel()

	// Setup: pattern matches "a ConstantValue AND anything".
	constMatcher := NewConstantMatcher()
	anyMatcher := NewAnyValue()
	pattern := NewAllOf("ConstantValue", constMatcher, anyMatcher)

	cv := &ConstantValue{Value: int64(7), Typ: TypeInt}
	got := pattern.BindMatches(NewBindings(), cv)
	if len(got) != 1 {
		t.Fatalf("expected 1 match, got %d", len(got))
	}
	b := got[0]
	// Both downstream matchers + the AllOf itself should be bound.
	if Get[*ConstantValue](b, constMatcher) != cv {
		t.Fatal("constMatcher binding wrong")
	}
	if Get[Value](b, anyMatcher) != Value(cv) {
		t.Fatal("anyMatcher binding wrong")
	}
	if Get[*ConstantValue](b, pattern) != cv {
		t.Fatal("allOf self-binding wrong")
	}
}

// AllOf collapses to empty when any single downstream fails.
func TestAllOf_AnyFailureCollapses(t *testing.T) {
	t.Parallel()

	// Expects ConstantValue AND FieldValue — input is ConstantValue,
	// fails the field matcher, AllOf returns empty.
	pattern := NewAllOf("Value", NewConstantMatcher(), NewFieldMatcher())

	cv := &ConstantValue{Value: int64(1), Typ: TypeInt}
	if got := pattern.BindMatches(NewBindings(), cv); len(got) != 0 {
		t.Fatalf("expected 0 matches on AND failure, got %d", len(got))
	}
}

// AnyOfMatcher: at least one downstream matches.
func TestAnyOf_UnionOfMatches(t *testing.T) {
	t.Parallel()

	constMatcher := NewConstantMatcher()
	fieldMatcher := NewFieldMatcher()
	pattern := NewAnyOf("Value", constMatcher, fieldMatcher)

	// ConstantValue input: only constMatcher matches → 1 result.
	cv := &ConstantValue{Value: int64(3), Typ: TypeInt}
	got := pattern.BindMatches(NewBindings(), cv)
	if len(got) != 1 {
		t.Fatalf("ConstantValue input: expected 1 match, got %d", len(got))
	}
	// The AnyOf combinator itself is bound; the specific down-
	// stream that matched is also bound.
	if Get[*ConstantValue](got[0], constMatcher) != cv {
		t.Fatal("ConstantValue did not bind constMatcher")
	}

	// FieldValue input: only fieldMatcher matches → 1 result.
	fv := &FieldValue{Field: "name", Typ: TypeString}
	got = pattern.BindMatches(NewBindings(), fv)
	if len(got) != 1 {
		t.Fatalf("FieldValue input: expected 1 match, got %d", len(got))
	}
}

// AnyOf collapses to empty when no downstream matches.
func TestAnyOf_NoMatchCollapses(t *testing.T) {
	t.Parallel()
	pattern := NewAnyOf("Value", NewConstantMatcher(), NewFieldMatcher())
	av := &ArithmeticValue{Op: OpAdd}
	if got := pattern.BindMatches(NewBindings(), av); len(got) != 0 {
		t.Fatalf("expected 0 matches when no downstream fires, got %d", len(got))
	}
}

// AllOf + AnyOf composition — real rule patterns nest these.
func TestCombinators_Nested(t *testing.T) {
	t.Parallel()

	// Pattern: ArithmeticValue whose children include ANY of
	// (Constant, Field).
	constMatcher := NewConstantMatcher()
	fieldMatcher := NewFieldMatcher()
	inner := NewAnyOf("Value", constMatcher, fieldMatcher)
	pattern := &ArithmeticMatcher{
		Op:    OpAdd,
		Left:  inner,
		Right: inner,
	}

	// 5 + name → both sides match inner; AllOf-style composition of
	// ArithmeticMatcher's left×right bindings yields 1 result.
	expr := &ArithmeticValue{
		Op:    OpAdd,
		Left:  &ConstantValue{Value: int64(5), Typ: TypeInt},
		Right: &FieldValue{Field: "name", Typ: TypeString},
	}
	got := pattern.BindMatches(NewBindings(), expr)
	if len(got) != 1 {
		t.Fatalf("expected 1 match, got %d", len(got))
	}
}

// PlannerBindings.MergedWith: nil-safe fast paths.
func TestMergedWith_NilSafety(t *testing.T) {
	t.Parallel()
	empty := NewBindings()
	m := NewConstantMatcher()
	cv := &ConstantValue{Value: int64(1), Typ: TypeInt}
	nonEmpty := empty.Bind(m, cv)

	// empty.MergedWith(nonEmpty) → nonEmpty
	if got := empty.MergedWith(nonEmpty); got != nonEmpty {
		t.Fatal("empty merge should short-circuit to rhs")
	}
	// nonEmpty.MergedWith(empty) → nonEmpty
	if got := nonEmpty.MergedWith(empty); got != nonEmpty {
		t.Fatal("empty merge should short-circuit to lhs")
	}
	// nonEmpty.MergedWith(nil) → nonEmpty
	if got := nonEmpty.MergedWith(nil); got != nonEmpty {
		t.Fatal("nil merge should short-circuit to lhs")
	}
}

// PlannerBindings.MergedWith: values from both inputs visible.
func TestMergedWith_BothBindingsVisible(t *testing.T) {
	t.Parallel()
	m1 := NewConstantMatcher()
	m2 := NewFieldMatcher()
	cv := &ConstantValue{Value: int64(1), Typ: TypeInt}
	fv := &FieldValue{Field: "x", Typ: TypeString}
	b1 := NewBindings().Bind(m1, cv)
	b2 := NewBindings().Bind(m2, fv)

	merged := b1.MergedWith(b2)
	if Get[*ConstantValue](merged, m1) != cv {
		t.Fatal("merged: lhs binding lost")
	}
	if Get[*FieldValue](merged, m2) != fv {
		t.Fatal("merged: rhs binding lost")
	}
}
