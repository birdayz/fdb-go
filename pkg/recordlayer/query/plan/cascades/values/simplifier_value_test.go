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

func TestSimplifyValue_CoalesceAllNulls(t *testing.T) {
	t.Parallel()
	v := NewScalarFunctionValue("COALESCE", NullableLong,
		NewNullValue(TypeInt), NewNullValue(TypeInt))
	got := SimplifyValue(v)
	if _, ok := got.(*NullValue); !ok {
		t.Fatalf("COALESCE(NULL, NULL) = %T, want NullValue", got)
	}
}

func TestSimplifyValue_CoalesceFirstNonNullConstant(t *testing.T) {
	t.Parallel()
	v := NewScalarFunctionValue("COALESCE", NullableLong,
		NewNullValue(TypeInt),
		&ConstantValue{Value: int64(42), Typ: TypeInt},
		&FieldValue{Field: "x", Typ: TypeInt},
	)
	got := SimplifyValue(v)
	c, ok := got.(*ConstantValue)
	if !ok {
		t.Fatalf("COALESCE(NULL, 42, x) = %T, want ConstantValue", got)
	}
	if c.Value != int64(42) {
		t.Fatalf("COALESCE(NULL, 42, x) = %v, want 42", c.Value)
	}
}

func TestSimplifyValue_CoalesceRemoveRedundantNulls(t *testing.T) {
	t.Parallel()
	x := &FieldValue{Field: "x", Typ: TypeInt}
	y := &FieldValue{Field: "y", Typ: TypeInt}
	v := NewScalarFunctionValue("COALESCE", NullableLong,
		x, NewNullValue(TypeInt), y, NewNullValue(TypeInt))
	got := SimplifyValue(v)
	sf, ok := got.(*ScalarFunctionValue)
	if !ok {
		t.Fatalf("COALESCE(x, NULL, y, NULL) = %T, want ScalarFunctionValue", got)
	}
	if len(sf.Args) != 2 {
		t.Fatalf("args len = %d, want 2 (nulls removed)", len(sf.Args))
	}
}

func TestSimplifyValue_CoalesceNoChangeNeeded(t *testing.T) {
	t.Parallel()
	x := &FieldValue{Field: "x", Typ: TypeInt}
	y := &FieldValue{Field: "y", Typ: TypeInt}
	v := NewScalarFunctionValue("COALESCE", NullableLong, x, y)
	got := SimplifyValue(v)
	if got != Value(v) {
		t.Fatal("COALESCE(x, y) with no nulls should be unchanged")
	}
}

// TestCannotFoldCoalesce_BooleanNil verifies that cannotFoldCoalesce
// correctly classifies BooleanValue(nil) as non-foldable.
// BooleanValue{nil} represents SQL UNKNOWN — nullable, not the same
// as a non-null boolean literal. This is the cannotFoldCoalesce fix:
// without it, BooleanValue{nil} would be treated as a foldable
// constant and COALESCE(NULL_bool, x) would incorrectly skip the
// NULL_bool.
func TestCannotFoldCoalesce_BooleanNil(t *testing.T) {
	t.Parallel()

	// BooleanValue(nil) = SQL UNKNOWN: cannotFoldCoalesce must return
	// true (cannot fold — it's nullable/unknown).
	boolNil := &BooleanValue{Value: nil}
	if !cannotFoldCoalesce(boolNil) {
		t.Error("cannotFoldCoalesce(BooleanValue{nil}) = false, want true")
	}

	// BooleanValue with non-nil *bool = concrete TRUE/FALSE: can fold.
	boolTrue := NewBooleanValue(true)
	if cannotFoldCoalesce(boolTrue) {
		t.Error("cannotFoldCoalesce(BooleanValue{true}) = true, want false")
	}

	boolFalse := NewBooleanValue(false)
	if cannotFoldCoalesce(boolFalse) {
		t.Error("cannotFoldCoalesce(BooleanValue{false}) = true, want false")
	}

	// NullValue is always foldable (it IS a NULL literal).
	nv := NewNullValue(TypeInt)
	if cannotFoldCoalesce(nv) {
		t.Error("cannotFoldCoalesce(NullValue) = true, want false")
	}

	// ConstantValue with non-nil payload is foldable.
	cv := &ConstantValue{Value: int64(42), Typ: TypeInt}
	if cannotFoldCoalesce(cv) {
		t.Error("cannotFoldCoalesce(ConstantValue{42}) = true, want false")
	}

	// ConstantValue with nil payload is NOT foldable (typed NULL is
	// represented as ConstantValue{Value: nil}, which is not guaranteed
	// non-null).
	cvNil := &ConstantValue{Value: nil, Typ: TypeInt}
	if !cannotFoldCoalesce(cvNil) {
		t.Error("cannotFoldCoalesce(ConstantValue{nil}) = false, want true")
	}

	// FieldValue is non-constant — cannot fold.
	fv := &FieldValue{Field: "x", Typ: TypeInt}
	if !cannotFoldCoalesce(fv) {
		t.Error("cannotFoldCoalesce(FieldValue) = false, want true")
	}
}

