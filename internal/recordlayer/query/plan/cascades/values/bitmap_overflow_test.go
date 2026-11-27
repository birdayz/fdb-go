package values

import (
	"errors"
	"math"
	"testing"
)

// bitmapValue is the ArithmeticValue the SQL walker builds for a bitmap
// function over a constant of type argType, with the INT entry size.
func bitmapValue(t *testing.T, op ArithmeticOp, arg int64, argType Type, entrySize int64) *ArithmeticValue {
	t.Helper()
	v, err := NewArithmeticValue(op, &ConstantValue{Value: arg, Typ: argType}, &ConstantValue{Value: entrySize, Typ: NullableInt})
	if err != nil {
		t.Fatalf("NewArithmeticValue(%s): %v", op.LogicalOperatorName(), err)
	}
	return v
}

// TestBitmapFunctions_OverflowIsChecked pins the checked arithmetic in the
// bitmap bucketing functions, on both of Java's lanes.
//
// Java composes them from Math.multiplyExact / Math.subtractExact
// (ArithmeticValue.java:515-522), in int over the II lane and in long over the
// LI lane, so an overflow raises rather than producing a number. Unchecked, the
// minimum of either width with the default entry size 10000 wraps: floorDiv(min,
// 10000) * 10000 falls below the minimum and wraps to a bogus POSITIVE bucket
// offset, a wrong answer rather than a failure. The intermediate product is what
// overflows in BOTH functions, so BITMAP_BIT_POSITION is exercised on the same
// input even though its final subtraction would be in range.
func TestBitmapFunctions_OverflowIsChecked(t *testing.T) {
	t.Parallel()

	const entrySize = int64(10000)
	for _, lane := range []struct {
		name string
		min  int64
		typ  Type
	}{
		{"LI (a BIGINT argument)", math.MinInt64, NullableLong},
		{"II (an INTEGER argument)", math.MinInt32, NullableInt},
	} {
		// floorDiv(min, 10000) * 10000 < min: the quotient rounds toward
		// negative infinity, so multiplying back overshoots the minimum.
		if prod := floorDivInt64(lane.min, entrySize) * entrySize; lane.typ == NullableLong {
			if _, ok := mulInt64Checked(floorDivInt64(lane.min, entrySize), entrySize); ok {
				t.Fatalf("%s: test premise broken, the product no longer overflows", lane.name)
			}
		} else if prod >= math.MinInt32 {
			t.Fatalf("%s: test premise broken, the product %d fits in int", lane.name, prod)
		}
		for _, op := range []ArithmeticOp{OpBitmapBucketOffset, OpBitmapBitPosition} {
			v := bitmapValue(t, op, lane.min, lane.typ, entrySize)
			got, err := v.Evaluate(nil)
			var overflow *ArithmeticOverflowError
			if !errors.As(err, &overflow) {
				t.Errorf("%s %s(min, %d) = %v, %v; want *ArithmeticOverflowError: Java raises from "+
					"multiplyExact, so the wrapped value must never be produced", lane.name, op.LogicalOperatorName(), entrySize, got, err)
			}
		}
	}

	// In-range inputs must still compute, and compute correctly, on both lanes:
	// a guard that rejected everything would satisfy the assertions above.
	for _, typ := range []Type{NullableLong, NullableInt} {
		for _, tc := range []struct {
			op        ArithmeticOp
			arg, want int64
		}{
			{OpBitmapBucketOffset, 45678, 40000},
			{OpBitmapBitPosition, 45678, 5678},
			// Negative inputs floor toward negative infinity, as Java's floorDiv
			// does: -1 lands in bucket -10000 at position 9999.
			{OpBitmapBucketOffset, -1, -10000},
			{OpBitmapBitPosition, -1, 9999},
			{OpBitmapBucketOffset, -3, -10000},
			{OpBitmapBitPosition, -3, 9997},
			{OpBitmapBucketOffset, 0, 0},
			{OpBitmapBitPosition, 0, 0},
			{OpBitmapBucketOffset, 12345, 10000},
			{OpBitmapBitPosition, 12345, 2345},
		} {
			got, err := bitmapValue(t, tc.op, tc.arg, typ, entrySize).Evaluate(nil)
			if err != nil || got != any(tc.want) {
				t.Errorf("%s(%d %v, %d) = %v, %v; want %d", tc.op.LogicalOperatorName(), tc.arg, typ.Code(), entrySize, got, err, tc.want)
			}
		}
	}
}
