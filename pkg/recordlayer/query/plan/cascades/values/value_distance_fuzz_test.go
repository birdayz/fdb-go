package values

import (
	"math"
	"testing"
)

// FuzzDistanceValue_NumericProperties fuzzes the 4 distance metrics
// against random vector pairs. Pins:
//
//  1. No panic on any byte input.
//  2. Result is always a valid float64 (no NaN, no Inf — Java's
//     metric eval is similarly bounded).
//  3. Symmetry (d(a, b) == d(b, a)) — holds for Euclidean,
//     EuclideanSquare, Cosine; NOT for DotProduct (which is
//     direction-dependent only when sign matters; -(a·b) is
//     symmetric so this property holds too).
//  4. Self-distance is 0 for Euclidean / EuclideanSquare /
//     Cosine; NOT for DotProduct (which is -|a|² for self-pair).
//  5. Non-negative for Euclidean / EuclideanSquare; bounded
//     [0, 2] for Cosine.
//
// Random vectors built from raw bytes (interpreted as 4 float64
// elements per side, byte-swept).
func FuzzDistanceValue_NumericProperties(f *testing.F) {
	// Seed with happy-path vectors covering each metric's domain.
	f.Add(int64(1), int64(0), int64(0), int64(1)) // unit vectors
	f.Add(int64(5), int64(5), int64(5), int64(5)) // identical
	f.Add(int64(0), int64(0), int64(0), int64(0)) // zero vectors
	f.Add(int64(-3), int64(4), int64(3), int64(-4))

	f.Fuzz(func(t *testing.T, ax, ay, bx, by int64) {
		// Convert to float64 to avoid integer-only arithmetic.
		// Clamp ranges to avoid Inf accumulation: [-1000, 1000].
		clamp := func(x int64) float64 {
			if x > 1000 {
				return 1000
			}
			if x < -1000 {
				return -1000
			}
			return float64(x)
		}
		a := []float64{clamp(ax), clamp(ay)}
		b := []float64{clamp(bx), clamp(by)}

		// Test each metric.
		for _, op := range []DistanceOperator{
			DistanceEuclidean,
			DistanceEuclideanSquare,
			DistanceCosine,
			DistanceDotProduct,
		} {
			d1 := NewDistanceValue(op, LiteralValue(a), LiteralValue(b))
			r1, ok := d1.Evaluate(nil).(float64)
			if !ok {
				t.Fatalf("op=%v: Evaluate returned non-float64", op)
			}
			if math.IsNaN(r1) || math.IsInf(r1, 0) {
				t.Fatalf("op=%v vec(%v, %v): result NaN/Inf: %v", op, a, b, r1)
			}

			// Symmetry: d(a, b) == d(b, a) for all 4 metrics.
			d2 := NewDistanceValue(op, LiteralValue(b), LiteralValue(a))
			r2, _ := d2.Evaluate(nil).(float64)
			if math.Abs(r1-r2) > 1e-9 {
				t.Fatalf("op=%v: symmetry broken d(a,b)=%v d(b,a)=%v",
					op, r1, r2)
			}

			// Self-distance: d(a, a) is 0 for L2 / L2-sq / Cosine
			// (assuming non-zero vector); not for DotProduct.
			dSelf := NewDistanceValue(op, LiteralValue(a), LiteralValue(a))
			rSelf, _ := dSelf.Evaluate(nil).(float64)
			switch op {
			case DistanceEuclidean, DistanceEuclideanSquare:
				if math.Abs(rSelf) > 1e-9 {
					t.Fatalf("op=%v: self-distance = %v, want 0", op, rSelf)
				}
			case DistanceCosine:
				// Self-distance is 0 if vector is non-zero, 1 if zero
				// (cosine zero-vector fallback returns 1).
				normA := a[0]*a[0] + a[1]*a[1]
				if normA > 0 && math.Abs(rSelf) > 1e-9 {
					t.Fatalf("op=%v: self-distance non-zero vector = %v, want 0", op, rSelf)
				}
			}

			// Non-negativity for Euclidean variants.
			switch op {
			case DistanceEuclidean, DistanceEuclideanSquare:
				if r1 < 0 {
					t.Fatalf("op=%v: negative distance %v", op, r1)
				}
			case DistanceCosine:
				if r1 < -1e-9 || r1 > 2+1e-9 {
					t.Fatalf("op=%v: out-of-range %v, want [0, 2]", op, r1)
				}
			}
		}
	})
}
