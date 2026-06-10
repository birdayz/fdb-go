package rabitq

import (
	"math"
	"math/rand"
	"testing"
)

// --- Helper functions ---

func randomVector(rng *rand.Rand, dims int) []float64 {
	v := make([]float64, dims)
	for i := range v {
		v[i] = rng.NormFloat64()
	}
	return v
}

func normalizeVector(v []float64) []float64 {
	n := l2Norm(v)
	if n == 0 {
		return v
	}
	out := make([]float64, len(v))
	for i := range v {
		out[i] = v[i] / n
	}
	return out
}

func exactEuclideanSquare(a, b []float64) float64 {
	sum := 0.0
	for i := range a {
		d := a[i] - b[i]
		sum += d * d
	}
	return sum
}

// --- Tests ---

func TestEncodedVectorSerializationRoundTrip(t *testing.T) {
	t.Parallel()

	seeds := []int64{0xdeadc0de, 0xfdb5ca1e, 0xf005ba1}
	dimensions := []int{3, 5, 10, 128, 768}
	exBits := []int{1, 4, 5, 6, 7, 8}

	for _, seed := range seeds {
		for _, dim := range dimensions {
			for _, nb := range exBits {
				t.Run("", func(t *testing.T) {
					t.Parallel()
					rng := rand.New(rand.NewSource(seed))
					v := randomVector(rng, dim)

					q := NewRaBitQuantizer(MetricEuclidean, nb)
					encoded := q.Encode(v)

					data := encoded.ToBytes()
					if data[0] != TypeByte {
						t.Fatalf("expected type ordinal %d, got %d", TypeByte, data[0])
					}

					decoded, err := EncodedVectorFromBytes(data, dim, nb)
					if err != nil {
						t.Fatalf("fromBytes: %v", err)
					}

					if decoded.NumDimensions() != dim {
						t.Fatalf("dim mismatch: %d vs %d", decoded.NumDimensions(), dim)
					}
					if decoded.NumExBits != nb {
						t.Fatalf("numExBits mismatch: %d vs %d", decoded.NumExBits, nb)
					}
					if decoded.FAddEx != encoded.FAddEx {
						t.Fatalf("fAddEx mismatch: %v vs %v", decoded.FAddEx, encoded.FAddEx)
					}
					if decoded.FRescaleEx != encoded.FRescaleEx {
						t.Fatalf("fRescaleEx mismatch: %v vs %v", decoded.FRescaleEx, encoded.FRescaleEx)
					}
					if decoded.FErrorEx != encoded.FErrorEx {
						t.Fatalf("fErrorEx mismatch: %v vs %v", decoded.FErrorEx, encoded.FErrorEx)
					}
					for i := 0; i < dim; i++ {
						if decoded.Encoded[i] != encoded.Encoded[i] {
							t.Fatalf("encoded[%d] mismatch: %d vs %d", i, decoded.Encoded[i], encoded.Encoded[i])
						}
					}
				})
			}
		}
	}
}

func TestQuantizerEncodeSelfDistanceNearZero(t *testing.T) {
	t.Parallel()

	// Encode a vector, then estimate distance between it and its encoding.
	// Should be very close to zero (self-distance).
	seeds := []int64{0xdeadc0de, 0xfdb5ca1e, 0xf005ba1}
	dimensions := []int{3, 5, 10, 128, 768}
	exBits := []int{4, 5, 6, 7, 8}

	for _, seed := range seeds {
		for _, dim := range dimensions {
			for _, nb := range exBits {
				t.Run("", func(t *testing.T) {
					t.Parallel()
					rng := rand.New(rand.NewSource(seed))
					v := randomVector(rng, dim)

					q := NewRaBitQuantizer(MetricEuclidean, nb)
					encoded := q.Encode(v)

					est := NewRaBitEstimator(MetricEuclidean, nb)
					dist, err := est.Distance(v, encoded)
					if err != nil {
						t.Fatalf("Distance returned error: %v", err)
					}

					if dist > 0.01 {
						t.Fatalf("self-distance should be near 0, got %v (dims=%d, exBits=%d)", dist, dim, nb)
					}
				})
			}
		}
	}
}

