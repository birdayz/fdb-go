package values

import (
	"testing"
)

func TestSimplifyValue_NilSafe(t *testing.T) {
	t.Parallel()
	if got := SimplifyValue(nil); got != nil {
		t.Fatalf("nil: got %v, want nil", got)
	}
}

func TestSimplifyValue_LeafConstantsUnchanged(t *testing.T) {
	t.Parallel()
	c := &ConstantValue{Value: int64(5), Typ: TypeInt}
	if got := SimplifyValue(c); got != Value(c) {
		t.Fatalf("ConstantValue: should be unchanged (pointer-equal)")
	}
	null := NewNullValue(TypeInt)
	if got := SimplifyValue(null); got != Value(null) {
		t.Fatal("NullValue: should be unchanged")
	}
	bv := NewBooleanValue(true)
	if got := SimplifyValue(bv); got != Value(bv) {
		t.Fatal("BooleanValue: should be unchanged")
	}
	fv := &FieldValue{Field: "x", Typ: TypeInt}
	if got := SimplifyValue(fv); got != Value(fv) {
		t.Fatal("FieldValue: non-constant, should be unchanged")
	}
}

func TestSimplifyValue_ArithmeticFold(t *testing.T) {
	t.Parallel()
	// 1 + 2 → 3
	a := &ArithmeticValue{
		Op:    OpAdd,
		Left:  &ConstantValue{Value: int64(1), Typ: TypeInt},
		Right: &ConstantValue{Value: int64(2), Typ: TypeInt},
	}
	got := SimplifyValue(a)
	cv, ok := got.(*ConstantValue)
	if !ok {
		t.Fatalf("expected *ConstantValue, got %T", got)
	}
	if cv.Value != int64(3) {
		t.Fatalf("Value: got %v, want 3", cv.Value)
	}
	if cv.Typ != TypeInt {
		t.Fatalf("Typ: got %v, want TypeInt (preserved from source)", cv.Typ)
	}
}

func TestSimplifyValue_NestedArithmeticFold(t *testing.T) {
	t.Parallel()
	// (1 + 2) * 3 → 9 (full collapse)
	v := &ArithmeticValue{
		Op: OpMul,
		Left: &ArithmeticValue{
			Op:    OpAdd,
			Left:  &ConstantValue{Value: int64(1), Typ: TypeInt},
			Right: &ConstantValue{Value: int64(2), Typ: TypeInt},
		},
		Right: &ConstantValue{Value: int64(3), Typ: TypeInt},
	}
	got := SimplifyValue(v)
	cv := got.(*ConstantValue)
	if cv.Value != int64(9) {
		t.Fatalf("(1+2)*3: got %v, want 9", cv.Value)
	}
}

func TestSimplifyValue_PartialFold(t *testing.T) {
	t.Parallel()
	// name + (1 + 2) → name + 3 (partial fold; outer is non-constant
	// because LHS is a FieldValue, but inner constant arithmetic still
	// collapses).
	v := &ArithmeticValue{
		Op:   OpAdd,
		Left: &FieldValue{Field: "name", Typ: TypeInt},
		Right: &ArithmeticValue{
			Op:    OpAdd,
			Left:  &ConstantValue{Value: int64(1), Typ: TypeInt},
			Right: &ConstantValue{Value: int64(2), Typ: TypeInt},
		},
	}
	got := SimplifyValue(v).(*ArithmeticValue)
	if got.Op != OpAdd {
		t.Fatalf("outer Op: got %v, want OpAdd", got.Op)
	}
	if _, ok := got.Left.(*FieldValue); !ok {
		t.Fatalf("Left: got %T, want *FieldValue (untouched)", got.Left)
	}
	rhs, ok := got.Right.(*ConstantValue)
	if !ok {
		t.Fatalf("Right: got %T, want *ConstantValue (folded)", got.Right)
	}
	if rhs.Value != int64(3) {
		t.Fatalf("Right.Value: got %v, want 3", rhs.Value)
	}
}

func TestSimplifyValue_NoFoldOnNonConstantLeaves(t *testing.T) {
	t.Parallel()
	// name + 5 → name + 5 (no fold; pointer-equal).
	v := &ArithmeticValue{
		Op:    OpAdd,
		Left:  &FieldValue{Field: "name", Typ: TypeInt},
		Right: &ConstantValue{Value: int64(5), Typ: TypeInt},
	}
	got := SimplifyValue(v)
	if got != Value(v) {
		t.Fatalf("nothing to fold: should be pointer-equal")
	}
}

func TestSimplifyValue_CastFold(t *testing.T) {
	t.Parallel()
	// CAST(1+2 AS STRING) → "3"
	v := NewCastValue(
		&ArithmeticValue{
			Op:    OpAdd,
			Left:  &ConstantValue{Value: int64(1), Typ: TypeInt},
			Right: &ConstantValue{Value: int64(2), Typ: TypeInt},
		},
		TypeString,
	)
	got := SimplifyValue(v)
	cv, ok := got.(*ConstantValue)
	if !ok {
		t.Fatalf("expected *ConstantValue, got %T", got)
	}
	if cv.Value != "3" {
		t.Fatalf("Value: got %v, want '3'", cv.Value)
	}
	if cv.Typ != TypeString {
		t.Fatalf("Typ: got %v, want TypeString", cv.Typ)
	}
}

