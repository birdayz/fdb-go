package recordlayer

import "math"

// fhtKacRotator implements the FHT-KAC random orthogonal rotator.
// Wire-compatible with Java's com.apple.foundationdb.linear.FhtKacRotator.
//
// The rotator applies multiple rounds of:
//  1. Rademacher sign flips (random +1/-1 per dimension)
//  2. Normalized FHT on the largest power-of-2 block (alternating head/tail)
//  3. pi/4 Givens rotation pairing first half with second half
//
// All three operations preserve distances (orthogonal). The combination with
// random signs decorrelates dimensions, improving RaBitQ quantization quality.
//
// The rotator is deterministic given a seed: it uses Java's java.util.Random
// to generate the sign bits, matching Java's implementation exactly.
type fhtKacRotator struct {
	numDimensions int
	rounds        int
	signs         [][]bool // signs[round][dim]: true = +1, false = -1
}

const invSqrt2 = 1.0 / math.Sqrt2

// newFhtKacRotator creates a new FHT-KAC rotator matching Java's constructor.
// seed is used to initialize a Java-compatible java.util.Random for sign generation.
// rounds is typically 10 (matching Java's default in HNSW).
func newFhtKacRotator(seed int64, numDimensions, rounds int) *fhtKacRotator {
	rng := newJavaRandom(seed)
	signs := make([][]bool, rounds)
	for r := 0; r < rounds; r++ {
		s := make([]bool, numDimensions)
		for i := 0; i < numDimensions; i++ {
			s[i] = rng.nextBoolean()
		}
		signs[r] = s
	}
	return &fhtKacRotator{
		numDimensions: numDimensions,
		rounds:        rounds,
		signs:         signs,
	}
}

// apply applies the forward rotation to a vector.
// Matches Java's FhtKacRotator.operate().
func (r *fhtKacRotator) apply(x []float64) []float64 {
	y := make([]float64, r.numDimensions)
	copy(y, x)

	for round := 0; round < r.rounds; round++ {
		// 1) Rademacher signs
		s := r.signs[round]
		for i := 0; i < r.numDimensions; i++ {
			if !s[i] {
				y[i] = -y[i]
			}
		}

		// 2) FHT on largest 2^k block; alternate head/tail
		m := largestPow2LE(r.numDimensions)
		start := 0
		if round&1 != 0 {
			start = r.numDimensions - m
		}
		fhtNormalized(y, start, m)

		// 3) pi/4 Givens rotation between halves
		givensPiOver4(y)
	}
	return y
}

// transposedApply applies the inverse (transpose) rotation.
// Matches Java's FhtKacRotator.operateTranspose().
func (r *fhtKacRotator) transposedApply(x []float64) []float64 {
	y := make([]float64, r.numDimensions)
	copy(y, x)

	for round := r.rounds - 1; round >= 0; round-- {
		// Inverse of step 3: Givens transpose (angle -> -pi/4)
		givensMinusPiOver4(y)

		// Inverse of step 2: FWHT is its own inverse (orthonormal)
		m := largestPow2LE(r.numDimensions)
		start := 0
		if round&1 != 0 {
			start = r.numDimensions - m
		}
		fhtNormalized(y, start, m)

		// Inverse of step 1: Rademacher signs (self-inverse)
		s := r.signs[round]
		for i := 0; i < r.numDimensions; i++ {
			if !s[i] {
				y[i] = -y[i]
			}
		}
	}
	return y
}

// largestPow2LE returns the largest power of 2 <= n.
// Matches Java's Integer.numberOfLeadingZeros approach.
func largestPow2LE(n int) int {
	if n <= 0 {
		return 0
	}
	// Find highest set bit position.
	v := uint32(n)
	v |= v >> 1
	v |= v >> 2
	v |= v >> 4
	v |= v >> 8
	v |= v >> 16
	return int((v >> 1) + 1)
}

// fhtNormalized applies the normalized Fast Walsh-Hadamard Transform in-place
// on y[start..start+m-1], where m is a power of two.
// Matches Java's FhtKacRotator.fhtNormalized().
func fhtNormalized(y []float64, start, m int) {
	for length := 1; length < m; length <<= 1 {
		step := length << 1
		for i := start; i < start+m; i += step {
			for j := 0; j < length; j++ {
				a := i + j
				b := a + length
				u := y[a]
				v := y[b]
				y[a] = u + v
				y[b] = u - v
			}
		}
	}
	scale := 1.0 / math.Sqrt(float64(m))
	for i := start; i < start+m; i++ {
		y[i] *= scale
	}
}

// givensPiOver4 applies pi/4 Givens rotation pairing element i with element i+h.
// Matches Java's FhtKacRotator.givensPiOver4().
func givensPiOver4(y []float64) {
	h := len(y) / 2 // floor division
	for i := 0; i < h; i++ {
		j := i + h
		if j >= len(y) {
			break
		}
		u := y[i]
		v := y[j]
		y[i] = (u + v) * invSqrt2
		y[j] = (-u + v) * invSqrt2
	}
}

