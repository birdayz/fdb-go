package cascades

import (
	"math"
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/properties"
)

// FuzzCostMonotonicity pins two B4 cost-model invariants:
//
//  1. EstimateCost over the BEST member is non-increasing across
//     fixpoint iterations: optimisation can only shrink the cheapest
//     member's cost, never grow it. (A rule that yielded a strictly-
//     more-expensive expression as the cheapest member would be a
//     correctness regression.)
//
//  2. The best-member's cost is finite and non-negative for every
//     reachable Reference state — i.e. the cost function never
//     produces NaN / Inf / negative values on optimised trees.
//
// The fuzzer reuses the FuzzFixpointApply tree builder (random
// expression shapes from the byte stream) and applies a random
// rule subset, but optimises in single-iteration steps and asserts
// monotonicity at each step.
func FuzzCostMonotonicity(f *testing.F) {
	f.Add([]byte{0, 1, 2, 3, 4, 5})
	f.Add(make([]byte, 8))
	f.Fuzz(func(t *testing.T, b []byte) {
		if len(b) < 4 {
			return
		}
		expr := buildFuzzExpression(b, 0, 0)
		ref := expressions.InitialOf(expr)
		rules := selectRules(b)

		prevBest := properties.BestRefCost(ref).Total()
		if !goodCost(prevBest) {
			t.Fatalf("initial best cost not finite/non-negative: %v", prevBest)
		}

		// Drive the optimiser one iteration at a time and assert
		// monotonicity at each step. 50 iters is the FixpointApply
		// default cap — convergence is allowed before then.
		for iter := 0; iter < 50; iter++ {
			progress, _ := FixpointApply(rules, ref, 1)
			best := properties.BestRefCost(ref).Total()
			if !goodCost(best) {
				t.Fatalf("iter %d best cost not finite/non-negative: %v (members=%d)",
					iter, best, len(ref.Members()))
			}
			// Monotonicity tolerance: floating-point recomputation
			// can introduce tiny non-determinism, so allow a 1e-9
			// relative slop. Without slop, ULP-level wobble would
			// flag spurious failures.
			tol := math.Max(1e-9, math.Abs(prevBest)*1e-9)
			if best > prevBest+tol {
				t.Fatalf("iter %d: best cost grew from %v to %v (rule yielded a more expensive cheapest-member; tol=%v)",
					iter, prevBest, best, tol)
			}
			if progress == 0 {
				return // converged
			}
			prevBest = best
		}
	})
}

func goodCost(c float64) bool {
	return !math.IsNaN(c) && !math.IsInf(c, 0) && c >= 0
}
