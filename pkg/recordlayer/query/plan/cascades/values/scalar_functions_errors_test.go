package values

import (
	"errors"
	"math"
	"testing"
)

// TestEvalScalarFunction_HappyAndNull pins the restored scalar-function
// family on the new (any, error) channel: correct results with a nil
// error, and a nil-error SQL-NULL result for any NULL argument. RFC-087
// Phase D.
func TestEvalScalarFunction_HappyAndNull(t *testing.T) {
	t.Parallel()
	cases := []struct {
		fn   string
		args []any
		want any
	}{
		{"UPPER", []any{"abc"}, "ABC"},
		{"LOWER", []any{"ABC"}, "abc"},
		{"CONCAT", []any{"a", "b", "c"}, "abc"},
		{"CONCAT", []any{"a", nil, "c"}, "ac"}, // NULL skipped, not poisoned
		{"SUBSTRING", []any{"hello", int64(2), int64(3)}, "ell"},
		{"SUBSTR", []any{"world", int64(1), int64(3)}, "wor"},
		{"TRIM", []any{"  hi  "}, "hi"},
		{"LEFT", []any{"hello", int64(3)}, "hel"},
		{"RIGHT", []any{"hello", int64(3)}, "llo"},
		{"POSITION", []any{"world", "hello world"}, int64(7)},
		{"REVERSE", []any{"abc"}, "cba"},
	}
	for _, tc := range cases {
		got, err := evalScalarFunction(tc.fn, tc.args)
		if err != nil {
			t.Fatalf("%s(%v): unexpected error %v", tc.fn, tc.args, err)
		}
		if got != tc.want {
			t.Fatalf("%s(%v): got %v, want %v", tc.fn, tc.args, got, tc.want)
		}
	}

	// NULL argument → SQL NULL (nil value, nil error) for every function
	// that propagates NULL.
	nullCases := []struct {
		fn   string
		args []any
	}{
		{"UPPER", []any{nil}},
		{"LOWER", []any{nil}},
		{"SUBSTRING", []any{nil, int64(1)}},
		{"TRIM", []any{nil}},
		{"LEFT", []any{nil, int64(2)}},
		{"RIGHT", []any{nil, int64(2)}},
		{"POSITION", []any{nil, "hello"}},
		{"REVERSE", []any{nil}},
	}
	for _, tc := range nullCases {
		got, err := evalScalarFunction(tc.fn, tc.args)
		if err != nil {
			t.Fatalf("%s(NULL): unexpected error %v", tc.fn, err)
		}
		if got != nil {
			t.Fatalf("%s(NULL): got %v, want nil (SQL NULL)", tc.fn, got)
		}
	}
}

// TestEvalScalarFunction_ErrorEdges pins the four data-dependent runtime
// error edges that RFC-087 Phase D moves off the decline-to-nil / panic
// paths onto the typed-error channel. Each is matched via errors.As so a
// wrapped error would still satisfy the assertion.
func TestEvalScalarFunction_ErrorEdges(t *testing.T) {
	t.Parallel()

	// ABS(MinInt64) overflows two's-complement negation → 22003.
	if v, err := evalScalarFunction("ABS", []any{int64(math.MinInt64)}); v != nil || err == nil {
		t.Fatalf("ABS(MinInt64): got (%v, %v), want (nil, ArithmeticOverflowError)", v, err)
	} else {
		var overflow *ArithmeticOverflowError
		if !errors.As(err, &overflow) {
			t.Fatalf("ABS(MinInt64): got %T, want *ArithmeticOverflowError", err)
		}
	}

	// MOD by zero (int and float paths) → 22012.
	for _, args := range [][]any{
		{int64(5), int64(0)},
		{float64(5), float64(0)},
		{int64(5), float64(0)}, // mixed → float path
	} {
		v, err := evalScalarFunction("MOD", args)
		if v != nil || err == nil {
			t.Fatalf("MOD(%v): got (%v, %v), want (nil, ArithmeticDivisionByZeroError)", args, v, err)
		}
		var divZero *ArithmeticDivisionByZeroError
		if !errors.As(err, &divZero) {
			t.Fatalf("MOD(%v): got %T, want *ArithmeticDivisionByZeroError", args, err)
		}
	}

	// SQRT of a negative number → 22023 (Go-only divergence from the old
	// embedded NULL behaviour, per RFC-087 step 3).
	for _, args := range [][]any{
		{float64(-1)},
		{int64(-4)},
	} {
		v, err := evalScalarFunction("SQRT", args)
		if v != nil || err == nil {
			t.Fatalf("SQRT(%v): got (%v, %v), want (nil, InvalidArgumentError)", args, v, err)
		}
		var invalidArg *InvalidArgumentError
		if !errors.As(err, &invalidArg) {
			t.Fatalf("SQRT(%v): got %T, want *InvalidArgumentError", args, err)
		}
	}

	// GREATEST / LEAST with incompatible argument types → 22000 (this is
	// the panic Phase A deferred; Phase D converts it to a return).
	for _, fn := range []string{"GREATEST", "LEAST"} {
		for _, args := range [][]any{
			{true, int64(1)},
			{float64(1.5), "a"},
			{int64(0), false},
		} {
			v, err := evalScalarFunction(fn, args)
			if v != nil || err == nil {
				t.Fatalf("%s(%v): got (%v, %v), want (nil, ScalarTypeMismatchError)", fn, args, v, err)
			}
			var mismatch *ScalarTypeMismatchError
			if !errors.As(err, &mismatch) {
				t.Fatalf("%s(%v): got %T, want *ScalarTypeMismatchError", fn, args, err)
			}
		}
	}
}

