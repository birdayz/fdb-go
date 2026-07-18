package values

// Java-test-suite-inspired unit tests for the Value hierarchy.
//
// These ports take the parameterized-table style of Java's
// fdb-record-layer-core/src/test/java/com/apple/foundationdb/record/
// query/plan/cascades/{ArithmeticValueTest,BooleanValueTest}.java but
// keep within our seed's surface — int64-only ArithmeticValue
// evaluation, no Type hierarchy promotion yet, no Bindings /
// EvaluationContext machinery. Cross-type coercion cases (Java
// promotes long↔int, float↔double, etc.) are deliberately omitted
// until Phase 4.0 ports the Type hierarchy. The tests will then
// extend naturally to the broader surface — same pattern, more rows.
//
// Test discipline goal (per RFC-025): each Value subtype gets parameterised
// coverage that runs in <100ms with no FDB / no testcontainer / no
// conformance server. When Phase 1 splits this file into
// `pkg/recordlayer/query/plan/cascades/values/`, these tests move with
// the source files; only the import path changes.

import (
	"errors"
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

// ----- ArithmeticValue ---------------------------------------------------

// TestArithmeticValue_BinaryOps_Parameterised mirrors
// ArithmeticValueTest.BinaryPredicateTestProvider: a flat table of
// (op, left, right, want) cases so a regression in one operator
// surfaces with the failing row visible.
func TestArithmeticValue_BinaryOps_Parameterised(t *testing.T) {
	t.Parallel()
	// Operands carry plan-time ordinals — sorted {a,b,c} → a=0, b=1, c=2
	// (fom sorts keys), read positionally from the row. Statically LONG:
	// this is the LONG-lane parity table (rows feed MaxInt64-scale data).
	a := NewFieldValueWithResolvedOrdinal("a", 0, NullableLong)
	b := NewFieldValueWithResolvedOrdinal("b", 1, NullableLong)
	cases := []struct {
		name string
		op   ArithmeticOp
		l, r int64
		want any
	}{
		// Java's "Arguments.of(List.of(INT_1, INT_1), AddFn(), 2, false)" line by line.
		{"add 1+1", OpAdd, 1, 1, int64(2)},
		{"add 0+0", OpAdd, 0, 0, int64(0)},
		{"add neg+neg", OpAdd, -3, -4, int64(-7)},
		{"add max-1+1 in range", OpAdd, math.MaxInt64 - 1, 1, int64(math.MaxInt64)},
		{"sub 1-1", OpSub, 1, 1, int64(0)},
		{"sub 10-3", OpSub, 10, 3, int64(7)},
		{"sub 0 - max", OpSub, 0, math.MaxInt64, int64(-math.MaxInt64)},
		{"mul 0", OpMul, 7, 0, int64(0)},
		{"mul 1", OpMul, 12345, 1, int64(12345)},
		{"mul -2 * 3", OpMul, -2, 3, int64(-6)},
		{"mul -2 * -3", OpMul, -2, -3, int64(6)},
		{"div 20/4", OpDiv, 20, 4, int64(5)},
		{"div trunc-toward-zero +", OpDiv, 7, 2, int64(3)},
		{"div trunc-toward-zero -", OpDiv, -7, 2, int64(-3)},
		{"div by 1", OpDiv, math.MaxInt64, 1, int64(math.MaxInt64)},
		{"mod 20%7", OpMod, 20, 7, int64(6)},
		{"mod neg dividend", OpMod, -20, 7, int64(-6)}, // Go truncates toward zero, MySQL/Postgres parity
		{"mod neg divisor", OpMod, 20, -7, int64(6)},
		{"mod result-zero", OpMod, 21, 7, int64(0)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			av := &ArithmeticValue{Op: tc.op, Left: a, Right: b}
			got, errEv0 := av.Evaluate(fom(map[string]any{"a": tc.l, "b": tc.r}))
			require.NoError(t, errEv0)
			if got != tc.want {
				t.Fatalf("op %v %d %d: got %v, want %v", tc.op, tc.l, tc.r, got, tc.want)
			}
		})
	}
}

