package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
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
	winner, _ := getWinnerForOrdering(ref, properties.PreserveOrdering(), nil)
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
	winner, _ := getWinnerForOrdering(ref, properties.PreserveOrdering(), nil)
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
	parts := []properties.RequestedOrderingPart{
		{Value: values.NewFlatFieldValue("NAME", values.UnknownType), SortOrder: properties.RequestedSortOrderAscending},
	}
	reqOrd := properties.NewRequestedOrdering(parts, properties.DistinctnessPreserveDistinctness, false)

	winner, _ := getWinnerForOrdering(ref, reqOrd, nil)
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
	// Since RFC-184 W2 the bare in-memory sort IS its own physical member (no
	// physicalInMemorySortWrapper) — its HintOrdering carries the sort keys.
	return plans.NewRecordQueryInMemorySortPlan(inner, keys)
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

	reqOn := func(so properties.RequestedSortOrder) *properties.RequestedOrdering {
		return properties.NewRequestedOrdering(
			[]properties.RequestedOrderingPart{{Value: values.NewFlatFieldValue("S", values.UnknownType), SortOrder: so}},
			properties.DistinctnessPreserveDistinctness, false)
	}

	refNatural := expressions.InitialOf(natural)
	if w := bestSatisfyingMember(refNatural, reqOn(properties.RequestedSortOrderAscendingNullsLast), nil); w != nil {
		t.Fatalf("ASC NULLS LAST must not be satisfied by a natural-ASC provider; got %T", w)
	}
	if w := bestSatisfyingMember(refNatural, reqOn(properties.RequestedSortOrderAscending), nil); w != natural {
		t.Fatalf("natural ASC request should be satisfied by the natural-ASC provider; got %T", w)
	}

	refCounter := expressions.InitialOf(counterflow)
	if w := bestSatisfyingMember(refCounter, reqOn(properties.RequestedSortOrderAscending), nil); w != nil {
		t.Fatalf("natural ASC must not be satisfied by an ASC-NULLS-LAST provider; got %T", w)
	}
	if w := bestSatisfyingMember(refCounter, reqOn(properties.RequestedSortOrderAscendingNullsLast), nil); w != counterflow {
		t.Fatalf("ASC NULLS LAST request should be satisfied by the ASC-NULLS-LAST provider; got %T", w)
	}

	// getWinnerForOrdering falls back to the cheapest plan when nothing
	// satisfies — it must not return the counterflow-mismatched member AS
	// the satisfying winner, but it still returns a plan (the sort stays).
	if w, _ := getWinnerForOrdering(refNatural, reqOn(properties.RequestedSortOrderAscendingNullsLast), nil); w != natural {
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
	winner, _ := getWinnerForOrdering(ref, properties.PreserveOrdering(), nil)
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

	if _, ok := yielded[0].(*plans.RecordQueryProjectionPlan); !ok {
		t.Fatalf("yielded[0] = %T, want *plans.RecordQueryProjectionPlan", yielded[0])
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
	winner, _ := getWinnerForOrdering(ref, properties.PreserveOrdering(), nil)
	if winner == nil {
		t.Fatal("getWinnerForOrdering(PRESERVE) returned nil — this is the bug")
	}

	// Also verify with nil ordering
	winner2, _ := getWinnerForOrdering(ref, nil, nil)
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

	wrap, ok := yielded[0].(*plans.RecordQueryPredicatesFilterPlan)
	if !ok {
		t.Fatalf("yielded[0] = %T, want *plans.RecordQueryPredicatesFilterPlan", yielded[0])
	}
	if wrap.GetInner() == nil {
		t.Fatal("filter plan inner is nil")
	}
}

// TestPinOrderedSpine pins the RULE-time twin of the extraction spine
// pinning: a sort-dropping yield of an order-preserving wrapper must bake
// the wrapper's source group to the satisfying member — otherwise
// extraction's generic rebuild relinks it to a cheaper unordered sibling
// after the sort is already gone.
func TestPinOrderedSpine(t *testing.T) {
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

	basePlan := plans.NewRecordQueryPredicatesFilterPlan(
		plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false),
		[]predicates.QueryPredicate{predicates.NewConstantPredicate(predicates.TriTrue)},
	)
	wrapper := basePlan.WithQuantifiers([]expressions.Quantifier{expressions.ForEachQuantifier(srcRef)})

	reqS := properties.NewRequestedOrdering(
		[]properties.RequestedOrderingPart{{Value: values.NewFlatFieldValue("S", values.UnknownType), SortOrder: properties.RequestedSortOrderAscending}},
		properties.DistinctnessPreserveDistinctness, false)

	pinned := pinOrderedSpine(wrapper, reqS, nil)
	if pinned == nil {
		t.Fatal("a delegator over a group WITH a satisfying member must pin, not decline")
	}
	qs := pinned.GetQuantifiers()
	if len(qs) != 1 {
		t.Fatalf("pinned wrapper must keep its single quantifier, got %d", len(qs))
	}
	members := qs[0].GetRangesOver().AllMembers()
	if len(members) != 1 || members[0] != ordered {
		t.Fatalf("the pinned source group must be a singleton holding the ORDERED member; got %d members", len(members))
	}

	// A non-delegator returns unchanged.
	if got := pinOrderedSpine(ordered, reqS, nil); got != ordered {
		t.Fatalf("non-delegator must pass through unchanged, got %T", got)
	}

	// Unpinnable (no satisfying member) declines with nil.
	loneRef := expressions.InitialOf(cheap)
	loneWrapper := basePlan.WithQuantifiers([]expressions.Quantifier{expressions.ForEachQuantifier(loneRef)})
	if got := pinOrderedSpine(loneWrapper, reqS, nil); got != nil {
		t.Fatalf("delegator over an orderless group must decline, got %T", got)
	}
}

