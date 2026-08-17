package cascades

import (
	"fmt"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func mustRungConstruct[T any](value T, err error) T {
	if err != nil {
		panic("construct cost-model rung fixture: " + err.Error())
	}
	return value
}

func rungRowType() values.Type {
	return values.NewRecordType("CostRungRow", false, []values.Field{
		{Name: "A", FieldType: values.NullableLong, Ordinal: 0},
		{Name: "B", FieldType: values.NullableLong, Ordinal: 1},
		{Name: "K", FieldType: values.NullableLong, Ordinal: 2},
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 3},
	})
}

func rungScan(recordType string) *plans.RecordQueryScanPlan {
	return mustRungConstruct(plans.NewRecordQueryScanPlan(
		[]string{recordType}, rungRowType(), false))
}

func rungIndex(name string, ranges []*predicates.ComparisonRange) *plans.RecordQueryIndexPlan {
	return mustRungConstruct(plans.NewRecordQueryIndexPlan(
		name, ranges, []string{"T"}, rungRowType(), false))
}

func rungResultValue() values.Value {
	return values.NewRawRecordConstructorValue(values.RecordConstructorField{
		Name:  "VALUE",
		Value: &values.ConstantValue{Value: int64(1), Typ: values.NotNullLong},
	})
}

func rungLongLiteral(value int64) values.Value {
	return &values.ConstantValue{Value: value, Typ: values.NotNullLong}
}

func rungUnion(children ...plans.RecordQueryPlan) *plans.RecordQueryUnorderedUnionPlan {
	return mustRungConstruct(plans.NewRecordQueryUnorderedUnionPlan(children))
}

func rungFilter(
	inner plans.RecordQueryPlan, predicates ...predicates.QueryPredicate,
) *plans.RecordQueryPredicatesFilterPlan {
	return mustRungConstruct(plans.NewRecordQueryPredicatesFilterPlan(inner, predicates))
}

func rungSort(inner plans.RecordQueryPlan) *plans.RecordQueryInMemorySortPlan {
	return mustRungConstruct(plans.NewRecordQueryInMemorySortPlan(inner, nil))
}

func rungTypeFilter(types []string, inner plans.RecordQueryPlan) *plans.RecordQueryTypeFilterPlan {
	return mustRungConstruct(plans.NewRecordQueryTypeFilterPlan(types, inner))
}

func rungLimit(inner plans.RecordQueryPlan, limit, offset int64) *plans.RecordQueryLimitPlan {
	return mustRungConstruct(plans.NewRecordQueryLimitPlan(inner, limit, offset))
}

func rungInJoin(inner plans.RecordQueryPlan, binding string) *plans.RecordQueryInJoinPlan {
	return mustRungConstruct(plans.NewRecordQueryInJoinPlan(inner, binding, false, false))
}

func rungMap(inner plans.RecordQueryPlan) *plans.RecordQueryMapPlan {
	return mustRungConstruct(plans.NewRecordQueryMapPlan(inner, rungResultValue()))
}

func rungCoveringIndex(index *plans.RecordQueryIndexPlan) *plans.RecordQueryCoveringIndexPlan {
	return mustRungConstruct(plans.NewRecordQueryCoveringIndexPlan(index))
}

func rungFetch(inner plans.RecordQueryPlan) *plans.RecordQueryFetchFromPartialRecordPlan {
	return mustRungConstruct(plans.NewRecordQueryFetchFromPartialRecordPlan(
		inner, nil, rungRowType(), plans.FetchIndexRecordsPrimaryKey))
}

func rungNLJ(
	outer, inner plans.RecordQueryPlan,
	joinPredicates []predicates.QueryPredicate,
) *plans.RecordQueryNestedLoopJoinPlan {
	return mustRungConstruct(plans.NewRecordQueryNestedLoopJoinPlan(
		outer,
		inner,
		joinPredicates,
		plans.JoinInner,
		values.NamedCorrelationIdentifier("outer"),
		values.NamedCorrelationIdentifier("inner"),
		rungResultValue(),
	))
}

func rungFullScan(recordType string) *expressions.FullUnorderedScanExpression {
	return mustRungConstruct(expressions.NewFullUnorderedScanExpression(
		[]string{recordType}, rungRowType()))
}

func assertStrictPlanningPreference(
	t *testing.T,
	less func(expressions.RelationalExpression, expressions.RelationalExpression) bool,
	winner expressions.RelationalExpression,
	loser expressions.RelationalExpression,
) {
	t.Helper()
	if !less(winner, loser) {
		t.Fatalf("expected %T to be strictly preferred over %T", winner, loser)
	}
	if less(loser, winner) {
		t.Fatalf("cost comparator is asymmetric in the wrong direction: %T also beats %T", loser, winner)
	}
}

func rungEqualityRange(t *testing.T, operand values.Value) *predicates.ComparisonRange {
	t.Helper()
	comparison := &predicates.Comparison{
		Type:    predicates.ComparisonEquals,
		Operand: operand,
	}
	merged := predicates.EmptyComparisonRange().Merge(comparison)
	if !merged.Ok {
		t.Fatal("failed to create equality comparison range")
	}
	return merged.Range
}

func rungPredicate(field string) predicates.QueryPredicate {
	root := mustRungConstruct(values.NewQuantifiedObjectValue(
		values.NamedCorrelationIdentifier("cost_rung_predicate"), rungRowType()))
	request := mustRungConstruct(values.FieldByName(field))
	resolved := mustRungConstruct(values.ResolveFieldAccess(
		root, []values.FieldRequest{request}))
	return predicates.NewComparisonPredicate(
		resolved,
		predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(1)),
	)
}

