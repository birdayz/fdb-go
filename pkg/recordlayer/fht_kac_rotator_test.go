package recordlayer

import (
	"math"
	"math/rand"
	"testing"

	"fdb.dev/pkg/rabitq"
)

// --- Java Random compatibility tests ---

func TestJavaRandomNextBoolean(t *testing.T) {
	t.Parallel()

	// Verify our Java Random implementation matches Java's java.util.Random.
	// The LCG algorithm is:
	//   initialSeed = (seed ^ 0x5DEECE66DL) & ((1L << 48) - 1)
	//   next(bits): seed = (seed * 0x5DEECE66DL + 0xBL) & mask; return seed >>> (48 - bits)
	//   nextBoolean() = next(1) != 0
	//
	// Verified against Java Random(42).nextBoolean() sequence.
	rng := newJavaRandom(42)
	expected := []bool{
		true, false, true, false, false,
		true, false, true, true, false,
		true, false, false, false, false,
		true, false, true, true, true,
	}
	for i, want := range expected {
		got := rng.nextBoolean()
		if got != want {
			t.Fatalf("nextBoolean()[%d]: got %v, want %v", i, got, want)
		}
	}
}

func TestJavaRandomNextLong(t *testing.T) {
	t.Parallel()

	// Verify Java Random.nextLong() compatibility.
	// Java Random(0).nextLong() = -4962768465676381896
	rng := newJavaRandom(0)
	got := rng.nextLong()
	want := int64(-4962768465676381896)
	if got != want {
		t.Fatalf("Random(0).nextLong() = %d, want %d", got, want)
	}
}

func TestJavaRandomNextLongSeed42(t *testing.T) {
	t.Parallel()

	// Java Random(42).nextLong() computed via LCG algorithm.
	rng := newJavaRandom(42)
	got := rng.nextLong()
	want := int64(-5025562857975149833)
	if got != want {
		t.Fatalf("Random(42).nextLong() = %d, want %d", got, want)
	}
}

// --- FHT helper tests ---

func TestLargestPow2LE(t *testing.T) {
	t.Parallel()

	tests := []struct {
		n    int
		want int
	}{
		{1, 1},
		{2, 2},
		{3, 2},
		{4, 4},
		{5, 4},
		{7, 4},
		{8, 8},
		{9, 8},
		{15, 8},
		{16, 16},
		{100, 64},
		{128, 128},
		{768, 512},
		{1024, 1024},
	}
	for _, tt := range tests {
		got := largestPow2LE(tt.n)
		if got != tt.want {
			t.Errorf("largestPow2LE(%d) = %d, want %d", tt.n, got, tt.want)
		}
	}
}

// --- FHT-KAC Rotator tests ---

func TestFhtKacRotatorOrthogonality(t *testing.T) {
	t.Parallel()

	// An orthogonal rotation preserves vector norms (distances).
	// ||rotate(v)||^2 should equal ||v||^2.
	dims := 128
	rounds := 10
	rotator := newFhtKacRotator(12345, dims, rounds)

	rng := rand.New(rand.NewSource(42))
	for trial := 0; trial < 50; trial++ {
		v := randomVector(rng, dims)
		origNorm := dot(v, v)
		rotated := rotator.apply(v)
		rotatedNorm := dot(rotated, rotated)

		relErr := math.Abs(origNorm-rotatedNorm) / origNorm
		if relErr > 1e-10 {
			t.Fatalf("trial %d: norm not preserved: original=%v, rotated=%v, relErr=%v",
				trial, origNorm, rotatedNorm, relErr)
		}
	}
}

func TestFhtKacRotatorInverse(t *testing.T) {
	t.Parallel()

	// transposedApply should be the exact inverse of apply.
	dims := 64
	rounds := 10
	rotator := newFhtKacRotator(99999, dims, rounds)

	rng := rand.New(rand.NewSource(77))
	for trial := 0; trial < 50; trial++ {
		v := randomVector(rng, dims)
		rotated := rotator.apply(v)
		recovered := rotator.transposedApply(rotated)

		for i := range v {
			if math.Abs(v[i]-recovered[i]) > 1e-10 {
				t.Fatalf("trial %d, dim %d: inverse failed: original=%v, recovered=%v",
					trial, i, v[i], recovered[i])
			}
		}
	}
}

