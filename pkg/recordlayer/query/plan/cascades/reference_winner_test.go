package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func TestReference_Winner_NoWinner(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, nil)
	ref := expressions.InitialOf(scan)
	if ref.Winner() != nil {
		t.Fatal("expected nil winner")
	}
	if ref.HasWinner() {
		t.Fatal("expected no winner")
	}
}

func TestReference_Winner_SetAndGet(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, nil)
	ref := expressions.InitialOf(scan)
	ref.SetWinner(scan)
	if ref.Winner() != scan {
		t.Fatal("expected scan as winner")
	}
	if !ref.HasWinner() {
		t.Fatal("expected winner present")
	}
}

func TestReference_Winner_Overwrites(t *testing.T) {
	t.Parallel()
	scan1 := expressions.NewFullUnorderedScanExpression([]string{"A"}, nil)
	scan2 := expressions.NewFullUnorderedScanExpression([]string{"B"}, nil)
	ref := expressions.InitialOf(scan1)
	ref.Insert(scan2)

	ref.SetWinner(scan1)
	ref.SetWinner(scan2)
	if ref.Winner() != scan2 {
		t.Fatal("second SetWinner should overwrite first")
	}
}

func TestSortElimination_ViaChildOrderedMember(t *testing.T) {
	t.Parallel()

	// Set up: Sort(STATUS ASC) → Scan. Insert an ordered index scan as a
	// MEMBER of the scan Reference — no winner stamping. Extraction must
	// elide the sort by scanning the child's members' derived rich
	// orderings (Planner.OrderedChildWinner).
	a1 := values.UniqueCorrelationIdentifier()
	cand := NewValueIndexScanMatchCandidate(
		"Order$status",
		[]string{"Order"},
		[]string{"STATUS"},
		[]values.CorrelationIdentifier{a1},
		values.UnknownType,
		false,
		nil,
	)
	ctx := &indexTestPlanContext{candidates: []MatchCandidate{cand}}

	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	q := expressions.ForEachQuantifier(scanRef)
	sort := expressions.NewLogicalSortExpression(
		[]expressions.SortKey{
			{Value: &values.FieldValue{Field: "STATUS", Typ: values.UnknownType}},
		},
		q,
	)
	sortRef := expressions.InitialOf(sort)

	// Explore so the planner's Memo and exploration state are populated.
	rules := DefaultExpressionRules()
	p := NewPlanner(rules, ctx).
		WithPlanningExpressionRules(BatchAExpressionRules())
	exploreRewriting(p, sortRef)

	emptyPrefix := map[values.CorrelationIdentifier]*predicates.ComparisonRange{}
	scanPlan := cand.ToScanPlan(emptyPrefix, false)
	idxPlan := extractIndexPlan(scanPlan)
	if idxPlan == nil {
		t.Fatal("could not extract index plan from candidate")
	}
	orderedScan := &physicalIndexScanWrapper{
		plan:        idxPlan,
		columnNames: []string{"STATUS"},
		unique:      false,
	}
	scanRef.Insert(orderedScan)

	plan, err := properties.ExtractBestPlanFromSelector(sortRef, p, properties.DefaultStatistics{})
	if err != nil {
		t.Fatalf("ExtractBestPlanFromSelector: %v", err)
	}
	if plan == nil {
		t.Fatal("plan is nil")
	}
	// The extracted plan should be the index scan (sort eliminated),
	// not a LogicalSortExpression or InMemorySort.
	if !IsPhysicalIndexScan(plan) && !IsPhysicalFetchFromPartialRecord(plan) {
		t.Fatalf("sort should be eliminated via the child's ordered member; got %T", plan)
	}
}

