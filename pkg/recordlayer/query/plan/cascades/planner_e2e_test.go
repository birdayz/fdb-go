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

	p := NewPlanner(allRules(), nil).WithImplementationRules(DefaultImplementationRules())
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

	p := NewPlanner(allRules(), nil).WithImplementationRules(DefaultImplementationRules())
	plan, _, err := p.Plan(rootRef)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan == nil {
		t.Fatal("Plan returned nil")
	}
	if _, ok := plan.(*physicalFilterWrapper); !ok {
		t.Fatalf("expected physicalFilterWrapper, got %T", plan)
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

	p := NewPlanner(allRules(), nil).WithImplementationRules(DefaultImplementationRules())
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

	p := NewPlanner(allRules(), nil).WithImplementationRules(DefaultImplementationRules())
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

	p := NewPlanner(allRules(), nil).WithImplementationRules(DefaultImplementationRules())
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

	p := NewPlanner(allRules(), nil).WithImplementationRules(DefaultImplementationRules())
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
//  2. PushOrderingThroughFilterRule: pushes Sort below Filter.
//  3. SortOverOrderedElimRule: eliminates Sort because the scan provides ID ordering.
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

	rules := append(DefaultExpressionRules(), BatchAExpressionRules()...)
	p := NewPlanner(rules, ctx).
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

	// The top-level plan should be a physical filter wrapping a physical scan.
	fw, ok := best.(*physicalFilterWrapper)
	if !ok {
		t.Fatalf("expected physicalFilterWrapper at root, got %T (%s)", best, describePlan(best))
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