// TestScalarFunctionValue_PropagatesError pins that the error channel
// reaches a query through ScalarFunctionValue.Evaluate, not just the
// bare evalScalarFunction helper.
func TestScalarFunctionValue_PropagatesError(t *testing.T) {
	t.Parallel()
	v := NewScalarFunctionValue("SQRT", TypeFloat,
		&ConstantValue{Value: float64(-9), Typ: TypeFloat})
	got, err := v.Evaluate(nil)
	if got != nil || err == nil {
		t.Fatalf("SQRT(-9).Evaluate: got (%v, %v), want (nil, InvalidArgumentError)", got, err)
	}
	var invalidArg *InvalidArgumentError
	if !errors.As(err, &invalidArg) {
		t.Fatalf("SQRT(-9).Evaluate: got %T, want *InvalidArgumentError", err)
	}
}

// TestScalarInt64Boundary_NoWrap pins the 2^63 int64-conversion boundary
// (RFC-087). math.MaxInt64 (2^63-1) has no exact float64
// representation and rounds UP to 2^63, so the old `f <= math.MaxInt64` guards
// admitted 2^63 and int64(2^63) wrapped to math.MinInt64. The fix
// (float64FitsInt64, exclusive upper bound at 2^63) keeps such values as
// float64 instead of silently wrapping. Without the fix POWER(2,63) returns a
// negative int64 and these assertions fail.
func TestScalarInt64Boundary_NoWrap(t *testing.T) {
	t.Parallel()
	const twoPow63 = 9223372036854775808.0 // 2^63, smallest float64 > math.MaxInt64

	// POWER(2,63) is whole-valued but == 2^63 → must stay float64, not wrap.
	got, err := evalScalarFunction("POWER", []any{float64(2), float64(63)})
	if err != nil {
		t.Fatalf("POWER(2,63): unexpected error %v", err)
	}
	f, ok := got.(float64)
	if !ok {
		t.Fatalf("POWER(2,63) must return float64 (int64 would have wrapped), got %T = %v", got, got)
	}
	if f != twoPow63 {
		t.Fatalf("POWER(2,63) = %v, want %v", f, twoPow63)
	}

	// POWER(2,62) = 2^62 fits int64 → returns int64 (still folds when safe).
	got62, err := evalScalarFunction("POWER", []any{float64(2), float64(62)})
	if err != nil {
		t.Fatalf("POWER(2,62): %v", err)
	}
	if got62 != int64(1)<<62 {
		t.Fatalf("POWER(2,62) = %v (%T), want int64 %d", got62, got62, int64(1)<<62)
	}

	// FLOOR of a value at 2^63 stays float64 (no wrap).
	gotFloor, err := evalScalarFunction("FLOOR", []any{twoPow63})
	if err != nil {
		t.Fatalf("FLOOR(2^63): %v", err)
	}
	if _, ok := gotFloor.(float64); !ok {
		t.Fatalf("FLOOR(2^63) must stay float64, got %T = %v", gotFloor, gotFloor)
	}

	// scalarFnInt64Arg rejects 2^63 but accepts MinInt64 (-2^63 is exact).
	if _, ok := scalarFnInt64Arg(twoPow63); ok {
		t.Fatal("scalarFnInt64Arg(2^63) must reject — int64(2^63) overflows")
	}
	if iv, ok := scalarFnInt64Arg(float64(math.MinInt64)); !ok || iv != math.MinInt64 {
		t.Fatalf("scalarFnInt64Arg(MinInt64) = (%d,%v), want (%d,true)", iv, ok, int64(math.MinInt64))
	}

	// float64FitsInt64 boundary table.
	if float64FitsInt64(twoPow63) {
		t.Fatal("float64FitsInt64(2^63) must be false")
	}
	if !float64FitsInt64(float64(math.MinInt64)) {
		t.Fatal("float64FitsInt64(MinInt64) must be true")
	}
	if !float64FitsInt64(twoPow63 - 2048) { // representable, fits
		t.Fatal("float64FitsInt64(2^63-2048) must be true")
	}
}
