package shapeb

import (
	"testing"
)

// The same 10-line predicate matcher from the shape-(a) spike, now
// expressed against the generic interface.
//
// Rule body retrieval: `Get[*ConstantValue](b, lhs)`. Type param
// propagates to the return type; no explicit `.(*ConstantValue)`
// assertion at the call site. If the binding happens to be of a
// different type, the Get panic surfaces a typed error rather than
// a `_, ok := v.(T); !ok` branch at every retrieval.
//
// Matcher composition: the lhs/rhs InstanceMatchers have concrete T
// parameters (*ConstantValue, *FieldValue). ArithmeticMatcher's
// Left/Right are BindingMatcher[Value]. The type-mismatch is
// resolved with UpcastToValue at every composition site — the
// explicit boilerplate that shape (b) cannot avoid.
func TestShapeB_ConstPlusField(t *testing.T) {
	t.Parallel()
	lhs := NewInstanceMatcher[*ConstantValue]()
	rhs := NewInstanceMatcher[*FieldValue]()

	// UpcastToValue is the friction point. Every composition of a
	// narrow matcher under a broader combinator pays this.
	matcher := &ArithmeticMatcher{
		Op:    OpAdd,
		Left:  UpcastToValue[*ConstantValue](lhs),
		Right: UpcastToValue[*FieldValue](rhs),
	}

	expr := &ArithmeticValue{
		Op:    OpAdd,
		Left:  &ConstantValue{Value: int64(5), Typ: TypeInt},
		Right: &FieldValue{Field: "name", Typ: TypeString},
	}

	bindings := matcher.BindMatches(NewBindings(), expr)
	if len(bindings) != 1 {
		t.Fatalf("expected 1 binding, got %d", len(bindings))
	}

	// Retrieval-site win: typed Get. No `.(*ConstantValue)` dance at
	// the call site; the assertion is inside Get[T].
	b := bindings[0]
	cv := Get[*ConstantValue](b, lhs)
	fv := Get[*FieldValue](b, rhs)

	if cv.Value != int64(5) {
		t.Fatalf("expected constant=5, got %v", cv.Value)
	}
	if fv.Field != "name" {
		t.Fatalf("expected field=name, got %q", fv.Field)
	}
}

// Mismatch on input root type: BindMatches returns empty. Matches
// shape (a)'s behaviour.
func TestShapeB_MismatchEmpty(t *testing.T) {
	t.Parallel()
	lhs := NewInstanceMatcher[*ConstantValue]()
	rhs := NewInstanceMatcher[*FieldValue]()
	matcher := &ArithmeticMatcher{
		Op:    OpAdd,
		Left:  UpcastToValue[*ConstantValue](lhs),
		Right: UpcastToValue[*FieldValue](rhs),
	}
	expr := &ConstantValue{Value: int64(5), Typ: TypeInt}
	if got := matcher.BindMatches(NewBindings(), expr); len(got) != 0 {
		t.Fatalf("expected 0 matches on type mismatch, got %d", len(got))
	}
}

// Sub-shape mismatch: arith on the left, expected a constant.
func TestShapeB_SubShapeMismatch(t *testing.T) {
	t.Parallel()
	lhs := NewInstanceMatcher[*ConstantValue]()
	rhs := NewInstanceMatcher[*FieldValue]()
	matcher := &ArithmeticMatcher{
		Op:    OpAdd,
		Left:  UpcastToValue[*ConstantValue](lhs),
		Right: UpcastToValue[*FieldValue](rhs),
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

// AnyMatcher downstream: generic-Value bound.
func TestShapeB_AnyDownstream(t *testing.T) {
	t.Parallel()
	any1 := NewAnyMatcher()
	any2 := NewAnyMatcher()
	matcher := &ArithmeticMatcher{
		Op:    OpSub,
		Left:  any1, // already BindingMatcher[Value] — no upcast needed
		Right: any2,
	}

	expr := &ArithmeticValue{
		Op:    OpSub,
		Left:  &ConstantValue{Value: int64(1), Typ: TypeInt},
		Right: &FieldValue{Field: "x", Typ: TypeInt},
	}
	got := matcher.BindMatches(NewBindings(), expr)
	if len(got) != 1 {
		t.Fatalf("expected 1 match, got %d", len(got))
	}
	// Retrieval returns Value. No narrower type known at compile time.
	v := Get[Value](got[0], any1)
	if _, ok := v.(*ConstantValue); !ok {
		t.Fatalf("expected left = *ConstantValue, got %T", v)
	}
}

// Demonstration of the composition friction: without UpcastToValue,
// the assignment doesn't typecheck. This test is disabled (commented)
// since it's a compile-time failure, not a runtime test — the
// comment itself is the spike evidence.
//
// func TestShapeB_WithoutUpcastDoesNotCompile(t *testing.T) {
//     lhs := &InstanceMatcher[*ConstantValue]{}
//     _ = &ArithmeticMatcher{
//         Op:   OpAdd,
//         Left: lhs, // error: cannot use lhs (*InstanceMatcher[*ConstantValue])
//                   // as BindingMatcher[Value]
//     }
// }
