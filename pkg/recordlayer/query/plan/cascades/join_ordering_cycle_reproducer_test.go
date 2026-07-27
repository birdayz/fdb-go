package cascades

// Pins the fix for the compareJoinOrdering non-transitivity documented at
// planning_cost_model.go:1450 (TODO.md CQ-24) and the (now deleted)
// pair-dependent joinShapesDiffer CPU branch.
//
// The comparator used to pick its metric from a property of the PAIR being
// compared (joinShapesDiffer(planA, planB)): different root join shapes
// (FlatMap vs NestedLoopJoin) compared on raw CPU, same shapes fell through
// to Cost.Less (Total, then Cardinality). That is not a function of a single
// Cost value, so it was not guaranteed transitive, and a concrete 3-cycle was
// constructed from the real cost formulas (see cost_model_total_preorder_test.go's
// joinOrderingCorpus for the cycle numbers, and TODO.md CQ-24 for the
// derivation).
//
// The fix: FlatMapCost.Cardinality now uses the same outerCard*innerCard
// join-cardinality form as NestedLoopJoinCost.Cardinality (cost_formulas.go),
// so Cardinality is a true GROUP property — equal for both shapes given the
// same outer/inner inputs — and compareJoinOrdering ranks every pair with a
// single metric (Cost.Less), no shape-conditioned branch.

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// fullScanLeaf builds a bare, comparison-free (full) scan of recordType,
// whose concrete cost is deterministic given stats[recordType] = total:
// Cardinality = 0.9*total, CPU = 0.045*total (scanLikeCost, sel=1, no
// equality bind).
func fullScanLeaf(recordType string) *plans.RecordQueryScanPlan {
	return plans.NewRecordQueryScanPlan([]string{recordType}, values.UnknownType, false)
}

