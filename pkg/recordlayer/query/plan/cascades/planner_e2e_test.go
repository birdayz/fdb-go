package cascades

import (
	"fmt"
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
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
	if _, ok := plan.(*physicalScanWrapper); !ok {
		t.Fatalf("expected physicalScanWrapper, got %T", plan)
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
	if _, ok := plan.(*physicalPredicatesFilterWrapper); !ok {
		t.Fatalf("expected physicalPredicatesFilterWrapper, got %T", plan)
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
//  1. PrimaryScanRule: yields physicalScanWrapper with PK ordering (ID).
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

	fw, ok := best.(*physicalPredicatesFilterWrapper)
	if !ok {
		t.Fatalf("expected physicalPredicatesFilterWrapper at root, got %T (%s)", best, describePlan(best))
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
	case *physicalInMemorySortWrapper:
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

// TestE2E_JoinCommutativityExploration verifies that the planner
// explores both join directions for a SelectExpression with
// ChildrenAsSet=true and 2 ForEach quantifiers (an INNER join).
// The NLJ rule should fire twice — once with the original quantifier
// order (A outer, B inner) and once with the swapped order (B outer,
// A inner) — yielding 2 distinct physical NLJ members.
func TestE2E_JoinCommutativityExploration(t *testing.T) {
	t.Parallel()

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
	selRef := expressions.InitialOf(sel)

	rules := DefaultExpressionRules()
	p := NewPlanner(rules, EmptyPlanContext()).
		WithPlanningExpressionRules(BatchAExpressionRules()).
		WithImplementationRules(DefaultImplementationRules())
	if _, _, err := p.Plan(selRef); err != nil {
		t.Fatalf("Plan: %v", err)
	}

	// Collect all physical NLJ members — there should be at least 2
	// (one per join direction). Physical wrappers are inserted into
	// Members during the PLANNING phase.
	var nljPlans []*plans.RecordQueryNestedLoopJoinPlan
	for _, m := range selRef.AllMembers() {
		nlj, ok := m.(*physicalNestedLoopJoinWrapper)
		if !ok {
			continue
		}
		nljPlans = append(nljPlans, nlj.GetPlan())
	}

	if len(nljPlans) < 2 {
		var explains []string
		for _, m := range selRef.AllMembers() {
			explains = append(explains, fmt.Sprintf("%T", m))
		}
		t.Fatalf("expected at least 2 NLJ members (both join directions), got %d; members: %v",
			len(nljPlans), explains)
	}

	// Verify we have both A-outer-B-inner and B-outer-A-inner.
	foundAB := false
	foundBA := false
	for _, nlj := range nljPlans {
		outerExplain := nlj.GetOuter().Explain()
		innerExplain := nlj.GetInner().Explain()
		if outerExplain == "Scan(A)" && innerExplain == "Scan(B)" {
			foundAB = true
		}
		if outerExplain == "Scan(B)" && innerExplain == "Scan(A)" {
			foundBA = true
		}
	}

	if !foundAB {
		t.Error("missing NLJ with A as outer and B as inner")
	}
	if !foundBA {
		t.Error("missing NLJ with B as outer and A as inner")
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
		nlj, ok := m.(*physicalNestedLoopJoinWrapper)
		if !ok {
			continue
		}
		outerExplain := nlj.GetPlan().GetOuter().Explain()
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