// TestSimplifyValue_CoalesceBooleanNilPreservesInSimplifyCoalesce
// verifies that simplifyCoalesce treats BooleanValue(nil) as a
// non-foldable position (via cannotFoldCoalesce returning true), so
// it doesn't short-circuit fold to the next constant. The
// simplifyCoalesce function itself correctly preserves the COALESCE
// when BooleanValue(nil) appears first. (The general IsConstantValue
// path may still fold the whole expression — that's a separate
// concern from the COALESCE-specific simplification.)
func TestSimplifyValue_CoalesceBooleanNilPreservesInSimplifyCoalesce(t *testing.T) {
	t.Parallel()

	// COALESCE(BooleanValue(nil), ConstantValue(42)):
	// simplifyCoalesce should NOT return ConstantValue(42) as a
	// short-circuit fold (the BooleanValue(nil) blocks early-out).
	boolNil := &BooleanValue{Value: nil}
	constant42 := &ConstantValue{Value: int64(42), Typ: TypeInt}
	v := NewScalarFunctionValue("COALESCE", NullableLong, boolNil, constant42)

	// Call simplifyCoalesce directly (bypasses the general
	// EvaluateConstant path that treats BooleanValue as constant).
	got := simplifyCoalesce(v)

	// simplifyCoalesce should return v unchanged — BooleanValue(nil)
	// is non-foldable so it can't short-circuit to 42.
	if got != Value(v) {
		t.Errorf("simplifyCoalesce should return v unchanged when first arg is BooleanValue(nil), got %T", got)
	}
}

func TestSimplifyValueWithContext_EliminateArithmeticConstant(t *testing.T) {
	t.Parallel()
	alias := NamedCorrelationIdentifier("const_col")
	ctx := ValueSimplifyContext{
		ConstantAliases: map[CorrelationIdentifier]struct{}{alias: {}},
		IsRoot:          true,
	}
	// var_col + 5: ConstantValue(5) has empty correlation set → subset of any constantAliases.
	// The rule drops the constant operand → returns var_col.
	nonConstAlias := NamedCorrelationIdentifier("var_col")
	v1 := &ArithmeticValue{
		Op:    OpAdd,
		Left:  NewQuantifiedObjectValue(nonConstAlias),
		Right: &ConstantValue{Value: int64(5), Typ: TypeInt},
	}
	got1 := SimplifyValueWithContext(v1, ctx)
	if qov, ok := got1.(*QuantifiedObjectValue); !ok || qov.Correlation != nonConstAlias {
		t.Fatalf("var_col + 5: expected QOV(var_col), got %T", got1)
	}

	// const_col + 5 where const_col is constant → should eliminate to... well,
	// both operands are constant (5 is literal, const_col is in constantAliases)
	// so the whole thing is fully constant → FoldConstant wraps it.
	// Actually: left=QOV(const_col) correlates to {const_col} which IS in constantAliases.
	// right=ConstantValue(5) correlates to {} which IS subset of constantAliases.
	// ALL correlations constant → eliminateArithmetic returns nil (whole tree is constant).
	// foldConstant fires: wraps in ConstantValue.
	v2 := &ArithmeticValue{
		Op:    OpAdd,
		Left:  NewQuantifiedObjectValue(alias),
		Right: &ConstantValue{Value: int64(5), Typ: TypeInt},
	}
	got2 := SimplifyValueWithContext(v2, ctx)
	if _, ok := got2.(*ConstantValue); !ok {
		t.Fatalf("fully constant col + 5: expected ConstantValue wrap, got %T", got2)
	}

	// var_col + const_col where const_col is constant → drop const_col, return var_col
	v3 := &ArithmeticValue{
		Op:    OpAdd,
		Left:  NewQuantifiedObjectValue(nonConstAlias),
		Right: NewQuantifiedObjectValue(alias),
	}
	got3 := SimplifyValueWithContext(v3, ctx)
	qov, ok := got3.(*QuantifiedObjectValue)
	if !ok {
		t.Fatalf("var + const: expected QuantifiedObjectValue, got %T", got3)
	}
	if qov.Correlation != nonConstAlias {
		t.Fatalf("expected non-constant alias %v, got %v", nonConstAlias, qov.Correlation)
	}
}

