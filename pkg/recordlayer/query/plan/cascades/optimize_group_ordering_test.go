package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func optimizeOrderingRowType() *values.RecordType {
	return values.NewRecordType("OptimizeOrdering", false, []values.Field{
		{Name: "X", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "Y", FieldType: values.NotNullLong, Ordinal: 1},
		{Name: "Z", FieldType: values.NotNullLong, Ordinal: 2},
	})
}

func optimizeOrderingField(t testing.TB, field string) values.Value {
	t.Helper()
	ordinal, ok := map[string]int{"X": 0, "Y": 1, "Z": 2}[field]
	if !ok {
		t.Fatalf("unknown optimize-ordering field %q", field)
	}
	root, rootErr := values.NewQuantifiedObjectValue(
		values.NamedCorrelationIdentifier("optimize_ordering"), optimizeOrderingRowType())
	root = mustConstruct(t, root, rootErr)
	resolved, err := values.ResolveFieldOrdinals(root, []int{ordinal})
	return mustConstruct(t, resolved, err)
}

func optimizeSortedMemberOn(t testing.TB, field string) expressions.RelationalExpression {
	t.Helper()
	inner, innerErr := plans.NewRecordQueryScanPlan(
		[]string{"T"}, optimizeOrderingRowType(), false)
	inner = mustConstruct(t, inner, innerErr)
	innerRef := expressions.InitialOf(inner)
	innerQ := expressions.NamedForEachQuantifier(
		values.NamedCorrelationIdentifier("optimize_ordering"), innerRef)
	sorted, err := plans.NewRecordQueryInMemorySortPlanFromQuantifier(
		innerQ,
		[]plans.SortKey{{Field: field, ValueExpr: optimizeOrderingField(t, field), NullsFirst: true}},
	)
	return mustConstruct(t, sorted, err)
}

// TestOptimizeGroup_RetainsRequestedOrderingWinners pins ordered-alternative
// retention across group pruning: a group holding a CHEAPER unordered final
// and a COSTLIER ordered final, where that ordering was REQUESTED (pushed
// RequestedOrderingConstraint), must not be pruned to the single overall
// cost winner — that destroys the ordered plan before the parent's ordered
// lookup (bestSatisfyingMember) or extraction elision (OrderedChildWinner)
// can choose it, silently forcing an avoidable enforcer sort. OptimizeGroup
// keeps, per pushed requested ordering, the cheapest final satisfying it —
// winner-per-(group, properties) (Graefe 1995 §2), realized in the member
// set.
func TestOptimizeGroup_RetainsRequestedOrderingWinners(t *testing.T) {
	t.Parallel()

	// Cheap unordered final: a plain physical scan.
	scan := mustFullUnorderedScan(t, []string{"T"}, optimizeOrderingRowType())
	scanRef := expressions.InitialOf(scan)
	mustFireExpressionRule(t, NewPrimaryScanRule(), scanRef)
	cheap := findPhysicalExpr(scanRef)
	if cheap == nil {
		t.Fatal("PrimaryScanRule yielded no physical scan")
	}

	// Costlier ordered final: an in-memory sort on X (any ordered provider
	// works; the in-memory sort wrapper costs strictly more than the scan).
	ordered := optimizeSortedMemberOn(t, "X")

	ref := expressions.InitialOf(mustFullUnorderedScan(t, []string{"T"}, optimizeOrderingRowType()))
	ref.InsertFinal(cheap)
	ref.InsertFinal(ordered)

	reqX := properties.NewRequestedOrdering(
		[]properties.RequestedOrderingPart{{Value: optimizeOrderingField(t, "X"), SortOrder: properties.RequestedSortOrderAscending}},
		properties.DistinctnessPreserveDistinctness, false)

	p := NewPlanner(nil, nil)
	p.constraintMap = NewConstraintMap()
	Set(p.constraintMap, ref, RequestedOrderingConstraintKey, []*properties.RequestedOrdering{reqX})

	// Sanity: the cost model really ranks the scan below the sort, so the
	// ordered member survives ONLY via requested-ordering retention.
	if !PlanningCostModelLess(cheap, ordered) {
		t.Fatal("precondition: the unordered scan must be cheaper than the in-memory sort")
	}

	task := &OptimizeGroupTask{Phase: PhasePlanning, Ref: ref}
	task.Run(plannerTestContext(), p)

	if got := ref.Winner(); got != cheap {
		t.Fatalf("overall winner must stay the cost-cheapest member, got %T", got)
	}
	if w := bestSatisfyingMember(ref, reqX, nil); w != ordered {
		t.Fatalf("the requested-ordering winner must survive group pruning; got %T (nil means the ordered alternative was pruned and the parent must now materialise a sort)", w)
	}
}

