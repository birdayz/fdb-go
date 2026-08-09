package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func TestImplementStreamingAgg_UnorderedScanFires(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"Orders"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	gb := expressions.NewGroupByExpression(
		[]values.Value{&values.FieldValue{Field: "customer_id", Typ: values.UnknownType}},
		[]expressions.AggregateSpec{
			{Function: expressions.AggCount, Operand: &values.FieldValue{Field: "id", Typ: values.UnknownType}},
		},
		scanQ,
	)
	gbRef := expressions.InitialOf(gb)

	FireExpressionRule(NewPrimaryScanRule(), scanRef)

	// Streaming agg is the only aggregation implementation, so it fires
	// regardless of input ordering.
	results := FireExpressionRule(NewImplementStreamingAggregationRule(), gbRef)
	if len(results) == 0 {
		t.Fatal("streaming agg should fire over unordered scan (only agg implementation)")
	}
}

func TestImplementStreamingAgg_UnorderedInput_Fires(t *testing.T) {
	t.Parallel()

	// GroupBy over a scan with no sort — streaming agg fires regardless
	// since it is the only aggregation implementation.
	scan := expressions.NewFullUnorderedScanExpression([]string{"Orders"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	gb := expressions.NewGroupByExpression(
		[]values.Value{&values.FieldValue{Field: "region", Typ: values.UnknownType}},
		[]expressions.AggregateSpec{
			{Function: expressions.AggSum, Operand: &values.FieldValue{Field: "amount", Typ: values.UnknownType}},
		},
		scanQ,
	)
	gbRef := expressions.InitialOf(gb)

	// Physicalize the scan only (no sort).
	FireExpressionRule(NewPrimaryScanRule(), scanRef)

	results := FireExpressionRule(NewImplementStreamingAggregationRule(), gbRef)
	if len(results) == 0 {
		t.Fatal("ImplementStreamingAggregationRule should fire with unordered input (only agg implementation)")
	}
}

func TestImplementStreamingAgg_IndexOrderedInput(t *testing.T) {
	t.Parallel()

	// Sort(customer_id) over Scan, with an index on (customer_id).
	// OrderedIndexScanRule produces an index scan ordered by customer_id.
	// GroupBy(customer_id) should then get a streaming aggregation.
	a1 := values.UniqueCorrelationIdentifier()
	cand := newKnownDistinctValueIndexCandidate(
		"idx_orders_cid",
		[]string{"Orders"},
		[]string{"customer_id"},
		[]values.CorrelationIdentifier{a1},
		values.UnknownType,
		false,
		nil,
	)
	ctx := &indexTestPlanContext{candidates: []MatchCandidate{cand}}

	scan := expressions.NewFullUnorderedScanExpression([]string{"Orders"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	sortExpr := expressions.NewLogicalSortExpression(
		[]expressions.SortKey{
			{Value: &values.FieldValue{Field: "customer_id", Typ: values.UnknownType}},
		}, scanQ)
	sortRef := expressions.InitialOf(sortExpr)
	sortQ := expressions.ForEachQuantifier(sortRef)

	gb := expressions.NewGroupByExpression(
		[]values.Value{&values.FieldValue{Field: "customer_id", Typ: values.UnknownType}},
		[]expressions.AggregateSpec{
			{Function: expressions.AggCount, Operand: &values.FieldValue{Field: "id", Typ: values.UnknownType}},
		},
		sortQ,
	)
	gbRef := expressions.InitialOf(gb)

	// OrderedIndexScanRule replaces Sort(Scan) with an index scan.
	FireExpressionRuleWithMemo(NewOrderedIndexScanRule(), sortRef, ctx, nil)

	// Now fire streaming agg — the inner (sortRef) has an index scan
	// member with ordering on customer_id.
	results := FireExpressionRule(NewImplementStreamingAggregationRule(), gbRef)
	if len(results) == 0 {
		t.Fatal("ImplementStreamingAggregationRule didn't fire with index-ordered input")
	}

	// Since RFC-184 W2 the memo holds the bare *plans.RecordQueryStreamingAggregationPlan.
	agg := results[0].(*plans.RecordQueryStreamingAggregationPlan)
	explain := agg.Explain()
	if explain == "" {
		t.Fatal("empty explain string")
	}
}

func TestImplementStreamingAgg_EmptyGroupingKeys(t *testing.T) {
	t.Parallel()

	// No grouping keys — global aggregate (COUNT(*) with no GROUP BY).
	// Should fire unconditionally (no ordering requirement).
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	gb := expressions.NewGroupByExpression(
		nil,
		[]expressions.AggregateSpec{
			{Function: expressions.AggCount, Operand: &values.FieldValue{Field: "x", Typ: values.UnknownType}},
		},
		scanQ,
	)
	gbRef := expressions.InitialOf(gb)

	// Physicalize the scan.
	FireExpressionRule(NewPrimaryScanRule(), scanRef)

	results := FireExpressionRule(NewImplementStreamingAggregationRule(), gbRef)
	if len(results) == 0 {
		t.Fatal("ImplementStreamingAggregationRule should fire with empty grouping keys (global aggregate)")
	}
}

// TestImplementStreamingAgg_CountCoveringRequiresDistinctIndexRecords pins the
// zero-column covering shortcut for COUNT(*). Covering a raw fan-out index
// would count index entries rather than records; an index whose distinctness
// signal is unknown must also decline. A known scalar index can still become a
// zero-column covering scan.
func TestImplementStreamingAgg_CountCoveringRequiresDistinctIndexRecords(t *testing.T) {
	t.Parallel()

	fanOut := true
	scalar := false
	tests := []struct {
		name         string
		signal       *bool
		wantCovering bool
	}{
		{name: "fanout", signal: &fanOut, wantCovering: false},
		{name: "unknown", signal: nil, wantCovering: false},
		{name: "scalar", signal: &scalar, wantCovering: true},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			indexPlan := plans.NewRecordQueryIndexPlan(
				"T$v",
				nil,
				[]string{"T"},
				values.UnknownType,
				false,
			)
			if tc.signal != nil {
				indexPlan = indexPlan.WithDistinctRecordsSignal(*tc.signal)
			}
			// The PRODUCTION shape, and it has to be exact for this test to
			// discriminate at all (RFC-220): the access path emits
			// Fetch(Covering(IndexScan)), and the count-only shortcut reaches PAST
			// the fetch to the covering child — that reach is what the fan-out guard
			// gates. Putting the covering plan directly in this group instead would
			// let the rule's general "aggregate over every physical alternative"
			// loop yield an aggregate over it with no guard consulted, and the
			// assertion below would read TRUE for every case, guard or no guard.
			coveringRef := expressions.InitialOf(plans.NewRecordQueryCoveringIndexPlan(indexPlan))
			fetchPlan := plans.NewRecordQueryFetchFromPartialRecordPlanFromQuantifier(
				expressions.ForEachQuantifier(coveringRef),
				nil, values.UnknownType, plans.FetchIndexRecordsPrimaryKey,
			)
			innerRef := expressions.InitialOf(fetchPlan)
			groupBy := expressions.NewGroupByExpression(
				nil,
				[]expressions.AggregateSpec{{Function: expressions.AggCount}},
				expressions.ForEachQuantifier(innerRef),
			)
			results := FireExpressionRule(
				NewImplementStreamingAggregationRule(),
				expressions.InitialOf(groupBy),
			)

			foundCovering := false
			for _, result := range results {
				agg, ok := result.(*plans.RecordQueryStreamingAggregationPlan)
				if !ok {
					continue
				}
				if _, ok := agg.GetInner().(*plans.RecordQueryCoveringIndexPlan); ok {
					foundCovering = true
				}
			}
			if foundCovering != tc.wantCovering {
				t.Fatalf("covering COUNT(*) index path = %v, want %v (signal %v)", foundCovering, tc.wantCovering, tc.signal)
			}
		})
	}
}

func TestStreamingAggPlan_Explain(t *testing.T) {
	t.Parallel()

	inner := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	plan := plans.NewRecordQueryStreamingAggregationPlan(
		inner,
		[]values.Value{
			&values.FieldValue{Field: "a", Typ: values.UnknownType},
			&values.FieldValue{Field: "b", Typ: values.UnknownType},
		},
		[]expressions.AggregateSpec{
			{Function: expressions.AggSum, Operand: &values.FieldValue{Field: "x", Typ: values.UnknownType}},
		},
	)

	got := plan.Explain()
	want := "StreamingAgg(keys=[a, b], Scan(T))"
	if got != want {
		t.Fatalf("Explain() = %q, want %q", got, want)
	}
}

func TestStreamingAggPlan_EqualityAndHash(t *testing.T) {
	t.Parallel()

	inner := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	p1 := plans.NewRecordQueryStreamingAggregationPlan(
		inner,
		[]values.Value{&values.FieldValue{Field: "a", Typ: values.UnknownType}},
		[]expressions.AggregateSpec{
			{Function: expressions.AggCount, Operand: &values.FieldValue{Field: "x", Typ: values.UnknownType}},
		},
	)
	p2 := plans.NewRecordQueryStreamingAggregationPlan(
		inner,
		[]values.Value{&values.FieldValue{Field: "a", Typ: values.UnknownType}},
		[]expressions.AggregateSpec{
			{Function: expressions.AggCount, Operand: &values.FieldValue{Field: "x", Typ: values.UnknownType}},
		},
	)
	p3 := plans.NewRecordQueryStreamingAggregationPlan(
		inner,
		[]values.Value{&values.FieldValue{Field: "b", Typ: values.UnknownType}},
		[]expressions.AggregateSpec{
			{Function: expressions.AggSum, Operand: &values.FieldValue{Field: "y", Typ: values.UnknownType}},
		},
	)

	if !p1.EqualsPlanWithoutChildren(p2) {
		t.Fatal("identical plans should be equal")
	}
	if p1.EqualsPlanWithoutChildren(p3) {
		t.Fatal("different plans should not be equal")
	}
	if p1.HashCodeWithoutChildren() != p2.HashCodeWithoutChildren() {
		t.Fatal("identical plans should have same hash")
	}
}

// TestStreamingAgg_OrderedChildPinned pins RFC-181 P0.2: the ordered
// alternative's child must be a PINNED singleton, never the SHARED inner
// group — the agg wrapper's WithChildren relinks at extraction to whatever
// leaf-replaceable plan its child group resolves to, and the shared
// multi-member group can resolve to a cheaper UNORDERED member, silently
// splitting groups (grouping-key order is a correctness precondition of
// streaming aggregation). Driven through the REAL planner: the test-driver
// shortcut (FireExpressionRule) has no memo, so MemoizeExpression already
// degrades to a fresh singleton there and cannot exhibit the shared-group
// leak.
func TestStreamingAgg_OrderedChildPinned(t *testing.T) {
	t.Parallel()

	a1 := values.UniqueCorrelationIdentifier()
	cand := newKnownDistinctValueIndexCandidate(
		"T$g", []string{"T"}, []string{"G"},
		[]values.CorrelationIdentifier{a1}, values.UnknownType, false, nil)
	ctx := &indexTestPlanContext{candidates: []MatchCandidate{cand}}

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	innerRef := expressions.InitialOf(scan)
	groupKey := &values.FieldValue{Field: "G", Typ: values.UnknownType}
	gb := expressions.NewGroupByExpression(
		[]values.Value{groupKey}, nil,
		expressions.ForEachQuantifier(innerRef),
	)
	topRef := expressions.InitialOf(gb)

	p := NewPlanner(DefaultExpressionRules(), ctx).
		WithPlanningExpressionRules(BatchAExpressionRules()).
		WithImplementationRules(DefaultImplementationRules())
	if _, _, err := p.Plan(topRef); err != nil {
		t.Fatalf("Plan: %v", err)
	}

	// Find the ordered streaming-agg alternative: its plan child is the
	// index scan (not the in-memory sort).
	// Since RFC-184 W2 the memo holds the bare *plans.RecordQueryStreamingAggregationPlan.
	var orderedAgg *plans.RecordQueryStreamingAggregationPlan
	for _, m := range topRef.AllMembers() {
		w, ok := m.(*plans.RecordQueryStreamingAggregationPlan)
		if !ok {
			continue
		}
		// EITHER scan type counts. The claim under test is that the ordered
		// alternative reads the index DIRECTLY rather than sorting in memory;
		// whether that direct read is a fetching scan or a covering one is
		// RFC-220's concern, not this pin's. Since RFC-220 the access path emits
		// Fetch(Covering(IndexScan)), so the aggregate's child is the covering
		// plan — a type change, not a lost alternative.
		switch w.GetInner().(type) {
		case *plans.RecordQueryIndexPlan, *plans.RecordQueryCoveringIndexPlan:
			orderedAgg = w
		}
		if orderedAgg != nil {
			break
		}
	}
	if orderedAgg == nil {
		t.Fatal("the planner stopped yielding the ordered streaming-agg alternative for this shape — the invariant sentinel would evaporate; fix the yield, don't skip")
	}

	childRef := orderedAgg.GetInnerQuantifier().GetRangesOver()
	members := childRef.AllMembers()
	if len(members) != 1 {
		t.Fatalf("the ordered agg's child must be a PINNED singleton; got %d members — the shared inner group leaks the unordered winner into the extraction relink", len(members))
	}
	if got := findPhysicalPlan(childRef); got != orderedAgg.GetInner() {
		t.Fatalf("findPhysicalPlan over the pinned child = %T, want the ordered index plan — extraction would relink the agg onto an unordered child", got)
	}
}

// TestImplementStreamingAgg_PinsOrderedSpine: when the ordered member the
// rule selects is an order-preserving DELEGATOR (a predicates filter whose
// HintOrdering reports the ordering of SOME source member) over a group
// whose WINNER is a cheaper UNORDERED scan, yielding FinalOf(wrapper)
// alone pins only the wrapper — extraction's generic rebuild relinks the
// wrapper's source group to the unordered winner, and the streaming
// aggregate runs over unordered input: equal keys arrive in separate runs
// and the groups SILENTLY SPLIT. The yield must pin the whole ordering
// spine (pinOrderedSpine, executable-child verified) so the filter's
// source group is a singleton holding the ordered member.
func TestImplementStreamingAgg_PinsOrderedSpine(t *testing.T) {
	t.Parallel()

	ordered := sortedMemberOn(t, "S")
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	srcRef := expressions.InitialOf(scan)
	FireExpressionRule(NewPrimaryScanRule(), srcRef)
	cheap := findPhysicalExpr(srcRef)
	if cheap == nil {
		t.Fatal("no physical scan")
	}
	srcRef.InsertFinal(cheap)
	srcRef.Insert(ordered)
	srcRef.InsertFinal(ordered)
	srcRef.SetWinner(cheap)

	filterWrapper := plans.NewRecordQueryPredicatesFilterPlan(
		plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false),
		[]predicates.QueryPredicate{predicates.NewConstantPredicate(predicates.TriTrue)},
	).WithQuantifiers([]expressions.Quantifier{expressions.ForEachQuantifier(srcRef)})
	innerRef := expressions.InitialOf(filterWrapper)

	gb := expressions.NewGroupByExpression(
		[]values.Value{&values.FieldValue{Field: "S", Typ: values.UnknownType}},
		[]expressions.AggregateSpec{
			{Function: expressions.AggCount, Operand: &values.FieldValue{Field: "S", Typ: values.UnknownType}},
		},
		expressions.ForEachQuantifier(innerRef),
	)
	gbRef := expressions.InitialOf(gb)

	yielded := FireExpressionRule(NewImplementStreamingAggregationRule(), gbRef)
	if len(yielded) == 0 {
		t.Fatal("rule should fire")
	}
	// Find the ORDERED alternative: the agg wrapper whose child resolves to
	// a predicates-filter wrapper (the delegator), not the in-memory sort.
	// Since RFC-184 W2 the memo holds the bare *plans.RecordQueryStreamingAggregationPlan.
	var orderedAgg *plans.RecordQueryStreamingAggregationPlan
	var pinnedFilter *plans.RecordQueryPredicatesFilterPlan
	for _, y := range yielded {
		aggW, ok := y.(*plans.RecordQueryStreamingAggregationPlan)
		if !ok {
			continue
		}
		qs := aggW.GetQuantifiers()
		if len(qs) != 1 {
			continue
		}
		for _, m := range qs[0].GetRangesOver().AllMembers() {
			if fw, isFilter := m.(*plans.RecordQueryPredicatesFilterPlan); isFilter {
				orderedAgg = aggW
				pinnedFilter = fw
			}
		}
	}
	if orderedAgg == nil {
		t.Fatal("no ordered (filter-delegator) streaming-agg alternative was yielded")
	}
	// The filter's OWN source group must be pinned to the ORDERED member —
	// a group still holding the unordered winner rebuilds unordered at
	// extraction and splits aggregate groups.
	fqs := pinnedFilter.GetQuantifiers()
	if len(fqs) != 1 {
		t.Fatalf("filter wrapper must keep one quantifier, got %d", len(fqs))
	}
	members := fqs[0].GetRangesOver().AllMembers()
	if len(members) != 1 || members[0] != ordered {
		t.Fatalf("the ordered alternative's filter source group must be a SINGLETON holding the ordered member (spine pinned); got %d members, winner unordered=%v",
			len(members), fqs[0].GetRangesOver().Winner() == cheap)
	}
	// And the pin must reach the EXECUTABLE plan: the filter's concrete
	// child is the ordered member's plan, not the stale scan.
	orderedPE := ordered.(physicalPlanExpression)
	if !planHasDirectChild(pinnedFilter.GetRecordQueryPlan(), orderedPE.GetRecordQueryPlan()) {
		t.Fatal("the pinned filter's concrete plan does not execute the ordered child (pin did not reach the executable plan)")
	}
}

// coveringInnerGT builds a `> v` ComparisonRange — a SARG that RESTRICTS the
// scan without collapsing the leading column to a constant, so the index's
// ordering claim on that column survives and the scan is still selective.
func coveringInnerGT(t *testing.T, v any) *predicates.ComparisonRange {
	t.Helper()
	cmp := predicates.NewLiteralComparison(predicates.ComparisonGreaterThan, v)
	res := predicates.EmptyComparisonRange().Merge(&cmp)
	if !res.Ok {
		t.Fatalf("failed to build > range for %v", v)
	}
	return res.Range
}

// coveringInnerFetch builds the shape the access path now emits for every
// value-index access: Fetch(Covering(IndexScan)), with the covering scan held
// in its own Reference as the fetch's live child edge.
//
// `comps` nil gives a FULL-RANGE scan; a non-empty range gives a SARG'd one.
// The two differ only in that field, which is the whole point: a walker has to
// reach past the covering wrapper to tell them apart.
func coveringInnerFetch(indexName string, comps []*predicates.ComparisonRange) *plans.RecordQueryFetchFromPartialRecordPlan {
	idx := plans.NewRecordQueryIndexPlan(indexName, comps, []string{"Orders"}, values.UnknownType, false).
		WithIndexMetadata([]string{"customer_id", "amount"}, []string{"id"}, false)
	cov := plans.NewRecordQueryCoveringIndexPlan(idx)
	covQ := expressions.ForEachQuantifier(expressions.InitialOf(cov))
	return plans.NewRecordQueryFetchFromPartialRecordPlanFromQuantifier(
		covQ, nil, values.UnknownType, plans.FetchIndexRecordsPrimaryKey)
}

// groupByOverInnerRef builds GROUP BY customer_id, COUNT(id) over innerRef.
func groupByOverInnerRef(innerRef *expressions.Reference) *expressions.Reference {
	gb := expressions.NewGroupByExpression(
		[]values.Value{&values.FieldValue{Field: "customer_id", Typ: values.UnknownType}},
		[]expressions.AggregateSpec{
			{Function: expressions.AggCount, Operand: &values.FieldValue{Field: "id", Typ: values.UnknownType}},
		},
		expressions.ForEachQuantifier(innerRef),
	)
	return expressions.InitialOf(gb)
}

// aggInnerIsFetch reports whether any yielded streaming aggregation carries a
// Fetch as its inner — i.e. whether the selective-Fetch ALTERNATIVE was
// yielded at all.
//
// This asks about YIELD, not about the final plan shape, and the distinction is
// why the regression it guards was invisible. A plan that is never CONSTRUCTED
// is not a losing plan: no cost comparison mentions it, no plan dump shows it,
// and every golden stays byte-identical while the planner's choice set silently
// shrank by one.
func aggInnerIsFetch(yielded []expressions.RelationalExpression) bool {
	for _, y := range yielded {
		agg, ok := y.(*plans.RecordQueryStreamingAggregationPlan)
		if !ok {
			continue
		}
		if _, isFetch := agg.GetInner().(*plans.RecordQueryFetchFromPartialRecordPlan); isFetch {
			return true
		}
	}
	return false
}

// TestImplementStreamingAgg_SelectiveCoveringFetchAlternativeIsYielded pins
// that the ordered streaming-aggregation-over-selective-Fetch alternative is
// yielded for a SARG'd fetch and skipped for a full-range one.
//
// The rule declines an ordered Fetch whose inner scans the whole index, because
// reading every row through random PK lookups always loses to a sequential scan
// plus an in-memory sort. That decision needs the scan's comparison ranges, and
// the ranges live under the covering wrapper the access path now always builds.
// A walker that stops at the wrapper sees no ranges and therefore reads EVERY
// fetch as full-range, so the alternative stops being constructed for every
// query — the aggregate falls back to sorting the entire table even where a
// selective index scan already delivers the grouping order.
//
// Both arms are driven from the same shape so the test cannot pass by the rule
// yielding (or refusing) uniformly.
func TestImplementStreamingAgg_SelectiveCoveringFetchAlternativeIsYielded(t *testing.T) {
	t.Parallel()

	t.Run("sargd_fetch_yields_the_alternative", func(t *testing.T) {
		t.Parallel()

		fetch := coveringInnerFetch("idx_orders_cid", []*predicates.ComparisonRange{coveringInnerGT(t, int64(5))})
		yielded := FireExpressionRule(NewImplementStreamingAggregationRule(), groupByOverInnerRef(expressions.InitialOf(fetch)))
		if len(yielded) == 0 {
			t.Fatal("rule should fire over an ordered covering-backed fetch")
		}
		if !aggInnerIsFetch(yielded) {
			t.Fatalf("the selective-Fetch alternative was NOT yielded (%d alternatives, none over a Fetch) — "+
				"the full-range test cannot see past the covering wrapper, so every fetch reads as full-range "+
				"and the aggregate falls back to sorting the whole table", len(yielded))
		}
	})

	t.Run("full_range_fetch_does_not_yield_the_alternative", func(t *testing.T) {
		t.Parallel()

		fetch := coveringInnerFetch("idx_orders_cid", nil)
		yielded := FireExpressionRule(NewImplementStreamingAggregationRule(), groupByOverInnerRef(expressions.InitialOf(fetch)))
		if len(yielded) == 0 {
			t.Fatal("rule should still yield the InMemorySort alternative over a full-range fetch")
		}
		if aggInnerIsFetch(yielded) {
			t.Fatal("a FULL-RANGE Fetch alternative was yielded: reading every row through random PK " +
				"lookups is strictly worse than a sequential scan plus an in-memory sort, so it must not be built")
		}
	})
}

// TestIsFullRangeFetch_SeesPastTheCoveringWrapper drives the decision directly,
// so the full-range/SARG'd distinction is pinned independently of whatever else
// the aggregation rule requires of an inner.
func TestIsFullRangeFetch_SeesPastTheCoveringWrapper(t *testing.T) {
	t.Parallel()

	if isFullRangeFetch(coveringInnerFetch("idx_a", []*predicates.ComparisonRange{coveringInnerGT(t, int64(5))})) {
		t.Fatal("a fetch over a SARG'd covering scan reads as full-range: the comparison ranges live under the covering wrapper")
	}
	if !isFullRangeFetch(coveringInnerFetch("idx_a", nil)) {
		t.Fatal("a fetch over an unrestricted covering scan must read as full-range")
	}
}

// orderedIndexScanOn builds a forward scan of an index leading with
// customer_id, so its ordering satisfies GROUP BY customer_id.
func orderedIndexScanOn(indexName string) *plans.RecordQueryIndexPlan {
	return plans.NewRecordQueryIndexPlan(indexName, nil, []string{"Orders"}, values.UnknownType, false).
		WithIndexMetadata([]string{"customer_id", "amount"}, []string{"id"}, false)
}

// TestFindOrderedPhysicalExprs_PrefersFinalsOverExploratory pins the member-set
// policy: when a reference holds physical FINAL members, only those are
// enumerated; exploratory members are a fallback for references that hold no
// finals, never an addition to them.
//
// This is Java's policy, not a Go convenience. The plan partitions Java's
// ImplementStreamingAggregationRule matches over come from
// Reference.toPlanPartitions, backed by the reference's propertiesMap, and that
// map is fed only by FINAL expressions — at construction (Reference.java:182)
// and in insertUnchecked under `if (isFinal)` (Reference.java:372-378). An
// exploratory member is invisible to the Java rule.
//
// The pin exists because the alternative policy is silently plausible:
// enumerating both sets never drops an alternative, so nothing fails, and the
// rule quietly builds parents over members the cost framework has not admitted
// as plans. Two member-set policies for one concept is how the next divergence
// starts.
func TestFindOrderedPhysicalExprs_PrefersFinalsOverExploratory(t *testing.T) {
	t.Parallel()

	groupingKeys := []values.Value{&values.FieldValue{Field: "customer_id", Typ: values.UnknownType}}

	exploratory := orderedIndexScanOn("idx_exploratory")
	final := orderedIndexScanOn("idx_final")
	ref := expressions.InitialOf(exploratory)
	ref.InsertFinal(final)

	got := findOrderedPhysicalExprs(ref, groupingKeys)
	if len(got) != 1 || got[0] != final {
		t.Fatalf("with a physical FINAL present, enumeration must return only finals; got %d members (final present=%v, exploratory present=%v)",
			len(got), slicesContainsExpr(got, final), slicesContainsExpr(got, exploratory))
	}

	// Fallback arm: a reference holding no finals still enumerates its
	// exploratory members — a rule firing mid-planning would otherwise see an
	// empty group and yield nothing at all.
	exploratoryOnly := expressions.InitialOf(orderedIndexScanOn("idx_only"))
	if len(findOrderedPhysicalExprs(exploratoryOnly, groupingKeys)) != 1 {
		t.Fatal("a reference with no finals must fall back to its exploratory members")
	}
}

func slicesContainsExpr(xs []expressions.RelationalExpression, want expressions.RelationalExpression) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

// countableCoveringFetch builds Fetch(Covering(IndexScan)) over a
// duplicate-free index — the shape a bare COUNT can answer straight from the
// index entries, with no primary-key fetch.
func countableCoveringFetch(indexName string) *plans.RecordQueryFetchFromPartialRecordPlan {
	idx := plans.NewRecordQueryIndexPlan(indexName, nil, []string{"Orders"}, values.UnknownType, false).
		WithIndexMetadata([]string{"customer_id"}, []string{"id"}, false).
		WithDistinctRecordsSignal(false)
	cov := plans.NewRecordQueryCoveringIndexPlan(idx)
	covQ := expressions.ForEachQuantifier(expressions.InitialOf(cov))
	return plans.NewRecordQueryFetchFromPartialRecordPlanFromQuantifier(
		covQ, nil, values.UnknownType, plans.FetchIndexRecordsPrimaryKey)
}

// TestImplementStreamingAgg_EveryCoveringScanYieldsItsOwnCountAlternative pins
// that a bare COUNT over a group offering N distinct covering scans yields N
// count-from-the-index alternatives, one per scan.
//
// The scans are genuinely different plans — different indexes, therefore
// different entry widths and different scan costs — so choosing between them is
// the cost model's job. Taking the first at rule time is not a cheap
// approximation of that choice; it deletes the others from the search space,
// where no cost comparison and no plan dump can report their absence. Java's
// onMatch yields once per candidate plan partition
// (ImplementStreamingAggregationRule.java:117-122) and memoizes ALL of a
// partition's plans into the new inner reference (:130); it never picks one.
func TestImplementStreamingAgg_EveryCoveringScanYieldsItsOwnCountAlternative(t *testing.T) {
	t.Parallel()

	narrow := countableCoveringFetch("idx_orders_cid")
	wide := countableCoveringFetch("idx_orders_cid_amount")
	innerRef := expressions.InitialOf(narrow)
	innerRef.Insert(wide)

	gb := expressions.NewGroupByExpression(
		nil, // no grouping keys: a bare COUNT
		[]expressions.AggregateSpec{{Function: expressions.AggCount}},
		expressions.ForEachQuantifier(innerRef),
	)
	yielded := FireExpressionRule(NewImplementStreamingAggregationRule(), expressions.InitialOf(gb))

	// Count the DISTINCT covering scans that were given a parent. Yields over
	// the fetches themselves are the separate, non-covering alternative and are
	// deliberately not counted here.
	seen := map[*plans.RecordQueryCoveringIndexPlan]bool{}
	for _, y := range yielded {
		agg, ok := y.(*plans.RecordQueryStreamingAggregationPlan)
		if !ok {
			continue
		}
		if cov, isCov := agg.GetInner().(*plans.RecordQueryCoveringIndexPlan); isCov {
			seen[cov] = true
		}
	}
	if len(seen) != 2 {
		t.Fatalf("a group offering 2 distinct covering scans yielded %d count-from-the-index alternatives; "+
			"want 2 — a rule-time single pick makes the runner-up invisible to the cost model rather than out-priced by it",
			len(seen))
	}
}
