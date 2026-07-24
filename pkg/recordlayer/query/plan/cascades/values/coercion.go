package values

import "math"

// CompareFloat64 is a total order over float64 faithful to Java's
// java.lang.Double.compare (used by the Record Layer's
// Comparisons.compare for ordering predicates) and — at the ==0
// boundary — to Double.equals / Double.doubleToLongBits (used by
// Comparisons.compareEquals for `=`/`!=`). Those two are consistent:
// compare(a,b)==0 iff a.equals(b), so a single total order reproduces
// both the `=`/`<`/`>` predicate path and sort/merge/dedup ordering.
//
// Two edge values differ from Go's native float comparison:
//   - NaN sorts GREATER than every non-NaN value, and NaN compares
//     equal to NaN (native `<`/`>` are both false, and `==` is false).
//   - Negative zero sorts strictly BEFORE positive zero: -0.0 < 0.0
//     (native `==` treats them as equal).
//
// This is exactly the total order FDB tuple encoding imposes on an
// indexed FLOAT/DOUBLE column, so an in-memory comparator and an
// ordered index scan agree on both edge values.
func CompareFloat64(a, b float64) int {
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	// Reached only when a and b are IEEE-equal (including the
	// +0.0/-0.0 pair, where native `<`/`>` are both false) or when at
	// least one operand is NaN. Break the tie on the canonical bit
	// pattern exactly as Double.compare does via doubleToLongBits: all
	// NaNs collapse to one canonical value that sorts above every
	// finite/infinite value, and -0.0's sign bit places it below +0.0.
	ab := canonicalFloat64Bits(a)
	bb := canonicalFloat64Bits(b)
	switch {
	case ab < bb:
		return -1
	case ab > bb:
		return 1
	default:
		return 0
	}
}

// canonicalFloat64Bits returns the signed bit pattern Java's
// Double.doubleToLongBits produces: every NaN — regardless of payload —
// maps to the single canonical NaN (0x7ff8000000000000) so distinct
// NaN encodings compare equal, and that value (being positive and
// larger than +Inf's bits) sorts above every non-NaN. The sign bit
// makes -0.0's pattern negative and 0.0's zero, so -0.0 < 0.0.
func canonicalFloat64Bits(f float64) int64 {
	if math.IsNaN(f) {
		return int64(0x7ff8000000000000)
	}
	return int64(math.Float64bits(f))
}

// canonicalFloat32Bits returns the signed bit pattern Java's
// Float.floatToIntBits produces. Like canonicalFloat64Bits, it preserves the
// sign distinction between -0.0 and +0.0 while collapsing every NaN payload
// and sign encoding to Java's canonical quiet NaN.
func canonicalFloat32Bits(f float32) int32 {
	if math.IsNaN(float64(f)) {
		return int32(0x7fc00000)
	}
	return int32(math.Float32bits(f))
}

// ToInt64 reports whether v is an integral type; returns the int64
// promotion when so.
func ToInt64(v any) (int64, bool) {
	switch x := v.(type) {
	case int64:
		return x, true
	case int:
		return int64(x), true
	case int32:
		return int64(x), true
	case int16:
		return int64(x), true
	case int8:
		return int64(x), true
	}
	return 0, false
}

// ToFloat64 reports whether v is numeric (int-like or float) and
// returns its float64 promotion. isFloat distinguishes native-float
// inputs from integral ones promoted here — comparison-time promotion
// uses it to prefer the int path when both sides are integral.
func ToFloat64(v any) (f float64, isFloat, numeric bool) {
	switch x := v.(type) {
	case float64:
		return x, true, true
	case float32:
		return float64(x), true, true
	case int64:
		return float64(x), false, true
	case int:
		return float64(x), false, true
	case int32:
		return float64(x), false, true
	case int16:
		return float64(x), false, true
	case int8:
		return float64(x), false, true
	case uint64:
		// The FDB tuple layer decodes positive integers above math.MaxInt64
		// as uint64 (its only unsigned return) — a numeric row value like
		// any other.
		return float64(x), false, true
	case uint:
		return float64(x), false, true
	}
	return 0, false, false
}

// CompareExactInts compares two values EXACTLY when both are admitted
// integer forms — int/int8/int16/int32/int64 plus the uint/uint64 half the
// tuple layer uses for positive values above math.MaxInt64. ok=false when
// either side is not an integer. The order is by true numeric value — the
// order the FDB tuple integer encoding gives an indexed column — computed
// on (negative, magnitude) form so no pair round-trips through float64
// (promotion ties adjacent values above 2^53 and at the 2^63 boundary).
func CompareExactInts(a, b any) (int, bool) {
	an, am, aok := intParts(a)
	if !aok {
		return 0, false
	}
	bn, bm, bok := intParts(b)
	if !bok {
		return 0, false
	}
	if an != bn {
		if an {
			return -1, true
		}
		return 1, true
	}
	if am == bm {
		return 0, true
	}
	less := am < bm
	if an {
		// Both negative: larger magnitude is the smaller value.
		less = !less
	}
	if less {
		return -1, true
	}
	return 1, true
}

func intParts(v any) (neg bool, mag uint64, ok bool) {
	switch n := v.(type) {
	case int:
		return intPartsSigned(int64(n))
	case int8:
		return intPartsSigned(int64(n))
	case int16:
		return intPartsSigned(int64(n))
	case int32:
		return intPartsSigned(int64(n))
	case int64:
		return intPartsSigned(n)
	case uint:
		return false, uint64(n), true
	case uint64:
		return false, n, true
	default:
		return false, 0, false
	}
}

func intPartsSigned(n int64) (bool, uint64, bool) {
	if n < 0 {
		// Two's-complement negation via unsigned arithmetic is exact for
		// math.MinInt64 (where -n would overflow).
		return true, -uint64(n), true
	}
	return false, uint64(n), true
}

// LiteralValue wraps a Go-native literal in the matching Value
// subtype: nil → NullValue, bool → BooleanValue, otherwise a
// ConstantValue. Typ defaults to TypeUnknown; the simplifier does
// not depend on the type tag today — it inspects the wrapped Value
// subtype.
func LiteralValue(lit any) Value {
	if lit == nil {
		return &NullValue{Typ: TypeUnknown}
	}
	if b, ok := lit.(bool); ok {
		return NewBooleanValue(b)
	}
	return &ConstantValue{Value: lit, Typ: TypeUnknown}
}