func TestQuantizerEncodeSelfDistanceCosine(t *testing.T) {
	t.Parallel()

	seeds := []int64{0xdeadc0de, 0xfdb5ca1e}
	dimensions := []int{3, 10, 128}
	exBits := []int{4, 6, 8}

	for _, seed := range seeds {
		for _, dim := range dimensions {
			for _, nb := range exBits {
				t.Run("", func(t *testing.T) {
					t.Parallel()
					rng := rand.New(rand.NewSource(seed))
					v := normalizeVector(randomVector(rng, dim))

					q := NewRaBitQuantizer(MetricCosine, nb)
					encoded := q.Encode(v)

					est := NewRaBitEstimator(MetricCosine, nb)
					result := est.EstimateDistance(v, encoded)

					if math.Abs(result.Distance) > 0.01 {
						t.Fatalf("cosine self-distance should be near 0, got %v", result.Distance)
					}
				})
			}
		}
	}
}

func TestQuantizerEncodeSelfDistanceDotProduct(t *testing.T) {
	t.Parallel()

	seeds := []int64{0xdeadc0de, 0xfdb5ca1e}
	dimensions := []int{3, 10, 128}
	exBits := []int{4, 6, 8}

	for _, seed := range seeds {
		for _, dim := range dimensions {
			for _, nb := range exBits {
				t.Run("", func(t *testing.T) {
					t.Parallel()
					rng := rand.New(rand.NewSource(seed))
					v := normalizeVector(randomVector(rng, dim))

					q := NewRaBitQuantizer(MetricInnerProduct, nb)
					encoded := q.Encode(v)

					est := NewRaBitEstimator(MetricInnerProduct, nb)
					result := est.EstimateDistance(v, encoded)

					// For dot product metric, self-distance of unit vector = -1.
					if math.Abs(result.Distance+1.0) > 0.01 {
						t.Fatalf("dot product self-distance of unit vector should be near -1, got %v", result.Distance)
					}
				})
			}
		}
	}
}

func TestEstimateDistanceVsExact(t *testing.T) {
	t.Parallel()

	// Verify that estimated distance is within error bounds of true distance
	// for a significant fraction of random vector pairs.
	rng := rand.New(rand.NewSource(42))
	numExBits := 7
	numDims := 128
	numRounds := 200

	withinBounds := 0
	var sumRelError float64

	for round := 0; round < numRounds; round++ {
		v := randomVector(rng, numDims)
		q := randomVector(rng, numDims)

		quantizer := NewRaBitQuantizer(MetricEuclidean, numExBits)
		encoded := quantizer.Encode(v)

		estimator := NewRaBitEstimator(MetricEuclidean, numExBits)
		result := estimator.EstimateDistance(q, encoded)

		trueDist := exactEuclideanSquare(v, q)

		if trueDist >= result.Distance-result.Error && trueDist < result.Distance+result.Error {
			withinBounds++
		}

		if trueDist > 0 {
			sumRelError += math.Abs(result.Distance-trueDist) / trueDist
		}
	}

	fractionWithin := float64(withinBounds) / float64(numRounds)
	avgRelError := sumRelError / float64(numRounds)

	if fractionWithin < 0.7 {
		t.Fatalf("expected >70%% within bounds, got %.1f%%", fractionWithin*100)
	}
	if avgRelError > 0.15 {
		t.Fatalf("expected avg relative error < 15%%, got %.1f%%", avgRelError*100)
	}
}

func TestEstimateDistanceVsExactHighDim(t *testing.T) {
	t.Parallel()

	// Higher dimensions (768) should give better estimates.
	rng := rand.New(rand.NewSource(12345))
	numExBits := 6
	numDims := 768
	numRounds := 100

	withinBounds := 0

	for round := 0; round < numRounds; round++ {
		v := randomVector(rng, numDims)
		q := randomVector(rng, numDims)

		quantizer := NewRaBitQuantizer(MetricEuclidean, numExBits)
		encoded := quantizer.Encode(v)

		estimator := NewRaBitEstimator(MetricEuclidean, numExBits)
		result := estimator.EstimateDistance(q, encoded)

		trueDist := exactEuclideanSquare(v, q)

		if trueDist >= result.Distance-result.Error && trueDist < result.Distance+result.Error {
			withinBounds++
		}
	}

	fractionWithin := float64(withinBounds) / float64(numRounds)
	if fractionWithin < 0.80 {
		t.Fatalf("768D: expected >80%% within bounds, got %.1f%%", fractionWithin*100)
	}
}

