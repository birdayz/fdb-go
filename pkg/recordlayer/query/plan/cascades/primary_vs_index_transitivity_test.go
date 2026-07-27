package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// Criterion #7 transitivity.
//
// The rung used to be PAIR-RESTRICTED: it adjudicated only (lone primary scan)
// versus (singular index-scan-with-fetch) and abstained for index-versus-index.
// A criterion that decides only some pair shapes is not a total preorder, and a
// comparator built by lexicographic composition is transitive only if every
// criterion is one. The two tests below are the concrete cycles that produced,
// both reproducible on the pre-fix rung and on Java's own
// comparePrimaryScanToIndexScan.

// makeCriterion7Primary builds a lone primary scan under a type filter, with
// the given comparisons as its search arguments.
func makeCriterion7Primary(t *testing.T, ranges ...*predicates.ComparisonRange) plans.RecordQueryPlan {
	t.Helper()
	scan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false).
		WithScanComparisons(ranges)
	return plans.NewRecordQueryTypeFilterPlan([]string{"T"}, scan)
}

// makeCriterion7Index builds a singular index scan with a fetch and no type
// filter, with the given comparisons as its search arguments.
func makeCriterion7Index(t *testing.T, name string, fetches int, ranges ...*predicates.ComparisonRange) plans.RecordQueryPlan {
	t.Helper()
	var cur plans.RecordQueryPlan = plans.NewRecordQueryIndexPlan(
		name, ranges, []string{"T"}, values.UnknownType, false)
	for i := 0; i < fetches; i++ {
		cur = plans.NewRecordQueryFetchFromPartialRecordPlan(
			cur, nil, values.UnknownType, plans.FetchIndexRecordsPrimaryKey)
	}
	return cur
}

// assertNoStrictCycle walks the given ring and fails if every consecutive
// comparison is strict, which would make the ring a cycle.
func assertNoStrictCycle(t *testing.T, names []string, ring []expressions.RelationalExpression) {
	t.Helper()
	cyclic := true
	for i := range ring {
		next := (i + 1) % len(ring)
		cmp := planningCostModelCompareWith(ring[i], ring[next], nil, nil)
		t.Logf("compare(%s, %s) = %d", names[i], names[next], cmp)
		if cmp >= 0 {
			cyclic = false
		}
	}
	if cyclic {
		t.Fatalf("CYCLE: the comparator ranks %v strictly around the ring, so the "+
			"elected winner depends on comparison order", names)
	}
}

// TestCriterion7_AbstentionCycleIsGone pins the three-plan cycle the rung's
// pair-restriction admitted. It contains no IN operator: the SARG sub-case
// preferred the index over the type-filtered primary scan, the PREFER_SCAN
// config branch preferred that same primary scan over a second index, and the
// rung then ABSTAINED between the two indexes so the fetch-count rung closed
// the ring the other way.
func TestCriterion7_AbstentionCycleIsGone(t *testing.T) {
	t.Parallel()

	bind := rungEqualityRange(t, values.NewQuantifiedObjectValue(
		values.NamedCorrelationIdentifier("cycle_bind")))

	sargedIndexThreeFetches := makeCriterion7Index(t, "idx_cycle_sarged", 3, bind)
	primaryWithTypeFilter := makeCriterion7Primary(t)
	plainIndexOneFetch := makeCriterion7Index(t, "idx_cycle_plain", 1)

	// Preconditions: the fetch rung really does prefer the plain index, which is
	// the arm that used to close the ring.
	sargedOps := concretePlanCounts(sargedIndexThreeFetches, nil)
	plainOps := concretePlanCounts(plainIndexOneFetch, nil)
	if sargedOps.fetchCount != 3 || plainOps.fetchCount != 1 {
		t.Fatalf("fetch precondition = (%d, %d), want (3, 1)", sargedOps.fetchCount, plainOps.fetchCount)
	}
	if concretePlanCounts(primaryWithTypeFilter, nil).typeFilterCount == 0 {
		t.Fatal("the primary scan must carry a type filter for this cycle to form")
	}

	assertNoStrictCycle(t,
		[]string{"sargedIndex+3fetches", "primaryScan+typeFilter", "plainIndex+1fetch"},
		[]expressions.RelationalExpression{
			sargedIndexThreeFetches, primaryWithTypeFilter, plainIndexOneFetch,
		})
}

