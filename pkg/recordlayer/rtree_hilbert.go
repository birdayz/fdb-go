package recordlayer

import "math/big"

// hilbertValue computes the Hilbert curve index for an N-dimensional point.
// The result is a BigInteger with numBits*numDimensions bits.
// Matches Java's RTreeHilbertCurveHelpers.hilbertValue().
func hilbertValue(coords []int64) *big.Int {
	n := len(coords)
	if n == 0 {
		return big.NewInt(0)
	}

	numBits := 64

	// Shift coordinates from signed to unsigned space.
	shifted := make([]uint64, n)
	for i, c := range coords {
		shifted[i] = shiftCoordinate(c)
	}

	// Compute transposed Hilbert index.
	transposed := transposedIndex(numBits, shifted)

	// Interleave bits into a single BigInteger.
	return toIndex(numBits, transposed, n)
}

// shiftCoordinate maps a signed int64 to unsigned uint64 space for Hilbert ordering.
// Matches Java's RTreeHilbertCurveHelpers.shiftCoordinate().
//
// Mapping:
//
//	MinInt64 → 0
//	-1       → MaxInt64
//	0        → MaxInt64 + 1 (= 1 << 63)
//	MaxInt64 → MaxUint64
func shiftCoordinate(coord int64) uint64 {
	if coord < 0 {
		// Maps [-2^63, -1] → [0, 2^63-1]
		return uint64(coord) + (1 << 63)
	}
	// Maps [0, 2^63-1] → [2^63, 2^64-1]
	return uint64(coord) | (1 << 63)
}

// transposedIndex computes the Hilbert curve transposition.
// This is the core algorithm from davidmoten/hilbert-curve (Apache 2.0).
// Input: unsigned coordinates. Output: transposed representation (modified in-place pattern).
// Matches Java's RTreeHilbertCurveHelpers.transposedIndex().
func transposedIndex(numBits int, coords []uint64) []uint64 {
	n := len(coords)
	x := make([]uint64, n)
	copy(x, coords)

	m := uint64(1) << uint(numBits-1)

	// Inverse undo excess work.
	var q, p uint64
	for q = m; q > 1; q >>= 1 {
		p = q - 1
		for i := 0; i < n; i++ {
			if x[i]&q != 0 {
				x[0] ^= p
			} else {
				t := (x[0] ^ x[i]) & p
				x[0] ^= t
				x[i] ^= t
			}
		}
	}

	// Gray encode.
	for i := 1; i < n; i++ {
		x[i] ^= x[i-1]
	}
	t2 := uint64(0)
	for q = m; q > 1; q >>= 1 {
		if x[n-1]&q != 0 {
			t2 ^= q - 1
		}
	}
	for i := 0; i < n; i++ {
		x[i] ^= t2
	}

	return x
}

// toIndex interleaves the transposed bits into a single big.Int.
// For N dimensions with B bits each, produces a B*N bit integer.
// Matches Java's RTreeHilbertCurveHelpers.toIndex().
func toIndex(numBits int, transposed []uint64, numDimensions int) *big.Int {
	result := new(big.Int)

	totalBits := numBits * numDimensions
	b := 0
	bitsLeft := numBits
	for bitsLeft > 0 {
		bitsLeft--
		for dim := 0; dim < numDimensions; dim++ {
			if transposed[dim]&(1<<uint(bitsLeft)) != 0 {
				result.SetBit(result, totalBits-1-b, 1)
			}
			b++
		}
	}

	return result
}