func TestEncodeDirectionPreserved(t *testing.T) {
	t.Parallel()

	// Verify that the encoded vector points in roughly the same direction as the original.
	// The re-centered encoded vector (encoded - cb) should have a high cosine similarity
	// with the original.
	seeds := []int64{0xdeadc0de, 0xfdb5ca1e, 0xf005ba1}
	dims := []int{3, 10, 128, 768}

	for _, seed := range seeds {
		for _, dim := range dims {
			t.Run("", func(t *testing.T) {
				t.Parallel()
				rng := rand.New(rand.NewSource(seed))
				v := randomVector(rng, dim)

				q := NewRaBitQuantizer(MetricEuclidean, 7)
				encoded := q.Encode(v)

				cb := -(float64(1<<7) - 0.5)
				reCentered := make([]float64, dim)
				for i := 0; i < dim; i++ {
					reCentered[i] = float64(encoded.Encoded[i]) + cb
				}

				// Compute cosine similarity.
				vNorm := normalizeVector(v)
				rcNorm := normalizeVector(reCentered)
				cosSim := dot(vNorm, rcNorm)

				if cosSim < 0.99 {
					t.Fatalf("direction not preserved: cosine similarity = %v (dim=%d)", cosSim, dim)
				}
			})
		}
	}
}

func TestZeroVector(t *testing.T) {
	t.Parallel()

	v := []float64{0, 0, 0}
	q := NewRaBitQuantizer(MetricEuclidean, 4)
	encoded := q.Encode(v)

	// All codes should be zero-ish (half level due to sign bit).
	if encoded.NumDimensions() != 3 {
		t.Fatalf("expected 3 dims, got %d", encoded.NumDimensions())
	}

	// Serialization round-trip should work.
	data := encoded.ToBytes()
	decoded, err := EncodedVectorFromBytes(data, 3, 4)
	if err != nil {
		t.Fatalf("fromBytes: %v", err)
	}
	for i := 0; i < 3; i++ {
		if decoded.Encoded[i] != encoded.Encoded[i] {
			t.Fatalf("encoded[%d] mismatch after round-trip", i)
		}
	}
}

func TestUnitVector(t *testing.T) {
	t.Parallel()

	// Unit vector along first axis.
	v := make([]float64, 128)
	v[0] = 1.0

	q := NewRaBitQuantizer(MetricEuclidean, 4)
	encoded := q.Encode(v)

	if encoded.NumDimensions() != 128 {
		t.Fatalf("expected 128 dims, got %d", encoded.NumDimensions())
	}

	// Serialization round-trip.
	data := encoded.ToBytes()
	decoded, err := EncodedVectorFromBytes(data, 128, 4)
	if err != nil {
		t.Fatalf("fromBytes: %v", err)
	}
	if decoded.FAddEx != encoded.FAddEx {
		t.Fatalf("fAddEx mismatch")
	}
}

func TestAllSameValueVector(t *testing.T) {
	t.Parallel()

	v := make([]float64, 64)
	for i := range v {
		v[i] = 0.5
	}

	q := NewRaBitQuantizer(MetricEuclidean, 5)
	encoded := q.Encode(v)

	// All encoded components should be the same (uniform vector).
	first := encoded.Encoded[0]
	for i := 1; i < len(encoded.Encoded); i++ {
		if encoded.Encoded[i] != first {
			t.Fatalf("encoded[%d] = %d, expected %d (uniform vector)", i, encoded.Encoded[i], first)
		}
	}

	// Self-distance should be near zero.
	est := NewRaBitEstimator(MetricEuclidean, 5)
	dist, err := est.Distance(v, encoded)
	if err != nil {
		t.Fatalf("Distance returned error: %v", err)
	}
	if dist > 0.01 {
		t.Fatalf("self-distance should be near 0, got %v", dist)
	}
}

func TestBitPackingSmall(t *testing.T) {
	t.Parallel()

	// Test bit packing with known values.
	// 3 components with 2 bits each (numExBits=1, bitsPerComponent=2).
	encoded := []int{3, 0, 2} // binary: 11, 00, 10
	bitsPerComponent := 2
	numBits := 3 * bitsPerComponent // 6 bits -> 1 byte
	buf := make([]byte, (numBits+7)/8)
	packEncodedComponents(encoded, bitsPerComponent, buf)

	// Expected: 11 00 10 xx = 0b11001000 = 0xC8
	if buf[0] != 0xC8 {
		t.Fatalf("expected 0xC8, got 0x%02X", buf[0])
	}

	// Unpack and verify.
	unpacked, err := unpackComponents(buf, 3, 1)
	if err != nil {
		t.Fatalf("unpackComponents: %v", err)
	}
	for i, want := range encoded {
		if unpacked[i] != want {
			t.Fatalf("unpacked[%d] = %d, want %d", i, unpacked[i], want)
		}
	}
}

