package rabitq

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"math"
	"math/rand"
	"runtime"
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

// TestNewQuantizerKeepsItsCount pins that a quantizer carries the extra-bit
// count it was made with, whatever it is: an unsupported count used to be
// replaced by 4, so an index configured for 0 or 9 bits stored 4-bit codes.
// ValidNumExBits is the encoder's range, Java's RaBitQuantizer's 1 to 8.
func TestNewQuantizerKeepsItsCount(t *testing.T) {
	t.Parallel()
	for _, bits := range []int{-1, 0, 1, 4, 8, 9, 15} {
		if got := NewQuantizer(MetricEuclidean, bits).NumExBits(); got != bits {
			t.Errorf("NewQuantizer(%d).NumExBits() = %d", bits, got)
		}
		if got, want := ValidNumExBits(bits), bits >= 1 && bits <= 8; got != want {
			t.Errorf("ValidNumExBits(%d) = %t, want %t", bits, got, want)
		}
	}
}

// Scorer must be bit-identical to Distance — it exists only to hoist
// allocations out of the per-code loop (RFC-094 094.4).
func TestScorerMatchesDistance(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewSource(7))
	for _, exBits := range []int{1, 2, 4} {
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
			// Cosine zero-norm queries must ERROR through the scorer exactly
			// like Distance — not rank as a finite estimate (RFC-094).
			qc := NewQuantizer(MetricCosine, exBits)
			zq := make([]float64, dims)
			zcode := qc.Encode(vec)
			_, derr := qc.Distance(zq, zcode, dims)
			_, serr2 := qc.NewScorer(zq).Score(zcode, dims)
			if (derr == nil) != (serr2 == nil) {
				t.Fatalf("exBits=%d: cosine zero-query divergence: Distance err=%v Scorer err=%v", exBits, derr, serr2)
			}
		}
	}
}

// FuzzUnpackComponentsFastPath pins the 2-bit fast path bit-identical to the
// generic loop on the same inputs (the production default numExBits=1 shape).
func FuzzUnpackComponentsFastPath(f *testing.F) {
	f.Add([]byte{0xB1, 0x00, 0xFF, 0x6C}, uint8(16))
	f.Fuzz(func(t *testing.T, data []byte, dimsRaw uint8) {
		dims := (int(dimsRaw)%64 + 1) * 4 // %4 == 0, 4..256
		need := dims / 4
		if len(data) < need {
			return
		}
		fast := make([]int, dims)
		if err := unpackComponentsInto(data, fast, 1); err != nil {
			t.Fatalf("fast path errored on valid input: %v", err)
		}
		generic := make([]int, dims)
		unpackComponentsGenericInto(data, generic, 2)
		for i := range fast {
			if fast[i] != generic[i] {
				t.Fatalf("component %d: fast %d != generic %d", i, fast[i], generic[i])
			}
		}
	})
}

func BenchmarkScorerScore(b *testing.B) {
	q := NewQuantizer(MetricEuclidean, 1)
	vec := make([]float64, 128)
	query := make([]float64, 128)
	for i := range vec {
		vec[i] = float64(i%17)/8 - 1
		query[i] = float64(i%13)/6 - 1
	}
	code := q.Encode(vec)
	s := q.NewScorer(query)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.Score(code, 128); err != nil {
			b.Fatal(err)
		}
	}
}

// TestScorerFusedPathMatchesDistance forces the fused 2-bit unpack+dot shape
// (exBits=1, dims%4==0) that the random-dims differential only hits by
// chance, across the production dimension counts.
func TestScorerFusedPathMatchesDistance(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewSource(11))
	q := NewQuantizer(MetricEuclidean, 1)
	for _, dims := range []int{4, 64, 128, 256} {
		for trial := 0; trial < 50; trial++ {
			vec := make([]float64, dims)
			query := make([]float64, dims)
			for i := range vec {
				vec[i] = rng.NormFloat64() * 3
				query[i] = rng.NormFloat64() * 3
			}
			code := q.Encode(vec)
			want, werr := q.Distance(query, code, dims)
			got, gerr := q.NewScorer(query).Score(code, dims)
			if (werr == nil) != (gerr == nil) {
				t.Fatalf("dims=%d trial=%d: error mismatch: %v vs %v", dims, trial, werr, gerr)
			}
			if werr == nil && got != want {
				t.Fatalf("dims=%d trial=%d: fused Score=%v Distance=%v", dims, trial, got, want)
			}
		}
	}
}