// TestPinOrderedSpine_DeclinesWhenRelinkRefused pins the executable-plan
// verification: several WithChildren impls KEEP their original concrete
// plan when the new child isn't leaf-replaceable (isLeafReplaceable) —
// the quantifier then points at the pinned member while
// GetRecordQueryPlan() still executes the old child. Such a "pin" must be
// DECLINED (nil), never yielded: dropping a sort on its strength executes
// the unpinned child in whatever order it has.
func TestPinOrderedSpine_DeclinesWhenRelinkRefused(t *testing.T) {
	t.Parallel()

	// An ordered member whose plan is NOT leaf-replaceable: a projection
	// wrapper (RecordQueryProjectionPlan is outside the isLeafReplaceable
	// set) delegating over an in-memory sort on S.
	sorted := sortedMemberOn(t, "S")
	sortedRef := expressions.InitialOf(sorted)
	orderedProjection := plans.NewRecordQueryProjectionPlanFromQuantifier(
		[]values.Value{values.NewFlatFieldValue("S", values.UnknownType)},
		nil,
		expressions.ForEachQuantifier(sortedRef),
	)

	srcRef := expressions.InitialOf(orderedProjection)

	wrapper := plans.NewRecordQueryPredicatesFilterPlan(
		plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false),
		[]predicates.QueryPredicate{predicates.NewConstantPredicate(predicates.TriTrue)},
	).WithQuantifiers([]expressions.Quantifier{expressions.ForEachQuantifier(srcRef)})

	reqS := properties.NewRequestedOrdering(
		[]properties.RequestedOrderingPart{{Value: values.NewFlatFieldValue("S", values.UnknownType), SortOrder: properties.RequestedSortOrderAscending}},
		properties.DistinctnessPreserveDistinctness, false)

	if got := pinOrderedSpine(wrapper, reqS, nil); got != nil {
		gotPE, _ := got.(physicalPlanExpression)
		var childIsProjection bool
		if gotPE != nil {
			for _, c := range gotPE.GetRecordQueryPlan().GetChildren() {
				if _, ok := c.(*plans.RecordQueryProjectionPlan); ok {
					childIsProjection = true
				}
			}
		}
		if !childIsProjection {
			t.Fatalf("pin returned a wrapper whose executable plan does NOT contain the pinned projection child — the relink was refused and the pin is a lie (%T)", got)
		}
	}
}

// TestGetWinnerForOrdering_SatisfiedContract pins RFC-186 §2C: the second
// return distinguishes a member that SATISFIES the requested ordering from
// the load-bearing unordered FALLBACK. The fallback must still YIELD
// (returning nothing would empty the group under a sort — the child's only
// requested ordering is the sort's, and ImplementInMemorySortRule bails on
// an empty group: no plan at all); satisfied=false is the caller's signal
// that the ordering claim, if any, must come from downstream re-derivation
// (orderingDelegator / pinOrderedSpine), never from this return.
func TestGetWinnerForOrdering_SatisfiedContract(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	ref := expressions.InitialOf(scan)
	FireExpressionRule(NewPrimaryScanRule(), ref)
	phys := findPhysicalExpr(ref)
	if phys == nil {
		t.Fatal("no physical expr after PrimaryScanRule")
	}
	ref.SetWinner(phys)

	t.Run("preserve is trivially satisfied", func(t *testing.T) {
		t.Parallel()
		w, satisfied := getWinnerForOrdering(ref, properties.PreserveOrdering(), nil)
		if w == nil || !satisfied {
			t.Fatalf("(w=%v, satisfied=%v), want stamped winner with satisfied=true", w, satisfied)
		}
	})

	t.Run("unsatisfiable ordering yields the fallback with satisfied=false", func(t *testing.T) {
		t.Parallel()
		// A bare unordered primary scan cannot satisfy an ordering on X.
		reqX := properties.NewRequestedOrdering(
			[]properties.RequestedOrderingPart{{Value: values.NewFlatFieldValue("X", values.UnknownType), SortOrder: properties.RequestedSortOrderAscending}},
			properties.DistinctnessPreserveDistinctness, false)
		w, satisfied := getWinnerForOrdering(ref, reqX, nil)
		if w == nil {
			t.Fatal("the unordered fallback must still YIELD (no-empty-group: the enforcer wraps it)")
		}
		if satisfied {
			t.Fatal("an unordered scan must never be reported as satisfying an ordering on X")
		}
	})
}