// TestCriterion7_AdjudicatedCycleIsGone pins the harder cycle, and the reason
// the repair could not simply extend the old verdicts to the abstained pairs.
//
// Every consecutive comparison in this ring is a (primary scan, index scan)
// pair the pre-fix rung DID adjudicate — no abstention is involved. The rung
// was cyclic on its own domain, because its type-filter sub-case decided on the
// SUBSET relation between the two sides' search-argument sets, and a subset
// relation is a partial order that no total preorder can reproduce:
//
//	P1{a} < I2{b,c}   a is not in {b,c}, so the sub-case is skipped and
//	                  PREFER_SCAN prefers the primary
//	I2{b,c} < P2{b}   {b} is a strict subset of {b,c}: the sub-case fires
//	P2{b} < I3{c,a}   b is not in {c,a}: skipped again, PREFER_SCAN
//	I3{c,a} < P1{a}   {a} is a strict subset of {c,a}: the sub-case fires
//
// Ranking the sets by SIZE agrees with the subset test on every comparable pair
// and differs only on incomparable ones — which is exactly this configuration.
func TestCriterion7_AdjudicatedCycleIsGone(t *testing.T) {
	t.Parallel()

	a := rungEqualityRange(t, values.LiteralValue(int64(1)))
	b := rungEqualityRange(t, values.LiteralValue(int64(2)))
	c := rungEqualityRange(t, values.LiteralValue(int64(3)))

	p1 := makeCriterion7Primary(t, a)
	i2 := makeCriterion7Index(t, "idx_ring_2", 1, b, c)
	p2 := makeCriterion7Primary(t, b)
	i3 := makeCriterion7Index(t, "idx_ring_3", 1, c, a)

	// Preconditions: every leg is a pair the pair-restricted rung adjudicated,
	// so the cycle cannot be blamed on abstention.
	for _, side := range []struct {
		name string
		plan plans.RecordQueryPlan
		want string
	}{
		{"P1{a}", p1, "primary"},
		{"I2{b,c}", i2, "index"},
		{"P2{b}", p2, "primary"},
		{"I3{c,a}", i3, "index"},
	} {
		ops := concretePlanCounts(side.plan, nil)
		isPrimary := ops.scanCount == 1 && ops.indexScanCount == 0 && ops.coveringIndexCount == 0
		isIndex := isSingularIndexScanWithFetch(ops)
		if side.want == "primary" && !isPrimary {
			t.Fatalf("%s is not a lone primary scan", side.name)
		}
		if side.want == "index" && !isIndex {
			t.Fatalf("%s is not a singular index scan with fetch", side.name)
		}
	}
	if got := distinctSargCount(p1); got != 1 {
		t.Fatalf("P1 search arguments = %d, want 1", got)
	}
	if got := distinctSargCount(i2); got != 2 {
		t.Fatalf("I2 search arguments = %d, want 2", got)
	}

	assertNoStrictCycle(t,
		[]string{"P1{a}", "I2{b,c}", "P2{b}", "I3{c,a}"},
		[]expressions.RelationalExpression{p1, i2, p2, i3})
}

// TestCriterion7_FetchPayingIndexLosesToCoveringIndex pins the ONE class of
// genuinely-new verdict the ranked rung produces across the SQL-level suites:
// a singular index scan that pays a fetch now loses to a covering index scan
// that needs none. The pre-fix rung abstained on this pair (it is not a
// primary-scan-versus-index pair), so the verdict was previously supplied by the
// fetch rung further down.
//
// The two rungs AGREE, which is why nothing observable moved: this rung ranks
// the fetch-paying side worse, and the fetch rung's own metric
// (indexScanCount + fetchCount) is 2 against 0 in the same direction. The
// assertion below pins both so a future change to either cannot silently
// diverge from the other.
func TestCriterion7_FetchPayingIndexLosesToCoveringIndex(t *testing.T) {
	t.Parallel()

	sarg := rungEqualityRange(t, values.LiteralValue(int64(1)))
	fetchPaying := plans.NewRecordQueryFetchFromPartialRecordPlan(
		plans.NewRecordQueryIndexPlan("idx_fetch_paying",
			[]*predicates.ComparisonRange{sarg}, []string{"T"}, values.UnknownType, false),
		nil, values.UnknownType, plans.FetchIndexRecordsPrimaryKey)
	covering := plans.NewRecordQueryIndexPlan("idx_covering_no_fetch",
		[]*predicates.ComparisonRange{sarg}, []string{"T"}, values.UnknownType, false).
		WithCovering([]string{"K"})

	opsFetch := concretePlanCounts(fetchPaying, nil)
	opsCovering := concretePlanCounts(covering, nil)

	// The shape observed in the measurement: equal search arguments, equal type
	// filters, one side paying a fetch and the other not.
	if !isSingularIndexScanWithFetch(opsFetch) {
		t.Fatal("the fetch-paying side must be a singular index scan with fetch")
	}
	if isSingularIndexScanWithFetch(opsCovering) {
		t.Fatal("the covering side must NOT be a singular index scan with fetch")
	}
	if distinctSargCount(fetchPaying) != distinctSargCount(covering) {
		t.Fatalf("search arguments must tie, got %d vs %d",
			distinctSargCount(fetchPaying), distinctSargCount(covering))
	}
	if opsFetch.typeFilterCount != 0 || opsCovering.typeFilterCount != 0 {
		t.Fatalf("neither side may carry a type filter, got %d and %d",
			opsFetch.typeFilterCount, opsCovering.typeFilterCount)
	}

	if cmp := comparePrimaryScanVsIndexScan(
		fetchPaying, covering, opsFetch, opsCovering, PreferScan,
	); cmp <= 0 {
		t.Fatalf("rung(fetchPaying, covering) = %d, want the covering scan to win", cmp)
	}

	// The fetch rung below points the SAME way, independently.
	fetchMetricPaying := opsFetch.indexScanCount + opsFetch.fetchCount
	fetchMetricCovering := opsCovering.indexScanCount + opsCovering.fetchCount
	if fetchMetricPaying <= fetchMetricCovering {
		t.Fatalf("fetch metric = %d vs %d, want the fetch-paying side to be strictly worse "+
			"so the two rungs agree", fetchMetricPaying, fetchMetricCovering)
	}

	assertStrictPlanningPreference(t, PlanningCostModelLess, covering, fetchPaying)
}