// These literal bytes come from the live Java RaBitQuantizer at target
// fdacd162a9c8acfadc49082b89185c823ab8ae4a. The conformance encoder test
// independently compares the current Java oracle to Go, including every header.
func TestRaBitQJavaEncodingGoldens(t *testing.T) {
	t.Parallel()
	// Java Math.sqrt retains a native NaN sign, and EncodedRealVector writes
	// its raw bits. These are independently measured ARM64 Java 4.14.2.0
	// bytes, not an allowance for either NaN: each architecture has one exact
	// expected encoding. All finite headers and all decoded bits share the
	// original AMD64 oracle below. RaBitQArchitectureContract pins the Java side.
	arm64NaN := map[string]string{
		"zero_four_1":   "03000000000000000080000000000000007ff8000000000000aa",
		"zero_four_4":   "03000000000000000080000000000000007ff8000000000000842100",
		"zero_four_8":   "03000000000000000080000000000000007ff80000000000008040201000",
		"signed_zero_1": "03000000000000000080000000000000007ff8000000000000a8",
		"signed_zero_4": "03000000000000000080000000000000007ff80000000000008420",
		"signed_zero_8": "03000000000000000080000000000000007ff800000000000080402000",
		"equal_three_8": "034008000000000000bf80182436517a377ff8000000000000ff7fbfc0",
	}

	for _, tc := range []struct {
		name       string
		vector     []float64
		bits       int
		hex        string
		decodedHex string
	}{
		{"fma_rounding_boundary_1", []float64{math.Ldexp(9, -29), 1 + math.Ldexp(1, -27)}, 1, "033ff0000004000001bff55555560000003ff4444433ccccd0b0", "023fd43d1364cfeb7c3fee5b9d1737e13a"},
		{"fma_rounding_boundary_4", []float64{math.Ldexp(9, -29), 1 + math.Ldexp(1, -27)}, 4, "033ff0000004000001bfc084210a2c38793fbf6170e820cff087c0", "023fa081ee50c76d733feffbbdbc82640f"},
		{"fma_rounding_boundary_8", []float64{math.Ldexp(9, -29), 1 + math.Ldexp(1, -27)}, 8, "033ff0000004000001bf80080403ffbebf3f7e75902131b8e3807fc0", "023f60080200ff5f303feffffbfffdbf01"},
		{"quantization_seven_1", []float64{1, -2, 3, -4, 5, -6, 7}, 1, "034061800000000000c01cb7cb7cb7cb7d4014f6d59069a9589ccc", "023ffb9d471196c4f4bffb9d471196c4f44014b5f54d3113b7c014b5f54d3113b74014b5f54d3113b7c014b5f54d3113b74014b5f54d3113b7"},
		{"quantization_seven_4", []float64{1, -2, 3, -4, 5, -6, 7}, 4, "034061800000000000bfecb7cb7cb7cb7d3fdd686479602f4492ec7d8be0", "023ff1f16ecbd15537c0002616eaa2ccb2400753766f5ceec8c00e80d5f41710de4014a2729d9721ffc01839225ff4330a401bcfd222514415"},
		{"quantization_seven_8", []float64{1, -2, 3, -4, 5, -6, 7}, 8, "034061800000000000bfac5d9b4d4be0cc3f9a089c0e09b496922ded86fda09ff8", "023ff02d618dc15c66c0001103f43c879a40080b5721986100c01002d5277a1d344013fffebe2809e7c017fd2854d5f69a401bfa51eb83e34e"},
		{"asymmetric_four_1", []float64{-3.0, 8.0, 2.0, 6.0}, 1, "03405c400000000000c0233bea3677d46d400c595f2ee831d77b", "02c003040a596e6555401c860f862598004003040a596e6555401c860f86259800"},
		{"asymmetric_four_4", []float64{-3.0, 8.0, 2.0, 6.0}, 4, "03405c400000000000bff0b3bb6b02f4c43fe0710ee86e1c9957e7b0", "02c006f5b4957b29e140202d1c520b23533ffd38b749e29264401800dfb38c65f7"},
		{"asymmetric_four_8", []float64{-3.0, 8.0, 2.0, 6.0}, 8, "03405c400000000000bfb01acb293941cf3f97bc98cf24394a507fa7fbe0", "02c00807fa605cee07402002a273cd53863ffff52a1cf6e4144017f7df95b92b0f"},
		{"axis_four_1", []float64{0.0, -7.0, 0.0, 0.0}, 1, "034048800000000000c022aaaaaaaaaaaa4021bbbbbbbbbbb98a", "0240002a725cde2cb9c0183fab8b4d431640002a725cde2cb940002a725cde2cb9"},
		{"axis_four_4", []float64{0.0, -7.0, 0.0, 0.0}, 4, "034048800000000000bfece739ce739ce73feb7543b7543ca8802100", "023fccdbb419ae6b01c01bf4d678e0f7a93fccdbb419ae6b013fccdbb419ae6b01"},
		{"axis_four_8", []float64{0.0, -7.0, 0.0, 0.0}, 8, "034048800000000000bfac0e070381c0e03faaa6ed1021eeba8000201000", "023f8c0dfc73b7ea8dc01bfff5757e0e983f8c0dfc73b7ea8d3f8c0dfc73b7ea8d"},
		{"equal_three_1", []float64{1.0, 1.0, 1.0}, 1, "034008000000000000bff55555555555530000000000000000fc", "023ff00000000000003ff00000000000003ff0000000000000"},
		{"equal_three_4", []float64{1.0, 1.0, 1.0}, 4, "034008000000000000bfc08421084210840000000000000000fffe", "023ff00000000000003ff00000000000003ff0000000000000"},
		{"equal_three_8", []float64{1.0, 1.0, 1.0}, 8, "034008000000000000bf80182436517a37fff8000000000000ff7fbfc0", "023fefffffffffffff3fefffffffffffff3fefffffffffffff"},
		{"fractional_five_1", []float64{0.1, -1.7, 0.003, 11.5, -0.25}, 1, "034060e6ccdfaca362c02d97b7cbf09dfb4028d27be6b0afb69b40", "024009cce836294ed5c009cce836294ed54009cce836294ed5402359ae289efb20c009cce836294ed5"},
		{"fractional_five_4", []float64{0.1, -1.7, 0.003, 11.5, -0.25}, 4, "034060e6ccdfaca362bff7af82af5d95293fee1fff7392959d8361f780", "023fd7aa036de29587bffd9484495b3ae93fd7aa036de295874026ecb3527380dbbfd7aa036de29587"},
		{"fractional_five_8", []float64{0.1, -1.7, 0.003, 11.5, -0.25}, 8, "034060e6ccdfaca362bfb739ec24c3cdae3fa7338723e84bd98136a01fd7d0", "023fbd08632d652f50bffb37dcfa8edc5b3f9739e8f11dbf734026ffd82ac2f514bfcfefa04b88e73e"},
		{"scalar_1", []float64{3.0}, 1, "034022000000000000c0100000000000000000000000000000c0", "024008000000000000"},
		{"scalar_4", []float64{3.0}, 4, "034022000000000000bfd8c6318c6318c60000000000000000f8", "024008000000000000"},
		{"scalar_8", []float64{3.0}, 8, "034022000000000000bf9c82ac402603900000000000000000eb80", "024008000000000000"},
		{"signed_zero_1", []float64{math.Copysign(0, -1), 0.0, math.Copysign(0, -1)}, 1, "0300000000000000008000000000000000fff8000000000000a8", "02000000000000000000000000000000000000000000000000"},
		{"signed_zero_4", []float64{math.Copysign(0, -1), 0.0, math.Copysign(0, -1)}, 4, "0300000000000000008000000000000000fff80000000000008420", "02000000000000000000000000000000000000000000000000"},
		{"signed_zero_8", []float64{math.Copysign(0, -1), 0.0, math.Copysign(0, -1)}, 8, "0300000000000000008000000000000000fff800000000000080402000", "02000000000000000000000000000000000000000000000000"},
		{"zero_four_1", []float64{0.0, 0.0, 0.0, 0.0}, 1, "0300000000000000008000000000000000fff8000000000000aa", "020000000000000000000000000000000000000000000000000000000000000000"},
		{"zero_four_4", []float64{0.0, 0.0, 0.0, 0.0}, 4, "0300000000000000008000000000000000fff8000000000000842100", "020000000000000000000000000000000000000000000000000000000000000000"},
		{"zero_four_8", []float64{0.0, 0.0, 0.0, 0.0}, 8, "0300000000000000008000000000000000fff80000000000008040201000", "020000000000000000000000000000000000000000000000000000000000000000"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if runtime.GOARCH == "arm64" {
				if armHex, ok := arm64NaN[tc.name]; ok {
					tc.hex = armHex
				}
			}
			want, err := hex.DecodeString(tc.hex)
			if err != nil {
				t.Fatal(err)
			}
			got := NewRaBitQuantizer(MetricEuclidean, tc.bits).Encode(tc.vector).ToBytes()
			if !bytes.Equal(got, want) {
				t.Fatalf("encoded bytes = %x, Java wants %x", got, want)
			}
			wantDecoded, err := hex.DecodeString(tc.decodedHex)
			if err != nil {
				t.Fatal(err)
			}
			components, err := NewQuantizer(MetricEuclidean, tc.bits).Decode(want, len(tc.vector))
			if err != nil {
				t.Fatal(err)
			}
			if len(wantDecoded) != 1+8*len(components) {
				t.Fatal("invalid decoded golden length")
			}
			for i, component := range components {
				wantBits := binary.BigEndian.Uint64(wantDecoded[1+8*i:])
				if gotBits := math.Float64bits(component); gotBits != wantBits {
					t.Errorf("decoded component %d bits = %016x, Java wants %016x", i, gotBits, wantBits)
				}
			}
			decoded, err := EncodedVectorFromBytes(want, len(tc.vector), tc.bits)
			if err != nil {
				t.Fatal(err)
			}
			if got := decoded.ToBytes(); !bytes.Equal(got, want) {
				t.Fatalf("round-trip changed Java header or components: %x, want %x", got, want)
			}
		})
	}
}

