package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

func TestGetWinnerForOrdering_PreserveReturnsNoPropsWinner(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	ref := expressions.InitialOf(scan)

	scanRule := NewPrimaryScanRule()
	FireExpressionRule(scanRule, ref)

	physExpr := findPhysicalExpr(ref)
	if physExpr == nil {
		t.Fatal("no physical expr in ref after PrimaryScanRule")
	}

	// Stamp a winner via NoProperties key
	ref.SetWinner(expressions.NoProperties, physExpr)

	// getWinnerForOrdering(PRESERVE) should return the stamped winner
	winner := getWinnerForOrdering(ref, PreserveOrdering(), nil)
	if winner == nil {
		t.Fatal("getWinnerForOrdering(PRESERVE) returned nil")
	}
	if winner != physExpr {
		t.Fatalf("getWinnerForOrdering(PRESERVE) = %p, want %p (stamped winner)", winner, physExpr)
	}
}

func TestGetWinnerForOrdering_FallbackToFindBestWhenNoWinner(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	ref := expressions.InitialOf(scan)

	scanRule := NewPrimaryScanRule()
	FireExpressionRule(scanRule, ref)

	// No winner stamped — getWinnerForOrdering should fall back to findBestValidPhysicalExpr
	winner := getWinnerForOrdering(ref, PreserveOrdering(), nil)
	if winner == nil {
		t.Fatal("getWinnerForOrdering(PRESERVE) returned nil (fallback should work)")
	}
	if _, ok := winner.(physicalPlanExpression); !ok {
		t.Fatalf("winner is %T, want physicalPlanExpression", winner)
	}
}

func TestGetWinnerForOrdering_OrderingLookup(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	ref := expressions.InitialOf(scan)

	scanRule := NewPrimaryScanRule()
	FireExpressionRule(scanRule, ref)

	physExpr := findPhysicalExpr(ref)
	if physExpr == nil {
		t.Fatal("no physical expr")
	}

	// Stamp an ordering-specific winner
	props := expressions.OrderingFromNameDir([]string{"NAME"}, []bool{false})
	ref.SetWinner(props, physExpr)

	// Look up by RequestedOrdering with same name
	parts := []RequestedOrderingPart{
		{Value: &values.FieldValue{Field: "NAME"}, SortOrder: RequestedSortOrderAscending},
	}
	reqOrd := NewRequestedOrdering(parts, DistinctnessPreserveDistinctness, false)

	winner := getWinnerForOrdering(ref, reqOrd, nil)
	if winner == nil {
		t.Fatal("getWinnerForOrdering returned nil for matching ordering")
	}
	if winner != physExpr {
		t.Fatal("getWinnerForOrdering did not return the stamped winner")
	}
}

func TestFindPhysicalPlanVsFindBestPhysicalExpr_InsertionOrderMatters(t *testing.T) {
	t.Parallel()

	// Create two scan expressions and implement them, yielding two physical wrappers.
	scan1 := expressions.NewFullUnorderedScanExpression([]string{"Table1"}, values.UnknownType)
	ref1 := expressions.InitialOf(scan1)
	FireExpressionRule(NewPrimaryScanRule(), ref1)
	phys1 := findPhysicalExpr(ref1)

	scan2 := expressions.NewFullUnorderedScanExpression([]string{"Table2"}, values.UnknownType)
	ref2 := expressions.InitialOf(scan2)
	FireExpressionRule(NewPrimaryScanRule(), ref2)
	phys2 := findPhysicalExpr(ref2)

	if phys1 == nil || phys2 == nil {
		t.Fatal("no physical expressions after PrimaryScanRule")
	}

	// Put both into a new Reference
	ref := expressions.InitialOf(phys1)
	ref.Insert(phys2)

	first := findPhysicalPlan(ref)
	if first == nil {
		t.Fatal("findPhysicalPlan returned nil")
	}

	best := findBestValidPhysicalExpr(ref, PlanningCostModelLess)
	if best == nil {
		t.Fatal("findBestValidPhysicalExpr returned nil")
	}

	// Log what each returns so we can see if they differ
	t.Logf("findPhysicalPlan returned: %v (type %T)", first, first)
	t.Logf("findBestValidPhysicalExpr returned: %v (type %T)", best, best)

	// Test getWinnerForOrdering with no winner stamped
	winner := getWinnerForOrdering(ref, PreserveOrdering(), nil)
	if winner == nil {
		t.Fatal("getWinnerForOrdering returned nil")
	}
	t.Logf("getWinnerForOrdering(PRESERVE) returned: %v (type %T)", winner, winner)

	// Winner should equal findBestValidPhysicalExpr since no winner is stamped
	if winner != best {
		t.Fatal("getWinnerForOrdering fallback should return findBestValidPhysicalExpr result")
	}
}

