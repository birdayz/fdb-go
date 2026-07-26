package properties

import "testing"

// TestFlatMapCost_ZeroInnerCardinalityStaysZero pins the fix for a real
// defect: FlatMapCost used to treat an exact zero Cardinality as "unknown"
// and substitute LeafScanCardinality, on the assumption that a real operator
// never legitimately produces zero rows. That assumption is false —
// RecordQueryLimitPlan.HintCost returns Cardinality 0 for LIMIT 0
// (plan/plans/cost.go) — so a FlatMap whose inner is a LIMIT-0 subtree, the
// cheapest possible join, was costed as outerCard*LeafScanCardinality, the
// most expensive shape on the plan instead of the correct zero-row join.
func TestFlatMapCost_ZeroInnerCardinalityStaysZero(t *testing.T) {
	t.Parallel()

	outer := Cost{Cardinality: 500, CPU: 50}
	zeroInner := Cost{Cardinality: 0, CPU: 3} // e.g. RecordQueryLimitPlan HintCost for LIMIT 0

	got := FlatMapCost(outer, zeroInner)
	if got.Cardinality != 0 {
		t.Fatalf("Cardinality = %v, want 0 (a genuinely empty inner must yield a zero-row join, not outerCard*LeafScanCardinality)", got.Cardinality)
	}
}

// TestFlatMapCost_ZeroOuterCardinalityStaysZero is the mirror case: a
// LIMIT-0 (or empty-IN-list) OUTER must also propagate as an exact-zero
// join, not be inflated by the same substitution.
func TestFlatMapCost_ZeroOuterCardinalityStaysZero(t *testing.T) {
	t.Parallel()

	zeroOuter := Cost{Cardinality: 0, CPU: 3}
	inner := Cost{Cardinality: 500, CPU: 50}

	got := FlatMapCost(zeroOuter, inner)
	if got.Cardinality != 0 {
		t.Fatalf("Cardinality = %v, want 0", got.Cardinality)
	}
}

// TestNestedLoopJoinCost_ZeroCardinalityStaysZero is FlatMapCost's sibling
// test for the materialized NestedLoopJoinCost formula, which had the
// identical zero-guard conflation.
func TestNestedLoopJoinCost_ZeroCardinalityStaysZero(t *testing.T) {
	t.Parallel()

	t.Run("zero inner", func(t *testing.T) {
		t.Parallel()
		outer := Cost{Cardinality: 500, CPU: 50}
		zeroInner := Cost{Cardinality: 0, CPU: 3}
		got := NestedLoopJoinCost(outer, zeroInner, 1)
		if got.Cardinality != 0 {
			t.Fatalf("Cardinality = %v, want 0", got.Cardinality)
		}
	})

	t.Run("zero outer", func(t *testing.T) {
		t.Parallel()
		zeroOuter := Cost{Cardinality: 0, CPU: 3}
		inner := Cost{Cardinality: 500, CPU: 50}
		got := NestedLoopJoinCost(zeroOuter, inner, 1)
		if got.Cardinality != 0 {
			t.Fatalf("Cardinality = %v, want 0", got.Cardinality)
		}
	})
}

// TestInMemorySortCost_ZeroCardinalityStaysZero pins a second instance of the
// same zero-as-sentinel conflation, found while auditing every zero-guard in
// this file: InMemorySortCost floored its LOG argument at 1 (needed — log2 is
// undefined below that) but then returned the FLOORED value as Cardinality
// too, so a LIMIT-0 child sorting zero rows was reported as sorting one.
func TestInMemorySortCost_ZeroCardinalityStaysZero(t *testing.T) {
	t.Parallel()
	got := InMemorySortCost(Cost{Cardinality: 0, CPU: 5})
	if got.Cardinality != 0 {
		t.Fatalf("Cardinality = %v, want 0 (a LIMIT-0 child sorts zero rows)", got.Cardinality)
	}
	if got.CPU != 5 {
		t.Fatalf("CPU = %v, want 5 (child.CPU + 0*SortCPU*logN == child.CPU)", got.CPU)
	}
}

// TestFlatMapCost_NonZeroCardinalityUnaffected confirms the fix is narrowly
// scoped: a genuinely unspecified/absent Cost (the zero VALUE, {}) still
// combines the same way it always has for non-zero inputs — this test would
// catch an over-correction that broke the ordinary outerCard*innerCard
// multiplication.
func TestFlatMapCost_NonZeroCardinalityUnaffected(t *testing.T) {
	t.Parallel()
	outer := Cost{Cardinality: 10, CPU: 1}
	inner := Cost{Cardinality: 20, CPU: 2}
	got := FlatMapCost(outer, inner)
	if got.Cardinality != 200 {
		t.Fatalf("Cardinality = %v, want 200 (10*20)", got.Cardinality)
	}
}