// TestCriterion7_RankIsATotalPreorder brute-forces the rung ITSELF (not the
// whole chain) over a corpus covering every class of its ladder, and asserts
// the three properties a lexicographic-chain rung has to have.
func TestCriterion7_RankIsATotalPreorder(t *testing.T) {
	t.Parallel()

	one := rungEqualityRange(t, values.LiteralValue(int64(1)))
	two := rungEqualityRange(t, values.LiteralValue(int64(2)))

	covering := plans.NewRecordQueryIndexPlan("idx_rank_covering",
		[]*predicates.ComparisonRange{one}, []string{"T"}, values.UnknownType, false).
		WithCovering([]string{"K"})

	corpus := []struct {
		name string
		plan plans.RecordQueryPlan
	}{
		{"primary", plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)},
		{"primary+tf", makeCriterion7Primary(t)},
		{"primary+tf+1sarg", makeCriterion7Primary(t, one)},
		{"primary+tf+2sargs", makeCriterion7Primary(t, one, two)},
		{"index", makeCriterion7Index(t, "idx_rank_lean", 1)},
		{"index+1sarg", makeCriterion7Index(t, "idx_rank_1", 1, one)},
		{"index+2sargs", makeCriterion7Index(t, "idx_rank_2", 1, one, two)},
		{
			"index+tf",
			plans.NewRecordQueryTypeFilterPlan([]string{"T"},
				makeCriterion7Index(t, "idx_rank_tf", 1, one)),
		},
		{"coveringNoFetch", covering},
		{
			"sortedPrimary",
			plans.NewRecordQueryInMemorySortPlan(
				plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false), nil),
		},
		{
			"sortedIndex",
			plans.NewRecordQueryInMemorySortPlan(
				makeCriterion7Index(t, "idx_rank_sorted", 1, one, two), nil),
		},
		{
			"twoIndexes",
			plans.NewRecordQueryIntersectionPlan([]plans.RecordQueryPlan{
				makeCriterion7Index(t, "idx_rank_int_a", 0, one),
				makeCriterion7Index(t, "idx_rank_int_b", 0, two),
			}, nil),
		},
	}

	for _, pref := range []IndexScanPreference{PreferScan, PreferIndex, PreferPrimaryKeyIndex} {
		n := len(corpus)
		cmp := make([][]int, n)
		for i := range cmp {
			cmp[i] = make([]int, n)
			for j := range cmp[i] {
				cmp[i][j] = comparePrimaryScanVsIndexScan(
					corpus[i].plan, corpus[j].plan,
					concretePlanCounts(corpus[i].plan, nil),
					concretePlanCounts(corpus[j].plan, nil),
					pref)
			}
		}
		for i := 0; i < n; i++ {
			if cmp[i][i] != 0 {
				t.Errorf("pref=%v IRREFLEXIVITY: compare(%s,%s)=%d, want 0",
					pref, corpus[i].name, corpus[i].name, cmp[i][i])
			}
			for j := 0; j < n; j++ {
				if cmp[i][j] != -cmp[j][i] {
					t.Errorf("pref=%v ANTISYMMETRY: compare(%s,%s)=%d but compare(%s,%s)=%d",
						pref, corpus[i].name, corpus[j].name, cmp[i][j],
						corpus[j].name, corpus[i].name, cmp[j][i])
				}
			}
		}
		// A total preorder needs BOTH the strict relation and the indifference
		// relation to be transitive; the pre-fix rung failed the latter (it
		// abstained between two indexes it ranked on opposite sides of a
		// primary scan).
		for i := 0; i < n; i++ {
			for j := 0; j < n; j++ {
				for k := 0; k < n; k++ {
					if cmp[i][j] < 0 && cmp[j][k] < 0 && cmp[i][k] >= 0 {
						t.Errorf("pref=%v TRANSITIVITY: %s < %s < %s but compare(%s,%s)=%d",
							pref, corpus[i].name, corpus[j].name, corpus[k].name,
							corpus[i].name, corpus[k].name, cmp[i][k])
					}
					if cmp[i][j] == 0 && cmp[j][k] == 0 && cmp[i][k] != 0 {
						t.Errorf("pref=%v INDIFFERENCE-TRANSITIVITY: %s ~ %s ~ %s but compare(%s,%s)=%d",
							pref, corpus[i].name, corpus[j].name, corpus[k].name,
							corpus[i].name, corpus[k].name, cmp[i][k])
					}
				}
			}
		}
	}
}