// TestCompareJoinOrdering_ShapeMismatchTransitivityHolds constructs the exact
// three physical plans that produced the shape-mismatch 3-cycle before the
// fix — two FlatMaps (A, C) and one materialized NestedLoopJoin (B), with
// leaf record-type cardinalities chosen so the pre-fix CPU-only branch (for
// the differently-shaped pairs A/B and B/C) and the pre-fix Total fallback
// (for the same-shaped pair A/C) disagreed cyclically:
//
//	A = FlatMap(outer T=1, inner T=1000)
//	B = NLJ(outer T=1, inner T=220)
//	C = FlatMap(outer T=100, inner T=1)
//
// Pre-fix: CPU order C(15.795) < B(24.9885) < A(36.5715) decided A-vs-B and
// B-vs-C (different shapes), while Total order A(36.9765) < C(56.295) decided
// A-vs-C (same shape) — so C<B<A but also A<C: a non-transitive 3-cycle.
//
// Post-fix: FlatMapCost.Cardinality now includes the inner cardinality, so
// A's inner (1000 rows, a non-probe re-scan) is priced far more expensively
// than before, and a single Total-based metric orders all three consistently.
func TestCompareJoinOrdering_ShapeMismatchTransitivityHolds(t *testing.T) {
	t.Parallel()

	aliasOuter := values.NamedCorrelationIdentifier("O")
	aliasInner := values.NamedCorrelationIdentifier("I")
	resVal := values.NewQuantifiedObjectValue(values.UniqueCorrelationIdentifier())

	stats := properties.MapStatistics{PerType: map[string]float64{
		"A_outer": 1, "A_inner": 1000,
		"B_outer": 1, "B_inner": 220,
		"C_outer": 100, "C_inner": 1,
	}}

	planA := plans.NewRecordQueryFlatMapPlan(
		fullScanLeaf("A_outer"), fullScanLeaf("A_inner"), aliasOuter, aliasInner, resVal, false)

	pred := predicates.NewComparisonPredicate(
		values.NewFieldValue(values.NewQuantifiedObjectValue(aliasOuter), "flag", values.UnknownType),
		predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(1)),
	)
	planB := plans.NewRecordQueryNestedLoopJoinPlan(
		fullScanLeaf("B_outer"), fullScanLeaf("B_inner"),
		[]predicates.QueryPredicate{pred}, plans.JoinInner, "O", "I", resVal)

	planC := plans.NewRecordQueryFlatMapPlan(
		fullScanLeaf("C_outer"), fullScanLeaf("C_inner"), aliasOuter, aliasInner, resVal, false)

	costA := concretePlanCost(planA, stats, nil)
	costB := concretePlanCost(planB, stats, nil)
	costC := concretePlanCost(planC, stats, nil)
	t.Logf("costA (FlatMap)  = %+v (Total=%v)", costA, costA.Total())
	t.Logf("costB (NLJ)      = %+v (Total=%v)", costB, costB.Total())
	t.Logf("costC (FlatMap)  = %+v (Total=%v)", costC, costC.Total())

	// A and C are both FlatMaps, B is the materialized NLJ — the shape
	// pairing the deleted joinShapesDiffer used to gate its CPU-only branch
	// on. planA/planB/planC's declared types already pin those shapes
	// (*RecordQueryFlatMapPlan, *RecordQueryNestedLoopJoinPlan,
	// *RecordQueryFlatMapPlan) — nothing further to assert here now that the
	// classification helper is gone.

	cmpAC := compareJoinOrdering(planA, planC, stats, nil)
	cmpAB := compareJoinOrdering(planA, planB, stats, nil)
	cmpBC := compareJoinOrdering(planB, planC, stats, nil)
	t.Logf("compareJoinOrdering(A,C) = %d", cmpAC)
	t.Logf("compareJoinOrdering(A,B) = %d", cmpAB)
	t.Logf("compareJoinOrdering(B,C) = %d", cmpBC)

	less := func(x, y plans.RecordQueryPlan) bool { return compareJoinOrdering(x, y, stats, nil) < 0 }

	// The pre-fix cycle was: A<C, B<A, C<B all simultaneously true. Assert
	// that does NOT hold post-fix.
	cycle := less(planA, planC) && less(planB, planA) && less(planC, planB)
	if cycle {
		t.Fatalf("CYCLE STILL PRESENT: A<C=%v B<A=%v C<B=%v all true simultaneously — "+
			"compareJoinOrdering is still non-transitive on this triple",
			less(planA, planC), less(planB, planA), less(planC, planB))
	}

	// Stronger: the three pairwise comparisons must form a genuine strict
	// total order over {A, B, C} — exactly one of the three plans must be
	// less than both others, one greater than both, and the relation must be
	// consistent (no "beats one, loses to the other" contradiction for any
	// pair against the third).
	names := map[plans.RecordQueryPlan]string{planA: "A", planB: "B", planC: "C"}
	all := []plans.RecordQueryPlan{planA, planB, planC}
	for _, x := range all {
		for _, y := range all {
			for _, z := range all {
				if x == y || y == z || x == z {
					continue
				}
				if less(x, y) && less(y, z) && !less(x, z) {
					t.Errorf("TRANSITIVITY VIOLATED: %s < %s < %s but NOT %s < %s",
						names[x], names[y], names[z], names[x], names[z])
				}
			}
		}
	}

	// The linear-fold winner (Reference.GetBest's algorithm: best := all[0];
	// for m in all[1:] { if less(m, best) { best = m } }) must be the SAME
	// regardless of insertion order once the comparator is transitive.
	fold := func(order []plans.RecordQueryPlan) plans.RecordQueryPlan {
		best := order[0]
		for _, m := range order[1:] {
			if less(m, best) {
				best = m
			}
		}
		return best
	}
	orderABC := fold([]plans.RecordQueryPlan{planA, planB, planC})
	orderBCA := fold([]plans.RecordQueryPlan{planB, planC, planA})
	orderCAB := fold([]plans.RecordQueryPlan{planC, planA, planB})
	orderACB := fold([]plans.RecordQueryPlan{planA, planC, planB})
	orderBAC := fold([]plans.RecordQueryPlan{planB, planA, planC})
	orderCBA := fold([]plans.RecordQueryPlan{planC, planB, planA})
	t.Logf("linear-fold winner: [A,B,C]=%s [B,C,A]=%s [C,A,B]=%s [A,C,B]=%s [B,A,C]=%s [C,B,A]=%s",
		names[orderABC], names[orderBCA], names[orderCAB],
		names[orderACB], names[orderBAC], names[orderCBA])
	winners := map[string]bool{
		names[orderABC]: true, names[orderBCA]: true, names[orderCAB]: true,
		names[orderACB]: true, names[orderBAC]: true, names[orderCBA]: true,
	}
	if len(winners) != 1 {
		t.Errorf("linear-fold winner depends on insertion order: %v", winners)
	}
}

