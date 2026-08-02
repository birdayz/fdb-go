package values

import (
	"errors"
	"math"
	"testing"
)

// TestBitmapScalarFunctions_OverflowIsChecked pins the checked arithmetic in the
// bitmap bucketing functions.
//
// Java composes them from Math.multiplyExact / Math.subtractExact
// (ArithmeticValue.java:513-520), so an overflow raises rather than producing a
// number. Go multiplied and subtracted unchecked: for the minimum BIGINT with
// the default entry size 10000, floorDiv(MinInt64, 10000) * 10000 falls BELOW
// MinInt64 and wraps to a bogus POSITIVE bucket offset — a wrong answer, not a
// failure, which is the worst outcome available.
//
// The intermediate product is what overflows in BOTH functions, so
// BITMAP_BIT_POSITION is exercised on the same input even though its final
// subtraction would be in range.
func TestBitmapScalarFunctions_OverflowIsChecked(t *testing.T) {
	t.Parallel()

	const entrySize = int64(10000)

	// floorDiv(MinInt64, 10000) * 10000 < MinInt64: the quotient rounds toward
	// negative infinity, so multiplying back overshoots the minimum.
	quotient := floorDivInt64(math.MinInt64, entrySize)
	if _, ok := mulInt64Checked(quotient, entrySize); ok {
		t.Fatalf("test premise broken: floorDiv(MinInt64, %d) * %d = %d*%d no longer "+
			"overflows, so this input cannot exercise the checked path",
			entrySize, entrySize, quotient, entrySize)
	}

	for _, fn := range []string{"BITMAP_BUCKET_OFFSET", "BITMAP_BIT_POSITION"} {
		v, err := evalScalarFunction(fn, []any{int64(math.MinInt64), entrySize})
		if err == nil {
			t.Errorf("%s(MinInt64, %d) returned %v with no error; Java raises "+
				"ArithmeticException from multiplyExact, so the wrapped value must "+
				"never be produced", fn, entrySize, v)
			continue
		}
		var overflow *ArithmeticOverflowError
		if !errors.As(err, &overflow) {
			t.Errorf("%s(MinInt64, %d): want *ArithmeticOverflowError, got %T: %v",
				fn, entrySize, err, err)
		}
	}

	// In-range inputs must still compute, and compute correctly — a guard that
	// rejected everything would satisfy the assertions above.
	for _, tc := range []struct {
		fn         string
		arg, want  int64
		wantErrNot bool
	}{
		{fn: "BITMAP_BUCKET_OFFSET", arg: 45678, want: 40000},
		{fn: "BITMAP_BIT_POSITION", arg: 45678, want: 5678},
		// Negative inputs floor toward negative infinity, as Java's floorDiv
		// does: -1 lands in bucket -10000 at position 9999.
		{fn: "BITMAP_BUCKET_OFFSET", arg: -1, want: -10000},
		{fn: "BITMAP_BIT_POSITION", arg: -1, want: 9999},
	} {
		got, err := evalScalarFunction(tc.fn, []any{tc.arg, entrySize})
		if err != nil {
			t.Errorf("%s(%d, %d): unexpected error %v", tc.fn, tc.arg, entrySize, err)
			continue
		}
		if got != any(tc.want) {
			t.Errorf("%s(%d, %d) = %v, want %d", tc.fn, tc.arg, entrySize, got, tc.want)
		}
	}
}