// TestPlanningCostModel_RungOrderResidualBeforeDataAccess pins the first
// lexicographic conflict in the ordinal model. The zero-residual candidate
// deliberately performs two data accesses, while the one-residual candidate
// performs only one. Residual count must decide before data-access count.
func TestPlanningCostModel_RungOrderResidualBeforeDataAccess(t *testing.T) {
	t.Parallel()

	noResidual := rungUnion(rungScan("T"), rungScan("T"))
	oneResidual := rungFilter(rungScan("T"), rungPredicate("K"))

	if got := countResidualPredicatesWithContext(noResidual, nil); got != 0 {
		t.Fatalf("zero-residual candidate has %d residual conjuncts, want 0", got)
	}
	if got := countResidualPredicatesWithContext(oneResidual, nil); got != 1 {
		t.Fatalf("one-residual candidate has %d residual conjuncts, want 1", got)
	}
	opsWinner := findExpressionsByType(noResidual, nil, nil)
	opsLoser := findExpressionsByType(oneResidual, nil, nil)
	winnerAccesses := opsWinner.scanCount + opsWinner.indexScanCount + opsWinner.coveringIndexCount
	loserAccesses := opsLoser.scanCount + opsLoser.indexScanCount + opsLoser.coveringIndexCount
	if winnerAccesses != 2 || loserAccesses != 1 {
		t.Fatalf("data-access precondition = (%d, %d), want (2, 1)", winnerAccesses, loserAccesses)
	}

	assertStrictPlanningPreference(t, PlanningCostModelLess, noResidual, oneResidual)
}

// TestPlanningCostModel_RungOrderDataAccessBeforeSort makes the later sort
// criterion prefer the two-access candidate. The one-access candidate must
// still win because data-access count is the earlier rung.
func TestPlanningCostModel_RungOrderDataAccessBeforeSort(t *testing.T) {
	t.Parallel()

	oneAccessWithSort := rungSort(rungScan("T"))
	twoAccessesWithoutSort := rungUnion(rungScan("T"), rungScan("T"))

	opsWinner := findExpressionsByType(oneAccessWithSort, nil, nil)
	opsLoser := findExpressionsByType(twoAccessesWithoutSort, nil, nil)
	winnerAccesses := opsWinner.scanCount + opsWinner.indexScanCount + opsWinner.coveringIndexCount
	loserAccesses := opsLoser.scanCount + opsLoser.indexScanCount + opsLoser.coveringIndexCount
	if winnerAccesses != 1 || loserAccesses != 2 {
		t.Fatalf("data-access precondition = (%d, %d), want (1, 2)", winnerAccesses, loserAccesses)
	}
	if opsWinner.inMemorySortCount != 1 || opsLoser.inMemorySortCount != 0 {
		t.Fatalf(
			"sort-count precondition = (%d, %d), want (1, 0)",
			opsWinner.inMemorySortCount,
			opsLoser.inMemorySortCount,
		)
	}

	assertStrictPlanningPreference(t, PlanningCostModelLess, oneAccessWithSort, twoAccessesWithoutSort)
}

// TestPlanningCostModel_RungOrderSortBeforeTypeFilterCount proves the
// Go-specific sort-elimination rung runs before Java's type-filter rungs. The
// sort-free candidate intentionally has the worse type-filter count.
func TestPlanningCostModel_RungOrderSortBeforeTypeFilterCount(t *testing.T) {
	t.Parallel()

	sortFreeTypeHeavy := rungTypeFilter(
		[]string{"T1", "T2", "T3"}, rungScan("T"))
	sortedWithoutTypeFilter := rungSort(rungScan("T"))

	opsWinner := findExpressionsByType(sortFreeTypeHeavy, nil, nil)
	opsLoser := findExpressionsByType(sortedWithoutTypeFilter, nil, nil)
	if opsWinner.inMemorySortCount != 0 || opsLoser.inMemorySortCount != 1 {
		t.Fatalf(
			"sort-count precondition = (%d, %d), want (0, 1)",
			opsWinner.inMemorySortCount,
			opsLoser.inMemorySortCount,
		)
	}
	if opsWinner.typeFilterCount != 3 || opsLoser.typeFilterCount != 0 {
		t.Fatalf(
			"type-filter precondition = (%d, %d), want (3, 0)",
			opsWinner.typeFilterCount,
			opsLoser.typeFilterCount,
		)
	}

	assertStrictPlanningPreference(t, PlanningCostModelLess, sortFreeTypeHeavy, sortedWithoutTypeFilter)
}

// TestPlanningCostModel_RungOrderTypeFilterCountBeforeDepth makes the deeper
// type filter lose solely because it admits more record types. This pins the
// two type-filter rungs' order, not just their independent directions.
func TestPlanningCostModel_RungOrderTypeFilterCountBeforeDepth(t *testing.T) {
	t.Parallel()

	fewerTypesShallow := rungTypeFilter(
		[]string{"T1"}, rungLimit(rungScan("T"), 10, 0))
	moreTypesDeep := rungLimit(
		rungTypeFilter([]string{"T1", "T2"}, rungScan("T")), 10, 0)

	if got := costExprDepth(fewerTypesShallow, matchTypeFilter); got != 0 {
		t.Fatalf("shallow type-filter depth = %d, want 0", got)
	}
	if got := costExprDepth(moreTypesDeep, matchTypeFilter); got != 1 {
		t.Fatalf("deep type-filter depth = %d, want 1", got)
	}

	assertStrictPlanningPreference(t, PlanningCostModelLess, fewerTypesShallow, moreTypesDeep)
}

