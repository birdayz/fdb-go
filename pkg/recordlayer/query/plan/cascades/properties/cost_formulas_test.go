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
		got := NestedLoopJoinCost(outer, zeroInner, 1, 0)
		if got.Cardinality != 0 {
			t.Fatalf("Cardinality = %v, want 0", got.Cardinality)
		}
	})

	t.Run("zero outer", func(t *testing.T) {
		t.Parallel()
		zeroOuter := Cost{Cardinality: 0, CPU: 3}
		inner := Cost{Cardinality: 500, CPU: 50}
		got := NestedLoopJoinCost(zeroOuter, inner, 1, 0)
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

// TestNestedLoopJoinCost_UniqueKeyEqualityJoin_MatchesFlatMapCost pins the
// exact production defect a review found: for a unique/full-PK equality
// join, the FlatMap shape correctly gives outerCard*1 = outerCard (a
// unique-key equality join returns at most one inner row per outer row), but
// NestedLoopJoinCost applied a flat FilterSelectivity (0.5) to the RAW inner
// scan, giving outerCard*innerCard*0.5 — a 10-row outer joined via a unique
// key against a 1000-row inner was costed at 5000 rows by the materialized
// shape versus the true 10, a 500x overestimate that also made the two
// physical shapes of the IDENTICAL logical join disagree — precisely the
// invariant compareJoinOrdering (planning_cost_model.go, RFC-192) requires
// to hold for its total-preorder guarantee.
//
// uniqueKeyConjuncts=1 is the caller's proof (plans.NestedLoopJoinUniqueKeyConjuncts
// in production) that the join's one predicate is a full equality bind on
// the inner's own unique key.
func TestNestedLoopJoinCost_UniqueKeyEqualityJoin_MatchesFlatMapCost(t *testing.T) {
	t.Parallel()
	outer := Cost{Cardinality: 10, CPU: 1}
	inner := Cost{Cardinality: 1000, CPU: 5}

	nljGot := NestedLoopJoinCost(outer, inner, 1, 1)
	if nljGot.Cardinality != 10 {
		t.Fatalf("NestedLoopJoinCost.Cardinality = %v, want 10 (outerCard — a unique-key equality join returns at most one inner row per outer row)", nljGot.Cardinality)
	}

	// The FlatMap shape of the SAME logical join: its inner already applied
	// the identical equality bind at the scan/index leaf, so inner.Cardinality
	// there is 1 (a proven point probe — see plans/cost.go's
	// isProvablePointProbe), not the raw 1000 NestedLoopJoinCost receives.
	flatMapGot := FlatMapCost(outer, Cost{Cardinality: 1, CPU: FetchCPU})
	if flatMapGot.Cardinality != nljGot.Cardinality {
		t.Fatalf("the two physical shapes of the SAME logical join disagree: NestedLoopJoinCost=%v FlatMapCost=%v, want equal", nljGot.Cardinality, flatMapGot.Cardinality)
	}
}

// TestNestedLoopJoinCost_NonUniqueEqualityJoin_FlatSelectivityUnaffected
// pins the gate: when uniqueness cannot be proven (uniqueKeyConjuncts=0),
// NestedLoopJoinCost must apply EXACTLY the old flat FilterSelectivity
// formula — the correction is an ADDITION for a proven case, never a
// replacement of the honest fallback for a non-unique join (e.g.
// `category IN (...)` against a non-unique secondary index, or a plain
// cross-join predicate).
func TestNestedLoopJoinCost_NonUniqueEqualityJoin_FlatSelectivityUnaffected(t *testing.T) {
	t.Parallel()
	outer := Cost{Cardinality: 10, CPU: 1}
	inner := Cost{Cardinality: 1000, CPU: 5}

	got := NestedLoopJoinCost(outer, inner, 1, 0)
	want := outer.Cardinality * inner.Cardinality * FilterSelectivity
	if got.Cardinality != want {
		t.Fatalf("Cardinality = %v, want %v (unchanged flat-selectivity formula)", got.Cardinality, want)
	}
}