func TestFhtKacRotatorDistancePreservation(t *testing.T) {
	t.Parallel()

	// Rotation preserves pairwise distances: ||rotate(a) - rotate(b)||^2 == ||a - b||^2.
	dims := 128
	rounds := 10
	rotator := newFhtKacRotator(54321, dims, rounds)

	rng := rand.New(rand.NewSource(88))
	for trial := 0; trial < 30; trial++ {
		a := randomVector(rng, dims)
		b := randomVector(rng, dims)

		origDist := exactEuclideanSquare(a, b)
		ra := rotator.apply(a)
		rb := rotator.apply(b)
		rotatedDist := exactEuclideanSquare(ra, rb)

		relErr := math.Abs(origDist-rotatedDist) / origDist
		if relErr > 1e-10 {
			t.Fatalf("trial %d: distance not preserved: original=%v, rotated=%v, relErr=%v",
				trial, origDist, rotatedDist, relErr)
		}
	}
}

func TestFhtKacRotatorDeterministic(t *testing.T) {
	t.Parallel()

	// Same seed produces same rotation.
	dims := 32
	rotator1 := newFhtKacRotator(42, dims, 10)
	rotator2 := newFhtKacRotator(42, dims, 10)

	v := make([]float64, dims)
	for i := range v {
		v[i] = float64(i)
	}

	r1 := rotator1.apply(v)
	r2 := rotator2.apply(v)

	for i := range r1 {
		if r1[i] != r2[i] {
			t.Fatalf("dim %d: rotator outputs differ: %v vs %v", i, r1[i], r2[i])
		}
	}
}

func TestFhtKacRotatorDifferentSeeds(t *testing.T) {
	t.Parallel()

	// Different seeds produce different rotations.
	dims := 32
	rotator1 := newFhtKacRotator(42, dims, 10)
	rotator2 := newFhtKacRotator(43, dims, 10)

	v := make([]float64, dims)
	for i := range v {
		v[i] = float64(i) + 1.0
	}

	r1 := rotator1.apply(v)
	r2 := rotator2.apply(v)

	allSame := true
	for i := range r1 {
		if r1[i] != r2[i] {
			allSame = false
			break
		}
	}
	if allSame {
		t.Fatal("different seeds produced identical rotations")
	}
}

func TestFhtKacRotatorNonPowerOf2Dims(t *testing.T) {
	t.Parallel()

	// Non-power-of-2 dimensions (e.g. 768, 100) should work correctly.
	// FHT operates on the largest power-of-2 block, Givens pairs remaining.
	for _, dims := range []int{3, 5, 7, 10, 100, 768} {
		rotator := newFhtKacRotator(12345, dims, 10)
		v := make([]float64, dims)
		for i := range v {
			v[i] = float64(i) + 0.5
		}

		origNorm := dot(v, v)
		rotated := rotator.apply(v)
		rotatedNorm := dot(rotated, rotated)

		relErr := math.Abs(origNorm-rotatedNorm) / origNorm
		if relErr > 1e-9 {
			t.Errorf("dims=%d: norm not preserved: relErr=%v", dims, relErr)
		}

		// Test inverse.
		recovered := rotator.transposedApply(rotated)
		for i := range v {
			if math.Abs(v[i]-recovered[i]) > 1e-9 {
				t.Errorf("dims=%d, dim %d: inverse failed", dims, i)
				break
			}
		}
	}
}

// --- Transform tests ---

func TestHNSWTransformApplyInverse(t *testing.T) {
	t.Parallel()

	dims := 64
	centroid := make([]float64, dims)
	for i := range centroid {
		centroid[i] = -float64(i) * 0.01 // negated centroid
	}

	transform := newHNSWTransform(42, centroid, dims, false)

	rng := rand.New(rand.NewSource(99))
	for trial := 0; trial < 20; trial++ {
		v := randomVector(rng, dims)
		transformed := transform.apply(v)
		recovered := transform.invertedApply(transformed)

		for i := range v {
			if math.Abs(v[i]-recovered[i]) > 1e-9 {
				t.Fatalf("trial %d, dim %d: transform not invertible: original=%v, recovered=%v",
					trial, i, v[i], recovered[i])
			}
		}
	}
}

func TestHNSWTransformNilIsIdentity(t *testing.T) {
	t.Parallel()

	var transform *hnswTransform // nil
	v := []float64{1, 2, 3}

	result := transform.apply(v)
	if &result[0] != &v[0] {
		t.Fatal("nil transform should return input slice directly")
	}

	result2 := transform.invertedApply(v)
	if &result2[0] != &v[0] {
		t.Fatal("nil transform invertedApply should return input slice directly")
	}
}

func TestHNSWTransformSeedNeg1IsNil(t *testing.T) {
	t.Parallel()

	// rotatorSeed = -1 means no rotation (identity).
	transform := newHNSWTransform(-1, nil, 64, false)
	if transform != nil {
		t.Fatal("seed -1 should return nil transform")
	}
}