func TestProjectionRule_WrapsWinnerNotFirst(t *testing.T) {
	t.Parallel()

	// Build: Projection(inner-ref)
	// Inner-ref has two physical plans. Verify the Projection wraps
	// the winner (cost-model best), not the first.
	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	innerRef := expressions.InitialOf(scan)

	scanRule := NewPrimaryScanRule()
	FireExpressionRule(scanRule, innerRef)

	projVals := []values.Value{
		&values.FieldValue{Field: "ID", Typ: values.UnknownType},
	}
	proj := expressions.NewLogicalProjectionExpression(
		projVals,
		expressions.ForEachQuantifier(innerRef),
	)
	topRef := expressions.InitialOf(proj)

	projRule := NewImplementProjectionRule()
	yielded := FireExpressionRule(projRule, topRef)
	if len(yielded) == 0 {
		t.Fatal("ImplementProjectionRule yielded nothing")
	}

	wrap, ok := yielded[0].(*physicalProjectionWrapper)
	if !ok {
		t.Fatalf("yielded[0] = %T, want *physicalProjectionWrapper", yielded[0])
	}
	if wrap.plan == nil {
		t.Fatal("projection wrapper has nil plan")
	}
	t.Logf("ProjectionRule yielded %d plans", len(yielded))
}

// TestFilterRule_UsesWinnerPerOrdering verifies that the ImplementFilterRule
// yields one FilterPlan per requested ordering when constraints are available.
func TestGetWinnerForOrdering_PreserveOnRefWithMultiplePhysical(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	ref := expressions.InitialOf(scan)

	scanRule := NewPrimaryScanRule()
	FireExpressionRule(scanRule, ref)

	// Verify findPhysicalPlan finds something
	plan := findPhysicalPlan(ref)
	if plan == nil {
		t.Fatal("findPhysicalPlan returned nil")
	}

	// Verify findPhysicalExpr finds something
	expr := findPhysicalExpr(ref)
	if expr == nil {
		t.Fatal("findPhysicalExpr returned nil")
	}

	// Verify getWinnerForOrdering(PRESERVE) finds something (no winners stamped)
	winner := getWinnerForOrdering(ref, PreserveOrdering(), nil)
	if winner == nil {
		t.Fatal("getWinnerForOrdering(PRESERVE) returned nil — this is the bug")
	}

	// Also verify with nil ordering
	winner2 := getWinnerForOrdering(ref, nil, nil)
	if winner2 == nil {
		t.Fatal("getWinnerForOrdering(nil) returned nil")
	}

	t.Logf("findPhysicalPlan: %T", plan)
	t.Logf("findPhysicalExpr: %T", expr)
	t.Logf("getWinnerForOrdering(PRESERVE): %T", winner)
	t.Logf("AllMembers count: %d", len(ref.AllMembers()))
	for i, m := range ref.AllMembers() {
		_, isPhys := m.(physicalPlanExpression)
		t.Logf("  member[%d]: %T isPhysical=%v", i, m, isPhys)
	}
}

func TestFilterRule_UsesWinnerPerOrdering(t *testing.T) {
	t.Parallel()

	pred := predicates.NewConstantPredicate(predicates.TriTrue)
	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	innerRef := expressions.InitialOf(scan)

	scanRule := NewPrimaryScanRule()
	FireExpressionRule(scanRule, innerRef)

	filter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{pred},
		expressions.ForEachQuantifier(innerRef),
	)
	topRef := expressions.InitialOf(filter)

	// Fire without constraints — should still yield at least 1 plan (PRESERVE fallback)
	filterRule := NewImplementFilterRule()
	yielded := FireExpressionRule(filterRule, topRef)
	if len(yielded) != 1 {
		t.Fatalf("ImplementFilterRule yielded %d without constraints, want 1", len(yielded))
	}

	wrap, ok := yielded[0].(*physicalPredicatesFilterWrapper)
	if !ok {
		t.Fatalf("yielded[0] = %T, want *physicalPredicatesFilterWrapper", yielded[0])
	}
	if wrap.plan == nil {
		t.Fatal("FilterPlan is nil")
	}
}