// TestPlanningCostModel_RecursiveCTERungBeforeSort puts a redundant sort over
// the DFS candidate. The recursive DFS-over-level decision must fire before
// the later sort-count rung and still select DFS.
func TestPlanningCostModel_RecursiveCTERungBeforeSort(t *testing.T) {
	t.Parallel()

	dfs := mustRungConstruct(plans.NewRecordQueryRecursiveDfsJoinPlanFromQuantifiers(
		expressions.NewPhysicalQuantifier(expressions.InitialOf(
			rungScan("T"),
		)),
		expressions.NewPhysicalQuantifier(expressions.InitialOf(
			rungScan("T"),
		)),
		values.NamedCorrelationIdentifier("prior"),
		plans.DfsPreorder,
		false,
	))
	level := mustRungConstruct(plans.NewRecordQueryRecursiveLevelUnionPlanFromQuantifiers(
		expressions.NewPhysicalQuantifier(expressions.InitialOf(
			rungScan("T"),
		)),
		expressions.NewPhysicalQuantifier(expressions.InitialOf(
			rungScan("T"),
		)),
		values.NamedCorrelationIdentifier("scan"),
		values.NamedCorrelationIdentifier("insert"),
		false,
	))
	sortedDFS := rungSort(dfs)

	if cmp := compareRecursiveCTE(sortedDFS, level); cmp >= 0 {
		t.Fatalf("compareRecursiveCTE(Sort(DFS), level) = %d, want < 0", cmp)
	}
	opsDFS := findExpressionsByType(sortedDFS, nil, nil)
	opsLevel := findExpressionsByType(level, nil, nil)
	if opsDFS.inMemorySortCount != 1 || opsLevel.inMemorySortCount != 0 {
		t.Fatalf(
			"sort-count adversary = (%d, %d), want (1, 0)",
			opsDFS.inMemorySortCount,
			opsLevel.inMemorySortCount,
		)
	}

	assertStrictPlanningPreference(t, PlanningCostModelLess, sortedDFS, level)
}

// TestPlanningCostModel_JoinOrderingWinnerFlipsWithStatistics verifies that
// the same pair of FlatMap orders changes winner when the table cardinalities
// change. It exercises the full comparator and the join-ordering rung itself.
func TestPlanningCostModel_JoinOrderingWinnerFlipsWithStatistics(t *testing.T) {
	t.Parallel()

	aliasA := values.NamedCorrelationIdentifier("a")
	aliasB := values.NamedCorrelationIdentifier("b")
	scanA := rungScan("A")
	scanB := rungScan("B")
	aThenB := mustRungConstruct(plans.NewRecordQueryFlatMapPlan(
		scanA,
		scanB,
		aliasA,
		aliasB,
		rungResultValue(),
		false,
	))
	bThenA := mustRungConstruct(plans.NewRecordQueryFlatMapPlan(
		scanB,
		scanA,
		aliasB,
		aliasA,
		rungResultValue(),
		false,
	))

	aSmall := properties.MapStatistics{PerType: map[string]float64{"A": 10, "B": 10_000}}
	bSmall := properties.MapStatistics{PerType: map[string]float64{"A": 10_000, "B": 10}}

	if cmp := compareJoinOrdering(aThenB, bThenA, aSmall, nil); cmp >= 0 {
		t.Fatalf("A-small join comparison = %d, want A→B to win", cmp)
	}
	assertStrictPlanningPreference(
		t,
		NewPlanningCostModelLessWithContext(aSmall, nil),
		aThenB,
		bThenA,
	)

	if cmp := compareJoinOrdering(aThenB, bThenA, bSmall, nil); cmp <= 0 {
		t.Fatalf("B-small join comparison = %d, want B→A to win", cmp)
	}
	assertStrictPlanningPreference(
		t,
		NewPlanningCostModelLessWithContext(bSmall, nil),
		bThenA,
		aThenB,
	)
}

func makeInRungCandidates(
	t *testing.T,
	sarged bool,
) (*plans.RecordQueryInJoinPlan, *plans.RecordQueryLimitPlan) {
	t.Helper()

	bindingName := "in_value"
	var ranges []*predicates.ComparisonRange
	if sarged {
		bindingQOV := mustRungConstruct(values.NewQuantifiedObjectValue(
			values.NamedCorrelationIdentifier(bindingName), values.NotNullLong))
		ranges = []*predicates.ComparisonRange{
			rungEqualityRange(
				t,
				bindingQOV,
			),
		}
	}
	index := rungIndex("idx_in_rung", ranges)
	inJoin := rungInJoin(index, bindingName)
	limit := rungLimit(index, 1, 0)
	return inJoin, limit
}

// TestPlanningCostModel_InPlanPenaltyFlipsAfterSarging uses the same InJoin
// versus non-IN shape twice. An unsarged IN source loses at criterion #6; once
// its binding becomes a real index SARG, criterion #6 abstains and the later
// "more IN sources" rung makes the InJoin win.
func TestPlanningCostModel_InPlanPenaltyFlipsAfterSarging(t *testing.T) {
	t.Parallel()

	stats := properties.FixedStatistics{Cardinality: 1}
	less := NewPlanningCostModelLessWithContext(stats, nil)

	unsargedIn, unsargedOther := makeInRungCandidates(t, false)
	if penalty, applicable := compareInOperator(unsargedIn); !applicable || penalty != 1 {
		t.Fatalf("unsarged InJoin compareInOperator = (%d, %v), want (1, true)", penalty, applicable)
	}
	assertStrictPlanningPreference(t, less, unsargedOther, unsargedIn)

	sargedIn, sargedOther := makeInRungCandidates(t, true)
	if penalty, applicable := compareInOperator(sargedIn); !applicable || penalty != 0 {
		t.Fatalf("sarged InJoin compareInOperator = (%d, %v), want (0, true)", penalty, applicable)
	}
	opsIn := findExpressionsByType(sargedIn, stats, nil)
	opsOther := findExpressionsByType(sargedOther, stats, nil)
	if opsIn.inJoinCount != 1 || opsOther.inJoinCount != 0 {
		t.Fatalf("IN-source count precondition = (%d, %d), want (1, 0)", opsIn.inJoinCount, opsOther.inJoinCount)
	}
	otherCost := properties.EstimateCostWith(sargedOther, stats)
	inCost := properties.EstimateCostWith(sargedIn, stats)
	if !otherCost.Less(inCost) {
		t.Fatalf(
			"scalar-cost adversary missing: non-IN=%+v must be cheaper than InJoin=%+v",
			otherCost,
			inCost,
		)
	}
	assertStrictPlanningPreference(t, less, sargedIn, sargedOther)
}

