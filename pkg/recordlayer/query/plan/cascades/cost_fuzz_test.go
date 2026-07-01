package cascades

import (
	"math"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
)

// FuzzCostSanity pins the B4 cost-model invariant that survives the
// production task-stack driver: the best-member cost is finite and
// non-negative on every reachable Reference state (before AND after
// exploration) — i.e. the cost function never produces NaN / Inf /
// negative values on optimised trees.
//
// The legacy FixpointApply-era fuzzer additionally asserted best-cost
// MONOTONICITY across iterations. That invariant was an artifact of
// the memo-less legacy fixpoint engine and does NOT hold under the
// production driver: RFC-037 cross-group memo merges re-point a child
// group's canonical member list, so EstimateCost's documented
// first-member recursion (properties/cost.go) can price the SAME
// unchanged parent expression higher after a merge. Plan SELECTION is
// unaffected — alternatives are ranked through the same merged child
// groups, and extraction uses winners/GetBest — so absolute growth of
// the approximation is not a correctness signal.
func FuzzCostSanity(f *testing.F) {
	f.Add([]byte{0, 1, 2, 3, 4, 5})
	f.Add(make([]byte, 8))
	f.Fuzz(func(t *testing.T, b []byte) {
		if len(b) < 4 {
			return
		}
		expr := buildFuzzExpression(b, 0, 0)
		ref := expressions.InitialOf(expr)
		rules := selectRules(b)

		if c := properties.BestRefCost(ref).Total(); !goodCost(c) {
			t.Fatalf("initial best cost not finite/non-negative: %v", c)
		}

		p := NewPlanner(rules, nil)
		if _, conv := exploreRewriting(p, ref); !conv {
			t.Fatal("exploration did not converge — possible non-terminating rule interaction")
		}

		if c := properties.BestRefCost(ref).Total(); !goodCost(c) {
			t.Fatalf("post-exploration best cost not finite/non-negative: %v (members=%d)",
				c, len(ref.Members()))
		}
	})
}

func goodCost(c float64) bool {
	return !math.IsNaN(c) && !math.IsInf(c, 0) && c >= 0
}
