package values

import (
	"errors"
	"math"
	"testing"
)

// evalSF drops evalScalarFunction's error return so the many happy-path
// value assertions in scalar_functions_extra*_test.go stay terse. Those
// rows all evaluate to (value, nil) or a genuine (nil, nil) SQL-NULL
// decline; the data-dependent error edges are pinned explicitly in
// TestEvalScalarFunction_ErrorEdges below (which calls evalScalarFunction
// directly and inspects the error via errors.As).
func evalSF(name string, args []any) any {
	v, _ := evalScalarFunction(name, args)
	return v
}

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
