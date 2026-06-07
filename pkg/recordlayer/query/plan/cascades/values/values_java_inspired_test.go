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
	"math"
	"testing"
)

// ----- ArithmeticValue ---------------------------------------------------

// TestArithmeticValue_BinaryOps_Parameterised mirrors
// ArithmeticValueTest.BinaryPredicateTestProvider: a flat table of
// (op, left, right, want) cases so a regression in one operator
// surfaces with the failing row visible.
func TestArithmeticValue_BinaryOps_Parameterised(t *testing.T) {
	t.Parallel()
	a := &FieldValue{Field: "a", Typ: TypeInt}
	b := &FieldValue{Field: "b", Typ: TypeInt}
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
			got := mustEvaluate(av, map[string]any{"a": tc.l, "b": tc.r})
			if got != tc.want {
				t.Fatalf("op %v %d %d: got %v, want %v", tc.op, tc.l, tc.r, got, tc.want)
			}
		})
	}
}

// TestArithmeticValue_OverflowPanics pins that integer overflow panics
// with ArithmeticOverflowError (matching Java's Math.addExact throwing
// ArithmeticException). The executor catches this and reports 22003.
func TestArithmeticValue_OverflowPanics(t *testing.T) {
	t.Parallel()
	a := &FieldValue{Field: "a", Typ: TypeInt}
	b := &FieldValue{Field: "b", Typ: TypeInt}
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
		{"div MIN/-1", OpDiv, math.MinInt64, -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			av := &ArithmeticValue{Op: tc.op, Left: a, Right: b}
			defer func() {
				r := recover()
				if r == nil {
					t.Fatalf("op %v %d %d should panic with ArithmeticOverflowError", tc.op, tc.l, tc.r)
				}
				if _, ok := r.(*ArithmeticOverflowError); !ok {
					t.Fatalf("expected ArithmeticOverflowError, got %T: %v", r, r)
				}
			}()
			mustEvaluate(av, map[string]any{"a": tc.l, "b": tc.r})
		})
	}
}

// TestArithmeticValue_OverflowBoundaries pins that VALUES AT the
// overflow boundary still succeed — the inequality is strict.
// Asymmetric for sub: MAX - (-1) overflows, but MAX - 1 = MAX-1.
func TestArithmeticValue_OverflowBoundaries(t *testing.T) {
	t.Parallel()
	a := &FieldValue{Field: "a", Typ: TypeInt}
	b := &FieldValue{Field: "b", Typ: TypeInt}
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
			got := mustEvaluate(av, map[string]any{"a": tc.l, "b": tc.r})
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
	a := &FieldValue{Field: "a", Typ: TypeInt}
	b := &FieldValue{Field: "b", Typ: TypeInt}
	c := &FieldValue{Field: "c", Typ: TypeInt}
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
			got := mustEvaluate(tree, tc.row)
			if got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// TestArithmeticValue_DivByZero_AllOps pins that / and % by zero panic
// with ArithmeticDivisionByZeroError (matches Java's ArithmeticException).
// The executor recovers this panic and surfaces it as a SQL error.
func TestArithmeticValue_DivByZero_AllOps(t *testing.T) {
	t.Parallel()
	a := &FieldValue{Field: "a", Typ: TypeInt}
	b := &FieldValue{Field: "b", Typ: TypeInt}
	for _, op := range []ArithmeticOp{OpDiv, OpMod} {
		op := op
		t.Run(op.Symbol(), func(t *testing.T) {
			t.Parallel()
			av := &ArithmeticValue{Op: op, Left: a, Right: b}
			func() {
				defer func() {
					r := recover()
					if r == nil {
						t.Fatalf("%v by zero: expected panic", op)
					}
					if _, ok := r.(*ArithmeticDivisionByZeroError); !ok {
						t.Fatalf("%v by zero: expected *ArithmeticDivisionByZeroError, got %T", op, r)
					}
				}()
				mustEvaluate(av, map[string]any{"a": int64(5), "b": int64(0)})
			}()
		})
	}
}

// TestArithmeticValue_TypeMismatch_Panics verifies that ArithmeticValue
// panics with ScalarTypeMismatchError on type mismatches (string + int,
// bool + int). Java-aligned: Java's SemanticAnalyzer catches this at
// compile time; Go catches it at eval time via panic recovery, mapped
// to SQLSTATE 42804 by the executor.
func TestArithmeticValue_TypeMismatch_Panics(t *testing.T) {
	t.Parallel()
	a := &FieldValue{Field: "a", Typ: TypeInt}
	b := &FieldValue{Field: "b", Typ: TypeInt}
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
			func() {
				defer func() {
					r := recover()
					if r == nil {
						t.Fatalf("type mismatch %v: expected panic", tc.row)
					}
					if _, ok := r.(*ScalarTypeMismatchError); !ok {
						t.Fatalf("type mismatch %v: expected *ScalarTypeMismatchError, got %T: %v", tc.row, r, r)
					}
				}()
				av := &ArithmeticValue{Op: OpAdd, Left: a, Right: b}
				mustEvaluate(av, tc.row)
			}()
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
			if got := mustEvaluate(bv, nil); got != tc.want {
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
	if got := mustEvaluate(NewBooleanValue(true), nil); got != true {
		t.Fatalf("NewBooleanValue(true): got %v", got)
	}
	if got := mustEvaluate(NewBooleanValue(false), nil); got != false {
		t.Fatalf("NewBooleanValue(false): got %v", got)
	}
	// Verify factory and direct-struct paths produce equal Evaluate
	// behaviour. Two separate factories should not diverge on the
	// happy path even if internal pointer identities differ.
	a := NewBooleanValue(true)
	b := &BooleanValue{Value: boolPtr(true)}
	if mustEvaluate(a, nil) != mustEvaluate(b, nil) {
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
			got := mustEvaluate(c, nil)
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
			if got := mustEvaluate(c, nil); got != nil {
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
