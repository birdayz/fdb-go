package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

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
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	FireExpressionRule(NewPrimaryScanRule(), scanRef)
	cheap := findPhysicalExpr(scanRef)
	if cheap == nil {
		t.Fatal("PrimaryScanRule yielded no physical scan")
	}

	// Costlier ordered final: an in-memory sort on X (any ordered provider
	// works; the in-memory sort wrapper costs strictly more than the scan).
	ordered := sortedMemberOn(t, "X")

	ref := expressions.InitialOf(expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType))
	ref.InsertFinal(cheap)
	ref.InsertFinal(ordered)

	reqX := NewRequestedOrdering(
		[]RequestedOrderingPart{{Value: values.NewFlatFieldValue("X", values.UnknownType), SortOrder: RequestedSortOrderAscending}},
		DistinctnessPreserveDistinctness, false)

	p := NewPlanner(nil, nil)
	p.constraintMap = NewConstraintMap()
	Set(p.constraintMap, ref, RequestedOrderingConstraintKey, []*RequestedOrdering{reqX})

	// Sanity: the cost model really ranks the scan below the sort, so the
	// ordered member survives ONLY via requested-ordering retention.
	if !PlanningCostModelLess(cheap, ordered) {
		t.Fatal("precondition: the unordered scan must be cheaper than the in-memory sort")
	}

	task := &OptimizeGroupTask{Phase: PhasePlanning, Ref: ref}
	task.Run(p)

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

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	FireExpressionRule(NewPrimaryScanRule(), scanRef)
	cheap := findPhysicalExpr(scanRef)
	if cheap == nil {
		t.Fatal("PrimaryScanRule yielded no physical scan")
	}
	ordered := sortedMemberOn(t, "X")

	ref := expressions.InitialOf(expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType))
	ref.InsertFinal(cheap)
	ref.InsertFinal(ordered)

	p := NewPlanner(nil, nil)
	p.constraintMap = NewConstraintMap()
	// No requested-ordering constraint on ref.

	task := &OptimizeGroupTask{Phase: PhasePlanning, Ref: ref}
	task.Run(p)

	if n := len(ref.FinalMembers()); n != 1 {
		t.Fatalf("with no requested ordering, the group must prune to the single winner; got %d finals", n)
	}
	if ref.FinalMembers()[0] != cheap {
		t.Fatalf("the surviving final must be the cost winner, got %T", ref.FinalMembers()[0])
	}
}

// TestOptimizeInputs_SkipsPrunedExpression ports the identity guard of
// Java's CascadesPlanner.OptimizeInputs (`if (!group.containsExactly(
// expression)) return;`): an OptimizeInputs task whose expression has been
// pruned OUT of its group between push and pop must not drive child-group
// pruning on behalf of a dead parent.
func TestOptimizeInputs_SkipsPrunedExpression(t *testing.T) {
	t.Parallel()

	// Child group with two finals; parent expression ranges over it.
	childScan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	childRef := expressions.InitialOf(childScan)
	FireExpressionRule(NewPrimaryScanRule(), childRef)
	childPhys := findPhysicalExpr(childRef)
	if childPhys == nil {
		t.Fatal("no physical child")
	}
	childRef.InsertFinal(childPhys)
	childRef.InsertFinal(sortedMemberOn(t, "X"))
	childFinals := len(childRef.FinalMembers())

	// The parent expression must genuinely range over childRef.
	parentInner := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	parent := newPhysicalInMemorySortWrapper(
		plans.NewRecordQueryInMemorySortPlan(parentInner, []plans.SortKey{
			{Field: "Y", ValueExpr: values.NewFlatFieldValue("Y", values.UnknownType), NullsFirst: true},
		}),
		expressions.ForEachQuantifier(childRef))
	// The parent enters its group as a FINAL member (as physical yields do)
	// and is then pruned away in favor of another final — the exact state an
	// in-flight OptimizeInputsTask observes when its expression lost.
	parentRef := expressions.InitialOf(expressions.NewFullUnorderedScanExpression([]string{"P"}, values.UnknownType))
	other := sortedMemberOn(t, "Z")
	parentRef.InsertFinal(parent)
	parentRef.InsertFinal(other)
	parentRef.PruneWith(other)

	p := NewPlanner(nil, nil)
	p.constraintMap = NewConstraintMap()
	task := &OptimizeInputsTask{Phase: PhasePlanning, Ref: parentRef, Expr: parent}
	task.Run(p)
	// Drain whatever the task pushed; a correctly-guarded task pushes
	// nothing, so the child group's finals stay intact.
	for len(p.stack) > 0 {
		n := len(p.stack) - 1
		tk := p.stack[n]
		p.stack = p.stack[:n]
		tk.Run(p)
	}

	if got := len(childRef.FinalMembers()); got != childFinals {
		t.Fatalf("a pruned parent expression must not prune child groups: child finals %d -> %d", childFinals, got)
	}
}
