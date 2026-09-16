package values

import (
	"math"
	"testing"
)

// Floating scalar results must keep the sign observed by subsequent IEEE
// arithmetic rather than round-tripping through an integer carrier.
func TestScalarMathSignedZero(t *testing.T) {
	t.Parallel()
	negativeZero := math.Copysign(0, -1)
	if negativeZero != 0 || !math.Signbit(negativeZero) {
		t.Fatal("fixture must carry negative zero")
	}
	for _, tc := range []struct {
		name     string
		function string
		input    float64
		extra    []any
		want     float64
	}{
		{"floor negative zero", "FLOOR", negativeZero, nil, negativeZero},
		{"ceil negative zero", "CEIL", negativeZero, nil, negativeZero},
		{"ceiling negative fraction", "CEILING", -0.25, nil, negativeZero},
		{"ceil negative fraction", "CEIL", -0.25, nil, negativeZero},
		{"round negative zero", "ROUND", negativeZero, nil, negativeZero},
		{"round negative fraction", "ROUND", -0.25, nil, negativeZero},
		{"round positive precision", "ROUND", -0.001, []any{int64(2)}, negativeZero},
		{"round negative precision", "ROUND", -1, []any{int64(-1)}, negativeZero},
		{"round minimum precision", "ROUND", -1, []any{int64(math.MinInt64)}, negativeZero},
		{"power negative zero odd", "POWER", negativeZero, []any{int64(3)}, negativeZero},
		{"pow negative zero odd", "POW", negativeZero, []any{int64(3)}, negativeZero},
		{"power negative underflow", "POWER", -1e-30, []any{int64(13)}, negativeZero},
		{"floor positive zero", "FLOOR", 0, nil, 0},
		{"ceil positive zero", "CEIL", 0, nil, 0},
		{"round positive fraction", "ROUND", 0.25, nil, 0},
		{"power negative zero even", "POWER", negativeZero, []any{int64(2)}, 0},
		{"power positive underflow", "POWER", 1e-30, []any{int64(13)}, 0},
		{"floor negative fraction", "FLOOR", -0.25, nil, -1},
		{"ceil positive fraction", "CEIL", 0.25, nil, 1},
		{"round negative tie", "ROUND", -0.5, nil, -1},
		{"power finite", "POWER", -2, []any{int64(3)}, -8},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rawArgs := append([]any{tc.input}, tc.extra...)
			raw, err := evalScalarFunction(tc.function, rawArgs)
			if err != nil {
				t.Fatal(err)
			}
			f, ok := raw.(float64)
			if !ok || math.Float64bits(f) != math.Float64bits(tc.want) {
				t.Errorf("direct result = %T(%v), want float64(%v) (bitwise)", raw, raw, tc.want)
			}
			for _, typ := range []Type{NotNullFloat, NotNullDouble, UnknownType} {
				args := []Value{&ConstantValue{Value: tc.input, Typ: typ}}
				for _, extra := range tc.extra {
					args = append(args, &ConstantValue{Value: extra, Typ: NotNullLong})
				}
				resultType, ok := ScalarFunctionResultType(tc.function, args)
				if !ok {
					t.Fatal("function missing from scalar catalog")
				}
				function := NewScalarFunctionValue(tc.function, resultType, args...)
				folded := SimplifyValue(function)
				if _, ok := folded.(*ConstantValue); !ok {
					t.Fatalf("%s did not constant-fold: %T", tc.function, folded)
				}
				for _, value := range []Value{function, folded} {
					got, err := value.Evaluate(nil)
					if err != nil {
						t.Fatal(err)
					}
					f, ok := got.(float64)
					if !ok || math.Float64bits(f) != math.Float64bits(tc.want) {
						t.Errorf("%v %T result = %T(%v), want %v (bitwise)", typ, value, got, got, tc.want)
					}
				}
			}
		})
	}
}

