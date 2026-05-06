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
		{"uint64 not handled", uint64(42), 0, false}, // unsigned types deliberately excluded
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
		{"uint64 not handled", uint64(42), 0, false, false},
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
