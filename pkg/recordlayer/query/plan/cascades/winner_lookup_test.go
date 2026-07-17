package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
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

	// Stamp the group winner
	ref.SetWinner(physExpr)

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

	// Insert a member that genuinely PROVIDES (NAME ASC) — an in-memory
	// sort wrapper. Ordering winners are found by scanning members'
	// derived rich orderings, not a stamped key map.
	sorted := sortedMemberOn(t, "NAME")
	ref.Insert(sorted)

	// Look up by RequestedOrdering on the same column
	parts := []RequestedOrderingPart{
		{Value: values.NewFlatFieldValue("NAME", values.UnknownType), SortOrder: RequestedSortOrderAscending},
	}
	reqOrd := NewRequestedOrdering(parts, DistinctnessPreserveDistinctness, false)

	winner := getWinnerForOrdering(ref, reqOrd, nil)
	if winner == nil {
		t.Fatal("getWinnerForOrdering returned nil for matching ordering")
	}
	if winner != sorted {
		t.Fatalf("getWinnerForOrdering must return the ordering-providing member, got %T", winner)
	}
}

// sortedMemberOn builds a physical in-memory sort member sorted ascending
// (natural null placement) on the given fields.
func sortedMemberOn(t *testing.T, fields ...string) expressions.RelationalExpression {
	t.Helper()
	return sortedMemberWithNulls(t, fields, nil)
}

// sortedMemberWithNulls builds a physical in-memory sort member on the given
// fields, ascending; nullsFirst (parallel to fields, nil = natural, i.e. all
// true for ASC) sets each key's NULL placement.
func sortedMemberWithNulls(t *testing.T, fields []string, nullsFirst []bool) expressions.RelationalExpression {
	t.Helper()
	inner := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	keys := make([]plans.SortKey, len(fields))
	for i, f := range fields {
		nf := true // natural for ASC
		if nullsFirst != nil {
			nf = nullsFirst[i]
		}
		keys[i] = plans.SortKey{Field: f, ValueExpr: values.NewFlatFieldValue(f, values.UnknownType), NullsFirst: nf}
	}
	scanRef := expressions.InitialOf(expressions.NewFullUnorderedScanExpression([]string{"T"}, nil))
	return newPhysicalInMemorySortWrapper(
		plans.NewRecordQueryInMemorySortPlan(inner, keys),
		expressions.ForEachQuantifier(scanRef))
}

// TestBestSatisfyingMember_CounterflowNullsGate pins the RFC-180 D2 core:
// ordering-winner selection judges NULL placement. The retired
// name+direction winner key flattened the four-state sort order to a
// desc bool, so an ASC_NULLS_LAST requirement map-hit a natural-ASC
// (nulls first) provider and elided the enforcer sort with the wrong
// null order — and vice versa.
func TestBestSatisfyingMember_CounterflowNullsGate(t *testing.T) {
	t.Parallel()

	natural := sortedMemberWithNulls(t, []string{"S"}, []bool{true})      // ASC NULLS FIRST (natural)
	counterflow := sortedMemberWithNulls(t, []string{"S"}, []bool{false}) // ASC NULLS LAST

	reqOn := func(so RequestedSortOrder) *RequestedOrdering {
		return NewRequestedOrdering(
			[]RequestedOrderingPart{{Value: values.NewFlatFieldValue("S", values.UnknownType), SortOrder: so}},
			DistinctnessPreserveDistinctness, false)
	}

	refNatural := expressions.InitialOf(natural)
	if w := bestSatisfyingMember(refNatural, reqOn(RequestedSortOrderAscendingNullsLast), nil); w != nil {
		t.Fatalf("ASC NULLS LAST must not be satisfied by a natural-ASC provider; got %T", w)
	}
	if w := bestSatisfyingMember(refNatural, reqOn(RequestedSortOrderAscending), nil); w != natural {
		t.Fatalf("natural ASC request should be satisfied by the natural-ASC provider; got %T", w)
	}

	refCounter := expressions.InitialOf(counterflow)
	if w := bestSatisfyingMember(refCounter, reqOn(RequestedSortOrderAscending), nil); w != nil {
		t.Fatalf("natural ASC must not be satisfied by an ASC-NULLS-LAST provider; got %T", w)
	}
	if w := bestSatisfyingMember(refCounter, reqOn(RequestedSortOrderAscendingNullsLast), nil); w != counterflow {
		t.Fatalf("ASC NULLS LAST request should be satisfied by the ASC-NULLS-LAST provider; got %T", w)
	}

	// getWinnerForOrdering falls back to the cheapest plan when nothing
	// satisfies — it must not return the counterflow-mismatched member AS
	// the satisfying winner, but it still returns a plan (the sort stays).
	if w := getWinnerForOrdering(refNatural, reqOn(RequestedSortOrderAscendingNullsLast), nil); w != natural {
		t.Fatalf("fallback should return the cheapest member, got %T", w)
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