// TestArithmeticValue_OverflowPanics pins that integer overflow returns
// ArithmeticOverflowError on the error channel (matching Java's
// Math.addExact throwing ArithmeticException). The executor maps it to 22003.
// DIV is deliberately absent: Java's DIV_LL/DIV_II are UNCHECKED casts
// (`(long)l / (long)r`, ArithmeticValue.java:464/469) — the JVM wraps
// MinLong/-1 with no exception, so Go wraps too
// (TestArithmeticValue_DivMinWrapsLikeJava).
// TestArithmeticValue_DivMinWrapsLikeJava pins DIV_LL parity: MinInt64/-1
// WRAPS to MinInt64 (Java's unchecked JVM long division), where Go's
// native `/` would panic — the wrap is explicit in the long lane.
func TestArithmeticValue_DivMinWrapsLikeJava(t *testing.T) {
	t.Parallel()
	a := NewFieldValueWithResolvedOrdinal("a", 0, NullableLong)
	b := NewFieldValueWithResolvedOrdinal("b", 1, NullableLong)
	av := &ArithmeticValue{Op: OpDiv, Left: a, Right: b}
	v, err := av.Evaluate(fom(map[string]any{"a": int64(math.MinInt64), "b": int64(-1)}))
	if err != nil {
		t.Fatalf("MinInt64 / -1 must WRAP like Java DIV_LL, got error %v", err)
	}
	if got, ok := v.(int64); !ok || got != math.MinInt64 {
		t.Fatalf("MinInt64 / -1 = %v, want MinInt64 (JVM wrap parity)", v)
	}
}

func TestArithmeticValue_OverflowPanics(t *testing.T) {
	t.Parallel()
	// Statically LONG — these rows pin the LONG lane's Math.*Exact
	// bounds; under INT static types they'd exercise the range-guard
	// fall-through instead (pinned separately below).
	a := NewFieldValueWithResolvedOrdinal("a", 0, NullableLong)
	b := NewFieldValueWithResolvedOrdinal("b", 1, NullableLong)
	cases := []struct {
		name string
		op   ArithmeticOp
		l, r int64
	}{
		{"add MAX+1", OpAdd, math.MaxInt64, 1},
		{"add MIN+(-1)", OpAdd, math.MinInt64, -1},
		{"add MAX+MAX", OpAdd, math.MaxInt64, math.MaxInt64},
		{"sub MIN-1", OpSub, math.MinInt64, 1},
		{"sub MAX-(-1)", OpSub, math.MaxInt64, -1},
		{"mul MAX*2", OpMul, math.MaxInt64, 2},
		{"mul MIN*-1", OpMul, math.MinInt64, -1},
		{"mul -1*MIN", OpMul, -1, math.MinInt64},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			av := &ArithmeticValue{Op: tc.op, Left: a, Right: b}
			v, err := av.Evaluate(fom(map[string]any{"a": tc.l, "b": tc.r}))
			if v != nil || err == nil {
				t.Fatalf("op %v %d %d: got (%v, %v), want (nil, ArithmeticOverflowError)", tc.op, tc.l, tc.r, v, err)
			}
			var overflow *ArithmeticOverflowError
			if !errors.As(err, &overflow) {
				t.Fatalf("op %v %d %d: got %T, want *ArithmeticOverflowError", tc.op, tc.l, tc.r, err)
			}
		})
	}
}

// TestArithmeticValue_OverflowBoundaries pins that VALUES AT the
// overflow boundary still succeed — the inequality is strict.
// Asymmetric for sub: MAX - (-1) overflows, but MAX - 1 = MAX-1.
func TestArithmeticValue_OverflowBoundaries(t *testing.T) {
	t.Parallel()
	// Statically LONG — boundary rows are MaxInt64/MinInt64 values.
	a := NewFieldValueWithResolvedOrdinal("a", 0, NullableLong)
	b := NewFieldValueWithResolvedOrdinal("b", 1, NullableLong)
	cases := []struct {
		name string
		op   ArithmeticOp
		l, r int64
		want any
	}{
		{"add MAX+0", OpAdd, math.MaxInt64, 0, int64(math.MaxInt64)},
		{"add MIN+0", OpAdd, math.MinInt64, 0, int64(math.MinInt64)},
		{"sub MIN-0", OpSub, math.MinInt64, 0, int64(math.MinInt64)},
		{"mul MAX*1", OpMul, math.MaxInt64, 1, int64(math.MaxInt64)},
		{"mul MIN*1", OpMul, math.MinInt64, 1, int64(math.MinInt64)},
		{"mul 0*MAX", OpMul, 0, math.MaxInt64, int64(0)},
		{"mod MIN%-1", OpMod, math.MinInt64, -1, int64(0)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			av := &ArithmeticValue{Op: tc.op, Left: a, Right: b}
			got, errEv0 := av.Evaluate(fom(map[string]any{"a": tc.l, "b": tc.r}))
			require.NoError(t, errEv0)
			if got != tc.want {
				t.Fatalf("op %v %d %d: got %v, want %v", tc.op, tc.l, tc.r, got, tc.want)
			}
		})
	}
}

