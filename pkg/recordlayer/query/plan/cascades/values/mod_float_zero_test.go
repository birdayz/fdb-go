package values

import (
	"errors"
	"math"
	"testing"
)

func TestScalarModFloatZero(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		left, right any
		lt, rt      Type
	}{
		{"double_zero", float64(7), float64(0), NullableDouble, NullableDouble},
		{"double_negative_zero", float64(7), math.Copysign(0, -1), NullableDouble, NullableDouble},
		{"long_double", int64(7), float64(0), NullableLong, NullableDouble},
		{"double_long", float64(7), int64(0), NullableDouble, NullableLong},
		{"float_zero", float64(7), float64(0), NullableFloat, NullableFloat},
		{"float_negative_zero", float64(7), math.Copysign(0, -1), NullableFloat, NullableFloat},
		{"float_long", float64(7), int64(0), NullableFloat, NullableLong},
		{"long_float", int64(7), float64(0), NullableLong, NullableFloat},
		{"zero_zero", float64(0), float64(0), NullableDouble, NullableDouble},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			args := []Value{
				&ConstantValue{Value: tc.left, Typ: tc.lt},
				&ConstantValue{Value: tc.right, Typ: tc.rt},
			}
			typ, ok := ScalarFunctionResultType("MOD", args)
			wantCode := TypeCodeFloat
			if tc.lt.Code() == TypeCodeDouble || tc.rt.Code() == TypeCodeDouble {
				wantCode = TypeCodeDouble
			}
			if !ok || typ.Code() != wantCode {
				t.Fatalf("MOD result type = %v, want %v", typ, wantCode)
			}
			v := NewScalarFunctionValue("MOD", typ, args...)
			got, err := v.Evaluate(nil)
			if err != nil {
				t.Fatalf("MOD(%v, %v): %v; want NaN without an error", tc.left, tc.right, err)
			}
			if f, ok := got.(float64); !ok || !math.IsNaN(f) {
				t.Fatalf("MOD(%v, %v) = %T(%v), want NaN", tc.left, tc.right, got, got)
			}
		})
	}
}

func TestScalarModSpecialValues(t *testing.T) {
	t.Parallel()
	negZero := math.Copysign(0, -1)
	for _, tc := range []struct {
		name        string
		left, right any
		typ         Type
		want        any
		divZero     bool
	}{
		{"integer_zero", int64(7), int64(0), NullableLong, nil, true},
		{"int_zero", int64(7), int64(0), NullableInt, nil, true},
		{"integer_finite", int64(-7), int64(3), NullableLong, int64(-1), false},
		{"integer_min", int64(math.MinInt64), int64(-1), NullableLong, int64(0), false},
		{"positive_finite", float64(7.5), float64(2), NullableDouble, float64(1.5), false},
		{"negative_dividend", float64(-7.5), float64(2), NullableDouble, float64(-1.5), false},
		{"negative_divisor", float64(7.5), float64(-2), NullableDouble, float64(1.5), false},
		{"negative_zero_dividend", negZero, float64(2), NullableDouble, negZero, false},
		{"negative_zero_remainder", float64(-4), float64(2), NullableDouble, negZero, false},
		{"infinite_divisor", float64(7), math.Inf(1), NullableDouble, float64(7), false},
		{"infinite_dividend", math.Inf(1), float64(2), NullableDouble, math.NaN(), false},
		{"nan_dividend", math.NaN(), float64(2), NullableDouble, math.NaN(), false},
		{"nan_divisor", float64(7), math.NaN(), NullableDouble, math.NaN(), false},
		{"null_dividend_zero", nil, float64(0), NullableDouble, nil, false},
		{"null_divisor", float64(7), nil, NullableDouble, nil, false},
		{"both_null", nil, nil, NullableDouble, nil, false},
		{"float_operand_rounding", int64(16777217), int64(2), NullableFloat, float64(0), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			v := NewScalarFunctionValue("MOD", tc.typ,
				&ConstantValue{Value: tc.left, Typ: tc.typ},
				&ConstantValue{Value: tc.right, Typ: tc.typ})
			got, err := v.Evaluate(nil)
			if tc.divZero {
				var zero *ArithmeticDivisionByZeroError
				if !errors.As(err, &zero) || got != nil {
					t.Fatalf("got (%v, %v), want nil and ArithmeticDivisionByZeroError", got, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if want, ok := tc.want.(float64); ok {
				f, ok := got.(float64)
				if !ok || (math.IsNaN(want) && !math.IsNaN(f)) ||
					(!math.IsNaN(want) && math.Float64bits(f) != math.Float64bits(want)) {
					t.Fatalf("got %T(%v), want %v (signed-zero bits significant)", got, got, want)
				}
			} else if got != tc.want {
				t.Fatalf("got %T(%v), want %T(%v)", got, got, tc.want, tc.want)
			}
		})
	}
}

func TestScalarModMixedFloatLong(t *testing.T) {
	t.Parallel()
	for _, reverse := range []bool{false, true} {
		args := []Value{
			&ConstantValue{Value: float64(16777216), Typ: NullableFloat},
			&ConstantValue{Value: int64(16777217), Typ: NullableLong},
		}
		if reverse {
			args[0], args[1] = args[1], args[0]
		}
		typ, ok := ScalarFunctionResultType("MOD", args)
		if !ok || typ.Code() != TypeCodeFloat {
			t.Fatalf("MOD(FLOAT, LONG) result = %v, want FLOAT", typ)
		}
		got, err := NewScalarFunctionValue("MOD", typ, args...).Evaluate(nil)
		if err != nil || got != float64(0) {
			t.Fatalf("reverse=%v: got (%v, %v), want 0 after rounding LONG to FLOAT", reverse, got, err)
		}
	}
}

func TestScalarModNativeIntegerCarriers(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		a, b, zero any
	}{
		{"int", int(7), int(3), int(0)},
		{"int8", int8(7), int8(3), int8(0)},
		{"int16", int16(7), int16(3), int16(0)},
		{"int32", int32(7), int32(3), int32(0)},
		{"int64", int64(7), int64(3), int64(0)},
		{"uint", uint(7), uint(3), uint(0)},
		{"uint64", uint64(7), uint64(3), uint64(0)},
		{"large_mixed", int64(9007199254740993), int32(2), int32(0)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			for _, b := range []any{tc.b, tc.zero} {
				args := []Value{&ConstantValue{Value: tc.a}, &ConstantValue{Value: b}}
				for _, v := range []Value{
					NewScalarFunctionValue("MOD", TypeUnknown, args...),
					&ArithmeticValue{Op: OpMod, Left: args[0], Right: args[1]},
				} {
					got, err := v.Evaluate(nil)
					if b == tc.zero {
						var zero *ArithmeticDivisionByZeroError
						if got != nil || !errors.As(err, &zero) {
							t.Errorf("%T: got (%v, %v), want integral division-by-zero error", v, got, err)
						}
					} else if err != nil || got != int64(1) {
						t.Errorf("%T: got (%T(%v), %v), want exact int64(1)", v, got, got, err)
					}
				}
			}
		})
	}
}
