package shapea

import (
	"testing"
)

// The 10-line predicate matcher the RFC-022 spike asks for:
//
// Match `ArithmeticValue(Add, ConstantValue, FieldValue)` and pull
// out the constant + field name from the bindings for the rule body.
//
// In shape (a) the rule body pays the `any → *ConstantValue` downcast
// tax every time it touches a bound value. The matcher definition is
// short; the retrieval is where the cost lands.
func TestShapeA_ConstPlusField(t *testing.T) {
	t.Parallel()
	// Build the pattern.
	lhs := NewConstantMatcher()
	rhs := NewFieldMatcher()
	matcher := &ArithmeticMatcher{Op: OpAdd, Left: lhs, Right: rhs}

	// Input tree: 5 + name.
	expr := &ArithmeticValue{
		Op:    OpAdd,
		Left:  &ConstantValue{Value: int64(5), Typ: TypeInt},
		Right: &FieldValue{Field: "name", Typ: TypeString},
	}

	bindings := matcher.BindMatches(NewBindings(), expr)
	if len(bindings) != 1 {
		t.Fatalf("expected 1 binding set, got %d", len(bindings))
	}

	// Option 1: untyped Get + manual `.(T)` downcast (the shape-(a)
	// baseline the RFC-023 writeup compares against).
	b := bindings[0]
	cv, ok := b.Get(lhs).(*ConstantValue)
	if !ok {
		t.Fatalf("lhs binding not *ConstantValue: %T", b.Get(lhs))
	}
	fv, ok := b.Get(rhs).(*FieldValue)
	if !ok {
		t.Fatalf("rhs binding not *FieldValue: %T", b.Get(rhs))
	}
	if cv.Value != int64(5) {
		t.Fatalf("expected constant=5, got %v", cv.Value)
	}
	if fv.Field != "name" {
		t.Fatalf("expected field=name, got %q", fv.Field)
	}

	// Option 2: generic Get[T] helper (RFC-023 §Changes item 5). Same
	// compile-time safety envelope, less ceremony at every call site.
	cv2 := Get[*ConstantValue](b, lhs)
	fv2 := Get[*FieldValue](b, rhs)
	if cv2 != cv || fv2 != fv {
		t.Fatalf("Get[T] returned different values than untyped Get")
	}
}

// Get[T] panics cleanly on a type mismatch — rule authors see the
// problem immediately instead of silently retrieving nil.
func TestShapeA_GetTypeMismatchPanics(t *testing.T) {
	t.Parallel()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic on type mismatch, got none")
		}
	}()

	lhs := NewConstantMatcher()
	b := NewBindings().Bind(lhs, &ConstantValue{Value: int64(1), Typ: TypeInt})
	// Ask for the wrong type — should panic.
	_ = Get[*FieldValue](b, lhs)
}

// Mismatch on input type: matcher returns empty, no panic.
func TestShapeA_MismatchEmpty(t *testing.T) {
	t.Parallel()
	matcher := &ArithmeticMatcher{
		Op:    OpAdd,
		Left:  NewConstantMatcher(),
		Right: NewFieldMatcher(),
	}
	// Wrong shape: constant, not arithmetic.
	expr := &ConstantValue{Value: int64(5), Typ: TypeInt}
	if got := matcher.BindMatches(NewBindings(), expr); len(got) != 0 {
		t.Fatalf("expected 0 matches on type mismatch, got %d", len(got))
	}
}

// Sub-shape mismatch: arith on the left, but the rule wants a
// constant on the left.
func TestShapeA_SubShapeMismatch(t *testing.T) {
	t.Parallel()
	matcher := &ArithmeticMatcher{
		Op:    OpAdd,
		Left:  NewConstantMatcher(),
		Right: NewFieldMatcher(),
	}
	expr := &ArithmeticValue{
		Op:    OpAdd,
		Left:  &FieldValue{Field: "x", Typ: TypeInt}, // not a Constant
		Right: &FieldValue{Field: "y", Typ: TypeString},
	}
	if got := matcher.BindMatches(NewBindings(), expr); len(got) != 0 {
		t.Fatalf("expected 0 matches on sub-shape mismatch, got %d", len(got))
	}
}

// Spike finding: zero-size matcher structs collide as map keys.
// Pins the reason NewAnyValue exists.
func TestShapeA_ZeroSizeStructIdentityCollision(t *testing.T) {
	t.Parallel()
	// Hypothetical "identity collapses" — two separate `&AnyValue{}`
	// values point to the same address because the struct is
	// zero-size. Go's runtime is allowed to collapse them per the
	// spec ("two distinct zero-size variables may have the same
	// address in memory"). Under that collapse, the PlannerBindings
	// map can't distinguish the two matchers, and rule bodies
	// retrieve the WRONG bound value.
	//
	// The fix baked into the spike: AnyValue carries a nonce field
	// (the counter in NewAnyValue) so the pointer-to-struct
	// compares unique even though the logical matcher is identical.
	//
	// This test ensures two NewAnyValue() calls produce distinct
	// identities so the test in TestShapeA_AnyDownstream is not a
	// false positive.
	a := NewAnyValue()
	b := NewAnyValue()
	if a == b {
		t.Fatalf("NewAnyValue() returned the same pointer twice; nonce broken")
	}
}

// AnyValue downstream: compose AnyValue under ArithmeticMatcher. This
// is the equivalent of Java's `BindingMatcher<? super Value>` — in
// shape (a) there's no wildcard; AnyValue returns the same type the
// rule body gets regardless.
func TestShapeA_AnyDownstream(t *testing.T) {
	t.Parallel()
	any1 := NewAnyValue()
	any2 := NewAnyValue()
	matcher := &ArithmeticMatcher{Op: OpSub, Left: any1, Right: any2}

	expr := &ArithmeticValue{
		Op:    OpSub,
		Left:  &ConstantValue{Value: int64(1), Typ: TypeInt},
		Right: &FieldValue{Field: "x", Typ: TypeInt},
	}
	got := matcher.BindMatches(NewBindings(), expr)
	if len(got) != 1 {
		t.Fatalf("expected 1, got %d", len(got))
	}
	// Bound value is typed `any` — the rule body does the work.
	if _, ok := got[0].Get(any1).(Value); !ok {
		t.Fatalf("AnyValue did not bind a Value")
	}
}