// TestArithmeticValue_NullPropagation_Deep pins NULL-propagation
// through nested arithmetic — Java's test stresses this because the
// Cascades simplifier folds NULL holes at multiple depths.
// Our ArithmeticValue should propagate NULL through any depth so the
// surrounding NullPropagationRule (Phase 4.5) has a stable contract
// to rewrite against.
func TestArithmeticValue_NullPropagation_Deep(t *testing.T) {
	t.Parallel()
	// Operands carry plan-time ordinals — sorted {a,b,c} → a=0, b=1, c=2
	// (fom sorts keys), read positionally from the row.
	a := NewFieldValueWithResolvedOrdinal("a", 0, NullableInt)
	b := NewFieldValueWithResolvedOrdinal("b", 1, NullableInt)
	c := NewFieldValueWithResolvedOrdinal("c", 2, NullableInt)
	// (a + b) * c
	tree := &ArithmeticValue{
		Op:    OpMul,
		Left:  &ArithmeticValue{Op: OpAdd, Left: a, Right: b},
		Right: c,
	}
	cases := []struct {
		name string
		row  map[string]any
		want any
	}{
		{"all-non-null", map[string]any{"a": int64(2), "b": int64(3), "c": int64(4)}, int64(20)},
		{"a NULL", map[string]any{"a": nil, "b": int64(3), "c": int64(4)}, nil},
		{"b NULL", map[string]any{"a": int64(2), "b": nil, "c": int64(4)}, nil},
		{"c NULL", map[string]any{"a": int64(2), "b": int64(3), "c": nil}, nil},
		{"all NULL", map[string]any{"a": nil, "b": nil, "c": nil}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, errEv0 := tree.Evaluate(fom(tc.row))
			require.NoError(t, errEv0)
			if got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// TestArithmeticValue_DivByZero_AllOps pins that / and % by zero return
// ArithmeticDivisionByZeroError on the error channel (matches Java's
// ArithmeticException). The executor surfaces it as a SQL error.
func TestArithmeticValue_DivByZero_AllOps(t *testing.T) {
	t.Parallel()
	// Both integer lanes raise on /0 and %0 (Java DIV_II and DIV_LL alike
	// throw ArithmeticException for zero divisors) — cover each lane.
	for _, lane := range []struct {
		name string
		typ  Type
	}{{"int_lane", NullableInt}, {"long_lane", NullableLong}} {
		lane := lane
		t.Run(lane.name, func(t *testing.T) {
			t.Parallel()
			a := NewFieldValueWithResolvedOrdinal("a", 0, lane.typ)
			b := NewFieldValueWithResolvedOrdinal("b", 1, lane.typ)
			divByZeroOps(t, a, b)
		})
	}
}

func divByZeroOps(t *testing.T, a, b Value) {
	t.Helper()
	for _, op := range []ArithmeticOp{OpDiv, OpMod} {
		op := op
		t.Run(op.Symbol(), func(t *testing.T) {
			t.Parallel()
			av := &ArithmeticValue{Op: op, Left: a, Right: b}
			v, err := av.Evaluate(fom(map[string]any{"a": int64(5), "b": int64(0)}))
			if v != nil || err == nil {
				t.Fatalf("%v by zero: got (%v, %v), want (nil, ArithmeticDivisionByZeroError)", op, v, err)
			}
			var divByZero *ArithmeticDivisionByZeroError
			if !errors.As(err, &divByZero) {
				t.Fatalf("%v by zero: got %T, want *ArithmeticDivisionByZeroError", op, err)
			}
		})
	}
}

// TestArithmeticValue_TypeMismatch_Panics verifies that ArithmeticValue
// returns ScalarTypeMismatchError on type mismatches (string + int,
// bool + int). Java-aligned: Java's SemanticAnalyzer catches this at
// compile time; Go catches it at eval time on the error channel, mapped
// to SQLSTATE 42804 by the executor.
func TestArithmeticValue_TypeMismatch_Panics(t *testing.T) {
	t.Parallel()
	// Operands carry plan-time ordinals — sorted {a,b,c} → a=0, b=1, c=2
	// (fom sorts keys), read positionally from the row.
	a := NewFieldValueWithResolvedOrdinal("a", 0, NullableInt)
	b := NewFieldValueWithResolvedOrdinal("b", 1, NullableInt)
	cases := []struct {
		name string
		row  map[string]any
	}{
		{"string + int", map[string]any{"a": "foo", "b": int64(1)}},
		{"int + string", map[string]any{"a": int64(1), "b": "foo"}},
		{"bool + int", map[string]any{"a": true, "b": int64(1)}},
		{"int + bool", map[string]any{"a": int64(1), "b": false}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			av := &ArithmeticValue{Op: OpAdd, Left: a, Right: b}
			v, err := av.Evaluate(fom(tc.row))
			if v != nil || err == nil {
				t.Fatalf("type mismatch %v: got (%v, %v), want (nil, ScalarTypeMismatchError)", tc.row, v, err)
			}
			var mismatch *ScalarTypeMismatchError
			if !errors.As(err, &mismatch) {
				t.Fatalf("type mismatch %v: got %T, want *ScalarTypeMismatchError", tc.row, err)
			}
		})
	}
}

// ----- BooleanValue ------------------------------------------------------

// TestBooleanValue_KleeneTriBool pins the three-valued logic
// surface — true / false / nil-as-UNKNOWN — that BooleanValueTest
// exercises in Java. Our seed BooleanValue keeps the literal as a
// bool pointer; nil pointer renders UNKNOWN and Evaluate returns nil.
func TestBooleanValue_KleeneTriBool(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		val  *bool
		want any
	}{
		{"true literal", boolPtr(true), true},
		{"false literal", boolPtr(false), false},
		{"unknown literal", nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			bv := &BooleanValue{Value: tc.val}
			got, errEv0 := bv.Evaluate(nil)
			require.NoError(t, errEv0)
			if got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			if bv.Type().Code() != TypeCodeBoolean {
				t.Fatalf("Type: got %v, want a boolean type", bv.Type())
			}
		})
	}
}

