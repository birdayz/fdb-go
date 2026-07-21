package cascades

import (
	"fmt"
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func TestE2E_ScanOnlyPlan(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	rootRef := expressions.InitialOf(scan)

	p := NewPlanner(allRules(), nil).WithPlanningExpressionRules(BatchAExpressionRules()).WithImplementationRules(DefaultImplementationRules())
	plan, _, err := p.Plan(rootRef)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan == nil {
		t.Fatal("Plan returned nil")
	}
	// RFC-184 W2: a bare primary scan is its own physical expression.
	if _, ok := plan.(*plans.RecordQueryScanPlan); !ok {
		t.Fatalf("expected *plans.RecordQueryScanPlan, got %T", plan)
	}
}

func TestE2E_FilterOverScan(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	pred := predicates.NewComparisonPredicate(
		&values.FieldValue{Field: "x", Typ: values.UnknownType},
		predicates.NewLiteralComparison(predicates.ComparisonEquals, 42),
	)
	filter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{pred},
		expressions.ForEachQuantifier(scanRef),
	)
	rootRef := expressions.InitialOf(filter)

	p := NewPlanner(allRules(), nil).WithPlanningExpressionRules(BatchAExpressionRules()).WithImplementationRules(DefaultImplementationRules())
	plan, _, err := p.Plan(rootRef)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan == nil {
		t.Fatal("Plan returned nil")
	}
	if _, ok := plan.(*plans.RecordQueryPredicatesFilterPlan); !ok {
		t.Fatalf("expected *plans.RecordQueryPredicatesFilterPlan, got %T", plan)
	}
}

func TestE2E_SortOverFilterOverScan(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	pred := predicates.NewComparisonPredicate(
		&values.FieldValue{Field: "x", Typ: values.UnknownType},
		predicates.NewLiteralComparison(predicates.ComparisonGreaterThan, 10),
	)
	filter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{pred},
		expressions.ForEachQuantifier(scanRef),
	)
	filterRef := expressions.InitialOf(filter)
	sort := expressions.NewLogicalSortExpression(
		[]expressions.SortKey{{
			Value:   &values.FieldValue{Field: "x", Typ: values.UnknownType},
			Reverse: false,
		}},
		expressions.ForEachQuantifier(filterRef),
	)
	rootRef := expressions.InitialOf(sort)

	p := NewPlanner(allRules(), nil).WithPlanningExpressionRules(BatchAExpressionRules()).WithImplementationRules(DefaultImplementationRules())
	plan, _, err := p.Plan(rootRef)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan == nil {
		t.Fatal("Plan returned nil")
	}
}

func TestE2E_DistinctOverScan(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	distinct := expressions.NewLogicalDistinctExpression(
		expressions.ForEachQuantifier(scanRef),
	)
	rootRef := expressions.InitialOf(distinct)

	p := NewPlanner(allRules(), nil).WithPlanningExpressionRules(BatchAExpressionRules()).WithImplementationRules(DefaultImplementationRules())
	plan, _, err := p.Plan(rootRef)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan == nil {
		t.Fatal("Plan returned nil")
	}
}

func TestE2E_LimitOverScan(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	limit := expressions.NewLogicalLimitExpression(5, 0,
		expressions.ForEachQuantifier(scanRef),
	)
	rootRef := expressions.InitialOf(limit)

	p := NewPlanner(allRules(), nil).WithPlanningExpressionRules(BatchAExpressionRules()).WithImplementationRules(DefaultImplementationRules())
	plan, _, err := p.Plan(rootRef)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan == nil {
		t.Fatal("Plan returned nil")
	}
}

func TestE2E_UnionOfTwoScans(t *testing.T) {
	t.Parallel()
	scanA := expressions.NewFullUnorderedScanExpression([]string{"A"}, values.UnknownType)
	scanB := expressions.NewFullUnorderedScanExpression([]string{"B"}, values.UnknownType)
	union := expressions.NewLogicalUnionExpression([]expressions.Quantifier{
		expressions.ForEachQuantifier(expressions.InitialOf(scanA)),
		expressions.ForEachQuantifier(expressions.InitialOf(scanB)),
	})
	rootRef := expressions.InitialOf(union)

	p := NewPlanner(allRules(), nil).WithPlanningExpressionRules(BatchAExpressionRules()).WithImplementationRules(DefaultImplementationRules())
	plan, _, err := p.Plan(rootRef)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan == nil {
		t.Fatal("Plan returned nil")
	}
}