// TestPlanningCostModel_InJoinCountRung wraps both candidates in Map so the
// root-only IN penalty does not apply. The candidates tie on data access and
// fetch depth; the one with two nested InJoin sources must beat the one with a
// single InJoin source.
func TestPlanningCostModel_InJoinCountRung(t *testing.T) {
	t.Parallel()

	index := func() *plans.RecordQueryIndexPlan {
		return rungIndex("idx_nested_in_rung", nil)
	}
	moreInJoins := rungMap(rungInJoin(rungInJoin(index(), "inner_in"), "outer_in"))
	fewerInJoins := rungMap(rungLimit(rungInJoin(index(), "single_in"), 1, 0))

	if _, applicable := compareInOperator(moreInJoins); applicable {
		t.Fatal("root Map unexpectedly activated the root-only IN penalty")
	}
	if _, applicable := compareInOperator(fewerInJoins); applicable {
		t.Fatal("root Map unexpectedly activated the root-only IN penalty")
	}
	opsWinner := findExpressionsByType(moreInJoins, nil, nil)
	opsLoser := findExpressionsByType(fewerInJoins, nil, nil)
	if opsWinner.inJoinCount != 2 || opsLoser.inJoinCount != 1 {
		t.Fatalf(
			"InJoin-count precondition = (%d, %d), want (2, 1)",
			opsWinner.inJoinCount,
			opsLoser.inJoinCount,
		)
	}
	if winnerDepth, loserDepth := costExprDepth(moreInJoins, matchFetch), costExprDepth(fewerInJoins, matchFetch); winnerDepth != loserDepth {
		t.Fatalf("fetch-depth precondition differs: winner=%d loser=%d", winnerDepth, loserDepth)
	}

	assertStrictPlanningPreference(t, PlanningCostModelLess, moreInJoins, fewerInJoins)
}

// TestPlanningCostModel_PrimaryIndexWinnerFlipsWithConfiguration exercises the
// primary-vs-index branch through the full comparator. Changing only
// IndexScanPreference must reverse the strict winner.
func TestPlanningCostModel_PrimaryIndexWinnerFlipsWithConfiguration(t *testing.T) {
	t.Parallel()

	primary := rungScan("T")
	index := rungIndex("idx", nil)

	preferScan := &prefTestCtx{pref: PreferScan}
	assertStrictPlanningPreference(
		t,
		NewPlanningCostModelLessWithContext(nil, preferScan),
		primary,
		index,
	)

	preferIndex := &prefTestCtx{pref: PreferIndex}
	assertStrictPlanningPreference(
		t,
		NewPlanningCostModelLessWithContext(nil, preferIndex),
		index,
		primary,
	)
}

// TestPlanningCostModel_PrimaryIndexSARGRichIndexWins verifies criterion #7's
// contested band end to end: even under PreferScan, an index without a type
// filter wins over a type-filtered primary scan when it carries MORE search
// arguments.
//
// The isolation is differential rather than adversarial. The same pair with the
// index's extra comparison removed must flip the winner back to the primary
// scan, which no rung below criterion #7 could produce (the type-filter rung
// directly beneath prefers the index in BOTH shapes). So a verdict that tracks
// the extra comparison can only have come from this rung.
func TestPlanningCostModel_PrimaryIndexSARGRichIndexWins(t *testing.T) {
	t.Parallel()

	shared := rungEqualityRange(t, rungLongLiteral(1))
	extra := rungEqualityRange(t, rungLongLiteral(2))
	primaryScan := rungScan("T").
		WithScanComparisons([]*predicates.ComparisonRange{shared})
	primary := rungTypeFilter([]string{"T"}, primaryScan)
	richIndex := rungIndex(
		"idx_sarg_rich", []*predicates.ComparisonRange{shared, extra})
	leanIndex := rungIndex(
		"idx_sarg_lean", []*predicates.ComparisonRange{shared})
	ctx := &prefTestCtx{pref: PreferScan}

	opsPrimary := findExpressionsByType(primary, nil, ctx)
	opsRich := findExpressionsByType(richIndex, nil, ctx)
	opsLean := findExpressionsByType(leanIndex, nil, ctx)
	if opsPrimary.typeFilterCount == 0 || opsRich.typeFilterCount != 0 || opsLean.typeFilterCount != 0 {
		t.Fatalf(
			"type-filter precondition = (%d, %d, %d), want (>0, 0, 0)",
			opsPrimary.typeFilterCount, opsRich.typeFilterCount, opsLean.typeFilterCount,
		)
	}
	if got := distinctSargCount(primary); got != 1 {
		t.Fatalf("primary search-argument count = %d, want 1", got)
	}
	if got := distinctSargCount(richIndex); got != 2 {
		t.Fatalf("rich index search-argument count = %d, want 2", got)
	}
	if got := distinctSargCount(leanIndex); got != 1 {
		t.Fatalf("lean index search-argument count = %d, want 1", got)
	}

	if cmp := comparePrimaryScanVsIndexScan(primary, richIndex, opsPrimary, opsRich, PreferScan); cmp <= 0 {
		t.Fatalf("SARG-rich comparison = %d, want the index to win", cmp)
	}
	// Differential control: strip the extra comparison and the primary wins.
	if cmp := comparePrimaryScanVsIndexScan(primary, leanIndex, opsPrimary, opsLean, PreferScan); cmp >= 0 {
		t.Fatalf("SARG-equal comparison = %d, want the primary scan to win", cmp)
	}

	less := NewPlanningCostModelLessWithContext(nil, ctx)
	assertStrictPlanningPreference(t, less, richIndex, primary)
	assertStrictPlanningPreference(t, less, primary, leanIndex)
}