// TestSortElimination_CounterflowNullsNotElidedAtExtraction pins the RFC-180
// D2 fix at the extraction seam: an `ORDER BY status ASC NULLS LAST` sort
// must NOT be elided against a child member that provides natural-order
// ASC (nulls first). The retired name+direction winner key dropped NULL
// placement, so the counterflow requirement map-hit the natural-order
// winner and elided the sort with the wrong null order.
func TestSortElimination_CounterflowNullsNotElidedAtExtraction(t *testing.T) {
	t.Parallel()

	a1 := values.UniqueCorrelationIdentifier()
	cand := NewValueIndexScanMatchCandidate(
		"Order$status",
		[]string{"Order"},
		[]string{"STATUS"},
		[]values.CorrelationIdentifier{a1},
		values.UnknownType,
		false,
		nil,
	)
	ctx := &indexTestPlanContext{candidates: []MatchCandidate{cand}}

	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	q := expressions.ForEachQuantifier(scanRef)
	nullsLast := false
	sort := expressions.NewLogicalSortExpression(
		[]expressions.SortKey{
			{Value: &values.FieldValue{Field: "STATUS", Typ: values.UnknownType}, NullsFirst: &nullsLast},
		},
		q,
	)
	sortRef := expressions.InitialOf(sort)

	rules := DefaultExpressionRules()
	p := NewPlanner(rules, ctx).
		WithPlanningExpressionRules(BatchAExpressionRules())
	exploreRewriting(p, sortRef)

	emptyPrefix := map[values.CorrelationIdentifier]*predicates.ComparisonRange{}
	scanPlan := cand.ToScanPlan(emptyPrefix, false)
	idxPlan := extractIndexPlan(scanPlan)
	if idxPlan == nil {
		t.Fatal("could not extract index plan from candidate")
	}
	orderedScan := &physicalIndexScanWrapper{
		plan:        idxPlan,
		columnNames: []string{"STATUS"},
		unique:      false,
	}
	scanRef.Insert(orderedScan)

	// The natural-ASC index scan does NOT satisfy ASC NULLS LAST: the
	// elision hook must decline.
	if w := p.OrderedChildWinner(sort, scanRef); w != nil {
		t.Fatalf("ASC NULLS LAST must not elide against a natural-ASC index scan; got %T", w)
	}
}

// TestSortElimination_PinsOrderedSpineThroughWrapper reproduces the
// transparent-wrapper relink hole: Sort over a group whose satisfying
// member is a FILTER WRAPPER (order-preserving delegator), where the
// wrapper's child group holds BOTH an ordered index scan and a cheaper
// unordered scan stamped as the group's overall winner. Eliding the sort
// and then rebuilding generically resolves the child group to its overall
// winner — the UNORDERED scan — producing unordered output with the sort
// already gone. The elision path must pin the spine: the rebuilt tree
// must contain the ordered index scan.
func TestSortElimination_PinsOrderedSpineThroughWrapper(t *testing.T) {
	t.Parallel()

	a1 := values.UniqueCorrelationIdentifier()
	cand := NewValueIndexScanMatchCandidate(
		"Order$status",
		[]string{"Order"},
		[]string{"STATUS"},
		[]values.CorrelationIdentifier{a1},
		values.UnknownType,
		false,
		nil,
	)
	emptyPrefix := map[values.CorrelationIdentifier]*predicates.ComparisonRange{}
	idxPlan := extractIndexPlan(cand.ToScanPlan(emptyPrefix, false))
	if idxPlan == nil {
		t.Fatal("could not extract index plan from candidate")
	}
	orderedScan := &physicalIndexScanWrapper{
		plan:        idxPlan,
		columnNames: []string{"STATUS"},
		unique:      false,
	}

	// The wrapper's child group: cheap unordered scan (stamped overall
	// winner) + the ordered index scan.
	scanExpr := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	innerRef := expressions.InitialOf(scanExpr)
	FireExpressionRule(NewPrimaryScanRule(), innerRef)
	cheap := findPhysicalExpr(innerRef)
	if cheap == nil {
		t.Fatal("no physical scan")
	}
	innerRef.InsertFinal(cheap)
	innerRef.Insert(orderedScan)
	innerRef.InsertFinal(orderedScan)
	innerRef.SetWinner(cheap) // generic extraction would relink to THIS

	// The order-preserving filter wrapper over that group.
	filterWrap := &physicalPredicatesFilterWrapper{
		plan: plans.NewRecordQueryPredicatesFilterPlan(
			plans.NewRecordQueryScanPlan([]string{"Order"}, values.UnknownType, false),
			[]predicates.QueryPredicate{predicates.NewConstantPredicate(predicates.TriTrue)},
		),
		innerQuant: expressions.ForEachQuantifier(innerRef),
	}
	filterRef := expressions.InitialOf(filterWrap)

	sort := expressions.NewLogicalSortExpression(
		[]expressions.SortKey{
			{Value: &values.FieldValue{Field: "STATUS", Typ: values.UnknownType}},
		},
		expressions.ForEachQuantifier(filterRef),
	)
	sortRef := expressions.InitialOf(sort)

	p := NewPlanner(DefaultExpressionRules(), nil)
	plan, err := properties.ExtractBestPlanFromSelector(sortRef, p, properties.DefaultStatistics{})
	if err != nil {
		t.Fatalf("ExtractBestPlanFromSelector: %v", err)
	}
	if plan == nil {
		t.Fatal("plan is nil")
	}
	// The sort must be elided (the filter's source group HAS a satisfying
	// member) — and the pinned spine must carry the ORDERED index scan,
	// not the group's cheap overall winner.
	if _, isSort := plan.(*expressions.LogicalSortExpression); isSort {
		t.Fatalf("sort should be elided through the order-preserving filter wrapper; got %T", plan)
	}
	if !subtreeContainsIndexScan(plan) {
		t.Fatalf("elided-sort spine was relinked to the unordered winner — the ordered index scan is gone (plan root %T)", plan)
	}
}