// TestE2E_SortEliminationThroughFilter verifies that the planner
// eliminates a redundant Sort when the underlying scan provides the
// requested ordering and a filter sits between them.
//
// Input tree:   Sort(ID ASC) -> Filter(ID > 5) -> FullUnorderedScan(TABLE)
//
// Rule chain:
//  1. PrimaryScanRule: yields a bare RecordQueryScanPlan with PK ordering (ID).
//  2. PushRequestedOrderingThroughFilterRule: pushes ordering constraint through Filter.
//  3. ImplementSortRule: eliminates Sort because the scan provides ID ordering.
//
// Expected result: Filter -> Scan (no sort operator anywhere in the plan).
func TestE2E_SortEliminationThroughFilter(t *testing.T) {
	t.Parallel()

	// Build: Sort(ID ASC) -> Filter(ID > 5) -> FullUnorderedScan(TABLE)
	scan := expressions.NewFullUnorderedScanExpression([]string{"TABLE"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)

	pred := predicates.NewComparisonPredicate(
		&values.FieldValue{Field: "ID", Typ: values.UnknownType},
		predicates.NewLiteralComparison(predicates.ComparisonGreaterThan, 5),
	)
	filter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{pred},
		expressions.ForEachQuantifier(scanRef),
	)
	filterRef := expressions.InitialOf(filter)

	sort := expressions.NewLogicalSortExpression(
		[]expressions.SortKey{{
			Value:   &values.FieldValue{Field: "ID", Typ: values.UnknownType},
			Reverse: false,
		}},
		expressions.ForEachQuantifier(filterRef),
	)
	rootRef := expressions.InitialOf(sort)

	// PlanContext that declares ID as the primary key of TABLE.
	ctx := &e2ePKPlanContext{
		pkColumns: map[string][]string{
			"TABLE": {"ID"},
		},
	}

	rules := DefaultExpressionRules()
	p := NewPlanner(rules, ctx).
		WithPlanningExpressionRules(BatchAExpressionRules()).
		WithImplementationRules(DefaultImplementationRules())

	best, _, err := p.Plan(rootRef)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if best == nil {
		t.Fatal("Plan returned nil")
	}

	// The final plan must not contain any sort operator.
	if containsSort(best) {
		t.Fatalf("expected sort to be eliminated, but plan contains a sort operator: %s",
			describePlan(best))
	}

	fw, ok := best.(*plans.RecordQueryPredicatesFilterPlan)
	if !ok {
		t.Fatalf("expected *plans.RecordQueryPredicatesFilterPlan at root, got %T (%s)", best, describePlan(best))
	}
	innerRef := fw.GetQuantifiers()[0].GetRangesOver()
	innerPlan := findPhysicalPlan(innerRef)
	if innerPlan == nil {
		t.Fatal("expected physical scan inside filter, got nil")
	}
	if _, ok := innerPlan.(*plans.RecordQueryScanPlan); !ok {
		t.Fatalf("expected RecordQueryScanPlan inside filter, got %T", innerPlan)
	}
}

// e2ePKPlanContext is a minimal PlanContext that provides primary key
// column information for sort-elimination tests.
type e2ePKPlanContext struct {
	pkColumns map[string][]string
}

func (c *e2ePKPlanContext) GetPlannerConfiguration() PlannerConfiguration {
	return DefaultPlannerConfiguration()
}

func (c *e2ePKPlanContext) GetMatchCandidates() []MatchCandidate { return nil }

func (c *e2ePKPlanContext) GetPrimaryKeyColumns(recordType string) []string {
	return c.pkColumns[recordType]
}