// TestOptimizeGroup_UnrequestedOrderingStillPruned pins the bound on the
// retention: an ordered final whose ordering NO pushed constraint asks for
// is dead weight and must still be pruned with the rest of the losers.
func TestOptimizeGroup_UnrequestedOrderingStillPruned(t *testing.T) {
	t.Parallel()

	scan := mustFullUnorderedScan(t, []string{"T"}, optimizeOrderingRowType())
	scanRef := expressions.InitialOf(scan)
	mustFireExpressionRule(t, NewPrimaryScanRule(), scanRef)
	cheap := findPhysicalExpr(scanRef)
	if cheap == nil {
		t.Fatal("PrimaryScanRule yielded no physical scan")
	}
	ordered := optimizeSortedMemberOn(t, "X")

	ref := expressions.InitialOf(mustFullUnorderedScan(t, []string{"T"}, optimizeOrderingRowType()))
	ref.InsertFinal(cheap)
	ref.InsertFinal(ordered)

	p := NewPlanner(nil, nil)
	p.constraintMap = NewConstraintMap()
	// No requested-ordering constraint on ref.

	task := &OptimizeGroupTask{Phase: PhasePlanning, Ref: ref}
	task.Run(plannerTestContext(), p)

	if n := len(ref.FinalMembers()); n != 1 {
		t.Fatalf("with no requested ordering, the group must prune to the single winner; got %d finals", n)
	}
	if ref.FinalMembers()[0] != cheap {
		t.Fatalf("the surviving final must be the cost winner, got %T", ref.FinalMembers()[0])
	}
}

// TestOptimizeGroup_RetainsNonOrderingPartitionWinners pins that genuinely
// different non-ordering property classes survive the local cost prune. A
// primary scan is proven distinct; an otherwise same-schema index without a
// candidate duplicate signal is conservatively non-distinct. Ordering remains
// demand-driven (the sibling UnrequestedOrderingStillPruned test pins that
// bound).
func TestOptimizeGroup_RetainsNonOrderingPartitionWinners(t *testing.T) {
	t.Parallel()

	rowType := optimizeOrderingRowType()
	index, indexErr := plans.NewRecordQueryIndexPlan(
		"idx_x", nil, []string{"T"}, rowType, false)
	index = mustConstruct(t, index, indexErr)
	scan, scanErr := plans.NewRecordQueryScanPlan([]string{"T"}, rowType, false)
	scan = mustConstruct(t, scan, scanErr)

	ref := expressions.InitialOf(mustFullUnorderedScan(t, []string{"T"}, rowType))
	ref.InsertFinal(index)
	ref.InsertFinal(scan)

	p := NewPlanner(nil, nil)
	p.constraintMap = NewConstraintMap()
	(&OptimizeGroupTask{Phase: PhasePlanning, Ref: ref}).Run(plannerTestContext(), p)

	contains := func(want expressions.RelationalExpression) bool {
		for _, member := range ref.FinalMembers() {
			if member == want {
				return true
			}
		}
		return false
	}
	if !contains(index) || !contains(scan) {
		t.Fatalf("both distinctness partitions must survive child pruning; finals = %v", ref.FinalMembers())
	}
}

