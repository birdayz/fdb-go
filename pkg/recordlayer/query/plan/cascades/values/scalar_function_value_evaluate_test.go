package values

import (
	"errors"
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestScalarFunctionValue_Evaluate_GreatestLeastMismatchReturnsError pins the
// RFC-091 contract that the *production* eval path returns a typed
// *ScalarTypeMismatchError for GREATEST/LEAST with incompatible argument types,
// rather than leaking a panic.
//
// Before the fix, ScalarFunctionValue.Evaluate delegated unconditionally to
// evalScalarFunction, which panics on the mismatch — so the panic escaped this
// error-returning method. With the executor control-flow recovers removed (RFC-091
// A2), that residual panic surfaced only at the db/sql boundary as a generic
// internal error (or panicked outright for non-SQL callers) instead of flowing
// through translateExecError as the intended ErrCodeCannotConvertType.
func TestScalarFunctionValue_Evaluate_GreatestLeastMismatchReturnsError(t *testing.T) {
	t.Parallel()
	for _, fn := range []string{"GREATEST", "LEAST"} {
		t.Run(fn, func(t *testing.T) {
			t.Parallel()
			// fn(1, 'x') — int64 vs string, incompatible types.
			v := NewScalarFunctionValue(fn, TypeUnknown,
				&ConstantValue{Value: int64(1), Typ: TypeInt},
				&ConstantValue{Value: "x", Typ: TypeString},
			)

			got, err := v.Evaluate(nil)
			if err == nil {
				t.Fatalf("%s(1, 'x').Evaluate: want *ScalarTypeMismatchError, got nil err (value %v) — panic leaked or mismatch swallowed", fn, got)
			}
			var mismatch *ScalarTypeMismatchError
			if !errors.As(err, &mismatch) {
				t.Fatalf("%s(1, 'x').Evaluate: want *ScalarTypeMismatchError, got %T: %v", fn, err, err)
			}
			if got != nil {
				t.Errorf("%s(1, 'x').Evaluate: want nil value alongside the error, got %v", fn, got)
			}
		})
	}
}

// TestScalarFunctionValue_Evaluate_GreatestLeastHappyPath guards the refactor: the
// shared evalGreatestLeast still computes the right result and propagates NULL,
// via the error-returning production path.
func TestScalarFunctionValue_Evaluate_GreatestLeastHappyPath(t *testing.T) {
	t.Parallel()

	greatest := NewScalarFunctionValue("GREATEST", TypeInt,
		&ConstantValue{Value: int64(3), Typ: TypeInt},
		&ConstantValue{Value: int64(7), Typ: TypeInt},
		&ConstantValue{Value: int64(5), Typ: TypeInt},
	)
	if got, err := greatest.Evaluate(nil); err != nil || got != int64(7) {
		t.Fatalf("GREATEST(3,7,5) = (%v, %v), want (7, nil)", got, err)
	}

	least := NewScalarFunctionValue("LEAST", TypeInt,
		&ConstantValue{Value: int64(3), Typ: TypeInt},
		&ConstantValue{Value: int64(7), Typ: TypeInt},
		&ConstantValue{Value: int64(5), Typ: TypeInt},
	)
	if got, err := least.Evaluate(nil); err != nil || got != int64(3) {
		t.Fatalf("LEAST(3,7,5) = (%v, %v), want (3, nil)", got, err)
	}

	// NULL propagation: any NULL arg → NULL result, no error.
	withNull := NewScalarFunctionValue("GREATEST", TypeInt,
		&ConstantValue{Value: int64(3), Typ: TypeInt},
		&ConstantValue{Value: nil, Typ: TypeInt},
	)
	if got, err := withNull.Evaluate(nil); err != nil || got != nil {
		t.Fatalf("GREATEST(3, NULL) = (%v, %v), want (nil, nil)", got, err)
	}
}

func TestScalarFunctionValue_NumericCarrierMatchesResultType(t *testing.T) {
	t.Parallel()

	intValue := func(value int64) Value {
		return &ConstantValue{Value: value, Typ: NotNullInt}
	}
	doubleValue := func(value float64) Value {
		return &ConstantValue{Value: value, Typ: NotNullDouble}
	}
	tests := []struct {
		name         string
		args         []Value
		denominator  int64
		wantResult   float64
		wantQuotient float64
	}{
		{
			name:         "COALESCE",
			args:         []Value{intValue(3), doubleValue(1.5)},
			denominator:  2,
			wantResult:   3,
			wantQuotient: 1.5,
		},
		{
			name:         "GREATEST",
			args:         []Value{intValue(3), doubleValue(1.5)},
			denominator:  2,
			wantResult:   3,
			wantQuotient: 1.5,
		},
		{
			name:         "LEAST",
			args:         []Value{intValue(-5), doubleValue(-3.7)},
			denominator:  2,
			wantResult:   -5,
			wantQuotient: -2.5,
		},
		{
			name:         "FLOOR",
			args:         []Value{doubleValue(3.7)},
			denominator:  2,
			wantResult:   3,
			wantQuotient: 1.5,
		},
		{
			name:         "POWER",
			args:         []Value{intValue(2), intValue(3)},
			denominator:  3,
			wantResult:   8,
			wantQuotient: 8.0 / 3.0,
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			resultType, ok := ScalarFunctionResultType(test.name, test.args)
			require.True(t, ok)
			require.Equal(t, TypeCodeDouble, resultType.Code())

			function := NewScalarFunctionValue(test.name, resultType, test.args...)
			result, err := function.Evaluate(nil)
			require.NoError(t, err)
			require.IsType(t, float64(0), result)
			require.Equal(t, test.wantResult, result)

			division := &ArithmeticValue{
				Op:    OpDiv,
				Left:  function,
				Right: intValue(test.denominator),
			}
			quotient, err := division.Evaluate(nil)
			require.NoError(t, err)
			require.IsType(t, float64(0), quotient)
			require.InDelta(t, test.wantQuotient, quotient, 1e-12)

			folded := SimplifyValue(division)
			foldedQuotient, err := folded.Evaluate(nil)
			require.NoError(t, err)
			require.IsType(t, float64(0), foldedQuotient)
			require.InDelta(t, test.wantQuotient, foldedQuotient, 1e-12)
			require.False(t, math.IsNaN(foldedQuotient.(float64)))
		})
	}

	const floatWitness = int64(1<<62 | 1<<38 | 1)
	floatValue := &ConstantValue{Value: float64(0), Typ: NotNullFloat}
	floatArgs := []Value{
		&ConstantValue{Value: floatWitness, Typ: NotNullLong},
		floatValue,
	}
	resultType, ok := ScalarFunctionResultType("COALESCE", floatArgs)
	require.True(t, ok)
	require.Equal(t, TypeCodeFloat, resultType.Code())
	result, err := NewScalarFunctionValue("COALESCE", resultType, floatArgs...).Evaluate(nil)
	require.NoError(t, err)
	require.Equal(t, float64(float32(floatWitness)), result)

	// Nonlinear operators must promote INPUTS before evaluating, not merely
	// convert the result carrier. Java LONG→FLOAT rounds 16,777,217 to
	// 16,777,216 first, so modulo 2 is zero.
	modArgs := []Value{
		&ConstantValue{Value: int64(16_777_217), Typ: NotNullLong},
		&ConstantValue{Value: float64(2), Typ: NotNullFloat},
	}
	modType, ok := ScalarFunctionResultType("MOD", modArgs)
	require.True(t, ok)
	require.Equal(t, TypeCodeFloat, modType.Code())
	modResult, err := NewScalarFunctionValue("MOD", modType, modArgs...).Evaluate(nil)
	require.NoError(t, err)
	require.Equal(t, float64(0), modResult)
}