// TestPlanningCostModel_PrimaryVsIndexRungIgnoresSortBearingPlans pins the
// symmetric sort treatment. A plan carrying an in-memory sort has no stake in
// the primary-versus-index trade-off, on EITHER side, so criterion #7 ranks it
// last and the sort-count rung immediately below decides. The pre-fix rung
// applied that guard to the primary side only, which let an
// InMemorySort(IndexScan) win here and pre-empt the sort rung.
func TestPlanningCostModel_PrimaryVsIndexRungIgnoresSortBearingPlans(t *testing.T) {
	t.Parallel()

	shared := rungEqualityRange(t, rungLongLiteral(1))
	extra := rungEqualityRange(t, rungLongLiteral(2))
	primary := rungTypeFilter(
		[]string{"T"},
		rungScan("T").WithScanComparisons([]*predicates.ComparisonRange{shared}),
	)
	sortedIndex := rungSort(rungIndex(
		"idx_sorted_sarg_rich", []*predicates.ComparisonRange{shared, extra}))
	ctx := &prefTestCtx{pref: PreferScan}
	opsPrimary := findExpressionsByType(primary, nil, ctx)
	opsSorted := findExpressionsByType(sortedIndex, nil, ctx)
	if opsPrimary.inMemorySortCount != 0 || opsSorted.inMemorySortCount != 1 {
		t.Fatalf(
			"sort-count precondition = (%d, %d), want (0, 1)",
			opsPrimary.inMemorySortCount, opsSorted.inMemorySortCount,
		)
	}
	// Search-argument-wise this is the pair the previous test says the index
	// wins; the sort is the only difference.
	if distinctSargCount(sortedIndex) <= distinctSargCount(primary) {
		t.Fatalf(
			"adversary missing: sorted index search arguments %d must exceed the primary's %d",
			distinctSargCount(sortedIndex), distinctSargCount(primary),
		)
	}
	if cmp := comparePrimaryScanVsIndexScan(primary, sortedIndex, opsPrimary, opsSorted, PreferScan); cmp >= 0 {
		t.Fatalf("sort-bearing comparison = %d, want the sort-free primary scan to win", cmp)
	}
	assertStrictPlanningPreference(
		t,
		NewPlanningCostModelLessWithContext(nil, ctx),
		primary,
		sortedIndex,
	)
}

// TestPlanningCostModel_FetchCountBeforeFetchDepth makes the lower-total-fetch
// candidate place its implicit index fetch deeper in the tree. Total fetch
// count must win before the fetch-depth preference is consulted.
func TestPlanningCostModel_FetchCountBeforeFetchDepth(t *testing.T) {
	t.Parallel()

	index := func() *plans.RecordQueryIndexPlan {
		return rungIndex("idx_fetch_total", nil)
	}
	lowerTotalDeeper := rungLimit(index(), 10, 0)
	higherTotalShallower := rungFetch(index())

	opsWinner := findExpressionsByType(lowerTotalDeeper, nil, nil)
	opsLoser := findExpressionsByType(higherTotalShallower, nil, nil)
	winnerTotal := opsWinner.indexScanCount + opsWinner.fetchCount
	loserTotal := opsLoser.indexScanCount + opsLoser.fetchCount
	if winnerTotal != 1 || loserTotal != 2 {
		t.Fatalf("total-fetch precondition = (%d, %d), want (1, 2)", winnerTotal, loserTotal)
	}
	winnerDepth := costExprDepth(lowerTotalDeeper, matchFetch)
	loserDepth := costExprDepth(higherTotalShallower, matchFetch)
	if winnerDepth != 1 || loserDepth != 0 {
		t.Fatalf("fetch-depth precondition = (%d, %d), want (1, 0)", winnerDepth, loserDepth)
	}

	assertStrictPlanningPreference(t, PlanningCostModelLess, lowerTotalDeeper, higherTotalShallower)
}