func boolPtr(b bool) *bool { return &b }

// TestBooleanValue_NewBooleanValueFactory pins NewBooleanValue's
// equivalence with the literal constructor. Java has multiple
// factory methods (LiteralValue<Boolean>, BooleanValue.True / False);
// we have one factory + a literal struct, both surfaces should
// behave identically.
func TestBooleanValue_NewBooleanValueFactory(t *testing.T) {
	t.Parallel()
	got, errEv0 := NewBooleanValue(true).Evaluate(nil)
	require.NoError(t, errEv0)
	if got != true {
		t.Fatalf("NewBooleanValue(true): got %v", got)
	}
	got, errEv1 := NewBooleanValue(false).Evaluate(nil)
	require.NoError(t, errEv1)
	if got != false {
		t.Fatalf("NewBooleanValue(false): got %v", got)
	}
	// Verify factory and direct-struct paths produce equal Evaluate
	// behaviour. Two separate factories should not diverge on the
	// happy path even if internal pointer identities differ.
	a := NewBooleanValue(true)
	b := &BooleanValue{Value: boolPtr(true)}
	tmpEv0, errEv2 := a.Evaluate(nil)
	require.NoError(t, errEv2)
	tmpEv1, errEv3 := b.Evaluate(nil)
	require.NoError(t, errEv3)
	if tmpEv0 != tmpEv1 {
		t.Fatal("factory and literal produce divergent Evaluate")
	}
}

// ----- CastValue ---------------------------------------------------------

// TestCastValue_Identity_Parameterised mirrors Java's
// CastValueTest.identity test cases — the cast is the identity when
// the source and target types match. Our seed CastValue evaluates
// CAST(int AS INTEGER) == the original int.
func TestCastValue_Identity_Parameterised(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		v    any
		typ  Type
	}{
		{"int → int", int64(42), TypeInt},
		{"string → string", "hello", TypeString},
		{"bool → bool", true, TypeBool},
		{"float → float", float64(3.14), TypeFloat},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			lit := &ConstantValue{Value: tc.v, Typ: tc.typ}
			c := NewCastValue(lit, tc.typ)
			got, errEv0 := c.Evaluate(nil)
			require.NoError(t, errEv0)
			if got != tc.v {
				t.Fatalf("identity cast: got %v, want %v", got, tc.v)
			}
		})
	}
}

