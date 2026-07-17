package values

import (
	"math"
	"testing"
)

// TestToInt64 pins the integral-type promotion: every native Go signed
// integer type widens to int64; non-integral types (float, string,
// bool) return (0, false) — the false signal is what callers gate on
// to fall through to ToFloat64 / type-mismatch error.
func TestToInt64(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		v    any
		want int64
		ok   bool
	}{
		{"int64", int64(42), 42, true},
		{"int64 negative", int64(-7), -7, true},
		{"int", int(42), 42, true},
		{"int32", int32(42), 42, true},
		{"int16", int16(42), 42, true},
		{"int8", int8(42), 42, true},
		{"int8 max", int8(math.MaxInt8), math.MaxInt8, true},
		// Non-integral: returns (0, false).
		{"float64", float64(1.0), 0, false},
		{"float32", float32(1.0), 0, false},
		{"string", "42", 0, false},
		{"bool", true, 0, false},
		{"nil", nil, 0, false},
		// Unsigned stays excluded HERE: uint64 above math.MaxInt64 is not
		// losslessly an int64. Exact integer comparison across the signed/
		// unsigned boundary goes through CompareExactInts instead.
		{"uint64 not handled", uint64(42), 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := ToInt64(tc.v)
			if ok != tc.ok {
				t.Fatalf("ToInt64(%v): ok=%v, want %v", tc.v, ok, tc.ok)
			}
			if got != tc.want {
				t.Errorf("ToInt64(%v) = %d, want %d", tc.v, got, tc.want)
			}
		})
	}
}

// TestToFloat64 pins the numeric promotion: float and integral types
// widen to float64; isFloat distinguishes native-float inputs (so
// comparison-time promotion can prefer the int path when both sides
// are integral); non-numeric types return (0, false, false).
func TestToFloat64(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		v           any
		wantF       float64
		wantIsFloat bool
		wantNumeric bool
	}{
		// Native floats: isFloat=true, numeric=true.
		{"float64", float64(3.14), 3.14, true, true},
		{"float32", float32(2.5), 2.5, true, true},
		{"float64 negative", float64(-1.5), -1.5, true, true},
		// Integers promote to float; isFloat=false marks the promotion.
		{"int64", int64(42), 42, false, true},
		{"int", int(42), 42, false, true},
		{"int32", int32(42), 42, false, true},
		{"int16", int16(42), 42, false, true},
		{"int8", int8(42), 42, false, true},
		// Non-numeric: all-zero return.
		{"string", "1.5", 0, false, false},
		{"bool", true, 0, false, false},
		{"nil", nil, 0, false, false},
		// Unsigned IS numeric: the tuple layer decodes positive integers
		// above math.MaxInt64 as uint64, so predicates/sorts meet uint64 on
		// valid rows. The old "deliberately excluded" pin enshrined the gap.
		{"uint64", uint64(42), 42, false, true},
		{"uint", uint(7), 7, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f, isFloat, numeric := ToFloat64(tc.v)
			if numeric != tc.wantNumeric {
				t.Fatalf("ToFloat64(%v): numeric=%v, want %v", tc.v, numeric, tc.wantNumeric)
			}
			if isFloat != tc.wantIsFloat {
				t.Errorf("ToFloat64(%v): isFloat=%v, want %v", tc.v, isFloat, tc.wantIsFloat)
			}
			if f != tc.wantF {
				t.Errorf("ToFloat64(%v): f=%v, want %v", tc.v, f, tc.wantF)
			}
		})
	}
}

// TestLiteralValue pins the Go-native-to-Value wrapping: nil → NullValue,
// bool → BooleanValue (via NewBooleanValue), else ConstantValue with
// TypeUnknown.
func TestLiteralValue(t *testing.T) {
	t.Parallel()
	// nil wraps to NullValue.
	if v, ok := LiteralValue(nil).(*NullValue); !ok {
		t.Errorf("LiteralValue(nil): got %T, want *NullValue", v)
	}
	// bool wraps to BooleanValue.
	if v, ok := LiteralValue(true).(*BooleanValue); !ok {
		t.Errorf("LiteralValue(true): got %T, want *BooleanValue", v)
	}
	if v, ok := LiteralValue(false).(*BooleanValue); !ok {
		t.Errorf("LiteralValue(false): got %T, want *BooleanValue", v)
	}
	// int / string / float64 wrap to ConstantValue.
	for _, lit := range []any{int64(42), "hello", float64(3.14), []byte{1, 2}} {
		v := LiteralValue(lit)
		cv, ok := v.(*ConstantValue)
		if !ok {
			t.Errorf("LiteralValue(%v): got %T, want *ConstantValue", lit, v)
			continue
		}
		// ConstantValue must preserve the underlying value.
		if !ifaceEq(cv.Value, lit) {
			t.Errorf("LiteralValue(%v): ConstantValue.Value=%v, want %v", lit, cv.Value, lit)
		}
	}
}

// ifaceEq compares two `any` values for equality, handling []byte
// specially since `[]byte` is not comparable via `==`.
func ifaceEq(a, b any) bool {
	if ab, ok := a.([]byte); ok {
		bb, ok := b.([]byte)
		if !ok || len(ab) != len(bb) {
			return false
		}
		for i := range ab {
			if ab[i] != bb[i] {
				return false
			}
		}
		return true
	}
	return a == b
}

// TestCompareFloat64 pins the single Java-Double.compare-faithful total order
// both the predicate comparator (cmpAny) and the sort/merge comparator
// (compareValues) delegate to: -0.0 sorts strictly before 0.0, NaN is the
// greatest value and NaN==NaN, and finite values keep their natural order.
func TestCompareFloat64(t *testing.T) {
	t.Parallel()
	negZero := math.Copysign(0, -1)
	nan := math.NaN()
	cases := []struct {
		name string
		a, b float64
		want int // sign
	}{
		{"neg_zero_before_pos_zero", negZero, 0.0, -1},
		{"pos_zero_after_neg_zero", 0.0, negZero, 1},
		{"neg_zero_eq_neg_zero", negZero, negZero, 0},
		{"pos_zero_eq_pos_zero", 0.0, 0.0, 0},
		{"nan_eq_nan", nan, nan, 0},
		{"nan_gt_finite", nan, 5.0, 1},
		{"finite_lt_nan", 5.0, nan, -1},
		{"nan_gt_neg_finite", nan, -5.0, 1},
		{"nan_gt_pos_inf", nan, math.Inf(1), 1},
		{"nan_gt_neg_inf", nan, math.Inf(-1), 1},
		{"pos_inf_lt_nan", math.Inf(1), nan, -1},
		{"finite_order", 2.5, 10.5, -1},
		{"finite_reverse", 10.5, 2.5, 1},
		{"finite_equal", 3.25, 3.25, 0},
		{"neg_lt_pos", -1.0, 1.0, -1},
		{"pos_inf_gt_finite", math.Inf(1), 1e300, 1},
		{"neg_inf_lt_finite", math.Inf(-1), -1e300, -1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := CompareFloat64(c.a, c.b)
			if (c.want < 0 && got >= 0) || (c.want > 0 && got <= 0) || (c.want == 0 && got != 0) {
				t.Errorf("CompareFloat64(%v, %v) = %d, want sign %d", c.a, c.b, got, c.want)
			}
		})
	}
}