// TestPlanningCostModel_SargRichIndexBeatsSargPoorIndex pins criterion #7's
// contested band on the INDEX-versus-INDEX axis: an index scan that bound a
// search argument beats one that bound none, even when the bound one pays an
// extra explicit Fetch node that the fetch rung below would charge it for.
//
// This is the axis the ranked rung reaches that the pair-restricted form could
// not: the old rung abstained for index-versus-index and the fetch-count rung
// decided, handing the win to the unbound full index scan. A bound index probe
// beating an unbound index scan is the right preference, and the extra Fetch
// node is a structural detail rather than a real cost difference — both
// candidates fetch exactly once, one implicitly and one explicitly.
//
// The isolation is differential. The ONLY change between the two assertions is
// whether the poor side also binds a comparison; giving it one restores the tie
// at this rung and hands the decision back to the fetch-count rung, which
// reverses the winner. A verdict that tracks that single comparison can only
// have come from this rung.
func TestPlanningCostModel_SargRichIndexBeatsSargPoorIndex(t *testing.T) {
	t.Parallel()

	// rich: covering index bound on one column, plus an explicit fetch.
	rich := rungFetch(rungCoveringIndex(rungIndex(
		"idx_sarg_rich_leg",
		[]*predicates.ComparisonRange{rungEqualityRange(t, rungLongLiteral(1))},
	).WithIndexMetadata([]string{"K"}, nil, false)))
	// poor: unbound non-covering index scan, whose fetch is implicit.
	poor := rungIndex("idx_sarg_poor_leg", nil)
	// control: the same shape as poor, but bound like rich.
	control := rungIndex(
		"idx_sarg_control_leg",
		[]*predicates.ComparisonRange{rungEqualityRange(t, rungLongLiteral(1))})

	opsRich := concretePlanCounts(rich, nil)
	opsPoor := concretePlanCounts(poor, nil)
	opsControl := concretePlanCounts(control, nil)
	// Preconditions: all three are singular index scans with a fetch, none has a
	// type filter or a sort, so they all land in the contested band.
	for _, c := range []struct {
		name string
		ops  expressionCounts
	}{{"rich", opsRich}, {"poor", opsPoor}, {"control", opsControl}} {
		if !isSingularIndexScanWithFetch(c.ops) {
			t.Fatalf("%s is not a singular index scan with fetch", c.name)
		}
		if c.ops.typeFilterCount != 0 || c.ops.inMemorySortCount != 0 {
			t.Fatalf("%s must carry neither type filter nor sort, got tf=%d sort=%d",
				c.name, c.ops.typeFilterCount, c.ops.inMemorySortCount)
		}
	}
	if distinctSargCount(rich) != 1 || distinctSargCount(poor) != 0 || distinctSargCount(control) != 1 {
		t.Fatalf("search-argument precondition = (%d, %d, %d), want (1, 0, 1)",
			distinctSargCount(rich), distinctSargCount(poor), distinctSargCount(control))
	}
	// Adversary: the fetch-count rung below prefers the poor side, because its
	// fetch is implicit in the index scan while the rich side spells one out.
	// Total fetches tie, so only the explicit-node count separates them.
	if opsRich.indexScanCount+opsRich.fetchCount != opsPoor.indexScanCount+opsPoor.fetchCount {
		t.Fatalf("total-fetch adversary broken: rich=%d poor=%d",
			opsRich.indexScanCount+opsRich.fetchCount, opsPoor.indexScanCount+opsPoor.fetchCount)
	}
	if opsRich.fetchCount <= opsPoor.fetchCount {
		t.Fatalf("explicit-fetch adversary missing: rich fetches=%d must exceed poor fetches=%d",
			opsRich.fetchCount, opsPoor.fetchCount)
	}

	if cmp := comparePrimaryScanVsIndexScan(rich, poor, opsRich, opsPoor, PreferScan); cmp >= 0 {
		t.Fatalf("rung(rich, poor) = %d, want the bound index to win", cmp)
	}
	assertStrictPlanningPreference(t, PlanningCostModelLess, rich, poor)

	// Differential control: bind the poor side too and the rung ties, so the
	// fetch-count rung decides and the implicit-fetch candidate wins instead.
	if cmp := comparePrimaryScanVsIndexScan(rich, control, opsRich, opsControl, PreferScan); cmp != 0 {
		t.Fatalf("rung(rich, control) = %d, want 0 so the fetch rung can decide", cmp)
	}
	assertStrictPlanningPreference(t, PlanningCostModelLess, control, rich)
}

// TestPlanningCostModel_ExplicitFetchCountBeforeUnmatchedFields isolates the
// last sub-rung in the index-fetch block. Both candidates tie on total fetches
// and fetch depth; fewer explicit Fetch nodes must win even though the later
// unmatched-field rung strongly favors the loser.
func TestPlanningCostModel_ExplicitFetchCountBeforeUnmatchedFields(t *testing.T) {
	t.Parallel()

	// Both candidates carry ONE search argument so criterion #7, which ranks the
	// contested band by search-argument count, ties them and the index-fetch
	// block below actually gets to decide.
	implicit := rungIndex(
		"idx_implicit_fetch",
		[]*predicates.ComparisonRange{rungEqualityRange(t, rungLongLiteral(1))})
	explicitIndex := rungCoveringIndex(rungIndex(
		"idx_explicit_fetch",
		[]*predicates.ComparisonRange{rungEqualityRange(t, rungLongLiteral(1))},
	).WithIndexMetadata([]string{"K"}, nil, false))
	explicit := rungFetch(explicitIndex)
	ctx := &indexTestPlanContext{candidates: []MatchCandidate{
		newKnownDistinctValueIndexCandidate(
			"idx_implicit_fetch",
			[]string{"T"},
			[]string{"A", "B", "C"},
			nil,
			rungRowType(),
			false,
			nil,
		),
		newKnownDistinctValueIndexCandidate(
			"idx_explicit_fetch",
			[]string{"T"},
			[]string{"K"},
			nil,
			rungRowType(),
			false,
			nil,
		),
	}}

	opsWinner := findExpressionsByType(implicit, nil, ctx)
	opsLoser := findExpressionsByType(explicit, nil, ctx)
	winnerTotal := opsWinner.indexScanCount + opsWinner.fetchCount
	loserTotal := opsLoser.indexScanCount + opsLoser.fetchCount
	if winnerTotal != 1 || loserTotal != 1 {
		t.Fatalf("total-fetch precondition = (%d, %d), want (1, 1)", winnerTotal, loserTotal)
	}
	if got := costExprDepth(implicit, matchFetch); got != 0 {
		t.Fatalf("implicit fetch depth = %d, want 0", got)
	}
	if got := costExprDepth(explicit, matchFetch); got != 0 {
		t.Fatalf("explicit fetch depth = %d, want 0", got)
	}
	if opsWinner.fetchCount != 0 || opsLoser.fetchCount != 1 {
		t.Fatalf("explicit-fetch precondition = (%d, %d), want (0, 1)", opsWinner.fetchCount, opsLoser.fetchCount)
	}
	if opsWinner.unmatchedFieldCount != 2 || opsLoser.unmatchedFieldCount != 0 {
		t.Fatalf(
			"unmatched-field adversary = (%d, %d), want (2, 0)",
			opsWinner.unmatchedFieldCount,
			opsLoser.unmatchedFieldCount,
		)
	}

	assertStrictPlanningPreference(
		t,
		NewPlanningCostModelLessWithContext(nil, ctx),
		implicit,
		explicit,
	)
}