// TestCastValue_NullPropagation pins that CAST(NULL AS X) is NULL,
// not the type's zero value. This matches Java's CastValueTest
// nullPropagationTest and SQL §6.13 General Rule 1.
func TestCastValue_NullPropagation(t *testing.T) {
	t.Parallel()
	for _, target := range []Type{TypeInt, TypeString, TypeBool, TypeFloat} {
		target := target
		t.Run(target.Code().String(), func(t *testing.T) {
			t.Parallel()
			null := &NullValue{Typ: TypeUnknown}
			c := NewCastValue(null, target)
			got, errEv0 := c.Evaluate(nil)
			require.NoError(t, errEv0)
			if got != nil {
				t.Fatalf("CAST(NULL AS %v): got %v, want nil", target, got)
			}
			// CastValue.Type() forces nullable; the targets above are
			// already nullable singletons, so equality holds.
			if c.Type().Code() != target.Code() {
				t.Fatalf("CastValue.Type after NULL propagation: got %v, want code %v", c.Type(), target.Code())
			}
		})
	}
}

// TestArithmeticValue_IntLaneOverflowsAt32Bits pins RFC-181 P0.5: Java
// types INTEGER operands INT and runs ADD_II/MUL_II = Math.addExact(int,
// int) — overflow at 2^31 errors with the 22003 class. Go widened
// everything to int64 and only checked the int64 boundary, silently
// returning the wide value. With BOTH operands statically INT the int
// lane now enforces the int32 bounds.
func TestArithmeticValue_IntLaneOverflowsAt32Bits(t *testing.T) {
	t.Parallel()
	a := NewFieldValueWithResolvedOrdinal("a", 0, NullableInt)
	b := NewFieldValueWithResolvedOrdinal("b", 1, NullableInt)

	av := &ArithmeticValue{Op: OpAdd, Left: a, Right: b}
	v, err := av.Evaluate(fom(map[string]any{"a": int64(2000000000), "b": int64(2000000000)}))
	var overflow *ArithmeticOverflowError
	if !errors.As(err, &overflow) {
		t.Fatalf("INT + INT crossing 2^31 must overflow like Java ADD_II (Math.addExact(int,int)); got (%v, %v)", v, err)
	}

	// In-range INT arithmetic still succeeds.
	v, err = av.Evaluate(fom(map[string]any{"a": int64(1000000000), "b": int64(1000000000)}))
	if err != nil || v != int64(2000000000) {
		t.Fatalf("in-range INT add = (%v, %v), want 2000000000", v, err)
	}

	// LONG-typed operands keep the 64-bit boundary: the same magnitudes
	// that overflow the INT lane are fine as LONG.
	al := NewFieldValueWithResolvedOrdinal("a", 0, NullableLong)
	bl := NewFieldValueWithResolvedOrdinal("b", 1, NullableLong)
	avl := &ArithmeticValue{Op: OpAdd, Left: al, Right: bl}
	v, err = avl.Evaluate(fom(map[string]any{"a": int64(2000000000), "b": int64(2000000000)}))
	if err != nil || v != int64(4000000000) {
		t.Fatalf("LONG add of the same magnitudes = (%v, %v), want 4000000000", v, err)
	}

	// DIV_II wraps MinInt32/-1 (Java's unchecked (int) division).
	avd := &ArithmeticValue{Op: OpDiv, Left: a, Right: b}
	v, err = avd.Evaluate(fom(map[string]any{"a": int64(math.MinInt32), "b": int64(-1)}))
	if err != nil || v != int64(math.MinInt32) {
		t.Fatalf("MinInt32 / -1 on the INT lane = (%v, %v), want MinInt32 (JVM (int) wrap)", v, err)
	}
}