// subtreeContainsIndexScan walks an extracted (singleton-ref) tree for a
// physical index scan wrapper.
func subtreeContainsIndexScan(e expressions.RelationalExpression) bool {
	if e == nil {
		return false
	}
	if IsPhysicalIndexScan(e) {
		return true
	}
	for _, q := range e.GetQuantifiers() {
		ref := q.GetRangesOver()
		if ref == nil {
			continue
		}
		for _, m := range ref.AllMembers() {
			if subtreeContainsIndexScan(m) {
				return true
			}
		}
	}
	return false
}

// TestSortElimination_DeclinesWhenSpineUnpinnable pins the conservative
// arm: when the order-preserving wrapper's source group has NO satisfying
// member, elision must decline and the sort stays — an elided sort over
// an unpinnable spine is exactly the unordered-output hole.
func TestSortElimination_DeclinesWhenSpineUnpinnable(t *testing.T) {
	t.Parallel()

	scanExpr := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	innerRef := expressions.InitialOf(scanExpr)
	FireExpressionRule(NewPrimaryScanRule(), innerRef)
	cheap := findPhysicalExpr(innerRef)
	if cheap == nil {
		t.Fatal("no physical scan")
	}
	innerRef.InsertFinal(cheap)
	innerRef.SetWinner(cheap)

	filterWrap := &physicalPredicatesFilterWrapper{
		plan: plans.NewRecordQueryPredicatesFilterPlan(
			plans.NewRecordQueryScanPlan([]string{"Order"}, values.UnknownType, false),
			[]predicates.QueryPredicate{predicates.NewConstantPredicate(predicates.TriTrue)},
		),
		innerQuant: expressions.ForEachQuantifier(innerRef),
	}
	filterRef := expressions.InitialOf(filterWrap)

	sort := expressions.NewLogicalSortExpression(
		[]expressions.SortKey{
			{Value: &values.FieldValue{Field: "STATUS", Typ: values.UnknownType}},
		},
		expressions.ForEachQuantifier(filterRef),
	)
	p := NewPlanner(DefaultExpressionRules(), nil)
	if w := p.OrderedChildWinner(sort, filterRef); w != nil {
		t.Fatalf("a delegating wrapper over an orderless group must not satisfy; got %T", w)
	}
}

func TestSortElimination_ViaDataAccessOrderingWinner(t *testing.T) {
	t.Parallel()

	// Sort(STATUS ASC) → Filter(STATUS > 'a') → Scan with index on STATUS.
	// The filter creates PartialMatches via matching rules, data access
	// produces an ordered index scan, and ImplementSortRule eliminates
	// the sort when it finds the ordered scan in the filter Reference.
	a1 := values.UniqueCorrelationIdentifier()
	cand := NewValueIndexScanMatchCandidate(
		"Order$status",
		[]string{"Order"},
		[]string{"STATUS"},
		[]values.CorrelationIdentifier{a1},
		values.UnknownType,
		false,
		nil,
	)
	ctx := &indexTestPlanContext{candidates: []MatchCandidate{cand}}

	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	q := expressions.ForEachQuantifier(scanRef)
	filter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{
			predicates.NewComparisonPredicate(
				&values.FieldValue{Field: "STATUS", Typ: values.TypeString},
				predicates.NewLiteralComparison(predicates.ComparisonGreaterThan, "a"),
			),
		},
		q,
	)
	filterRef := expressions.InitialOf(filter)
	filterQ := expressions.ForEachQuantifier(filterRef)
	sort := expressions.NewLogicalSortExpression(
		[]expressions.SortKey{
			{Value: &values.FieldValue{Field: "STATUS", Typ: values.UnknownType}},
		},
		filterQ,
	)
	sortRef := expressions.InitialOf(sort)

	rules := DefaultExpressionRules()
	p := NewPlanner(rules, ctx).
		WithPlanningExpressionRules(BatchAExpressionRules()).
		WithImplementationRules(DefaultImplementationRules())
	plan, _, err := p.Plan(sortRef)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan == nil {
		t.Fatal("plan is nil")
	}

	// The plan should not be an in-memory sort — sort should be eliminated
	// via the ImplementSortRule + data access path.
	if !IsPhysicalIndexScan(plan) && !IsPhysicalFilter(plan) && !IsPhysicalFetchFromPartialRecord(plan) {
		t.Fatalf("sort should be eliminated via data access; got %T", plan)
	}
}