func TestHNSWTransformZeroCentroid(t *testing.T) {
	t.Parallel()

	// With zero centroid, transform is pure rotation.
	dims := 32
	centroid := make([]float64, dims)
	transform := newHNSWTransform(42, centroid, dims, false)

	v := make([]float64, dims)
	for i := range v {
		v[i] = float64(i)
	}

	// Pure rotation should preserve norm.
	origNorm := dot(v, v)
	transformed := transform.apply(v)
	transformedNorm := dot(transformed, transformed)

	relErr := math.Abs(origNorm-transformedNorm) / origNorm
	if relErr > 1e-10 {
		t.Fatalf("zero centroid transform should preserve norm: relErr=%v", relErr)
	}
}

func TestHNSWTransformWithNormalization(t *testing.T) {
	t.Parallel()

	// With normalize=true (cosine metric), the input is normalized before rotation.
	dims := 32
	centroid := make([]float64, dims) // zero centroid
	transform := newHNSWTransform(42, centroid, dims, true)

	v := make([]float64, dims)
	for i := range v {
		v[i] = float64(i) + 1.0
	}

	transformed := transform.apply(v)

	// Normalized then rotated → norm should be ~1.0 (rotation preserves norm).
	transformedNorm := math.Sqrt(dot(transformed, transformed))
	if math.Abs(transformedNorm-1.0) > 1e-10 {
		t.Fatalf("normalized + rotated vector should have norm 1.0, got %v", transformedNorm)
	}
}

// --- Cross-validation with Java FhtKacRotator ---

func TestFhtKacRotatorMatchesJava(t *testing.T) {
	t.Parallel()

	// Reference values computed by Java's FhtKacRotator(42, 8, 3).operate([1..8]).
	// Verified via direct Java execution.
	rotator := newFhtKacRotator(42, 8, 3)
	v := []float64{1, 2, 3, 4, 5, 6, 7, 8}
	result := rotator.apply(v)

	javaExpected := []float64{1.0, -3.0, 2.0, 4.0, -8.0, 6.0, -7.0, -5.0}
	for i := range result {
		if math.Abs(result[i]-javaExpected[i]) > 1e-10 {
			t.Fatalf("dim %d: Go=%v, Java=%v", i, result[i], javaExpected[i])
		}
	}
}

func TestFhtKacRotatorMatchesJava4D(t *testing.T) {
	t.Parallel()

	// Reference values computed by Java's FhtKacRotator(42, 4, 3).operate([1..4]).
	rotator := newFhtKacRotator(42, 4, 3)
	v := []float64{1, 2, 3, 4}
	result := rotator.apply(v)

	javaExpected := []float64{
		2.121320343559642, 0.707106781186547,
		-4.949747468305831, -0.707106781186548,
	}
	for i := range result {
		if math.Abs(result[i]-javaExpected[i]) > 1e-12 {
			t.Fatalf("dim %d: Go=%.15f, Java=%.15f", i, result[i], javaExpected[i])
		}
	}
}

// --- End-to-end: transform + RaBitQ quality test ---

func TestTransformImprovesRaBitQEstimation(t *testing.T) {
	t.Parallel()

	// Verify that applying FHT-KAC rotation before RaBitQ encoding
	// produces reasonable distance estimates.
	dims := 128
	numExBits := 4
	seed := int64(12345)
	rotator := newFhtKacRotator(seed, dims, 10)

	rng := rand.New(rand.NewSource(42))
	numTrials := 100
	withinBounds := 0

	for trial := 0; trial < numTrials; trial++ {
		v := randomVector(rng, dims)
		q := randomVector(rng, dims)

		// Rotate both vectors.
		rv := rotator.apply(v)
		rq := rotator.apply(q)

		// Encode rotated v.
		quantizer := rabitq.NewRaBitQuantizer(rabitq.MetricEuclidean, numExBits)
		encoded := quantizer.Encode(rv)

		// Estimate distance between rotated q and encoded rotated v.
		estimator := rabitq.NewRaBitEstimator(rabitq.MetricEuclidean, numExBits)
		result := estimator.EstimateDistance(rq, encoded)

		// True distance (rotation preserves distances).
		trueDist := exactEuclideanSquare(v, q)

		if trueDist >= result.Distance-result.Error && trueDist < result.Distance+result.Error {
			withinBounds++
		}
	}

	fractionWithin := float64(withinBounds) / float64(numTrials)
	if fractionWithin < 0.65 {
		t.Fatalf("expected >65%% within bounds with rotation, got %.1f%%", fractionWithin*100)
	}
}