// TestCompareJoinOrdering_RankIsATotalPreorder brute-forces compareJoinOrdering
// itself over a corpus mixing FlatMap and materialized NestedLoopJoin shapes at
// varying outer/inner cardinalities (including several where the inner is a
// cardinality-1 probe, where the two cost formulas already agreed even before
// the fix) and asserts irreflexivity, antisymmetry, and transitivity hold for
// every triple — the same shape of check as TestCriterion7_RankIsATotalPreorder
// (primary_vs_index_transitivity_test.go) applied to the join-ordering rung.
func TestCompareJoinOrdering_RankIsATotalPreorder(t *testing.T) {
	t.Parallel()

	aliasOuter := values.NamedCorrelationIdentifier("O")
	aliasInner := values.NamedCorrelationIdentifier("I")
	resVal := values.NewQuantifiedObjectValue(values.UniqueCorrelationIdentifier())
	pred := predicates.NewComparisonPredicate(
		values.NewFieldValue(values.NewQuantifiedObjectValue(aliasOuter), "flag", values.UnknownType),
		predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(1)),
	)

	flatMap := func(outerT, innerT string) plans.RecordQueryPlan {
		return plans.NewRecordQueryFlatMapPlan(
			fullScanLeaf(outerT), fullScanLeaf(innerT), aliasOuter, aliasInner, resVal, false)
	}
	nlj := func(outerT, innerT string) plans.RecordQueryPlan {
		return plans.NewRecordQueryNestedLoopJoinPlan(
			fullScanLeaf(outerT), fullScanLeaf(innerT),
			[]predicates.QueryPredicate{pred}, plans.JoinInner, "O", "I", resVal)
	}

	stats := properties.MapStatistics{PerType: map[string]float64{
		"outer_1": 1, "outer_20": 20, "outer_100": 100, "outer_1000": 1000,
		"inner_1": 1, "inner_20": 20, "inner_220": 220, "inner_1000": 1000,
	}}

	corpus := []struct {
		name    string
		plan    plans.RecordQueryPlan
		isNLJ   bool
		isFound bool
	}{
		{name: "flatMap(1,1)", plan: flatMap("outer_1", "inner_1"), isFound: true},
		{name: "flatMap(1,220)", plan: flatMap("outer_1", "inner_220"), isFound: true},
		{name: "flatMap(1,1000)", plan: flatMap("outer_1", "inner_1000"), isFound: true},
		{name: "flatMap(20,1)", plan: flatMap("outer_20", "inner_1"), isFound: true},
		{name: "flatMap(100,1)", plan: flatMap("outer_100", "inner_1"), isFound: true},
		{name: "flatMap(100,20)", plan: flatMap("outer_100", "inner_20"), isFound: true},
		{name: "flatMap(1000,1)", plan: flatMap("outer_1000", "inner_1"), isFound: true},
		{name: "nlj(1,1)", plan: nlj("outer_1", "inner_1"), isNLJ: true, isFound: true},
		{name: "nlj(1,220)", plan: nlj("outer_1", "inner_220"), isNLJ: true, isFound: true},
		{name: "nlj(1,1000)", plan: nlj("outer_1", "inner_1000"), isNLJ: true, isFound: true},
		{name: "nlj(20,1)", plan: nlj("outer_20", "inner_1"), isNLJ: true, isFound: true},
		{name: "nlj(100,20)", plan: nlj("outer_100", "inner_20"), isNLJ: true, isFound: true},
		{name: "nlj(1000,1)", plan: nlj("outer_1000", "inner_1"), isNLJ: true, isFound: true},
	}

	n := len(corpus)
	cmp := make([][]int, n)
	for i := range cmp {
		cmp[i] = make([]int, n)
		for j := range cmp[i] {
			cmp[i][j] = compareJoinOrdering(corpus[i].plan, corpus[j].plan, stats, nil)
		}
	}

	for i := 0; i < n; i++ {
		if cmp[i][i] != 0 {
			t.Errorf("IRREFLEXIVITY: compare(%s,%s)=%d, want 0", corpus[i].name, corpus[i].name, cmp[i][i])
		}
		for j := 0; j < n; j++ {
			if cmp[i][j] != -cmp[j][i] {
				t.Errorf("ANTISYMMETRY: compare(%s,%s)=%d but compare(%s,%s)=%d",
					corpus[i].name, corpus[j].name, cmp[i][j], corpus[j].name, corpus[i].name, cmp[j][i])
			}
		}
	}
	for i := 0; i < n; i++ {
		for j := 0; j < n; j++ {
			for k := 0; k < n; k++ {
				if cmp[i][j] < 0 && cmp[j][k] < 0 && cmp[i][k] >= 0 {
					t.Errorf("TRANSITIVITY: %s < %s < %s but compare(%s,%s)=%d",
						corpus[i].name, corpus[j].name, corpus[k].name,
						corpus[i].name, corpus[k].name, cmp[i][k])
				}
				if cmp[i][j] == 0 && cmp[j][k] == 0 && cmp[i][k] != 0 {
					t.Errorf("INDIFFERENCE-TRANSITIVITY: %s ~ %s ~ %s but compare(%s,%s)=%d",
						corpus[i].name, corpus[j].name, corpus[k].name,
						corpus[i].name, corpus[k].name, cmp[i][k])
				}
			}
		}
	}

	// Sanity: this corpus must actually MIX shapes and cardinalities (else
	// the test would trivially pass by never exercising the fixed branch).
	sawFlatMap, sawNLJ := false, false
	for _, c := range corpus {
		if c.isFound {
			if c.isNLJ {
				sawNLJ = true
			} else {
				sawFlatMap = true
			}
		}
	}
	if !sawFlatMap || !sawNLJ {
		t.Fatalf("corpus precondition failed: need both FlatMap and NLJ shapes, got flatMap=%v nlj=%v", sawFlatMap, sawNLJ)
	}
}