func TestPlan_OrderedMemberSelectable(t *testing.T) {
	t.Parallel()

	a1 := values.UniqueCorrelationIdentifier()
	cand := NewValueIndexScanMatchCandidate(
		"Order$status",
		[]string{"Order"},
		[]string{"STATUS"},
		[]values.CorrelationIdentifier{a1},
		values.UnknownType,
		false,
		nil,
	)
	ctx := &indexTestPlanContext{candidates: []MatchCandidate{cand}}

	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	q := expressions.ForEachQuantifier(scanRef)
	sort := expressions.NewLogicalSortExpression(
		[]expressions.SortKey{
			{Value: &values.FieldValue{Field: "STATUS", Typ: values.UnknownType}},
		},
		q,
	)
	sortRef := expressions.InitialOf(sort)

	rules := DefaultExpressionRules()
	p := NewPlanner(rules, ctx).
		WithPlanningExpressionRules(BatchAExpressionRules()).
		WithImplementationRules(DefaultImplementationRules())
	p.Plan(sortRef)

	// OrderedIndexScanRule produces an ordered index scan at the sort
	// level; bestSatisfyingMember must find it for a STATUS ASC request.
	reqOrd := NewRequestedOrdering(
		[]RequestedOrderingPart{{
			Value:     &values.FieldValue{Field: "STATUS", Typ: values.UnknownType},
			SortOrder: RequestedSortOrderAscending,
		}},
		DistinctnessPreserveDistinctness, false)
	winner := bestSatisfyingMember(sortRef, reqOrd, nil)
	if winner == nil {
		t.Fatal("expected an ordering-satisfying member for STATUS ASC")
	}
	if !IsPhysicalIndexScan(winner) && !IsPhysicalFetchFromPartialRecord(winner) {
		t.Fatalf("expected physicalIndexScanWrapper or physicalFetchFromPartialRecordWrapper, got %T", winner)
	}
}

// TestFinalOfChildrenVisibleToSemanticEquality pins the FinalOf identity
// fix: a finals-only pinned singleton must be visible through
// Reference.Get — otherwise two otherwise-identical wrappers over
// DIFFERENT pinned children both expose nil children, SemanticEquals
// collapses them, and InsertFinal deduplicates a distinct ordered
// alternative away.
func TestFinalOfChildrenVisibleToSemanticEquality(t *testing.T) {
	t.Parallel()

	childA := sortedMemberOn(t, "A")
	childB := sortedMemberOn(t, "B")

	mk := func(child expressions.RelationalExpression) expressions.RelationalExpression {
		return &physicalPredicatesFilterWrapper{
			plan: plans.NewRecordQueryPredicatesFilterPlan(
				plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false),
				[]predicates.QueryPredicate{predicates.NewConstantPredicate(predicates.TriTrue)},
			),
			innerQuant: expressions.ForEachQuantifier(expressions.FinalOf(child)),
		}
	}
	w1, w2 := mk(childA), mk(childB)

	ref := expressions.InitialOf(expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType))
	ref.InsertFinal(w1)
	ref.InsertFinal(w2)
	if got := len(ref.FinalMembers()); got != 2 {
		t.Fatalf("wrappers over DIFFERENT pinned children must not deduplicate: finals = %d, want 2", got)
	}
	if expressions.FinalOf(childA).Get() != childA {
		t.Fatal("Get() must expose the pinned final of a finals-only Reference")
	}
}
