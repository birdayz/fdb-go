package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
)

// TestCostSelectivity_EqualityBeatsRange pins the RFC-164 COST-SELECTIVITY
// invariant: an equality index-scan bound MUST be costed as more selective than
// a range bound, i.e. EqualityBoundSelectivity < RangeSelectivity. If a future
// edit re-inverts these (as the original FilterSelectivity=0.5 > RangeSelectivity=0.33
// did), an equality probe looks less selective than a range probe and the planner
// picks the wrong index. This is the constant-level sentinel; the plan-level proof
// is TestCostSelectivity_PrefersEqualityIndex in the embedded package.
func TestCostSelectivity_EqualityBeatsRange(t *testing.T) {
	t.Parallel()
	if !(properties.EqualityBoundSelectivity < properties.RangeSelectivity) {
		t.Fatalf("equality bound must be MORE selective than a range bound: "+
			"EqualityBoundSelectivity=%v must be < RangeSelectivity=%v",
			properties.EqualityBoundSelectivity, properties.RangeSelectivity)
	}
	// A selectivity is a fraction of rows retained: in (0, 1].
	if properties.EqualityBoundSelectivity <= 0 || properties.EqualityBoundSelectivity > 1 {
		t.Fatalf("EqualityBoundSelectivity=%v out of (0,1]", properties.EqualityBoundSelectivity)
	}
}