func TestRaBitQDecodeNonpositiveOrNaNNorm(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		norm float64
	}{
		{"zero", 0}, {"negative_zero", math.Copysign(0, -1)}, {"negative", -1}, {"nan", math.NaN()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := &EncodedVector{Encoded: []int{1, 8, 15, 20}, NumExBits: 4, FAddEx: tc.norm}
			got, err := NewQuantizer(MetricEuclidean, 4).Decode(e.ToBytes(), 4)
			if err != nil {
				t.Fatal(err)
			}
			for i, v := range got {
				if math.Float64bits(v) != 0 {
					t.Errorf("component %d = %v, want positive zero", i, v)
				}
			}
		})
	}
}

// This checks codec invariants over finite dyadic inputs, not Java parity;
// TestRaBitQJavaEncodingGoldens and the live conformance test supply that oracle.
func FuzzRaBitQEncoding(f *testing.F) {
	f.Add([]byte{0, 0, 0, 0}, uint8(4))
	f.Add([]byte{3, 128, 2, 96, 255}, uint8(8))
	f.Fuzz(func(t *testing.T, input []byte, bitsRaw uint8) {
		t.Parallel()
		if len(input) == 0 || len(input) > 128 {
			return
		}
		bits := int(bitsRaw)%8 + 1
		vector := make([]float64, len(input))
		var sumSquares int64
		for i, v := range input {
			n := int64(v) - 128
			sumSquares += n * n
			vector[i] = float64(n) / 16
		}
		encoded := NewRaBitQuantizer(MetricEuclidean, bits).Encode(vector)
		if encoded.FAddEx != float64(sumSquares)/256 {
			t.Fatalf("stored norm = %v, want exact dyadic norm %v", encoded.FAddEx, float64(sumSquares)/256)
		}
		for i, code := range encoded.Encoded {
			if code < 0 || code >= 1<<(bits+1) {
				t.Fatalf("component %d code %d outside %d-bit storage", i, code, bits+1)
			}
			if vector[i] != float64(int64(input[i])-128)/16 {
				t.Fatalf("encoder mutated input component %d", i)
			}
		}
		data := encoded.ToBytes()
		if len(data) != 25+(len(vector)*(bits+1)+7)/8 {
			t.Fatal("incorrect packed length")
		}
		decoded, err := EncodedVectorFromBytes(data, len(vector), bits)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(decoded.ToBytes(), data) {
			t.Fatal("round-trip changed calibration bits or packed components")
		}
		reconstructed, err := NewQuantizer(MetricEuclidean, bits).Decode(data, len(vector))
		if err != nil {
			t.Fatal(err)
		}
		for _, component := range reconstructed {
			if math.IsNaN(component) || math.IsInf(component, 0) {
				t.Fatalf("finite input reconstructed as %v", component)
			}
			if sumSquares == 0 && math.Float64bits(component) != 0 {
				t.Fatalf("zero residual reconstructed as %v", component)
			}
		}
	})
}

func TestDotRoundsProductsBeforeAccumulation(t *testing.T) {
	t.Parallel()
	v := []float64{math.Ldexp(9, -29), 1 + math.Ldexp(1, -27)}
	const want uint64 = 0x3ff0000004000001
	if got := math.Float64bits(dot(v, v)); got != want {
		t.Fatalf("dot bits = %016x, Java scalar reduction wants %016x", got, want)
	}
	// This vector must discriminate fusion, not merely compare two paths
	// which a compiler could change in the same way.
	fused := math.FMA(v[1], v[1], float64(v[0]*v[0]))
	if math.Float64bits(fused) != want+1 {
		t.Fatalf("counterexample no longer discriminates FMA: %016x", math.Float64bits(fused))
	}
}