func TestSimplifyValueWithContext_LiftConstructor(t *testing.T) {
	t.Parallel()
	ctx := ValueSimplifyContext{IsRoot: true}
	// Use correlated values to prevent foldConstant from wrapping.
	varAlias := NamedCorrelationIdentifier("var")
	inner := &RecordConstructorValue{Fields: []RecordConstructorField{
		{Name: "b", Value: NewQuantifiedObjectValue(varAlias)},
		{Name: "c", Value: &ConstantValue{Value: int64(3), Typ: TypeInt}},
	}}
	outer := &RecordConstructorValue{Fields: []RecordConstructorField{
		{Name: "a", Value: &ConstantValue{Value: int64(1), Typ: TypeInt}},
		{Name: "inner", Value: inner},
		{Name: "d", Value: &ConstantValue{Value: int64(4), Typ: TypeInt}},
	}}
	got := SimplifyValueWithContext(outer, ctx)
	rc, ok := got.(*RecordConstructorValue)
	if !ok {
		t.Fatalf("expected RecordConstructorValue, got %T", got)
	}
	if len(rc.Fields) != 4 {
		t.Fatalf("expected 4 fields (a, b, c, d), got %d", len(rc.Fields))
	}
}

func TestSimplifyValueWithContext_LiftConstructorNotAtRoot(t *testing.T) {
	t.Parallel()
	ctx := ValueSimplifyContext{IsRoot: false}
	varAlias := NamedCorrelationIdentifier("var")
	inner := &RecordConstructorValue{Fields: []RecordConstructorField{
		{Name: "b", Value: NewQuantifiedObjectValue(varAlias)},
	}}
	outer := &RecordConstructorValue{Fields: []RecordConstructorField{
		{Name: "a", Value: NewQuantifiedObjectValue(varAlias)},
		{Name: "inner", Value: inner},
	}}
	got := SimplifyValueWithContext(outer, ctx)
	rc, ok := got.(*RecordConstructorValue)
	if !ok {
		t.Fatalf("expected RecordConstructorValue, got %T", got)
	}
	if len(rc.Fields) != 2 {
		t.Fatalf("should NOT lift when isRoot=false, got %d fields", len(rc.Fields))
	}
}

func TestSimplifyValue_FieldOverRecordConstructor(t *testing.T) {
	t.Parallel()
	rc := NewRecordConstructorValue(
		RecordConstructorField{Name: "a", Value: &ConstantValue{Value: int64(1), Typ: TypeInt}},
		RecordConstructorField{Name: "b", Value: &ConstantValue{Value: "hello", Typ: TypeString}},
	)
	fv := &FieldValue{Field: "b", Typ: TypeString, Child: rc}
	got := SimplifyValue(fv)
	c, ok := got.(*ConstantValue)
	if !ok {
		t.Fatalf("field('b', RC{a:1, b:'hello'}) = %T, want *ConstantValue", got)
	}
	if c.Value != "hello" {
		t.Fatalf("field('b') = %v, want 'hello'", c.Value)
	}
}

func TestSimplifyValue_FieldOverRecordConstructor_NotFound(t *testing.T) {
	t.Parallel()
	rc := NewRecordConstructorValue(
		RecordConstructorField{Name: "a", Value: &ConstantValue{Value: int64(1), Typ: TypeInt}},
	)
	fv := &FieldValue{Field: "z", Typ: TypeString, Child: rc}
	got := SimplifyValue(fv)
	if _, ok := got.(*FieldValue); !ok {
		t.Fatalf("field('z') on RC without 'z' should remain FieldValue, got %T", got)
	}
}