// TestExploreGroup_PinnedPhysicalSelectionDoesNotGrow proves that a restricted
// parent edge is a selection, not a new equivalence group. The ordinary Fetch
// group legitimately admits Fetch(Covering(Index)) -> Index; the pinned edge
// deliberately does not, because growing it would mutate the parent that was
// built over the Fetch onto the Index before root-level cost comparison.
func TestExploreGroup_PinnedPhysicalSelectionDoesNotGrow(t *testing.T) {
	t.Parallel()

	rowType := optimizeOrderingRowType()
	index, indexErr := plans.NewRecordQueryIndexPlan(
		"idx_x", nil, []string{"T"}, rowType, false)
	index = mustConstruct(t, index, indexErr)
	index = index.WithIndexMetadata([]string{"X"}, []string{"Y"}, false)
	covering, coveringErr := plans.NewRecordQueryCoveringIndexPlan(index)
	covering = mustConstruct(t, covering, coveringErr)
	fetch, fetchErr := plans.NewRecordQueryFetchFromPartialRecordPlan(
		covering, nil, rowType, plans.FetchIndexRecordsPrimaryKey)
	fetch = mustConstruct(t, fetch, fetchErr)

	ref := expressions.PinnedFinalOf(fetch)
	p := NewPlanner(nil, nil)
	p.constraintMap = NewConstraintMap()
	(&ExploreGroupTask{Phase: PhasePlanning, Ref: ref}).Run(plannerTestContext(), p)

	if got := len(p.stack); got != 0 {
		t.Fatalf("pinned physical selection scheduled %d exploration tasks, want none", got)
	}
	if finals := ref.FinalMembers(); len(finals) != 1 || finals[0] != fetch {
		t.Fatalf("pinned physical selection grew or changed: finals = %v", finals)
	}
	if got := ref.Winner(); got != fetch {
		t.Fatalf("pinned winner = %T, want the exact Fetch member", got)
	}
}

// TestOptimizeInputs_SkipsPrunedExpression ports the identity guard of
// Java's CascadesPlanner.OptimizeInputs (`if (!group.containsExactly(
// expression)) return;`): an OptimizeInputs task whose expression has been
// pruned OUT of its group between push and pop must not drive child-group
// pruning on behalf of a dead parent.
//
// With dual insertion retired (RFC-181 WS-P stage (b): physical yields
// land ONLY in FinalMembers), a pruned loser is in NO member set, so
// Java's containsExactly (ContainsExactly) is again the exact guard —
// the interim finals-only compensation (ContainsFinal, which existed
// because a dual-inserted loser survived pruning via its exploratory
// copy) reverted to the Java shape, and its dual-inserted-loser twin
// fixture is unconstructible in the planner.
func TestOptimizeInputs_SkipsPrunedExpression(t *testing.T) {
	t.Parallel()

	// Child group with two finals; parent expression ranges over it.
	childScan := mustFullUnorderedScan(t, []string{"T"}, optimizeOrderingRowType())
	childRef := expressions.InitialOf(childScan)
	mustFireExpressionRule(t, NewPrimaryScanRule(), childRef)
	childPhys := findPhysicalExpr(childRef)
	if childPhys == nil {
		t.Fatal("no physical child")
	}
	childRef.InsertFinal(childPhys)
	childRef.InsertFinal(optimizeSortedMemberOn(t, "X"))
	childFinals := len(childRef.FinalMembers())

	// The parent expression must genuinely range over childRef. Since RFC-184 W2
	// the bare in-memory sort carries its child as a LIVE memo edge (no
	// physicalInMemorySortWrapper), so the FromQuantifier ctor ranges it over
	// childRef directly.
	parentQ := expressions.NamedForEachQuantifier(
		values.NamedCorrelationIdentifier("optimize_ordering"), childRef)
	parent, parentErr := plans.NewRecordQueryInMemorySortPlanFromQuantifier(
		parentQ,
		[]plans.SortKey{
			{Field: "Y", ValueExpr: optimizeOrderingField(t, "Y"), NullsFirst: true},
		})
	parent = mustConstruct(t, parent, parentErr)
	// The parent enters its group as a FINAL member (as physical yields do)
	// and is then pruned away in favor of another final — the exact state an
	// in-flight OptimizeInputsTask observes when its expression lost.
	parentRef := expressions.InitialOf(mustFullUnorderedScan(t, []string{"P"}, optimizeOrderingRowType()))
	other := optimizeSortedMemberOn(t, "Z")
	parentRef.InsertFinal(parent)
	parentRef.InsertFinal(other)
	parentRef.PruneWith(other)

	p := NewPlanner(nil, nil)
	p.constraintMap = NewConstraintMap()
	task := &OptimizeInputsTask{Phase: PhasePlanning, Ref: parentRef, Expr: parent}
	task.Run(plannerTestContext(), p)
	// Drain whatever the task pushed; a correctly-guarded task pushes
	// nothing, so the child group's finals stay intact.
	for len(p.stack) > 0 {
		n := len(p.stack) - 1
		tk := p.stack[n]
		p.stack = p.stack[:n]
		tk.Run(plannerTestContext(), p)
	}

	if got := len(childRef.FinalMembers()); got != childFinals {
		t.Fatalf("a pruned parent expression must not prune child groups: child finals %d -> %d", childFinals, got)
	}
}