// TestArithmeticValue_FloatLaneComputesInFloat32 pins the FLOAT lane
// (Java ADD_FF and the mixed IF/LF operators): computation in float32, so
// overflow saturates to ±Inf at the float32 boundary (~3.4e38) where the
// double lane returns the finite wide value.
func TestArithmeticValue_FloatLaneComputesInFloat32(t *testing.T) {
	t.Parallel()
	a := NewFieldValueWithResolvedOrdinal("a", 0, NullableFloat)
	b := NewFieldValueWithResolvedOrdinal("b", 1, NullableFloat)

	av := &ArithmeticValue{Op: OpAdd, Left: a, Right: b}
	v, err := av.Evaluate(fom(map[string]any{"a": float64(3e38), "b": float64(3e38)}))
	if err != nil {
		t.Fatalf("FLOAT add: %v", err)
	}
	f, ok := v.(float64)
	if !ok || !math.IsInf(f, 1) {
		t.Fatalf("FLOAT + FLOAT over the float32 boundary = %v, want +Inf (Java computes ADD_FF in float32)", v)
	}
}

// TestArithmeticValue_NestedIntArithmetic pins that the INT lane holds
// through NESTED arithmetic: ArithmeticValue.Type() declares INT⊕INT as
// INT (Java ADD_II's result type), so the OUTER op of `(a+b)+c` also
// dispatches on the int lane and errors at the int32 boundary. Before
// the Type() fix the intermediate claimed LONG and the outer op
// silently returned the wide value.
func TestArithmeticValue_NestedIntArithmetic(t *testing.T) {
	t.Parallel()
	a := NewFieldValueWithResolvedOrdinal("a", 0, NullableInt)
	b := NewFieldValueWithResolvedOrdinal("b", 1, NullableInt)
	c := NewFieldValueWithResolvedOrdinal("c", 2, NullableInt)
	inner := &ArithmeticValue{Op: OpAdd, Left: a, Right: b}
	if got := inner.Type(); got != NullableInt {
		t.Fatalf("INT+INT static type: got %v, want NullableInt (Java ADD_II result type)", got)
	}
	tree := &ArithmeticValue{Op: OpAdd, Left: inner, Right: c}
	// Inner sum 2_000_000_000 stays inside int32; only the outer op
	// crosses 2^31 — so ONLY correct outer-lane dispatch catches it.
	v, err := tree.Evaluate(fom(map[string]any{
		"a": int64(1_500_000_000), "b": int64(500_000_000), "c": int64(200_000_000),
	}))
	var overflow *ArithmeticOverflowError
	if !errors.As(err, &overflow) {
		t.Fatalf("nested INT add crossing 2^31: got (%v, %v), want ArithmeticOverflowError", v, err)
	}
	// Same magnitudes over LONG stay fine.
	al := NewFieldValueWithResolvedOrdinal("a", 0, NullableLong)
	bl := NewFieldValueWithResolvedOrdinal("b", 1, NullableLong)
	cl := NewFieldValueWithResolvedOrdinal("c", 2, NullableLong)
	treeL := &ArithmeticValue{
		Op:    OpAdd,
		Left:  &ArithmeticValue{Op: OpAdd, Left: al, Right: bl},
		Right: cl,
	}
	got, err := treeL.Evaluate(fom(map[string]any{
		"a": int64(1_500_000_000), "b": int64(500_000_000), "c": int64(200_000_000),
	}))
	require.NoError(t, err)
	if got != int64(2_200_000_000) {
		t.Fatalf("LONG nested add: got %v, want 2200000000", got)
	}
}

// TestArithmeticValue_IntLaneRangeGuard pins the guard explicitly: when
// the STATIC type says INT but the runtime value exceeds int32 (a state
// unreachable in Java, whose sound typing makes the (int) cast safe),
// the op falls through to the LONG lane and computes wide — it must not
// emulate Java's cast truncation, which no valid execution produces.
func TestArithmeticValue_IntLaneRangeGuard(t *testing.T) {
	t.Parallel()
	a := NewFieldValueWithResolvedOrdinal("a", 0, NullableInt)
	b := NewFieldValueWithResolvedOrdinal("b", 1, NullableInt)
	av := &ArithmeticValue{Op: OpAdd, Left: a, Right: b}
	got, err := av.Evaluate(fom(map[string]any{"a": int64(3_000_000_000), "b": int64(3_000_000_000)}))
	require.NoError(t, err)
	if got != int64(6_000_000_000) {
		t.Fatalf("out-of-int32-range INT-typed operands: got %v, want 6000000000 (LONG-lane fall-through)", got)
	}
}