func TestBitPackingCrossByteBoundary(t *testing.T) {
	t.Parallel()

	// 3 components with 5 bits each (numExBits=4, bitsPerComponent=5).
	// Total 15 bits -> 2 bytes.
	encoded := []int{31, 16, 7} // binary: 11111, 10000, 00111
	bitsPerComponent := 5
	numBits := 3 * bitsPerComponent // 15 bits -> 2 bytes
	buf := make([]byte, (numBits+7)/8)
	packEncodedComponents(encoded, bitsPerComponent, buf)

	// Unpack and verify round-trip.
	unpacked, err := unpackComponents(buf, 3, 4)
	if err != nil {
		t.Fatalf("unpackComponents: %v", err)
	}
	for i, want := range encoded {
		if unpacked[i] != want {
			t.Fatalf("unpacked[%d] = %d, want %d", i, unpacked[i], want)
		}
	}
}

func TestEncodedVectorFromBytesErrors(t *testing.T) {
	t.Parallel()

	// Too short.
	_, err := EncodedVectorFromBytes([]byte{3, 0, 0}, 1, 1)
	if err == nil {
		t.Fatal("expected error for short data")
	}

	// Wrong type ordinal.
	data := make([]byte, 30)
	data[0] = 0 // not RABITQ
	_, err = EncodedVectorFromBytes(data, 1, 1)
	if err == nil {
		t.Fatal("expected error for wrong ordinal")
	}
}

func TestEstimatorCosineZeroQuery(t *testing.T) {
	t.Parallel()

	// Zero query vector should return NaN for cosine metric.
	v := []float64{1.0, 2.0, 3.0}
	q := NewRaBitQuantizer(MetricCosine, 4)
	encoded := q.Encode(v)

	est := NewRaBitEstimator(MetricCosine, 4)
	result := est.EstimateDistance([]float64{0, 0, 0}, encoded)

	if !math.IsNaN(result.Distance) {
		t.Fatalf("expected NaN for zero query with cosine metric, got %v", result.Distance)
	}
}

func TestQuantizerPanicsOnInvalidExBits(t *testing.T) {
	t.Parallel()

	for _, nb := range []int{0, -1, 9, 100} {
		func() {
			defer func() {
				if r := recover(); r == nil {
					t.Fatalf("expected panic for numExBits=%d", nb)
				}
			}()
			NewRaBitQuantizer(MetricEuclidean, nb)
		}()
	}
}

func TestEstimatedDistanceSpecialValues(t *testing.T) {
	t.Parallel()

	// Replicate Java's basicEncodeWithEstimationTestSpecialValues for the
	// centroid=[0,0], v=[1,0], q=[0,1] case with expected distance 2.0.
	// This is the transformed (centroid-subtracted) case, so v and q are
	// relative to centroid.
	v := []float64{1.0, 0.0}
	q := []float64{0.0, 1.0}

	quantizer := NewRaBitQuantizer(MetricEuclidean, 7)
	encoded := quantizer.Encode(v)

	estimator := NewRaBitEstimator(MetricEuclidean, 7)
	result := estimator.EstimateDistance(q, encoded)

	// Expected: ||[1,0] - [0,1]||^2 = 2.0
	if math.Abs(result.Distance-2.0) > 0.01 {
		t.Fatalf("expected distance ~2.0, got %v", result.Distance)
	}
}

func TestEstimatedDistanceSpecialValues2(t *testing.T) {
	t.Parallel()

	// v=[0,0], q=[1,1] => ||[0,0] - [1,1]||^2 = 2.0
	v := []float64{0.0, 0.0}
	q := []float64{1.0, 1.0}

	quantizer := NewRaBitQuantizer(MetricEuclidean, 7)
	encoded := quantizer.Encode(v)

	estimator := NewRaBitEstimator(MetricEuclidean, 7)
	result := estimator.EstimateDistance(q, encoded)

	// Zero vector encoding should still give a reasonable distance estimate.
	// fAddEx = ||v||^2 = 0 for zero vector.
	if encoded.FAddEx != 0.0 {
		t.Fatalf("expected fAddEx=0 for zero vector, got %v", encoded.FAddEx)
	}
	// Distance estimate should be finite.
	if math.IsNaN(result.Distance) || math.IsInf(result.Distance, 0) {
		t.Fatalf("expected finite distance estimate, got %v", result.Distance)
	}
}