// containsSort recursively walks the physical plan tree and returns
// true if any node is a sort operator (in-memory sort wrapper or
// logical sort expression).
func containsSort(expr expressions.RelationalExpression) bool {
	switch expr.(type) {
	case *plans.RecordQueryInMemorySortPlan:
		return true
	case *expressions.LogicalSortExpression:
		return true
	}
	for _, q := range expr.GetQuantifiers() {
		ref := q.GetRangesOver()
		if ref == nil {
			continue
		}
		for _, m := range ref.AllMembers() {
			if ph, ok := m.(physicalPlanExpression); ok {
				if containsSort(ph) {
					return true
				}
			}
		}
	}
	return false
}

// TestE2E_JoinCommutativityExploration verifies that the planner's join
// commutativity machinery explores both join directions for a
// SelectExpression with ChildrenAsSet=true and 2 ForEach quantifiers
// (an INNER join). Both directions FORM (the swapped-quantifier fire),
// but physical yields land ONLY in FinalMembers and OptimizeGroup
// prunes finals to the winner (Java's prune-to-1) — so only ONE NLJ
// direction is visible in AllMembers() after Plan(). The test therefore
// pins the two properties separately: BOTH directions at yield time via
// a direct rule fire (FireExpressionRule performs the same
// ChildrenAsSet swapped-quantifier permutation as the planner), and a
// single valid-direction NLJ winner after the full planner run.
func TestE2E_JoinCommutativityExploration(t *testing.T) {
	t.Parallel()

	buildSelect := func() *expressions.Reference {
		scanA := expressions.NewFullUnorderedScanExpression([]string{"A"}, values.UnknownType)
		scanARef := expressions.InitialOf(scanA)
		scanAQ := expressions.ForEachQuantifier(scanARef)

		scanB := expressions.NewFullUnorderedScanExpression([]string{"B"}, values.UnknownType)
		scanBRef := expressions.InitialOf(scanB)
		scanBQ := expressions.ForEachQuantifier(scanBRef)

		joinPred := predicates.NewComparisonPredicate(
			&values.FieldValue{Field: "a_id", Typ: values.UnknownType},
			predicates.NewLiteralComparison(predicates.ComparisonEquals, "b_id"),
		)

		sel := expressions.NewSelectExpressionWithAliases(
			values.NewQuantifiedObjectValue(values.UniqueCorrelationIdentifier()),
			[]expressions.Quantifier{scanAQ, scanBQ},
			[]predicates.QueryPredicate{joinPred},
			[]string{"A", "B"},
		)
		return expressions.InitialOf(sel)
	}

	// Contains (not equality): a winning leg may carry a pushed-down
	// filter, e.g. PredicatesFilter(Scan(B), ...) — the DIRECTION is
	// which table each leg scans, not the leg's exact wrapper chain.
	directionsOf := func(nljPlans []*plans.RecordQueryNestedLoopJoinPlan) (foundAB, foundBA bool) {
		for _, nlj := range nljPlans {
			outerExplain := nlj.GetOuter().Explain()
			innerExplain := nlj.GetInner().Explain()
			if strings.Contains(outerExplain, "Scan(A)") && strings.Contains(innerExplain, "Scan(B)") {
				foundAB = true
			}
			if strings.Contains(outerExplain, "Scan(B)") && strings.Contains(innerExplain, "Scan(A)") {
				foundBA = true
			}
		}
		return
	}

	// FORMATION: fire the NLJ rule directly on a select whose children
	// hold physical scans — the primary and swapped binds must yield
	// BOTH join directions.
	fireRef := buildSelect()
	for _, q := range fireRef.Get().GetQuantifiers() {
		FireExpressionRule(NewPrimaryScanRule(), q.GetRangesOver())
	}
	var yieldedNLJs []*plans.RecordQueryNestedLoopJoinPlan
	for _, y := range FireExpressionRule(NewImplementNestedLoopJoinRule(), fireRef) {
		if nlj, ok := y.(*plans.RecordQueryNestedLoopJoinPlan); ok {
			yieldedNLJs = append(yieldedNLJs, nlj)
		}
	}
	formedAB, formedBA := directionsOf(yieldedNLJs)
	if !formedAB {
		t.Error("commutativity fire missing the NLJ yield with A as outer and B as inner")
	}
	if !formedBA {
		t.Error("commutativity fire missing the NLJ yield with B as outer and A as inner")
	}

	// WINNER: the full planner keeps at least one NLJ whose direction is
	// one of the two valid orders (the loser direction is pruned).
	selRef := buildSelect()
	rules := DefaultExpressionRules()
	p := NewPlanner(rules, EmptyPlanContext()).
		WithPlanningExpressionRules(BatchAExpressionRules()).
		WithImplementationRules(DefaultImplementationRules())
	if _, _, err := p.Plan(selRef); err != nil {
		t.Fatalf("Plan: %v", err)
	}

	var nljPlans []*plans.RecordQueryNestedLoopJoinPlan
	for _, m := range selRef.AllMembers() {
		if nlj, ok := m.(*plans.RecordQueryNestedLoopJoinPlan); ok {
			nljPlans = append(nljPlans, nlj)
		}
	}
	if len(nljPlans) == 0 {
		var explains []string
		for _, m := range selRef.AllMembers() {
			explains = append(explains, fmt.Sprintf("%T", m))
		}
		t.Fatalf("expected an NLJ winner after Plan(), got none; members: %v", explains)
	}
	winnerAB, winnerBA := directionsOf(nljPlans)
	if !winnerAB && !winnerBA {
		t.Fatalf("NLJ winner has an invalid join direction: outer=%q inner=%q",
			nljPlans[0].GetOuter().Explain(), nljPlans[0].GetInner().Explain())
	}
}

