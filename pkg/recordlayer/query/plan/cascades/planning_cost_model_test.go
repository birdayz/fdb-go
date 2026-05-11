package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

// TestPlanningCostModel_PhysicalBeatsLogical verifies criterion 1:
// a physical plan is always preferred over a logical expression.
func TestPlanningCostModel_PhysicalBeatsLogical(t *testing.T) {
	t.Parallel()

	scan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	physical := &physicalScanWrapper{plan: scan}
	logical := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)

	if !PlanningCostModelLess(physical, logical) {
		t.Error("PlanningCostModelLess(physical, logical) = false, want true")
	}
	if PlanningCostModelLess(logical, physical) {
		t.Error("PlanningCostModelLess(logical, physical) = true, want false")
	}
}

// TestPlanningCostModel_FewerResidualPredicatesWins verifies criterion 3:
// among physical plans with identical scan counts, the one with fewer
// residual predicates is preferred.
func TestPlanningCostModel_FewerResidualPredicatesWins(t *testing.T) {
	t.Parallel()

	scan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	scanRef := expressions.NewFinalReference([]expressions.RelationalExpression{&physicalScanWrapper{plan: scan}})
	innerQ := expressions.ForEachQuantifier(scanRef)

	pred1 := predicates.NewComparisonPredicate(
		&values.FieldValue{Field: "x", Typ: values.TypeInt},
		predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(1)),
	)
	pred2 := predicates.NewComparisonPredicate(
		&values.FieldValue{Field: "y", Typ: values.TypeInt},
		predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(2)),
	)

	// one-predicate filter
	onePred := NewPhysicalPredicatesFilterWrapper(
		plans.NewRecordQueryPredicatesFilterPlan(nil, []predicates.QueryPredicate{pred1}),
		innerQ,
	)
	// two-predicate filter — same scan underneath
	twoPred := NewPhysicalPredicatesFilterWrapper(
		plans.NewRecordQueryPredicatesFilterPlan(nil, []predicates.QueryPredicate{pred1, pred2}),
		innerQ,
	)

	if !PlanningCostModelLess(onePred, twoPred) {
		t.Error("PlanningCostModelLess(1-pred, 2-pred) = false, want true")
	}
	if PlanningCostModelLess(twoPred, onePred) {
		t.Error("PlanningCostModelLess(2-pred, 1-pred) = true, want false")
	}
}

// TestPlanningCostModel_HashTieBreakIsDeterministic verifies criterion 11:
// two distinct physical scans must produce a stable ordering — the comparison
// must not return 0 (one must strictly beat the other), and repeated calls
// must agree.
func TestPlanningCostModel_HashTieBreakIsDeterministic(t *testing.T) {
	t.Parallel()

	scanA := plans.NewRecordQueryScanPlan([]string{"A"}, values.UnknownType, false)
	scanB := plans.NewRecordQueryScanPlan([]string{"B"}, values.UnknownType, false)
	wrapA := &physicalScanWrapper{plan: scanA}
	wrapB := &physicalScanWrapper{plan: scanB}

	ab := PlanningCostModelLess(wrapA, wrapB)
	ba := PlanningCostModelLess(wrapB, wrapA)

	// Exactly one of the two must win (strict total order, not a tie).
	if ab == ba {
		t.Errorf("hash tie-break is inconsistent: Less(A,B)=%v Less(B,A)=%v — exactly one must be true", ab, ba)
	}

	// Must be stable across repeated calls.
	for i := 0; i < 10; i++ {
		if PlanningCostModelLess(wrapA, wrapB) != ab {
			t.Fatalf("hash tie-break changed on iteration %d", i)
		}
	}
}