// givensMinusPiOver4 applies the transpose (inverse) of the pi/4 Givens rotation.
// Matches Java's FhtKacRotator.givensMinusPiOver4().
func givensMinusPiOver4(y []float64) {
	h := len(y) / 2
	for i := 0; i < h; i++ {
		j := i + h
		if j >= len(y) {
			break
		}
		u := y[i]
		v := y[j]
		y[i] = (u - v) * invSqrt2
		y[j] = (u + v) * invSqrt2
	}
}

// --- Java java.util.Random implementation ---
// Required for wire-compatibility with Java's FhtKacRotator which uses
// new Random(seed).nextBoolean() for sign generation.
//
// Java's Random uses a 48-bit LCG (Linear Congruential Generator):
//   seed = (seed * 0x5DEECE66DL + 0xBL) & ((1L << 48) - 1)
//   next(bits) = (int)(seed >>> (48 - bits))
//   nextBoolean() = next(1) != 0

const (
	javaRandomMultiplier = 0x5DEECE66D
	javaRandomAddend     = 0xB
	javaRandomMask       = (1 << 48) - 1
)

type javaRandom struct {
	seed int64
}

// newJavaRandom creates a Java-compatible Random with the given seed.
// Matches Java's: this.seed = (seed ^ multiplier) & mask
func newJavaRandom(seed int64) *javaRandom {
	return &javaRandom{
		seed: (seed ^ javaRandomMultiplier) & javaRandomMask,
	}
}

// next generates the next n bits from the LCG.
// Matches Java's Random.next(int bits).
func (r *javaRandom) next(bits int) int32 {
	r.seed = (r.seed*javaRandomMultiplier + javaRandomAddend) & javaRandomMask
	return int32(r.seed >> (48 - bits))
}

// nextBoolean returns the next random boolean.
// Matches Java's Random.nextBoolean().
func (r *javaRandom) nextBoolean() bool {
	return r.next(1) != 0
}

// nextLong returns the next random int64.
// Matches Java's Random.nextLong() = ((long)next(32) << 32) + next(32).
func (r *javaRandom) nextLong() int64 {
	return (int64(r.next(32)) << 32) + int64(r.next(32))
}

// --- hnswTransform ---
// Combines rotation and centroid subtraction for the RaBitQ pipeline.
// Matches Java's StorageTransform = AffineOperator(FhtKacRotator, negatedCentroid).

type hnswTransform struct {
	rotator          *fhtKacRotator
	negatedCentroid  []float64 // nil = no centroid subtraction
	normalizeVectors bool      // true for cosine metric
}

// newHNSWTransform creates a transform from a rotator seed and centroid.
// If rotatorSeed is -1, returns nil (identity transform).
func newHNSWTransform(rotatorSeed int64, negatedCentroid []float64, numDimensions int, normalizeVectors bool) *hnswTransform {
	if rotatorSeed == -1 {
		return nil
	}
	return &hnswTransform{
		rotator:          newFhtKacRotator(rotatorSeed, numDimensions, 10),
		negatedCentroid:  negatedCentroid,
		normalizeVectors: normalizeVectors,
	}
}

// apply transforms a vector: normalize (if cosine) -> rotate -> add negated centroid.
// Matches Java's StorageTransform.apply() -> AffineOperator.apply().
// AffineOperator.apply: linearOperator.apply(v) then add translationVector.
func (t *hnswTransform) apply(vec []float64) []float64 {
	if t == nil {
		return vec
	}

	input := vec
	if t.normalizeVectors {
		input = normalizeFloat64s(vec)
	}

	result := t.rotator.apply(input)

	if t.negatedCentroid != nil {
		for i := range result {
			result[i] += t.negatedCentroid[i]
		}
	}

	return result
}

// invertedApply applies the inverse transform: subtract negated centroid -> inverse rotate.
// Matches Java's StorageTransform.invertedApply() -> AffineOperator.invertedApply().
// AffineOperator.invertedApply: subtract translationVector then transposedApply.
// Note: normalization is NOT undone (impossible without original norm).
func (t *hnswTransform) invertedApply(vec []float64) []float64 {
	if t == nil {
		return vec
	}

	result := make([]float64, len(vec))
	copy(result, vec)

	if t.negatedCentroid != nil {
		for i := range result {
			result[i] -= t.negatedCentroid[i]
		}
	}

	return t.rotator.transposedApply(result)
}

// normalizeFloat64s returns the L2-normalized version of a vector.
func normalizeFloat64s(v []float64) []float64 {
	var sumSq float64
	for _, x := range v {
		sumSq += x * x
	}
	norm := math.Sqrt(sumSq)
	if norm == 0 || math.IsInf(norm, 0) || math.IsNaN(norm) {
		out := make([]float64, len(v))
		copy(out, v)
		return out
	}
	out := make([]float64, len(v))
	inv := 1.0 / norm
	for i, x := range v {
		out[i] = x * inv
	}
	return out
}