// TestE2E_JoinCommutativitySkippedForLeftJoin verifies that the planner
// does NOT explore the swapped join direction for LEFT OUTER JOINs,
// since left join semantics are order-dependent.
func TestE2E_JoinCommutativitySkippedForLeftJoin(t *testing.T) {
	t.Parallel()

	scanA := expressions.NewFullUnorderedScanExpression([]string{"A"}, values.UnknownType)
	scanARef := expressions.InitialOf(scanA)
	scanAQ := expressions.ForEachQuantifier(scanARef)

	scanB := expressions.NewFullUnorderedScanExpression([]string{"B"}, values.UnknownType)
	scanBRef := expressions.InitialOf(scanB)
	scanBQ := expressions.ForEachQuantifier(scanBRef)

	sel := expressions.NewSelectExpressionWithJoinType(
		values.NewQuantifiedObjectValue(values.UniqueCorrelationIdentifier()),
		[]expressions.Quantifier{scanAQ, scanBQ},
		nil,
		[]string{"A", "B"},
		expressions.JoinLeftOuter,
	)
	selRef := expressions.InitialOf(sel)

	rules := DefaultExpressionRules()
	p := NewPlanner(rules, EmptyPlanContext()).
		WithPlanningExpressionRules(BatchAExpressionRules()).
		WithImplementationRules(DefaultImplementationRules())
	if _, _, err := p.Plan(selRef); err != nil {
		t.Fatalf("Plan: %v", err)
	}

	// For LEFT JOIN, only one direction should be explored. All NLJ
	// plans should have A as outer.
	for _, m := range selRef.AllMembers() {
		nlj, ok := m.(*plans.RecordQueryNestedLoopJoinPlan)
		if !ok {
			continue
		}
		outerExplain := nlj.GetOuter().Explain()
		if outerExplain == "Scan(B)" {
			t.Fatal("LEFT JOIN should not explore B-as-outer direction")
		}
	}
}

// describePlan returns a short diagnostic string for the plan tree.
func describePlan(expr expressions.RelationalExpression) string {
	ph, ok := expr.(physicalPlanExpression)
	if !ok {
		return fmt.Sprintf("logical(%T)", expr)
	}
	plan := ph.GetRecordQueryPlan()
	if plan != nil {
		return plan.Explain()
	}
	return fmt.Sprintf("physical(%T)", expr)
}