func makeNLJPredicateRungCandidates(
	suffix int,
) (morePredicates, fewerPredicates *plans.RecordQueryNestedLoopJoinPlan) {
	outer := rungScan(fmt.Sprintf("OUTER_%d", suffix))
	inner := rungScan(fmt.Sprintf("INNER_%d", suffix))
	first := rungPredicate("A")
	second := rungPredicate("B")
	morePredicates = rungNLJ(
		outer, inner, []predicates.QueryPredicate{first, second})
	fewerPredicates = rungNLJ(
		outer, inner, []predicates.QueryPredicate{predicates.NewAnd(first, second)})
	return morePredicates, fewerPredicates
}

// TestPlanningCostModel_NLJPredicateCountRung keeps residual CNF size equal:
// two one-conjunct predicates versus one two-conjunct AndPredicate. The Go
// extension prefers more predicates attached directly to the NLJ. The fixture
// is hash-adverse so deleting this rung makes the test fail at the hash tie.
func TestPlanningCostModel_NLJPredicateCountRung(t *testing.T) {
	t.Parallel()

	var winner, loser *plans.RecordQueryNestedLoopJoinPlan
	for suffix := 0; suffix < 1_000; suffix++ {
		candidateWinner, candidateLoser := makeNLJPredicateRungCandidates(suffix)
		if costExprHash(candidateLoser) < costExprHash(candidateWinner) {
			winner, loser = candidateWinner, candidateLoser
			break
		}
	}
	if winner == nil {
		t.Fatal("could not construct a hash-adverse NLJ predicate-count fixture")
	}

	if winnerResidual, loserResidual := countResidualPredicatesWithContext(winner, nil), countResidualPredicatesWithContext(loser, nil); winnerResidual != 2 || loserResidual != 2 {
		t.Fatalf("residual-count precondition = (%d, %d), want (2, 2)", winnerResidual, loserResidual)
	}
	opsWinner := findExpressionsByType(winner, nil, nil)
	opsLoser := findExpressionsByType(loser, nil, nil)
	if opsWinner.nljPredicateCount != 2 || opsLoser.nljPredicateCount != 1 {
		t.Fatalf(
			"NLJ-predicate precondition = (%d, %d), want (2, 1)",
			opsWinner.nljPredicateCount,
			opsLoser.nljPredicateCount,
		)
	}
	if cmp := compareJoinOrdering(winner, loser, nil, nil); cmp != 0 {
		t.Fatalf("join-ordering rung must tie before NLJ predicate count, got %d", cmp)
	}

	assertStrictPlanningPreference(t, PlanningCostModelLess, winner, loser)
}

// TestPlanningCostModel_DefaultOnEmptyRung exercises Java's final ordinal
// criterion through the full comparator. Scalar costs tie, and the selected
// fixture makes the hash prefer the DefaultOnEmpty loser, so the count rung is
// the only reason the bare scan wins.
func TestPlanningCostModel_DefaultOnEmptyRung(t *testing.T) {
	t.Parallel()

	var winner plans.RecordQueryPlan
	var loser *plans.RecordQueryDefaultOnEmptyPlan
	for suffix := 0; suffix < 1_000; suffix++ {
		scan := rungScan(fmt.Sprintf("DOE_%d", suffix))
		defaultOnEmpty := mustRungConstruct(plans.NewRecordQueryDefaultOnEmptyPlanFromQuantifier(
			expressions.NewPhysicalQuantifier(expressions.InitialOf(scan)),
			values.NewNullValue(rungRowType()),
		))
		if costExprHash(defaultOnEmpty) < costExprHash(scan) {
			winner, loser = scan, defaultOnEmpty
			break
		}
	}
	if winner == nil {
		t.Fatal("could not construct a hash-adverse DefaultOnEmpty fixture")
	}

	if got := findExpressionsByType(winner, nil, nil).numDefaultOnEmpty; got != 0 {
		t.Fatalf("winner DefaultOnEmpty count = %d, want 0", got)
	}
	if got := findExpressionsByType(loser, nil, nil).numDefaultOnEmpty; got != 1 {
		t.Fatalf("loser DefaultOnEmpty count = %d, want 1", got)
	}
	winnerCost := properties.EstimateCost(winner)
	loserCost := properties.EstimateCost(loser)
	if winnerCost != loserCost {
		t.Fatalf("scalar-cost precondition differs: winner=%+v loser=%+v", winnerCost, loserCost)
	}

	assertStrictPlanningPreference(t, PlanningCostModelLess, winner, loser)
}

func makeScalarFallbackCandidates(
	t *testing.T,
	suffix int,
) (cheaper, costlier plans.RecordQueryPlan) {
	t.Helper()
	indexName := fmt.Sprintf("SCALAR_%d", suffix)
	cheaper = rungIndex(
		indexName,
		[]*predicates.ComparisonRange{
			rungEqualityRange(t, rungLongLiteral(1)),
		},
	)
	rangeComparison := predicates.NewLiteralComparison(predicates.ComparisonGreaterThan, int64(1))
	merged := predicates.EmptyComparisonRange().Merge(&rangeComparison)
	if !merged.Ok {
		t.Fatal("failed to create range comparison")
	}
	costlier = rungIndex(
		indexName,
		[]*predicates.ComparisonRange{merged.Range},
	)
	return cheaper, costlier
}

