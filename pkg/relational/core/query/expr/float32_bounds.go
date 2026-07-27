package expr

import "math"

// This file provides the float32 analogue of the int64 ceil/floor
// rewriting narrowFloatConstAgainstInt already does for the
// double/float-constant-vs-INTEGER-column direction. A FLOAT (32-bit)
// indexed column packs its index entries as FDB tuple type-code 0x20
// (single); a float64/int64 comparand that doesn't happen to be
// EXACTLY representable as float32 must not be packed verbatim — the
// SARG range has to be rewritten to the tightest predicate float32
// arithmetic can express, exactly as the int case rewrites
// `x > 1.5` to `x >= 2` for an INTEGER column.
//
// float32SortKey/float32FromSortKey implement the standard
// "sortable float" bit transform (used by radix sorts of IEEE-754
// values): flip every bit of a negative value, set only the sign bit
// of a positive value. The result is a uint32 whose ordinary integer
// order matches the float32's real-number order for every non-NaN
// value, including ±0 and ±Inf. That lets float32NextUp/NextDown find
// the adjacent representable float32 with a plain +1/-1 on the key,
// without the sign-dependent bit-pattern traps a naive
// Float32bits(x)±1 falls into (raw IEEE-754 bit patterns order
// backwards for negative numbers).
func float32SortKey(bits uint32) uint32 {
	if bits&0x8000_0000 != 0 {
		return ^bits
	}
	return bits | 0x8000_0000
}

func float32FromSortKey(key uint32) uint32 {
	if key&0x8000_0000 != 0 {
		return key &^ 0x8000_0000
	}
	return ^key
}

// float32NextUp returns the smallest float32 strictly greater than x.
// x must be finite or -Inf (the caller never asks for the successor of
// +Inf — see float32Ceil/float32Floor). ±0 are canonicalized to +0 first:
// IEEE-754 -0 and +0 are the SAME real number, so "the value strictly
// above zero" must be identical regardless of which zero's bit pattern
// arrived here — without this, the raw sort-key transform (which treats
// -0 and +0 as adjacent-but-distinct code points) would answer +0 for
// NextUp(-0) instead of the smallest positive subnormal.
func float32NextUp(x float32) float32 {
	if x == 0 {
		x = 0
	}
	key := float32SortKey(math.Float32bits(x))
	return math.Float32frombits(float32FromSortKey(key + 1))
}

// float32NextDown returns the largest float32 strictly less than x.
// Mirrors float32NextUp's ±0 canonicalization (to -0, so both zeros'
// predecessor is the smallest NEGATIVE subnormal).
func float32NextDown(x float32) float32 {
	if x == 0 {
		x = float32(math.Copysign(0, -1))
	}
	key := float32SortKey(math.Float32bits(x))
	return math.Float32frombits(float32FromSortKey(key - 1))
}

// float32Ceil returns the smallest float32 x such that float64(x) >= f,
// for finite, non-NaN f. Go's float64→float32 conversion already
// rounds to nearest (ties to even); when that rounds DOWN (the nearest
// float32 is < f), the next representable float32 up is the true
// ceiling — there is no float32 in between by definition of "nearest".
func float32Ceil(f float64) float32 {
	x := float32(f)
	if float64(x) < f {
		return float32NextUp(x)
	}
	return x
}

// float32Floor returns the largest float32 x such that float64(x) <= f,
// for finite, non-NaN f. Mirrors float32Ceil for the opposite direction.
func float32Floor(f float64) float32 {
	x := float32(f)
	if float64(x) > f {
		return float32NextDown(x)
	}
	return x
}
