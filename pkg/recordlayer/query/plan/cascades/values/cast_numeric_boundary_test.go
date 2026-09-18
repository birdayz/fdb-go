package values

import (
	"errors"
	"fmt"
	"math"
	"math/big"
	"reflect"
	"testing"
)

// numericCastOracle uses exact rationals, not the production bit-shift algorithm
// or floating addition. Saturation precedes DOUBLE-to-INT modulo narrowing.
func numericCastOracle(v float64, source, target TypeCode) (int64, bool) {
	width := uint(64)
	if source == TypeCodeFloat {
		v = float64(float32(v))
		width = 32
	}
	r := new(big.Rat).SetFloat64(v)
	if r == nil {
		return 0, false
	}
	r.Add(r, big.NewRat(1, 2))
	q, rem := new(big.Int), new(big.Int)
	q.QuoRem(r.Num(), r.Denom(), rem)
	if rem.Sign() < 0 {
		q.Sub(q, big.NewInt(1))
	}
	half := new(big.Int).Lsh(big.NewInt(1), width-1)
	min := new(big.Int).Neg(half)
	max := new(big.Int).Sub(half, big.NewInt(1))
	if q.Cmp(min) < 0 {
		q.Set(min)
	}
	if q.Cmp(max) > 0 {
		q.Set(max)
	}
	if source != TypeCodeFloat && target == TypeCodeInt {
		modulus := new(big.Int).Lsh(big.NewInt(1), 32)
		q.Mod(q, modulus)
		if q.Bit(31) != 0 {
			q.Sub(q, modulus)
		}
	}
	return q.Int64(), true
}

func TestNumericCastBoundaryAnswers(t *testing.T) {
	t.Parallel()
	cases := []struct {
		v                       float64
		long, integer, floating int64
	}{
		{math.Copysign(0, -1), 0, 0, 0},
		{0, 0, 0, 0},
		{0.49999999999999994, 0, 0, 1},
		{0.5, 1, 1, 1},
		{-0.5, 0, 0, 0},
		{-0.5000000000000001, -1, -1, 0},
		{2147483647.5, 2147483648, -2147483648, 2147483647},
		{-2147483648.5, -2147483648, -2147483648, -2147483648},
		{-2147483648.6, -2147483649, 2147483647, -2147483648},
		{4294967296, 4294967296, 0, 2147483647},
		{4503599627370497, 4503599627370497, 1, 2147483647},
		{9223372036854774784, 9223372036854774784, -1024, 2147483647},
		{0x1p63, 9223372036854775807, -1, 2147483647},
		{-0x1p63, -9223372036854775808, 0, -2147483648},
		{math.Nextafter(-0x1p63, math.Inf(-1)), -9223372036854775808, 0, -2147483648},
		{1e20, 9223372036854775807, -1, 2147483647},
		{-1e20, -9223372036854775808, 0, -2147483648},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%016x", math.Float64bits(tc.v)), func(t *testing.T) {
			t.Parallel()
			for _, source := range []Type{NullableDouble, NullableFloat} {
				for _, target := range []Type{NullableLong, NullableInt} {
					want := tc.long
					if target.Code() == TypeCodeInt {
						want = tc.integer
					}
					if source.Code() == TypeCodeFloat {
						want = tc.floating
					}
					oracle, ok := numericCastOracle(tc.v, source.Code(), target.Code())
					if !ok || oracle != want {
						t.Fatalf("oracle %v→%v: %d/%v, want explicit %d", source, target, oracle, ok, want)
					}
					got, err := NewCastValue(&ConstantValue{Value: tc.v, Typ: source}, target).Evaluate(nil)
					if err != nil || got != want {
						t.Errorf("%v→%v: %T(%v), %v; want int64(%d)", source, target, got, got, err, want)
					}
				}
			}
		})
	}
}

func TestNumericCastSourceWidthAndErrors(t *testing.T) {
	t.Parallel()
	for _, source := range []Type{NullableDouble, NullableFloat} {
		for _, target := range []Type{NullableLong, NullableInt} {
			// Native binary32 is not an operator selector: a declared DOUBLE source
			// widens it, whereas FLOAT/float64 must first quantize the carrier.
			for _, v := range []any{float32(1e20), float32(0.5), float64(1e20), float64(0.49999999999999994)} {
				d, ok := v.(float64)
				if !ok {
					d = float64(v.(float32))
				}
				want, _ := numericCastOracle(d, source.Code(), target.Code())
				got, err := CastEvaluated(v, source, target)
				if err != nil || got != want {
					t.Errorf("%T %v→%v: %v/%v, want %d", v, source, target, got, err, want)
				}
			}
			for _, v := range []any{math.NaN(), math.Inf(1), math.Inf(-1), float32(math.Inf(1)), float32(math.NaN())} {
				got, err := CastEvaluated(v, source, target)
				var invalid *InvalidCastError
				want := "Cannot cast NaN or Infinite to " + target.Code().String()
				if got != nil || !errors.As(err, &invalid) || invalid.Message != want {
					t.Errorf("%v→%v: %v/%v, want %q", source, target, got, err, want)
				}
			}
			got, err := CastEvaluated(nil, source, target)
			if err != nil || got != nil {
				t.Errorf("NULL: %v/%v", got, err)
			}
		}
	}
	for _, v := range []int64{2147483648, -2147483649, math.MaxInt64, math.MinInt64} {
		got, err := CastEvaluated(v, NullableLong, NullableInt)
		var invalid *InvalidCastError
		if got != nil || !errors.As(err, &invalid) {
			t.Errorf("LONG→INT %d: %v/%v, want range error", v, got, err)
		}
	}
	for _, source := range []Type{NullableDouble, NullableFloat} {
		for _, target := range []Type{NullableLong, NullableInt} {
			input := []any{float64(1e20), nil, float64(-1e20)}
			want := []any{int64(math.MaxInt64), nil, int64(math.MinInt64)}
			if target.Code() == TypeCodeInt {
				want = []any{int64(-1), nil, int64(0)}
			}
			if source.Code() == TypeCodeFloat {
				want = []any{int64(math.MaxInt32), nil, int64(math.MinInt32)}
			}
			got, err := CastEvaluated(input, NewArrayType(true, source), NewArrayType(true, target))
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Errorf("ARRAY %v→%v: %v/%v, want %v", source, target, got, err, want)
			}
		}
	}
}

func FuzzNumericCastRationalOracle(f *testing.F) {
	for _, v := range []float64{0, math.Copysign(0, -1), 0.49999999999999994, 0.5, -0.5, -0.5000000000000001, 2147483647.5, 4503599627370497, 0x1p63, -0x1p63, 1e20, math.MaxFloat64, math.NaN(), math.Inf(1)} {
		f.Add(math.Float64bits(v))
	}
	f.Fuzz(func(t *testing.T, bits uint64) {
		t.Parallel()
		v := math.Float64frombits(bits)
		for _, source := range []Type{NullableDouble, NullableFloat} {
			for _, target := range []Type{NullableLong, NullableInt} {
				want, finite := numericCastOracle(v, source.Code(), target.Code())
				got, err := CastEvaluated(v, source, target)
				if finite {
					if err != nil || got != want {
						t.Fatalf("%016x %v→%v: %v/%v, want %d", bits, source, target, got, err, want)
					}
				} else {
					var invalid *InvalidCastError
					if got != nil || !errors.As(err, &invalid) {
						t.Fatalf("nonfinite %016x %v→%v: %v/%v", bits, source, target, got, err)
					}
				}
			}
		}
	})
}