// TestArithmeticValue_FloatLaneSingleRounding pins that integer operands
// convert DIRECTLY int64→float32 (Java's (float)l in ADD_LF — ONE
// rounding step). Routing through float64 first double-rounds: for the
// witness value float32(v) ≠ float32(float64(v)).
func TestArithmeticValue_FloatLaneSingleRounding(t *testing.T) {
	t.Parallel()
	const witness = int64(1<<62 | 1<<38 | 1)
	if float32(witness) == float32(float64(witness)) {
		t.Fatal("witness no longer distinguishes single from double rounding")
	}
	a := NewFieldValueWithResolvedOrdinal("a", 0, NullableLong)
	b := NewFieldValueWithResolvedOrdinal("b", 1, NullableFloat)
	av := &ArithmeticValue{Op: OpAdd, Left: a, Right: b}
	got, err := av.Evaluate(fom(map[string]any{"a": witness, "b": float64(float32(0))}))
	require.NoError(t, err)
	want := float64(float32(witness) + 0)
	if got != want {
		t.Fatalf("LF add: got %v, want %v (single-rounded (float)long)", got, want)
	}
}

// TestArithmeticValue_AddStringConcatenates pins Java's ADD string
// family (ArithmeticValue.java ADD_IS/LS/FS/DS/SI/SL/SF/SD/SS): `+`
// with a STRING operand concatenates, rendering numbers exactly as
// Java's string coercion — Integer/Long decimal, Float/Double via
// their toString (".0" on whole values, upper-case E exponents with no
// plus sign, Infinity/NaN spellings).
func TestArithmeticValue_AddStringConcatenates(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		l, r   any
		lt, rt Type
		want   string
	}{
		{"int+string", int64(5), "abc", NullableInt, TypeString, "5abc"},
		{"string+long", "n=", int64(-7), TypeString, NullableLong, "n=-7"},
		{"string+string", "ab", "cd", TypeString, TypeString, "abcd"},
		{"double whole keeps .0", "d=", float64(1), TypeString, NullableDouble, "d=1.0"},
		{"double exponent E form", "d=", float64(1e10), TypeString, NullableDouble, "d=1.0E10"},
		{"double negative exponent", "d=", float64(1e-10), TypeString, NullableDouble, "d=1.0E-10"},
		// Java's decimal/scientific boundary is [1e-3, 1e7): inside stays
		// DECIMAL (2e6 → "2000000.0", 0.001 → "0.001"), outside is
		// scientific with an UNPADDED exponent (1e7 → "1.0E7",
		// 1e-4 → "1.0E-4") — Go's 'g' would have picked "2e+06"/"0.0001".
		{"double below sci boundary decimal", "d=", float64(2e6), TypeString, NullableDouble, "d=2000000.0"},
		{"double at sci boundary", "d=", float64(1e7), TypeString, NullableDouble, "d=1.0E7"},
		{"double small decimal edge", "d=", float64(0.001), TypeString, NullableDouble, "d=0.001"},
		{"double below decimal edge", "d=", float64(1e-4), TypeString, NullableDouble, "d=1.0E-4"},
		{"double tiny", "d=", float64(1e-5), TypeString, NullableDouble, "d=1.0E-5"},
		{"double negative sci", "d=", float64(-2.5e8), TypeString, NullableDouble, "d=-2.5E8"},
		{"float renders float32", "f=", float64(1.5), TypeString, NullableFloat, "f=1.5"},
		{"float infinity spelling", "f=", math.Inf(1), TypeString, NullableFloat, "f=Infinity"},
		{"double NaN spelling", "d=", math.NaN(), TypeString, NullableDouble, "d=NaN"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			av := &ArithmeticValue{
				Op:    OpAdd,
				Left:  &ConstantValue{Value: tc.l, Typ: tc.lt},
				Right: &ConstantValue{Value: tc.r, Typ: tc.rt},
			}
			if got := av.Type(); got != NullableString {
				t.Fatalf("static type: got %v, want NullableString (Java ADD_*S result type)", got)
			}
			got, err := av.Evaluate(nil)
			require.NoError(t, err)
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
	// Non-ADD ops with a string operand still error (Java has no SUB/MUL
	// string operators).
	sub := &ArithmeticValue{
		Op:    OpSub,
		Left:  &ConstantValue{Value: "ab", Typ: TypeString},
		Right: &ConstantValue{Value: int64(1), Typ: NullableInt},
	}
	if _, err := sub.Evaluate(nil); err == nil {
		t.Fatal("string - int must still error (no Java SUB string operator)")
	}
}