func TestMultipleExBitsPrecision(t *testing.T) {
	t.Parallel()

	// Higher numExBits should generally give better precision.
	// Verify that the self-distance decreases as numExBits increases.
	rng := rand.New(rand.NewSource(999))
	v := randomVector(rng, 128)

	var prevDist float64 = math.MaxFloat64
	improvements := 0
	for _, nb := range []int{1, 2, 3, 4, 5, 6, 7, 8} {
		q := NewRaBitQuantizer(MetricEuclidean, nb)
		encoded := q.Encode(v)
		est := NewRaBitEstimator(MetricEuclidean, nb)
		dist, err := est.Distance(v, encoded)
		if err != nil {
			t.Fatalf("Distance returned error with %d ex bits: %v", nb, err)
		}
		if dist > 1.0 {
			t.Fatalf("self-distance too large with %d ex bits: %v", nb, dist)
		}
		if dist < prevDist {
			improvements++
		}
		prevDist = dist
	}
	// Higher bits should generally improve precision. Not strictly monotonic
	// for all vectors due to quantization noise, but the overall trend should
	// show some improvement (at least 2 of 7 transitions).
	if improvements < 2 {
		t.Fatalf("expected higher numExBits to generally improve precision, got only %d/7 improvements", improvements)
	}
}

// TestQuantizerInterface verifies the Quantizer wrapper implements the
// full Encode/Distance/Decode/GetTypeByte contract.
func TestQuantizerInterface(t *testing.T) {
	t.Parallel()

	q := NewQuantizer(MetricEuclidean, 4)
	if q.GetTypeByte() != 3 {
		t.Fatalf("expected type byte 3, got %d", q.GetTypeByte())
	}

	vec := []float64{1.0, 2.0, 3.0, 4.0}
	encoded := q.Encode(vec)
	if len(encoded) == 0 {
		t.Fatal("Encode returned empty bytes")
	}
	if encoded[0] != 3 {
		t.Fatalf("first byte should be type 3, got %d", encoded[0])
	}

	// Distance to self should be near zero.
	dist, err := q.Distance(vec, encoded, 4)
	if err != nil {
		t.Fatalf("Distance error: %v", err)
	}
	if dist > 0.01 {
		t.Fatalf("self-distance should be near 0, got %v", dist)
	}

	// Decode should return approximation.
	decoded, err := q.Decode(encoded, 4)
	if err != nil {
		t.Fatalf("Decode error: %v", err)
	}
	if len(decoded) != 4 {
		t.Fatalf("expected 4 dims, got %d", len(decoded))
	}
}

// Scorer must be bit-identical to Distance — it exists only to hoist
// allocations out of the per-code loop (RFC-094 094.4).
func TestScorerMatchesDistance(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewSource(7))
	for _, exBits := range []int{0, 1, 2} {
		q := NewQuantizer(MetricEuclidean, exBits)
		for trial := 0; trial < 200; trial++ {
			dims := 2 + rng.Intn(64)
			vec := make([]float64, dims)
			query := make([]float64, dims)
			for i := range vec {
				vec[i] = rng.NormFloat64() * 3
				query[i] = rng.NormFloat64() * 3
			}
			code := q.Encode(vec)
			want, werr := q.Distance(query, code, dims)
			sc := q.NewScorer(query)
			got, gerr := sc.Score(code, dims)
			if (werr == nil) != (gerr == nil) {
				t.Fatalf("exBits=%d trial=%d: error mismatch: %v vs %v", exBits, trial, werr, gerr)
			}
			if werr == nil && got != want {
				t.Fatalf("exBits=%d trial=%d dims=%d: Score=%v Distance=%v", exBits, trial, dims, got, want)
			}
			// Reuse across DIFFERENT codes must not contaminate: the unpack
			// accumulates via |=, so a dirty buffer poisons every code after
			// the first (re-scoring the SAME code is idempotent under OR and
			// hides it).
			vec2 := make([]float64, dims)
			for i := range vec2 {
				vec2[i] = rng.NormFloat64() * 3
			}
			code2 := q.Encode(vec2)
			want2, _ := q.Distance(query, code2, dims)
			got2, _ := sc.Score(code2, dims)
			if got2 != want2 {
				t.Fatalf("exBits=%d trial=%d: scorer reuse across codes diverged: %v vs %v", exBits, trial, got2, want2)
			}
			got1Again, _ := sc.Score(code, dims)
			if got1Again != want {
				t.Fatalf("exBits=%d trial=%d: third Score diverged: %v vs %v", exBits, trial, got1Again, want)
			}
		}
	}
}
