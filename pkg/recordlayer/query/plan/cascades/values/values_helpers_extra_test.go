package values

import (
	"math"
	"testing"
)

// ---------------------------------------------------------------------------
// compareScalar
// ---------------------------------------------------------------------------

func TestCompareScalar_SameTypeInt64(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		a, b int64
		want int
	}{
		{"greater", 5, 3, 1},
		{"less", 3, 5, -1},
		{"equal", 7, 7, 0},
		{"negative less", -10, -3, -1},
		{"zero vs positive", 0, 1, -1},
		{"MinInt64 vs MaxInt64", math.MinInt64, math.MaxInt64, -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := compareScalar(tc.a, tc.b)
			if !ok {
				t.Fatal("expected ok=true for int64 vs int64")
			}
			if got != tc.want {
				t.Fatalf("compareScalar(%d, %d) = %d, want %d", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

func TestCompareScalar_SameTypeFloat64(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		a, b float64
		want int
	}{
		{"greater", 5.5, 3.2, 1},
		{"less", 1.0, 2.0, -1},
		{"equal", 3.14, 3.14, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := compareScalar(tc.a, tc.b)
			if !ok {
				t.Fatal("expected ok=true for float64 vs float64")
			}
			if got != tc.want {
				t.Fatalf("compareScalar(%f, %f) = %d, want %d", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

func TestCompareScalar_SameTypeString(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		a, b string
		want int
	}{
		{"abc < def", "abc", "def", -1},
		{"def > abc", "def", "abc", 1},
		{"equal", "same", "same", 0},
		{"empty vs non-empty", "", "a", -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := compareScalar(tc.a, tc.b)
			if !ok {
				t.Fatal("expected ok=true for string vs string")
			}
			if got != tc.want {
				t.Fatalf("compareScalar(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

func TestCompareScalar_SameTypeBool(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		a, b bool
		want int
	}{
		{"true > false", true, false, 1},
		{"false < true", false, true, -1},
		{"true == true", true, true, 0},
		{"false == false", false, false, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := compareScalar(tc.a, tc.b)
			if !ok {
				t.Fatal("expected ok=true for bool vs bool")
			}
			if got != tc.want {
				t.Fatalf("compareScalar(%v, %v) = %d, want %d", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

func TestCompareScalar_CrossTypeNumeric(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		a, b any
		want int
	}{
		{"int64 > float64", int64(5), float64(3.0), 1},
		{"int64 < float64", int64(2), float64(3.5), -1},
		{"int64 == float64", int64(7), float64(7.0), 0},
		{"float64 > int64", float64(10.5), int64(3), 1},
		{"float64 < int64", float64(1.0), int64(5), -1},
		{"float64 == int64", float64(42.0), int64(42), 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := compareScalar(tc.a, tc.b)
			if !ok {
				t.Fatal("expected ok=true for cross-type numeric")
			}
			if got != tc.want {
				t.Fatalf("compareScalar(%v, %v) = %d, want %d", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

func TestCompareScalar_NilNotSupported(t *testing.T) {
	t.Parallel()
	// nil falls through all type switches — returns (0, false).
	if _, ok := compareScalar(nil, nil); ok {
		t.Fatal("nil vs nil should return ok=false")
	}
	if _, ok := compareScalar(nil, int64(5)); ok {
		t.Fatal("nil vs int64 should return ok=false")
	}
	if _, ok := compareScalar(int64(5), nil); ok {
		t.Fatal("int64 vs nil should return ok=false")
	}
}

func TestCompareScalar_ByteSliceNotSupported(t *testing.T) {
	t.Parallel()
	// []byte is not handled by compareScalar — returns (0, false).
	a := []byte{1, 2, 3}
	b := []byte{4, 5, 6}
	if _, ok := compareScalar(a, b); ok {
		t.Fatal("[]byte vs []byte should return ok=false")
	}
}

func TestCompareScalar_IncompatibleTypes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		a, b any
	}{
		{"string vs int64", "hello", int64(5)},
		{"int64 vs string", int64(5), "hello"},
		{"bool vs int64", true, int64(1)},
		{"int64 vs bool", int64(1), true},
		{"string vs float64", "x", float64(3.0)},
		{"float64 vs string", float64(3.0), "x"},
		{"bool vs string", true, "true"},
		{"string vs bool", "true", true},
		{"float64 vs bool", float64(1.0), true},
		{"bool vs float64", true, float64(1.0)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, ok := compareScalar(tc.a, tc.b)
			if ok {
				t.Fatalf("compareScalar(%T, %T) should return ok=false", tc.a, tc.b)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// nullifEqual
// ---------------------------------------------------------------------------

func TestNullifEqual_SameInt64(t *testing.T) {
	t.Parallel()
	if !nullifEqual(int64(42), int64(42)) {
		t.Fatal("same int64 should be equal")
	}
}

func TestNullifEqual_DifferentInt64(t *testing.T) {
	t.Parallel()
	if nullifEqual(int64(1), int64(2)) {
		t.Fatal("different int64 should not be equal")
	}
}

func TestNullifEqual_SameFloat64(t *testing.T) {
	t.Parallel()
	if !nullifEqual(float64(3.14), float64(3.14)) {
		t.Fatal("same float64 should be equal")
	}
}

func TestNullifEqual_SameString(t *testing.T) {
	t.Parallel()
	if !nullifEqual("abc", "abc") {
		t.Fatal("same string should be equal")
	}
}

func TestNullifEqual_DifferentString(t *testing.T) {
	t.Parallel()
	if nullifEqual("abc", "def") {
		t.Fatal("different string should not be equal")
	}
}

func TestNullifEqual_SameBool(t *testing.T) {
	t.Parallel()
	if !nullifEqual(true, true) {
		t.Fatal("true == true should be equal")
	}
	if !nullifEqual(false, false) {
		t.Fatal("false == false should be equal")
	}
}

func TestNullifEqual_DifferentBool(t *testing.T) {
	t.Parallel()
	if nullifEqual(true, false) {
		t.Fatal("true vs false should not be equal")
	}
}

func TestNullifEqual_CrossTypeIntFloat(t *testing.T) {
	t.Parallel()
	// int64(5) vs float64(5.0) — promoted comparison, should be equal.
	if !nullifEqual(int64(5), float64(5.0)) {
		t.Fatal("int64(5) == float64(5.0) should be equal")
	}
	if !nullifEqual(float64(7.0), int64(7)) {
		t.Fatal("float64(7.0) == int64(7) should be equal")
	}
	// Not equal when fractional part differs.
	if nullifEqual(int64(5), float64(5.5)) {
		t.Fatal("int64(5) != float64(5.5)")
	}
}

func TestNullifEqual_NilNilReturnsFalse(t *testing.T) {
	t.Parallel()
	// nil falls through all type switches — returns false.
	if nullifEqual(nil, nil) {
		t.Fatal("nil vs nil should return false (no nil handling in nullifEqual)")
	}
}

func TestNullifEqual_CrossTypeIncompatible(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		a, b any
	}{
		{"string vs int64", "5", int64(5)},
		{"int64 vs string", int64(5), "5"},
		{"bool vs int64", true, int64(1)},
		{"int64 vs bool", int64(1), true},
		{"string vs bool", "true", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if nullifEqual(tc.a, tc.b) {
				t.Fatalf("nullifEqual(%T(%v), %T(%v)) should be false", tc.a, tc.a, tc.b, tc.b)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// addInt64Checked
// ---------------------------------------------------------------------------

func TestAddInt64Checked_Normal(t *testing.T) {
	t.Parallel()
	got, ok := addInt64Checked(3, 4)
	if !ok || got != 7 {
		t.Fatalf("addInt64Checked(3, 4) = (%d, %v), want (7, true)", got, ok)
	}
}

func TestAddInt64Checked_Zero(t *testing.T) {
	t.Parallel()
	got, ok := addInt64Checked(0, 0)
	if !ok || got != 0 {
		t.Fatalf("addInt64Checked(0, 0) = (%d, %v), want (0, true)", got, ok)
	}
}

func TestAddInt64Checked_MaxPlusOneOverflows(t *testing.T) {
	t.Parallel()
	_, ok := addInt64Checked(math.MaxInt64, 1)
	if ok {
		t.Fatal("MaxInt64 + 1 should overflow")
	}
}

func TestAddInt64Checked_MinPlusNegOneOverflows(t *testing.T) {
	t.Parallel()
	_, ok := addInt64Checked(math.MinInt64, -1)
	if ok {
		t.Fatal("MinInt64 + (-1) should overflow")
	}
}

func TestAddInt64Checked_LargePositivePlusLargeNegativeNoOverflow(t *testing.T) {
	t.Parallel()
	// MaxInt64 + MinInt64 = -1, no overflow.
	got, ok := addInt64Checked(math.MaxInt64, math.MinInt64)
	if !ok {
		t.Fatal("MaxInt64 + MinInt64 should not overflow")
	}
	if got != -1 {
		t.Fatalf("got %d, want -1", got)
	}
}

func TestAddInt64Checked_NegativeNormal(t *testing.T) {
	t.Parallel()
	got, ok := addInt64Checked(-10, -20)
	if !ok || got != -30 {
		t.Fatalf("addInt64Checked(-10, -20) = (%d, %v), want (-30, true)", got, ok)
	}
}

// ---------------------------------------------------------------------------
// subInt64Checked
// ---------------------------------------------------------------------------

func TestSubInt64Checked_Normal(t *testing.T) {
	t.Parallel()
	got, ok := subInt64Checked(5, 3)
	if !ok || got != 2 {
		t.Fatalf("subInt64Checked(5, 3) = (%d, %v), want (2, true)", got, ok)
	}
}

func TestSubInt64Checked_MinMinusOneOverflows(t *testing.T) {
	t.Parallel()
	_, ok := subInt64Checked(math.MinInt64, 1)
	if ok {
		t.Fatal("MinInt64 - 1 should overflow")
	}
}

func TestSubInt64Checked_MaxMinusNegOneOverflows(t *testing.T) {
	t.Parallel()
	_, ok := subInt64Checked(math.MaxInt64, -1)
	if ok {
		t.Fatal("MaxInt64 - (-1) should overflow")
	}
}

func TestSubInt64Checked_Zero(t *testing.T) {
	t.Parallel()
	got, ok := subInt64Checked(0, 0)
	if !ok || got != 0 {
		t.Fatalf("subInt64Checked(0, 0) = (%d, %v), want (0, true)", got, ok)
	}
}

func TestSubInt64Checked_NegativeFromNegative(t *testing.T) {
	t.Parallel()
	// -5 - (-3) = -2
	got, ok := subInt64Checked(-5, -3)
	if !ok || got != -2 {
		t.Fatalf("subInt64Checked(-5, -3) = (%d, %v), want (-2, true)", got, ok)
	}
}

func TestSubInt64Checked_MaxMinusMax(t *testing.T) {
	t.Parallel()
	got, ok := subInt64Checked(math.MaxInt64, math.MaxInt64)
	if !ok || got != 0 {
		t.Fatalf("MaxInt64 - MaxInt64 = (%d, %v), want (0, true)", got, ok)
	}
}

// ---------------------------------------------------------------------------
// mulInt64Checked
// ---------------------------------------------------------------------------

func TestMulInt64Checked_Normal(t *testing.T) {
	t.Parallel()
	got, ok := mulInt64Checked(3, 4)
	if !ok || got != 12 {
		t.Fatalf("mulInt64Checked(3, 4) = (%d, %v), want (12, true)", got, ok)
	}
}

func TestMulInt64Checked_ZeroTimesAnything(t *testing.T) {
	t.Parallel()
	got, ok := mulInt64Checked(0, math.MaxInt64)
	if !ok || got != 0 {
		t.Fatalf("0 * MaxInt64 = (%d, %v), want (0, true)", got, ok)
	}
	got, ok = mulInt64Checked(math.MinInt64, 0)
	if !ok || got != 0 {
		t.Fatalf("MinInt64 * 0 = (%d, %v), want (0, true)", got, ok)
	}
}

func TestMulInt64Checked_MaxTimesTwo_Overflow(t *testing.T) {
	t.Parallel()
	_, ok := mulInt64Checked(math.MaxInt64, 2)
	if ok {
		t.Fatal("MaxInt64 * 2 should overflow")
	}
}

func TestMulInt64Checked_MinTimesNegOne_Overflow(t *testing.T) {
	t.Parallel()
	// -MinInt64 doesn't fit in int64.
	_, ok := mulInt64Checked(math.MinInt64, -1)
	if ok {
		t.Fatal("MinInt64 * -1 should overflow")
	}
	// Symmetric.
	_, ok = mulInt64Checked(-1, math.MinInt64)
	if ok {
		t.Fatal("-1 * MinInt64 should overflow")
	}
}

func TestMulInt64Checked_NegativeTimesNegative(t *testing.T) {
	t.Parallel()
	got, ok := mulInt64Checked(-3, -4)
	if !ok || got != 12 {
		t.Fatalf("(-3) * (-4) = (%d, %v), want (12, true)", got, ok)
	}
}

func TestMulInt64Checked_OneIdentity(t *testing.T) {
	t.Parallel()
	got, ok := mulInt64Checked(1, math.MaxInt64)
	if !ok || got != math.MaxInt64 {
		t.Fatalf("1 * MaxInt64 = (%d, %v)", got, ok)
	}
	got, ok = mulInt64Checked(math.MinInt64, 1)
	if !ok || got != math.MinInt64 {
		t.Fatalf("MinInt64 * 1 = (%d, %v)", got, ok)
	}
}

// ---------------------------------------------------------------------------
// scalarFnInt64Arg
// ---------------------------------------------------------------------------

func TestScalarFnInt64Arg_Int64(t *testing.T) {
	t.Parallel()
	got, ok := scalarFnInt64Arg(int64(42))
	if !ok || got != 42 {
		t.Fatalf("scalarFnInt64Arg(int64(42)) = (%d, %v)", got, ok)
	}
}

func TestScalarFnInt64Arg_WholeFloat64(t *testing.T) {
	t.Parallel()
	got, ok := scalarFnInt64Arg(float64(42.0))
	if !ok || got != 42 {
		t.Fatalf("scalarFnInt64Arg(float64(42.0)) = (%d, %v)", got, ok)
	}
}

func TestScalarFnInt64Arg_FractionalFloat64Declines(t *testing.T) {
	t.Parallel()
	_, ok := scalarFnInt64Arg(float64(42.5))
	if ok {
		t.Fatal("non-whole float64 should return ok=false")
	}
}

func TestScalarFnInt64Arg_String_Declines(t *testing.T) {
	t.Parallel()
	_, ok := scalarFnInt64Arg("hello")
	if ok {
		t.Fatal("string should return ok=false")
	}
}

func TestScalarFnInt64Arg_Nil_Declines(t *testing.T) {
	t.Parallel()
	_, ok := scalarFnInt64Arg(nil)
	if ok {
		t.Fatal("nil should return ok=false")
	}
}

func TestScalarFnInt64Arg_Bool_Declines(t *testing.T) {
	t.Parallel()
	_, ok := scalarFnInt64Arg(true)
	if ok {
		t.Fatal("bool should return ok=false")
	}
}

func TestScalarFnInt64Arg_NegativeWholeFloat64(t *testing.T) {
	t.Parallel()
	got, ok := scalarFnInt64Arg(float64(-100.0))
	if !ok || got != -100 {
		t.Fatalf("scalarFnInt64Arg(float64(-100.0)) = (%d, %v)", got, ok)
	}
}

func TestScalarFnInt64Arg_SmallIntTypes(t *testing.T) {
	t.Parallel()
	// int32 goes through ToInt64.
	got, ok := scalarFnInt64Arg(int32(7))
	if !ok || got != 7 {
		t.Fatalf("scalarFnInt64Arg(int32(7)) = (%d, %v)", got, ok)
	}
}

// ---------------------------------------------------------------------------
// ValueSize
// ---------------------------------------------------------------------------

func TestValueSize_Leaf_ConstantValue(t *testing.T) {
	t.Parallel()
	v := &ConstantValue{Value: int64(99), Typ: NotNullLong}
	if got := ValueSize(v); got != 1 {
		t.Fatalf("ValueSize(ConstantValue) = %d, want 1", got)
	}
}

func TestValueSize_TwoLevel(t *testing.T) {
	t.Parallel()
	// ArithmeticValue with two leaf children: 1 + 2 = 3 nodes.
	v := &ArithmeticValue{
		Op:    OpAdd,
		Left:  &ConstantValue{Value: int64(1), Typ: NotNullLong},
		Right: &ConstantValue{Value: int64(2), Typ: NotNullLong},
	}
	if got := ValueSize(v); got != 3 {
		t.Fatalf("ValueSize(arith(const, const)) = %d, want 3", got)
	}
}

func TestValueSize_DeepNesting(t *testing.T) {
	t.Parallel()
	// Cast(Arith(const, const)) = 4 nodes.
	inner := &ArithmeticValue{
		Op:    OpMul,
		Left:  &ConstantValue{Value: int64(2), Typ: NotNullLong},
		Right: &ConstantValue{Value: int64(3), Typ: NotNullLong},
	}
	outer := &CastValue{Child: inner, Target: NullableString}
	if got := ValueSize(outer); got != 4 {
		t.Fatalf("ValueSize(Cast(Arith(c,c))) = %d, want 4", got)
	}
}

func TestValueSize_NullValue(t *testing.T) {
	t.Parallel()
	if got := ValueSize(&NullValue{Typ: NullableLong}); got != 1 {
		t.Fatalf("ValueSize(NullValue) = %d, want 1", got)
	}
}

func TestValueSize_RecordConstructor(t *testing.T) {
	t.Parallel()
	// RecordConstructor with two fields: parent + 2 children = 3.
	rc := NewRecordConstructorValue(
		RecordConstructorField{Name: "x", Value: &ConstantValue{Value: int64(1), Typ: NotNullLong}},
		RecordConstructorField{Name: "y", Value: &ConstantValue{Value: int64(2), Typ: NotNullLong}},
	)
	if got := ValueSize(rc); got != 3 {
		t.Fatalf("ValueSize(RecordConstructor(c,c)) = %d, want 3", got)
	}
}

// ---------------------------------------------------------------------------
// IsConstantValue
// ---------------------------------------------------------------------------

func TestIsConstantValue_ConstantValue(t *testing.T) {
	t.Parallel()
	if !IsConstantValue(&ConstantValue{Value: int64(1), Typ: NotNullLong}) {
		t.Fatal("ConstantValue should be constant")
	}
}

func TestIsConstantValue_NullValue(t *testing.T) {
	t.Parallel()
	if !IsConstantValue(&NullValue{Typ: NullableLong}) {
		t.Fatal("NullValue should be constant")
	}
}

func TestIsConstantValue_BooleanValueTrue(t *testing.T) {
	t.Parallel()
	if !IsConstantValue(NewBooleanValue(true)) {
		t.Fatal("BooleanValue(true) should be constant")
	}
}

func TestIsConstantValue_BooleanValueNil(t *testing.T) {
	t.Parallel()
	if !IsConstantValue(&BooleanValue{Value: nil}) {
		t.Fatal("BooleanValue(nil) should be constant")
	}
}

func TestIsConstantValue_FieldValue(t *testing.T) {
	t.Parallel()
	if IsConstantValue(&FieldValue{Field: "x", Typ: NotNullLong}) {
		t.Fatal("FieldValue should NOT be constant")
	}
}

func TestIsConstantValue_ParameterValue(t *testing.T) {
	t.Parallel()
	if IsConstantValue(NewParameterValue(1)) {
		t.Fatal("ParameterValue should NOT be constant")
	}
}

func TestIsConstantValue_NilValue(t *testing.T) {
	t.Parallel()
	if IsConstantValue(nil) {
		t.Fatal("nil should NOT be constant")
	}
}

func TestIsConstantValue_CompositeAllConstant(t *testing.T) {
	t.Parallel()
	// CAST(1 + 2 AS STRING) — all children are constant.
	v := &CastValue{
		Child: &ArithmeticValue{
			Op:    OpAdd,
			Left:  &ConstantValue{Value: int64(1), Typ: NotNullLong},
			Right: &ConstantValue{Value: int64(2), Typ: NotNullLong},
		},
		Target: NullableString,
	}
	if !IsConstantValue(v) {
		t.Fatal("CAST(1+2 AS STRING) should be constant")
	}
}

func TestIsConstantValue_CompositeWithField(t *testing.T) {
	t.Parallel()
	v := &ArithmeticValue{
		Op:    OpAdd,
		Left:  &FieldValue{Field: "x", Typ: NotNullLong},
		Right: &ConstantValue{Value: int64(1), Typ: NotNullLong},
	}
	if IsConstantValue(v) {
		t.Fatal("x + 1 should NOT be constant")
	}
}

// ---------------------------------------------------------------------------
// ContainsAggregate
// ---------------------------------------------------------------------------

func TestContainsAggregate_ConstantValue(t *testing.T) {
	t.Parallel()
	if ContainsAggregate(&ConstantValue{Value: int64(1), Typ: NotNullLong}) {
		t.Fatal("ConstantValue should not contain aggregate")
	}
}

func TestContainsAggregate_DirectAggregateValue(t *testing.T) {
	t.Parallel()
	v := NewAggregateValue(AggCountStar, nil)
	if !ContainsAggregate(v) {
		t.Fatal("AggregateValue should contain aggregate")
	}
}

func TestContainsAggregate_NestedAggregate(t *testing.T) {
	t.Parallel()
	// ArithmeticValue wrapping an aggregate: SUM(x) + 1
	v := &ArithmeticValue{
		Op:    OpAdd,
		Left:  NewAggregateValue(AggSum, &FieldValue{Field: "x", Typ: NotNullLong}),
		Right: &ConstantValue{Value: int64(1), Typ: NotNullLong},
	}
	if !ContainsAggregate(v) {
		t.Fatal("tree containing SUM(x) should contain aggregate")
	}
}

func TestContainsAggregate_NilSafe(t *testing.T) {
	t.Parallel()
	if ContainsAggregate(nil) {
		t.Fatal("nil should not contain aggregate")
	}
}

// ---------------------------------------------------------------------------
// IsNonEvaluable
// ---------------------------------------------------------------------------

func TestIsNonEvaluable_QuantifiedObjectValue(t *testing.T) {
	t.Parallel()
	// QuantifiedObjectValue does NOT implement NonEvaluable — it has
	// a working Evaluate method. IsNonEvaluable returns false.
	v := NewQuantifiedObjectValue(NamedCorrelationIdentifier("t"))
	if IsNonEvaluable(v) {
		t.Fatal("QuantifiedObjectValue should NOT be NonEvaluable")
	}
}

func TestIsNonEvaluable_ConstantValue(t *testing.T) {
	t.Parallel()
	if IsNonEvaluable(&ConstantValue{Value: int64(7), Typ: NotNullLong}) {
		t.Fatal("ConstantValue should NOT be NonEvaluable")
	}
}

// ---------------------------------------------------------------------------
// Benchmarks — overflow-checked arithmetic
// ---------------------------------------------------------------------------

func BenchmarkAddInt64Checked(b *testing.B) {
	a := int64(1234567890)
	c := int64(9876543210)
	for b.Loop() {
		addInt64Checked(a, c)
	}
}

func BenchmarkSubInt64Checked(b *testing.B) {
	a := int64(9876543210)
	c := int64(1234567890)
	for b.Loop() {
		subInt64Checked(a, c)
	}
}

func BenchmarkMulInt64Checked(b *testing.B) {
	a := int64(123456)
	c := int64(654321)
	for b.Loop() {
		mulInt64Checked(a, c)
	}
}

func BenchmarkAddInt64Checked_Overflow(b *testing.B) {
	a := math.MaxInt64
	c := int64(1)
	for b.Loop() {
		addInt64Checked(int64(a), c)
	}
}

func BenchmarkMulInt64Checked_Overflow(b *testing.B) {
	a := int64(math.MaxInt64)
	c := int64(2)
	for b.Loop() {
		mulInt64Checked(a, c)
	}
}
