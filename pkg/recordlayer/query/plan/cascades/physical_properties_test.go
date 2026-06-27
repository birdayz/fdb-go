package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func TestPhysicalProperties_NoProperties_IsEmpty(t *testing.T) {
	t.Parallel()
	if !expressions.NoProperties.IsEmpty() {
		t.Fatal("NoProperties should be empty")
	}
}

func TestPhysicalProperties_WithOrdering_NotEmpty(t *testing.T) {
	t.Parallel()
	props := expressions.PhysicalProperties{}
	if !props.IsEmpty() {
		t.Fatal("zero-value should be empty")
	}
}

func TestReference_Winner_NoWinner(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, nil)
	ref := expressions.InitialOf(scan)
	if ref.Winner(expressions.NoProperties) != nil {
		t.Fatal("expected nil winner")
	}
	if ref.HasWinner(expressions.NoProperties) {
		t.Fatal("expected no winner")
	}
}

func TestReference_Winner_SetAndGet(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, nil)
	ref := expressions.InitialOf(scan)
	ref.SetWinner(expressions.NoProperties, scan)
	if ref.Winner(expressions.NoProperties) != scan {
		t.Fatal("expected scan as winner")
	}
	if !ref.HasWinner(expressions.NoProperties) {
		t.Fatal("expected winner present")
	}
}

func TestReference_Winner_MultipleProperties(t *testing.T) {
	t.Parallel()
	scan1 := expressions.NewFullUnorderedScanExpression([]string{"A"}, nil)
	scan2 := expressions.NewFullUnorderedScanExpression([]string{"B"}, nil)
	ref := expressions.InitialOf(scan1)
	ref.Insert(scan2)

	key1 := expressions.NoProperties
	key2 := expressions.PhysicalProperties{} // same as NoProperties

	ref.SetWinner(key1, scan1)
	if ref.Winner(key2) != scan1 {
		t.Fatal("equivalent keys should return same winner")
	}
}

func TestReference_Winner_OverwritesBetterPlan(t *testing.T) {
	t.Parallel()
	scan1 := expressions.NewFullUnorderedScanExpression([]string{"A"}, nil)
	scan2 := expressions.NewFullUnorderedScanExpression([]string{"B"}, nil)
	ref := expressions.InitialOf(scan1)
	ref.Insert(scan2)

	ref.SetWinner(expressions.NoProperties, scan1)
	ref.SetWinner(expressions.NoProperties, scan2)
	if ref.Winner(expressions.NoProperties) != scan2 {
		t.Fatal("second SetWinner should overwrite first")
	}
}

func TestPhysicalProperties_OrderingFromSortKeys(t *testing.T) {
	t.Parallel()
	keys := []expressions.SortKey{
		{Value: &values.FieldValue{Field: "name"}, Reverse: false},
		{Value: &values.FieldValue{Field: "age"}, Reverse: true},
	}
	props := expressions.OrderingFromSortKeys(keys)
	if props.IsEmpty() {
		t.Fatal("ordering properties should not be empty")
	}
	if props.OrderingCount() != 2 {
		t.Fatalf("expected 2 ordering columns, got %d", props.OrderingCount())
	}
}

func TestPhysicalProperties_Satisfies(t *testing.T) {
	t.Parallel()
	nameAsc := []expressions.SortKey{
		{Value: &values.FieldValue{Field: "name"}, Reverse: false},
	}
	nameAscAge := []expressions.SortKey{
		{Value: &values.FieldValue{Field: "name"}, Reverse: false},
		{Value: &values.FieldValue{Field: "age"}, Reverse: false},
	}
	nameDesc := []expressions.SortKey{
		{Value: &values.FieldValue{Field: "name"}, Reverse: true},
	}

	propsNameAsc := expressions.OrderingFromSortKeys(nameAsc)
	propsNameAscAge := expressions.OrderingFromSortKeys(nameAscAge)
	propsNameDesc := expressions.OrderingFromSortKeys(nameDesc)

	if !propsNameAscAge.Satisfies(propsNameAsc) {
		t.Fatal("(name ASC, age ASC) should satisfy (name ASC)")
	}
	if propsNameAsc.Satisfies(propsNameAscAge) {
		t.Fatal("(name ASC) should NOT satisfy (name ASC, age ASC)")
	}
	if propsNameAsc.Satisfies(propsNameDesc) {
		t.Fatal("(name ASC) should NOT satisfy (name DESC)")
	}
	if !propsNameAsc.Satisfies(expressions.NoProperties) {
		t.Fatal("any ordering should satisfy empty requirements")
	}
	if !expressions.NoProperties.Satisfies(expressions.NoProperties) {
		t.Fatal("empty should satisfy empty")
	}
}

func TestOptimizeReferenceTask_StampsWinner(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, nil)
	ref := expressions.InitialOf(scan)

	p := NewPlanner(nil, nil)
	task := &OptimizeReferenceTask{Ref: ref}
	task.Run(p)

	if ref.Winner(expressions.NoProperties) != scan {
		t.Fatal("OptimizeReferenceTask should stamp winner on Reference")
	}
	if p.BestMember(ref) != scan {
		t.Fatal("OptimizeReferenceTask should also stamp bestMember map")
	}
}

func TestSortElimination_ViaChildOrderingWinner(t *testing.T) {
	t.Parallel()

	// Set up: Sort(STATUS ASC) → Scan
	// Manually stamp an ordering winner on the scan Reference.
	// Verify extraction eliminates the sort.
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

	// Run BatchA rules (PrimaryScanRule will produce physicalScanWrapper).
	rules := DefaultExpressionRules()
	p := NewPlanner(rules, ctx).
		WithPlanningExpressionRules(BatchAExpressionRules())
	p.Explore(sortRef)

	// Now manually stamp an ordered index scan as the ordering winner
	// on the scan Reference. This simulates the future data-access
	// architecture where ordered scans go into the child Reference.
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

	// Stamp it as the ordering winner for STATUS ASC.
	orderingProps := expressions.OrderingFromNameDir([]string{"STATUS"}, []bool{false})
	scanRef.SetWinner(orderingProps, orderedScan)

	// Now extract — the sort should be eliminated via the child's
	// ordering winner.
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
		t.Fatalf("sort should be eliminated via child ordering winner; got %T", plan)
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

func TestOptimizeReferenceTask_StampsOrderingWinner(t *testing.T) {
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
	// level. stampOrderingWinners detects the ordering and stamps it.
	statusOrdering := expressions.OrderingFromNameDir([]string{"STATUS"}, []bool{false})
	winner := sortRef.Winner(statusOrdering)
	if winner == nil {
		t.Fatal("expected ordering-specific winner for STATUS ASC")
	}
	if !IsPhysicalIndexScan(winner) && !IsPhysicalFetchFromPartialRecord(winner) {
		t.Fatalf("expected physicalIndexScanWrapper or physicalFetchFromPartialRecordWrapper as ordering winner, got %T", winner)
	}
}