func TestScalarMathFloatingSpecialValues(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"FLOOR", "CEIL", "CEILING", "ROUND", "POWER", "POW"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			for _, input := range []any{nil, math.NaN(), math.Inf(-1), math.Inf(1)} {
				args := []Value{&ConstantValue{Value: input, Typ: NullableDouble}}
				power := name == "POWER" || name == "POW"
				if power {
					args = append(args, &ConstantValue{Value: int64(3), Typ: NotNullLong})
				}
				value := NewScalarFunctionValue(name, NullableDouble, args...)
				for _, expr := range []Value{value, SimplifyValue(value)} {
					got, err := expr.Evaluate(nil)
					if err != nil {
						t.Fatal(err)
					}
					if input == nil || power {
						if got != nil {
							t.Errorf("%s(%v) = %v, want NULL", name, input, got)
						}
						continue
					}
					f, ok := got.(float64)
					want := input.(float64)
					if !ok || (math.IsNaN(want) && !math.IsNaN(f)) || (!math.IsNaN(want) && f != want) {
						t.Errorf("%s(%v) = %T(%v), want same floating special value", name, input, got, got)
					}
				}
			}
		})
	}
}

func TestScalarZeroIntegerConversions(t *testing.T) {
	t.Parallel()
	for _, zero := range []float64{0, math.Copysign(0, -1)} {
		arg := &ConstantValue{Value: zero, Typ: NotNullDouble}
		for _, target := range []Type{NotNullInt, NotNullLong} {
			got, err := NewCastValue(arg, target).Evaluate(nil)
			if err != nil || got != int64(0) {
				t.Errorf("CAST(%v AS %v) = (%v, %v), want integer zero", zero, target, got, err)
			}
		}
		// SIGN classifies into -1, +0 or +1; unlike rounding, it does not
		// preserve the input zero sign. Retain that separate scalar contract.
		got, err := NewScalarFunctionValue("SIGN", NotNullDouble, arg).Evaluate(nil)
		f, ok := got.(float64)
		if err != nil || !ok || math.Float64bits(f) != 0 {
			t.Errorf("SIGN(%v) = (%v, %v), want float64(+0)", zero, got, err)
		}
		if got, ok := scalarFnInt64Arg(zero); !ok || got != 0 {
			t.Errorf("integer argument conversion of %v = (%v, %v), want 0", zero, got, ok)
		}
	}
}

// Compare to the math operation itself, not another engine route that could
// lose the same sign. Whole floating results must not acquire integer carriers.
func FuzzScalarFloatingMath(f *testing.F) {
	for _, input := range []float64{math.Copysign(0, -1), 0, -0.25, -1e-30, 3.5, twoPow63, math.Inf(-1), math.NaN()} {
		for op := range uint8(4) {
			f.Add(math.Float64bits(input), op, int8(13))
		}
	}
	f.Fuzz(func(t *testing.T, bits uint64, op uint8, exponent int8) {
		t.Parallel()
		input := math.Float64frombits(bits)
		args := []Value{&ConstantValue{Value: input, Typ: NotNullDouble}}
		var name string
		var want float64
		switch op % 4 {
		case 0:
			name, want = "FLOOR", math.Floor(input)
		case 1:
			name, want = "CEIL", math.Ceil(input)
		case 2:
			name, want = "ROUND", math.Round(input)
		case 3:
			name, want = "POWER", math.Pow(input, float64(exponent))
			args = append(args, &ConstantValue{Value: int64(exponent), Typ: NotNullLong})
		}
		value := NewScalarFunctionValue(name, NotNullDouble, args...)
		folded := SimplifyValue(value)
		switch folded.(type) {
		case *ConstantValue, *NullValue:
		default:
			t.Fatalf("%s did not fold: %T", name, folded)
		}
		for _, expr := range []Value{value, folded} {
			got, err := expr.Evaluate(nil)
			if err != nil {
				t.Fatal(err)
			}
			if name == "POWER" && (math.IsNaN(want) || math.IsInf(want, 0)) {
				if got != nil {
					t.Fatalf("POWER domain result = %v, want NULL", got)
				}
				continue
			}
			result, ok := got.(float64)
			if !ok || (math.IsNaN(want) && !math.IsNaN(result)) || (!math.IsNaN(want) && math.Float64bits(result) != math.Float64bits(want)) {
				t.Fatalf("%s(%v), exponent %d: %T result = %T(%v), want float64(%v) (bitwise except NaN)", name, input, exponent, expr, got, got, want)
			}
		}
	})
}
