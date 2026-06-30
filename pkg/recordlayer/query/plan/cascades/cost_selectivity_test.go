package cascades

import (
	"math"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
)

func eqBound(t *testing.T) *predicates.ComparisonRange {
	t.Helper()
	c := predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(5))
	return predicates.EmptyComparisonRange().Merge(&c).Range
}

func rangeBound(t *testing.T) *predicates.ComparisonRange {
	t.Helper()
	c := predicates.NewLiteralComparison(predicates.ComparisonGreaterThan, int64(10))
	return predicates.EmptyComparisonRange().Merge(&c).Range
}

// TestBoundSelectivity is the SINGLE numeric pin for the equality-vs-range scan
// bound cost, shared by all three scan-cost sites (physicalScanWrapper,
// physicalIndexScanWrapper, scanLikeCost) via boundSelectivity. It catches a
// per-site revert to the wrong constant that the constant-ordering and plan tests
// might not: each equality bound MUST contribute EqualityBoundSelectivity and each
// range bound RangeSelectivity.
func TestBoundSelectivity(t *testing.T) {
	t.Parallel()
	eq := eqBound(t)
	rng := rangeBound(t)

	for _, tc := range []struct {
		name      string
		comps     []*predicates.ComparisonRange
		wantSel   float64
		wantBound int
		wantAllEq bool
	}{
		{"single_equality", []*predicates.ComparisonRange{eq}, properties.EqualityBoundSelectivity, 1, true},
		{"single_range", []*predicates.ComparisonRange{rng}, properties.RangeSelectivity, 1, false},
		{"eq_and_range", []*predicates.ComparisonRange{eq, rng}, properties.EqualityBoundSelectivity * properties.RangeSelectivity, 2, false},
		{"two_equalities", []*predicates.ComparisonRange{eq, eq}, properties.EqualityBoundSelectivity * properties.EqualityBoundSelectivity, 2, true},
		{"empty_skipped", []*predicates.ComparisonRange{predicates.EmptyComparisonRange(), nil}, 1.0, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sel, n, allEq := boundSelectivity(tc.comps)
			// Tolerance, not ==: the runtime does sequential float64 multiplies
			// while the want is a compile-time constant product (different ULPs).
			if math.Abs(sel-tc.wantSel) > 1e-12 {
				t.Errorf("sel=%v, want %v", sel, tc.wantSel)
			}
			if n != tc.wantBound {
				t.Errorf("numBound=%v, want %v", n, tc.wantBound)
			}
			if allEq != tc.wantAllEq {
				t.Errorf("allEquality=%v, want %v", allEq, tc.wantAllEq)
			}
		})
	}
}

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
