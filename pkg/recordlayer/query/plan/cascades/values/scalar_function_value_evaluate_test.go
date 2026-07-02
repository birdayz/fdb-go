package values

import (
	"errors"
	"testing"
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
