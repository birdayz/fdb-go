package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
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