func TestSimplifyValue_ScalarFunctionFold(t *testing.T) {
	t.Parallel()
	// UPPER(LOWER('Hi')) → "HI" (full nested fold).
	v := NewScalarFunctionValue("UPPER", TypeString,
		NewScalarFunctionValue("LOWER", TypeString,
			&ConstantValue{Value: "Hi", Typ: TypeString}))
	got := SimplifyValue(v)
	cv, ok := got.(*ConstantValue)
	if !ok {
		t.Fatalf("expected *ConstantValue, got %T", got)
	}
	if cv.Value != "HI" {
		t.Fatalf("Value: got %v, want 'HI'", cv.Value)
	}
}

func TestSimplifyValue_ScalarFunctionPartialFold(t *testing.T) {
	t.Parallel()
	// UPPER(name) — leaf scalar fn over a field, can't fold; pointer-equal.
	v := NewScalarFunctionValue("UPPER", TypeString,
		&FieldValue{Field: "name", Typ: TypeString})
	if got := SimplifyValue(v); got != Value(v) {
		t.Fatalf("UPPER(field): should be unchanged")
	}
	// LENGTH(LOWER('Hello')) over a constant — folds fully.
	w := NewScalarFunctionValue("LENGTH", TypeInt,
		NewScalarFunctionValue("LOWER", TypeString,
			&ConstantValue{Value: "Hello", Typ: TypeString}))
	if got, ok := SimplifyValue(w).(*ConstantValue); !ok || got.Value != int64(5) {
		t.Fatalf("LENGTH(LOWER('Hello')): got %v %T, want ConstantValue 5", got, got)
	}
}

func TestSimplifyValue_NULLPropagatesThroughArith(t *testing.T) {
	t.Parallel()
	// NULL + 5 → NULL (NullValue, not nil) via fold path. Pin the
	// Type as well — the source ArithmeticValue is TypeInt, and the
	// folded NullValue must carry that forward so future type-aware
	// rules (`NULL :: TypeInt` vs `NULL :: TypeUnknown`) see the
	// correct annotation.
	v := &ArithmeticValue{
		Op:    OpAdd,
		Left:  NewNullValue(TypeInt),
		Right: &ConstantValue{Value: int64(5), Typ: TypeInt},
	}
	got := SimplifyValue(v)
	nv, ok := got.(*NullValue)
	if !ok {
		t.Fatalf("NULL + 5 should fold to NullValue, got %T (%v)", got, got)
	}
	if nv.Typ != TypeInt {
		t.Fatalf("NullValue.Typ: got %v, want TypeInt (carried from source)", nv.Typ)
	}
}

func TestSimplifyValue_PromoteFold(t *testing.T) {
	t.Parallel()
	// PROMOTE(1+2, TypeFloat) → ConstantValue(3) with Typ=TypeFloat.
	// Mirrors TestSimplifyValue_CastFold for the PromoteValue arm of
	// isFoldableComposite / simplifyChildren — keeps both cast-like
	// shapes covered before either grows real wire-up.
	v := NewPromoteValue(
		&ArithmeticValue{
			Op:    OpAdd,
			Left:  &ConstantValue{Value: int64(1), Typ: TypeInt},
			Right: &ConstantValue{Value: int64(2), Typ: TypeInt},
		},
		TypeFloat,
	)
	got := SimplifyValue(v)
	cv, ok := got.(*ConstantValue)
	if !ok {
		t.Fatalf("expected *ConstantValue, got %T", got)
	}
	// PromoteValue.Evaluate returns the child's value verbatim at
	// the seed (no type coercion yet — that lands with the Type
	// hierarchy port). The folded ConstantValue inherits its Typ
	// from the PromoteValue.Target.
	if cv.Value != int64(3) {
		t.Fatalf("Value: got %v, want 3", cv.Value)
	}
	if cv.Typ != TypeFloat {
		t.Fatalf("Typ: got %v, want TypeFloat (preserved from PROMOTE target)", cv.Typ)
	}
}

func TestSimplifyValue_PromotePartialFold(t *testing.T) {
	t.Parallel()
	// PROMOTE(name, TypeFloat) — non-constant child; pointer-equal
	// short-circuit through simplifyChildren.
	v := NewPromoteValue(&FieldValue{Field: "name", Typ: TypeInt}, TypeFloat)
	if got := SimplifyValue(v); got != Value(v) {
		t.Fatal("PROMOTE(field) should be unchanged")
	}
}

func TestSimplifyValue_NoFoldOnUnknownComposite(t *testing.T) {
	t.Parallel()
	// Composites SimplifyValue doesn't know about pass through
	// untouched (RecordConstructorValue here). Pinned so a future
	// change that adds composite handling has a clear regression
	// signal.
	v := NewRecordConstructorValue(
		RecordConstructorField{Name: "a", Value: &ConstantValue{Value: int64(1), Typ: TypeInt}},
	)
	if got := SimplifyValue(v); got != Value(v) {
		t.Fatal("RecordConstructorValue: should be unchanged (not in seed set)")
	}
}