// TestPlanningCostModel_ScalarFallbackRung uses two non-unique index scans with
// the same ordinal profile. Equality and range bounds are both unbounded for
// the provable-cardinality rung, but the scalar estimator prices equality as
// more selective. The fixture is hash-adverse, proving the scalar
// fallback—not the final hash—selects it.
func TestPlanningCostModel_ScalarFallbackRung(t *testing.T) {
	t.Parallel()

	var winner, loser plans.RecordQueryPlan
	for suffix := 0; suffix < 1_000; suffix++ {
		candidateWinner, candidateLoser := makeScalarFallbackCandidates(t, suffix)
		winnerCost := properties.EstimateCost(candidateWinner)
		loserCost := properties.EstimateCost(candidateLoser)
		if winnerCost.Less(loserCost) &&
			costExprHash(candidateLoser) < costExprHash(candidateWinner) {
			winner, loser = candidateWinner, candidateLoser
			break
		}
	}
	if winner == nil {
		t.Fatal("could not construct a hash-adverse scalar-cost fixture")
	}

	opsWinner := findExpressionsByType(winner, nil, nil)
	opsLoser := findExpressionsByType(loser, nil, nil)
	if opsWinner != opsLoser {
		t.Fatalf("ordinal operator profiles differ: winner=%+v loser=%+v", opsWinner, opsLoser)
	}
	if winnerResidual, loserResidual := countResidualPredicatesWithContext(winner, nil), countResidualPredicatesWithContext(loser, nil); winnerResidual != 0 || loserResidual != 0 {
		t.Fatalf("residual-count precondition = (%d, %d), want (0, 0)", winnerResidual, loserResidual)
	}
	winnerCost := properties.EstimateCost(winner)
	loserCost := properties.EstimateCost(loser)
	if !winnerCost.Less(loserCost) {
		t.Fatalf("scalar-cost precondition missing: winner=%+v loser=%+v", winnerCost, loserCost)
	}

	assertStrictPlanningPreference(t, PlanningCostModelLess, winner, loser)
}

func makeRewritingRungSelect(
	inner *expressions.Reference,
	preds []predicates.QueryPredicate,
) *expressions.SelectExpression {
	quantifier := expressions.ForEachQuantifier(inner)
	flowedType := mustRungConstruct(quantifier.GetFlowedObjectType())
	root := mustRungConstruct(values.NewQuantifiedObjectValue(
		quantifier.GetAlias(), flowedType))
	return mustRungConstruct(expressions.NewSelectExpression(
		root,
		[]expressions.Quantifier{quantifier},
		preds,
	))
}

// TestRewritingCostModel_ResidualConjunctRung covers the third REWRITING
// tier through the real comparator. Select and table-function counts tie; the
// candidate with fewer normalized residual conjuncts must win.
func TestRewritingCostModel_ResidualConjunctRung(t *testing.T) {
	t.Parallel()

	scanRef := expressions.InitialOf(rungFullScan("T"))
	oneConjunct := makeRewritingRungSelect(
		scanRef,
		[]predicates.QueryPredicate{rungPredicate("A")},
	)
	twoConjuncts := makeRewritingRungSelect(
		scanRef,
		[]predicates.QueryPredicate{rungPredicate("A"), rungPredicate("B")},
	)

	scope := newDesignationScope()
	if got := scope.exprCount(oneConjunct, isSelectExpression, map[*expressions.Reference]bool{}); got != 1 {
		t.Fatalf("one-conjunct Select count = %d, want 1", got)
	}
	if got := scope.exprCount(twoConjuncts, isSelectExpression, map[*expressions.Reference]bool{}); got != 1 {
		t.Fatalf("two-conjunct Select count = %d, want 1", got)
	}
	if got := scope.residualConjuncts(oneConjunct, map[*expressions.Reference]bool{}); got != 1 {
		t.Fatalf("one-conjunct residual count = %d, want 1", got)
	}
	if got := scope.residualConjuncts(twoConjuncts, map[*expressions.Reference]bool{}); got != 2 {
		t.Fatalf("two-conjunct residual count = %d, want 2", got)
	}

	assertStrictPlanningPreference(t, RewritingCostModelLess, oneConjunct, twoConjuncts)
}

// TestRewritingCostModel_PredicateDepthRung ties the first three REWRITING
// tiers and moves the same single predicate between the outer and inner
// Select. The pushed-down predicate, closer to the leaf, must win.
func TestRewritingCostModel_PredicateDepthRung(t *testing.T) {
	t.Parallel()

	scanRef := expressions.InitialOf(rungFullScan("T"))
	predicate := rungPredicate("A")

	pushedInner := makeRewritingRungSelect(
		scanRef,
		[]predicates.QueryPredicate{predicate},
	)
	pushed := makeRewritingRungSelect(expressions.InitialOf(pushedInner), nil)

	pulledInner := makeRewritingRungSelect(scanRef, nil)
	pulled := makeRewritingRungSelect(
		expressions.InitialOf(pulledInner),
		[]predicates.QueryPredicate{predicate},
	)

	scope := newDesignationScope()
	if pushedSelects, pulledSelects := scope.exprCount(pushed, isSelectExpression, map[*expressions.Reference]bool{}), scope.exprCount(pulled, isSelectExpression, map[*expressions.Reference]bool{}); pushedSelects != 2 || pulledSelects != 2 {
		t.Fatalf("Select-count precondition = (%d, %d), want (2, 2)", pushedSelects, pulledSelects)
	}
	if pushedResiduals, pulledResiduals := scope.residualConjuncts(pushed, map[*expressions.Reference]bool{}), scope.residualConjuncts(pulled, map[*expressions.Reference]bool{}); pushedResiduals != 1 || pulledResiduals != 1 {
		t.Fatalf("residual-count precondition = (%d, %d), want (1, 1)", pushedResiduals, pulledResiduals)
	}
	pushedByLevel := map[int]int{}
	pulledByLevel := map[int]int{}
	scope.predCountByLevel(pushed, pushedByLevel, map[*expressions.Reference]bool{})
	scope.predCountByLevel(pulled, pulledByLevel, map[*expressions.Reference]bool{})
	if pushedByLevel[1] != 1 || pushedByLevel[2] != 0 || len(pushedByLevel) != 2 {
		t.Fatalf("pushed predicate levels = %v, want map[1:1 2:0]", pushedByLevel)
	}
	if pulledByLevel[1] != 0 || pulledByLevel[2] != 1 || len(pulledByLevel) != 2 {
		t.Fatalf("pulled predicate levels = %v, want map[1:0 2:1]", pulledByLevel)
	}

	assertStrictPlanningPreference(t, RewritingCostModelLess, pushed, pulled)
}
